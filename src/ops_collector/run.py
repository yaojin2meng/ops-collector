"""多站点编排：并发隔离采集、每站点独立失败处理、artifact 与 run summary 落盘。"""

from __future__ import annotations

import json
import time
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path
from typing import Any

from . import SCHEMA_VERSION
from .adapters import make_adapter
from .checkpoint import CheckpointStore
from .config import CollectorConfig, SiteConfig
from .errors import OpsCollectorError
from .http import ApiClient
from .logging_util import JsonlLogger
from .records import CallRecord, GroupRatio, PerfMetric

RUNS_DIR = "runs"
LOGS_DIR = "logs"
STATE_DIR = "state"


def default_var_dir() -> Path:
    return Path(__file__).resolve().parent.parent.parent / "var"


class SiteResult:
    def __init__(self, site_id: str, kind: str):
        self.site_id = site_id
        self.site_kind = kind
        self.status: str = "pending"        # success | partial | failed | skipped
        self.error_code: str | None = None
        self.error_message: str | None = None
        self.counts: dict[str, int] = {}
        self.artifact: str | None = None
        self.duration_s: float = 0.0

    def to_dict(self) -> dict[str, Any]:
        return {
            "site_id": self.site_id,
            "site_kind": self.site_kind,
            "status": self.status,
            "error_code": self.error_code,
            "error_message": self.error_message,
            "counts": self.counts,
            "artifact": self.artifact,
            "duration_s": round(self.duration_s, 2),
        }


class RunResult:
    def __init__(self, run_id: str):
        self.run_id = run_id
        self.started_at = int(time.time())
        self.finished_at: int | None = None
        self.status: str = "running"        # success | partial | failed
        self.site_results: list[SiteResult] = []
        self.output_dir: str | None = None

    @property
    def run_summary(self) -> dict[str, Any]:
        return {
            "schema_version": SCHEMA_VERSION,
            "run_id": self.run_id,
            "started_at": self.started_at,
            "finished_at": self.finished_at,
            "status": self.status,
            "sites": [s.to_dict() for s in self.site_results],
            "output_dir": self.output_dir,
        }

    def to_json(self) -> str:
        return json.dumps(self.run_summary, ensure_ascii=False, indent=2)


def _collect_site(site: SiteConfig, cfg: CollectorConfig, run_id: str,
                  out_dir: Path, state_dir: Path,
                  logger: JsonlLogger, *, hours_windows: list[int] | None = None,
                  incremental: bool = True) -> SiteResult:
    result = SiteResult(site.site_id, site.kind)
    t0 = time.monotonic()
    windows = hours_windows or cfg.defaults.windows_hours
    try:
        client = ApiClient(
            site.base_url,
            timeout_s=site.timeout_s or cfg.defaults.timeout_s,
            retries=cfg.defaults.retries,
            min_interval_s=site.min_interval_s if site.min_interval_s is not None
            else cfg.defaults.min_interval_s,
            backoff_base_s=cfg.defaults.backoff_base_s,
            verify=site.verify_tls,
        )
        adapter = make_adapter(site, client)
        logger.info("site.start", run_id=run_id, site_id=site.site_id, kind=site.kind)

        # 1) 调用日志（增量：从上次成功位点拉到当前）
        cp = CheckpointStore(state_dir)
        last_ts = cp.last_log_timestamp(site.site_id) if incremental else None
        now = int(time.time())
        logs: list[CallRecord] = adapter.fetch_logs(
            start_ts=last_ts, end_ts=now,
            max_pages=cfg.defaults.max_pages_per_resource,
            page_size=cfg.defaults.page_size)
        new_last = max((r.observed_at for r in logs if r.observed_at), default=last_ts or 0)

        # 2) 分组倍率
        ratios: list[GroupRatio] = adapter.fetch_group_ratios()

        # 3) 性能指标（按时间窗）
        models = sorted({r.model for r in logs if r.model})
        perf: list[PerfMetric] = []
        for h in windows:
            perf.extend(adapter.fetch_performance(
                models=models, hours=h, max_models=cfg.defaults.concurrency * 5))

        artifact = {
            "schema_version": SCHEMA_VERSION,
            "run_id": run_id,
            "site_id": site.site_id,
            "site_kind": site.kind,
            "collected_at": now,
            "capabilities": {
                "native_success_rate": adapter.capabilities.native_success_rate,
                "native_ttft": adapter.capabilities.native_ttft,
                "native_tps": adapter.capabilities.native_tps,
            },
            "call_logs": [r.model_dump() for r in logs],
            "group_ratios": [r.model_dump() for r in ratios],
            "performance": [p.model_dump() for p in perf],
        }
        out_dir.mkdir(parents=True, exist_ok=True)
        artifact_path = out_dir / f"site-{site.site_id}.json"
        artifact_path.write_text(json.dumps(artifact, ensure_ascii=False, indent=2),
                                 encoding="utf-8")

        result.counts = {"call_logs": len(logs), "group_ratios": len(ratios),
                         "performance": len(perf)}
        result.status = "success"
        result.artifact = str(artifact_path)
        if new_last and new_last > (last_ts or 0):
            cp.save(site.site_id, {"last_log_timestamp": new_last})
        logger.info("site.done", run_id=run_id, site_id=site.site_id,
                    counts=result.counts)
    except OpsCollectorError as e:
        result.status = "failed"
        result.error_code = e.code
        result.error_message = str(e)[:300]
        logger.error("site.failed", run_id=run_id, site_id=site.site_id,
                     code=e.code, message=result.error_message)
    except Exception as e:  # 未知异常也按站点隔离
        result.status = "failed"
        result.error_code = "INTERNAL_ERROR"
        result.error_message = f"{type(e).__name__}: {e}"[:300]
        logger.error("site.failed", run_id=run_id, site_id=site.site_id,
                     code="INTERNAL_ERROR", message=result.error_message)
    finally:
        result.duration_s = time.monotonic() - t0
    return result


def run_collect(cfg: CollectorConfig, *, var_dir: Path | None = None,
                site_ids: list[str] | None = None, run_id: str | None = None,
                incremental: bool = True) -> RunResult:
    var_dir = var_dir or default_var_dir()
    run_id = run_id or uuid.uuid4().hex[:12]
    out_dir = var_dir / RUNS_DIR / run_id
    logger = JsonlLogger(var_dir / LOGS_DIR, run_id=run_id)
    result = RunResult(run_id)
    result.output_dir = str(out_dir)

    sites = cfg.enabled_sites()
    if site_ids:
        wanted = set(site_ids)
        sites = [s for s in sites if s.site_id in wanted]
        missing = wanted - {s.site_id for s in sites}
        if missing:
            logger.warn("sites.not_found", run_id=run_id, site_ids=sorted(missing))

    concurrency = max(1, cfg.defaults.concurrency)
    with ThreadPoolExecutor(max_workers=min(concurrency, max(1, len(sites)))) as pool:
        futures = {pool.submit(_collect_site, s, cfg, run_id, out_dir,
                               var_dir / STATE_DIR, logger): s for s in sites}
        for fut in as_completed(futures):
            result.site_results.append(fut.result())

    statuses = [s.status for s in result.site_results]
    if not statuses:
        result.status = "failed"
    elif all(s == "success" for s in statuses):
        result.status = "success"
    elif any(s in ("success", "partial") for s in statuses):
        result.status = "partial"
    else:
        result.status = "failed"
    result.finished_at = int(time.time())

    out_dir.mkdir(parents=True, exist_ok=True)
    (out_dir / "run.json").write_text(result.to_json(), encoding="utf-8")
    logger.info("run.done", run_id=run_id, status=result.status)
    logger.close()
    return result
