// Package align: 请求日志与上游渠道的自动对齐。
//
// 对齐优先级（写入 ChannelSource）：
//  1. rule   — 配置中的模型 ID → 渠道映射（运营者显式指定，含可选 key 泳道标签，
//     用于多Key渠道的精确归属）；规则命中时覆盖 native 值。
//  2. native — 站点日志自带的上游渠道字段（new-api=log.channel 渠道 id；
//     sub2api=usage.account_id 上游账号 id）。
//
// 渠道名回查：admin 面可用时由调用方传入 id→name 映射；查不到则名字留空
// （诚实缺失，不猜测）。key 永远只以运营者自定义标签出现，绝无真实密钥。
package align

import (
	"path"
	"strconv"

	"opscollector/internal/config"
	"opscollector/internal/records"
)

// Stats 对齐统计（进 run summary counts）。
type Stats struct {
	Native    int // 日志自带渠道归属
	Rule      int // 配置规则对齐
	Unaligned int // 未能对齐（如实保留空值）
}

// Apply 就地对齐日志并返回统计。namesByID 为渠道 id → 名称（可 nil）。
// 规则按配置顺序取第一条命中的。
func Apply(recs []records.CallRecord, rules []config.AlignmentRule,
	namesByID map[int64]string) ([]records.CallRecord, Stats) {
	stats := Stats{}
	for i := range recs {
		rec := &recs[i]
		model := ""
		if rec.Model != nil {
			model = *rec.Model
		}

		if rule := matchRule(rules, model); rule != nil {
			id := strconv.FormatInt(rule.ChannelID, 10)
			rec.ChannelID = &id
			if rule.KeyLabel != "" {
				label := rule.KeyLabel
				rec.ChannelKeyLabel = &label
			}
			if name, ok := namesByID[rule.ChannelID]; ok && name != "" {
				n := name
				rec.ChannelName = &n
			}
			src := "rule"
			rec.ChannelSource = &src
			stats.Rule++
			continue
		}

		if rec.ChannelID != nil && *rec.ChannelID != "" && *rec.ChannelID != "0" {
			if id, err := strconv.ParseInt(*rec.ChannelID, 10, 64); err == nil {
				if name, ok := namesByID[id]; ok && name != "" {
					n := name
					rec.ChannelName = &n
				}
			}
			src := "native"
			rec.ChannelSource = &src
			stats.Native++
			continue
		}

		stats.Unaligned++
	}
	return recs, stats
}

// matchRule 返回第一条命中的规则；pattern 支持 `*` 通配（path.Match 语义）。
func matchRule(rules []config.AlignmentRule, model string) *config.AlignmentRule {
	for i := range rules {
		for _, pattern := range rules[i].Models {
			if matchPattern(pattern, model) {
				return &rules[i]
			}
		}
	}
	return nil
}

func matchPattern(pattern, model string) bool {
	if pattern == "*" {
		return true
	}
	if pattern == model {
		return true
	}
	// path.Match 的 * 不跨界，模型 ID 无分隔符约束，语义合适
	ok, err := path.Match(pattern, model)
	return err == nil && ok
}
