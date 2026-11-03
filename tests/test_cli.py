"""CLI 测试：init / validate-config / export / secrets（离线，无网络）。"""

from __future__ import annotations

import json

import pytest

from ops_collector.cli import main


def test_init_creates_config(tmp_path):
    cfg_path = tmp_path / "config" / "collector.json"
    rc = main(["init", "--config", str(cfg_path)])
    assert rc == 0
    data = json.loads(cfg_path.read_text(encoding="utf-8"))
    assert data["schema_version"] == 1
    assert len(data["sites"]) == 2
    # 第二次不覆盖
    rc2 = main(["init", "--config", str(cfg_path)])
    assert rc2 == 1
    rc3 = main(["init", "--config", str(cfg_path), "--force"])
    assert rc3 == 0


def test_validate_config_ok(tmp_path):
    cfg_path = tmp_path / "collector.json"
    assert main(["init", "--config", str(cfg_path)]) == 0
    rc = main(["validate-config", "--config", str(cfg_path)])
    assert rc == 0


def test_validate_config_bad(tmp_path):
    p = tmp_path / "bad.json"
    p.write_text(json.dumps({"schema_version": 7, "sites": []}), encoding="utf-8")
    rc = main(["validate-config", "--config", str(p)])
    assert rc == 1


def test_stdout_is_pure_json(tmp_path, capsys):
    cfg_path = tmp_path / "collector.json"
    main(["init", "--config", str(cfg_path)])
    out = capsys.readouterr().out
    # stdout 全部可解析为 JSON
    json.loads(out)


def test_export_flat_rows(tmp_path, capsys):
    run_dir = tmp_path / "run-abc"
    run_dir.mkdir()
    (run_dir / "site-na1.json").write_text(json.dumps({
        "site_id": "na1",
        "performance": [
            {"model": "gpt-4o-mini", "group": "default", "window_hours": 24,
             "success_rate": {"value": 99.5, "provenance": "native"},
             "avg_ttft_ms": {"value": 420, "provenance": "native"},
             "avg_output_tps": {"value": 55.0, "provenance": "native"}},
        ],
    }, ensure_ascii=False), encoding="utf-8")
    rc = main(["export", str(run_dir)])
    assert rc == 0
    data = json.loads(capsys.readouterr().out)
    assert data["ok"] is True
    assert data["rows"][0]["model"] == "gpt-4o-mini"
    assert data["rows"][0]["avg_ttft_ms"] == 420


def test_secrets_hint(capsys):
    rc = main(["secrets", "hint"])
    assert rc == 0
    data = json.loads(capsys.readouterr().out)
    assert data["ok"] is True
