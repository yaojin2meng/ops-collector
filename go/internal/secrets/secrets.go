// Package secrets: 凭据存取（Windows 凭据管理器优先，环境变量兜底）。
//
// 规则：
//   - 密码/令牌绝不写进配置文件、命令行参数、URL、日志或 artifact。
//   - secret_ref 约定：
//     keyring:<entry>  → keyring 服务 "ops-collector"，条目 <entry>
//     env:<NAME>       → 环境变量 NAME
//     其他             → 先找 OPS_COLLECTOR_SECRET_<REF大写,-/:→_>，
//     再找 keyring 条目 <REF>
//   - 交互式补录：stdin 为终端时隐藏回显输入。
package secrets

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/zalando/go-keyring"
	"golang.org/x/term"

	ierr "opscollector/internal/errors"
)

const (
	KeyringService = "ops-collector"
	EnvPrefix      = "OPS_COLLECTOR_SECRET_"
)

// keyringGet / keyringSet / keyringDelete 默认走真实 keyring，
// 可被 hooks.go 的测试注入点替换。
func defaultKeyringGet(entry string) (string, error) {
	v, err := keyring.Get(KeyringService, entry)
	if err != nil {
		return "", err
	}
	return v, nil
}

func defaultKeyringSet(entry, value string) error {
	return keyring.Set(KeyringService, entry, value)
}

func defaultKeyringDelete(entry string) error {
	return keyring.Delete(KeyringService, entry)
}

// GetSecret 按约定解析 secret_ref。找不到时（允许交互且在终端）提示输入。
func GetSecret(ref string, prompt string, allowPrompt bool) (string, error) {
	if ref == "" {
		return "", ierr.New(ierr.CodeSecretNotFound, "secret_ref 为空")
	}
	if v := lookup(ref); v != "" {
		return v, nil
	}
	if allowPrompt && prompt != "" && term.IsTerminal(int(os.Stdin.Fd())) {
		// 提示走 stderr：pty 场景保证 stdout 仍是纯 JSON
		fmt.Fprintf(os.Stderr, "%s（输入不会回显）: ", prompt)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err == nil && len(b) > 0 {
			return string(b), nil
		}
	}
	return "", ierr.New(ierr.CodeSecretNotFound,
		"凭据不可用: "+ref+"（可运行 `ops-collector secrets set` 写入凭据管理器）")
}

func lookup(ref string) string {
	switch {
	case strings.HasPrefix(ref, "keyring:"):
		entry := strings.TrimPrefix(ref, "keyring:")
		if v, err := keyringGet(entry); err == nil {
			return v
		}
		return ""
	case strings.HasPrefix(ref, "env:"):
		return os.Getenv(strings.TrimPrefix(ref, "env:"))
	}
	envName := EnvPrefix + strings.NewReplacer("-", "_", ":", "_", ".", "_").
		Replace(strings.ToUpper(ref))
	if v := os.Getenv(envName); v != "" {
		return v
	}
	if v, err := keyringGet(ref); err == nil {
		return v
	}
	return ""
}

func SetSecret(entry, value string) error {
	if err := keyringSet(entry, value); err != nil {
		return ierr.Wrap(ierr.CodeSecret, "写入凭据管理器失败", err)
	}
	return nil
}

func DeleteSecret(entry string) bool {
	return keyringDelete(entry) == nil
}

func ListEntriesHint() []string {
	var out []string
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, EnvPrefix) {
			out = append(out, strings.ToLower(strings.TrimPrefix(k, EnvPrefix)))
		}
	}
	return out
}

// ---- 会话令牌缓存（复用同一 keyring 服务，值为 JSON） ----

func SessionEntry(siteID string) string { return "session:" + siteID }

type Session struct {
	AccessToken     string `json:"access_token"`
	RefreshToken    string `json:"refresh_token,omitempty"`
	AccessExpiresAt int64  `json:"access_expires_at,omitempty"`
	UserID          int64  `json:"user_id,omitempty"`
}

func GetSession(siteID string) *Session {
	raw, err := keyringGet(SessionEntry(siteID))
	if err != nil || raw == "" {
		return nil
	}
	var s Session
	if json.Unmarshal([]byte(raw), &s) != nil {
		return nil
	}
	return &s
}

func PutSession(siteID string, s *Session) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	// Windows 凭据 blob 上限 2560 字节（CRED_MAX_CREDENTIAL_BLOB_SIZE）：
	// 超 2000 先降级丢 refresh_token（access_token 单独仍可缓存）；
	// 仍超限则不缓存并报错（调用方忽略，等效于每次重新登录）。
	if len(b) > 2000 && s.RefreshToken != "" {
		s2 := *s
		s2.RefreshToken = ""
		if b2, e2 := json.Marshal(&s2); e2 == nil && len(b2) <= 2000 {
			b = b2
		}
	}
	if len(b) > 2500 {
		return ierr.New(ierr.CodeSecret, "会话过大，跳过缓存")
	}
	return keyringSet(SessionEntry(siteID), string(b))
}

func DropSession(siteID string) { _ = keyringDelete(SessionEntry(siteID)) }

// Redact 把文本中出现的任何凭据值替换为 ***（≥4字符才替换）。
func Redact(text string, secretsList []string) string {
	for _, s := range secretsList {
		if len(s) >= 4 {
			text = strings.ReplaceAll(text, s, "***")
		}
	}
	return text
}
