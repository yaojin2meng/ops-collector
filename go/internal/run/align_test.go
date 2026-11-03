package run

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"opscollector/internal/config"
	"opscollector/internal/secrets"
	"opscollector/internal/testmock"
)

// 模型 → 上游渠道映射对齐：规则覆盖 native、admin 渠道名回查、counts 上报。
func TestLogAlignmentEndToEnd(t *testing.T) {
	m := map[string]string{}
	secrets.SetKeyringForTest(
		func(e string) (string, error) { return m[e], nil },
		func(e, v string) error { m[e] = v; return nil },
		func(e string) error { delete(m, e); return nil },
	)
	t.Cleanup(secrets.ResetKeyring)
	na := testmock.NewApiSite()
	defer na.Close()
	_ = secrets.SetSecret("na1:password", testmock.NAPIPassword)

	verify, admin := true, true
	cfg := &config.Config{
		Defaults: config.DefaultsConfig{
			WindowsHours: []int{1}, Concurrency: 1,
			MinIntervalS: func() *float64 { v := 0.0; return &v }(),
			Retries:      1, BackoffBaseS: 0.001,
		},
		Sites: []*config.SiteConfig{
			{SiteID: "na1", Kind: "new_api", BaseURL: na.URL,
				Auth: config.AuthConfig{Mode: "password", Username: "opuser",
					SecretRef: "na1:password"},
				Enabled: &verify, Admin: &admin,
				LogAlignment: []config.AlignmentRule{
					// gpt-4o-* 指定到渠道 2（覆盖日志 native 的渠道 1），
					// 多Key渠道用 key_label 精确到泳道
					{Models: []string{"gpt-4o-*"}, ChannelID: 2, KeyLabel: "supplierA-main"},
				}},
		},
	}

	varDir := filepath.Join(t.TempDir(), "var")
	summary, err := Collect(cfg, Options{VarDir: varDir})
	if err != nil {
		t.Fatal(err)
	}
	sr := summary.Sites[0]
	if sr.Status != "success" {
		t.Fatalf("status=%s err=%s", sr.Status, sr.ErrorMessage)
	}
	if sr.Counts["aligned_rule"] == 0 || sr.Counts["aligned_native"] == 0 {
		t.Fatalf("对齐 counts 异常: %+v", sr.Counts)
	}

	b, err := os.ReadFile(filepath.Join(varDir, "runs", summary.RunID, "site-na1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var art struct {
		CallLogs []struct {
			Model       string  `json:"model"`
			ChannelID   *string `json:"channel_id"`
			ChannelName *string `json:"channel_name"`
			ChannelKey  *string `json:"channel_key_label"`
			ChannelSrc  *string `json:"channel_source"`
		} `json:"call_logs"`
	}
	if err := json.Unmarshal(b, &art); err != nil {
		t.Fatal(err)
	}
	ruleHits, nativeHits := 0, 0
	for _, l := range art.CallLogs {
		switch {
		case l.Model == "gpt-4o-mini":
			if l.ChannelID == nil || *l.ChannelID != "2" {
				t.Fatalf("规则应把 gpt-4o-mini 映射到渠道 2: %v", l.ChannelID)
			}
			if l.ChannelKey == nil || *l.ChannelKey != "supplierA-main" {
				t.Fatalf("key 泳道标签缺失: %v", l.ChannelKey)
			}
			if l.ChannelSrc == nil || *l.ChannelSrc != "rule" {
				t.Fatalf("来源应为 rule: %v", l.ChannelSrc)
			}
			// 渠道 2 的名称由 admin 面回查（claude-backup）
			if l.ChannelName == nil || *l.ChannelName != "claude-backup" {
				t.Fatalf("规则渠道名回查失败: %v", l.ChannelName)
			}
			ruleHits++
		case l.Model == "claude-sonnet-4":
			if l.ChannelID == nil || *l.ChannelID != "2" {
				t.Fatalf("native 渠道 id 错误: %v", l.ChannelID)
			}
			if l.ChannelSrc == nil || *l.ChannelSrc != "native" {
				t.Fatalf("来源应为 native: %v", l.ChannelSrc)
			}
			if l.ChannelName == nil || *l.ChannelName != "claude-backup" {
				t.Fatalf("native 渠道名回查失败: %v", l.ChannelName)
			}
			nativeHits++
		}
	}
	if ruleHits == 0 || nativeHits == 0 {
		t.Fatalf("对齐覆盖异常: rule=%d native=%d", ruleHits, nativeHits)
	}
}
