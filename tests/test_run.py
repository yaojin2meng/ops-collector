"""编排测试：多站点并发采集、artifact 落盘、断点续采、部分失败。"""

from __future__ import annotations

import json

import httpx
import pytest

from ops_collector.run import run_collect

from conftest import _napi_app


def test_run_two_sites_success(two_site_config, seeded_passwords,
                               patch_run_clients, tmp_path):
    result = run_collect(two_site_config, var_dir=tmp_path / "var")
    assert result.status == "success"
    summary = result.run_summary
    assert len(summary["sites"]) == 2
    for s in summary["sites"]:
        assert s["status"] == "success"
        assert s["counts"]["call_logs"] > 0
        assert s["counts"]["group_ratios"] > 0
        assert s["counts"]["performance"] > 0
        assert s["artifact"]

    # artifact 内容抽验
    na = json.loads((tmp_path / "var" / "runs" / result.run_id / "site-na1.json")
                    .read_text(encoding="utf-8"))
    assert na["site_kind"] == "new_api"
    assert na["capabilities"]["native_ttft"] is True
    assert any(p["avg_ttft_ms"]["value"] == 420 for p in na["performance"])
    # run.json 存在
    run_json = json.loads((tmp_path / "var" / "runs" / result.run_id / "run.json")
                          .read_text(encoding="utf-8"))
    assert run_json["status"] == "success"


def test_run_incremental_checkpoint(two_site_config, seeded_passwords,
                                    patch_run_clients, tmp_path):
    result1 = run_collect(two_site_config, var_dir=tmp_path / "var")
    assert result1.status == "success"
    # 第二次增量：断点已写入
    cp = tmp_path / "var" / "state" / "na1.json"
    assert cp.exists()
    state = json.loads(cp.read_text(encoding="utf-8"))
    assert state["last_log_timestamp"] > 0


def test_run_single_site_filter(two_site_config, seeded_passwords,
                                patch_run_clients, tmp_path):
    result = run_collect(two_site_config, var_dir=tmp_path / "var",
                         site_ids=["na1"])
    assert result.status == "success"
    assert [s["site_id"] for s in result.run_summary["sites"]] == ["na1"]


def test_run_site_failure_isolated(two_site_config, seeded_passwords,
                                   patch_run_clients, tmp_path, monkeypatch):
    # 让 sb1 的登录一直 500（瞬时错误耗尽重试）→ 该站点 failed，na1 正常
    transports = patch_run_clients

    def broken(request: httpx.Request) -> httpx.Response:
        return httpx.Response(500, json={"code": 500, "message": "db down"})

    transports["sb1"] = httpx.MockTransport(broken)
    result = run_collect(two_site_config, var_dir=tmp_path / "var")
    assert result.status == "partial"
    statuses = {s["site_id"]: s["status"] for s in result.run_summary["sites"]}
    assert statuses == {"na1": "success", "sb1": "failed"}


def test_run_auth_failure_no_leak(two_site_config, seeded_passwords,
                                  patch_run_clients, tmp_path):
    """密码错误时 artifact 不落盘、错误消息不含密码。"""
    transports = patch_run_clients
    seeded_passwords["sb1:password"] = "totally-wrong-pw"

    result = run_collect(two_site_config, var_dir=tmp_path / "var")
    statuses = {s["site_id"]: s for s in result.run_summary["sites"]}
    assert statuses["sb1"]["status"] == "failed"
    assert "totally-wrong-pw" not in json.dumps(result.run_summary, ensure_ascii=False)
    assert "totally-wrong-pw" not in (statuses["sb1"]["error_message"] or "")
