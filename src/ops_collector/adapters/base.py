"""adapter 协议与公共能力声明。"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Protocol

from ..auth import Authenticator, make_authenticator
from ..config import SiteConfig
from ..errors import OpsCollectorError
from ..http import ApiClient
from ..records import CallRecord, GroupRatio, PerfMetric


@dataclass
class Capabilities:
    """每个 adapter 显式声明能力，避免 agent 猜测。"""

    logs: bool
    group_ratios: bool
    native_success_rate: bool     # 站点提供原生成功率聚合
    native_ttft: bool             # 站点提供原生首字聚合
    native_tps: bool              # 站点提供原生吞吐聚合


class SiteAdapter(Protocol):
    kind: str
    capabilities: Capabilities

    def __init__(self, site: SiteConfig, client: ApiClient,
                 auth: Authenticator | None = None): ...

    def fetch_logs(self, *, start_ts: int | None = None, end_ts: int | None = None,
                   max_pages: int = 50, page_size: int = 100) -> list[CallRecord]: ...

    def fetch_group_ratios(self) -> list[GroupRatio]: ...

    def fetch_performance(self, *, models: list[str] | None = None,
                          hours: int = 24, max_models: int = 20
                          ) -> list[PerfMetric]: ...

    def site_meta(self) -> dict[str, Any]:
        """站点基本信息（用户、版本等），用于 login-test。"""
        ...


def make_adapter(site: SiteConfig, client: ApiClient) -> SiteAdapter:
    auth = make_authenticator(site.kind)
    if site.kind == "new_api":
        from .new_api import NewApiAdapter
        return NewApiAdapter(site, client, auth)
    if site.kind == "sub2api":
        from .sub2api import Sub2ApiAdapter
        return Sub2ApiAdapter(site, client, auth)
    raise OpsCollectorError(f"未知站点类型: {site.kind}", code="CONFIG_INVALID")
