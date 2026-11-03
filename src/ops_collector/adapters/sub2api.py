"""sub2api adapter（Wei-Shaw/sub2api）。

契约来源见 docs/upstream-contracts.md §2。核心端点：
- POST /api/v1/auth/login（+ /auth/refresh）
- GET  /api/v1/user/profile
- GET  /api/v1/usage?page=&page_size=&start_date=&end_date=&model=&group_id=
- GET  /api/v1/usage/errors
- GET  /api/v1/groups/available（rate_multiplier）
- GET  /api/v1/groups/rates
- GET  /api/v1/usage/dashboard/models（按模型聚合，含原生统计时优先采用）

性能指标说明（站点无用户级预聚合性能端点）：
- first_token_ms 为逐条原生字段 → TTFT 取均值（derived from native per-request）
- tps 按 output_tokens/((duration-first_token)/1000) 派生
- 成功率 = usage/(usage+errors)（近似口径，标注 derived）
"""

from __future__ import annotations

import datetime as _dt
from typing import Any

from ..auth import Authenticator, Sub2ApiAuth
from .. import secrets
from ..config import SiteConfig
from ..errors import ApiError, AuthError
from ..http import ApiClient, paginated_items, unwrap_sub2api
from ..records import CallRecord, GroupRatio, PerfMetric, clean_raw, _prov
from ..metrics import TPS_DEF, TTFT_DEF
from .base import Capabilities, SiteAdapter

USAGE_RAW_ALLOWLIST = {
    "id", "request_id", "model", "group_id", "api_key_id", "input_tokens",
    "output_tokens", "cache_read_tokens", "cache_creation_tokens",
    "total_cost", "actual_cost", "rate_multiplier", "request_type", "stream",
    "duration_ms", "first_token_ms", "created_at", "inbound_endpoint",
    "service_tier",
}


class Sub2ApiAdapter(SiteAdapter):
    kind = "sub2api"
    capabilities = Capabilities(
        logs=True, group_ratios=True,
        native_success_rate=False, native_ttft=False, native_tps=False,
    )

    def __init__(self, site: SiteConfig, client: ApiClient,
                 auth: Authenticator | None = None):
        self.site = site
        self.client = client
        self.auth = auth or Sub2ApiAuth()
        self._auth_ctx: dict | None = None

    def _headers(self) -> dict[str, str]:
        if self._auth_ctx is None:
            self._auth_ctx = self.auth.ensure_session(self.site, self.client)
        return self._auth_ctx["auth_headers"]

    def _get(self, path: str, **kw) -> Any:
        try:
            return self.client.get(path, headers=self._headers(), **kw)
        except AuthError:
            # JWT 可能已过期：尝试刷新，失败则清缓存重登
            try:
                self._auth_ctx = getattr(self.auth, "refresh", None) and \
                    self.auth.refresh(self.site, self.client)
            except Exception:
                self._auth_ctx = None
                secrets.drop_session(self.site.site_id)
            return self.client.get(path, headers=self._headers(), **kw)

    # ------------------------------------------------------------------
    def site_meta(self) -> dict[str, Any]:
        data = unwrap_sub2api(self._get("/api/v1/user/profile"), what="用户信息")
        user = data if isinstance(data, dict) else {}
        return {
            "user_id": user.get("id"),
            "username": user.get("email") or user.get("name"),
            "display_name": user.get("name") or user.get("email"),
            "role": user.get("role"),
            "balance": user.get("balance"),
        }

    # ------------------------------------------------------------------
    @staticmethod
    def _date(ts: int | None, *, end: bool = False) -> str | None:
        """转 YYYY-MM-DD。sub2api 的 end_date 是排他边界（endTime=日期+1天），
        因此 end_date 直接取 end_ts 所在日期即可覆盖到 end_ts（按天粒度，
        可能多覆盖到当天 24:00 前的记录，属上游日期粒度限制）。"""
        if ts is None:
            return None
        return _dt.datetime.fromtimestamp(ts, tz=_dt.timezone.utc).date().isoformat()

    def fetch_logs(self, *, start_ts=None, end_ts=None, max_pages=50,
                   page_size=100) -> list[CallRecord]:
        records: list[CallRecord] = []
        page = 1
        while page <= max_pages:
            params: dict[str, Any] = {"page": page, "page_size": page_size,
                                      "sort_by": "created_at", "sort_order": "desc"}
            sd = self._date(start_ts)
            ed = self._date(end_ts, end=True)
            if sd:
                params["start_date"] = sd
            if ed:
                params["end_date"] = ed
            data = unwrap_sub2api(self._get("/api/v1/usage", params=params),
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
        created = it.get("created_at")
        ts = 0
        if isinstance(created, (int, float)):
            ts = int(created)
        elif isinstance(created, str) and created:
            try:
                ts = int(_dt.datetime.fromisoformat(created.replace("Z", "+00:00")).timestamp())
            except ValueError:
                ts = 0
        total_cost = it.get("total_cost")
        # sub2api 失败请求在 errors 端点；usage 列表视为成功记账
        return CallRecord(
            site_id=self.site.site_id,
            site_kind=self.kind,
            record_id=str(it.get("id", it.get("request_id", ""))),
            observed_at=ts,
            model=it.get("model"),
            group=str(it["group_id"]) if it.get("group_id") is not None else None,
            status="success",
            error_class=None,
            input_tokens=it.get("input_tokens"),
            output_tokens=it.get("output_tokens"),
            cache_read_tokens=it.get("cache_read_tokens"),
            quota_cost=float(total_cost) if total_cost is not None else None,
            duration_ms=it.get("duration_ms"),
            first_token_ms=it.get("first_token_ms"),
            stream=bool(it["stream"]) if it.get("stream") is not None else None,
            raw=clean_raw(it, USAGE_RAW_ALLOWLIST),
        )

    def fetch_errors_count(self, *, start_ts=None, end_ts=None) -> tuple[int, list[dict]]:
        """失败请求数与（脱敏后的）错误列表。"""
        page = 1
        total = 0
        errs: list[dict] = []
        while page <= 20:
            params: dict[str, Any] = {"page": page, "page_size": 100}
            sd = self._date(start_ts)
            ed = self._date(end_ts, end=True)
            if sd:
                params["start_date"] = sd
            if ed:
                params["end_date"] = ed
            data = unwrap_sub2api(self._get("/api/v1/usage/errors", params=params),
                                  what="错误日志")
            items, t = paginated_items(data, what="错误日志")
            total = t
            errs.extend(it for it in items if isinstance(it, dict))
            if page * 100 >= total or not items:
                break
            page += 1
        return total, errs

    # ------------------------------------------------------------------
    def fetch_group_ratios(self) -> list[GroupRatio]:
        data = unwrap_sub2api(self._get("/api/v1/groups/available"), what="分组")
        if not isinstance(data, list):
            raise ApiError("分组响应不是数组")
        out: list[GroupRatio] = []
        for g in data:
            if not isinstance(g, dict):
                continue
            out.append(GroupRatio(
                site_id=self.site.site_id, site_kind=self.kind,
                group=str(g.get("name") or g.get("id")),
                ratio=float(g.get("rate_multiplier") or 1.0),
                description=g.get("description"),
                scope="site",
                extra={
                    "source": "/api/v1/groups/available",
                    "group_id": g.get("id"),
                    "platform": g.get("platform"),
                    "status": g.get("status"),
                },
            ))
        # 用户专属倍率（结构透传保存）
        try:
            rates = unwrap_sub2api(self._get("/api/v1/groups/rates"), what="用户倍率")
        except ApiError:
            rates = None
        if rates is not None:
            out.append(GroupRatio(
                site_id=self.site.site_id, site_kind=self.kind,
                group="_user_rates_raw", ratio=0.0, scope="site",
                extra={"source": "/api/v1/groups/rates", "raw": rates},
            ))
        return out

    # ------------------------------------------------------------------
    def fetch_performance(self, *, models=None, hours=24, max_models=20) -> list[PerfMetric]:
        from ..metrics import aggregate, sub2api_success_rate

        end_ts = int(_dt.datetime.now(tz=_dt.timezone.utc).timestamp())
        start_ts = end_ts - hours * 3600
        records = self.fetch_logs(start_ts=start_ts, end_ts=end_ts,
                                  max_pages=50, page_size=100)
        if models:
            allow = set(models)
            records = [r for r in records if r.model in allow]
        metrics = aggregate(records, site_id=self.site.site_id,
                            site_kind=self.kind, window_hours=hours)
        try:
            err_total, _ = self.fetch_errors_count(start_ts=start_ts, end_ts=end_ts)
        except ApiError:
            err_total = None
        if err_total is not None:
            usage_total = len(records)
            sr = sub2api_success_rate(usage_total, err_total)
            for m in metrics:
                m.success_rate = sr
            if not metrics:
                metrics.append(PerfMetric(
                    site_id=self.site.site_id, site_kind=self.kind,
                    model="_all", group=None, window_hours=hours,
                    success_rate=sr,
                    avg_ttft_ms=_prov(None, "missing", TTFT_DEF),
                    avg_output_tps=_prov(None, "missing", TPS_DEF),
                ))
        return metrics
