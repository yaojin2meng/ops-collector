package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capture 临时替换 stdout 执行 fn，返回捕获的输出。
func capture(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String()
}

func TestInitAndValidate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config", "collector.json")
	if rc := Run([]string{"init", "--config", cfgPath}); rc != 0 {
		t.Fatalf("init rc=%d", rc)
	}
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if json.Unmarshal(b, &data) != nil || data["schema_version"].(float64) != 1 {
		t.Fatal("示例配置不合法")
	}
	// 二次 init 不覆盖
	if rc := Run([]string{"init", "--config", cfgPath}); rc != 1 {
		t.Fatalf("二次 init 应返回 1，得到 %d", rc)
	}
	if rc := Run([]string{"init", "--config", cfgPath, "--force"}); rc != 0 {
		t.Fatal("force init 失败")
	}
	// validate
	out := capture(t, func() {
		if rc := Run([]string{"validate-config", "--config", cfgPath}); rc != 0 {
			t.Errorf("validate rc=%d", rc)
		}
	})
	var v map[string]any
	if json.Unmarshal([]byte(out), &v) != nil || v["ok"] != true {
		t.Fatalf("validate 输出: %s", out)
	}
}

func TestStdoutPureJSON(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "collector.json")
	out := capture(t, func() { Run([]string{"init", "--config", cfgPath}) })
	// stdout 全部可解析为 JSON
	var v any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		t.Fatalf("stdout 不是纯 JSON: %q", out)
	}
}

func TestValidateBadConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.json")
	os.WriteFile(p, []byte(`{"schema_version": 7, "sites": []}`), 0o644)
	if rc := Run([]string{"validate-config", "--config", p}); rc != 1 {
		t.Fatalf("rc=%d", rc)
	}
}

func TestExportFlatRows(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run-abc")
	os.MkdirAll(runDir, 0o755)
	os.WriteFile(filepath.Join(runDir, "site-na1.json"), []byte(`{
		"site_id": "na1",
		"performance": [{
			"model": "gpt-4o-mini", "group": "default", "window_hours": 24,
			"success_rate": {"value": 99.5, "provenance": "native"},
			"avg_ttft_ms": {"value": 420, "provenance": "native"},
			"avg_output_tps": {"value": 55.0, "provenance": "native"}
		}]
	}`), 0o644)
	var out string
	rc := 0
	out = capture(t, func() { rc = Run([]string{"export", runDir}) })
	if rc != 0 {
		t.Fatalf("export rc=%d", rc)
	}
	var data struct {
		OK   bool `json:"ok"`
		Rows []struct {
			SiteID      string  `json:"site_id"`
			Model       string  `json:"model"`
			AvgTTFTMs   float64 `json:"avg_ttft_ms"`
			SuccessRate float64 `json:"success_rate"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("export 输出: %s", out)
	}
	if !data.OK || len(data.Rows) != 1 ||
		data.Rows[0].Model != "gpt-4o-mini" || data.Rows[0].AvgTTFTMs != 420 {
		t.Fatalf("rows: %+v", data)
	}
}

func TestSecretsHint(t *testing.T) {
	out := capture(t, func() {
		if rc := Run([]string{"secrets", "hint"}); rc != 0 {
			t.Errorf("rc=%d", rc)
		}
	})
	var v map[string]any
	if json.Unmarshal([]byte(out), &v) != nil || v["ok"] != true {
		t.Fatalf("hint 输出: %s", out)
	}
}

func TestPricesCommand(t *testing.T) {
	dir := t.TempDir()
	runDir := filepath.Join(dir, "run-xyz")
	os.MkdirAll(runDir, 0o755)
	os.WriteFile(filepath.Join(runDir, "site-na1.json"), []byte(`{
		"site_id": "na1",
		"model_prices": [
			{"model": "gpt-4o-mini", "group": "default", "billing_type": "ratio",
			 "input_per_m_usd": 1.5, "output_per_m_usd": 1.5,
			 "cache_read_per_m_usd": 0.75, "group_ratio": 1.0,
			 "provenance": "native"},
			{"model": "sora-video", "group": "default", "billing_type": "per_call",
			 "per_call_usd": 0.5, "group_ratio": 1.0, "provenance": "native"}
		],
		"preset_ratio_config": {"source": "/api/ratio_config", "model_ratio": {"gpt-4o-mini": 0.75}}
	}`), 0o644)
	var out string
	rc := 0
	out = capture(t, func() { rc = Run([]string{"prices", runDir}) })
	if rc != 0 {
		t.Fatalf("prices rc=%d", rc)
	}
	var data struct {
		OK   bool `json:"ok"`
		Rows []struct {
			Model         string   `json:"model"`
			BillingType   string   `json:"billing_type"`
			InputPerM     *float64 `json:"input_per_m_usd"`
			CacheReadPerM *float64 `json:"cache_read_per_m_usd"`
			PerCall       *float64 `json:"per_call_usd"`
		} `json:"rows"`
		Presets map[string]any `json:"preset_ratio_config"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("prices 输出: %s", out)
	}
	if !data.OK || len(data.Rows) != 2 {
		t.Fatalf("rows: %+v", data)
	}
	if data.Rows[0].InputPerM == nil || *data.Rows[0].InputPerM != 1.5 {
		t.Fatal("input 价错误")
	}
	if data.Rows[0].CacheReadPerM == nil || *data.Rows[0].CacheReadPerM != 0.75 {
		t.Fatal("缓存价错误")
	}
	if data.Rows[1].PerCall == nil || *data.Rows[1].PerCall != 0.5 {
		t.Fatal("按次价错误")
	}
	if data.Presets["na1"] == nil {
		t.Fatal("预设倍率配置缺失")
	}
}

func TestUnknownCommand(t *testing.T) {
	if rc := Run([]string{"nope"}); rc != 64 {
		t.Fatalf("未知命令应返回 64（用法错误），得到 %d", rc)
	}
}
