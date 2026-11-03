"""版本化多站点配置：解析与校验（pydantic v2）。

凭据不写进配置文件；配置里只放 secret_ref（凭据管理器条目名或环境变量名）。
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Literal

from pydantic import BaseModel, Field, field_validator, model_validator

from .errors import ConfigError

CONFIG_SCHEMA_VERSION = 1
AuthMode = Literal["password", "api_key"]
SiteKind = Literal["new_api", "sub2api"]


class AuthConfig(BaseModel):
    mode: AuthMode = "password"
    # password 模式：new-api 用 username，sub2api 用 email
    username: str | None = None
    email: str | None = None
    # 指向凭据的引用：keyring 条目名或环境变量名（OPS_COLLECTOR_ 前缀约定见 secrets.py）
    secret_ref: str | None = None
    # api_key 模式：站点安全设置里创建的访问令牌
    api_key_secret_ref: str | None = None

    @model_validator(mode="after")
    def _check(self) -> "AuthConfig":
        if self.mode == "password":
            if not self.secret_ref:
                raise ValueError("password 模式必须提供 secret_ref")
        else:
            if not self.api_key_secret_ref:
                raise ValueError("api_key 模式必须提供 api_key_secret_ref")
        return self


class SiteConfig(BaseModel):
    site_id: str = Field(min_length=1, max_length=64, pattern=r"^[A-Za-z0-9_-]+$")
    name: str | None = None
    kind: SiteKind
    base_url: str
    auth: AuthConfig
    enabled: bool = True
    verify_tls: bool = True
    # 站点级覆盖（秒）
    timeout_s: float | None = Field(default=None, gt=0, le=300)
    min_interval_s: float | None = Field(default=None, ge=0)
    notes: str | None = None

    @field_validator("base_url")
    @classmethod
    def _check_base_url(cls, v: str) -> str:
        v = v.rstrip("/")
        if not (v.startswith("https://") or v.startswith("http://")):
            raise ValueError("base_url 必须以 http(s):// 开头")
        if v.startswith("http://"):
            # 仅显式本地/内网地址允许 http
            host = v.split("://", 1)[1].split("/", 1)[0].split(":", 1)[0]
            if host not in ("localhost", "127.0.0.1", "::1") and not host.startswith("192.168.") \
                    and not host.startswith("10.") and not host.endswith(".local"):
                raise ValueError(
                    "生产站点必须使用 HTTPS；如确为可信内网，请在 host 使用 "
                    "localhost/192.168.x/10.x/.local 形式"
                )
        return v


class DefaultsConfig(BaseModel):
    windows_hours: list[int] = Field(default_factory=lambda: [1, 24, 168])
    page_size: int = Field(default=100, ge=1, le=100)
    max_pages_per_resource: int = Field(default=50, ge=1)
    concurrency: int = Field(default=4, ge=1, le=32)
    timeout_s: float = Field(default=20.0, gt=0, le=300)
    retries: int = Field(default=3, ge=0, le=10)
    min_interval_s: float = Field(default=0.4, ge=0)
    backoff_base_s: float = Field(default=1.2, gt=0)


class CollectorConfig(BaseModel):
    schema_version: int = CONFIG_SCHEMA_VERSION
    defaults: DefaultsConfig = Field(default_factory=DefaultsConfig)
    sites: list[SiteConfig] = Field(default_factory=list)

    @field_validator("schema_version")
    @classmethod
    def _check_version(cls, v: int) -> int:
        if v != CONFIG_SCHEMA_VERSION:
            raise ValueError(f"不支持的配置 schema_version={v}，当前支持 {CONFIG_SCHEMA_VERSION}")
        return v

    @model_validator(mode="after")
    def _check_sites(self) -> "CollectorConfig":
        ids = [s.site_id for s in self.sites]
        dup = {i for i in ids if ids.count(i) > 1}
        if dup:
            raise ValueError(f"site_id 重复: {sorted(dup)}")
        return self

    def enabled_sites(self) -> list[SiteConfig]:
        return [s for s in self.sites if s.enabled]


def load_config(path: str | Path) -> CollectorConfig:
    p = Path(path)
    if not p.exists():
        raise ConfigError(f"配置文件不存在: {p}", code="CONFIG_NOT_FOUND")
    try:
        raw = json.loads(p.read_text(encoding="utf-8"))
    except json.JSONDecodeError as e:
        raise ConfigError(f"配置不是合法 JSON: {p}: 行{e.lineno}列{e.colno}") from e
    try:
        return CollectorConfig.model_validate(raw)
    except ValueError as e:
        raise ConfigError(f"配置校验失败: {e}") from e
