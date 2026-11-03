package channels

import (
	"net/url"
	"strconv"
	"strings"

	"opscollector/internal/auth"
	"opscollector/internal/config"
	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"
	"opscollector/internal/records"
)

// NewApiChannels 对接 QuantumNous/new-api 管理面渠道接口。
// 契约：router/channel-router.go + controller/channel.go（AdminAuth）。
//   - GET  /api/channel/?p=&page_size=[&status=1|0]  → data{items,total,...}
//   - GET  /api/channel/:id
//   - POST /api/channel/  body {mode, channel:{...}}（ChannelSensitiveWrite）
//   - PUT  /api/channel/  body {...channel 字段}
//   - DELETE /api/channel/:id
//   - POST /api/channel/:id/status  body {status: 1|2}
//   - GET  /api/channel/test/:id?model=
//
// 状态码：1=enabled 2=manual_disabled 3=auto_disabled。
type NewApiChannels struct {
	site   *config.SiteConfig
	client *httpc.Client
	auth   auth.Authenticator
}

func NewNewApiChannels(site *config.SiteConfig, client *httpc.Client) (*NewApiChannels, error) {
	a, ok := auth.MakeAuthenticator("new_api")
	if !ok {
		return nil, ierr.New(ierr.CodeInternal, "new_api 认证器缺失")
	}
	return &NewApiChannels{site: site, client: client, auth: a}, nil
}

func (m *NewApiChannels) Kind() string { return "new_api" }

// headers 每次经 EnsureSession 取会话（会话缓存由 keyring 承担，
// 管理器无共享可变状态 → 并发安全）。
func (m *NewApiChannels) headers() (map[string]string, error) {
	ctx, err := m.auth.EnsureSession(siteView{m.site}, m.client)
	if err != nil {
		return nil, err
	}
	return ctx.Headers, nil
}

// get 带 401 自愈的请求（收敛到 auth.WithSelfHeal 单一实现）。
func (m *NewApiChannels) get(path string, params url.Values) (any, error) {
	return auth.WithSelfHeal(siteView{m.site}, m.client, m.auth,
		func(headers map[string]string) (any, error) {
			return m.client.Get(path, params, headers)
		})
}

func (m *NewApiChannels) post(path string, body any) (any, error) {
	h, err := m.headers()
	if err != nil {
		return nil, err
	}
	return m.client.Post(path, body, h)
}

// do 带自愈的任意方法请求（PUT/DELETE 等）。
func (m *NewApiChannels) do(method, path string, body any) (any, error) {
	return auth.WithSelfHeal(siteView{m.site}, m.client, m.auth,
		func(headers map[string]string) (any, error) {
			return m.client.DoJSON(method, path, nil, body, headers)
		})
}

// List 拉取渠道列表（分页）。statusFilter: "" 全部 / "enabled" / "disabled"。
// 过滤统一在客户端按归一化状态做（disabled = manual_disabled+auto_disabled），
// 保证两种站点语义一致。
func (m *NewApiChannels) List(opts ListOptions) ([]records.Channel, int64, error) {
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = 100
	}
	maxPages := opts.MaxPages
	if maxPages <= 0 {
		maxPages = 50
	}
	out := make([]records.Channel, 0, pageSize)
	var total int64
	for page := 1; page <= maxPages; page++ {
		params := url.Values{"p": {strconv.Itoa(page)},
			"page_size": {strconv.Itoa(pageSize)}}
		data, err := m.get("/api/channel/", params)
		if err != nil {
			return nil, 0, err
		}
		data, err = httpc.UnwrapNewAPI(data, "渠道列表")
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
				ch := mapNewApiChannel(m.site, cm)
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
		total = int64(len(out))
	}
	return out, total, nil
}

// Get 单个渠道。
func (m *NewApiChannels) Get(id int64) (*records.Channel, error) {
	data, err := m.get("/api/channel/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return nil, err
	}
	data, err = httpc.UnwrapNewAPI(data, "渠道详情")
	if err != nil {
		return nil, err
	}
	cm, ok := data.(map[string]any)
	if !ok {
		return nil, ierr.New(ierr.CodeAPIError, "渠道详情不是对象")
	}
	ch := mapNewApiChannel(m.site, cm)
	return &ch, nil
}

// Add 添加渠道：body 里给渠道字段；自动包一层 {"channel": ...}。
func (m *NewApiChannels) Add(body map[string]any) (*records.Channel, error) {
	payload := body
	if _, has := body["channel"]; !has {
		payload = map[string]any{"mode": "single", "channel": body}
	}
	resp, err := m.post("/api/channel/", payload)
	if err != nil {
		return nil, err
	}
	// 必须先解包 {success, message, data} 包络，data 才是创建结果
	data, err := httpc.UnwrapNewAPI(resp, "渠道创建")
	if err != nil {
		return nil, err
	}
	// data 可能是创建的渠道对象或 {id: N}；有 id 就回读详情保证字段完整
	if dm, ok := data.(map[string]any); ok {
		if idv, ok := httpc.AsInt(dm["id"]); ok && idv > 0 {
			return m.Get(idv)
		}
	}
	// 上游已创建但响应形态不认识：绝不静默返回 (nil, nil)
	return nil, ierr.New(ierr.CodeAddRespUnknown,
		"渠道可能已创建，但响应形态未识别；请用 channels list 按 name 对账后取 id")
}

// Update 更新渠道：Get-merge-PUT —— body 只需给要改的字段（+id），
// 与现有配置合并后整包 PUT（new-api 的 PUT 是全量语义，直接透传部分字段
// 会把未提供字段清空）。
func (m *NewApiChannels) Update(body map[string]any) (*records.Channel, error) {
	id, ok := httpc.AsInt(body["id"])
	if !ok || id <= 0 {
		return nil, ierr.Configf("new-api 渠道更新 body 必须包含数字 id")
	}
	current, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	merged := map[string]any{}
	for k, v := range current.Raw {
		merged[k] = v
	}
	for k, v := range body {
		merged[k] = v
	}
	// Raw 里可能带 JSON 解包后的数组形态；PUT 需要逗号串的 models/group
	if models, ok := merged["models"].([]any); ok {
		merged["models"] = joinCSV(models)
	}
	if groups, ok := merged["group"].([]any); ok {
		merged["group"] = joinCSV(groups)
	}
	if _, err := m.do("PUT", "/api/channel/", merged); err != nil {
		return nil, err
	}
	return m.Get(id)
}

// Delete 删除渠道。必须解包校验 success（new-api 对删除失败也返回
// HTTP 200 + success:false，只看状态码会把失败当成功）。
func (m *NewApiChannels) Delete(id int64) error {
	resp, err := m.do("DELETE", "/api/channel/"+strconv.FormatInt(id, 10), nil)
	if err != nil {
		return err
	}
	if _, err := httpc.UnwrapNewAPI(resp, "渠道删除"); err != nil {
		return err
	}
	return nil
}

// SetStatus 启停渠道：enabled→1，disabled→2（手动禁用；3 为系统自动禁用态，
// 不允许手动设置）。同样解包校验 success。
func (m *NewApiChannels) SetStatus(id int64, status records.ChannelStatus) error {
	var code int64
	switch status {
	case records.ChannelEnabled:
		code = 1
	case records.ChannelManualDisabled, records.ChannelAutoDisabled, "disabled":
		code = 2
	default:
		return ierr.Configf("不支持的状态: %s（可选 enabled / disabled）", status)
	}
	resp, err := m.post("/api/channel/"+strconv.FormatInt(id, 10)+"/status",
		map[string]any{"status": code})
	if err != nil {
		return err
	}
	_, err = httpc.UnwrapNewAPI(resp, "渠道状态更新")
	return err
}

// testRawAllowlist 限制渠道测试响应的输出字段（全项目唯一透传原始 map 的口）。
var testRawAllowlist = map[string]bool{
	"time": true, "message": true, "model": true, "channel": true,
	"code": true, "endpoint": true,
}

// Test 测试渠道（可选 model）。响应过白名单后返回。
func (m *NewApiChannels) Test(id int64, model string) (map[string]any, error) {
	params := url.Values{}
	if model != "" {
		params.Set("model", model)
	}
	data, err := m.get("/api/channel/test/"+strconv.FormatInt(id, 10), params)
	if err != nil {
		return nil, err
	}
	data, err = httpc.UnwrapNewAPI(data, "渠道测试")
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if dm, ok := data.(map[string]any); ok {
		for k, v := range dm {
			if testRawAllowlist[k] {
				out[k] = v
			}
		}
	}
	return out, nil
}

// joinCSV 把 JSON 解包出的字符串数组拼回逗号串（PUT 全量语义需要）。
func joinCSV(items []any) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok && s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ",")
}

// mapNewApiChannel 把 new-api Channel JSON 映射为统一记录。
func mapNewApiChannel(site *config.SiteConfig, m map[string]any) records.Channel {
	strPtr := func(k string) *string {
		if v, ok := m[k].(string); ok && v != "" {
			return &v
		}
		return nil
	}
	intPtr := func(k string) *int64 {
		if v, ok := httpc.AsInt(m[k]); ok {
			return &v
		}
		return nil
	}
	name, _ := m["name"].(string)
	ch := records.Channel{
		SchemaVersion: records.SchemaVersion,
		SiteID:        site.SiteID,
		SiteKind:      "new_api",
		ChannelID:     ptrInt(m["id"]),
		Name:          name,
	}
	ch.Type = intPtr("type")
	if code, ok := httpc.AsInt(m["status"]); ok {
		c := code
		ch.StatusCode = &c
		ch.Status = normalizeStatus(code)
	}
	if models, ok := m["models"].(string); ok && models != "" {
		ch.Models = splitCSV(models)
	}
	if group, ok := m["group"].(string); ok && group != "" {
		ch.Groups = splitCSV(group)
	}
	ch.Priority = intPtr("priority")
	ch.Weight = intPtr("weight")
	ch.ResponseTimeMs = intPtr("response_time")
	ch.TestTime = intPtr("test_time")
	ch.UsedQuota = intPtr("used_quota")
	if b, ok := httpc.AsF64(m["balance"]); ok {
		ch.BalanceUSD = &b
	}
	ch.BaseURL = strPtr("base_url")
	ch.Tag = strPtr("tag")
	// 多Key（密钥聚合）渠道摘要：只取计数与模式，key 状态明细不落盘
	if info, ok := m["channel_info"].(map[string]any); ok {
		ch.MultiKey = boolPtr(info["is_multi_key"])
		if size, ok := httpc.AsInt(info["multi_key_size"]); ok {
			ch.MultiKeySize = &size
		}
		if mode, ok := info["multi_key_mode"].(string); ok && mode != "" {
			ch.MultiKeyMode = &mode
		}
		if statusList, ok := info["multi_key_status_list"].(map[string]any); ok {
			enabled := int64(0)
			for _, s := range statusList {
				if code, ok := httpc.AsInt(s); ok && code == 1 {
					enabled++
				}
			}
			ch.MultiKeyEnabled = &enabled
		}
	}
	ch.Raw = records.CleanChannelRaw(m)
	return ch
}

func ptrInt(v any) int64 {
	id, _ := httpc.AsInt(v)
	return id
}

func boolPtr(v any) *bool {
	if b, ok := httpc.AsBool(v); ok {
		return &b
	}
	return nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
