package channels

import (
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"opscollector/internal/config"
	ierr "opscollector/internal/errors"
	"opscollector/internal/logging"
	"opscollector/internal/records"
)

// ---- 变化检测（渠道状态自动更新 + 变化提醒模型） ----

// NotifyOptions 决定关注哪些变化。
type NotifyOptions struct {
	// OnStatus: 渠道启用/禁用状态变化（默认开）
	OnStatus bool
	// OnAddRemove: 渠道新增/删除（默认开）
	OnAddRemove bool
	// OnModels: 渠道模型列表变化（默认关）
	OnModels bool
	// OnRT: 响应时间超过 RTAboveMs 时提醒（0=不检测）
	OnRT      bool
	RTAboveMs int64
	// OnBalance: 余额低于 BalanceBelowUSD 时提醒（0=不检测）
	OnBalance       bool
	BalanceBelowUSD float64
}

// DefaultNotifyOptions 默认关注状态与增删。
func DefaultNotifyOptions() NotifyOptions {
	return NotifyOptions{OnStatus: true, OnAddRemove: true}
}

// Change 是一次检测到的变化。
type Change struct {
	Type       string         `json:"type"` // status_changed|channel_added|channel_removed|models_changed|response_time_spike|balance_below
	SiteID     string         `json:"site_id"`
	ChannelID  int64          `json:"channel_id"`
	Name       string         `json:"name"`
	Detail     map[string]any `json:"detail"`
	DetectedAt int64          `json:"detected_at"`
}

// chanSig 是 diff 用的渠道签名。
type chanSig struct {
	Status       string   `json:"status"`
	ResponseTime *int64   `json:"response_time,omitempty"`
	Balance      *float64 `json:"balance,omitempty"`
	ModelsHash   string   `json:"models_hash,omitempty"`
	Name         string   `json:"name"`
}

// WatchStore 持久化上次快照（var/state/channel-watch/<site>.json，原子写）。
type WatchStore struct {
	dir string
}

func NewWatchStore(varDir string) *WatchStore {
	dir := filepath.Join(varDir, "state", "channel-watch")
	_ = os.MkdirAll(dir, 0o755)
	return &WatchStore{dir: dir}
}

type watchState struct {
	SchemaVersion int                `json:"schema_version"`
	SiteID        string             `json:"site_id"`
	UpdatedAt     int64              `json:"updated_at"`
	Channels      map[string]chanSig `json:"channels"`
}

func (s *WatchStore) path(siteID string) string {
	return filepath.Join(s.dir, siteID+".json")
}

func (s *WatchStore) load(siteID string) *watchState {
	b, err := os.ReadFile(s.path(siteID))
	if err != nil {
		return nil
	}
	var st watchState
	if json.Unmarshal(b, &st) != nil || st.Channels == nil {
		return nil
	}
	return &st
}

func (s *WatchStore) save(st *watchState) error {
	full := map[string]any{"schema_version": 1, "site_id": st.SiteID,
		"updated_at": st.UpdatedAt, "channels": st.Channels}
	b, err := json.MarshalIndent(full, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, st.SiteID+"-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, s.path(st.SiteID)); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func snapshotSigs(chs []records.Channel) map[string]chanSig {
	sigs := make(map[string]chanSig, len(chs))
	for _, c := range chs {
		sigs[strconv.FormatInt(c.ChannelID, 10)] = sigOf(c)
	}
	return sigs
}

func sigOf(c records.Channel) chanSig {
	sig := chanSig{Status: string(c.Status), Name: c.Name}
	if c.ResponseTimeMs != nil {
		rt := *c.ResponseTimeMs
		sig.ResponseTime = &rt
	}
	if c.BalanceUSD != nil {
		b := *c.BalanceUSD
		sig.Balance = &b
	}
	if len(c.Models) > 0 {
		sorted := append([]string(nil), c.Models...)
		sort.Strings(sorted)
		h := fnv64(sorted)
		sig.ModelsHash = h
	}
	return sig
}

func fnv64(items []string) string {
	var h uint64 = 14695981039346656037
	for _, s := range items {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= 1099511628211
		}
		h ^= '^'
		h *= 1099511628211
	}
	return strconv.FormatUint(h, 16)
}

// Check 拉取当前渠道状态，与上次快照 diff，落新快照，返回变化列表。
// 首次运行只建立基线（changes 为空，baseline=true）。
// 列表因分页上限截断时跳过 removed 检测（防 phantom removed 告警风暴）。
func Check(site *config.SiteConfig, mgr Manager, store *WatchStore,
	opts NotifyOptions) (changes []Change, baseline bool, total int64, err error) {
	// 大 MaxPages：渠道数通常远小于日志量；上限兜底防失控
	chs, total, err := mgr.List(ListOptions{PageSize: 500, MaxPages: 40})
	if err != nil {
		return nil, false, 0, err
	}
	truncated := int64(len(chs)) < total
	now := time.Now().Unix()
	sigs := snapshotSigs(chs)
	prev := store.load(site.SiteID)

	if prev == nil {
		// 建立基线，不算变化
		if serr := store.save(&watchState{SiteID: site.SiteID,
			UpdatedAt: now, Channels: sigs}); serr != nil {
			return nil, false, total, ierr.Wrap(ierr.CodeInternal, "快照写入失败", serr)
		}
		return []Change{}, true, total, nil
	}

	changes = diffSnapshots(site.SiteID, prev.Channels, sigs, chs, opts, truncated)
	if serr := store.save(&watchState{SiteID: site.SiteID,
		UpdatedAt: now, Channels: sigs}); serr != nil {
		return nil, false, total, ierr.Wrap(ierr.CodeInternal, "快照写入失败", serr)
	}
	return changes, false, total, nil
}

// diffSnapshots 对比新旧快照生成变化。truncated=true 时跳过 removed 检测
// （被截掉的渠道会被误判为"删除"，宁漏报不误报）。
func diffSnapshots(siteID string, old, new map[string]chanSig,
	current []records.Channel, opts NotifyOptions, truncated bool) []Change {
	now := time.Now().Unix()
	changes := make([]Change, 0)

	add := func(t string, id int64, name string, detail map[string]any) {
		changes = append(changes, Change{Type: t, SiteID: siteID,
			ChannelID: id, Name: name, Detail: detail, DetectedAt: now})
	}

	// 新增 / 变化（以新快照为准遍历）
	for idStr, ns := range new {
		id, _ := strconv.ParseInt(idStr, 10, 64)
		oldSig, existed := old[idStr]
		if !existed {
			if opts.OnAddRemove {
				add("channel_added", id, ns.Name,
					map[string]any{"status": ns.Status})
			}
			continue
		}
		if opts.OnStatus && oldSig.Status != ns.Status {
			add("status_changed", id, ns.Name, map[string]any{
				"old_status": oldSig.Status, "new_status": ns.Status})
		}
		// 模型哈希双向比较（空↔非空也是真实变化）
		if opts.OnModels && oldSig.ModelsHash != ns.ModelsHash {
			add("models_changed", id, ns.Name, map[string]any{
				"old_models_hash": oldSig.ModelsHash, "new_models_hash": ns.ModelsHash})
		}
		if opts.OnRT && opts.RTAboveMs > 0 && ns.ResponseTime != nil &&
			*ns.ResponseTime > opts.RTAboveMs &&
			(oldSig.ResponseTime == nil || *oldSig.ResponseTime <= opts.RTAboveMs) {
			add("response_time_spike", id, ns.Name, map[string]any{
				"threshold_ms":  opts.RTAboveMs,
				"response_time": *ns.ResponseTime,
				"previous":      oldSig.ResponseTime,
			})
		}
		if opts.OnBalance && opts.BalanceBelowUSD > 0 && ns.Balance != nil &&
			*ns.Balance < opts.BalanceBelowUSD &&
			(oldSig.Balance == nil || *oldSig.Balance >= opts.BalanceBelowUSD) {
			add("balance_below", id, ns.Name, map[string]any{
				"threshold_usd": opts.BalanceBelowUSD,
				"balance":       *ns.Balance,
				"previous":      oldSig.Balance,
			})
		}
	}
	// 删除（旧有新无）。列表截断时跳过：被截掉 ≠ 被删，宁漏报不误报
	for idStr, oldSig := range old {
		if _, exists := new[idStr]; !exists {
			if opts.OnAddRemove && !truncated {
				id, _ := strconv.ParseInt(idStr, 10, 64)
				add("channel_removed", id, oldSig.Name,
					map[string]any{"old_status": oldSig.Status})
			}
		}
	}
	// 稳定输出顺序
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Type != changes[j].Type {
			return changes[i].Type < changes[j].Type
		}
		return changes[i].ChannelID < changes[j].ChannelID
	})
	return changes
}

// Watch 周期性 Check：intervalS 间隔轮询，durationS=0 时直到 Ctrl+C。
// durationS>0 时严格遵守时间预算（下一轮等待 = min(interval, 距 deadline)）。
// 每轮检测到的变化写入通知 JSONL（var/logs/channel-watch/<site>.jsonl）
// 并累计返回；每轮日志走 stderr。
func Watch(site *config.SiteConfig, mgr Manager, store *WatchStore,
	intervalS int, durationS int, opts NotifyOptions,
	log *logging.Logger) ([]Change, int, error) {
	if intervalS <= 0 {
		intervalS = 300
	}
	notifDir := filepath.Join(filepath.Dir(store.dir), "..", "logs", "channel-watch")
	_ = os.MkdirAll(notifDir, 0o755)
	notifPath := filepath.Join(notifDir, site.SiteID+".jsonl")

	all := make([]Change, 0)
	ticks := 0

	ctx := context.Background()
	if durationS <= 0 {
		// Ctrl+C 退出
		var cancel context.CancelFunc
		ctx, cancel = signal.NotifyContext(ctx, os.Interrupt)
		defer cancel()
	}

	t := time.NewTimer(0)
	defer t.Stop()
	var deadline time.Time
	if durationS > 0 {
		deadline = time.Now().Add(time.Duration(durationS) * time.Second)
	}

	for {
		select {
		case <-ctx.Done():
			return all, ticks, nil
		case <-t.C:
			ticks++
			changes, baseline, total, err := Check(site, mgr, store, opts)
			if err != nil {
				log.Error("channel_watch.check_failed", map[string]any{
					"site_id": site.SiteID, "tick": ticks, "error": err.Error()})
			} else {
				log.Info("channel_watch.tick", map[string]any{
					"site_id": site.SiteID, "tick": ticks, "total": total,
					"baseline": baseline, "changes": len(changes)})
				for i := range changes {
					all = append(all, changes[i])
					if nerr := appendNotification(notifPath, &changes[i]); nerr != nil {
						log.Error("channel_watch.notify_write_failed", map[string]any{
							"site_id": site.SiteID, "error": nerr.Error()})
					}
				}
				if len(changes) > 0 {
					// 变化即时打到 stderr，供调用方（模型）实时感知
					log.Info("channel_changes", map[string]any{
						"site_id": site.SiteID, "changes": changes})
				}
			}
			// 下一轮等待：严格遵守 duration 预算（不溢出到下一个完整 interval）
			if !deadline.IsZero() {
				remain := time.Until(deadline)
				if remain <= 0 {
					return all, ticks, nil
				}
				wait := time.Duration(intervalS) * time.Second
				if wait > remain {
					wait = remain
				}
				t.Reset(wait)
			} else {
				t.Reset(time.Duration(intervalS) * time.Second)
			}
		}
	}
}

// appendNotification 追加一条变化通知；写失败如实上抛（由调用方记日志）。
func appendNotification(path string, c *Change) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}
