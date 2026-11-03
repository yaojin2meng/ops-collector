package metrics

import (
	"testing"

	"opscollector/internal/records"
)

func rec(i int64, status records.CallStatus, model, group string,
	output int64, durationMs, firstTokenMs *int64) records.CallRecord {
	m, g := model, group
	return records.CallRecord{
		SiteID: "t", SiteKind: "new_api", RecordID: string(rune('a' + i)),
		ObservedAt: 1000 + i, Model: &m, Group: &g, Status: status,
		OutputTokens: &output, DurationMs: durationMs, FirstTokenMs: firstTokenMs,
		InputTokens: i64(10),
	}
}

func i64(v int64) *int64 { return &v }

func TestAggregateSuccessRate(t *testing.T) {
	recs := []records.CallRecord{
		rec(0, records.StatusSuccess, "m1", "g1", 100, i64(2000), i64(500)),
		rec(1, records.StatusSuccess, "m1", "g1", 100, i64(2000), i64(500)),
		rec(2, records.StatusError, "m1", "g1", 0, nil, nil),
		// unknown 但带时长/首字（保留在桶里，不进成功率分母）
		rec(3, records.StatusUnknown, "m1", "g1", 100, i64(2000), i64(500)),
	}
	out := Aggregate(recs, "t", "new_api", 24)
	if len(out) != 1 {
		t.Fatalf("期望 1 个桶，得到 %d", len(out))
	}
	p := out[0]
	if p.SuccessRate.Value == nil || abs(*p.SuccessRate.Value-66.6667) > 0.01 {
		t.Fatalf("成功率错误: %+v", p.SuccessRate)
	}
	if p.SuccessRate.Provenance != records.ProvDerived {
		t.Fatal("provenance 应为 derived")
	}
	if p.RequestCount == nil || *p.RequestCount != 4 {
		t.Fatalf("request_count 错误: %v", p.RequestCount)
	}
}

func TestAggregateTPSFormula(t *testing.T) {
	// output=100, duration=2000ms, first_token=500ms → 100/1.5 ≈ 66.67
	recs := []records.CallRecord{
		rec(0, records.StatusSuccess, "m1", "g1", 100, i64(2000), i64(500)),
	}
	p := Aggregate(recs, "t", "new_api", 1)[0]
	if p.AvgOutputTPS.Value == nil || abs(*p.AvgOutputTPS.Value-100.0/1.5) > 0.01 {
		t.Fatalf("tps 错误: %+v", p.AvgOutputTPS)
	}
	if p.AvgOutputTPS.Provenance != records.ProvDerived {
		t.Fatal("tps provenance 应为 derived")
	}
}

func TestAggregateTTFTMean(t *testing.T) {
	recs := []records.CallRecord{
		rec(0, records.StatusSuccess, "m1", "g1", 100, i64(2000), i64(300)),
		rec(1, records.StatusSuccess, "m1", "g1", 100, i64(2000), i64(500)),
	}
	p := Aggregate(recs, "t", "new_api", 1)[0]
	if p.AvgTTFTMs.Value == nil || *p.AvgTTFTMs.Value != 400 {
		t.Fatalf("ttft 错误: %+v", p.AvgTTFTMs)
	}
}

func TestAggregateGroupsAndModelsSeparate(t *testing.T) {
	recs := []records.CallRecord{
		rec(0, records.StatusSuccess, "m1", "g1", 100, i64(2000), i64(500)),
		rec(1, records.StatusSuccess, "m1", "g2", 100, i64(2000), i64(500)),
		rec(2, records.StatusSuccess, "m2", "g1", 100, i64(2000), i64(500)),
	}
	out := Aggregate(recs, "t", "new_api", 1)
	keys := map[[2]string]bool{}
	for _, p := range out {
		g := ""
		if p.Group != nil {
			g = *p.Group
		}
		keys[[2]string{g, p.Model}] = true
	}
	if len(keys) != 3 {
		t.Fatalf("期望 3 个桶: %v", keys)
	}
}

func TestAggregateErrorOnlyBucket(t *testing.T) {
	r := rec(0, records.StatusError, "m1", "g1", 0, nil, nil)
	p := Aggregate([]records.CallRecord{r}, "t", "new_api", 1)[0]
	if p.SuccessRate.Value == nil || *p.SuccessRate.Value != 0 {
		t.Fatalf("成功率应为 0: %+v", p.SuccessRate)
	}
	if p.AvgOutputTPS.Value != nil || p.AvgTTFTMs.Value != nil {
		t.Fatal("无有效值时应为 nil")
	}
}

func TestSub2ApiSuccessRateHelper(t *testing.T) {
	sr := Sub2ApiSuccessRate(8, 1)
	if sr.Value == nil || abs(*sr.Value-88.888) > 0.01 {
		t.Fatalf("成功率错误: %+v", sr)
	}
	if sr.Provenance != records.ProvDerived {
		t.Fatal("provenance 应为 derived")
	}
	if Sub2ApiSuccessRate(0, 0).Value != nil {
		t.Fatal("0/0 应为 nil")
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
