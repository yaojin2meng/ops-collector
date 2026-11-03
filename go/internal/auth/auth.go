// Package auth: 账号密码登录 + 会话令牌缓存 + 访问令牌（PAT）直连。
//
//   - 会话缓存到 Windows 凭据管理器，不落盘明文。
//   - 密码只在登录瞬间从凭据存储读取。
//   - 2FA / 人机校验（Turnstile 等）自动化不可用 → LOGIN_BLOCKED，建议改用 PAT。
package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"strings"
	"time"

	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"
	"opscollector/internal/secrets"
)

var (
	turnstileHints = []string{"turnstile", "captcha", "人机", "验证码"}
	twofaHints     = []string{"2fa", "two_factor", "totp", "challenge"}
)

func looksBlocked(msg string) bool {
	low := strings.ToLower(msg)
	for _, h := range turnstileHints {
		if strings.Contains(low, strings.ToLower(h)) {
			return true
		}
	}
	return false
}

func looks2FA(msg string) bool {
	low := strings.ToLower(msg)
	for _, h := range twofaHints {
		if strings.Contains(low, h) {
			return true
		}
	}
	return false
}

// AuthCtx 是鉴权后的请求上下文。
type AuthCtx struct {
	Headers map[string]string
	UserID  int64
}

// Authenticator 为站点建立会话。
type Authenticator interface {
	EnsureSession(site SiteAuthInfo, client *httpc.Client) (AuthCtx, error)
}

// SiteAuthInfo 是 adapters 传入的最小站点信息视图。
type SiteAuthInfo interface {
	GetSiteID() string
	GetKind() string
	GetAuthMode() string
	GetUsername() string
	GetEmail() string
	GetSecretRef() string
	GetAPIKeySecretRef() string
}

// ---------------------------------------------------------------------------
// new-api

type NewApiAuth struct{}

func (NewApiAuth) EnsureSession(site SiteAuthInfo, client *httpc.Client) (AuthCtx, error) {
	if site.GetAuthMode() == "api_key" {
		return newApiPATSession(site, client)
	}

	if cached := secrets.GetSession(site.GetSiteID()); cached != nil &&
		cached.AccessToken != "" &&
		(cached.AccessExpiresAt == 0 || cached.AccessExpiresAt > time.Now().Unix()+30) {
		return AuthCtx{Headers: bearer(cached.AccessToken, cached.UserID),
			UserID: cached.UserID}, nil
	}

	password, err := secrets.GetSecret(site.GetSecretRef(),
		fmt.Sprintf("站点 %s 的密码", site.GetSiteID()), true)
	if err != nil {
		return AuthCtx{}, err
	}
	// 主方案 + 备用 padding（上游前端加密形态未锁定，登录失败时换一种再试）
	body, altBody, usedEncryption, err := newAPILoginPayload(client, site.GetUsername(), password)
	if err != nil {
		return AuthCtx{}, err
	}
	login := func(b map[string]any) (any, error) {
		resp, err := client.Post("/api/user/login", b, nil)
		if err != nil {
			if looksBlocked(err.Error()) {
				return nil, ierr.New(ierr.CodeLoginBlocked,
					fmt.Sprintf("站点 %s 登录被人机校验拦截，请在站点安全设置创建访问令牌后改用 api_key 模式", site.GetSiteID()))
			}
			return nil, err
		}
		data, uerr := httpc.UnwrapNewAPI(resp, "登录")
		if uerr != nil {
			// new-api 密码错误也是 HTTP 200 + success:false
			return nil, ierr.New(ierr.CodeAuthFailed,
				fmt.Sprintf("站点 %s 登录失败: %v", site.GetSiteID(), uerr))
		}
		return data, nil
	}
	data, err := login(body)
	if err != nil && usedEncryption && altBody != nil && ierr.CodeOf(err) == ierr.CodeAuthFailed {
		// 主 padding 不匹配（只能由服务端解密侧暴露）→ 换备用 padding 再试一次
		data, err = login(altBody)
	}
	if err != nil {
		if usedEncryption && ierr.CodeOf(err) == ierr.CodeAuthFailed {
			return AuthCtx{}, ierr.New(ierr.CodeAuthFailed,
				fmt.Sprintf("站点 %s 登录失败（已尝试两种 RSA padding）；"+
					"建议改用 api_key 模式（安全设置里的访问令牌）", site.GetSiteID()))
		}
		return AuthCtx{}, err
	}
	dm, _ := data.(map[string]any)
	if dm != nil {
		if tok, ok := dm["access_token"].(string); ok && tok != "" {
			user, _ := dm["user"].(map[string]any)
			var uid int64
			if user != nil {
				if f, ok := user["id"].(float64); ok {
					uid = int64(f)
				}
			}
			expires := time.Now().Unix() + 3600
			if e, ok := dm["access_expires_at"].(float64); ok {
				expires = int64(e)
			}
			_ = secrets.PutSession(site.GetSiteID(), &secrets.Session{
				AccessToken: tok, AccessExpiresAt: expires, UserID: uid})
			return AuthCtx{Headers: bearer(tok, uid), UserID: uid}, nil
		}
		if looks2FA(string(mustJSON(data))) {
			return AuthCtx{}, ierr.New(ierr.CodeLoginBlocked,
				fmt.Sprintf("站点 %s 登录需要 2FA 验证，自动化不支持；请在安全设置创建访问令牌", site.GetSiteID()))
		}
	}
	return AuthCtx{}, ierr.New(ierr.CodeAuthFailed,
		fmt.Sprintf("站点 %s 登录响应缺少 access_token（可能需要 2FA）", site.GetSiteID()))
}

// newApiPATSession 为 api_key（PAT/访问令牌）模式建立鉴权上下文。
// new-api 部分版本的令牌鉴权链要求 New-Api-User 头 → 首次使用时用令牌调
// /api/user/self 解析 user_id 并缓存；解析失败仍继续（新版本从令牌本身解析身份）。
func newApiPATSession(site SiteAuthInfo, client *httpc.Client) (AuthCtx, error) {
	token, err := secrets.GetSecret(site.GetAPIKeySecretRef(),
		fmt.Sprintf("站点 %s 的访问令牌(PAT)", site.GetSiteID()), true)
	if err != nil {
		return AuthCtx{}, err
	}
	if cached := secrets.GetSession(site.GetSiteID()); cached != nil &&
		cached.AccessToken == token && cached.UserID > 0 {
		return AuthCtx{Headers: bearer(token, cached.UserID), UserID: cached.UserID}, nil
	}
	uid := int64(0)
	resp, err := client.Get("/api/user/self", nil, bearer(token, 0))
	if err == nil {
		if data, uerr := httpc.UnwrapNewAPI(resp, "用户信息"); uerr == nil {
			if um, ok := data.(map[string]any); ok {
				uid, _ = httpc.AsInt(um["id"])
			}
		}
	}
	_ = secrets.PutSession(site.GetSiteID(), &secrets.Session{
		AccessToken: token, UserID: uid})
	return AuthCtx{Headers: bearer(token, uid), UserID: uid}, nil
}

// newAPILoginPayload 若站点启用密码加密则取公钥加密（同时生成备用 padding 的
// 请求体），否则明文。返回 (主请求体, 备用请求体, 是否使用了加密, error)。
func newAPILoginPayload(client *httpc.Client, username, password string) (
	map[string]any, map[string]any, bool, error) {
	plain := map[string]any{"username": username, "password": password}
	keyResp, err := client.Get("/api/user/login/encryption-key", nil, nil)
	if err != nil {
		// 端点不可达：按未启用加密处理；若站点实际启用，登录失败消息会提示
		return plain, nil, false, nil
	}
	keyData, uerr := httpc.UnwrapNewAPI(keyResp, "加密密钥")
	if uerr != nil {
		return plain, nil, false, nil
	}
	kd, ok := keyData.(map[string]any)
	if !ok || kd["enabled"] != true {
		return plain, nil, false, nil
	}
	pub, _ := kd["public_key"].(string)
	kid, _ := kd["kid"].(string)
	primary, alt, encErr := rsaEncryptPasswordDual(password, pub)
	if encErr != nil {
		return nil, nil, false, ierr.New(ierr.CodeAuthFailed,
			"密码加密失败：公钥解析或加密方案均不匹配")
	}
	mk := func(enc string) map[string]any {
		return map[string]any{
			"username": username, "password_encrypted": enc,
			"encryption_key_id": kid,
		}
	}
	return mk(primary), mk(alt), true, nil
}

// rsaEncryptPasswordDual 用两种 padding 各加密一次：
// 返回 (OAEP-SHA256 密文, PKCS1v15 密文, error)。padding 匹配与否只能由
// 服务端解密侧判定，因此客户端两种都备好，由登录结果决定取舍。
func rsaEncryptPasswordDual(password, publicKeyPEM string) (string, string, error) {
	block, _ := pem.Decode([]byte(publicKeyPEM))
	var der []byte
	if block != nil {
		der = block.Bytes
	} else {
		// 可能是裸 base64 DER
		if b, err := base64.StdEncoding.DecodeString(publicKeyPEM); err == nil {
			der = b
		} else {
			return "", "", fmt.Errorf("公钥既不是 PEM 也不是 base64")
		}
	}
	var pub any
	if p, err := x509.ParsePKIXPublicKey(der); err == nil {
		pub = p
	} else if p, err := x509.ParsePKCS1PublicKey(der); err == nil {
		pub = p
	} else {
		return "", "", fmt.Errorf("公钥解析失败")
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return "", "", fmt.Errorf("公钥不是 RSA 类型")
	}
	pwd := []byte(password)
	oaep, err1 := rsa.EncryptOAEP(sha256.New(), rand.Reader, rsaPub, pwd, nil)
	pkcs, err2 := rsa.EncryptPKCS1v15(rand.Reader, rsaPub, pwd)
	if err1 != nil || err2 != nil {
		return "", "", fmt.Errorf("加密失败: %v / %v", err1, err2)
	}
	return base64.StdEncoding.EncodeToString(oaep),
		base64.StdEncoding.EncodeToString(pkcs), nil
}

func bearer(token string, userID int64) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + token}
	if userID > 0 {
		h["New-Api-User"] = fmt.Sprintf("%d", userID)
	}
	return h
}

// ---------------------------------------------------------------------------
// sub2api

type Sub2ApiAuth struct{}

func (Sub2ApiAuth) EnsureSession(site SiteAuthInfo, client *httpc.Client) (AuthCtx, error) {
	if site.GetAuthMode() == "api_key" {
		token, err := secrets.GetSecret(site.GetAPIKeySecretRef(),
			fmt.Sprintf("站点 %s 的 API Key", site.GetSiteID()), true)
		if err != nil {
			return AuthCtx{}, err
		}
		return AuthCtx{Headers: map[string]string{"Authorization": "Bearer " + token}}, nil
	}

	if cached := secrets.GetSession(site.GetSiteID()); cached != nil &&
		cached.AccessToken != "" &&
		(cached.AccessExpiresAt == 0 || cached.AccessExpiresAt > time.Now().Unix()+30) {
		return AuthCtx{Headers: map[string]string{"Authorization": "Bearer " + cached.AccessToken},
			UserID: cached.UserID}, nil
	}

	password, err := secrets.GetSecret(site.GetSecretRef(),
		fmt.Sprintf("站点 %s 的密码", site.GetSiteID()), true)
	if err != nil {
		return AuthCtx{}, err
	}
	email := site.GetEmail()
	if email == "" {
		email = site.GetUsername()
	}
	body := map[string]any{"email": email, "password": password}
	// 人机校验字段允许用环境变量注入（运维提前准备），不从命令行传
	if v := os.Getenv("SUB2API_TURNSTILE_TOKEN"); v != "" {
		body["turnstile_token"] = v
	}
	if v := os.Getenv("SUB2API_TENCENT_CAPTCHA_TICKET"); v != "" {
		body["tencent_captcha_ticket"] = v
	}
	if v := os.Getenv("SUB2API_TENCENT_CAPTCHA_RANDSTR"); v != "" {
		body["tencent_captcha_randstr"] = v
	}

	resp, err := client.Post("/api/v1/auth/login", body, nil)
	if err != nil {
		if looksBlocked(err.Error()) {
			_, hasCaptcha := body["turnstile_token"]
			if !hasCaptcha {
				return AuthCtx{}, ierr.New(ierr.CodeLoginBlocked,
					fmt.Sprintf("站点 %s 登录需要人机校验令牌；可设置环境变量 "+
						"SUB2API_TURNSTILE_TOKEN 或改用 API Key 模式", site.GetSiteID()))
			}
		}
		return AuthCtx{}, err
	}
	data, err := httpc.UnwrapSub2API(resp, "登录")
	if err != nil {
		return AuthCtx{}, err
	}
	dm, _ := data.(map[string]any)
	tok := ""
	if dm != nil {
		tok, _ = dm["access_token"].(string)
	}
	if tok == "" {
		if dm != nil && looks2FA(string(mustJSON(data))) {
			return AuthCtx{}, ierr.New(ierr.CodeLoginBlocked,
				fmt.Sprintf("站点 %s 登录需要 2FA，自动化不支持", site.GetSiteID()))
		}
		return AuthCtx{}, ierr.New(ierr.CodeAuthFailed,
			fmt.Sprintf("站点 %s 登录响应缺少 access_token", site.GetSiteID()))
	}
	rt, _ := dm["refresh_token"].(string)
	var uid int64
	if u, ok := dm["user"].(map[string]any); ok {
		if f, ok := u["id"].(float64); ok {
			uid = int64(f)
		}
	}
	expires := int64(0)
	if e, ok := dm["expires_in"].(float64); ok && e > 0 {
		expires = time.Now().Unix() + int64(e)
	}
	_ = secrets.PutSession(site.GetSiteID(), &secrets.Session{
		AccessToken: tok, RefreshToken: rt, AccessExpiresAt: expires, UserID: uid})
	return AuthCtx{Headers: map[string]string{"Authorization": "Bearer " + tok},
		UserID: uid}, nil
}

// Refresh 用 refresh_token 换新 access_token。
func (Sub2ApiAuth) Refresh(site SiteAuthInfo, client *httpc.Client) (AuthCtx, error) {
	cached := secrets.GetSession(site.GetSiteID())
	if cached == nil || cached.RefreshToken == "" {
		return AuthCtx{}, ierr.New(ierr.CodeAuthFailed,
			fmt.Sprintf("站点 %s 无 refresh_token，请重新登录", site.GetSiteID()))
	}
	resp, err := client.Post("/api/v1/auth/refresh",
		map[string]any{"refresh_token": cached.RefreshToken}, nil)
	if err != nil {
		return AuthCtx{}, err
	}
	data, err := httpc.UnwrapSub2API(resp, "刷新会话")
	if err != nil {
		return AuthCtx{}, err
	}
	dm, _ := data.(map[string]any)
	tok := ""
	if dm != nil {
		tok, _ = dm["access_token"].(string)
	}
	if tok == "" {
		return AuthCtx{}, ierr.New(ierr.CodeAuthFailed,
			fmt.Sprintf("站点 %s 刷新会话失败", site.GetSiteID()))
	}
	if rt, ok := dm["refresh_token"].(string); ok && rt != "" {
		cached.RefreshToken = rt
	}
	if e, ok := dm["expires_in"].(float64); ok && e > 0 {
		cached.AccessExpiresAt = time.Now().Unix() + int64(e)
	}
	cached.AccessToken = tok
	_ = secrets.PutSession(site.GetSiteID(), cached)
	return AuthCtx{Headers: map[string]string{"Authorization": "Bearer " + tok},
		UserID: cached.UserID}, nil
}

func MakeAuthenticator(kind string) (Authenticator, bool) {
	switch kind {
	case "new_api":
		return NewApiAuth{}, true
	case "sub2api":
		return Sub2ApiAuth{}, true
	}
	return nil, false
}

// mustJSON 序列化任意值用于启发式字符串匹配；失败返回空串。
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
