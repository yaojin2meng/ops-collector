// Package channels: 渠道管理（管理员面）。给运营模型（agent）用的接口：
// 列表/详情/添加/更新/删除/启停/测试 + 状态自动检查（check/watch，变化提醒）。
//
// 安全边界：
//   - 仅当站点配置 `admin: true` 时可用（运营者自有的管理员账号）。
//   - 上游契约见 docs/upstream-contracts.md §5。
//   - 密钥类字段（key 等）永不进入记录与输出（records.CleanChannelRaw）。
package channels

import (
	"opscollector/internal/config"
	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"
	"opscollector/internal/records"
)

// ListOptions 列表过滤。
type ListOptions struct {
	PageSize int
	MaxPages int
	// StatusFilter: "" 全部 | "enabled" | "disabled"
	StatusFilter string
}

// Manager 是渠道管理接口（管理员面）。
type Manager interface {
	Kind() string

	// List 返回渠道列表与总数。
	List(opts ListOptions) ([]records.Channel, int64, error)
	// Get 单个渠道详情。
	Get(id int64) (*records.Channel, error)
	// Add 添加渠道。body 为站点原生请求体（从文件读入，避免命令行泄密）。
	// new-api: {"channel": {...}} 会自动包装；直接给渠道字段亦可。
	Add(body map[string]any) (*records.Channel, error)
	// Update 更新渠道（body 需含站点要求的定位字段，如 new-api 的 id）。
	Update(body map[string]any) (*records.Channel, error)
	// Delete 删除渠道。
	Delete(id int64) error
	// SetStatus 启停渠道。enabled / disabled（映射到站点原生状态码）。
	SetStatus(id int64, status records.ChannelStatus) error
	// Test 测试渠道连通性（sub2api 暂无等价单渠道端点 → UNSUPPORTED）。
	Test(id int64, model string) (map[string]any, error)
}

// Make 按站点类型构造渠道管理器；未开启 admin 时报 ADMIN_REQUIRED。
func Make(site *config.SiteConfig, client *httpc.Client) (Manager, error) {
	if !site.AdminEnabled() {
		return nil, ierr.New("ADMIN_REQUIRED",
			"站点 "+site.SiteID+" 未开启管理员面：请在配置中设置 \"admin\": true"+
				"（仅限运营者自有的管理员账号）")
	}
	switch site.Kind {
	case "new_api":
		return NewNewApiChannels(site, client)
	case "sub2api":
		return NewSub2ApiChannels(site, client)
	}
	return nil, ierr.Configf("未知站点类型: %s", site.Kind)
}

// normalizeStatus 归一化渠道状态。
func normalizeStatus(code int64) records.ChannelStatus {
	switch code {
	case 1:
		return records.ChannelEnabled
	case 2:
		return records.ChannelManualDisabled
	case 3:
		return records.ChannelAutoDisabled
	}
	return records.ChannelStatusUnknown
}
