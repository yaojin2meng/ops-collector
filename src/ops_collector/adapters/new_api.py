"""new-api adapter（QuantumNous/new-api）。

契约来源见 docs/upstream-contracts.md §1。全部端点：
- POST /api/user/login（+ GET /api/user/login/encryption-key）
- GET  /api/user/self
- GET  /api/log/self?p=&page_size=&start_timestamp=&end_timestamp=
- GET  /api/log/self/stat
- GET  /api/pricing（group_ratio / usable_group / 模型倍率）
- GET  /api/perf-metrics/summary?hours=&recent_model=
- GET  /api/perf-metrics?model=&hours=（按分组原生 TTFT/tps/success_rate）
"""

from __future__ import annotations

from typing import Any

from ..auth import Authenticator, NewApiAuth
from .. import secrets
from ..errors import ApiError, AuthError
from ..config import SiteConfig
from ..errors import ApiError
from ..http import ApiClient, paginated_items, unwrap_new_api
from ..records import CallRecord, GroupRatio, PerfMetric, clean_raw
from .base import Capabilities, SiteAdapter

LOG_RAW_ALLOWLIST = {
    "id", "user_id", "created_at", "type", "content", "username", "token_name",
    "model_name", "quota", "prompt_tokens", "completion_tokens", "use_time",
    "is_stream", "channel", "group",
}


class NewApiAdapter(SiteAdapter):
    kind = "new_api"
    capabilities = Capabilities(
        logs=True, group_ratios=True,
        native_success_rate=True, native_ttft=True, native_tps=True,
    )

    def __init__(self, site: SiteConfig, client: ApiClient,
                 auth: Authenticator | None = None):
        self.site = site
        self.client = client
        self.auth = auth or NewApiAuth()
        self._auth_ctx: dict | None = None

    # ------------------------------------------------------------------
    def _headers(self) -> dict[str, str]:
        if self._auth_ctx is None:
            self._auth_ctx = self.auth.ensure_session(self.site, self.client)
        return self._auth_ctx["auth_headers"]

    def _get(self, path: str, **kw) -> Any:
        try:
            return self.client.get(path, headers=self._headers(), **kw)
        except AuthError:
            # 会话可能已被服务端吊销：清缓存重登一次
            secrets.drop_session(self.site.site_id)
            self._auth_ctx = None
            return self.client.get(path, headers=self._headers(), **kw)

    # ------------------------------------------------------------------
    def site_meta(self) -> dict[str, Any]:
        data = unwrap_new_api(self._get("/api/user/self"), what="用户信息")
        user = data if isinstance(data, dict) else {}
        return {
            "user_id": user.get("id"),
            "username": user.get("username"),
            "display_name": user.get("display_name") or user.get("username"),
            "role": user.get("role"),
            "quota": user.get("quota"),
            "group": user.get("group"),
        }

    # ------------------------------------------------------------------
    def fetch_logs(self, *, start_ts=None, end_ts=None, max_pages=50,
                   page_size=100) -> list[CallRecord]:
        records: list[CallRecord] = []
        page = 1
        while page <= max_pages:
            params: dict[str, Any] = {"p": page, "page_size": page_size}
            if start_ts:
                params["start_timestamp"] = int(start_ts)
            if end_ts:
                params["end_timestamp"] = int(end_ts)
            data = unwrap_new_api(self._get("/api/log/self", params=params),
                                  what="调用日志")
            items, total = paginated_items(data, what="调用日志")
            if not items:
                break
            for it in items:
                if isinstance(it, dict):
                    records.append(self._to_call_record(it))
            if page * page_size >= total:
                break
            page += 1
        return records

    def _to_call_record(self, it: dict[str, Any]) -> CallRecord:
        # type: 1=消费 2=充值 3=管理 4=系统；只有消费类型是调用记录
        log_type = it.get("type", 1)
        quota_cost = it.get("quota")
        return CallRecord(
            site_id=self.site.site_id,
            site_kind=self.kind,
            record_id=str(it.get("id", "")),
            observed_at=int(it.get("created_at") or 0),
            model=it.get("model_name"),
            group=it.get("group"),
            status="success" if (log_type == 1) else "unknown",
            error_class=None,
            input_tokens=it.get("prompt_tokens"),
            output_tokens=it.get("completion_tokens"),
            quota_cost=float(quota_cost) if quota_cost is not None else None,
            duration_ms=int(it.get("use_time") or 0) * 1000 if it.get("use_time") is not None else None,
            first_token_ms=None,   # 日志逐条无 TTFT 字段；用 perf-metrics 原生聚合
            stream=bool(it["is_stream"]) if it.get("is_stream") is not None else None,
            raw=clean_raw(it, LOG_RAW_ALLOWLIST),
        )

    # ------------------------------------------------------------------
    def fetch_group_ratios(self) -> list[GroupRatio]:
        # 注意：/api/pricing 的 group_ratio / usable_group / vendors 与 data 平级
        # （PricingData 结构，见 docs/upstream-contracts.md §1），不能对它做 unwrap。
        payload = self._get("/api/pricing")
        data = payload if isinstance(payload, dict) else None
        if data is None:
            raise ApiError("定价响应不是对象")
        out: list[GroupRatio] = []
        group_ratio = data.get("group_ratio") or {}
        usable = data.get("usable_group") or {}
        for g, ratio in group_ratio.items():
            desc = None
            if isinstance(usable.get(g), dict):
                desc = usable[g].get("desc")
            elif isinstance(usable.get(g), str):
                desc = usable[g]
            out.append(GroupRatio(
                site_id=self.site.site_id, site_kind=self.kind, group=str(g),
                ratio=float(ratio), description=desc, scope="site",
                extra={"source": "/api/pricing group_ratio"},
            ))
        # 模型级覆盖倍率（若上游返回）
        for m in data.get("data") or []:
            if not isinstance(m, dict):
                continue
            mgroup = m.get("group_ratio")
            if isinstance(mgroup, dict):
                for g, ratio in mgroup.items():
                    out.append(GroupRatio(
                        site_id=self.site.site_id, site_kind=self.kind,
                        group=str(g), ratio=float(ratio), scope="model",
                        model_name=m.get("model_name"),
                        extra={"source": "/api/pricing model.group_ratio"},
                    ))
        return out

    # ------------------------------------------------------------------
    def fetch_performance(self, *, models=None, hours=24, max_models=20) -> list[PerfMetric]:
        model_names = list(models or [])
        # 汇总端点：原生 success_rate / avg_tps / request_count
        params: dict[str, Any] = {"hours": hours}
        if model_names:
            params["recent_model"] = model_names[:max_models]
        summary_data = unwrap_new_api(self._get("/api/perf-metrics/summary", params=params),
                                      what="性能汇总")
        models_summary: dict[str, dict[str, Any]] = {}
        if isinstance(summary_data, dict):
            for m in summary_data.get("models") or []:
                if isinstance(m, dict) and m.get("model_name"):
                    models_summary[m["model_name"]] = m

        # 按分组端点：原生 avg_ttft_ms（首字）
        per_group: dict[str, list[dict[str, Any]]] = {}
        targets = model_names[:max_models] or list(models_summary.keys())[:max_models]
        for name in targets:
            try:
                detail = unwrap_new_api(
                    self._get("/api/perf-metrics",
                              params={"model": name, "hours": hours}),
                    what=f"模型性能 {name}")
            except ApiError:
                continue
            if isinstance(detail, dict) and isinstance(detail.get("groups"), list):
                per_group[name] = [g for g in detail["groups"] if isinstance(g, dict)]

        from ..metrics import merge_native_perf
        return merge_native_perf(models_summary, per_group,
                                 site_id=self.site.site_id, site_kind=self.kind,
                                 window_hours=hours)

    def fetch_log_stat(self, *, start_ts=None, end_ts=None,
                       model_name=None, group=None) -> dict[str, Any]:
        params: dict[str, Any] = {}
        if start_ts:
            params["start_timestamp"] = int(start_ts)
        if end_ts:
            params["end_timestamp"] = int(end_ts)
        if model_name:
            params["model_name"] = model_name
        if group:
            params["group"] = group
        data = unwrap_new_api(self._get("/api/log/self/stat", params=params),
                              what="日志统计")
        return data if isinstance(data, dict) else {}
