package run

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"opscollector/internal/config"
	"opscollector/internal/secrets"
	"opscollector/internal/testmock"
)

// 分页截断：数据有洞时绝不推进断点，artifact 标注 truncated。
func TestTruncatedLogsNoCheckpointAdvance(t *testing.T) {
	m := map[string]string{}
	secrets.SetKeyringForTest(
		func(e string) (string, error) { return m[e], nil },
		func(e, v string) error { m[e] = v; return nil },
		func(e string) error { delete(m, e); return nil },
	)
	t.Cleanup(secrets.ResetKeyring)
	na := testmock.NewApiSite()
	defer na.Close()
	sb := testmock.Sub2ApiSite(false)
	defer sb.Close()
	_ = secrets.SetSecret("na1:password", testmock.NAPIPassword)
	_ = secrets.SetSecret("sb1:password", testmock.SUBPassword)

	verify := true
	disabled := false
	cfg := &config.Config{
		Defaults: config.DefaultsConfig{
			WindowsHours: []int{1}, Concurrency: 1,
			MinIntervalS: func() *float64 { v := 0.0; return &v }(),
			Retries:      1, BackoffBaseS: 0.001,
			// 强制截断：mock 有 14 条日志，每页 1 条、只拉 1 页
			PageSize: 1, MaxPagesPerResource: 1,
		},
		Sites: []*config.SiteConfig{
			{SiteID: "na1", Kind: "new_api", BaseURL: na.URL,
				Auth: config.AuthConfig{Mode: "password", Username: "opuser",
					SecretRef: "na1:password"},
				Enabled: &verify},
			{SiteID: "sb1", Kind: "sub2api", BaseURL: sb.URL,
				Auth: config.AuthConfig{Mode: "password", Email: "ops@example.com",
					SecretRef: "sb1:password"},
				Enabled: &disabled},
		},
	}

	varDir := filepath.Join(t.TempDir(), "var")
	summary, err := Collect(cfg, Options{VarDir: varDir, Incremental: true,
		SiteIDs: []string{"na1", "sb1"}})
	if err != nil {
		t.Fatal(err)
	}
	// sb1 被禁用 → 如实回报（agent 行动性），na1 正常采集
	if summary.Status != "success" || len(summary.Sites) != 1 {
		t.Fatalf("status=%s sites=%d", summary.Status, len(summary.Sites))
	}
	if len(summary.DisabledSiteIDs) != 1 || summary.DisabledSiteIDs[0] != "sb1" {
		t.Fatalf("disabled_site_ids 缺失: %+v", summary)
	}
	// 断点未写入（截轮不推进）
	if _, err := os.Stat(filepath.Join(varDir, "state", "na1.json")); err == nil {
		t.Fatal("截断时断点不应推进")
	}
	// artifact 标注 truncated
	b, err := os.ReadFile(filepath.Join(varDir, "runs", summary.RunID, "site-na1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var art map[string]any
	if json.Unmarshal(b, &art) != nil {
		t.Fatal("artifact 解析失败")
	}
	if art["call_logs_truncated"] != true {
		t.Fatalf("缺少 truncated 标注: %v", art["call_logs_truncated"])
	}
	if n, _ := art["call_logs"].([]any); len(n) != 1 {
		t.Fatalf("截断后应只有 1 条: %d", len(n))
	}
	_ = strings.TrimSpace
}
