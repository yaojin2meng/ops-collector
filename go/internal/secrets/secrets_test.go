package secrets

import (
	"strings"
	"testing"
)

func TestEnvPrefixLookup(t *testing.T) {
	t.Setenv("OPS_COLLECTOR_SECRET_MYNEWAPI_PASSWORD", "pw123")
	if got := lookup("mynewapi:password"); got != "pw123" {
		t.Fatalf("lookup = %q", got)
	}
}

func TestEnvExplicitLookup(t *testing.T) {
	t.Setenv("MY_CUSTOM_VAR", "v")
	if got := lookup("env:MY_CUSTOM_VAR"); got != "v" {
		t.Fatalf("lookup = %q", got)
	}
}

func TestMissingSecret(t *testing.T) {
	t.Setenv("OPS_COLLECTOR_SECRET_NOPE", "")
	// 无终端环境（go test）下不允许交互，应报错
	_, err := GetSecret("nope", "prompt", false)
	if err == nil || !strings.Contains(err.Error(), "凭据不可用") {
		t.Fatalf("期望凭据不可用错误，得到 %v", err)
	}
}

func TestRedact(t *testing.T) {
	out := Redact("login with pw123456 ok", []string{"pw123456"})
	if strings.Contains(out, "pw123456") || !strings.Contains(out, "***") {
		t.Fatalf("redact 失败: %s", out)
	}
}

func TestRedactShortValuesIgnored(t *testing.T) {
	if got := Redact("abc abc", []string{"ab"}); got != "abc abc" {
		t.Fatalf("got %q", got)
	}
}

func TestSessionEntryNaming(t *testing.T) {
	if SessionEntry("site-x") != "session:site-x" {
		t.Fatal("entry 命名错误")
	}
}

func TestFakeSessionStore(t *testing.T) {
	// 用注入点模拟 keyring，验证会话存取
	store := map[string]string{}
	oldGet, oldSet := keyringGet, keyringSet
	defer func() { keyringGet, keyringSet = oldGet, oldSet }()
	keyringGet = func(e string) (string, error) { return store[e], nil }
	keyringSet = func(e, v string) error { store[e] = v; return nil }

	PutSession("t1", &Session{AccessToken: "tok", UserID: 7})
	s := GetSession("t1")
	if s == nil || s.AccessToken != "tok" || s.UserID != 7 {
		t.Fatalf("会话存取失败: %+v", s)
	}
}
