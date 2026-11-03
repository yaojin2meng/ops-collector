package adapters

import (
	"testing"

	"opscollector/internal/config"
	"opscollector/internal/httpc"
	"opscollector/internal/records"
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

func napiSite(baseURL string) *config.SiteConfig {
	verify := true
	return &config.SiteConfig{
		SiteID: "na1", Kind: "new_api", BaseURL: baseURL,
		Auth: config.AuthConfig{Mode: "password", Username: "opuser",
			SecretRef: "na1:password"},
		Enabled: &verify, VerifyTLS: &verify,
	}
}

func subSite(baseURL string) *config.SiteConfig {
	verify := true
	return &config.SiteConfig{
		SiteID: "sb1", Kind: "sub2api", BaseURL: baseURL,
		Auth: config.AuthConfig{Mode: "password", Email: "ops@example.com",
			SecretRef: "sb1:password"},
		Enabled: &verify, VerifyTLS: &verify,
	}
}

// ---------------- new-api ----------------

func TestNewApiCapabilities(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	a, err := Make(napiSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	if err != nil {
		t.Fatal(err)
	}
	caps := a.Capabilities()
	if !caps.NativeTTFT || !caps.NativeSuccessRate || !caps.NativeTPS {
		t.Fatalf("caps: %+v", caps)
	}
}

func TestNewApiSiteMeta(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	a, _ := Make(napiSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	meta, err := a.SiteMeta()
	if err != nil {
		t.Fatal(err)
	}
	if meta["user_id"].(float64) != 42 || meta["username"] != "opuser" {
		t.Fatalf("meta: %v", meta)
	}
}

func TestNewApiLogsPagination(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	a, _ := Make(napiSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	recs, _, err := a.FetchLogs(nil, nil, 10, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 13 {
		t.Fatalf("期望 13 条，得到 %d", len(recs))
	}
	ids := map[string]bool{}
	for _, r := range recs {
		ids[r.RecordID] = true
		if r.SiteKind != "new_api" {
			t.Fatal("site_kind 错误")
		}
	}
	if len(ids) != 13 {
		t.Fatal("record_id 有重复")
	}
	r := recs[0]
	if r.Model == nil || *r.Model != "gpt-4o-mini" || r.Group == nil || *r.Group != "default" {
		t.Fatalf("字段映射错误: %+v", r)
	}
	if r.DurationMs == nil || *r.DurationMs != 3000 {
		t.Fatalf("use_time 秒→毫秒错误: %v", r.DurationMs)
	}
	if r.InputTokens == nil || r.OutputTokens == nil {
		t.Fatal("token 字段缺失")
	}
	if r.FirstTokenMs != nil {
		t.Fatal("new-api 日志逐条无 TTFT")
	}
	if r.Status != records.StatusSuccess {
		t.Fatal("type=1 应为 success")
	}
}

func TestNewApiGroupRatios(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	a, _ := Make(napiSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	ratios, err := a.FetchGroupRatios()
	if err != nil {
		t.Fatal(err)
	}
	site := map[string]records.GroupRatio{}
	for _, r := range ratios {
		if r.Scope == "site" {
			site[r.Group] = r
		}
	}
	if site["default"].Ratio != 1.0 || site["vip"].Ratio != 0.8 {
		t.Fatalf("site 倍率错误: %+v", site)
	}
	if site["vip"].Description == nil || *site["vip"].Description != "VIP 分组" {
		t.Fatalf("描述错误: %+v", site["vip"])
	}
	foundModelOverride := false
	for _, r := range ratios {
		if r.Scope == "model" && r.ModelName != nil && *r.ModelName == "claude-sonnet-4" &&
			r.Group == "vip" && r.Ratio == 0.5 {
			foundModelOverride = true
		}
	}
	if !foundModelOverride {
		t.Fatal("模型级覆盖倍率缺失")
	}
}

func TestNewApiPerformanceNative(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	a, _ := Make(napiSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	perf, err := a.FetchPerformance(nil, 24, 20)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[[2]string]records.PerfMetric{}
	for _, p := range perf {
		g := ""
		if p.Group != nil {
			g = *p.Group
		}
		byKey[[2]string{p.Model, g}] = p
	}
	p := byKey[[2]string{"gpt-4o-mini", "default"}]
	if p.AvgTTFTMs.Value == nil || *p.AvgTTFTMs.Value != 420 {
		t.Fatalf("原生 TTFT 错误: %+v", p.AvgTTFTMs)
	}
	if p.AvgTTFTMs.Provenance != records.ProvNative ||
		p.AvgOutputTPS.Provenance != records.ProvNative ||
		p.SuccessRate.Provenance != records.ProvNative {
		t.Fatalf("provenance 应为 native: %+v", p)
	}
	if p.AvgOutputTPS.Value == nil || *p.AvgOutputTPS.Value != 55.0 {
		t.Fatalf("原生 tps 错误: %+v", p.AvgOutputTPS)
	}
	vip := byKey[[2]string{"gpt-4o-mini", "vip"}]
	if vip.AvgTTFTMs.Value == nil || *vip.AvgTTFTMs.Value != 380 {
		t.Fatalf("vip TTFT 错误: %+v", vip.AvgTTFTMs)
	}
	claude := byKey[[2]string{"claude-sonnet-4", "default"}]
	if claude.AvgOutputTPS.Value == nil || *claude.AvgOutputTPS.Value != 31.0 {
		t.Fatalf("claude tps 错误: %+v", claude.AvgOutputTPS)
	}
}

// ---------------- sub2api ----------------

func TestSubCapabilities(t *testing.T) {
	store := installStore(t)
	store["sb1:password"] = testmock.SUBPassword
	srv := testmock.Sub2ApiSite(false)
	defer srv.Close()
	a, _ := Make(subSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	if a.Capabilities().NativeTTFT {
		t.Fatal("sub2api 无原生 TTFT 聚合")
	}
}

func TestSubSiteMeta(t *testing.T) {
	store := installStore(t)
	store["sb1:password"] = testmock.SUBPassword
	srv := testmock.Sub2ApiSite(false)
	defer srv.Close()
	a, _ := Make(subSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	meta, err := a.SiteMeta()
	if err != nil {
		t.Fatal(err)
	}
	if meta["user_id"].(float64) != 7 || meta["username"] != "ops@example.com" {
		t.Fatalf("meta: %v", meta)
	}
}

func TestSubLogsWithFirstToken(t *testing.T) {
	store := installStore(t)
	store["sb1:password"] = testmock.SUBPassword
	srv := testmock.Sub2ApiSite(false)
	defer srv.Close()
	a, _ := Make(subSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	recs, _, err := a.FetchLogs(nil, nil, 5, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 8 {
		t.Fatalf("期望 8 条，得到 %d", len(recs))
	}
	r := recs[0]
	if r.FirstTokenMs == nil || *r.FirstTokenMs <= 0 {
		t.Fatal("first_token_ms 缺失")
	}
	if r.Status != records.StatusSuccess {
		t.Fatal("usage 记录应为 success")
	}
	if r.DurationMs == nil || r.OutputTokens == nil {
		t.Fatal("duration/output 缺失")
	}
	if r.Group == nil || *r.Group != "3" {
		t.Fatalf("group_id 映射错误: %v", r.Group)
	}
}

func TestSubGroupRatios(t *testing.T) {
	store := installStore(t)
	store["sb1:password"] = testmock.SUBPassword
	srv := testmock.Sub2ApiSite(false)
	defer srv.Close()
	a, _ := Make(subSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	ratios, err := a.FetchGroupRatios()
	if err != nil {
		t.Fatal(err)
	}
	var claude *records.GroupRatio
	for i := range ratios {
		// join 键统一：Group 恒为 gid 字符串；人类名在 extra.group_name
		if ratios[i].Group == "3" && ratios[i].Extra["group_name"] == "claude-pro" {
			claude = &ratios[i]
		}
	}
	if claude == nil {
		t.Fatalf("claude-pro 分组缺失: %+v", ratios)
	}
	// 用户专属倍率 0.75 覆盖分组默认 0.8（default 保留在 extra）
	if claude.Ratio != 0.75 {
		t.Fatalf("claude-pro 倍率应被用户专属倍率覆盖: %+v", claude)
	}
	if claude.Extra["default_rate_multiplier"] != 0.8 {
		t.Fatalf("默认倍率未保留: %v", claude.Extra)
	}
	// 不再有 _user_rates_raw 哨兵记录
	for _, r := range ratios {
		if r.Group == "_user_rates_raw" {
			t.Fatal("哨兵记录不应存在")
		}
	}
}

func TestSubPerformanceDerived(t *testing.T) {
	store := installStore(t)
	store["sb1:password"] = testmock.SUBPassword
	srv := testmock.Sub2ApiSite(false)
	defer srv.Close()
	a, _ := Make(subSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	perf, err := a.FetchPerformance(nil, 24, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(perf) == 0 {
		t.Fatal("无性能指标")
	}
	p := perf[0]
	if p.AvgTTFTMs.Provenance != records.ProvDerived ||
		p.AvgOutputTPS.Provenance != records.ProvDerived ||
		p.SuccessRate.Provenance != records.ProvDerived {
		t.Fatalf("sub2api 指标应为 derived: %+v", p)
	}
	// 成功率 = 8/(8+1) ≈ 88.89%
	if p.SuccessRate.Value == nil || *p.SuccessRate.Value < 88.88 || *p.SuccessRate.Value > 88.90 {
		t.Fatalf("成功率错误: %+v", p.SuccessRate)
	}
	// tps 派生：第一条 usage 800/((9000-350)/1000) ≈ 92.49，聚合均值应 > 0
	if p.AvgOutputTPS.Value == nil || *p.AvgOutputTPS.Value <= 0 {
		t.Fatalf("tps 派生错误: %+v", p.AvgOutputTPS)
	}
}
