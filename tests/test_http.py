"""HTTP 客户端测试：重试、退避、包络解析、限速。"""

from __future__ import annotations

import httpx
import pytest

from ops_collector.errors import ApiError, AuthError, TransientError
from ops_collector.http import (ApiClient, paginated_items, parse_json_body,
                                unwrap_new_api, unwrap_sub2api)


def test_retry_on_500_then_success():
    calls = {"n": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        if calls["n"] < 3:
            return httpx.Response(500, json={"error": "boom"})
        return httpx.Response(200, json={"ok": True})

    with ApiClient("https://x.test", transport=httpx.MockTransport(handler),
                   retries=3, min_interval_s=0, backoff_base_s=0.01) as c:
        data = c.get("/api/thing")
    assert data == {"ok": True}
    assert calls["n"] == 3


def test_401_no_retry():
    calls = {"n": 0}

    def handler(request: httpx.Request) -> httpx.Response:
        calls["n"] += 1
        return httpx.Response(401, json={"error": "unauthorized"})

    with ApiClient("https://x.test", transport=httpx.MockTransport(handler),
                   retries=3, min_interval_s=0) as c:
        with pytest.raises(AuthError):
            c.get("/api/thing")
    assert calls["n"] == 1


def test_429_retry_after_header():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(429, headers={"Retry-After": "0"},
                              json={"error": "slow down"})

    with ApiClient("https://x.test", transport=httpx.MockTransport(handler),
                   retries=1, min_interval_s=0, backoff_base_s=0.01) as c:
        with pytest.raises(TransientError):
            c.get("/api/thing")


def test_400_permanent_error():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(400, json={"error": "bad"})

    with ApiClient("https://x.test", transport=httpx.MockTransport(handler),
                   retries=2, min_interval_s=0) as c:
        with pytest.raises(ApiError, match="HTTP_400|400"):
            c.get("/api/thing")


def test_network_error_retries_then_raises():
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("conn refused")

    with ApiClient("https://x.test", transport=httpx.MockTransport(handler),
                   retries=1, min_interval_s=0, backoff_base_s=0.01) as c:
        with pytest.raises(TransientError):
            c.get("/api/thing")


def test_invalid_json_response():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(200, content=b"<html>not json</html>")

    with ApiClient("https://x.test", transport=httpx.MockTransport(handler),
                   min_interval_s=0) as c:
        with pytest.raises(ApiError, match="JSON"):
            c.get("/api/thing")


def test_new_api_envelope():
    assert unwrap_new_api({"success": True, "data": 5}, what="t") == 5
    with pytest.raises(ApiError, match="t"):
        unwrap_new_api({"success": False, "message": "bad"}, what="t")


def test_sub2api_envelope():
    assert unwrap_sub2api({"code": 0, "data": [1]}, what="t") == [1]
    with pytest.raises(ApiError, match="t"):
        unwrap_sub2api({"code": 1, "message": "bad"}, what="t")


def test_paginated_items_shapes():
    items, total = paginated_items({"items": [1, 2], "total": 2}, what="t")
    assert items == [1, 2] and total == 2
    items, total = paginated_items([9, 8], what="t")
    assert items == [9, 8] and total == 2
    with pytest.raises(ApiError):
        paginated_items({"no_items": 1}, what="t")
