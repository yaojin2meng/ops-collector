package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func baseSite() map[string]any {
	return map[string]any{
		"site_id": "s1", "kind": "new_api", "base_url": "https://s1.test",
		"auth": map[string]any{"mode": "password", "username": "u", "secret_ref": "s1:password"},
	}
}

func cfgJSON(sites ...map[string]any) string {
	b, _ := json.Marshal(map[string]any{"schema_version": 1, "sites": sites})
	return string(b)
}

func TestValidConfig(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "collector.json", cfgJSON(baseSite()))
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sites[0].SiteID != "s1" {
		t.Fatalf("site_id = %s", cfg.Sites[0].SiteID)
	}
	if cfg.Defaults.PageSize != 100 || len(cfg.Defaults.WindowsHours) != 3 {
		t.Fatalf("defaults 未填充: %+v", cfg.Defaults)
	}
}

func TestDuplicateSiteID(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "collector.json", cfgJSON(baseSite(), baseSite()))
	if _, err := Load(p); err == nil || wantCode(err, "CONFIG_INVALID") == false {
		t.Fatalf("期望 CONFIG_INVALID，得到 %v", err)
	}
}

func TestProdHTTPRejected(t *testing.T) {
	s := baseSite()
	s["base_url"] = "http://api.example.com"
	dir := t.TempDir()
	p := write(t, dir, "collector.json", cfgJSON(s))
	_, err := Load(p)
	if err == nil || !wantCode(err, "CONFIG_INVALID") {
		t.Fatalf("期望 HTTPS 错误，得到 %v", err)
	}
}

func TestLocalhostHTTPAllowed(t *testing.T) {
	s := baseSite()
	s["base_url"] = "http://localhost:3000"
	dir := t.TempDir()
	p := write(t, dir, "collector.json", cfgJSON(s))
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
}

func TestWrongSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "collector.json", `{"schema_version": 99, "sites": []}`)
	_, err := Load(p)
	if err == nil || !wantCode(err, "CONFIG_INVALID") {
		t.Fatalf("期望 schema_version 错误，得到 %v", err)
	}
}

func TestPasswordModeRequiresSecretRef(t *testing.T) {
	s := baseSite()
	s["auth"] = map[string]any{"mode": "password", "username": "u"}
	dir := t.TempDir()
	p := write(t, dir, "collector.json", cfgJSON(s))
	_, err := Load(p)
	if err == nil {
		t.Fatal("期望 secret_ref 缺失错误")
	}
}

func TestAPIKeyMode(t *testing.T) {
	s := baseSite()
	s["auth"] = map[string]any{"mode": "api_key", "api_key_secret_ref": "s1:pat"}
	dir := t.TempDir()
	p := write(t, dir, "collector.json", cfgJSON(s))
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EnabledSites()[0].Auth.Mode != "api_key" {
		t.Fatal("mode != api_key")
	}
}

func TestInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "collector.json", "{not json")
	if _, err := Load(p); err == nil {
		t.Fatal("期望 JSON 错误")
	}
}

func TestMissingFile(t *testing.T) {
	if _, err := Load("Z:/no/such/file.json"); err == nil {
		t.Fatal("期望 CONFIG_NOT_FOUND")
	}
}

func TestChineseDirPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "中文目录")
	p := write(t, dir, "collector.json", cfgJSON(baseSite()))
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sites[0].Kind != "new_api" {
		t.Fatal("kind 错误")
	}
}

func TestSiteIDPattern(t *testing.T) {
	s := baseSite()
	s["site_id"] = "bad id!"
	dir := t.TempDir()
	p := write(t, dir, "collector.json", cfgJSON(s))
	if _, err := Load(p); err == nil {
		t.Fatal("期望 site_id 模式错误")
	}
}
