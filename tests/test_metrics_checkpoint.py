"""指标聚合与 checkpoint 测试。"""

from __future__ import annotations

import json

import pytest

from ops_collector.checkpoint import CheckpointStore
from ops_collector.metrics import aggregate, sub2api_success_rate
from ops_collector.records import CallRecord


def _rec(i: int, *, status="success", model="m1", group="g1",
         output_tokens=100, duration_ms=2000, first_token_ms=500,
         observed_at=None) -> CallRecord:
    return CallRecord(
        site_id="t", site_kind="new_api", record_id=str(i),
        observed_at=observed_at if observed_at is not None else 1000 + i,
        model=model, group=group, status=status,
        output_tokens=output_tokens, duration_ms=duration_ms,
        first_token_ms=first_token_ms, input_tokens=10,
    )


def test_aggregate_success_rate():
    recs = [_rec(0), _rec(1), _rec(2, status="error"), _rec(3, status="unknown")]
    out = aggregate(recs, site_id="t", site_kind="new_api", window_hours=24)
    assert len(out) == 1
    p = out[0]
    # success 2 / classified 3
    assert abs(p.success_rate["value"] - 66.66666) < 0.01
    assert p.success_rate["provenance"] == "derived"
    assert p.request_count == 4  # unknown 也在桶里，只是不进分母


def test_aggregate_tps_formula():
    # output=100, duration=2000ms, first_token=500ms → 100/1.5 = 66.67 tps
    recs = [_rec(0)]
    p = aggregate(recs, site_id="t", site_kind="new_api", window_hours=1)[0]
    assert abs(p.avg_output_tps["value"] - 100 / 1.5) < 0.01
    assert p.avg_output_tps["provenance"] == "derived"


def test_aggregate_ttft_mean():
    recs = [_rec(0, first_token_ms=300), _rec(1, first_token_ms=500)]
    p = aggregate(recs, site_id="t", site_kind="new_api", window_hours=1)[0]
    assert p.avg_ttft_ms["value"] == 400


def test_aggregate_groups_and_models_separate():
    recs = [_rec(0, group="g1", model="m1"), _rec(1, group="g2", model="m1"),
            _rec(2, group="g1", model="m2")]
    out = aggregate(recs, site_id="t", site_kind="new_api", window_hours=1)
    keys = {(p.group, p.model) for p in out}
    assert keys == {("g1", "m1"), ("g2", "m1"), ("g1", "m2")}


def test_aggregate_error_only_bucket():
    recs = [_rec(0, status="error", output_tokens=0, first_token_ms=None)]
    p = aggregate(recs, site_id="t", site_kind="new_api", window_hours=1)[0]
    assert p.success_rate["value"] == 0.0
    assert p.avg_output_tps["value"] is None
    assert p.avg_ttft_ms["value"] is None


def test_sub2api_success_rate_helper():
    sr = sub2api_success_rate(8, 1)
    assert abs(sr["value"] - 88.888) < 0.01
    assert sr["provenance"] == "derived"
    assert sub2api_success_rate(0, 0)["value"] is None


# ---------------------------------------------------------------------------
# checkpoint


def test_checkpoint_roundtrip(tmp_path):
    store = CheckpointStore(tmp_path / "state")
    assert store.last_log_timestamp("s1") is None
    store.save("s1", {"last_log_timestamp": 12345})
    assert store.last_log_timestamp("s1") == 12345
    data = json.loads((tmp_path / "state" / "s1.json").read_text(encoding="utf-8"))
    assert data["schema_version"] == 1


def test_checkpoint_corrupt_file_tolerated(tmp_path):
    d = tmp_path / "state"
    d.mkdir()
    (d / "s1.json").write_text("{broken", encoding="utf-8")
    store = CheckpointStore(d)
    assert store.load("s1") == {}


def test_checkpoint_chinese_site_dir(tmp_path):
    store = CheckpointStore(tmp_path / "状态")
    store.save("站点甲", {"last_log_timestamp": 9})
    assert CheckpointStore(tmp_path / "状态").last_log_timestamp("站点甲") == 9
