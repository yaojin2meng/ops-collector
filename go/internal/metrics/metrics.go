// Package metrics: 把 CallRecord 列表聚成 (group, model) × 时间窗的性能指标。
//
// 成功率口径（固定，写入 definition）：
//
//	成功请求数 / 可明确分类请求数（success + error；unknown 不计入分母）。
//
// 派生公式：
//
//	output_tps = output_tokens / ((duration_ms - first_token_ms)/1000)
//	             （缺 first_token_ms 时退化为 output_tokens / (duration_ms/1000)）
package metrics

import (
	"sort"

	ierr "opscollector/internal/errors"
	"opscollector/internal/records"
)

const (
	SuccessRateDef = "成功请求数/可明确分类请求数（success+error，unknown 不计分母）"
	TPSDef         = "output_tokens / ((duration_ms - first_token_ms)/1000)；缺首字时用 output_tokens/(duration_ms/1000)"
	TTFTDef        = "调用日志逐条 first_token_ms 的平均值（derived from per-request native values）"
	NativeSRDef    = "站点原生聚合：排除业务拒绝，含当前不完整小时"
)

func mean(vals []float64) *float64 {
	if len(vals) == 0 {
		return nil
	}
	s := 0.0
	for _, v := range vals {
		s += v
	}
	m := s / float64(len(vals))
	return &m
}

// Aggregate 聚合。key = (group, model)。
func Aggregate(recs []records.CallRecord, siteID, siteKind string,
	windowHours int) []records.PerfMetric {
	type bucket struct {
		recs []records.CallRecord
	}
	buckets := map[[2]string]*bucket{}
	for _, r := range recs {
		if r.FirstTokenMs == nil && r.DurationMs == nil && r.Status == records.StatusUnknown {
			continue
		}
		g, m := "_default", "_unknown"
		if r.Group != nil {
			g = *r.Group
		}
		if r.Model != nil {
			m = *r.Model
		}
		key := [2]string{g, m}
		b, ok := buckets[key]
		if !ok {
			b = &bucket{}
			buckets[key] = b
		}
		b.recs = append(b.recs, r)
	}

	keys := make([][2]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})

	out := make([]records.PerfMetric, 0, len(keys))
	for _, k := range keys {
		rows := buckets[k].recs
		classified, ok := 0, 0
		for _, r := range rows {
			if r.Status == records.StatusSuccess {
				classified++
				ok++
			} else if r.Status == records.StatusError {
				classified++
			}
		}
		var sr *float64
		if classified > 0 {
			v := float64(ok) / float64(classified) * 100
			sr = &v
		}
		var ttftVals, tpsVals []float64
		for _, r := range rows {
			if r.FirstTokenMs != nil {
				ttftVals = append(ttftVals, float64(*r.FirstTokenMs))
			}
			if r.Status != records.StatusSuccess || r.OutputTokens == nil || *r.OutputTokens <= 0 {
				continue
			}
			if r.DurationMs != nil && *r.DurationMs > 0 {
				effMs := *r.DurationMs
				if r.FirstTokenMs != nil {
					effMs -= *r.FirstTokenMs
				}
				if effMs > 0 {
					tpsVals = append(tpsVals, float64(*r.OutputTokens)/(float64(effMs)/1000.0))
				}
			}
		}
		group := k[0]
		var groupPtr *string
		if group != "_default" {
			groupPtr = &group
		}
		rc := len(rows)
		out = append(out, records.PerfMetric{
			SchemaVersion: records.SchemaVersion,
			SiteID:        siteID, SiteKind: siteKind,
			Model: k[1], Group: groupPtr, WindowHours: windowHours,
			SuccessRate:  records.Prov(sr, records.ProvDerived, SuccessRateDef),
			AvgTTFTMs:    records.Prov(mean(ttftVals), records.ProvDerived, TTFTDef),
			AvgOutputTPS: records.Prov(mean(tpsVals), records.ProvDerived, TPSDef),
			RequestCount: &rc,
		})
	}
	return out
}

// MergeNativePerf 合并 new-api 原生指标：modelsSummary 来自
// /perf-metrics/summary，perGroup[model] 来自 /perf-metrics?model= 的 groups。
func MergeNativePerf(modelsSummary map[string]map[string]any,
	perGroup map[string][]map[string]any,
	siteID, siteKind string, windowHours int) []records.PerfMetric {
	out := []records.PerfMetric{}
	seen := map[[2]string]bool{}

	models := make([]string, 0, len(perGroup))
	for m := range perGroup {
		models = append(models, m)
	}
	sort.Strings(models)
	for _, model := range models {
		groups := perGroup[model]
		for _, g := range groups {
			gname, _ := g["group"].(string)
			seen[[2]string{model, gname}] = true
			out = append(out, records.PerfMetric{
				SchemaVersion: records.SchemaVersion,
				SiteID:        siteID, SiteKind: siteKind,
				Model: model, Group: &gname, WindowHours: windowHours,
				SuccessRate:  provAny(g["success_rate"], records.ProvNative, NativeSRDef),
				AvgTTFTMs:    provAny(g["avg_ttft_ms"], records.ProvNative, "站点原生 TTFT 聚合"),
				AvgOutputTPS: provAny(g["avg_tps"], records.ProvNative, "站点原生吞吐聚合"),
				AvgLatencyMs: ptrMetric(provAny(g["avg_latency_ms"], records.ProvNative, "站点原生延迟聚合")),
			})
		}
	}

	// 只在没有按分组数据时补充 summary 全局值
	for model, s := range modelsSummary {
		hasGroup := false
		for k := range seen {
			if k[0] == model && k[1] != "" {
				hasGroup = true
				break
			}
		}
		if hasGroup {
			continue
		}
		m := records.PerfMetric{
			SchemaVersion: records.SchemaVersion,
			SiteID:        siteID, SiteKind: siteKind,
			Model: model, Group: nil, WindowHours: windowHours,
			SuccessRate:  provAny(s["success_rate"], records.ProvNative, NativeSRDef),
			AvgTTFTMs:    records.Prov(nil, records.ProvUnsupported, "summary 端点无 TTFT，请用 perf-metrics 按分组获取"),
			AvgOutputTPS: provAny(s["avg_tps"], records.ProvNative, "站点原生吞吐聚合"),
			AvgLatencyMs: ptrMetric(provAny(s["avg_latency_ms"], records.ProvNative, "站点原生延迟聚合")),
		}
		if rc, ok := s["request_count"].(float64); ok {
			ri := int(rc)
			m.RequestCount = &ri
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		gi, gj := "", ""
		if out[i].Group != nil {
			gi = *out[i].Group
		}
		if out[j].Group != nil {
			gj = *out[j].Group
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return gi < gj
	})
	return out
}

func provAny(v any, p records.Provenance, def string) records.Metric {
	if f, ok := v.(float64); ok {
		return records.ProvF64(f, p, def)
	}
	return records.Prov(nil, p, def)
}

func ptrMetric(m records.Metric) *records.Metric { return &m }

// Sub2ApiSuccessRate 近似口径：usage_total/(usage_total+errors_total)。
func Sub2ApiSuccessRate(usageTotal, errorsTotal int64) records.Metric {
	def := "usage_total/(usage_total+errors_total)，来自 usage 与 usage/errors 两个端点（近似口径）"
	total := usageTotal + errorsTotal
	if total == 0 {
		return records.Prov(nil, records.ProvDerived, def)
	}
	v := float64(usageTotal) / float64(total) * 100
	return records.ProvF64(v, records.ProvDerived, def)
}

var _ = ierr.CodeInternal
