// Package records: 统一记录模型。所有数值带来源标注
// （native=站点原生聚合 / derived=本地推算 / unsupported / missing）与口径说明。
// 字段白名单清洗：提示词、响应正文、Cookie、令牌等一律不进入记录。
package records

// SchemaVersion 与 Python 版一致。
const SchemaVersion = 1

type Provenance string

const (
	ProvNative      Provenance = "native"
	ProvDerived     Provenance = "derived"
	ProvUnsupported Provenance = "unsupported"
	ProvMissing     Provenance = "missing"
)

// Metric 是一个带出处的数值。
type Metric struct {
	Value      *float64   `json:"value"`
	Provenance Provenance `json:"provenance"`
	Definition string     `json:"definition,omitempty"`
}

func Prov(v *float64, p Provenance, def string) Metric {
	return Metric{Value: v, Provenance: p, Definition: def}
}

func ProvF64(v float64, p Provenance, def string) Metric {
	return Metric{Value: &v, Provenance: p, Definition: def}
}

type CallStatus string

const (
	StatusSuccess CallStatus = "success"
	StatusError   CallStatus = "error"
	StatusUnknown CallStatus = "unknown"
)

// CallRecord 是一条脱敏后的调用日志。
type CallRecord struct {
	SchemaVersion   int        `json:"schema_version"`
	SiteID          string     `json:"site_id"`
	SiteKind        string     `json:"site_kind"`
	RecordID        string     `json:"record_id"`
	ObservedAt      int64      `json:"observed_at"` // unix 秒
	Model           *string    `json:"model"`
	Group           *string    `json:"group"`
	Status          CallStatus `json:"status"`
	ErrorClass      *string    `json:"error_class"`
	InputTokens     *int64     `json:"input_tokens"`
	OutputTokens    *int64     `json:"output_tokens"`
	CacheReadTokens *int64     `json:"cache_read_tokens"`
	QuotaCost       *float64   `json:"quota_cost"`
	DurationMs      *int64     `json:"duration_ms"`
	FirstTokenMs    *int64     `json:"first_token_ms"`
	Stream          *bool      `json:"stream"`
	// 上游渠道对齐（见 internal/align）：
	//   ChannelID   上游渠道标识（new-api=渠道 id；sub2api=account_id），稳定 ID 而非网址
	//   ChannelName 渠道名（admin 面可回查时填充）
	//   ChannelKeyLabel 多Key渠道的 key 泳道标签（运营者自定义，绝非密钥本身）
	//   ChannelSource 对齐来源：native=站点日志自带 / rule=配置映射
	ChannelID       *string        `json:"channel_id,omitempty"`
	ChannelName     *string        `json:"channel_name,omitempty"`
	ChannelKeyLabel *string        `json:"channel_key_label,omitempty"`
	ChannelSource   *string        `json:"channel_source,omitempty"`
	Raw             map[string]any `json:"raw"`
}

// GroupRatio 分组倍率。
type GroupRatio struct {
	SchemaVersion int            `json:"schema_version"`
	SiteID        string         `json:"site_id"`
	SiteKind      string         `json:"site_kind"`
	Group         string         `json:"group"`
	Ratio         float64        `json:"ratio"`
	Description   *string        `json:"description"`
	Scope         string         `json:"scope"` // site | model
	ModelName     *string        `json:"model_name,omitempty"`
	Extra         map[string]any `json:"extra"`
}

// PerfMetric 是一个 (group, model) 组合在时间窗内的性能指标。
type PerfMetric struct {
	SchemaVersion int            `json:"schema_version"`
	SiteID        string         `json:"site_id"`
	SiteKind      string         `json:"site_kind"`
	Model         string         `json:"model"`
	Group         *string        `json:"group"`
	WindowHours   int            `json:"window_hours"`
	SuccessRate   Metric         `json:"success_rate"`
	AvgTTFTMs     Metric         `json:"avg_ttft_ms"`
	AvgOutputTPS  Metric         `json:"avg_output_tps"`
	AvgLatencyMs  *Metric        `json:"avg_latency_ms,omitempty"`
	RequestCount  *int           `json:"request_count,omitempty"`
	Extra         map[string]any `json:"extra,omitempty"`
}

// ModelPrice 是一个 (model, group) 组合的模型单价（USD）。
//
// new-api（native 计算）：基准 1 倍率 = $0.002/1K tokens = $2/1M
// （common/constants.go: QuotaPerUnit=500000），价格 = 倍率 × 2 × 分组倍率；
// 按次计费模型（quota_type=1）为 model_price × 分组倍率 USD/次。
// 缓存价 = model_ratio × cache_ratio × 2 × 分组倍率。
//
// sub2api（derived）：站点不向用户暴露模型定价 → 从 usage 日志
// 实付成本派生：input_cost/input_tokens×1M；分组倍率（rate_multiplier）
// 作为当前实付价系数一并在记录中给出。
type ModelPrice struct {
	SchemaVersion int    `json:"schema_version"`
	SiteID        string `json:"site_id"`
	SiteKind      string `json:"site_kind"`
	Model         string `json:"model"`
	Group         string `json:"group"`
	// 计费类型：ratio=按倍率，per_call=按次，unknown
	BillingType string `json:"billing_type"`
	// USD / 1M tokens（billing_type=ratio 时有效）
	InputPerM       *float64 `json:"input_per_m_usd"`
	OutputPerM      *float64 `json:"output_per_m_usd"`
	CacheReadPerM   *float64 `json:"cache_read_per_m_usd,omitempty"`
	CacheCreatePerM *float64 `json:"cache_create_per_m_usd,omitempty"`
	// USD / 次（billing_type=per_call 时有效）
	PerCall *float64 `json:"per_call_usd,omitempty"`
	// 原始倍率（便于运营核对）
	ModelRatio      *float64 `json:"model_ratio,omitempty"`
	CompletionRatio *float64 `json:"completion_ratio,omitempty"`
	CacheRatio      *float64 `json:"cache_ratio,omitempty"`
	GroupRatio      float64  `json:"group_ratio"`
	// 数据来源与口径
	Provenance Provenance     `json:"provenance"`
	Definition string         `json:"definition"`
	Extra      map[string]any `json:"extra,omitempty"`
}

// CleanRaw 只保留白名单字段，杜绝提示词/密钥类字段泄漏进 artifact。
func CleanRaw(payload map[string]any, allowlist map[string]bool) map[string]any {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		if allowlist[k] {
			out[k] = v
		}
	}
	return out
}
