"""共享测试夹具：内存凭据存储 + 两个站点的 MockTransport。"""

from __future__ import annotations

import json
import time
import uuid

import httpx
import pytest

from ops_collector import secrets as secrets_mod
from ops_collector.config import AuthConfig, CollectorConfig, DefaultsConfig, SiteConfig


# ---------------------------------------------------------------------------
# 内存凭据存储（绕开 keyring）


@pytest.fixture
def fake_secrets(monkeypatch):
    store: dict[str, str] = {}

    def fake_set(entry: str, value: str) -> None:
        store[entry] = value

    def fake_get(entry: str) -> str | None:
        return store.get(entry)

    def fake_get_secret(ref, *, prompt=None, allow_prompt=True) -> str:
        if ref in store:
            return store[ref]
        env_name = "OPS_COLLECTOR_SECRET_" + ref.upper().replace("-", "_").replace(":", "_")
        import os
        v = os.environ.get(env_name)
        if v:
            return v
        raise secrets_mod.SecretError(f"凭据不可用: {ref}", code="SECRET_NOT_FOUND")

    monkeypatch.setattr(secrets_mod, "set_secret", fake_set)
    monkeypatch.setattr(secrets_mod, "_keyring_get", fake_get)
    monkeypatch.setattr(secrets_mod, "get_secret", fake_get_secret)
    monkeypatch.setattr(secrets_mod, "get_session",
                        lambda site_id: json.loads(store[f"session:{site_id}"])
                        if f"session:{site_id}" in store else None)
    monkeypatch.setattr(secrets_mod, "put_session",
                        lambda site_id, s: store.__setitem__(
                            f"session:{site_id}", json.dumps(s)))
    monkeypatch.setattr(secrets_mod, "drop_session",
                        lambda site_id: store.pop(f"session:{site_id}", None))
    return store


# ---------------------------------------------------------------------------
# new-api 站点 mock


NOW = int(time.time())
USER_ID = 42

NAPI_PASSWORD = "pw-newapi-123456"
SUB_PASSWORD = "pw-sub2api-654321"


def _napi_log(i: int, **over) -> dict:
    base = {
        "id": 1000 + i,
        "user_id": USER_ID,
        "created_at": NOW - 3600 * (i + 1),
        "type": 1,
        "username": "opuser",
        "token_name": "agent-key",
        "model_name": "gpt-4o-mini",
        "quota": 1234,
        "prompt_tokens": 500 + i,
        "completion_tokens": 200 + i,
        "use_time": 3,
        "is_stream": True,
        "channel": 7,
        "group": "default",
    }
    base.update(over)
    return base


def _napi_app(handler=None):
    """构造 new-api MockTransport。handler 可进一步自定义。"""

    def handle(request: httpx.Request) -> httpx.Response:
        path = request.url.path
        if path == "/api/user/login/encryption-key":
            return httpx.Response(200, json={"success": True, "message": "",
                                             "data": {"enabled": False}})
        if path == "/api/user/login":
            body = json.loads(request.content or b"{}")
            if body.get("username") == "opuser" and body.get("password") == NAPI_PASSWORD:
                return httpx.Response(200, json={
                    "success": True, "message": "",
                    "data": {
                        "access_token": "napi-access-token-xyz",
                        "token_type": "bearer",
                        "access_expires_at": NOW + 3600,
                        "session": {"id": "s1"},
                        "user": {"id": USER_ID, "username": "opuser", "role": 1},
                    }})
            return httpx.Response(200, json={"success": False,
                                             "message": "Username or password is incorrect, or user has been banned"})
        if path == "/api/user/self":
            return httpx.Response(200, json={
                "success": True, "message": "",
                "data": {"id": USER_ID, "username": "opuser", "role": 1,
                         "quota": 500000, "group": "default"}})
        if path == "/api/log/self":
            p = int(request.url.params.get("p", "1"))
            page_size = int(request.url.params.get("page_size", "100"))
            all_logs = [_napi_log(i) for i in range(12)] + [
                _napi_log(90, model_name="claude-sonnet-4", group="vip",
                          prompt_tokens=900, completion_tokens=700, use_time=8)]
            start = (p - 1) * page_size
            items = all_logs[start:start + page_size]
            return httpx.Response(200, json={
                "success": True, "message": "",
                "data": {"page": p, "page_size": page_size,
                         "total": len(all_logs), "items": items}})
        if path == "/api/pricing":
            return httpx.Response(200, json={
                "success": True, "message": "",
                "data": [
                    {"model_name": "gpt-4o-mini", "quota_type": 0,
                     "model_ratio": 0.75, "completion_ratio": 1.0,
                     "enable_groups": ["default", "vip"]},
                    {"model_name": "claude-sonnet-4", "quota_type": 0,
                     "model_ratio": 3.0, "completion_ratio": 5.0,
                     "enable_groups": ["default"], "group_ratio": {"vip": 0.5}},
                ],
                "vendors": [],
                "group_ratio": {"default": 1.0, "vip": 0.8},
                "usable_group": {"default": {"desc": "默认分组", "ratio": 1.0},
                                 "vip": {"desc": "VIP 分组", "ratio": 0.8}},
                "supported_endpoint": {}, "auto_groups": ["default"]})
        if path == "/api/perf-metrics/summary":
            hours = int(request.url.params.get("hours", "24"))
            scale = 1.0 if hours <= 24 else 0.8
            return httpx.Response(200, json={
                "success": True, "message": "",
                "data": {
                    "summary": {"avg_latency_ms": 1200, "success_rate": 99.2,
                                "avg_tps": 45.0},
                    "window_start": NOW - hours * 3600, "window_end": NOW,
                    "models": [
                        {"model_name": "gpt-4o-mini", "avg_latency_ms": 900,
                         "success_rate": 99.5 * scale, "avg_tps": 52.0,
                         "request_count": 12},
                        {"model_name": "claude-sonnet-4", "avg_latency_ms": 2100,
                         "success_rate": 97.0 * scale, "avg_tps": 31.0,
                         "request_count": 1},
                    ]}})
        if path == "/api/perf-metrics":
            model = request.url.params.get("model", "")
            if model == "gpt-4o-mini":
                groups = [
                    {"group": "default", "avg_ttft_ms": 420, "avg_latency_ms": 900,
                     "success_rate": 99.6, "avg_tps": 55.0, "series": []},
                    {"group": "vip", "avg_ttft_ms": 380, "avg_latency_ms": 850,
                     "success_rate": 99.9, "avg_tps": 60.0, "series": []},
                ]
            else:
                groups = [{"group": "default", "avg_ttft_ms": 640,
                           "avg_latency_ms": 2100, "success_rate": 97.0,
                           "avg_tps": 31.0, "series": []}]
            return httpx.Response(200, json={
                "success": True, "message": "",
                "data": {"model_name": model, "summary": None, "series": [],
                         "window_start": NOW - 86400, "window_end": NOW,
                         "series_schema": "v1", "groups": groups}})
        if handler:
            return handler(request)
        return httpx.Response(404, json={"success": False, "message": "not found"})

    return httpx.MockTransport(handle)


# ---------------------------------------------------------------------------
# sub2api 站点 mock

SUB_GROUP_ID = 3


def _sub_usage(i: int, **over) -> dict:
    base = {
        "id": 5000 + i,
        "request_id": f"req-{i}",
        "user_id": 7,
        "api_key_id": 11,
        "account_id": 2,
        "model": "claude-sonnet-4",
        "group_id": SUB_GROUP_ID,
        "input_tokens": 1200 + i,
        "output_tokens": 800 + i,
        "cache_read_tokens": 100,
        "cache_creation_tokens": 0,
        "total_cost": 0.021,
        "actual_cost": 0.02,
        "rate_multiplier": 1.0,
        "request_type": "chat",
        "stream": True,
        "duration_ms": 9000 + i * 100,
        "first_token_ms": 350 + i * 10,
        "created_at": NOW - 1800 * (i + 1),
        "inbound_endpoint": "/v1/messages",
    }
    base.update(over)
    return base


def _sub_app(handler=None):
    def handle(request: httpx.Request) -> httpx.Response:
        path = request.url.path
        if path == "/api/v1/auth/login":
            body = json.loads(request.content or b"{}")
            if body.get("email") == "ops@example.com" and body.get("password") == SUB_PASSWORD:
                return httpx.Response(200, json={
                    "code": 0, "message": "success",
                    "data": {
                        "access_token": "sub-jwt-token-abc",
                        "refresh_token": "sub-refresh-abc",
                        "expires_in": 86400,
                        "token_type": "Bearer",
                        "user": {"id": 7, "email": "ops@example.com",
                                 "name": "运营账号"},
                    }})
            return httpx.Response(401, json={"code": 401, "message": "invalid email or password"})
        if path == "/api/v1/user/profile":
            return httpx.Response(200, json={
                "code": 0, "message": "success",
                "data": {"id": 7, "email": "ops@example.com", "name": "运营账号",
                         "role": "user", "balance": 12.5}})
        if path == "/api/v1/usage":
            p = int(request.url.params.get("page", "1"))
            page_size = int(request.url.params.get("page_size", "100"))
            all_rows = [_sub_usage(i) for i in range(8)]
            start = (p - 1) * page_size
            items = all_rows[start:start + page_size]
            return httpx.Response(200, json={
                "code": 0, "message": "success",
                "data": {"items": items, "total": len(all_rows),
                         "page": p, "page_size": page_size,
                         "pages": (len(all_rows) + page_size - 1) // page_size}})
        if path == "/api/v1/usage/errors":
            return httpx.Response(200, json={
                "code": 0, "message": "success",
                "data": {"items": [{"id": 9001, "model": "claude-sonnet-4",
                                    "error": "upstream 500", "created_at": NOW - 600}],
                         "total": 1, "page": 1, "page_size": 100, "pages": 1}})
        if path == "/api/v1/groups/available":
            return httpx.Response(200, json={
                "code": 0, "message": "success",
                "data": [
                    {"id": SUB_GROUP_ID, "name": "claude-pro", "platform": "claude",
                     "rate_multiplier": 0.8, "status": "active",
                     "is_exclusive": False, "description": "Claude 专属分组"},
                ]})
        if path == "/api/v1/groups/rates":
            return httpx.Response(200, json={
                "code": 0, "message": "success",
                "data": {str(SUB_GROUP_ID): {"rate": 0.75, "customized": True}}})
        if handler:
            return handler(request)
        return httpx.Response(404, json={"code": 404, "message": "not found"})

    return httpx.MockTransport(handle)


# ---------------------------------------------------------------------------
# 配置


@pytest.fixture
def two_site_config(tmp_path) -> CollectorConfig:
    return CollectorConfig(
        defaults=DefaultsConfig(windows_hours=[1, 24], concurrency=2,
                                min_interval_s=0, retries=1),
        sites=[
            SiteConfig(site_id="na1", kind="new_api",
                       base_url="https://na1.test",
                       auth=AuthConfig(mode="password", username="opuser",
                                       secret_ref="na1:password")),
            SiteConfig(site_id="sb1", kind="sub2api",
                       base_url="https://sb1.test",
                       auth=AuthConfig(mode="password", email="ops@example.com",
                                       secret_ref="sb1:password")),
        ],
    )


@pytest.fixture
def seeded_passwords(fake_secrets):
    fake_secrets["na1:password"] = NAPI_PASSWORD
    fake_secrets["sb1:password"] = SUB_PASSWORD
    fake_secrets["t:password"] = NAPI_PASSWORD  # adapter 测试用的通用站点
    return fake_secrets


@pytest.fixture
def patch_run_clients(monkeypatch, two_site_config):
    """让 run.py 用 MockTransport 构造 ApiClient。"""
    import ops_collector.run as run_mod

    transports = {
        "na1": _napi_app(),
        "sb1": _sub_app(),
    }

    real_client = run_mod.ApiClient

    def factory(base_url, **kw):
        site_id = "na1" if "na1.test" in base_url else "sb1"
        return real_client(base_url, transport=transports[site_id], **kw)

    monkeypatch.setattr(run_mod, "ApiClient", factory)
    return transports
