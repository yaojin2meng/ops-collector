package records

// Channel 是脱敏后的渠道记录（管理员面）。
//
// 安全规则：key/密钥类字段（key、channel_info、setting(s)、param_override、
// header_override、openai_organization）永远不进入 Raw 与任何输出。
type Channel struct {
	SchemaVersion int    `json:"schema_version"`
	SiteID        string `json:"site_id"`
	SiteKind      string `json:"site_kind"`
	ChannelID     int64  `json:"channel_id"`
	Name          string `json:"name"`
	// Platform: sub2api 平台（claude/openai/...）；new-api 为空
	Platform string `json:"platform,omitempty"`
	// Type: new-api 渠道类型码
	Type *int64 `json:"type,omitempty"`
	// Status 归一化：enabled | manual_disabled | auto_disabled | unknown
	Status ChannelStatus `json:"status"`
	// StatusCode 站点原始状态码（new-api 1/2/3；sub2api active/disabled 映射）
	StatusCode *int64   `json:"status_code,omitempty"`
	Models     []string `json:"models,omitempty"`
	Groups     []string `json:"groups,omitempty"`    // new-api 分组名
	GroupIDs   []int64  `json:"group_ids,omitempty"` // sub2api 分组 id
	Priority   *int64   `json:"priority,omitempty"`
	Weight     *int64   `json:"weight,omitempty"`
	// 多Key（密钥聚合）渠道摘要：只含计数/模式，不含任何 key 或状态明细
	MultiKey        *bool   `json:"multi_key,omitempty"`
	MultiKeySize    *int64  `json:"multi_key_size,omitempty"`
	MultiKeyEnabled *int64  `json:"multi_key_enabled,omitempty"`
	MultiKeyMode    *string `json:"multi_key_mode,omitempty"`
	// ResponseTimeMs 最近一次测试响应时间（毫秒）
	ResponseTimeMs *int64         `json:"response_time_ms,omitempty"`
	TestTime       *int64         `json:"test_time,omitempty"`
	UsedQuota      *int64         `json:"used_quota,omitempty"`
	BalanceUSD     *float64       `json:"balance_usd,omitempty"`
	BaseURL        *string        `json:"base_url,omitempty"`
	Tag            *string        `json:"tag,omitempty"`
	Raw            map[string]any `json:"raw,omitempty"`
}

type ChannelStatus string

const (
	ChannelEnabled        ChannelStatus = "enabled"
	ChannelManualDisabled ChannelStatus = "manual_disabled"
	ChannelAutoDisabled   ChannelStatus = "auto_disabled"
	ChannelStatusUnknown  ChannelStatus = "unknown"
)

// 渠道 raw 字段白名单（不含任何密钥字段）。
var ChannelRawAllowlist = map[string]bool{
	"id": true, "type": true, "status": true, "name": true,
	"created_time": true, "test_time": true, "response_time": true,
	"base_url": true, "balance": true, "balance_updated_time": true,
	"models": true, "group": true, "used_quota": true, "priority": true,
	"weight": true, "tag": true, "other": true, "other_info": true,
	"auto_ban": true, "remark": true, "test_model": true,
	// sub2api channelResponse 字段
	"description": true, "billing_model_source": true,
	"restrict_models": true, "group_ids": true, "model_pricing": true,
	"created_at": true, "updated_at": true, "features": true,
}

// 渠道密钥类字段黑名单（双保险：即使上游误返回也剔除）。
var ChannelSecretFields = map[string]bool{
	"key": true, "keys": true, "channel_info": true, "setting": true,
	"settings": true, "param_override": true, "header_override": true,
	"openai_organization": true, "multi_key_mode": true,
}

func CleanChannelRaw(payload map[string]any) map[string]any {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		if !ChannelSecretFields[k] && ChannelRawAllowlist[k] {
			out[k] = v
		}
	}
	return out
}
