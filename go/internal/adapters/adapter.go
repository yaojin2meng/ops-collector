// Package adapters: 按站点类型隔离的数据采集适配器。
//
// 上游接口契约见仓库 docs/upstream-contracts.md（锁定自源码，非猜测）。
package adapters

import (
	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"

	"opscollector/internal/config"
	"opscollector/internal/records"
)

// Capabilities 显式声明能力，避免 agent 猜测。
type Capabilities struct {
	Logs              bool
	GroupRatios       bool
	NativeSuccessRate bool // 站点提供原生成功率聚合
	NativeTTFT        bool // 站点提供原生首字聚合
	NativeTPS         bool // 站点提供原生吞吐聚合
	NativeModelPrice  bool // 站点提供原生模型定价（可直接算倍率价）
	PresetRatioSync   bool // 有官方预设倍率配置端点可同步
}

// SiteAdapter 是站点适配器协议。
type SiteAdapter interface {
	Kind() string
	Capabilities() Capabilities
	SiteMeta() (map[string]any, error)
	// FetchLogs 返回 (记录, 是否因 maxPages 截断, error)。
	// truncated=true 表示数据不完整（按时间倒序拉取，最旧的未覆盖到），
	// 调用方不得据此推进增量断点。
	FetchLogs(startTS, endTS *int64, maxPages, pageSize int) ([]records.CallRecord, bool, error)
	FetchGroupRatios() ([]records.GroupRatio, error)
	FetchPerformance(models []string, hours, maxModels int) ([]records.PerfMetric, error)
	// FetchModelPrices 返回各 (model, group) 的模型单价（含缓存价）。
	FetchModelPrices() ([]records.ModelPrice, error)
	// FetchPresetRatioConfig 同步站点官方预设倍率配置（原生 raw）。
	// 无该能力的站点返回 (nil, UNSUPPORTED)。
	FetchPresetRatioConfig() (map[string]any, error)
}

// Make 按站点类型构造适配器。
func Make(site *config.SiteConfig, client *httpc.Client) (SiteAdapter, error) {
	switch site.Kind {
	case "new_api":
		return NewNewApiAdapter(site, client)
	case "sub2api":
		return NewSub2ApiAdapter(site, client)
	}
	return nil, ierr.Configf("未知站点类型: %s", site.Kind)
}
