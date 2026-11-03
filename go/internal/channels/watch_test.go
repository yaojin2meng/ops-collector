package channels

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"opscollector/internal/httpc"
	"opscollector/internal/records"
	"opscollector/internal/testmock"
)

func jsonMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// ---- 快照 diff 逻辑（不依赖网络） ----

func mkChan(id int64, status records.ChannelStatus, rt int64, balance float64,
	models ...string) records.Channel {
	name := "ch-" + string(rune('a'+id))
	c := records.Channel{ChannelID: id, Name: name, Status: status}
	if rt > 0 {
		v := rt
		c.ResponseTimeMs = &v
	}
	if balance > 0 {
		b := balance
		c.BalanceUSD = &b
	}
	c.Models = models
	return c
}

func TestDiffBaseline(t *testing.T) {
	dir := t.TempDir()
	store := NewWatchStore(dir)
	site := &config_Site{SiteID: "s1"}
	chs := []records.Channel{mkChan(1, records.ChannelEnabled, 300, 10)}
	changes, baseline, _, err := Check(site, stubMgr{chs}, store, DefaultNotifyOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !baseline || len(changes) != 0 {
		t.Fatalf("首次应为基线: baseline=%v changes=%d", baseline, len(changes))
	}
	// 快照已写入
	if store.load("s1") == nil {
		t.Fatal("基线快照未写入")
	}
}

func TestDiffStatusChangeAndRemove(t *testing.T) {
	dir := t.TempDir()
	store := NewWatchStore(dir)
	site := &config_Site{SiteID: "s1"}
	// 基线
	_, _, _, _ = Check(site, stubMgr{[]records.Channel{
		mkChan(1, records.ChannelEnabled, 300, 10),
		mkChan(2, records.ChannelEnabled, 400, 20),
	}}, store, DefaultNotifyOptions())

	// 第二轮：ch1 禁用，ch2 被删，ch3 新增
	changes, baseline, _, err := Check(site, stubMgr{[]records.Channel{
		mkChan(1, records.ChannelManualDisabled, 300, 10),
		mkChan(3, records.ChannelEnabled, 500, 5),
	}}, store, DefaultNotifyOptions())
	if err != nil || baseline {
		t.Fatalf("%v baseline=%v", err, baseline)
	}
	types := map[string]bool{}
	for _, c := range changes {
		types[c.Type+"/"+string(rune('a'+c.ChannelID))] = true
	}
	// mkChan 命名：id=1→ch-b, 2→ch-c, 3→ch-d
	if !types["status_changed/b"] {
		t.Fatalf("缺 status_changed: %v", changes)
	}
	if !types["channel_removed/c"] {
		t.Fatalf("缺 channel_removed: %v", changes)
	}
	if !types["channel_added/d"] {
		t.Fatalf("缺 channel_added: %v", changes)
	}
	if len(changes) != 3 {
		t.Fatalf("变化数错误: %v", changes)
	}
	// detail 检查
	for _, c := range changes {
		if c.Type == "status_changed" {
			if c.Detail["old_status"] != "enabled" || c.Detail["new_status"] != "manual_disabled" {
				t.Fatalf("detail 错误: %v", c.Detail)
			}
		}
	}
}

func TestDiffThresholdsEdgeCases(t *testing.T) {
	dir := t.TempDir()
	store := NewWatchStore(dir)
	site := &config_Site{SiteID: "s1"}
	opts := NotifyOptions{OnRT: true, RTAboveMs: 1000,
		OnBalance: true, BalanceBelowUSD: 5}
	// 基线：rt 800（低于阈值）、余额 10（高于阈值）
	_, _, _, _ = Check(site, stubMgr{[]records.Channel{
		mkChan(1, records.ChannelEnabled, 800, 10),
	}}, store, opts)

	// 第二轮：rt 升到 1200（越线→提醒），余额降到 3（越线→提醒）
	changes, _, _, err := Check(site, stubMgr{[]records.Channel{
		mkChan(1, records.ChannelEnabled, 1200, 3),
	}}, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("期望 2 条变化: %v", changes)
	}
	found := map[string]bool{}
	for _, c := range changes {
		found[c.Type] = true
	}
	if !found["response_time_spike"] || !found["balance_below"] {
		t.Fatalf("缺阈值提醒: %v", changes)
	}

	// 第三轮：rt 维持高位 1500（已越线，不再重复提醒）、余额 2（继续低于，不重复）
	changes, _, _, err = Check(site, stubMgr{[]records.Channel{
		mkChan(1, records.ChannelEnabled, 1500, 2),
	}}, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("持续越线不应重复提醒: %v", changes)
	}

	// 第四轮：rt 回落后再次越线 → 再次提醒
	_, _, _, _ = Check(site, stubMgr{[]records.Channel{
		mkChan(1, records.ChannelEnabled, 500, 2),
	}}, store, opts)
	changes, _, _, _ = Check(site, stubMgr{[]records.Channel{
		mkChan(1, records.ChannelEnabled, 2000, 2),
	}}, store, opts)
	if len(changes) != 1 || changes[0].Type != "response_time_spike" {
		t.Fatalf("回落再越线应提醒: %v", changes)
	}
}

func TestDiffModelsChange(t *testing.T) {
	dir := t.TempDir()
	store := NewWatchStore(dir)
	site := &config_Site{SiteID: "s1"}
	opts := NotifyOptions{OnModels: true}
	_, _, _, _ = Check(site, stubMgr{[]records.Channel{
		mkChan(1, records.ChannelEnabled, 300, 10, "m1", "m2"),
	}}, store, opts)
	changes, _, _, _ := Check(site, stubMgr{[]records.Channel{
		mkChan(1, records.ChannelEnabled, 300, 10, "m1", "m3"),
	}}, store, opts)
	if len(changes) != 1 || changes[0].Type != "models_changed" {
		t.Fatalf("模型变化未检出: %v", changes)
	}
}

func TestWatchLoopTiming(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	site := adminNapiSite(srv.URL, true)
	mgr, _ := Make(site, httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	wstore := NewWatchStore(t.TempDir())

	// 间隔 1s、时长 3s：应跑 ~4 轮（0s,1s,2s,3s 边界），基线 1 轮 + 3 轮无变化
	changes, ticks, err := Watch(site, mgr, wstore, 1, 3, DefaultNotifyOptions(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	if ticks < 3 || ticks > 5 {
		t.Fatalf("轮数异常: %d", ticks)
	}
	if len(changes) != 0 {
		t.Fatalf("无变化却报变化: %v", changes)
	}
}

func TestWatchDetectsLiveChange(t *testing.T) {
	store := installStore(t)
	store["na1:password"] = testmock.NAPIPassword
	srv := testmock.NewApiSite()
	defer srv.Close()
	site := adminNapiSite(srv.URL, true)
	mgr, _ := Make(site, httpc.New(srv.URL, httpc.Options{MinIntervalS: 0}))
	wstore := NewWatchStore(t.TempDir())

	// 后台 goroutine：800ms 后禁用渠道 1（模拟站点状态变化）
	done := make(chan struct{})
	go func() {
		tim := timeAfter(800)
		<-tim
		_ = mgr.SetStatus(1, records.ChannelManualDisabled)
		close(done)
	}()

	changes, _, err := Watch(site, mgr, wstore, 1, 3, DefaultNotifyOptions(), silentLogger())
	if err != nil {
		t.Fatal(err)
	}
	<-done
	found := false
	for _, c := range changes {
		if c.Type == "status_changed" && c.ChannelID == 1 {
			found = true
			if c.Detail["new_status"] != "manual_disabled" {
				t.Fatalf("detail 错误: %v", c.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("运行期变化未检出: %v", changes)
	}
	// 通知文件已写入
	if _, err := osStat(filepath.Join(wstore.dir, "..", "..", "logs", "channel-watch", "na1.jsonl")); err != nil {
		t.Fatalf("通知文件缺失: %v", err)
	}
}
