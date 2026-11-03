// sites 子命令：站点清单（只给 ID 元数据，不暴露网址）。
package cli

import (
	"flag"
	"fmt"
	"os"

	"opscollector/internal/config"
	ierr "opscollector/internal/errors"
)

func cmdSites(args []string) int {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		usageSites()
		return 0
	}
	fs := flag.NewFlagSet("sites", flag.ContinueOnError)
	cfgPath := fs.String("config", "config/collector.json", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return fail(ierr.New(ierr.CodeUsage, "参数错误: "+err.Error()))
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fail(err)
	}
	type siteRow struct {
		SiteID string `json:"site_id"`
		Name   string `json:"name,omitempty"`
		Kind   string `json:"kind"`
		Admin  bool   `json:"admin"`
		Status string `json:"status"` // enabled | disabled
	}
	rows := make([]siteRow, 0, len(cfg.Sites))
	for _, s := range cfg.Sites {
		st := "enabled"
		if !s.IsEnabled() {
			st = "disabled"
		}
		rows = append(rows, siteRow{
			SiteID: s.SiteID, Name: s.Name, Kind: s.Kind,
			Admin: s.AdminEnabled(), Status: st,
		})
	}
	emit(map[string]any{"ok": true, "sites": rows,
		"note": "站点以 site_id 标识（不含网址）；渠道以 channel_id 标识"})
	return 0
}

func usageSites() {
	fmt.Fprintln(os.Stderr, "用法: ops-collector sites [--config P]   # 站点清单（仅 ID 元数据）")
}
