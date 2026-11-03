package adapters

import (
	"fmt"
	"net/url"
	"strconv"
	"time"

	"opscollector/internal/auth"
	"opscollector/internal/config"
	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"
	"opscollector/internal/metrics"
	"opscollector/internal/records"
)

// NewApiAdapter 对接 QuantumNous/new-api。
// 契约来源见 docs/upstream-contracts.md §1。
type NewApiAdapter struct {
	site   *config.SiteConfig
	client *httpc.Client
	auth   auth.Authenticator
	ctx    *auth.AuthCtx
}

var logRawAllowlist = map[string]bool{
	"id": true, "user_id": true, "created_at": true, "type": true,
	"content": true, "username": true, "token_name": true, "model_name": true,
	"quota": true, "prompt_tokens": true, "completion_tokens": true,
	"use_time": true, "is_stream": true, "channel": true, "group": true,
}

func NewNewApiAdapter(site *config.SiteConfig, client *httpc.Client) (*NewApiAdapter, error) {
	a, ok := auth.MakeAuthenticator("new_api")
	if !ok {
		return nil, ierr.New(ierr.CodeInternal, "new_api 认证器缺失")
	}
	return &NewApiAdapter{site: site, client: client, auth: a}, nil
}

func (a *NewApiAdapter) Kind() string { return "new_api" }

func (a *NewApiAdapter) Capabilities() Capabilities {
	return Capabilities{Logs: true, GroupRatios: true,
		NativeSuccessRate: true, NativeTTFT: true, NativeTPS: true,
		NativeModelPrice: true, PresetRatioSync: true}
}

func (a *NewApiAdapter) headers() (map[string]string, error) {
	ctx, err := a.auth.EnsureSession(siteView{a.site}, a.client)
	if err != nil {
		return nil, err
	}
	return ctx.Headers, nil
}

// get 带 401 自愈（收敛到 auth.WithSelfHeal 单一实现）。
func (a *NewApiAdapter) get(path string, params url.Values) (any, error) {
	return auth.WithSelfHeal(siteView{a.site}, a.client, a.auth,
		func(headers map[string]string) (any, error) {
			return a.client.Get(path, params, headers)
		})
}

// SiteMeta 返回站点用户信息（用于 login-test）。
func (a *NewApiAdapter) SiteMeta() (map[string]any, error) {
	data, err := a.get("/api/user/self", nil)
	if err != nil {
		return nil, err
	}
	data, err = httpc.UnwrapNewAPI(data, "用户信息")
	if err != nil {
		return nil, err
	}
	u, _ := data.(map[string]any)
	out := map[string]any{}
	if u != nil {
		if v, ok := u["id"]; ok {
			out["user_id"] = v
		}
		if v, ok := u["username"]; ok {
			out["username"] = v
		}
		if v, ok := u["role"]; ok {
			out["role"] = v
		}
		if v, ok := u["quota"]; ok {
			out["quota"] = v
		}
		if v, ok := u["group"]; ok {
			out["group"] = v
		}
	}
	return out, nil
}

// FetchLogs 拉取 /api/log/self（分页）。truncated=true 表示因 maxPages 截断。
func (a *NewApiAdapter) FetchLogs(startTS, endTS *int64, maxPages, pageSize int) ([]records.CallRecord, bool, error) {
	out := make([]records.CallRecord, 0, pageSize)
	truncated := false
	for page := 1; page <= maxPages; page++ {
		params := url.Values{"p": {strconv.Itoa(page)},
			"page_size": {strconv.Itoa(pageSize)}}
		if startTS != nil {
			params.Set("start_timestamp", strconv.FormatInt(*startTS, 10))
		}
		if endTS != nil {
			params.Set("end_timestamp", strconv.FormatInt(*endTS, 10))
		}
		data, err := a.get("/api/log/self", params)
		if err != nil {
			return nil, false, err
		}
		data, err = httpc.UnwrapNewAPI(data, "调用日志")
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
				// new-api 日志混合流：1=消费 5=错误 才是调用记录；
				// 2=充值 3=管理 4=系统 等非调用类型直接跳过（避免污染成本统计）
				if t, ok := httpc.AsInt(m["type"]); ok && t != 1 && t != 5 {
					continue
				}
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

func (a *NewApiAdapter) toCallRecord(it map[string]any) records.CallRecord {
	strPtr := func(k string) *string {
		if v, ok := it[k].(string); ok && v != "" {
			return &v
		}
		return nil
	}
	intPtr := func(k string) *int64 {
		if v, ok := it[k]; ok {
			if n, ok2 := httpc.AsInt(v); ok2 {
				return &n
			}
		}
		return nil
	}
	observedAt := int64(0)
	if v, ok := httpc.AsInt(it["created_at"]); ok {
		observedAt = v
	}
	// 原生渠道归属：new-api 日志自带渠道 id（0=旧数据无归属）
	var channelID *string
	if ch, ok := httpc.AsInt(it["channel"]); ok && ch > 0 {
		s := strconv.FormatInt(ch, 10)
		channelID = &s
	}
	// type: 1=消费（成功调用）5=错误；调用记录只有这两种
	logType, _ := httpc.AsInt(it["type"])
	status := records.StatusUnknown
	switch logType {
	case 1:
		status = records.StatusSuccess
	case 5:
		status = records.StatusError
		ec := "upstream_error"
		return records.CallRecord{
			SchemaVersion: records.SchemaVersion,
			SiteID:        a.site.SiteID,
			SiteKind:      a.Kind(),
			RecordID:      fmt.Sprintf("%v", it["id"]),
			ObservedAt:    observedAt,
			Model:         strPtr("model_name"),
			Group:         strPtr("group"),
			Status:        status,
			ErrorClass:    &ec,
			ChannelID:     channelID,
			Raw:           records.CleanRaw(it, logRawAllowlist),
		}
	}
	var durationMs *int64
	if useTime, ok := httpc.AsInt(it["use_time"]); ok {
		d := useTime * 1000
		durationMs = &d
	}
	return records.CallRecord{
		SchemaVersion: records.SchemaVersion,
		SiteID:        a.site.SiteID,
		SiteKind:      a.Kind(),
		RecordID:      fmt.Sprintf("%v", it["id"]),
		ObservedAt:    observedAt,
		Model:         strPtr("model_name"),
		Group:         strPtr("group"),
		Status:        status,
		InputTokens:   intPtr("prompt_tokens"),
		OutputTokens:  intPtr("completion_tokens"),
		QuotaCost:     f64Ptr(it["quota"]),
		DurationMs:    durationMs,
		FirstTokenMs:  nil, // 日志逐条无 TTFT；用 perf-metrics 原生聚合
		Stream:        boolPtr(it["is_stream"]),
		ChannelID:     channelID,
		Raw:           records.CleanRaw(it, logRawAllowlist),
	}
}

// FetchGroupRatios 拉 /api/pricing。注意 group_ratio / usable_group 与 data
// 平级（PricingData 结构），不能对该端点做 unwrap。
func (a *NewApiAdapter) FetchGroupRatios() ([]records.GroupRatio, error) {
	payload, err := a.get("/api/pricing", nil)
	if err != nil {
		return nil, err
	}
	data, ok := payload.(map[string]any)
	if !ok {
		return nil, ierr.New(ierr.CodeAPIError, "定价响应不是对象")
	}
	var out []records.GroupRatio
	descOf := func(g string) *string {
		if usable, ok := data["usable_group"].(map[string]any); ok {
			if v, ok := usable[g]; ok {
				if dm, ok := v.(map[string]any); ok {
					if s, ok := dm["desc"].(string); ok {
						return &s
					}
				}
				if s, ok := v.(string); ok {
					return &s
				}
			}
		}
		return nil
	}
	if gr, ok := data["group_ratio"].(map[string]any); ok {
		for g, ratio := range gr {
			if f, ok := httpc.AsF64(ratio); ok {
				out = append(out, records.GroupRatio{
					SchemaVersion: records.SchemaVersion,
					SiteID:        a.site.SiteID, SiteKind: a.Kind(),
					Group: g, Ratio: f, Description: descOf(g), Scope: "site",
					Extra: map[string]any{"source": "/api/pricing group_ratio"},
				})
			}
		}
	}
	// 模型级覆盖倍率（若上游返回）
	if models, ok := data["data"].([]any); ok {
		for _, mi := range models {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			mgroup, ok := m["group_ratio"].(map[string]any)
			if !ok {
				continue
			}
			modelName, _ := m["model_name"].(string)
			for g, ratio := range mgroup {
				if f, ok := httpc.AsF64(ratio); ok {
					out = append(out, records.GroupRatio{
						SchemaVersion: records.SchemaVersion,
						SiteID:        a.site.SiteID, SiteKind: a.Kind(),
						Group: g, Ratio: f, Scope: "model", ModelName: &modelName,
						Extra: map[string]any{"source": "/api/pricing model.group_ratio"},
					})
				}
			}
		}
	}
	return out, nil
}

// FetchPerformance 拉 /api/perf-metrics/summary + /api/perf-metrics?model=。
func (a *NewApiAdapter) FetchPerformance(models []string, hours, maxModels int) ([]records.PerfMetric, error) {
	params := url.Values{"hours": {strconv.Itoa(hours)}}
	for i, m := range models {
		if i >= maxModels {
			break
		}
		params.Add("recent_model", m)
	}
	summaryData, err := a.get("/api/perf-metrics/summary", params)
	if err != nil {
		return nil, err
	}
	summaryData, err = httpc.UnwrapNewAPI(summaryData, "性能汇总")
	if err != nil {
		return nil, err
	}
	modelsSummary := map[string]map[string]any{}
	if sd, ok := summaryData.(map[string]any); ok {
		if arr, ok := sd["models"].([]any); ok {
			for _, mi := range arr {
				if m, ok := mi.(map[string]any); ok {
					if name, ok := m["model_name"].(string); ok && name != "" {
						modelsSummary[name] = m
					}
				}
			}
		}
	}

	// 按分组端点：原生 avg_ttft_ms（首字）
	perGroup := map[string][]map[string]any{}
	targets := models
	if len(targets) == 0 {
		for name := range modelsSummary {
			targets = append(targets, name)
		}
	}
	if len(targets) > maxModels {
		targets = targets[:maxModels]
	}
	for _, name := range targets {
		detail, err := a.get("/api/perf-metrics",
			url.Values{"model": {name}, "hours": {strconv.Itoa(hours)}})
		if err != nil {
			if ierr.CodeOf(err) == ierr.CodeAPIError {
				continue
			}
			return nil, err
		}
		detail, err = httpc.UnwrapNewAPI(detail, "模型性能 "+name)
		if err != nil {
			continue
		}
		if dm, ok := detail.(map[string]any); ok {
			if groups, ok := dm["groups"].([]any); ok {
				gs := make([]map[string]any, 0, len(groups))
				for _, g := range groups {
					if gm, ok := g.(map[string]any); ok {
						gs = append(gs, gm)
					}
				}
				perGroup[name] = gs
			}
		}
	}
	return metrics.MergeNativePerf(modelsSummary, perGroup,
		a.site.SiteID, a.Kind(), hours), nil
}

// FetchLogStat 拉 /api/log/self/stat。
func (a *NewApiAdapter) FetchLogStat(startTS, endTS *int64, modelName, group string) (map[string]any, error) {
	params := url.Values{}
	if startTS != nil {
		params.Set("start_timestamp", strconv.FormatInt(*startTS, 10))
	}
	if endTS != nil {
		params.Set("end_timestamp", strconv.FormatInt(*endTS, 10))
	}
	if modelName != "" {
		params.Set("model_name", modelName)
	}
	if group != "" {
		params.Set("group", group)
	}
	data, err := a.get("/api/log/self/stat", params)
	if err != nil {
		return nil, err
	}
	data, err = httpc.UnwrapNewAPI(data, "日志统计")
	if err != nil {
		return nil, err
	}
	m, _ := data.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// ---- 共享小助手 ----

func f64Ptr(v any) *float64 {
	if f, ok := httpc.AsF64(v); ok {
		return &f
	}
	return nil
}

func boolPtr(v any) *bool {
	if b, ok := httpc.AsBool(v); ok {
		return &b
	}
	return nil
}

// ---- 模型价格 ----

// usdPerMillionPerRatio 是 new-api 的倍率→美元换算基准：
// 1 倍率 = $0.002/1K tokens = $2/1M tokens
// （源码 common/constants.go: QuotaPerUnit = 500*1000.0, 注释 $0.002/1K）。
const usdPerMillionPerRatio = 2.0

// ratioPriceDef 是价格计算口径说明（写入每条记录）。
const ratioPriceDef = "USD/1M = 倍率 × 2 × 分组倍率（new-api 基准：1 倍率 = $0.002/1K tokens，QuotaPerUnit=500000）；缓存价 = model_ratio × cache_ratio × 2 × 分组倍率；按次模型 = model_price × 分组倍率 USD/次"

// FetchModelPrices 从 /api/pricing 计算各 (model, group) 的单价（含缓存价）。
func (a *NewApiAdapter) FetchModelPrices() ([]records.ModelPrice, error) {
	payload, err := a.get("/api/pricing", nil)
	if err != nil {
		return nil, err
	}
	data, ok := payload.(map[string]any)
	if !ok {
		return nil, ierr.New(ierr.CodeAPIError, "定价响应不是对象")
	}
	siteGroupRatio := map[string]float64{}
	if gr, ok := data["group_ratio"].(map[string]any); ok {
		for g, r := range gr {
			if f, ok := httpc.AsF64(r); ok {
				siteGroupRatio[g] = f
			}
		}
	}
	models, _ := data["data"].([]any)
	out := make([]records.ModelPrice, 0, len(models))
	for _, mi := range models {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["model_name"].(string)
		if name == "" {
			continue
		}
		modelRatio, _ := httpc.AsF64(m["model_ratio"])
		completionRatio, hasCompletion := httpc.AsF64(m["completion_ratio"])
		modelPrice, hasModelPrice := httpc.AsF64(m["model_price"])
		cacheRatio, hasCache := httpc.AsF64(m["cache_ratio"])
		createCacheRatio, hasCreateCache := httpc.AsF64(m["create_cache_ratio"])
		quotaType, _ := httpc.AsInt(m["quota_type"])

		// 该模型可用分组；模型级 group_ratio 覆盖优先于站点级
		groups := map[string]float64{}
		for _, gi := range toAnyList(m["enable_groups"]) {
			if g, ok := gi.(string); ok {
				if r, ok := siteGroupRatio[g]; ok {
					groups[g] = r
				} else {
					groups[g] = 1.0
				}
			}
		}
		if mgr, ok := m["group_ratio"].(map[string]any); ok {
			for g, r := range mgr {
				if f, ok := httpc.AsF64(r); ok {
					groups[g] = f
				}
			}
		}
		if len(groups) == 0 {
			groups["_default"] = 1.0
		}

		for g, gr := range groups {
			price := records.ModelPrice{
				SchemaVersion: records.SchemaVersion,
				SiteID:        a.site.SiteID, SiteKind: a.Kind(),
				Model: name, Group: g,
				GroupRatio:      gr,
				ModelRatio:      &modelRatio,
				CompletionRatio: ptrOrNil(hasCompletion, completionRatio),
				CacheRatio:      ptrOrNil(hasCache, cacheRatio),
				Provenance:      records.ProvNative,
				Definition:      ratioPriceDef,
				Extra:           map[string]any{"source": "/api/pricing"},
			}
			if quotaType == 1 {
				price.BillingType = "per_call"
				if hasModelPrice {
					v := modelPrice * gr
					price.PerCall = &v
				}
				out = append(out, price)
				continue
			}
			price.BillingType = "ratio"
			v := modelRatio * usdPerMillionPerRatio * gr
			price.InputPerM = &v
			if hasCompletion {
				o := modelRatio * completionRatio * usdPerMillionPerRatio * gr
				price.OutputPerM = &o
			}
			if hasCache {
				c := modelRatio * cacheRatio * usdPerMillionPerRatio * gr
				price.CacheReadPerM = &c
			}
			if hasCreateCache {
				cc := modelRatio * createCacheRatio * usdPerMillionPerRatio * gr
				price.CacheCreatePerM = &cc
			}
			out = append(out, price)
		}
	}
	return out, nil
}

// FetchPresetRatioConfig 同步站点官方预设倍率配置（公开端点，无需登录）。
func (a *NewApiAdapter) FetchPresetRatioConfig() (map[string]any, error) {
	payload, err := a.client.Get("/api/ratio_config", nil, nil)
	if err != nil {
		return nil, err
	}
	data, err := httpc.UnwrapNewAPI(payload, "官方预设倍率")
	if err != nil {
		return nil, err
	}
	m, ok := data.(map[string]any)
	if !ok {
		return nil, ierr.New(ierr.CodeAPIError, "官方预设倍率响应不是对象")
	}
	out := map[string]any{"source": "/api/ratio_config", "synced_at": time.Now().Unix()}
	for k, v := range m {
		out[k] = v
	}
	return out, nil
}

func ptrOrNil(ok bool, v float64) *float64 {
	if !ok {
		return nil
	}
	return &v
}

func toAnyList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}
