// Package cli: 命令行入口。stdout 只输出机器可解析 JSON，诊断走 stderr/日志文件。
//
// 命令：
//
//	ops-collector init                     生成示例配置
//	ops-collector validate-config          校验配置
//	ops-collector secrets set/delete/hint  凭据管理
//	ops-collector add-site                 添加站点（含凭据引用）
//	ops-collector login-test               只测登录与站点元信息
//	ops-collector collect                  采集（全量/指定站点）
//	ops-collector export                   汇总一次 run 的指标
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"

	"opscollector/internal/adapters"
	"opscollector/internal/config"
	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"
	"opscollector/internal/run"
	"opscollector/internal/secrets"
)

const Version = "0.1.0"

// emit 输出 JSON 到 stdout（不转义 HTML，UTF-8）。
func emit(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func fail(err error) int {
	code := ierr.CodeOf(err)
	emit(map[string]any{"ok": false, "error_code": code, "message": err.Error()})
	return 1
}

// Run 执行命令行，返回进程退出码。
func Run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "init":
		return cmdInit(args[1:])
	case "validate-config":
		return cmdValidate(args[1:])
	case "secrets":
		return cmdSecrets(args[1:])
	case "add-site":
		return cmdAddSite(args[1:])
	case "login-test":
		return cmdLoginTest(args[1:])
	case "collect":
		return cmdCollect(args[1:])
	case "export":
		return cmdExport(args[1:])
	case "prices":
		return cmdPrices(args[1:])
	case "channels":
		return cmdChannels(args[1:])
	case "sites":
		return cmdSites(args[1:])
	case "--version", "version":
		fmt.Printf("ops-collector %s\n", Version)
		return 0
	case "--help", "-h", "help":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n", args[0])
		usage()
		return 64
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `ops-collector — new-api/sub2api 多站点运营数据采集器

命令:
  init [--config P] [--force]            生成示例配置
  validate-config [--config P]           校验配置
  secrets set|delete|hint [entry]        凭据管理（条目如 siteX:password）
  add-site --site-id ID --kind K --base-url URL --user U [--mode password|api_key]
  sites [--config P]                     站点清单（仅 ID，不含网址）
  login-test [--config P] [--site ID]    测试登录与站点元信息
  collect [--config P] [--site ID]... [--no-incremental]
  export <run_dir>                       汇总 run 的性能指标为扁平表
  prices <run_dir>                       汇总 run 的模型价格表（含缓存价）
  channels <sub>                         渠道管理（管理员面，详见 channels 子命令帮助）

退出码: 0=成功/无变化  1=失败  2=check 有变化  64=用法错误
`)
}

// ---- init ----

func exampleConfig() *config.Config {
	enabled := true
	return &config.Config{
		SchemaVersion: 1,
		Sites: []*config.SiteConfig{
			{
				SiteID: "my-newapi", Name: "我的 new-api 站点", Kind: "new_api",
				BaseURL: "https://example-newapi.example.com",
				Auth: config.AuthConfig{Mode: "password", Username: "your-username",
					SecretRef: "my-newapi:password"},
				Enabled: &enabled,
				Notes:   "若站点开启 Turnstile/2FA，请改用 api_key 模式（安全设置里的访问令牌）",
			},
			{
				SiteID: "my-sub2api", Name: "我的 sub2api 站点", Kind: "sub2api",
				BaseURL: "https://example-sub2api.example.com",
				Auth: config.AuthConfig{Mode: "password", Email: "you@example.com",
					SecretRef: "my-sub2api:password"},
				Enabled: &enabled,
			},
		},
	}
}

func cmdInit(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	configPath := fs.String("config", "config/collector.json", "配置文件路径")
	force := fs.Bool("force", false, "覆盖已存在的配置")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if _, err := os.Stat(*configPath); err == nil && !*force {
		return fail(ierr.New(ierr.CodeConfigExists,
			fmt.Sprintf("配置已存在: %s（--force 覆盖）", *configPath)))
	}
	if err := exampleConfig().Save(*configPath); err != nil {
		return fail(err)
	}
	emit(map[string]any{"ok": true, "config": *configPath,
		"next": "编辑配置后运行 validate-config；用 `secrets set` 写入密码"})
	return 0
}

// ---- validate-config ----

func cmdValidate(args []string) int {
	fs := flag.NewFlagSet("validate-config", flag.ContinueOnError)
	configPath := fs.String("config", "config/collector.json", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail(err)
	}
	enabled := []string{}
	for _, s := range cfg.EnabledSites() {
		enabled = append(enabled, s.SiteID)
	}
	emit(map[string]any{"ok": true, "sites_total": len(cfg.Sites),
		"sites_enabled": enabled})
	return 0
}

// ---- secrets ----

func cmdSecrets(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "set":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "用法: secrets set <entry>")
			return 2
		}
		value, err := readPassword(fmt.Sprintf("输入 %s 的值（不回显）", args[1]))
		if err != nil {
			return fail(err)
		}
		if value == "" {
			return fail(ierr.New(ierr.CodeConfigInvalid, "空值不写入"))
		}
		if err := secrets.SetSecret(args[1], value); err != nil {
			return fail(err)
		}
		emit(map[string]any{"ok": true, "entry": args[1]})
		return 0
	case "delete":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "用法: secrets delete <entry>")
			return 2
		}
		ok := secrets.DeleteSecret(args[1])
		emit(map[string]any{"ok": ok, "entry": args[1]})
		if !ok {
			return 1
		}
		return 0
	case "hint":
		emit(map[string]any{"ok": true, "env_entries": secrets.ListEntriesHint(),
			"note": "keyring 条目无法枚举；凭据管理器中查看服务名 ops-collector"})
		return 0
	default:
		fmt.Fprintf(os.Stderr, "未知 secrets 子命令: %s\n", args[0])
		return 2
	}
}

// ---- add-site ----

func cmdAddSite(args []string) int {
	fs := flag.NewFlagSet("add-site", flag.ContinueOnError)
	configPath := fs.String("config", "config/collector.json", "配置文件路径")
	siteID := fs.String("site-id", "", "站点 ID")
	kind := fs.String("kind", "", "new_api | sub2api")
	baseURL := fs.String("base-url", "", "站点基址")
	user := fs.String("user", "", "new-api 用户名 / sub2api 邮箱")
	mode := fs.String("mode", "password", "password | api_key")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if *siteID == "" || *kind == "" || *baseURL == "" || *user == "" {
		return fail(ierr.Configf("add-site 需要 --site-id --kind --base-url --user"))
	}
	var cfg *config.Config
	if c, err := config.Load(*configPath); err == nil {
		cfg = c
	} else if ierr.CodeOf(err) == ierr.CodeConfigNotFound {
		cfg = &config.Config{SchemaVersion: 1}
	} else {
		return fail(err)
	}
	authCfg := config.AuthConfig{Mode: *mode}
	if *mode == "password" {
		authCfg.SecretRef = *siteID + ":password"
		if *kind == "new_api" {
			authCfg.Username = *user
		} else {
			authCfg.Email = *user
		}
	} else {
		authCfg.APIKeySecretRef = *siteID + ":pat"
	}
	kept := cfg.Sites[:0]
	for _, s := range cfg.Sites {
		if s.SiteID != *siteID {
			kept = append(kept, s)
		}
	}
	cfg.Sites = append(kept, &config.SiteConfig{
		SiteID: *siteID, Kind: *kind, BaseURL: *baseURL,
		Auth: authCfg, // Enabled 留空 = 启用
	})
	if err := cfg.Save(*configPath); err != nil {
		return fail(err)
	}
	nextEntry := *siteID + ":password"
	if *mode == "api_key" {
		nextEntry = *siteID + ":pat"
	}
	emit(map[string]any{"ok": true, "site_id": *siteID, "config": *configPath,
		"next": "运行 `secrets set " + nextEntry + "` 写入凭据，然后 login-test"})
	return 0
}

// ---- login-test ----

type loginResult struct {
	OK      bool           `json:"ok"`
	SiteID  string         `json:"site_id"`
	Kind    string         `json:"kind,omitempty"`
	Meta    map[string]any `json:"meta,omitempty"`
	ErrCode string         `json:"error_code,omitempty"`
	Message string         `json:"message,omitempty"`
}

func cmdLoginTest(args []string) int {
	fs := flag.NewFlagSet("login-test", flag.ContinueOnError)
	configPath := fs.String("config", "config/collector.json", "配置文件路径")
	siteID := fs.String("site", "", "只测指定站点")
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail(err)
	}
	var sites []*config.SiteConfig
	for _, s := range cfg.EnabledSites() {
		if *siteID == "" || s.SiteID == *siteID {
			sites = append(sites, s)
		}
	}
	if len(sites) == 0 && *siteID != "" {
		// 区分"不存在"与"已禁用"（agent 行动性）
		for _, s := range cfg.Sites {
			if s.SiteID == *siteID {
				return fail(ierr.New(ierr.CodeSiteDisabled,
					"站点已禁用（enabled=false）: "+*siteID))
			}
		}
		return fail(ierr.New(ierr.CodeSiteNotFound, "未找到站点: "+*siteID))
	}
	results := []loginResult{}
	failed := false
	for _, site := range sites {
		r := loginResult{SiteID: site.SiteID, Kind: site.Kind}
		client := httpc.New(site.BaseURL, httpc.Options{
			TimeoutS: pickT(site, cfg), Retries: 1, MinIntervalS: 0.2})
		adapter, err := adapters.Make(site, client)
		if err == nil {
			r.Meta, err = adapter.SiteMeta()
		}
		if err != nil {
			r.OK = false
			r.ErrCode = ierr.CodeOf(err)
			r.Message = err.Error()
			failed = true
		} else {
			r.OK = true
		}
		results = append(results, r)
	}
	emit(map[string]any{"ok": !failed, "results": results})
	if failed {
		return 1
	}
	return 0
}

func pickT(site *config.SiteConfig, cfg *config.Config) float64 {
	if site.TimeoutS > 0 {
		return site.TimeoutS
	}
	return cfg.Defaults.TimeoutS
}

// ---- collect ----

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func cmdCollect(args []string) int {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	configPath := fs.String("config", "config/collector.json", "配置文件路径")
	var sites stringList
	fs.Var(&sites, "site", "指定 site_id（可多次）")
	noIncremental := fs.Bool("no-incremental", false, "忽略断点，从零采集")
	varDir := fs.String("var-dir", "", "运行产物目录（默认 ./var）")
	if err := fs.Parse(args); err != nil {
		return fail(ierr.New(ierr.CodeUsage, "参数错误: "+err.Error()))
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail(err)
	}
	// Ctrl+C 贯穿到 HTTP 层（ctx 取消所有 in-flight 请求与等待）
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	summary, err := run.Collect(cfg, run.Options{
		VarDir: *varDir, SiteIDs: sites, Incremental: !*noIncremental, Ctx: ctx,
	})
	if err != nil {
		return fail(err)
	}
	emit(summary)
	if summary.Status == "failed" {
		return 1
	}
	return 0
}

// ---- export ----

func cmdExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if fs.NArg() < 1 {
		return fail(ierr.Configf("export 需要 run_dir 参数"))
	}
	runDir := fs.Arg(0)
	files, err := filepath.Glob(filepath.Join(runDir, "site-*.json"))
	if err != nil {
		return fail(ierr.Wrap(ierr.CodeInternal, "扫描 run 目录失败", err))
	}
	type row struct {
		SiteID       string         `json:"site_id"`
		Model        string         `json:"model"`
		Group        *string        `json:"group"`
		WindowHours  int            `json:"window_hours"`
		SuccessRate  any            `json:"success_rate"`
		AvgTTFTMs    any            `json:"avg_ttft_ms"`
		AvgOutputTPS any            `json:"avg_output_tps"`
		Provenance   map[string]any `json:"provenance"`
	}
	var rows []row
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var data struct {
			SiteID      string `json:"site_id"`
			Performance []struct {
				Model       string  `json:"model"`
				Group       *string `json:"group"`
				WindowHours int     `json:"window_hours"`
				SuccessRate struct {
					Value      any    `json:"value"`
					Provenance string `json:"provenance"`
				} `json:"success_rate"`
				AvgTTFTMs struct {
					Value      any    `json:"value"`
					Provenance string `json:"provenance"`
				} `json:"avg_ttft_ms"`
				AvgOutputTPS struct {
					Value      any    `json:"value"`
					Provenance string `json:"provenance"`
				} `json:"avg_output_tps"`
			} `json:"performance"`
		}
		if json.Unmarshal(b, &data) != nil {
			continue
		}
		for _, p := range data.Performance {
			rows = append(rows, row{
				SiteID: data.SiteID, Model: p.Model, Group: p.Group,
				WindowHours:  p.WindowHours,
				SuccessRate:  p.SuccessRate.Value,
				AvgTTFTMs:    p.AvgTTFTMs.Value,
				AvgOutputTPS: p.AvgOutputTPS.Value,
				Provenance: map[string]any{
					"success_rate": p.SuccessRate.Provenance,
					"ttft":         p.AvgTTFTMs.Provenance,
					"tps":          p.AvgOutputTPS.Provenance,
				},
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].SiteID != rows[j].SiteID {
			return rows[i].SiteID < rows[j].SiteID
		}
		if rows[i].WindowHours != rows[j].WindowHours {
			return rows[i].WindowHours < rows[j].WindowHours
		}
		return rows[i].Model < rows[j].Model
	})
	emit(map[string]any{"ok": true, "run_dir": runDir, "rows": rows})
	return 0
}

// ---- prices ----

func cmdPrices(args []string) int {
	fs := flag.NewFlagSet("prices", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return fail(err)
	}
	if fs.NArg() < 1 {
		return fail(ierr.Configf("prices 需要 run_dir 参数"))
	}
	runDir := fs.Arg(0)
	files, err := filepath.Glob(filepath.Join(runDir, "site-*.json"))
	if err != nil {
		return fail(ierr.Wrap(ierr.CodeInternal, "扫描 run 目录失败", err))
	}
	type priceRow struct {
		SiteID          string   `json:"site_id"`
		Model           string   `json:"model"`
		Group           string   `json:"group"`
		BillingType     string   `json:"billing_type"`
		InputPerM       *float64 `json:"input_per_m_usd"`
		OutputPerM      *float64 `json:"output_per_m_usd"`
		CacheReadPerM   *float64 `json:"cache_read_per_m_usd"`
		CacheCreatePerM *float64 `json:"cache_create_per_m_usd"`
		PerCall         *float64 `json:"per_call_usd"`
		GroupRatio      float64  `json:"group_ratio"`
		Provenance      string   `json:"provenance"`
	}
	var rows []priceRow
	presets := map[string]any{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var data struct {
			SiteID      string `json:"site_id"`
			ModelPrices []struct {
				Model           string   `json:"model"`
				Group           string   `json:"group"`
				BillingType     string   `json:"billing_type"`
				InputPerM       *float64 `json:"input_per_m_usd"`
				OutputPerM      *float64 `json:"output_per_m_usd"`
				CacheReadPerM   *float64 `json:"cache_read_per_m_usd,omitempty"`
				CacheCreatePerM *float64 `json:"cache_create_per_m_usd,omitempty"`
				PerCall         *float64 `json:"per_call_usd,omitempty"`
				GroupRatio      float64  `json:"group_ratio"`
				Provenance      string   `json:"provenance"`
			} `json:"model_prices"`
			PresetRatioConfig map[string]any `json:"preset_ratio_config"`
		}
		if json.Unmarshal(b, &data) != nil {
			continue
		}
		for _, p := range data.ModelPrices {
			rows = append(rows, priceRow{
				SiteID: data.SiteID, Model: p.Model, Group: p.Group,
				BillingType: p.BillingType,
				InputPerM:   p.InputPerM, OutputPerM: p.OutputPerM,
				CacheReadPerM: p.CacheReadPerM, CacheCreatePerM: p.CacheCreatePerM,
				PerCall: p.PerCall, GroupRatio: p.GroupRatio,
				Provenance: p.Provenance,
			})
		}
		if data.PresetRatioConfig != nil {
			presets[data.SiteID] = data.PresetRatioConfig
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].SiteID != rows[j].SiteID {
			return rows[i].SiteID < rows[j].SiteID
		}
		if rows[i].Model != rows[j].Model {
			return rows[i].Model < rows[j].Model
		}
		return rows[i].Group < rows[j].Group
	})
	emit(map[string]any{"ok": true, "run_dir": runDir, "rows": rows,
		"preset_ratio_config": presets})
	return 0
}

// readPassword 从终端读取密码（隐藏回显）。提示与换行走 stderr，
// 保证 pty 场景 stdout 仍是纯 JSON。
func readPassword(prompt string) (string, error) {
	if !termIsTerminal() {
		return "", ierr.New(ierr.CodeSecret,
			"stdin 不是终端，无法交互输入；请使用环境变量或凭据管理器")
	}
	fmt.Fprintf(os.Stderr, "%s（输入不会回显）: ", prompt)
	b, err := termReadPassword()
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", ierr.Wrap(ierr.CodeSecret, "读取密码失败", err)
	}
	return string(b), nil
}
