package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func alignmentSite(rules []map[string]any) map[string]any {
	return map[string]any{
		"site_id": "s1", "kind": "new_api", "base_url": "https://s1.test",
		"auth":          map[string]any{"mode": "password", "username": "u", "secret_ref": "s1:password"},
		"log_alignment": rules,
	}
}

func writeLoad(t *testing.T, sites []any) (*Config, error) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"schema_version": 1, "sites": sites})
	p := filepath.Join(t.TempDir(), "collector.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestAlignmentRuleValid(t *testing.T) {
	cfg, err := writeLoad(t, []any{alignmentSite([]map[string]any{
		{"models": []string{"gpt-4o", "claude-*"}, "channel_id": 7, "key_label": "supplierA-main"},
	})})
	if err != nil {
		t.Fatal(err)
	}
	rules := cfg.Sites[0].LogAlignment
	if len(rules) != 1 || rules[0].ChannelID != 7 || rules[0].KeyLabel != "supplierA-main" {
		t.Fatalf("rules: %+v", rules)
	}
}

func TestAlignmentRuleRejectsRawKey(t *testing.T) {
	_, err := writeLoad(t, []any{alignmentSite([]map[string]any{
		{"models": []string{"gpt-4o"}, "channel_id": 7,
			"key_label": "sk-proj-AbCdEf123456789012345678901234567890"},
	})})
	if err == nil || !strings.Contains(err.Error(), "key_label") {
		t.Fatalf("应拒绝真实密钥进配置: %v", err)
	}
}

func TestAlignmentRuleRejectsMissingModels(t *testing.T) {
	_, err := writeLoad(t, []any{alignmentSite([]map[string]any{
		{"channel_id": 7},
	})})
	if err == nil || !strings.Contains(err.Error(), "models") {
		t.Fatalf("应拒绝缺 models: %v", err)
	}
}

func TestAlignmentRuleRejectsBadChannelID(t *testing.T) {
	_, err := writeLoad(t, []any{alignmentSite([]map[string]any{
		{"models": []string{"gpt-4o"}, "channel_id": 0},
	})})
	if err == nil || !strings.Contains(err.Error(), "channel_id") {
		t.Fatalf("应拒绝非法 channel_id: %v", err)
	}
}
