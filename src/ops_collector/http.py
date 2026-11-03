"""HTTP 客户端：超时、限速、有限重试（429/5xx + Retry-After）、包络解析。

- 认证错误（401/403）与其它 4xx 不重试。
- 重试带指数退避 + 抖动；遇到 Retry-After 头时优先遵循。
- 统一 JSON 包络解析：new-api ``{success, message, data}`` 与
  sub2api ``{code, message, data}`` 两种。
"""

from __future__ import annotations

import json
import random
import threading
import time
from typing import Any

import httpx

from .errors import ApiError, AuthError, TransientError

RETRYABLE_STATUS = {429, 500, 502, 503, 504}


class ApiClient:
    def __init__(
        self,
        base_url: str,
        *,
        timeout_s: float = 20.0,
        retries: int = 3,
        min_interval_s: float = 0.4,
        backoff_base_s: float = 1.2,
        verify: bool = True,
        headers: dict[str, str] | None = None,
        transport: httpx.BaseTransport | None = None,
    ):
        self._client = httpx.Client(
            base_url=base_url,
            timeout=timeout_s,
            verify=verify,
            headers={"User-Agent": "ops-collector/0.1 (+ops-data-collection)",
                     **(headers or {})},
            transport=transport,
            follow_redirects=True,
        )
        self.retries = retries
        self.backoff_base_s = backoff_base_s
        self._min_interval = min_interval_s
        self._last_request_ts = 0.0
        self._lock = threading.Lock()

    def close(self) -> None:
        self._client.close()

    def __enter__(self) -> "ApiClient":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    # ------------------------------------------------------------------
    def _throttle(self) -> None:
        with self._lock:
            wait = self._min_interval - (time.monotonic() - self._last_request_ts)
            if wait > 0:
                time.sleep(wait)
            self._last_request_ts = time.monotonic()

    def request(self, method: str, path: str, *, params=None, json_body=None,
                headers: dict[str, str] | None = None) -> dict[str, Any] | list[Any]:
        """带重试的请求，返回解析后的 JSON（dict 或 list）。"""
        last_err: Exception | None = None
        for attempt in range(self.retries + 1):
            self._throttle()
            try:
                resp = self._client.request(
                    method, path, params=params, json=json_body, headers=headers
                )
            except (httpx.TimeoutException, httpx.TransportError) as e:
                last_err = TransientError(f"网络错误: {type(e).__name__}")
            else:
                if resp.status_code in (401, 403):
                    raise AuthError(f"认证被拒绝 (HTTP {resp.status_code})")
                if resp.status_code in RETRYABLE_STATUS:
                    retry_after = resp.headers.get("Retry-After")
                    delay = float(retry_after) if (retry_after or "").isdigit() else None
                    last_err = TransientError(f"上游暂时不可用 (HTTP {resp.status_code})")
                    if attempt < self.retries:
                        time.sleep(delay if delay is not None
                                   else self.backoff_base_s * (2**attempt) + random.uniform(0, 0.4))
                    continue
                if resp.status_code >= 400:
                    raise ApiError(f"上游返回 HTTP {resp.status_code}", status=resp.status_code)
                return parse_json_body(resp)

            if attempt < self.retries:
                time.sleep(self.backoff_base_s * (2**attempt) + random.uniform(0, 0.4))
        raise last_err or TransientError("请求失败")

    def get(self, path: str, **kw) -> Any:
        return self.request("GET", path, **kw)

    def post(self, path: str, **kw) -> Any:
        return self.request("POST", path, **kw)


def parse_json_body(resp: httpx.Response) -> Any:
    try:
        return resp.json()
    except (json.JSONDecodeError, ValueError) as e:
        raise ApiError(f"响应不是合法 JSON (HTTP {resp.status_code})",
                       status=resp.status_code) from e


def unwrap_new_api(payload: Any, *, what: str) -> Any:
    """new-api 包络 {success, message, data}。"""
    if not isinstance(payload, dict):
        raise ApiError(f"{what}: 包络不是对象")
    if payload.get("success") is not True:
        msg = str(payload.get("message") or "unknown")
        raise ApiError(f"{what}: {msg[:200]}")
    return payload.get("data")


def unwrap_sub2api(payload: Any, *, what: str) -> Any:
    """sub2api 包络 {code, message, data}，code==0 为成功。"""
    if not isinstance(payload, dict):
        raise ApiError(f"{what}: 包络不是对象")
    if payload.get("code") != 0:
        msg = str(payload.get("message") or "unknown")
        raise ApiError(f"{what}: {msg[:200]}")
    return payload.get("data")


def paginated_items(data: Any, *, what: str) -> tuple[list[Any], int]:
    """从分页 data 里取 (items, total)。兼容 {items,total} 与裸数组两种形态。"""
    if isinstance(data, dict):
        items = data.get("items")
        if items is None and "data" in data and isinstance(data["data"], list):
            items = data["data"]
        if not isinstance(items, list):
            raise ApiError(f"{what}: 分页数据缺少 items 数组")
        total = data.get("total", len(items))
        return items, int(total) if isinstance(total, (int, float)) else len(items)
    if isinstance(data, list):
        return data, len(data)
    raise ApiError(f"{what}: 未知分页数据形态")
