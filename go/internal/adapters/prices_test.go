package adapters

import (
	"math"
	"testing"

	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"
	"opscollector/internal/records"
	"opscollector/internal/testmock"
)

// deviates 报告 a 与 b 的偏差是否超过容差（true = 偏差过大，断言应失败）。
func deviates(a, b float64) bool { return math.Abs(a-b) > 1e-9 }

// ---- new-api 价格计算 ----

func TestNewApiModelPrices(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	a, _ := Make(napiSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	prices, err := a.FetchModelPrices()
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[[2]string]records.ModelPrice{}
	for _, p := range prices {
		byKey[[2]string{p.Model, p.Group}] = p
	}

	// gpt-4o-mini @ default: input = 0.75×2×1.0 = 1.5 $/1M
	p := byKey[[2]string{"gpt-4o-mini", "default"}]
	if p.InputPerM == nil || deviates(*p.InputPerM, 1.5) {
		t.Fatalf("input 价错误: %v", p.InputPerM)
	}
	if p.OutputPerM == nil || deviates(*p.OutputPerM, 1.5) {
		t.Fatalf("output 价错误: %v", p.OutputPerM)
	}
	// 缓存读 = 0.75×0.5×2×1.0 = 0.75；缓存写 = 0.75×1.25×2 = 1.875
	if p.CacheReadPerM == nil || deviates(*p.CacheReadPerM, 0.75) {
		t.Fatalf("缓存读价错误: %v", p.CacheReadPerM)
	}
	if p.CacheCreatePerM == nil || deviates(*p.CacheCreatePerM, 1.875) {
		t.Fatalf("缓存写价错误: %v", p.CacheCreatePerM)
	}
	if p.Provenance != records.ProvNative || p.BillingType != "ratio" {
		t.Fatalf("provenance/billing 错误: %+v", p)
	}
	if p.CacheRatio == nil || *p.CacheRatio != 0.5 {
		t.Fatalf("cache_ratio 未采集: %v", p.CacheRatio)
	}

	// gpt-4o-mini @ vip（站点分组倍率 0.8）: input = 0.75×2×0.8 = 1.2（浮点容差）
	p = byKey[[2]string{"gpt-4o-mini", "vip"}]
	if p.InputPerM == nil || deviates(*p.InputPerM, 1.2) {
		t.Fatalf("vip input 价错误: %v", p.InputPerM)
	}
	if p.CacheReadPerM == nil || deviates(*p.CacheReadPerM, 0.6) {
		t.Fatalf("vip 缓存读价错误: %v", p.CacheReadPerM)
	}

	// claude-sonnet-4 @ vip（模型级覆盖倍率 0.5）:
	// output = 3.0×5.0×2×0.5 = 15；缓存读 = 3.0×0.1×2×0.5 = 0.3
	p = byKey[[2]string{"claude-sonnet-4", "vip"}]
	if p.OutputPerM == nil || deviates(*p.OutputPerM, 15.0) {
		t.Fatalf("模型级覆盖 output 价错误: %v", p.OutputPerM)
	}
	if p.CacheReadPerM == nil || deviates(*p.CacheReadPerM, 0.3) {
		t.Fatalf("模型级覆盖缓存价错误: %v", p.CacheReadPerM)
	}

	// sora-video 按次: 0.5 × 1.0 = 0.5 USD/次
	p = byKey[[2]string{"sora-video", "default"}]
	if p.BillingType != "per_call" || p.PerCall == nil || deviates(*p.PerCall, 0.5) {
		t.Fatalf("按次计费错误: %+v", p)
	}
	if p.InputPerM != nil {
		t.Fatal("按次模型不应有 per-token 价")
	}
}

func TestNewApiPresetRatioSync(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	a, _ := Make(napiSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	cfg, err := a.FetchPresetRatioConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg["source"] != "/api/ratio_config" {
		t.Fatalf("source 错误: %v", cfg["source"])
	}
	mr, ok := cfg["model_ratio"].(map[string]any)
	if !ok || mr["gpt-4o-mini"] != 0.75 {
		t.Fatalf("model_ratio 错误: %v", cfg["model_ratio"])
	}
}

// ---- sub2api 派生价格 ----

func TestSubModelPricesDerived(t *testing.T) {
	store := installStore(t)
	store["sb1:password"] = testmock.SUBPassword
	srv := testmock.Sub2ApiSite(false)
	defer srv.Close()
	a, _ := Make(subSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	prices, err := a.FetchModelPrices()
	if err != nil {
		t.Fatal(err)
	}
	if len(prices) == 0 {
		t.Fatal("无价格记录")
	}
	p := prices[0]
	if p.Model != "claude-sonnet-4" || p.Group != "3" {
		t.Fatalf("model/group 错误: %+v", p)
	}
	if p.Provenance != records.ProvDerived {
		t.Fatal("sub2api 价格应为 derived")
	}
	// input_cost 0.003 / 1200 tokens × 1e6 = 2.5 $/1M
	if p.InputPerM == nil || deviates(*p.InputPerM, 2.5) {
		t.Fatalf("input 派生价错误: %v", p.InputPerM)
	}
	// output_cost 0.012 / 800 × 1e6 = 15.0 $/1M
	if p.OutputPerM == nil || deviates(*p.OutputPerM, 15.0) {
		t.Fatalf("output 派生价错误: %v", p.OutputPerM)
	}
	// 生效倍率 = 用户专属 0.75（覆盖分组默认 0.8）
	if p.Extra["rate_multiplier"] != 0.75 {
		t.Fatalf("生效倍率上下文错误: %v", p.Extra)
	}
}

func TestSubPresetUnsupported(t *testing.T) {
	store := installStore(t)
	store["sb1:password"] = testmock.SUBPassword
	srv := testmock.Sub2ApiSite(false)
	defer srv.Close()
	a, _ := Make(subSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	if _, err := a.FetchPresetRatioConfig(); err == nil {
		t.Fatal("sub2api 应报 UNSUPPORTED")
	} else if ierr.CodeOf(err) != ierr.CodeUnsupported {
		t.Fatalf("错误码错误: %v", err)
	}
}

func TestSubUserRateOverride(t *testing.T) {
	store := installStore(t)
	store["sb1:password"] = testmock.SUBPassword
	srv := testmock.Sub2ApiSite(false)
	defer srv.Close()
	a, _ := Make(subSite(srv.URL), httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	ratios, err := a.FetchGroupRatios()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range ratios {
		if r.Group == "3" && r.Extra["group_name"] == "claude-pro" {
			// mock: available rate 0.8, user rates 3→0.75 → 覆盖为 0.75
			if deviates(r.Ratio, 0.75) {
				t.Fatalf("用户专属倍率未覆盖: %v", r.Ratio)
			}
			if deviates(r.Extra["default_rate_multiplier"].(float64), 0.8) {
				t.Fatalf("默认倍率未保留: %v", r.Extra)
			}
			return
		}
	}
	t.Fatal("claude-pro 分组缺失")
}
