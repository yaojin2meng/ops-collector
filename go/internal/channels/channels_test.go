package channels

import (
	"strings"
	"testing"

	"opscollector/internal/config"
	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"
	"opscollector/internal/records"
	"opscollector/internal/secrets"
	"opscollector/internal/testmock"
)

func installStore(t *testing.T) map[string]string {
	t.Helper()
	m := map[string]string{}
	secrets.SetKeyringForTest(
		func(e string) (string, error) { return m[e], nil },
		func(e, v string) error { m[e] = v; return nil },
		func(e string) error { delete(m, e); return nil },
	)
	t.Cleanup(secrets.ResetKeyring)
	return m
}

func adminNapiSite(baseURL string, admin bool) *config.SiteConfig {
	verify, a := true, admin
	return &config.SiteConfig{
		SiteID: "na1", Kind: "new_api", BaseURL: baseURL,
		Auth: config.AuthConfig{Mode: "password", Username: "opuser",
			SecretRef: "na1:password"},
		Enabled: &verify, Admin: &a, VerifyTLS: &verify,
	}
}

func adminSubSite(baseURL string, admin bool) *config.SiteConfig {
	verify, a := true, admin
	return &config.SiteConfig{
		SiteID: "sb1", Kind: "sub2api", BaseURL: baseURL,
		Auth: config.AuthConfig{Mode: "password", Email: "ops@example.com",
			SecretRef: "sb1:password"},
		Enabled: &verify, Admin: &a, VerifyTLS: &verify,
	}
}

// ---- 门控 ----

func TestAdminGate(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	// admin: false → 拒绝
	_, err := Make(adminNapiSite(srv.URL, false),
		httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	if err == nil || ierr.CodeOf(err) != "ADMIN_REQUIRED" {
		t.Fatalf("期望 ADMIN_REQUIRED，得到 %v", err)
	}
	// admin: true → 可用
	if _, err := Make(adminNapiSite(srv.URL, true),
		httpc.New(srv.URL, httpc.Options{MinIntervalS: 0})); err != nil {
		t.Fatal(err)
	}
}

// ---- new-api 渠道 CRUD ----

func TestNewApiChannelsListAndSecretRedaction(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	mgr, _ := Make(adminNapiSite(srv.URL, true),
		httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	items, total, err := mgr.List(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("total=%d items=%d", total, len(items))
	}
	byID := map[int64]records.Channel{}
	for _, c := range items {
		byID[c.ChannelID] = c
	}
	// 状态归一化：1→enabled，3→auto_disabled
	if byID[1].Status != records.ChannelEnabled || byID[2].Status != records.ChannelAutoDisabled {
		t.Fatalf("状态映射错误: %+v %+v", byID[1], byID[2])
	}
	// 密钥永不泄漏：raw 与整条记录 JSON 都不含 sk-SECRET
	full := jsonMarshal(items)
	if strings.Contains(string(full), "sk-SECRET") {
		t.Fatal("渠道密钥泄漏进输出")
	}
	if byID[1].Models == nil || len(byID[1].Models) != 2 {
		t.Fatalf("models 解析错误: %v", byID[1].Models)
	}
	if byID[2].Groups == nil || len(byID[2].Groups) != 2 {
		t.Fatalf("groups 解析错误: %v", byID[2].Groups)
	}
	if byID[1].BalanceUSD == nil || *byID[1].BalanceUSD != 12.5 {
		t.Fatalf("balance 错误: %v", byID[1].BalanceUSD)
	}
}

func TestNewApiChannelsCRUDLifecycle(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	mgr, _ := Make(adminNapiSite(srv.URL, true),
		httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))

	// add：自动包 {"channel": ...}，返回创建后的详情
	ch, err := mgr.Add(map[string]any{
		"name": "new-chan", "type": 1, "key": "sk-new",
		"models": "gpt-4o-mini", "group": "default"})
	if err != nil {
		t.Fatal(err)
	}
	if ch == nil || ch.ChannelID == 0 || ch.Name != "new-chan" {
		t.Fatalf("add 返回错误: %+v", ch)
	}
	// set-status disabled → 2
	if err := mgr.SetStatus(ch.ChannelID, records.ChannelManualDisabled); err != nil {
		t.Fatal(err)
	}
	got, err := mgr.Get(ch.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != records.ChannelManualDisabled {
		t.Fatalf("status 未生效: %+v", got)
	}
	// set-status enabled → 1
	if err := mgr.SetStatus(ch.ChannelID, records.ChannelEnabled); err != nil {
		t.Fatal(err)
	}
	// test
	res, err := mgr.Test(ch.ChannelID, "gpt-4o-mini")
	if err != nil {
		t.Fatal(err)
	}
	if res["time"] == nil {
		t.Fatalf("test 响应缺 time: %v", res)
	}
	// delete
	if err := mgr.Delete(ch.ChannelID); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Get(ch.ChannelID); err == nil {
		t.Fatal("删除后 get 应失败")
	}
}

// ---- sub2api 渠道 CRUD ----

func TestSub2ApiChannelsList(t *testing.T) {
	store := installStore(t)
	store["sb1:password"] = testmock.SUBPassword
	srv := testmock.Sub2ApiSite(false)
	defer srv.Close()
	mgr, _ := Make(adminSubSite(srv.URL, true),
		httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	items, total, err := mgr.List(ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("total=%d items=%d", total, len(items))
	}
	byID := map[int64]records.Channel{}
	for _, c := range items {
		byID[c.ChannelID] = c
	}
	if byID[11].Status != records.ChannelEnabled ||
		byID[12].Status != records.ChannelManualDisabled {
		t.Fatalf("状态映射错误: %+v %+v", byID[11], byID[12])
	}
	// models 从 model_pricing[].models 汇总
	if len(byID[11].Models) != 1 || byID[11].Models[0] != "claude-sonnet-4" {
		t.Fatalf("models 错误: %v", byID[11].Models)
	}
	if len(byID[11].GroupIDs) != 1 || byID[11].GroupIDs[0] != 3 {
		t.Fatalf("group_ids 错误: %v", byID[11].GroupIDs)
	}
	// enabled 过滤（客户端侧）
	items, total, err = mgr.List(ListOptions{StatusFilter: "enabled"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || total != 1 {
		t.Fatalf("enabled 过滤错误: %d %d", len(items), total)
	}
}

func TestSub2ApiChannelsLifecycle(t *testing.T) {
	store := installStore(t)
	store["sb1:password"] = testmock.SUBPassword
	srv := testmock.Sub2ApiSite(false)
	defer srv.Close()
	mgr, _ := Make(adminSubSite(srv.URL, true),
		httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))

	ch, err := mgr.Add(map[string]any{
		"name": "new-sub", "group_ids": []int64{3},
		"model_pricing": []map[string]any{
			{"platform": "claude", "models": []string{"claude-haiku"},
				"billing_mode": "token", "input_price": 1.0, "output_price": 5.0}}})
	if err != nil {
		t.Fatal(err)
	}
	if ch == nil || ch.ChannelID == 0 || ch.Status != records.ChannelEnabled {
		t.Fatalf("add 返回错误: %+v", ch)
	}
	// set-status disabled → "disabled"
	if err := mgr.SetStatus(ch.ChannelID, records.ChannelManualDisabled); err != nil {
		t.Fatal(err)
	}
	got, _ := mgr.Get(ch.ChannelID)
	if got.Status != records.ChannelManualDisabled {
		t.Fatalf("status 未生效: %+v", got)
	}
	// test → UNSUPPORTED
	if _, err := mgr.Test(ch.ChannelID, ""); err == nil ||
		ierr.CodeOf(err) != ierr.CodeUnsupported {
		t.Fatalf("sub2api test 应 UNSUPPORTED: %v", err)
	}
	// delete
	if err := mgr.Delete(ch.ChannelID); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Get(ch.ChannelID); err == nil {
		t.Fatal("删除后 get 应失败")
	}
}
