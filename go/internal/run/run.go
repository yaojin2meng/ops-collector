// Package run: 多站点编排。并发隔离采集、每站点独立失败处理、
// artifact 与 run summary 落盘。
package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"opscollector/internal/adapters"
	"opscollector/internal/align"
	"opscollector/internal/channels"
	"opscollector/internal/checkpoint"
	"opscollector/internal/config"
	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"
	"opscollector/internal/logging"
	"opscollector/internal/records"
)

const SchemaVersion = 1

type SiteResult struct {
	SiteID       string         `json:"site_id"`
	SiteKind     string         `json:"site_kind"`
	Status       string         `json:"status"` // success | partial | failed | skipped
	ErrorCode    string         `json:"error_code,omitempty"`
	ErrorMessage string         `json:"error_message,omitempty"`
	Counts       map[string]int `json:"counts,omitempty"`
	Artifact     string         `json:"artifact,omitempty"`
	DurationS    float64        `json:"duration_s"`
}

type RunSummary struct {
	SchemaVersion int          `json:"schema_version"`
	RunID         string       `json:"run_id"`
	StartedAt     int64        `json:"started_at"`
	FinishedAt    int64        `json:"finished_at"`
	Status        string       `json:"status"` // success | partial | failed
	Sites         []SiteResult `json:"sites"`
	// 指定 --site 时如实回报：不存在的与被禁用的（agent 行动性）
	MissingSiteIDs  []string `json:"missing_site_ids,omitempty"`
	DisabledSiteIDs []string `json:"disabled_site_ids,omitempty"`
	OutputDir       string   `json:"output_dir"`
}

// DefaultVarDir 默认相对当前工作目录的 var/。
func DefaultVarDir() string { return "var" }

// Options 控制一次采集。
type Options struct {
	VarDir       string
	SiteIDs      []string // nil/空 = 全部启用站点
	Incremental  bool
	WindowsHours []int // nil = 用配置默认
	// Ctx 采集全程的取消通道（collect 命令接入 Ctrl+C）
	Ctx context.Context
}

// maxModelsPerf 性能派生时的模型采样上限（与并发数解耦，防魔法数）。
const maxModelsPerf = 20

// Collect 执行一次多站点采集。
func Collect(cfg *config.Config, opts Options) (*RunSummary, error) {
	// 防御：直接构造的 Config 可能未经 Load/Validate，补一次默认值
	cfg.Defaults.Normalize()
	varDir := opts.VarDir
	if varDir == "" {
		varDir = DefaultVarDir()
	}
	runID := newRunID()
	outDir := filepath.Join(varDir, "runs", runID)
	log := logging.New(filepath.Join(varDir, "logs"), runID, true)
	defer log.Close()

	sites := cfg.EnabledSites()
	var missingSiteIDs, disabledSiteIDs []string
	if len(opts.SiteIDs) > 0 {
		want := map[string]bool{}
		for _, id := range opts.SiteIDs {
			want[id] = true
		}
		filtered := sites[:0]
		for _, s := range sites {
			if want[s.SiteID] {
				filtered = append(filtered, s)
			}
		}
		sites = filtered
		have := map[string]bool{}
		for _, s := range sites {
			have[s.SiteID] = true
		}
		allByID := map[string]*config.SiteConfig{}
		for _, s := range cfg.Sites {
			allByID[s.SiteID] = s
		}
		for id := range want {
			if !have[id] {
				if s, ok := allByID[id]; ok && !s.IsEnabled() {
					disabledSiteIDs = append(disabledSiteIDs, id)
				} else {
					missingSiteIDs = append(missingSiteIDs, id)
				}
			}
		}
		if len(missingSiteIDs) > 0 {
			log.Warn("sites.not_found", map[string]any{"run_id": runID, "site_ids": missingSiteIDs})
		}
		if len(disabledSiteIDs) > 0 {
			log.Warn("sites.disabled", map[string]any{"run_id": runID, "site_ids": disabledSiteIDs})
		}
	}

	summary := &RunSummary{
		SchemaVersion: SchemaVersion,
		RunID:         runID,
		StartedAt:     time.Now().Unix(),
		OutputDir:     outDir,
		Status:        "running",
		Sites:         make([]SiteResult, 0, len(sites)),
	}

	concurrency := cfg.Defaults.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	results := make([]SiteResult, len(sites))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, site := range sites {
		wg.Add(1)
		go func(idx int, s *config.SiteConfig) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = collectSite(s, cfg, runID, outDir,
				filepath.Join(varDir, "state"), log, opts)
		}(i, site)
	}
	wg.Wait()

	summary.Sites = results
	summary.MissingSiteIDs = missingSiteIDs
	summary.DisabledSiteIDs = disabledSiteIDs
	successCount, anyOK := 0, false
	for _, r := range results {
		switch r.Status {
		case "success":
			successCount++
			anyOK = true
		case "partial":
			anyOK = true
		}
	}
	switch {
	case len(results) == 0:
		summary.Status = "failed"
	case successCount == len(results):
		summary.Status = "success"
	case anyOK:
		summary.Status = "partial"
	default:
		summary.Status = "failed"
	}
	summary.FinishedAt = time.Now().Unix()

	if err := os.MkdirAll(outDir, 0o755); err == nil {
		if b, err := json.MarshalIndent(summary, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(outDir, "run.json"), b, 0o644)
		}
	}
	log.Info("run.done", map[string]any{"run_id": runID, "status": summary.Status})
	return summary, nil
}

func collectSite(site *config.SiteConfig, cfg *config.Config, runID, outDir,
	stateDir string, log *logging.Logger, opts Options) (result SiteResult) {
	result = SiteResult{SiteID: site.SiteID, SiteKind: site.Kind,
		Status: "pending", Counts: map[string]int{}}
	t0 := time.Now()
	defer func() { result.DurationS = time.Since(t0).Seconds() }()
	// panic 隔离：单站点的未知 panic 不允许炸掉整个采集进程
	defer func() {
		if r := recover(); r != nil {
			result.Status = "failed"
			result.ErrorCode = ierr.CodePanic
			result.ErrorMessage = truncate(fmt.Sprintf("panic: %v", r), 300)
			log.Error("site.panic", map[string]any{
				"run_id": runID, "site_id": site.SiteID,
				"panic": fmt.Sprintf("%v", r)})
		}
	}()

	windows := opts.WindowsHours
	if len(windows) == 0 {
		windows = cfg.Defaults.WindowsHours
	}
	try := func() error {
		client := httpc.New(site.BaseURL, httpc.Options{
			TimeoutS:     pickTimeout(site, cfg),
			Retries:      cfg.Defaults.Retries,
			MinIntervalS: pickInterval(site, cfg),
			BackoffBaseS: cfg.Defaults.BackoffBaseS,
		})
		if opts.Ctx != nil {
			client.WithBaseContext(opts.Ctx)
		}
		adapter, err := adapters.Make(site, client)
		if err != nil {
			return err
		}
		log.Info("site.start", map[string]any{"run_id": runID,
			"site_id": site.SiteID, "kind": site.Kind})

		// 1) 调用日志（增量：从上次成功位点拉到当前）
		cp := checkpoint.NewStore(stateDir)
		var lastTS *int64
		if opts.Incremental {
			if v, ok := cp.LastLogTimestamp(site.SiteID); ok {
				lastTS = &v
			}
		}
		now := time.Now().Unix()
		logs, truncated, err := adapter.FetchLogs(lastTS, &now,
			cfg.Defaults.MaxPagesPerResource, cfg.Defaults.PageSize)
		if err != nil {
			return err
		}
		// 截断 = 数据有洞：绝不前移断点（下次从同一位置重拉），并在 artifact 标注
		newLast := int64(0)
		if !truncated {
			if lastTS != nil {
				newLast = *lastTS
			}
			for _, r := range logs {
				if r.ObservedAt > newLast {
					newLast = r.ObservedAt
				}
			}
		} else {
			log.Warn("logs.truncated", map[string]any{
				"run_id": runID, "site_id": site.SiteID,
				"count": len(logs),
				"note":  "超出 max_pages 上限，本轮不推进断点；请调大 max_pages_per_resource"})
		}

		// 1.5) 日志 ↔ 上游渠道对齐：admin 可用时回查渠道名，配置规则可覆盖
		var namesByID map[int64]string
		if site.AdminEnabled() {
			if cmgr, cerr := channels.Make(site, client); cerr == nil {
				if chs, _, lerr := cmgr.List(channels.ListOptions{PageSize: 500, MaxPages: 20}); lerr == nil {
					namesByID = make(map[int64]string, len(chs))
					for _, c := range chs {
						namesByID[c.ChannelID] = c.Name
					}
				} else {
					log.Warn("align.channels_fetch_failed", map[string]any{
						"run_id": runID, "site_id": site.SiteID, "error": lerr.Error()})
				}
			}
		}
		logs, astats := align.Apply(logs, site.LogAlignment, namesByID)
		log.Info("align.done", map[string]any{"run_id": runID,
			"site_id": site.SiteID, "native": astats.Native,
			"rule": astats.Rule, "unaligned": astats.Unaligned})

		// 2) 分组倍率
		ratios, err := adapter.FetchGroupRatios()
		if err != nil {
			return err
		}

		// 2.5) 模型价格（含缓存价）+ 官方预设倍率同步（可选能力）
		prices, err := adapter.FetchModelPrices()
		if err != nil {
			return err
		}
		var presetConfig map[string]any
		if adapter.Capabilities().PresetRatioSync {
			presetConfig, err = adapter.FetchPresetRatioConfig()
			if err != nil && ierr.CodeOf(err) != ierr.CodeUnsupported {
				log.Warn("preset_ratio.sync_failed", map[string]any{
					"run_id": runID, "site_id": site.SiteID, "error": err.Error()})
				presetConfig = nil
			}
		}

		// 3) 性能指标（按时间窗）
		modelSet := map[string]bool{}
		for _, r := range logs {
			if r.Model != nil && *r.Model != "" {
				modelSet[*r.Model] = true
			}
		}
		models := make([]string, 0, len(modelSet))
		for m := range modelSet {
			models = append(models, m)
		}
		var perf []records.PerfMetric
		for _, h := range windows {
			pm, err := adapter.FetchPerformance(models, h, maxModelsPerf)
			if err != nil {
				return err
			}
			perf = append(perf, pm...)
		}

		artifact := map[string]any{
			"schema_version": records.SchemaVersion,
			"run_id":         runID,
			"site_id":        site.SiteID,
			"site_kind":      site.Kind,
			"collected_at":   now,
			"capabilities": map[string]bool{
				"native_success_rate": adapter.Capabilities().NativeSuccessRate,
				"native_ttft":         adapter.Capabilities().NativeTTFT,
				"native_tps":          adapter.Capabilities().NativeTPS,
				"native_model_price":  adapter.Capabilities().NativeModelPrice,
				"preset_ratio_sync":   adapter.Capabilities().PresetRatioSync,
			},
			"call_logs":    logs,
			"group_ratios": ratios,
			"performance":  perf,
			"model_prices": prices,
		}
		if truncated {
			artifact["call_logs_truncated"] = true
			artifact["call_logs_note"] = "超出 max_pages_per_resource 上限，本轮数据不完整且断点未推进"
		}
		if presetConfig != nil {
			artifact["preset_ratio_config"] = presetConfig
		}
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return ierr.Wrap(ierr.CodeInternal, "创建输出目录失败", err)
		}
		artifactPath := filepath.Join(outDir, fmt.Sprintf("site-%s.json", site.SiteID))
		b, err := json.MarshalIndent(artifact, "", "  ")
		if err != nil {
			return ierr.Wrap(ierr.CodeInternal, "序列化 artifact 失败", err)
		}
		if err := os.WriteFile(artifactPath, b, 0o644); err != nil {
			return ierr.Wrap(ierr.CodeInternal, "写入 artifact 失败", err)
		}

		result.Counts = map[string]int{
			"call_logs": len(logs), "group_ratios": len(ratios),
			"performance": len(perf), "model_prices": len(prices),
			"aligned_native": astats.Native, "aligned_rule": astats.Rule,
			"aligned_unaligned": astats.Unaligned,
		}
		if truncated {
			result.Counts["call_logs_truncated"] = 1
		}
		result.Status = "success"
		result.Artifact = artifactPath
		if newLast > 0 && (lastTS == nil || newLast > *lastTS) {
			if err := cp.Save(site.SiteID, map[string]any{"last_log_timestamp": newLast}); err != nil {
				log.Warn("checkpoint.save_failed", map[string]any{
					"run_id": runID, "site_id": site.SiteID, "error": err.Error()})
			}
		}
		log.Info("site.done", map[string]any{"run_id": runID,
			"site_id": site.SiteID, "counts": result.Counts})
		return nil
	}

	if err := try(); err != nil {
		result.Status = "failed"
		result.ErrorCode = ierr.CodeOf(err)
		result.ErrorMessage = truncate(err.Error(), 300)
		log.Error("site.failed", map[string]any{"run_id": runID,
			"site_id": site.SiteID, "code": result.ErrorCode,
			"message": result.ErrorMessage})
	}
	return result
}

func pickTimeout(site *config.SiteConfig, cfg *config.Config) float64 {
	if site.TimeoutS > 0 {
		return site.TimeoutS
	}
	return cfg.Defaults.TimeoutS
}

func pickInterval(site *config.SiteConfig, cfg *config.Config) float64 {
	if site.MinIntervalS != nil {
		return *site.MinIntervalS
	}
	if cfg.Defaults.MinIntervalS != nil {
		return *cfg.Defaults.MinIntervalS
	}
	return 0.4
}

func truncate(s string, n int) string {
	if len(s) > n {
		r := []rune(s)
		if len(r) > n {
			return string(r[:n])
		}
	}
	return s
}
