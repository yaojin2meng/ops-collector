package adapters

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"opscollector/internal/auth"
	"opscollector/internal/config"
	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"
	"opscollector/internal/metrics"
	"opscollector/internal/records"
)

// Sub2ApiAdapter 对接 Wei-Shaw/sub2api。
// 契约来源见 docs/upstream-contracts.md §2。
//
// 性能指标说明（站点无用户级预聚合性能端点）：
//   - first_token_ms 为逐条原生字段 → TTFT 取均值（derived from native）
//   - tps 按 output_tokens/((duration-first_token)/1000) 派生
//   - 成功率按 (model,group) 分桶：usage_n/(usage_n+errors_n)（derived）
type Sub2ApiAdapter struct {
	site   *config.SiteConfig
	client *httpc.Client
	auth   auth.Authenticator
}

var usageRawAllowlist = map[string]bool{
	"id": true, "request_id": true, "model": true, "group_id": true,
	"api_key_id": true, "account_id": true, "input_tokens": true,
	"output_tokens":     true,
	"cache_read_tokens": true, "cache_creation_tokens": true,
	"total_cost": true, "actual_cost": true, "rate_multiplier": true,
	"request_type": true, "stream": true, "duration_ms": true,
	"first_token_ms": true, "created_at": true, "inbound_endpoint": true,
	"service_tier": true,
}

func NewSub2ApiAdapter(site *config.SiteConfig, client *httpc.Client) (*Sub2ApiAdapter, error) {
	a, ok := auth.MakeAuthenticator("sub2api")
	if !ok {
		return nil, ierr.New(ierr.CodeInternal, "sub2api 认证器缺失")
	}
	return &Sub2ApiAdapter{site: site, client: client, auth: a}, nil
}

func (a *Sub2ApiAdapter) Kind() string { return "sub2api" }

func (a *Sub2ApiAdapter) Capabilities() Capabilities {
	return Capabilities{Logs: true, GroupRatios: true,
		NativeSuccessRate: false, NativeTTFT: false, NativeTPS: false,
		NativeModelPrice: false, PresetRatioSync: false}
}

func (a *Sub2ApiAdapter) headers() (map[string]string, error) {
	ctx, err := a.auth.EnsureSession(siteView{a.site}, a.client)
	if err != nil {
		return nil, err
	}
	return ctx.Headers, nil
}

// get 带 401 自愈：refresh → 重登（收敛到 auth.WithSelfHeal）。
func (a *Sub2ApiAdapter) get(path string, params url.Values) (any, error) {
	return auth.WithSelfHeal(siteView{a.site}, a.client, a.auth,
		func(headers map[string]string) (any, error) {
			return a.client.Get(path, params, headers)
		})
}

// SiteMeta 返回用户信息（用于 login-test）。
func (a *Sub2ApiAdapter) SiteMeta() (map[string]any, error) {
	data, err := a.get("/api/v1/user/profile", nil)
	if err != nil {
		return nil, err
	}
	data, err = httpc.UnwrapSub2API(data, "用户信息")
	if err != nil {
		return nil, err
	}
	u, _ := data.(map[string]any)
	out := map[string]any{}
	if u != nil {
		if v, ok := u["id"]; ok {
			out["user_id"] = v
		}
		if email, ok := u["email"].(string); ok {
			out["username"] = email
		} else if name, ok := u["name"].(string); ok {
			out["username"] = name
		}
		if v, ok := u["name"].(string); ok {
			out["display_name"] = v
		}
		if v, ok := u["role"]; ok {
			out["role"] = v
		}
		if v, ok := u["balance"]; ok {
			out["balance"] = v
		}
	}
	return out, nil
}

// FetchLogs 拉取 /api/v1/usage（分页，日期过滤）。
// sub2api 的 end_date 是排他边界（endTime=日期+1天），因此 end_date 直接取
// end_ts 所在日期即可覆盖到 end_ts（按天粒度，可能多覆盖到当天 24:00 前，
// 属上游日期粒度限制）。
func (a *Sub2ApiAdapter) FetchLogs(startTS, endTS *int64, maxPages, pageSize int) ([]records.CallRecord, bool, error) {
	out := make([]records.CallRecord, 0, pageSize)
	truncated := false
	for page := 1; page <= maxPages; page++ {
		params := url.Values{
			"page": {strconv.Itoa(page)}, "page_size": {strconv.Itoa(pageSize)},
			"sort_by": {"created_at"}, "sort_order": {"desc"},
		}
		if sd := dateStr(startTS); sd != "" {
			params.Set("start_date", sd)
		}
		if ed := dateStr(endTS); ed != "" {
			params.Set("end_date", ed)
		}
		data, err := a.get("/api/v1/usage", params)
		if err != nil {
			return nil, false, err
		}
		data, err = httpc.UnwrapSub2API(data, "调用日志")
		if err != nil {
			return nil, false, err
		}
		items, total, err := httpc.PaginatedItems(data, "调用日志")
		if err != nil {
			return nil, false, err
		}
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				out = append(out, a.toCallRecord(m))
			}
		}
		if int64(page*pageSize) >= total {
			break
		}
		if page == maxPages && int64(maxPages*pageSize) < total {
			truncated = true
		}
	}
	return out, truncated, nil
}

func (a *Sub2ApiAdapter) toCallRecord(it map[string]any) records.CallRecord {
	strPtr := func(k string) *string {
		if v, ok := it[k].(string); ok && v != "" {
			return &v
		}
		return nil
	}
	intPtr := func(k string) *int64 {
		if v, ok := httpc.AsInt(it[k]); ok {
			return &v
		}
		return nil
	}
	var observedAt int64
	if v, ok := it["created_at"]; ok {
		switch n := v.(type) {
		case float64:
			observedAt = int64(n)
		case string:
			if t, err := time.Parse(time.RFC3339, strings.Replace(n, "Z", "+00:00", 1)); err == nil {
				observedAt = t.Unix()
			}
		}
	}
	recordID := ""
	if id, ok := it["id"]; ok {
		recordID = fmt.Sprintf("%v", id)
	}
	if recordID == "" || recordID == "<nil>" {
		if rid, ok := it["request_id"].(string); ok {
			recordID = rid
		}
	}
	var group *string
	if gid, ok := httpc.AsInt(it["group_id"]); ok {
		g := strconv.FormatInt(gid, 10)
		group = &g
	}
	// 原生上游归属：sub2api usage 的 account_id 即上游账号 id
	var channelID *string
	if acc, ok := httpc.AsInt(it["account_id"]); ok && acc > 0 {
		s := strconv.FormatInt(acc, 10)
		channelID = &s
	}
	return records.CallRecord{
		SchemaVersion:   records.SchemaVersion,
		SiteID:          a.site.SiteID,
		SiteKind:        a.Kind(),
		RecordID:        recordID,
		ObservedAt:      observedAt,
		Model:           strPtr("model"),
		Group:           group,
		Status:          records.StatusSuccess, // sub2api 失败请求在 errors 端点
		InputTokens:     intPtr("input_tokens"),
		OutputTokens:    intPtr("output_tokens"),
		CacheReadTokens: intPtr("cache_read_tokens"),
		QuotaCost:       f64Ptr(it["total_cost"]),
		DurationMs:      intPtr("duration_ms"),
		FirstTokenMs:    intPtr("first_token_ms"),
		Stream:          boolPtr(it["stream"]),
		ChannelID:       channelID,
		Raw:             records.CleanRaw(it, usageRawAllowlist),
	}
}

// FetchErrorsCount 拉失败请求数与脱敏错误列表。
func (a *Sub2ApiAdapter) FetchErrorsCount(startTS, endTS *int64) (int64, []map[string]any, error) {
	var total int64
	var errs []map[string]any
	for page := 1; page <= 20; page++ {
		params := url.Values{"page": {strconv.Itoa(page)}, "page_size": {"100"}}
		if sd := dateStr(startTS); sd != "" {
			params.Set("start_date", sd)
		}
		if ed := dateStr(endTS); ed != "" {
			params.Set("end_date", ed)
		}
		data, err := a.get("/api/v1/usage/errors", params)
		if err != nil {
			return 0, nil, err
		}
		data, err = httpc.UnwrapSub2API(data, "错误日志")
		if err != nil {
			return 0, nil, err
		}
		items, t, err := httpc.PaginatedItems(data, "错误日志")
		if err != nil {
			return 0, nil, err
		}
		total = t
		for _, it := range items {
			if m, ok := it.(map[string]any); ok {
				errs = append(errs, m)
			}
		}
		if int64(page*100) >= total || len(items) == 0 {
			break
		}
	}
	return total, errs, nil
}

// FetchGroupRatios 拉 /api/v1/groups/available + /api/v1/groups/rates。
func (a *Sub2ApiAdapter) FetchGroupRatios() ([]records.GroupRatio, error) {
	data, err := a.get("/api/v1/groups/available", nil)
	if err != nil {
		return nil, err
	}
	data, err = httpc.UnwrapSub2API(data, "分组")
	if err != nil {
		return nil, err
	}
	arr, ok := data.([]any)
	if !ok {
		return nil, ierr.New(ierr.CodeAPIError, "分组响应不是数组")
	}
	var out []records.GroupRatio
	for _, gi := range arr {
		g, ok := gi.(map[string]any)
		if !ok {
			continue
		}
		// join 键统一：Group 恒为数字 gid 字符串（与 CallRecord/PerfMetric/
		// ModelPrice 的 Group 同键域），人类可读名放 extra.group_name
		gid := ""
		if id, ok := httpc.AsInt(g["id"]); ok {
			gid = strconv.FormatInt(id, 10)
		} else if s, ok := g["name"].(string); ok {
			gid = s
		}
		name := ""
		if s, ok := g["name"].(string); ok {
			name = s
		}
		ratio := 1.0
		if f, ok := httpc.AsF64(g["rate_multiplier"]); ok {
			ratio = f
		}
		var desc *string
		if s, ok := g["description"].(string); ok {
			desc = &s
		}
		extra := map[string]any{"source": "/api/v1/groups/available",
			"group_name": name}
		if id, ok := g["id"]; ok {
			extra["group_id"] = id
		}
		if p, ok := g["platform"]; ok {
			extra["platform"] = p
		}
		if s, ok := g["status"]; ok {
			extra["status"] = s
		}
		out = append(out, records.GroupRatio{
			SchemaVersion: records.SchemaVersion,
			SiteID:        a.site.SiteID, SiteKind: a.Kind(),
			Group: gid, Ratio: ratio, Description: desc, Scope: "site",
			Extra: extra,
		})
	}
	// 用户专属倍率（/groups/rates 返回 group_id → 自定义倍率）：
	// 合并进对应分组的 extra（不产生哨兵记录，控制面信息不进数据列表）
	userRates := map[string]float64{}
	if rates, err := a.get("/api/v1/groups/rates", nil); err == nil {
		if rd, err2 := httpc.UnwrapSub2API(rates, "用户倍率"); err2 == nil {
			if rm, ok := rd.(map[string]any); ok {
				for gid, r := range rm {
					if f, ok := httpc.AsF64(r); ok {
						userRates[gid] = f
					}
				}
			}
		}
	}
	for i := range out {
		if gid, ok := out[i].Extra["group_id"]; ok {
			key := fmt.Sprintf("%v", gid)
			if r, ok := userRates[key]; ok {
				// 用户专属倍率覆盖分组默认倍率
				out[i].Extra["default_rate_multiplier"] = out[i].Ratio
				out[i].Extra["user_rate_override"] = r
				out[i].Ratio = r
			}
		}
	}
	return out, nil
}

// FetchPerformance 从 usage 日志本地派生（TTFT 均值 / tps 公式 / 成功率）。
// 成功率按 (model, group) 分桶：success = usage_n/(usage_n+errors_n)，
// 仅当 errors 明细缺失 model 归属时才退化为站点级近似（definition 中如实披露）。
func (a *Sub2ApiAdapter) FetchPerformance(models []string, hours, maxModels int) ([]records.PerfMetric, error) {
	now := time.Now().Unix()
	startTS := now - int64(hours)*3600
	recs, _, err := a.FetchLogs(&startTS, &now, 50, 100)
	if err != nil {
		return nil, err
	}
	if len(models) > 0 {
		allow := map[string]bool{}
		for _, m := range models {
			allow[m] = true
		}
		filtered := make([]records.CallRecord, 0, len(recs))
		for _, r := range recs {
			if r.Model != nil && allow[*r.Model] {
				filtered = append(filtered, r)
			}
		}
		recs = filtered
	}
	perf := metrics.Aggregate(recs, a.site.SiteID, a.Kind(), hours)
	errTotal, errItems, err := a.FetchErrorsCount(&startTS, &now)
	if err != nil && ierr.CodeOf(err) == ierr.CodeAPIError {
		errTotal = -1 // 端点不可用时保持 aggregate 自身口径
	} else if err != nil {
		return nil, err
	}
	if errTotal >= 0 {
		// 尝试按 (model, group) 分桶计失败数
		bucketErr := map[[2]string]int64{}
		hasModelAttr := false
		for _, e := range errItems {
			model, _ := httpc.AsStr(e["model"])
			if model == "" {
				continue
			}
			hasModelAttr = true
			gid := "_default"
			if g, ok := httpc.AsInt(e["group_id"]); ok {
				gid = strconv.FormatInt(g, 10)
			}
			bucketErr[[2]string{model, gid}]++
		}
		usageN := map[[2]string]int64{}
		for _, r := range recs {
			g := "_default"
			if r.Group != nil {
				g = *r.Group
			}
			m := "_unknown"
			if r.Model != nil {
				m = *r.Model
			}
			usageN[[2]string{m, g}]++
		}
		for i := range perf {
			m, g := perf[i].Model, "_default"
			if perf[i].Group != nil {
				g = *perf[i].Group
			}
			key := [2]string{m, g}
			en := bucketErr[key]
			if hasModelAttr {
				perf[i].SuccessRate = metrics.Sub2ApiSuccessRate(usageN[key], en)
			} else {
				// errors 无 model 归属（旧版本）：退化为站点级近似并披露
				sr := metrics.Sub2ApiSuccessRate(int64(len(recs)), errTotal)
				sr.Definition = "站点级近似（errors 明细缺少 model 字段，无法按模型分桶）：" + sr.Definition
				perf[i].SuccessRate = sr
			}
		}
		if len(perf) == 0 && errTotal > 0 {
			// 无成功调用但有失败：给出全站口径行
			perf = append(perf, records.PerfMetric{
				SchemaVersion: records.SchemaVersion,
				SiteID:        a.site.SiteID, SiteKind: a.Kind(),
				Model: "_site", WindowHours: hours,
				SuccessRate:  metrics.Sub2ApiSuccessRate(0, errTotal),
				AvgTTFTMs:    records.Prov(nil, records.ProvMissing, metrics.TTFTDef),
				AvgOutputTPS: records.Prov(nil, records.ProvMissing, metrics.TPSDef),
				Extra:        map[string]any{"note": "窗口内无成功调用，仅失败计数"},
			})
		}
	}
	return perf, nil
}

func dateStr(ts *int64) string {
	if ts == nil {
		return ""
	}
	return time.Unix(*ts, 0).UTC().Format("2006-01-02")
}

// ---- 模型价格（从 usage 实付成本派生） ----

const subPriceDef = "USD/1M = sum(input_cost)/sum(input_tokens)×1e6（output 同理），来自 usage 日志实付成本，已含分组倍率与用户专属倍率；站点不向用户暴露原生模型定价，故为派生口径"

// FetchModelPrices 从 usage 日志派生各 (model, group) 的实付单价（USD/1M）。
// 金额字段来自逐条记录的 input_cost / output_cost / total_cost，
// 天然包含分组 rate_multiplier 与用户专属倍率的影响。
func (a *Sub2ApiAdapter) FetchModelPrices() ([]records.ModelPrice, error) {
	// 分组倍率上下文（context 用，不参与金额计算）
	groupRate := map[string]float64{}
	if ratios, err := a.FetchGroupRatios(); err == nil {
		for _, r := range ratios {
			if r.Group == "_user_rates_raw" {
				continue
			}
			if gid, ok := r.Extra["group_id"]; ok {
				groupRate[fmt.Sprintf("%v", gid)] = r.Ratio
			}
		}
	}

	type agg struct {
		inputCost, inputTok, outputCost, outputTok float64
		count                                      int
	}
	aggs := map[[2]string]*agg{}
	var order [][2]string

	now := time.Now().Unix()
	start := now - 7*24*3600 // 价格口径用最近 7 天，避免太旧的价格漂移
	for page := 1; page <= 50; page++ {
		params := url.Values{"page": {strconv.Itoa(page)}, "page_size": {"100"},
			"start_date": {time.Unix(start, 0).UTC().Format("2006-01-02")},
			"end_date":   {time.Unix(now, 0).UTC().Format("2006-01-02")}}
		data, err := a.get("/api/v1/usage", params)
		if err != nil {
			return nil, err
		}
		data, err = httpc.UnwrapSub2API(data, "调用日志")
		if err != nil {
			return nil, err
		}
		items, total, err := httpc.PaginatedItems(data, "调用日志")
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			model, _ := m["model"].(string)
			if model == "" {
				continue
			}
			gid := "_default"
			if g, ok := httpc.AsInt(m["group_id"]); ok {
				gid = strconv.FormatInt(g, 10)
			}
			key := [2]string{model, gid}
			if _, seen := aggs[key]; !seen {
				order = append(order, key)
				aggs[key] = &agg{}
			}
			g := aggs[key]
			g.count++
			if c, ok := httpc.AsF64(m["input_cost"]); ok {
				g.inputCost += c
			}
			if t, ok := httpc.AsF64(m["input_tokens"]); ok {
				g.inputTok += t
			}
			if c, ok := httpc.AsF64(m["output_cost"]); ok {
				g.outputCost += c
			}
			if t, ok := httpc.AsF64(m["output_tokens"]); ok {
				g.outputTok += t
			}
		}
		if int64(page*100) >= total {
			break
		}
	}

	out := make([]records.ModelPrice, 0, len(order))
	for _, key := range order {
		g := aggs[key]
		price := records.ModelPrice{
			SchemaVersion: records.SchemaVersion,
			SiteID:        a.site.SiteID, SiteKind: a.Kind(),
			Model: key[0], Group: key[1],
			BillingType: "ratio",
			Provenance:  records.ProvDerived,
			Definition:  subPriceDef,
			Extra: map[string]any{
				"source":          "/api/v1/usage（近7天实付）",
				"sample_count":    g.count,
				"rate_multiplier": groupRate[key[1]],
			},
		}
		if g.inputTok > 0 {
			v := g.inputCost / g.inputTok * 1e6
			price.InputPerM = &v
		}
		if g.outputTok > 0 {
			v := g.outputCost / g.outputTok * 1e6
			price.OutputPerM = &v
		}
		out = append(out, price)
	}
	return out, nil
}

// FetchPresetRatioConfig sub2api 用户面无预设倍率端点。
func (a *Sub2ApiAdapter) FetchPresetRatioConfig() (map[string]any, error) {
	return nil, ierr.New(ierr.CodeUnsupported,
		"sub2api 用户面无官方预设倍率端点（价格为管理员侧配置）")
}
