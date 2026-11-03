// channels 子命令组：渠道管理（管理员面，给运营模型用的接口）。
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"opscollector/internal/channels"
	"opscollector/internal/config"
	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"
	"opscollector/internal/logging"
	"opscollector/internal/records"
)

func cmdChannels(args []string) int {
	if len(args) == 0 {
		channelsUsage()
		return 64
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdChannelsList(rest)
	case "get":
		return cmdChannelsGet(rest)
	case "add":
		return cmdChannelsAdd(rest)
	case "update":
		return cmdChannelsUpdate(rest)
	case "delete":
		return cmdChannelsDelete(rest)
	case "set-status":
		return cmdChannelsSetStatus(rest)
	case "test":
		return cmdChannelsTest(rest)
	case "check":
		return cmdChannelsCheck(rest)
	case "watch":
		return cmdChannelsWatch(rest)
	case "--help", "-h", "help":
		channelsUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "未知 channels 子命令: %s\n", sub)
		channelsUsage()
		return 64
	}
}

// auditLog 把管理面写操作留痕到 var/logs/collector.jsonl（审计回放用）。
func auditLog(varDir, op, siteID string, fields map[string]any) {
	log := logging.New(filepath.Join(varDirOr(varDir), "logs"), "", false)
	defer log.Close()
	f := map[string]any{"op": op, "site_id": siteID}
	for k, v := range fields {
		f[k] = v
	}
	log.Info("channel.audit", f)
}

func channelsUsage() {
	fmt.Fprint(os.Stderr, `ops-collector channels — 渠道管理（管理员面，需站点配置 "admin": true）

子命令:
  channels list   --site X [--status enabled|disabled] [--page-size N]
  channels get    --site X --id N
  channels add    --site X --body-file F     （站点原生请求体 JSON；密钥走文件不进命令行）
  channels update --site X --body-file F     （body 需含 id）
  channels delete --site X --id N --yes      （--yes 确认删除）
  channels set-status --site X --id N --status enabled|disabled
  channels test   --site X --id N [--model M]
  channels check  --site X [--on status,add_remove,models,rt,balance]
                     [--rt-above-ms N] [--balance-below-usd N]
                     （一次性：与上次快照 diff，输出变化；首次运行建立基线；
                       有变化时退出码为 2，便于模型/定时任务程序化感知）
  channels watch  --site X --interval-s 300 [--duration-s 0] [同 check 的提醒参数]
                     （周期自动更新状态并提醒；duration-s=0 表示直到 Ctrl+C）
`)
}

// makeChannels 构造站点渠道管理器（含 admin 门控与超时配置）。
// 管理操作显式指定站点，disabled 站点也允许管理。
func makeChannels(cfg *config.Config, siteID string) (channels.Manager, error) {
	site := findSite(cfg, siteID)
	if site == nil {
		return nil, ierr.New(ierr.CodeSiteNotFound, "未找到站点: "+siteID)
	}
	client := httpc.New(site.BaseURL, httpc.Options{
		TimeoutS: pickT(site, cfg), Retries: cfg.Defaults.Retries,
		MinIntervalS: derefInterval(site)})
	return channels.Make(site, client)
}

// derefInterval 站点级限速覆盖（nil = 0，管理操作频率低）。
func derefInterval(site *config.SiteConfig) float64 {
	if site.MinIntervalS != nil {
		return *site.MinIntervalS
	}
	return 0
}

func mustSite(cfgPath string) (*config.Config, error) {
	return config.Load(cfgPath)
}

// loggingForWatch watch 循环的诊断日志（stderr + var/logs/collector.jsonl）。
func loggingForWatch(varDir string) *logging.Logger {
	return logging.New(filepath.Join(varDir, "logs"), "", true)
}

func cmdChannelsList(args []string) int {
	fs := flag.NewFlagSet("channels list", flag.ContinueOnError)
	cfgPath := fs.String("config", "config/collector.json", "")
	siteID := fs.String("site", "", "")
	status := fs.String("status", "", "enabled|disabled|空")
	pageSize := fs.Int("page-size", 100, "")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if *siteID == "" {
		return fail(ierr.Configf("需要 --site"))
	}
	cfg, err := mustSite(*cfgPath)
	if err != nil {
		return fail(err)
	}
	mgr, err := makeChannels(cfg, *siteID)
	if err != nil {
		return fail(err)
	}
	items, total, err := mgr.List(channels.ListOptions{
		PageSize: *pageSize, StatusFilter: *status})
	if err != nil {
		return fail(err)
	}
	emit(map[string]any{"ok": true, "site_id": *siteID,
		"total": total, "items": items})
	return 0
}

func cmdChannelsGet(args []string) int {
	fs := flag.NewFlagSet("channels get", flag.ContinueOnError)
	cfgPath := fs.String("config", "config/collector.json", "")
	siteID := fs.String("site", "", "")
	id := fs.Int64("id", 0, "")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if *siteID == "" || *id == 0 {
		return fail(ierr.Configf("需要 --site 与 --id"))
	}
	cfg, err := mustSite(*cfgPath)
	if err != nil {
		return fail(err)
	}
	mgr, err := makeChannels(cfg, *siteID)
	if err != nil {
		return fail(err)
	}
	ch, err := mgr.Get(*id)
	if err != nil {
		return fail(err)
	}
	emit(map[string]any{"ok": true, "site_id": *siteID, "channel": ch})
	return 0
}

// readBodyFile 从 JSON 文件读渠道请求体（密钥不进命令行/日志）。
func readBodyFile(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, ierr.Wrap(ierr.CodeConfigInvalid, "读取 body 文件失败", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, ierr.Wrap(ierr.CodeConfigInvalid, "body 文件不是 JSON 对象", err)
	}
	return m, nil
}

func cmdChannelsAdd(args []string) int {
	fs := flag.NewFlagSet("channels add", flag.ContinueOnError)
	cfgPath := fs.String("config", "config/collector.json", "")
	siteID := fs.String("site", "", "")
	bodyFile := fs.String("body-file", "", "渠道请求体 JSON 文件")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if *siteID == "" || *bodyFile == "" {
		return fail(ierr.Configf("需要 --site 与 --body-file"))
	}
	cfg, err := mustSite(*cfgPath)
	if err != nil {
		return fail(err)
	}
	mgr, err := makeChannels(cfg, *siteID)
	if err != nil {
		return fail(err)
	}
	body, err := readBodyFile(*bodyFile)
	if err != nil {
		return fail(err)
	}
	ch, err := mgr.Add(body)
	if err != nil {
		return fail(err)
	}
	addedID := int64(0)
	if ch != nil {
		addedID = ch.ChannelID
	}
	auditLog("var", "channel.add", *siteID,
		map[string]any{"channel_id": addedID, "name": body["name"]})
	emit(map[string]any{"ok": true, "site_id": *siteID, "channel": ch})
	return 0
}

func cmdChannelsUpdate(args []string) int {
	fs := flag.NewFlagSet("channels update", flag.ContinueOnError)
	cfgPath := fs.String("config", "config/collector.json", "")
	siteID := fs.String("site", "", "")
	bodyFile := fs.String("body-file", "", "")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if *siteID == "" || *bodyFile == "" {
		return fail(ierr.Configf("需要 --site 与 --body-file"))
	}
	cfg, err := mustSite(*cfgPath)
	if err != nil {
		return fail(err)
	}
	mgr, err := makeChannels(cfg, *siteID)
	if err != nil {
		return fail(err)
	}
	body, err := readBodyFile(*bodyFile)
	if err != nil {
		return fail(err)
	}
	ch, err := mgr.Update(body)
	if err != nil {
		return fail(err)
	}
	updatedID := int64(0)
	if ch != nil {
		updatedID = ch.ChannelID
	}
	auditLog("var", "channel.update", *siteID,
		map[string]any{"channel_id": updatedID, "fields": bodyFields(body)})
	emit(map[string]any{"ok": true, "site_id": *siteID, "channel": ch})
	return 0
}

func cmdChannelsDelete(args []string) int {
	fs := flag.NewFlagSet("channels delete", flag.ContinueOnError)
	cfgPath := fs.String("config", "config/collector.json", "")
	siteID := fs.String("site", "", "")
	id := fs.Int64("id", 0, "")
	yes := fs.Bool("yes", false, "确认删除")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if *siteID == "" || *id == 0 {
		return fail(ierr.Configf("需要 --site 与 --id"))
	}
	if !*yes {
		// 删除是破坏性操作：必须显式 --yes
		return fail(ierr.New("CONFIRM_REQUIRED",
			fmt.Sprintf("删除渠道 %s/%d 是破坏性操作：确认请追加 --yes", *siteID, *id)))
	}
	cfg, err := mustSite(*cfgPath)
	if err != nil {
		return fail(err)
	}
	mgr, err := makeChannels(cfg, *siteID)
	if err != nil {
		return fail(err)
	}
	if err := mgr.Delete(*id); err != nil {
		return fail(err)
	}
	auditLog("var", "channel.delete", *siteID, map[string]any{"channel_id": *id})
	emit(map[string]any{"ok": true, "site_id": *siteID,
		"deleted": *id})
	return 0
}

func cmdChannelsSetStatus(args []string) int {
	fs := flag.NewFlagSet("channels set-status", flag.ContinueOnError)
	cfgPath := fs.String("config", "config/collector.json", "")
	siteID := fs.String("site", "", "")
	id := fs.Int64("id", 0, "")
	status := fs.String("status", "", "enabled|disabled")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if *siteID == "" || *id == 0 || *status == "" {
		return fail(ierr.Configf("需要 --site --id --status enabled|disabled"))
	}
	cfg, err := mustSite(*cfgPath)
	if err != nil {
		return fail(err)
	}
	mgr, err := makeChannels(cfg, *siteID)
	if err != nil {
		return fail(err)
	}
	var st records.ChannelStatus
	switch strings.ToLower(*status) {
	case "enabled", "active":
		st = records.ChannelEnabled
	case "disabled":
		st = records.ChannelManualDisabled
	default:
		return fail(ierr.Configf("status 仅支持 enabled|disabled，得到 %s", *status))
	}
	if err := mgr.SetStatus(*id, st); err != nil {
		return fail(err)
	}
	auditLog("var", "channel.set_status", *siteID,
		map[string]any{"channel_id": *id, "status": string(st)})
	emit(map[string]any{"ok": true, "site_id": *siteID, "id": *id,
		"status": string(st)})
	return 0
}

func cmdChannelsTest(args []string) int {
	fs := flag.NewFlagSet("channels test", flag.ContinueOnError)
	cfgPath := fs.String("config", "config/collector.json", "")
	siteID := fs.String("site", "", "")
	id := fs.Int64("id", 0, "")
	model := fs.String("model", "", "")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if *siteID == "" || *id == 0 {
		return fail(ierr.Configf("需要 --site 与 --id"))
	}
	cfg, err := mustSite(*cfgPath)
	if err != nil {
		return fail(err)
	}
	mgr, err := makeChannels(cfg, *siteID)
	if err != nil {
		return fail(err)
	}
	result, err := mgr.Test(*id, *model)
	if err != nil {
		return fail(err)
	}
	emit(map[string]any{"ok": true, "site_id": *siteID, "id": *id,
		"result": result})
	return 0
}

// parseNotifyFlags check/watch 共用的提醒规则参数。
// 返回 opts 与 parse 后必须调用的 apply 闭包。
func parseNotifyFlags(fs *flag.FlagSet) (*channels.NotifyOptions, func()) {
	opts := channels.DefaultNotifyOptions()
	var on string
	fs.StringVar(&on, "on", "status,add_remove",
		"提醒项: status,add_remove,models,rt,balance 组合")
	fs.Int64Var(&opts.RTAboveMs, "rt-above-ms", 0, "响应时间超过该毫秒数时提醒")
	fs.Float64Var(&opts.BalanceBelowUSD, "balance-below-usd", 0, "余额低于该美元数时提醒")
	apply := func() {
		if on == "" {
			return
		}
		want := map[string]bool{}
		for _, part := range strings.Split(on, ",") {
			want[strings.TrimSpace(part)] = true
		}
		opts.OnStatus = want["status"]
		opts.OnAddRemove = want["add_remove"]
		opts.OnModels = want["models"]
		opts.OnRT = want["rt"]
		opts.OnBalance = want["balance"]
		if opts.OnRT && opts.RTAboveMs <= 0 {
			opts.RTAboveMs = 5000
		}
		if opts.OnBalance && opts.BalanceBelowUSD <= 0 {
			opts.BalanceBelowUSD = 10
		}
	}
	return &opts, apply
}

func cmdChannelsCheck(args []string) int {
	fs := flag.NewFlagSet("channels check", flag.ContinueOnError)
	cfgPath := fs.String("config", "config/collector.json", "")
	siteID := fs.String("site", "", "")
	varDir := fs.String("var-dir", "", "")
	opts, applyNotify := parseNotifyFlags(fs)
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	applyNotify()
	if *siteID == "" {
		return fail(ierr.Configf("需要 --site"))
	}
	cfg, err := mustSite(*cfgPath)
	if err != nil {
		return fail(err)
	}
	site := findSite(cfg, *siteID)
	mgr, err := makeChannels(cfg, *siteID)
	if err != nil {
		return fail(err)
	}
	store := channels.NewWatchStore(varDirOr(*varDir))
	changes, baseline, total, err := channels.Check(site, mgr, store, *opts)
	if err != nil {
		return fail(err)
	}
	emit(map[string]any{"ok": true, "site_id": *siteID,
		"baseline": baseline, "total": total, "changes": changes})
	if len(changes) > 0 {
		// 有变化时退出码 2，方便调用方（模型/定时任务）程序化感知
		return 2
	}
	return 0
}

func cmdChannelsWatch(args []string) int {
	fs := flag.NewFlagSet("channels watch", flag.ContinueOnError)
	cfgPath := fs.String("config", "config/collector.json", "")
	siteID := fs.String("site", "", "")
	intervalS := fs.Int("interval-s", 300, "状态自动更新间隔（秒）")
	durationS := fs.Int("duration-s", 0, "运行时长（秒），0=直到 Ctrl+C")
	varDir := fs.String("var-dir", "", "")
	opts, applyNotify := parseNotifyFlags(fs)
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	applyNotify()
	if *siteID == "" {
		return fail(ierr.Configf("需要 --site"))
	}
	cfg, err := mustSite(*cfgPath)
	if err != nil {
		return fail(err)
	}
	site := findSite(cfg, *siteID)
	mgr, err := makeChannels(cfg, *siteID)
	if err != nil {
		return fail(err)
	}
	store := channels.NewWatchStore(varDirOr(*varDir))
	log := loggingForWatch(varDirOr(*varDir))
	defer log.Close()
	changes, ticks, err := channels.Watch(site, mgr, store,
		*intervalS, *durationS, *opts, log)
	if err != nil {
		return fail(err)
	}
	emit(map[string]any{"ok": true, "site_id": *siteID,
		"ticks": ticks, "change_count": len(changes), "changes": changes})
	return 0
}

// bodyFields 列出更新请求体涉及的字段名（审计只记键名，不记值，防密钥入日志）。
func bodyFields(body map[string]any) []string {
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	return keys
}

func findSite(cfg *config.Config, siteID string) *config.SiteConfig {
	for _, s := range cfg.Sites {
		if s.SiteID == siteID {
			return s
		}
	}
	return nil
}

func varDirOr(v string) string {
	if v != "" {
		return v
	}
	return "var"
}
