"""凭据存取：Windows 凭据管理器（keyring）优先，环境变量兜底。

规则：
- 密码/令牌绝不写进配置文件、命令行参数、URL、日志或 artifact。
- secret_ref 命名约定：
    * 形如 ``keyring:<entry>``  → keyring 服务名 ``ops-collector``，用户名 ``<entry>``
    * 形如 ``env:<NAME>``      → 环境变量 ``NAME``
    * 其他                     → 先按环境变量 ``OPS_COLLECTOR_SECRET_<REF大写>`` 找，
      再按 keyring 条目 ``<REF>`` 找。
- 交互式补录：stdin 为 TTY 时用 getpass 提示输入，可选存入 keyring。
"""

from __future__ import annotations

import getpass
import os
import sys
from pathlib import Path

from .errors import SecretError

KEYRING_SERVICE = "ops-collector"
ENV_PREFIX = "OPS_COLLECTOR_SECRET_"


def _keyring():
    try:
        import keyring
    except Exception as e:  # pragma: no cover - 平台相关
        raise SecretError(f"keyring 不可用: {type(e).__name__}") from e
    try:
        # 触发一次后端探测，失败则降级
        keyring.get_keyring()
        return keyring
    except Exception as e:
        raise SecretError(f"keyring 后端不可用: {type(e).__name__}") from e


def get_secret(ref: str | None, *, prompt: str | None = None,
               allow_prompt: bool = True) -> str:
    """按约定解析 secret_ref 并返回值。找不到时（允许交互时）提示输入。"""
    if not ref:
        raise SecretError("secret_ref 为空")
    value = _lookup(ref)
    if value:
        return value
    if allow_prompt and prompt and sys.stdin.isatty():
        value = getpass.getpass(f"{prompt}（输入不会回显）: ")
        if value:
            return value
    raise SecretError(f"凭据不可用: {ref}（可运行 `ops-collector secrets set` 写入凭据管理器）",
                      code="SECRET_NOT_FOUND")


def _lookup(ref: str) -> str | None:
    if ref.startswith("keyring:"):
        entry = ref.split(":", 1)[1]
        return _keyring_get(entry)
    if ref.startswith("env:"):
        name = ref.split(":", 1)[1]
        return os.environ.get(name)
    env_name = ENV_PREFIX + ref.upper().replace("-", "_").replace(":", "_")
    v = os.environ.get(env_name)
    if v:
        return v
    return _keyring_get(ref)


def _keyring_get(entry: str) -> str | None:
    kr = _keyring()
    try:
        v = kr.get_password(KEYRING_SERVICE, entry)
    except Exception as e:
        raise SecretError(f"读取凭据管理器失败: {type(e).__name__}") from e
    return v or None


def set_secret(entry: str, value: str) -> None:
    kr = _keyring()
    try:
        kr.set_password(KEYRING_SERVICE, entry, value)
    except Exception as e:
        raise SecretError(f"写入凭据管理器失败: {type(e).__name__}") from e


def delete_secret(entry: str) -> bool:
    kr = _keyring()
    try:
        kr.delete_password(KEYRING_SERVICE, entry)
        return True
    except Exception:
        return False


def list_entries_hint() -> list[str]:
    """keyring 无法枚举条目，只返回环境变量提示。"""
    return sorted(
        k[len(ENV_PREFIX):].lower() for k in os.environ if k.startswith(ENV_PREFIX)
    )


def session_entry(site_id: str) -> str:
    """会话 token 在 keyring 中的条目名。"""
    return f"session:{site_id}"


# 会话令牌的存取（复用同一 keyring 服务）
def get_session(site_id: str) -> dict | None:
    import json
    raw = _keyring_get(session_entry(site_id))
    if not raw:
        return None
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        return None


def put_session(site_id: str, session: dict) -> None:
    import json
    set_secret(session_entry(site_id), json.dumps(session, ensure_ascii=False))


def drop_session(site_id: str) -> None:
    delete_secret(session_entry(site_id))


def redact(text: str, secrets: list[str]) -> str:
    """把文本中出现的任何凭据值替换为 ***。用于日志与错误消息兜底。"""
    out = text
    for s in secrets:
        if s and len(s) >= 4:
            out = out.replace(s, "***")
    return out
