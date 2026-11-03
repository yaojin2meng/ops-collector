package align

import (
	"testing"

	"opscollector/internal/config"
	"opscollector/internal/records"
)

func strPtr(s string) *string { return &s }

func TestRuleExactMatchOverridesNative(t *testing.T) {
	rules := []config.AlignmentRule{
		{Models: []string{"gpt-4o"}, ChannelID: 7, KeyLabel: "supplierA-main"},
	}
	recs := []records.CallRecord{
		{Model: strPtr("gpt-4o"), ChannelID: strPtr("1")},
	}
	out, stats := Apply(recs, rules, map[int64]string{7: "supplierA-chan", 1: "openai-main"})
	if stats.Rule != 1 || stats.Native != 0 || stats.Unaligned != 0 {
		t.Fatalf("stats: %+v", stats)
	}
	r := out[0]
	if r.ChannelID == nil || *r.ChannelID != "7" {
		t.Fatalf("规则应覆盖 native: %v", r.ChannelID)
	}
	if r.ChannelKeyLabel == nil || *r.ChannelKeyLabel != "supplierA-main" {
		t.Fatalf("key 泳道标签缺失: %v", r.ChannelKeyLabel)
	}
	if r.ChannelName == nil || *r.ChannelName != "supplierA-chan" {
		t.Fatalf("规则渠道名回查失败: %v", r.ChannelName)
	}
	if r.ChannelSource == nil || *r.ChannelSource != "rule" {
		t.Fatalf("来源标注错误: %v", r.ChannelSource)
	}
}

func TestGlobMatch(t *testing.T) {
	rules := []config.AlignmentRule{
		{Models: []string{"claude-*"}, ChannelID: 2},
	}
	cases := map[string]bool{
		"claude-sonnet-4": true,
		"claude-opus-4":   true,
		"claude":          false, // "claude-*" 要求带后缀；裸名需另行列出
		"gpt-4o":          false,
		"openai/claude-x": false, // path.Match 的 * 不跨 /
	}
	for model, want := range cases {
		recs := []records.CallRecord{{Model: strPtr(model)}}
		_, stats := Apply(recs, rules, nil)
		got := stats.Rule == 1
		if got != want {
			t.Fatalf("model=%q 期望 %v 得到 %v", model, want, got)
		}
	}
}

func TestStarMatchesAll(t *testing.T) {
	rules := []config.AlignmentRule{{Models: []string{"*"}, ChannelID: 9}}
	recs := []records.CallRecord{{Model: strPtr("anything")}}
	_, stats := Apply(recs, rules, nil)
	if stats.Rule != 1 {
		t.Fatalf("* 应匹配所有模型: %+v", stats)
	}
}

func TestNativeJoinName(t *testing.T) {
	recs := []records.CallRecord{{Model: strPtr("m1"), ChannelID: strPtr("3")}}
	out, stats := Apply(recs, nil, map[int64]string{3: "chan-three"})
	if stats.Native != 1 {
		t.Fatalf("stats: %+v", stats)
	}
	if out[0].ChannelName == nil || *out[0].ChannelName != "chan-three" {
		t.Fatalf("native 名称回查失败: %v", out[0].ChannelName)
	}
	if out[0].ChannelSource == nil || *out[0].ChannelSource != "native" {
		t.Fatalf("来源应为 native: %v", out[0].ChannelSource)
	}
}

func TestUnalignedHonestEmpty(t *testing.T) {
	recs := []records.CallRecord{{Model: strPtr("m1")}}
	out, stats := Apply(recs, nil, nil)
	if stats.Unaligned != 1 || stats.Native != 0 || stats.Rule != 0 {
		t.Fatalf("stats: %+v", stats)
	}
	if out[0].ChannelID != nil || out[0].ChannelSource != nil {
		t.Fatal("未对齐应保持空值（不猜测）")
	}
}

func TestFirstMatchingRuleWins(t *testing.T) {
	rules := []config.AlignmentRule{
		{Models: []string{"gpt-4o-*"}, ChannelID: 1},
		{Models: []string{"gpt-4o-mini"}, ChannelID: 2},
	}
	recs := []records.CallRecord{{Model: strPtr("gpt-4o-mini")}}
	out, _ := Apply(recs, rules, nil)
	if out[0].ChannelID == nil || *out[0].ChannelID != "1" {
		t.Fatalf("应取配置顺序第一条命中规则: %v", out[0].ChannelID)
	}
}

func TestNativeZeroChannelTreatedAsMissing(t *testing.T) {
	recs := []records.CallRecord{{Model: strPtr("m1"), ChannelID: strPtr("0")}}
	_, stats := Apply(recs, nil, nil)
	if stats.Unaligned != 1 {
		t.Fatalf("channel=0 应视为无归属: %+v", stats)
	}
}
