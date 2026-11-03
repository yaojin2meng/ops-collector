"""认证流程测试（mock 传输 + 内存凭据）。"""

from __future__ import annotations

import json

import httpx
import pytest

from ops_collector import secrets as S
from ops_collector.auth import NewApiAuth, Sub2ApiAuth
from ops_collector.config import AuthConfig, SiteConfig
from ops_collector.errors import AuthError, LoginBlockedError
from ops_collector.http import ApiClient

from conftest import NAPI_PASSWORD, SUB_PASSWORD, _napi_app, _sub_app


def _site(kind: str, **auth_over) -> SiteConfig:
    auth = {"mode": "password", "secret_ref": "t:password"}
    if kind == "new_api":
        auth["username"] = "opuser"
    else:
        auth["email"] = "ops@example.com"
    auth.update(auth_over)
    return SiteConfig(site_id="t1", kind=kind, base_url="https://t.test",
                      auth=AuthConfig(**auth))


def test_new_api_login_success_and_session_cache(fake_secrets):
    fake_secrets["t:password"] = NAPI_PASSWORD
    site = _site("new_api")
    with ApiClient("https://t.test", transport=_napi_app(), min_interval_s=0) as c:
        ctx = NewApiAuth().ensure_session(site, c)
        assert ctx["auth_headers"]["Authorization"] == "Bearer napi-access-token-xyz"
        assert ctx["auth_headers"]["New-Api-User"] == "42"
        # 第二次走缓存，不再登录
        ctx2 = NewApiAuth().ensure_session(site, c)
        assert ctx2["auth_headers"] == ctx["auth_headers"]
        assert json.loads(fake_secrets["session:t1"])["user_id"] == 42


def test_new_api_wrong_password(fake_secrets):
    fake_secrets["t:password"] = "wrong-password"
    site = _site("new_api")
    with ApiClient("https://t.test", transport=_napi_app(), min_interval_s=0) as c:
        with pytest.raises(AuthError):
            NewApiAuth().ensure_session(site, c)


def test_sub2api_login_success(fake_secrets):
    fake_secrets["t:password"] = SUB_PASSWORD
    site = _site("sub2api")
    with ApiClient("https://t.test", transport=_sub_app(), min_interval_s=0) as c:
        ctx = Sub2ApiAuth().ensure_session(site, c)
        assert ctx["auth_headers"]["Authorization"] == "Bearer sub-jwt-token-abc"
        assert ctx["user_id"] == 7


def test_sub2api_refresh_flow(fake_secrets):
    fake_secrets["t:password"] = SUB_PASSWORD
    site = _site("sub2api")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/v1/auth/refresh":
            body = json.loads(request.content or b"{}")
            assert body.get("refresh_token") == "sub-refresh-abc"
            return httpx.Response(200, json={"code": 0, "message": "success",
                                             "data": {"access_token": "new-jwt",
                                                      "refresh_token": "new-rt"}})
        return _sub_app().handler(request)

    with ApiClient("https://t.test", transport=httpx.MockTransport(handler),
                   min_interval_s=0) as c:
        auth = Sub2ApiAuth()
        auth.ensure_session(site, c)  # 先登录
        ctx = auth.refresh(site, c)
        assert ctx["auth_headers"]["Authorization"] == "Bearer new-jwt"
        assert json.loads(fake_secrets["session:t1"])["access_token"] == "new-jwt"


def test_turnstile_blocked_message():
    msg = "请完成 turnstile 人机校验"
    assert NewApiAuth and LoginBlockedError  # smoke
    from ops_collector.auth import _looks_blocked
    assert _looks_blocked(msg)
    assert not _looks_blocked("ok")


def test_2fa_challenge_detected():
    from ops_collector.auth import _looks_2fa
    assert _looks_2fa({"challenge": "totp_required"})
    assert not _looks_2fa({"access_token": "x"})


def test_api_key_mode_no_login(fake_secrets):
    fake_secrets["t:pat"] = "pat-token-1"
    site = _site("new_api", mode="api_key", secret_ref=None,
                 api_key_secret_ref="t:pat")
    with ApiClient("https://t.test", transport=_napi_app(), min_interval_s=0) as c:
        ctx = NewApiAuth().ensure_session(site, c)
        assert ctx["auth_headers"] == {"Authorization": "Bearer pat-token-1"}
