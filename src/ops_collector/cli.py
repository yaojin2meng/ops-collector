"""CLI：stdout 只输出机器可解析 JSON，诊断走 stderr / 日志文件。

命令：
    ops-collector init                     生成示例配置
    ops-collector validate-config          校验配置
    ops-collector secrets set/delete/hint  凭据管理
    ops-collector add-site                 添加站点（含凭据写入）
    ops-collector login-test               只测登录与站点元信息
    ops-collector collect                  采集（全量/指定站点）
    ops-collector export                   汇总最近一次 run 的指标
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from . import __version__
from .config import AuthConfig, CollectorConfig, SiteConfig, load_config
from .errors import ConfigError, OpsCollectorError
from .secrets import delete_secret, get_secret, list_entries_hint, set_secret

EXAMPLE_CONFIG = {
    "schema_version": 1,
    "defaults": {
        "windows_hours": [1, 24, 168],
        "page_size": 100,
        "max_pages_per_resource": 50,
        "concurrency": 4,
        "timeout_s": 20.0,
        "retries": 3,
        "min_interval_s": 0.4,
    },
    "sites": [
        {
            "site_id": "my-newapi",
            "name": "我的 new-api 站点",
            "kind": "new_api",
            "base_url": "https://example-newapi.example.com",
            "auth": {
                "mode": "password",
                "username": "your-username",
                "secret_ref": "my-newapi:password",
            },
            "notes": "若站点开启 Turnstile/2FA，请改用 api_key 模式（安全设置里的访问令牌）",
        },
        {
            "site_id": "my-sub2api",
            "name": "我的 sub2api 站点",
            "kind": "sub2api",
            "base_url": "https://example-sub2api.example.com",
            "auth": {
                "mode": "password",
                "email": "you@example.com",
                "secret_ref": "my-sub2api:password",
            },
        },
    ],
}


def _emit(obj) -> None:
    print(json.dumps(obj, ensure_ascii=False, indent=2))


def _fail(e: OpsCollectorError) -> int:
    _emit({"ok": False, "error_code": e.code, "message": str(e)})
    return 1


# ---------------------------------------------------------------------------
def cmd_init(args) -> int:
    path = Path(args.config)
    if path.exists() and not args.force:
        return _fail(ConfigError(f"配置已存在: {path}（--force 覆盖）", code="CONFIG_EXISTS"))
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(EXAMPLE_CONFIG, ensure_ascii=False, indent=2),
                    encoding="utf-8")
    _emit({"ok": True, "config": str(path),
           "next": "编辑配置后运行 validate-config；用 `secrets set` 写入密码"})
    return 0


def cmd_validate_config(args) -> int:
    try:
        cfg = load_config(args.config)
    except ConfigError as e:
        return _fail(e)
    _emit({"ok": True, "sites_total": len(cfg.sites),
           "sites_enabled": [s.site_id for s in cfg.enabled_sites()]})
    return 0


def cmd_secrets(args) -> int:
    if args.secrets_cmd == "set":
        import getpass
        value = getpass.getpass(f"输入 {args.entry} 的值（不回显）: ")
        if not value:
            return _fail(ConfigError("空值不写入", code="SECRET_EMPTY"))
        set_secret(args.entry, value)
        _emit({"ok": True, "entry": args.entry})
        return 0
    if args.secrets_cmd == "delete":
        ok = delete_secret(args.entry)
        _emit({"ok": ok, "entry": args.entry})
        return 0 if ok else 1
    # hint
    _emit({"ok": True, "env_entries": list_entries_hint(),
           "note": "keyring 条目无法枚举；凭据管理器中查看服务名 ops-collector"})
    return 0


def cmd_add_site(args) -> int:
    try:
        cfg = load_config(args.config)
    except ConfigError as e:
        if e.code != "CONFIG_NOT_FOUND":
            return _fail(e)
        cfg = CollectorConfig()
    auth_kwargs = ({"username": args.user} if args.kind == "new_api"
                   else {"email": args.user})
    mode = args.mode
    auth = AuthConfig(mode=mode, secret_ref=f"{args.site_id}:password" if mode == "password" else None,
                      api_key_secret_ref=f"{args.site_id}:pat" if mode == "api_key" else None,
                      **auth_kwargs)
    site = SiteConfig(site_id=args.site_id, kind=args.kind,
                      base_url=args.base_url, auth=auth)
    cfg.sites = [s for s in cfg.sites if s.site_id != args.site_id] + [site]
    Path(args.config).parent.mkdir(parents=True, exist_ok=True)
    Path(args.config).write_text(cfg.model_dump_json(indent=2), encoding="utf-8")
    _emit({"ok": True, "site_id": args.site_id, "config": args.config,
           "next": f"运行 `secrets set {args.site_id}:{'password' if mode == 'password' else 'pat'}` 写入凭据，然后 login-test"})
    return 0


def cmd_login_test(args) -> int:
    from .adapters import make_adapter
    from .http import ApiClient

    try:
        cfg = load_config(args.config)
    except ConfigError as e:
        return _fail(e)
    sites = [s for s in cfg.enabled_sites() if not args.site or s.site_id == args.site]
    if not sites:
        return _fail(ConfigError(f"未找到站点: {args.site}", code="SITE_NOT_FOUND"))
    out = []
    for site in sites:
        try:
            client = ApiClient(
                site.base_url,
                timeout_s=site.timeout_s or cfg.defaults.timeout_s,
                retries=1,
                min_interval_s=0.2,
                verify=site.verify_tls,
            )
            adapter = make_adapter(site, client)
            meta = adapter.site_meta()
            out.append({"ok": True, "site_id": site.site_id, "kind": site.kind,
                        "meta": meta})
            client.close()
        except OpsCollectorError as e:
            out.append({"ok": False, "site_id": site.site_id,
                        "error_code": e.code, "message": str(e)})
    failed = [r for r in out if not r.get("ok")]
    _emit({"ok": not failed, "results": out})
    return 1 if failed else 0


def cmd_collect(args) -> int:
    from .run import run_collect

    try:
        cfg = load_config(args.config)
    except ConfigError as e:
        return _fail(e)
    try:
        result = run_collect(cfg, site_ids=args.site or None,
                             incremental=not args.no_incremental)
    except OpsCollectorError as e:
        return _fail(e)
    _emit(result.run_summary)
    return 0 if result.status in ("success", "partial") else 1


def cmd_export(args) -> None:
    run_dir = Path(args.run_dir)
    rows = []
    for site_file in sorted(run_dir.glob("site-*.json")):
        data = json.loads(site_file.read_text(encoding="utf-8"))
        for p in data.get("performance", []):
            rows.append({
                "site_id": data.get("site_id"),
                "model": p.get("model"),
                "group": p.get("group"),
                "window_hours": p.get("window_hours"),
                "success_rate": (p.get("success_rate") or {}).get("value"),
                "avg_ttft_ms": (p.get("avg_ttft_ms") or {}).get("value"),
                "avg_output_tps": (p.get("avg_output_tps") or {}).get("value"),
                "provenance": {
                    "success_rate": (p.get("success_rate") or {}).get("provenance"),
                    "ttft": (p.get("avg_ttft_ms") or {}).get("provenance"),
                    "tps": (p.get("avg_output_tps") or {}).get("provenance"),
                },
            })
    rows.sort(key=lambda r: (r["site_id"], r["window_hours"], r["model"] or ""))
    _emit({"ok": True, "run_dir": str(run_dir), "rows": rows})


def build_parser() -> argparse.ArgumentParser:
    ap = argparse.ArgumentParser(prog="ops-collector",
                                 description="new-api/sub2api 多站点运营数据采集器")
    ap.add_argument("--version", action="version", version=f"ops-collector {__version__}")
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("init", help="生成示例配置")
    p.add_argument("--config", default="config/collector.json")
    p.add_argument("--force", action="store_true")
    p.set_defaults(func=cmd_init)

    p = sub.add_parser("validate-config", help="校验配置")
    p.add_argument("--config", default="config/collector.json")
    p.set_defaults(func=cmd_validate_config)

    p = sub.add_parser("secrets", help="凭据管理")
    p.add_argument("secrets_cmd", choices=["set", "delete", "hint"])
    p.add_argument("entry", nargs="?", help="凭据条目名，如 my-newapi:password")
    p.set_defaults(func=cmd_secrets)

    p = sub.add_parser("add-site", help="添加站点")
    p.add_argument("--config", default="config/collector.json")
    p.add_argument("--site-id", required=True)
    p.add_argument("--kind", required=True, choices=["new_api", "sub2api"])
    p.add_argument("--base-url", required=True)
    p.add_argument("--user", required=True, help="new-api 用户名 / sub2api 邮箱")
    p.add_argument("--mode", default="password", choices=["password", "api_key"])
    p.set_defaults(func=cmd_add_site)

    p = sub.add_parser("login-test", help="测试登录与站点元信息")
    p.add_argument("--config", default="config/collector.json")
    p.add_argument("--site")
    p.set_defaults(func=cmd_login_test)

    p = sub.add_parser("collect", help="采集全部/指定站点")
    p.add_argument("--config", default="config/collector.json")
    p.add_argument("--site", action="append", help="可多次指定 site_id")
    p.add_argument("--no-incremental", action="store_true",
                   help="忽略断点，从零采集")
    p.set_defaults(func=cmd_collect)

    p = sub.add_parser("export", help="汇总 run 的性能指标为扁平表")
    p.add_argument("run_dir", help="var/runs/<run_id> 目录")
    p.set_defaults(func=cmd_export)
    return ap


def main(argv=None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args) or 0
    except OpsCollectorError as e:
        return _fail(e)


if __name__ == "__main__":
    sys.exit(main())
