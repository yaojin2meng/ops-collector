package channels

import (
	"net/http"
	"net/http/httptest"
	"testing"

	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"
	"opscollector/internal/testmock"
)

// 删除不存在的渠道：new-api 返回 HTTP 200 + success:false，
// 写路径必须解包并如实报错（不能把失败当成功）。
func TestDeleteFailureUnwrapped(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	mgr, _ := Make(adminNapiSite(srv.URL, true),
		httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	err := mgr.Delete(9999)
	if err == nil {
		t.Fatal("删除不存在的渠道应报错（success:false 必须被解包）")
	}
	if ierr.CodeOf(err) != ierr.CodeAPIError {
		t.Fatalf("错误码: %v", err)
	}
}

// SetStatus 对不存在渠道同理。
func TestSetStatusFailureUnwrapped(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	mgr, _ := Make(adminNapiSite(srv.URL, true),
		httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	if err := mgr.SetStatus(9999, records_ChannelEnabled); err == nil {
		t.Fatal("对不存在渠道 set-status 应报错")
	}
}

// Add 响应形态不认识 → ADD_RESPONSE_UNKNOWN（绝不静默 (nil,nil) 报成功）。
func TestAddUnknownResponseShape(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	// 定制 mock：创建"成功"但 data 是字符串（形态漂移）
	mux := http.NewServeMux()
	mux.HandleFunc("/api/user/login/encryption-key", func(w http.ResponseWriter, r *http.Request) {
		writeJSONTest(w, map[string]any{"success": true, "data": map[string]any{"enabled": false}})
	})
	mux.HandleFunc("/api/user/login", func(w http.ResponseWriter, r *http.Request) {
		writeJSONTest(w, map[string]any{"success": true, "data": map[string]any{
			"access_token": testmock.NAPIToken, "access_expires_at": testmock.Now + 3600,
			"user": map[string]any{"id": 42}}})
	})
	mux.HandleFunc("/api/channel/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONTest(w, map[string]any{"success": true, "message": "", "data": "weird-shape"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mgr, _ := Make(adminNapiSite(srv.URL, true),
		httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	ch, err := mgr.Add(map[string]any{"name": "x", "type": 1, "key": "k",
		"models": "m", "group": "default"})
	if err == nil || ierr.CodeOf(err) != ierr.CodeAddRespUnknown {
		t.Fatalf("期望 ADD_RESPONSE_UNKNOWN，得到 %v (ch=%v)", err, ch)
	}
}

// PAT（api_key）模式必须带 New-Api-User 头（旧版鉴权链强制）。
func TestPATSessionSendsNewApiUserHeader(t *testing.T) {
	store := installStore(t)
	store["na1:pat"] = "pat-token-1"
	srv := testmock.NewApiSite()
	defer srv.Close()
	site := adminNapiSite(srv.URL, true)
	site.Auth = patAuth(site.SiteID)
	mgr, _ := Make(site, httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	ch, err := mgr.Get(1)
	if err != nil {
		t.Fatal(err)
	}
	if ch.ChannelID != 1 {
		t.Fatalf("PAT 模式渠道获取失败: %+v", ch)
	}
}

// Watch duration 预算：interval=2s、duration=1s → 最多 2 轮（0s 与 1s 边界），
// 且绝不溢出到 2s（修复前会跑满一个完整 interval）。
func TestWatchDurationBudgetExact(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	site := adminNapiSite(srv.URL, true)
	mgr, _ := Make(site, httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	wstore := NewWatchStore(t.TempDir())
	start := wallNow()
	_, ticks, err := Watch(site, mgr, wstore, 2, 1, DefaultNotifyOptions(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if ticks < 1 || ticks > 2 {
		t.Fatalf("期望 1~2 轮，得到 %d", ticks)
	}
	if elapsed := wallSince(start); elapsed > 1900*milli {
		t.Fatalf("duration 预算溢出: %v", elapsed)
	}
}
