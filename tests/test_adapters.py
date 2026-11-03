"""adapter 测试：new-api 与 sub2api 的日志/倍率/性能。"""

from __future__ import annotations

import pytest

from ops_collector import secrets as S
from ops_collector.adapters import make_adapter
from ops_collector.http import ApiClient

from conftest import NAPI_PASSWORD, SUB_PASSWORD, _napi_app, _sub_app
from test_auth import _site


# ---------------------------------------------------------------------------
# new-api


@pytest.fixture
def napi_adapter(seeded_passwords):
    site = _site("new_api")
    with ApiClient("https://t.test", transport=_napi_app(), min_interval_s=0) as c:
        yield make_adapter(site, c)


def test_napi_capabilities(napi_adapter):
    assert napi_adapter.capabilities.native_ttft is True
    assert napi_adapter.capabilities.native_success_rate is True


def test_napi_site_meta(napi_adapter):
    meta = napi_adapter.site_meta()
    assert meta["user_id"] == 42
    assert meta["username"] == "opuser"


def test_napi_logs_pagination(napi_adapter):
    records = napi_adapter.fetch_logs(max_pages=10, page_size=5)
    # mock 里共 13 条，page_size=5 → 3 页
    assert len(records) == 13
    ids = {r.record_id for r in records}
    assert len(ids) == 13
    r = records[0]
    assert r.site_kind == "new_api"
    assert r.model == "gpt-4o-mini"
    assert r.group == "default"
    assert r.input_tokens is not None and r.output_tokens is not None
    assert r.duration_ms == 3000
    # 白名单清洗：不含 content 之类（mock 数据本身无敏感字段，raw 键受限）
    assert set(r.raw.keys()) <= {
        "id", "user_id", "created_at", "type", "content", "username",
        "token_name", "model_name", "quota", "prompt_tokens",
        "completion_tokens", "use_time", "is_stream", "channel", "group"}


def test_napi_group_ratios(napi_adapter):
    ratios = napi_adapter.fetch_group_ratios()
    site_ratios = {r.group: r for r in ratios if r.scope == "site"}
    assert site_ratios["default"].ratio == 1.0
    assert site_ratios["vip"].ratio == 0.8
    assert site_ratios["vip"].description == "VIP 分组"
    model_over = [r for r in ratios if r.scope == "model"]
    assert any(r.model_name == "claude-sonnet-4" and r.group == "vip"
               and r.ratio == 0.5 for r in model_over)


def test_napi_performance_native(napi_adapter):
    perf = napi_adapter.fetch_performance(hours=24)
    by_key = {(p.model, p.group): p for p in perf}
    # 按分组原生 TTFT
    p = by_key[("gpt-4o-mini", "default")]
    assert p.avg_ttft_ms["value"] == 420
    assert p.avg_ttft_ms["provenance"] == "native"
    assert p.avg_output_tps["value"] == 55.0
    assert p.success_rate["provenance"] == "native"
    # vip 分组
    assert by_key[("gpt-4o-mini", "vip")].avg_ttft_ms["value"] == 380
    # claude 模型
    assert by_key[("claude-sonnet-4", "default")].avg_output_tps["value"] == 31.0


# ---------------------------------------------------------------------------
# sub2api


@pytest.fixture
def sub_adapter(seeded_passwords):
    seeded_passwords["t:password"] = SUB_PASSWORD  # sub2api 站点用对应密码
    site = _site("sub2api")
    with ApiClient("https://t.test", transport=_sub_app(), min_interval_s=0) as c:
        yield make_adapter(site, c)


def test_sub_capabilities(sub_adapter):
    assert sub_adapter.capabilities.native_ttft is False


def test_sub_site_meta(sub_adapter):
    meta = sub_adapter.site_meta()
    assert meta["user_id"] == 7
    assert meta["username"] == "ops@example.com"


def test_sub_logs_with_first_token(sub_adapter):
    records = sub_adapter.fetch_logs(max_pages=5, page_size=100)
    assert len(records) == 8
    r = records[0]
    assert r.site_kind == "sub2api"
    assert r.first_token_ms is not None and r.first_token_ms > 0
    assert r.status == "success"
    assert r.duration_ms and r.output_tokens


def test_sub_group_ratios(sub_adapter):
    ratios = sub_adapter.fetch_group_ratios()
    claude = [r for r in ratios if r.group == "claude-pro"]
    assert claude and claude[0].ratio == 0.8
    raw = [r for r in ratios if r.group == "_user_rates_raw"]
    assert raw and raw[0].extra["raw"]


def test_sub_performance_derived(sub_adapter):
    perf = sub_adapter.fetch_performance(hours=24)
    # 8 条 usage + 1 条 error
    assert perf
    p = perf[0]
    assert p.site_kind == "sub2api"
    assert p.avg_ttft_ms["provenance"] == "derived"
    assert p.avg_output_tps["provenance"] == "derived"
    # 成功率 = 8/(8+1) ≈ 88.89%
    assert p.success_rate["provenance"] == "derived"
    assert abs(p.success_rate["value"] - 88.88888888888889) < 0.01
    # tps 派生公式抽验：output_tokens / ((duration_ms - first_token_ms)/1000)
    # 第一条 usage：800 / ((9000-350)/1000) ≈ 92.49 tps，应出现在聚合结果里
    expected = 800 / ((9000 - 350) / 1000)
    vals = [m.avg_output_tps["value"] for m in perf if m.avg_output_tps["value"]]
    assert vals and any(abs(v - expected) < 20 for v in vals)
