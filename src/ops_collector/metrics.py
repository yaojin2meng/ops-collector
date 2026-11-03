"""指标聚合：把 CallRecord 列表聚成 (group, model) × 时间窗的性能指标。

成功率口径（固定，写入 definition）：
    成功请求数 / 可明确分类请求数（success + error；unknown 不计入分母）。
sub2api 无逐条成败标注时改用 errors 端点口径（见 adapter）。

派生公式：
    output_tps = output_tokens / ((duration_ms - first_token_ms)/1000)
                （缺 first_token_ms 时退化为 output_tokens / (duration_ms/1000)）
"""

from __future__ import annotations

from collections import defaultdict
from typing import Any

from .records import CallRecord, PerfMetric, _prov

SUCCESS_RATE_DEF = "成功请求数/可明确分类请求数（success+error，unknown 不计分母）"
TPS_DEF = "output_tokens / ((duration_ms - first_token_ms)/1000)；缺首字时用 output_tokens/(duration_ms/1000)"
TTFT_DEF = "调用日志逐条 first_token_ms 的平均值（derived from per-request native values）"


def _mean(values: list[float]) -> float | None:
    return sum(values) / len(values) if values else None


def aggregate(records: list[CallRecord], *, site_id: str, site_kind: str,
              window_hours: int) -> list[PerfMetric]:
    buckets: dict[tuple[str, str], list[CallRecord]] = defaultdict(list)
    for r in records:
        if r.first_token_ms is None and r.duration_ms is None and r.status == "unknown":
            continue
        buckets[(r.group or "_default", r.model or "_unknown")].append(r)

    out: list[PerfMetric] = []
    for (group, model), rows in sorted(buckets.items()):
        classified = [r for r in rows if r.status in ("success", "error")]
        ok = [r for r in classified if r.status == "success"]
        success_rate = (len(ok) / len(classified) * 100) if classified else None
        prov: str = "derived"

        ttft_vals = [float(r.first_token_ms) for r in rows if r.first_token_ms is not None]
        tps_vals: list[float] = []
        for r in rows:
            if r.status != "success" or not r.output_tokens:
                continue
            if r.duration_ms and r.duration_ms > 0:
                eff_ms = r.duration_ms - (r.first_token_ms or 0)
                if eff_ms > 0:
                    tps_vals.append(r.output_tokens / (eff_ms / 1000.0))
        ttft = _mean(ttft_vals)
        tps = _mean(tps_vals)

        out.append(PerfMetric(
            site_id=site_id,
            site_kind=site_kind,
            model=model,
            group=None if group == "_default" else group,
            window_hours=window_hours,
            success_rate=_prov(success_rate, prov, SUCCESS_RATE_DEF),
            avg_ttft_ms=_prov(ttft, "derived", TTFT_DEF),
            avg_output_tps=_prov(tps, "derived", TPS_DEF),
            request_count=len(rows),
        ))
    return out


def merge_native_perf(models_summary: dict[str, dict[str, Any]],
                      per_group: dict[str, list[dict[str, Any]]],
                      *, site_id: str, site_kind: str,
                      window_hours: int) -> list[PerfMetric]:
    """合并 new-api 原生指标：models_summary 来自 /perf-metrics/summary，
    per_group[model] 来自 /perf-metrics?model= 的 groups 数组。"""
    out: list[PerfMetric] = []
    seen_groups: set[tuple[str, str]] = set()

    for model, groups in per_group.items():
        for g in groups:
            key = (model, str(g.get("group")))
            seen_groups.add(key)
            out.append(PerfMetric(
                site_id=site_id, site_kind=site_kind, model=model,
                group=g.get("group"), window_hours=window_hours,
                success_rate=_prov(g.get("success_rate"), "native",
                                   "站点原生聚合：排除业务拒绝，含当前不完整小时"),
                avg_ttft_ms=_prov(g.get("avg_ttft_ms"), "native", "站点原生 TTFT 聚合"),
                avg_output_tps=_prov(g.get("avg_tps"), "native", "站点原生吞吐聚合"),
                avg_latency_ms=_prov(g.get("avg_latency_ms"), "native", "站点原生延迟聚合"),
            ))

    for model, s in models_summary.items():
        if (model, "") in seen_groups or any(k[0] == model and k[1] != "" for k in seen_groups):
            continue
        out.append(PerfMetric(
            site_id=site_id, site_kind=site_kind, model=model, group=None,
            window_hours=window_hours,
            success_rate=_prov(s.get("success_rate"), "native",
                               "站点原生聚合：排除业务拒绝，含当前不完整小时"),
            avg_ttft_ms=_prov(None, "unsupported", "summary 端点无 TTFT，请用 perf-metrics 按分组获取"),
            avg_output_tps=_prov(s.get("avg_tps"), "native", "站点原生吞吐聚合"),
            avg_latency_ms=_prov(s.get("avg_latency_ms"), "native", "站点原生延迟聚合"),
            request_count=s.get("request_count"),
        ))
    return out


def sub2api_success_rate(usage_total: int, errors_total: int) -> dict[str, Any]:
    """sub2api 近似口径：usage 列表为成功记账记录，失败单独在 errors 端点。"""
    total = usage_total + errors_total
    rate = (usage_total / total * 100) if total else None
    return _prov(rate, "derived",
                 "usage_total/(usage_total+errors_total)，来自 usage 与 usage/errors 两个端点（近似口径）")
