package auth

import (
	"testing"

	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"
	"opscollector/internal/secrets"
	"opscollector/internal/testmock"
)

// fakeSite 实现 SiteAuthInfo。
type fakeSite struct {
	id, kind, mode, username, email, secretRef, patRef string
}

func (s fakeSite) GetSiteID() string          { return s.id }
func (s fakeSite) GetKind() string            { return s.kind }
func (s fakeSite) GetAuthMode() string        { return s.mode }
func (s fakeSite) GetUsername() string        { return s.username }
func (s fakeSite) GetEmail() string           { return s.email }
func (s fakeSite) GetSecretRef() string       { return s.secretRef }
func (s fakeSite) GetAPIKeySecretRef() string { return s.patRef }

// installFakeStore 把 keyring 换成内存实现，返回 store 供断言。
func installFakeStore() map[string]string {
	m := map[string]string{}
	secrets.SetKeyringForTest(
		func(e string) (string, error) { return m[e], nil },
		func(e, v string) error { m[e] = v; return nil },
		func(e string) error { delete(m, e); return nil },
	)
	return m
}

func TestNewApiLoginSuccessAndCache(t *testing.T) {
	store := installFakeStore()
	defer secrets.ResetKeyring()
	store["t:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()

	site := fakeSite{id: "t1", kind: "new_api", mode: "password",
		username: "opuser", secretRef: "t:password"}
	c := httpc.New(srv.URL, httpc.Options{MinIntervalS: 0, Retries: 1})
	ctx, err := NewApiAuth{}.EnsureSession(site, c)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Headers["Authorization"] != "Bearer "+testmock.NAPIToken {
		t.Fatalf("headers: %v", ctx.Headers)
	}
	if ctx.Headers["New-Api-User"] != "42" || ctx.UserID != 42 {
		t.Fatalf("New-Api-User/user_id 错误: %v", ctx.Headers)
	}
	// 第二次走缓存
	ctx2, err := NewApiAuth{}.EnsureSession(site, c)
	if err != nil || ctx2.Headers["Authorization"] != ctx.Headers["Authorization"] {
		t.Fatalf("缓存失败: %v", err)
	}
	if store["session:t1"] == "" {
		t.Fatal("会话未写入")
	}
}

func TestNewApiWrongPassword(t *testing.T) {
	store := installFakeStore()
	defer secrets.ResetKeyring()
	store["t:password"] = "totally-wrong-pw"
	srv := testmock.NewApiSite()
	defer srv.Close()
	site := fakeSite{id: "t1", kind: "new_api", mode: "password",
		username: "opuser", secretRef: "t:password"}
	c := httpc.New(srv.URL, httpc.Options{MinIntervalS: 0, Retries: 1})
	_, err := NewApiAuth{}.EnsureSession(site, c)
	if err == nil || ierr.CodeOf(err) != ierr.CodeAuthFailed {
		t.Fatalf("期望 AUTH_FAILED，得到 %v", err)
	}
}

func TestSub2ApiLoginSuccess(t *testing.T) {
	store := installFakeStore()
	defer secrets.ResetKeyring()
	store["t:password"] = testmock.SUBPassword
	srv := testmock.Sub2ApiSite(false)
	defer srv.Close()
	site := fakeSite{id: "t1", kind: "sub2api", mode: "password",
		email: "ops@example.com", secretRef: "t:password"}
	c := httpc.New(srv.URL, httpc.Options{MinIntervalS: 0, Retries: 1})
	ctx, err := Sub2ApiAuth{}.EnsureSession(site, c)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Headers["Authorization"] != "Bearer "+testmock.SUBToken || ctx.UserID != 7 {
		t.Fatalf("ctx: %+v", ctx)
	}
}

func TestSub2ApiRefreshFlow(t *testing.T) {
	installFakeStore()
	defer secrets.ResetKeyring()
	// 预置会话（含 refresh token）
	secrets.PutSession("t1", &secrets.Session{
		AccessToken: "expired-jwt", RefreshToken: testmock.SUBRefresh, UserID: 7})
	srv := testmock.Sub2ApiSite(false)
	defer srv.Close()
	site := fakeSite{id: "t1", kind: "sub2api", mode: "password",
		email: "ops@example.com", secretRef: "t:password"}
	c := httpc.New(srv.URL, httpc.Options{MinIntervalS: 0, Retries: 1})
	ctx, err := Sub2ApiAuth{}.Refresh(site, c)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Headers["Authorization"] != "Bearer "+testmock.SUBToken {
		t.Fatalf("refresh 后 token 错误: %v", ctx.Headers)
	}
}

func TestBlockedHeuristics(t *testing.T) {
	if !looksBlocked("请完成 turnstile 人机校验") {
		t.Fatal("turnstile 未识别")
	}
	if looksBlocked("ok") {
		t.Fatal("误报")
	}
	if !looks2FA(`{"challenge":"totp_required"}`) {
		t.Fatal("2fa 未识别")
	}
	if looks2FA(`{"access_token":"x"}`) {
		t.Fatal("误报")
	}
}

func TestAPIKeyModeNoLogin(t *testing.T) {
	store := installFakeStore()
	defer secrets.ResetKeyring()
	store["t:pat"] = "pat-token-1"
	srv := testmock.NewApiSite()
	defer srv.Close()
	site := fakeSite{id: "t1", kind: "new_api", mode: "api_key", patRef: "t:pat"}
	c := httpc.New(srv.URL, httpc.Options{MinIntervalS: 0, Retries: 1})
	ctx, err := NewApiAuth{}.EnsureSession(site, c)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Headers["Authorization"] != "Bearer pat-token-1" {
		t.Fatalf("headers: %v", ctx.Headers)
	}
}
