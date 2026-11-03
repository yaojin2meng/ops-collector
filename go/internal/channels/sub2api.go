package channels

import (
	"fmt"
	"net/url"
	"strconv"

	"opscollector/internal/auth"
	"opscollector/internal/config"
	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"
	"opscollector/internal/records"
)

// Sub2ApiChannels 对接 Wei-Shaw/sub2api 管理面渠道接口。
// 契约：backend/internal/server/routes/admin.go + handler/admin/channel_handler.go
//   - GET    /api/v1/admin/channels?page=&page_size=
//   - GET    /api/v1/admin/channels/:id
//   - POST   /api/v1/admin/channels   body {name, model_pricing:[...], group_ids,...}
//   - PUT    /api/v1/admin/channels/:id  body 部分更新 {status: "active"|"disabled",...}
//   - DELETE /api/v1/admin/channels/:id
//
// 渠道状态：active=enabled，disabled=manual_disabled。
// 无单渠道连通性测试端点（有独立的 channel-monitors 体系）→ Test 报 UNSUPPORTED。
type Sub2ApiChannels struct {
	site   *config.SiteConfig
	client *httpc.Client
	auth   auth.Authenticator
}

func NewSub2ApiChannels(site *config.SiteConfig, client *httpc.Client) (*Sub2ApiChannels, error) {
	a, ok := auth.MakeAuthenticator("sub2api")
	if !ok {
		return nil, ierr.New(ierr.CodeInternal, "sub2api 认证器缺失")
	}
	return &Sub2ApiChannels{site: site, client: client, auth: a}, nil
}

func (m *Sub2ApiChannels) Kind() string { return "sub2api" }

const subAdminChannels = "/api/v1/admin/channels"

// headers 每次经 EnsureSession 取会话（同 new-api，管理器无共享状态）。
func (m *Sub2ApiChannels) headers() (map[string]string, error) {
	ctx, err := m.auth.EnsureSession(siteView{m.site}, m.client)
	if err != nil {
		return nil, err
	}
	return ctx.Headers, nil
}

// get 带 401 自愈（refresh → 重登，收敛到 auth.WithSelfHeal）。
func (m *Sub2ApiChannels) get(path string, params url.Values) (any, error) {
	return auth.WithSelfHeal(siteView{m.site}, m.client, m.auth,
		func(headers map[string]string) (any, error) {
			return m.client.Get(path, params, headers)
		})
}

// do 带自愈的任意方法请求。
func (m *Sub2ApiChannels) do(method, path string, body any) (any, error) {
	return auth.WithSelfHeal(siteView{m.site}, m.client, m.auth,
		func(headers map[string]string) (any, error) {
			return m.client.DoJSON(method, path, nil, body, headers)
		})
}

// List 拉取渠道列表（分页）。statusFilter: "" 全部 / "enabled" / "disabled"。
func (m *Sub2ApiChannels) List(opts ListOptions) ([]records.Channel, int64, error) {
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}
	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = 50
	}
	var out []records.Channel
	var total int64
	for page := 1; page <= maxPages; page++ {
		params := url.Values{"page": {strconv.Itoa(page)},
			"page_size": {strconv.Itoa(pageSize)}}
		// sub2api List 的 status 过滤参数形态未从源码锁定，客户端侧过滤
		data, err := m.get(subAdminChannels, params)
		if err != nil {
			return nil, 0, err
		}
		data, err = httpc.UnwrapSub2API(data, "渠道列表")
		if err != nil {
			return nil, 0, err
		}
		items, t, err := httpc.PaginatedItems(data, "渠道列表")
		if err != nil {
			return nil, 0, err
		}
		total = t
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			if cm, ok := it.(map[string]any); ok {
				ch := mapSub2ApiChannel(m.site, cm)
				switch opts.StatusFilter {
				case "enabled":
					if ch.Status != records.ChannelEnabled {
						continue
					}
				case "disabled":
					if ch.Status == records.ChannelEnabled {
						continue
					}
				}
				out = append(out, ch)
			}
		}
		if int64(page*pageSize) >= total {
			break
		}
	}
	if opts.StatusFilter != "" {
		// 客户端过滤后 total 不再等于列表长度，如实标注
		total = int64(len(out))
	}
	return out, total, nil
}

// Get 单个渠道。
func (m *Sub2ApiChannels) Get(id int64) (*records.Channel, error) {
	data, err := m.get(subAdminChannels+"/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return nil, err
	}
	data, err = httpc.UnwrapSub2API(data, "渠道详情")
	if err != nil {
		return nil, err
	}
	cm, ok := data.(map[string]any)
	if !ok {
		return nil, ierr.New(ierr.CodeAPIError, "渠道详情不是对象")
	}
	ch := mapSub2ApiChannel(m.site, cm)
	return &ch, nil
}

// Add 添加渠道（body 原样透传：name 必填，model_pricing[].models 至少 1 个）。
func (m *Sub2ApiChannels) Add(body map[string]any) (*records.Channel, error) {
	if _, ok := body["name"].(string); !ok || body["name"].(string) == "" {
		return nil, ierr.Configf("sub2api 渠道创建 body 必须包含 name")
	}
	resp, err := m.do("POST", subAdminChannels, body)
	if err != nil {
		return nil, err
	}
	// 必须先解包 {code, message, data} 包络，data 才是创建结果
	data, err := httpc.UnwrapSub2API(resp, "渠道创建")
	if err != nil {
		return nil, err
	}
	if dm, ok := data.(map[string]any); ok {
		if idv, ok := httpc.AsInt(dm["id"]); ok && idv > 0 {
			return m.Get(idv)
		}
		return mapSub2ApiChannelPtr(m.site, dm), nil
	}
	// 上游已创建但响应形态不认识：绝不静默返回 (nil, nil)
	return nil, ierr.New(ierr.CodeAddRespUnknown,
		"渠道可能已创建，但响应形态未识别；请用 channels list 按 name 对账后取 id")
}

// Update 更新渠道：body 部分透传（PUT /:id）。
func (m *Sub2ApiChannels) Update(body map[string]any) (*records.Channel, error) {
	id, ok := httpc.AsInt(body["id"])
	if !ok || id <= 0 {
		return nil, ierr.Configf("sub2api 渠道更新 body 必须包含数字 id")
	}
	payload := map[string]any{}
	for k, v := range body {
		if k != "id" {
			payload[k] = v
		}
	}
	if _, err := m.do("PUT", subAdminChannels+"/"+strconv.FormatInt(id, 10), payload); err != nil {
		return nil, err
	}
	return m.Get(id)
}

// Delete 删除渠道。必须解包校验 code==0（sub2api 对业务失败也返回 HTTP 200）。
func (m *Sub2ApiChannels) Delete(id int64) error {
	resp, err := m.do("DELETE", subAdminChannels+"/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return err
	}
	_, err = httpc.UnwrapSub2API(resp, "渠道删除")
	return err
}

// SetStatus 启停渠道：enabled→active，disabled→disabled。解包校验 code==0。
func (m *Sub2ApiChannels) SetStatus(id int64, status records.ChannelStatus) error {
	var s string
	switch status {
	case records.ChannelEnabled:
		s = "active"
	case records.ChannelManualDisabled, records.ChannelAutoDisabled, "disabled":
		s = "disabled"
	default:
		return ierr.Configf("不支持的状态: %s（可选 enabled / disabled）", status)
	}
	resp, err := m.do("PUT", subAdminChannels+"/"+strconv.FormatInt(id, 10),
		map[string]any{"status": s})
	if err != nil {
		return err
	}
	_, err = httpc.UnwrapSub2API(resp, "渠道状态更新")
	return err
}

// Test sub2api 无单渠道测试端点（channel-monitors 体系独立）。
func (m *Sub2ApiChannels) Test(id int64, model string) (map[string]any, error) {
	return nil, ierr.New(ierr.CodeUnsupported,
		fmt.Sprintf("sub2api 无单渠道测试端点（id=%d）；请使用站点管理后台的 channel-monitors", id))
}

// mapSub2ApiChannel 把 sub2api channelResponse 映射为统一记录。
func mapSub2ApiChannel(site *config.SiteConfig, m map[string]any) records.Channel {
	name, _ := m["name"].(string)
	ch := records.Channel{
		SchemaVersion: records.SchemaVersion,
		SiteID:        site.SiteID,
		SiteKind:      "sub2api",
		ChannelID:     ptrInt(m["id"]),
		Name:          name,
		Raw:           records.CleanChannelRaw(m),
	}
	// sub2api 渠道模型在各 model_pricing[].models
	modelSet := map[string]bool{}
	if pricings, ok := m["model_pricing"].([]any); ok {
		for _, pi := range pricings {
			p, ok := pi.(map[string]any)
			if !ok {
				continue
			}
			if pf, ok := p["platform"].(string); ok && ch.Platform == "" {
				ch.Platform = pf
			}
			if models, ok := p["models"].([]any); ok {
				for _, mo := range models {
					if s, ok := mo.(string); ok && s != "" {
						modelSet[s] = true
					}
				}
			}
		}
	}
	for mo := range modelSet {
		ch.Models = append(ch.Models, mo)
	}
	// 排序保证确定性
	if len(ch.Models) > 1 {
		for i := 1; i < len(ch.Models); i++ {
			for j := i; j > 0 && ch.Models[j] < ch.Models[j-1]; j-- {
				ch.Models[j], ch.Models[j-1] = ch.Models[j-1], ch.Models[j]
			}
		}
	}
	if gids, ok := m["group_ids"].([]any); ok {
		for _, g := range gids {
			if id, ok := httpc.AsInt(g); ok {
				ch.GroupIDs = append(ch.GroupIDs, id)
			}
		}
	}
	statusRaw, _ := m["status"].(string)
	switch statusRaw {
	case "active":
		ch.Status = records.ChannelEnabled
		one := int64(1)
		ch.StatusCode = &one
	case "disabled":
		ch.Status = records.ChannelManualDisabled
		two := int64(2)
		ch.StatusCode = &two
	default:
		ch.Status = records.ChannelStatusUnknown
	}
	return ch
}

func mapSub2ApiChannelPtr(site *config.SiteConfig, m map[string]any) *records.Channel {
	ch := mapSub2ApiChannel(site, m)
	return &ch
}
