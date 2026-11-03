"""可选 MCP 封装（stdio）：把采集器作为工具暴露给运营 agent。

启动：`python -m ops_collector.mcp_server`（需 `pip install ops-collector[mcp]`）。
工具：
    list_sites      列出配置中启用的站点
    collect         执行采集并返回 run summary（同步、小数据量场景适用）
    get_run_status  读取某次 run 的 run.json
注意：MCP 进程里 stdout 属于协议通道，所有诊断只进 stderr（JsonlLogger 已保证）。
"""

from __future__ import annotations

import json
from pathlib import Path

from .config import load_config
from .errors import OpsCollectorError
from .run import default_var_dir, run_collect


def _run_summary_payload(run_id: str) -> dict:
    p = default_var_dir() / "runs" / run_id / "run.json"
    if not p.exists():
        return {"ok": False, "error": f"run 不存在: {run_id}"}
    return {"ok": True, **json.loads(p.read_text(encoding="utf-8"))}


def create_server():
    try:
        from mcp.server.fastmcp import FastMCP
    except ImportError as e:
        raise OpsCollectorError(
            "MCP 支持未安装：pip install ops-collector[mcp]",
            code="MCP_NOT_INSTALLED") from e

    mcp = FastMCP("ops-collector")

    @mcp.tool()
    def list_sites(config: str = "config/collector.json") -> str:
        """列出采集器配置中启用的站点（仅 ID 元数据，不含网址）。"""
        cfg = load_config(config)
        return json.dumps({
            "ok": True,
            "sites": [{"site_id": s.site_id, "kind": s.kind,
                       "name": s.name}
                      for s in cfg.enabled_sites()],
            "note": "站点以 site_id 标识（不以其网址）",
        }, ensure_ascii=False)

    @mcp.tool()
    def collect(site: str | None = None, config: str = "config/collector.json",
                no_incremental: bool = False) -> str:
        """执行一次采集。site 为空则采集全部启用站点；返回 run summary。"""
        cfg = load_config(config)
        result = run_collect(cfg, site_ids=[site] if site else None,
                             incremental=not no_incremental)
        return json.dumps(result.run_summary, ensure_ascii=False)

    @mcp.tool()
    def get_run_status(run_id: str) -> str:
        """读取某次采集的 run summary。"""
        return json.dumps(_run_summary_payload(run_id), ensure_ascii=False)

    @mcp.tool()
    def read_run_metrics(run_id: str) -> str:
        """读取某次采集的性能指标扁平表（供运营分析）。"""
        run_dir = default_var_dir() / "runs" / run_id
        rows = []
        for site_file in sorted(run_dir.glob("site-*.json")):
            data = json.loads(site_file.read_text(encoding="utf-8"))
            for p in data.get("performance", []):
                rows.append({
                    "site_id": data.get("site_id"),
                    "model": p.get("model"),
                    "group": p.get("group"),
                    "window_hours": p.get("window_hours"),
                    "success_rate": (p.get("success_rate") or {}).get("value"),
                    "avg_ttft_ms": (p.get("avg_ttft_ms") or {}).get("value"),
                    "avg_output_tps": (p.get("avg_output_tps") or {}).get("value"),
                })
        return json.dumps({"ok": True, "rows": rows}, ensure_ascii=False)

    return mcp


def main() -> None:
    mcp = create_server()
    mcp.run()


if __name__ == "__main__":
    main()
