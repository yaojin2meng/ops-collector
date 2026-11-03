"""凭据存取测试（环境变量路径 + 脱敏）。"""

from __future__ import annotations

import pytest

from ops_collector import secrets as S
from ops_collector.errors import SecretError


def test_env_prefix_lookup(monkeypatch):
    monkeypatch.setenv("OPS_COLLECTOR_SECRET_MYNEWAPI_PASSWORD", "pw123")
    assert S._lookup("mynewapi:password") == "pw123"


def test_env_explicit_lookup(monkeypatch):
    monkeypatch.setenv("MY_CUSTOM_VAR", "v")
    assert S._lookup("env:MY_CUSTOM_VAR") == "v"


def test_missing_secret_raises(monkeypatch):
    monkeypatch.delenv("OPS_COLLECTOR_SECRET_NOPE", raising=False)
    with pytest.raises(SecretError, match="SECRET_NOT_FOUND|凭据不可用"):
        S.get_secret("nope", allow_prompt=False)


def test_redact():
    out = S.redact("login with pw123456 ok", ["pw123456"])
    assert "pw123456" not in out and "***" in out


def test_redact_short_values_ignored():
    assert S.redact("abc abc", ["ab"]) == "abc abc"


def test_session_entry_naming():
    assert S.session_entry("site-x") == "session:site-x"
