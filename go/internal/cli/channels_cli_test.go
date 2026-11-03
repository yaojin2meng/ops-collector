package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"opscollector/internal/secrets"
	"opscollector/internal/testmock"
)

func installCliStore(t *testing.T) map[string]string {
	t.Helper()
	m := map[string]string{}
	secrets.SetKeyringForTest(
		func(e string) (string, error) { return m[e], nil },
		func(e, v string) error { m[e] = v; return nil },
		func(e string) error { delete(m, e); return nil },
	)
	t.Cleanup(secrets.ResetKeyring)
	return m
}

// channelsAdminConfig 生成指向 mock 站点、带 admin 开关的配置文件。
func channelsAdminConfig(t *testing.T, naURL, sbURL string, admin bool) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "collector.json")
	cfg := `{"schema_version":1,"defaults":{"min_interval_s":0,"retries":1,"backoff_base_s":0.001},"sites":[
		{"site_id":"na1","kind":"new_api","base_url":"` + naURL + `",
		 "auth":{"mode":"password","username":"opuser","secret_ref":"na1:password"},
		 "enabled":true,"admin":` + boolJSON(admin) + `},
		{"site_id":"sb1","kind":"sub2api","base_url":"` + sbURL + `",
		 "auth":{"mode":"password","email":"ops@example.com","secret_ref":"sb1:password"},
		 "enabled":true,"admin":` + boolJSON(admin) + `}]}`

	if err := os.WriteFile(p, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestChannelsAdminGate(t *testing.T) {
	store := installCliStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	cfg := channelsAdminConfig(t, srv.URL, "https://unused.test", false)

	var out string
	rc := 0
	out = capture(t, func() { rc = Run([]string{"channels", "list", "--config", cfg, "--site", "na1"}) })
	if rc != 1 {
		t.Fatalf("未开 admin 应失败: rc=%d out=%s", rc, out)
	}
	if !strings.Contains(out, "ADMIN_REQUIRED") {
		t.Fatalf("期望 ADMIN_REQUIRED: %s", out)
	}
}

func TestChannelsListAndDeleteGuard(t *testing.T) {
	store := installCliStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	cfg := channelsAdminConfig(t, srv.URL, "https://unused.test", true)

	// list
	var out string
	rc := 0
	out = capture(t, func() {
		rc = Run([]string{"channels", "list", "--config", cfg, "--site", "na1"})
	})
	if rc != 0 {
		t.Fatalf("list rc=%d: %s", rc, out)
	}
	var data struct {
		OK    bool `json:"ok"`
		Total int  `json:"total"`
		Items []struct {
			ChannelID int64  `json:"channel_id"`
			Name      string `json:"name"`
			Status    string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("输出: %s", out)
	}
	if !data.OK || data.Total != 2 || len(data.Items) != 2 {
		t.Fatalf("list 数据: %+v", data)
	}
	if data.Items[0].ChannelID != 1 || data.Items[0].Status != "enabled" {
		t.Fatalf("items[0]: %+v", data.Items[0])
	}
	// 密钥不泄漏
	if strings.Contains(out, "sk-SECRET") {
		t.Fatal("密钥泄漏进 list 输出")
	}

	// delete 无 --yes → CONFIRM_REQUIRED，且渠道仍在
	out = capture(t, func() {
		rc = Run([]string{"channels", "delete", "--config", cfg, "--site", "na1", "--id", "1"})
	})
	if rc != 1 || !strings.Contains(out, "CONFIRM_REQUIRED") {
		t.Fatalf("期望 CONFIRM_REQUIRED: rc=%d out=%s", rc, out)
	}
	// --yes → 删除成功
	out = capture(t, func() {
		rc = Run([]string{"channels", "delete", "--config", cfg, "--site", "na1", "--id", "1", "--yes"})
	})
	if rc != 0 {
		t.Fatalf("delete --yes rc=%d: %s", rc, out)
	}
	out = capture(t, func() {
		rc = Run([]string{"channels", "get", "--config", cfg, "--site", "na1", "--id", "1"})
	})
	if rc == 0 {
		t.Fatalf("删除后 get 应失败: %s", out)
	}
}

func TestChannelsSetStatusAndCheck(t *testing.T) {
	store := installCliStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	cfg := channelsAdminConfig(t, srv.URL, "https://unused.test", true)
	varDir := t.TempDir()

	// 基线
	var out string
	rc := 0
	out = capture(t, func() {
		rc = Run([]string{"channels", "check", "--config", cfg, "--site", "na1",
			"--var-dir", varDir})
	})
	if rc != 0 {
		t.Fatalf("基线 check rc=%d: %s", rc, out)
	}
	if !strings.Contains(out, `"baseline": true`) {
		t.Fatalf("应标注 baseline: %s", out)
	}

	// 禁用渠道 1 → check 应报 status_changed 且退出码 2
	out = capture(t, func() {
		rc = Run([]string{"channels", "set-status", "--config", cfg,
			"--site", "na1", "--id", "1", "--status", "disabled"})
	})
	if rc != 0 {
		t.Fatalf("set-status rc=%d: %s", rc, out)
	}
	out = capture(t, func() {
		rc = Run([]string{"channels", "check", "--config", cfg, "--site", "na1",
			"--var-dir", varDir})
	})
	if rc != 2 {
		t.Fatalf("有变化应退出码 2: rc=%d out=%s", rc, out)
	}
	if !strings.Contains(out, "status_changed") {
		t.Fatalf("缺 status_changed: %s", out)
	}
}

func TestChannelsAddFromBodyFile(t *testing.T) {
	store := installCliStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	cfg := channelsAdminConfig(t, srv.URL, "https://unused.test", true)
	bodyFile := filepath.Join(t.TempDir(), "channel.json")
	// 密钥只放在文件里（不进命令行）
	os.WriteFile(bodyFile, []byte(`{"name":"from-file","type":1,
		"key":"sk-file-secret","models":"gpt-4o-mini","group":"default"}`), 0o600)

	var out string
	rc := 0
	out = capture(t, func() {
		rc = Run([]string{"channels", "add", "--config", cfg, "--site", "na1",
			"--body-file", bodyFile})
	})
	if rc != 0 {
		t.Fatalf("add rc=%d: %s", rc, out)
	}
	var data struct {
		OK      bool `json:"ok"`
		Channel *struct {
			ChannelID int64  `json:"channel_id"`
			Name      string `json:"name"`
		} `json:"channel"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("输出: %s", out)
	}
	if !data.OK || data.Channel == nil || data.Channel.Name != "from-file" {
		t.Fatalf("add 返回: %s", out)
	}
	// 文件里的密钥不出现在输出
	if strings.Contains(out, "sk-file-secret") {
		t.Fatal("密钥泄漏进 add 输出")
	}
}
