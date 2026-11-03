package run

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"opscollector/internal/config"
	ierr "opscollector/internal/errors"
	"opscollector/internal/secrets"
	"opscollector/internal/testmock"
)

func installStore(t *testing.T) map[string]string {
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

func twoSiteConfig(naURL, sbURL string) *config.Config {
	verify := true
	noInterval := 0.0
	return &config.Config{
		Defaults: config.DefaultsConfig{
			WindowsHours: []int{1, 24}, Concurrency: 2,
			MinIntervalS: &noInterval, Retries: 1, BackoffBaseS: 0.001,
		},
		Sites: []*config.SiteConfig{
			{SiteID: "na1", Kind: "new_api", BaseURL: naURL,
				Auth: config.AuthConfig{Mode: "password", Username: "opuser",
					SecretRef: "na1:password"},
				Enabled: &verify},
			{SiteID: "sb1", Kind: "sub2api", BaseURL: sbURL,
				Auth: config.AuthConfig{Mode: "password", Email: "ops@example.com",
					SecretRef: "sb1:password"},
				Enabled: &verify},
		},
	}
}

func TestRunTwoSitesSuccess(t *testing.T) {
	installStore(t)
	na := testmock.NewApiSite()
	defer na.Close()
	sb := testmock.Sub2ApiSite(false)
	defer sb.Close()
	// 注入密码（keyring 被替换为内存实现）
	_ = secrets.SetSecret("na1:password", testmock.NAPIPassword)
	_ = secrets.SetSecret("sb1:password", testmock.SUBPassword)

	varDir := filepath.Join(t.TempDir(), "var")
	summary, err := Collect(twoSiteConfig(na.URL, sb.URL),
		Options{VarDir: varDir, Incremental: true})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Status != "success" {
		t.Fatalf("status = %s, sites=%+v", summary.Status, summary.Sites)
	}
	if len(summary.Sites) != 2 {
		t.Fatal("期望 2 个站点")
	}
	for _, s := range summary.Sites {
		if s.Status != "success" {
			t.Fatalf("站点 %s 状态 %s: %s", s.SiteID, s.Status, s.ErrorMessage)
		}
		if s.Counts["call_logs"] <= 0 || s.Counts["group_ratios"] <= 0 || s.Counts["performance"] <= 0 {
			t.Fatalf("counts 异常: %+v", s.Counts)
		}
		if s.Artifact == "" {
			t.Fatal("artifact 路径缺失")
		}
	}
	// artifact 内容抽验
	b, err := os.ReadFile(filepath.Join(varDir, "runs", summary.RunID, "site-na1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var art struct {
		SiteKind     string `json:"site_kind"`
		Capabilities map[string]bool
		Performance  []struct {
			Model     string `json:"model"`
			AvgTTFTMs struct {
				Value      *float64 `json:"value"`
				Provenance string   `json:"provenance"`
			} `json:"avg_ttft_ms"`
		} `json:"performance"`
		ModelPrices []struct {
			Model     string   `json:"model"`
			Group     string   `json:"group"`
			InputPerM *float64 `json:"input_per_m_usd"`
		} `json:"model_prices"`
		PresetRatioConfig map[string]any `json:"preset_ratio_config"`
	}
	if err := json.Unmarshal(b, &art); err != nil {
		t.Fatal(err)
	}
	if art.SiteKind != "new_api" || !art.Capabilities["native_ttft"] {
		t.Fatalf("artifact 头错误: %s %v", art.SiteKind, art.Capabilities)
	}
	// 模型价格 + 官方预设同步
	if len(art.ModelPrices) == 0 {
		t.Fatal("model_prices 缺失")
	}
	if art.PresetRatioConfig == nil || art.PresetRatioConfig["source"] != "/api/ratio_config" {
		t.Fatalf("preset_ratio_config 缺失: %v", art.PresetRatioConfig)
	}
	foundPrice := false
	for _, p := range art.ModelPrices {
		if p.Model == "gpt-4o-mini" && p.Group == "default" &&
			p.InputPerM != nil && *p.InputPerM == 1.5 {
			foundPrice = true
		}
	}
	if !foundPrice {
		t.Fatal("gpt-4o-mini default 输入价 1.5 未出现在 artifact")
	}
	foundTTFT := false
	for _, p := range art.Performance {
		if p.Model == "gpt-4o-mini" && p.AvgTTFTMs.Value != nil && *p.AvgTTFTMs.Value == 420 {
			foundTTFT = true
		}
	}
	if !foundTTFT {
		t.Fatal("原生 TTFT 420 未出现在 artifact")
	}
	// run.json 存在
	if _, err := os.Stat(filepath.Join(varDir, "runs", summary.RunID, "run.json")); err != nil {
		t.Fatal("run.json 缺失")
	}
}

func TestRunIncrementalCheckpoint(t *testing.T) {
	installStore(t)
	na := testmock.NewApiSite()
	defer na.Close()
	sb := testmock.Sub2ApiSite(false)
	defer sb.Close()
	_ = secrets.SetSecret("na1:password", testmock.NAPIPassword)
	_ = secrets.SetSecret("sb1:password", testmock.SUBPassword)

	varDir := filepath.Join(t.TempDir(), "var")
	s1, err := Collect(twoSiteConfig(na.URL, sb.URL), Options{VarDir: varDir})
	if err != nil || s1.Status != "success" {
		t.Fatalf("%v %s", err, s1.Status)
	}
	cpPath := filepath.Join(varDir, "state", "na1.json")
	if _, err := os.Stat(cpPath); err != nil {
		t.Fatal("断点未写入")
	}
	b, _ := os.ReadFile(cpPath)
	var m map[string]any
	if json.Unmarshal(b, &m) != nil || m["last_log_timestamp"].(float64) <= 0 {
		t.Fatal("断点内容错误")
	}
}

func TestRunSingleSiteFilter(t *testing.T) {
	installStore(t)
	na := testmock.NewApiSite()
	defer na.Close()
	sb := testmock.Sub2ApiSite(false)
	defer sb.Close()
	_ = secrets.SetSecret("na1:password", testmock.NAPIPassword)
	_ = secrets.SetSecret("sb1:password", testmock.SUBPassword)

	summary, err := Collect(twoSiteConfig(na.URL, sb.URL),
		Options{VarDir: t.TempDir(), SiteIDs: []string{"na1"}})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Status != "success" || len(summary.Sites) != 1 ||
		summary.Sites[0].SiteID != "na1" {
		t.Fatalf("过滤失败: %+v", summary.Sites)
	}
}

func TestRunSiteFailureIsolated(t *testing.T) {
	installStore(t)
	na := testmock.NewApiSite()
	defer na.Close()
	sb := testmock.Sub2ApiSite(true) // broken: 全部 500
	defer sb.Close()
	_ = secrets.SetSecret("na1:password", testmock.NAPIPassword)
	_ = secrets.SetSecret("sb1:password", testmock.SUBPassword)

	summary, err := Collect(twoSiteConfig(na.URL, sb.URL), Options{VarDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Status != "partial" {
		t.Fatalf("期望 partial，得到 %s", summary.Status)
	}
	statuses := map[string]string{}
	for _, s := range summary.Sites {
		statuses[s.SiteID] = s.Status
	}
	if statuses["na1"] != "success" || statuses["sb1"] != "failed" {
		t.Fatalf("隔离失败: %v", statuses)
	}
}

func TestRunAuthFailureNoLeak(t *testing.T) {
	store := installStore(t)
	na := testmock.NewApiSite()
	defer na.Close()
	sb := testmock.Sub2ApiSite(false)
	defer sb.Close()
	_ = secrets.SetSecret("na1:password", testmock.NAPIPassword)
	wrong := "totally-wrong-pw"
	_ = secrets.SetSecret("sb1:password", wrong)

	summary, err := Collect(twoSiteConfig(na.URL, sb.URL), Options{VarDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	var sbResult *SiteResult
	for i := range summary.Sites {
		if summary.Sites[i].SiteID == "sb1" {
			sbResult = &summary.Sites[i]
		}
	}
	if sbResult == nil || sbResult.Status != "failed" {
		t.Fatalf("sb1 应失败: %+v", sbResult)
	}
	full, _ := json.Marshal(summary)
	if strings.Contains(string(full), wrong) {
		t.Fatal("密码泄漏进 run summary")
	}
	if strings.Contains(sbResult.ErrorMessage, wrong) {
		t.Fatal("密码泄漏进错误消息")
	}
	_ = store
}

func TestRunSiteNotFound(t *testing.T) {
	installStore(t)
	na := testmock.NewApiSite()
	defer na.Close()
	sb := testmock.Sub2ApiSite(false)
	defer sb.Close()
	summary, err := Collect(twoSiteConfig(na.URL, sb.URL),
		Options{VarDir: t.TempDir(), SiteIDs: []string{"ghost"}})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Status != "failed" || len(summary.Sites) != 0 {
		t.Fatalf("未知站点应失败: %+v", summary)
	}
}

var _ = ierr.CodeInternal
