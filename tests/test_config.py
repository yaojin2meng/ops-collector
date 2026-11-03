"""配置解析与校验测试。"""

from __future__ import annotations

import json

import pytest

from ops_collector.config import CollectorConfig, load_config
from ops_collector.errors import ConfigError


def _write(tmp_path, data):
    p = tmp_path / "collector.json"
    p.write_text(json.dumps(data, ensure_ascii=False), encoding="utf-8")
    return p


def _base_site(**over):
    site = {
        "site_id": "s1", "kind": "new_api",
        "base_url": "https://s1.test",
        "auth": {"mode": "password", "username": "u", "secret_ref": "s1:password"},
    }
    site.update(over)
    return site


def test_valid_config_roundtrip(tmp_path):
    p = _write(tmp_path, {"schema_version": 1, "sites": [_base_site()]})
    cfg = load_config(p)
    assert cfg.sites[0].site_id == "s1"
    assert cfg.defaults.windows_hours == [1, 24, 168]


def test_duplicate_site_id_rejected(tmp_path):
    p = _write(tmp_path, {"sites": [_base_site(), _base_site()]})
    with pytest.raises(ConfigError, match="site_id 重复"):
        load_config(p)


def test_prod_http_rejected(tmp_path):
    p = _write(tmp_path, {"sites": [_base_site(base_url="http://api.example.com")]})
    with pytest.raises(ConfigError, match="HTTPS"):
        load_config(p)


def test_localhost_http_allowed(tmp_path):
    p = _write(tmp_path, {"sites": [_base_site(base_url="http://localhost:3000")]})
    cfg = load_config(p)
    assert cfg.sites[0].base_url == "http://localhost:3000"


def test_wrong_schema_version(tmp_path):
    p = _write(tmp_path, {"schema_version": 99, "sites": []})
    with pytest.raises(ConfigError, match="schema_version"):
        load_config(p)


def test_password_mode_requires_secret_ref(tmp_path):
    bad = _base_site()
    bad["auth"] = {"mode": "password", "username": "u"}
    p = _write(tmp_path, {"sites": [bad]})
    with pytest.raises(ConfigError, match="secret_ref"):
        load_config(p)


def test_api_key_mode_requires_pat_ref(tmp_path):
    ok = _base_site(auth={"mode": "api_key", "api_key_secret_ref": "s1:pat"})
    p = _write(tmp_path, {"sites": [ok]})
    cfg = load_config(p)
    assert cfg.enabled_sites()[0].auth.mode == "api_key"


def test_invalid_json(tmp_path):
    p = tmp_path / "collector.json"
    p.write_text("{not json", encoding="utf-8")
    with pytest.raises(ConfigError, match="JSON"):
        load_config(p)


def test_missing_file():
    with pytest.raises(ConfigError, match="不存在"):
        load_config("Z:/no/such/file.json")


def test_chinese_dir_path(tmp_path):
    d = tmp_path / "中文目录"
    d.mkdir()
    p = _write(d, {"sites": [_base_site()]})
    cfg = load_config(p)
    assert cfg.sites[0].kind == "new_api"


def test_site_id_pattern(tmp_path):
    p = _write(tmp_path, {"sites": [_base_site(site_id="bad id!")]})
    with pytest.raises(ConfigError):
        load_config(p)
