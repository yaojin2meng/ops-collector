"""统一记录模型：所有站点数据归一化后的稳定契约。

每个数值都带来源标注（native=站点原生聚合 / derived=本地按公式推算 /
unsupported=上游无此数据 / missing=本次未取到）与口径说明。
字段白名单清洗：提示词、响应正文、Cookie、令牌等一律不进入记录。
"""

from __future__ import annotations

from typing import Any, Literal

from pydantic import BaseModel, Field

from . import SCHEMA_VERSION

MetricProvenance = Literal["native", "derived", "unsupported", "missing"]


def _prov(value: Any, provenance: MetricProvenance = "native",
          definition: str | None = None) -> dict[str, Any]:
    out: dict[str, Any] = {"value": value, "provenance": provenance}
    if definition:
        out["definition"] = definition
    return out


class CallRecord(BaseModel):
    """一条脱敏后的调用日志。"""

    schema_version: int = SCHEMA_VERSION
    site_id: str
    site_kind: str
    record_id: str                      # 站点内唯一 id（new-api 日志 id / sub2api usage id）
    observed_at: int                    # unix 秒
    model: str | None = None
    group: str | None = None            # 分组名（new-api）或分组 id 字符串（sub2api）
    status: Literal["success", "error", "unknown"] = "unknown"
    error_class: str | None = None
    input_tokens: int | None = None
    output_tokens: int | None = None
    cache_read_tokens: int | None = None
    quota_cost: float | None = None     # new-api quota / sub2api total_cost
    duration_ms: int | None = None
    first_token_ms: int | None = None   # TTFT（逐条原生字段）
    stream: bool | None = None
    raw: dict[str, Any] = Field(default_factory=dict)  # 白名单内的上游原始字段


class GroupRatio(BaseModel):
    """分组倍率。new-api: group->ratio；sub2api: 每组 rate_multiplier。"""

    schema_version: int = SCHEMA_VERSION
    site_id: str
    site_kind: str
    group: str
    ratio: float
    description: str | None = None
    scope: Literal["site", "model"] = "site"
    model_name: str | None = None       # scope=model 时填写
    extra: dict[str, Any] = Field(default_factory=dict)


class PerfMetric(BaseModel):
    """一个 (group, model) 组合在时间窗内的性能指标。"""

    schema_version: int = SCHEMA_VERSION
    site_id: str
    site_kind: str
    model: str
    group: str | None = None
    window_hours: int
    success_rate: dict[str, Any]        # _prov() 结构
    avg_ttft_ms: dict[str, Any]         # 首字
    avg_output_tps: dict[str, Any]      # tk/s
    avg_latency_ms: dict[str, Any] | None = None
    request_count: int | None = None


def clean_raw(payload: dict[str, Any], allowlist: set[str]) -> dict[str, Any]:
    """只保留白名单字段（浅拷贝），杜绝提示词/密钥类字段泄漏进 artifact。"""
    return {k: v for k, v in payload.items() if k in allowlist}
