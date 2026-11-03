"""认证：账号密码登录 + 会话令牌缓存 + 访问令牌（PAT）直连。

- 会话（access token 等）缓存到 keyring，不落盘明文。
- 密码只在登录瞬间从凭据存储读取。
- 2FA / 人机校验（Turnstile 等）自动化不可用 → LoginBlockedError，建议改用 PAT。
"""

from __future__ import annotations

import base64
import time
from typing import Any, Protocol

from . import secrets
from .config import SiteConfig
from .errors import ApiError, AuthError, LoginBlockedError, TransientError
from .http import ApiClient, unwrap_new_api, unwrap_sub2api

TURNSTILE_HINTS = ("turnstile", "captcha", "人机", "验证码")
AUTH2FA_HINTS = ("2fa", "two_factor", "totp", "challenge")


class Authenticator(Protocol):
    def ensure_session(self, site: SiteConfig, client: ApiClient) -> dict:
        """返回 {auth_headers, user_id}；必要时自动登录/刷新。"""
        ...


def _looks_blocked(message) -> bool:
    low = str(message).lower()
    return any(h in low for h in TURNSTILE_HINTS)


def _looks_2fa(message) -> bool:
    low = str(message).lower()
    return any(h in low for h in AUTH2FA_HINTS)


# ---------------------------------------------------------------------------
# new-api


class NewApiAuth:
    """new-api：POST /api/user/login → Bearer access_token + New-Api-User 头。"""

    def ensure_session(self, site: SiteConfig, client: ApiClient) -> dict:
        if site.auth.mode == "api_key":
            token = secrets.get_secret(site.auth.api_key_secret_ref,
                                       prompt=f"站点 {site.site_id} 的访问令牌(PAT)")
            return {"auth_headers": _bearer(token), "user_id": None}

        cached = secrets.get_session(site.site_id)
        if cached and cached.get("access_token"):
            if cached.get("access_expires_at", 0) > time.time() + 30:
                return {"auth_headers": _bearer(cached["access_token"], cached.get("user_id")),
                        "user_id": cached.get("user_id")}

        password = secrets.get_secret(site.auth.secret_ref,
                                      prompt=f"站点 {site.site_id} 的密码")
        username = site.auth.username or ""
        body, headers = _new_api_login_payload(client, username, password)

        try:
            resp = client.post("/api/user/login", json_body=body, headers=headers)
        except ApiError as e:
            if _looks_blocked(str(e)):
                raise LoginBlockedError(
                    f"站点 {site.site_id} 登录被人机校验拦截，请在站点安全设置创建访问令牌后改用 api_key 模式"
                ) from e
            raise
        try:
            data = unwrap_new_api(resp, what="登录")
        except ApiError as e:
            # new-api 密码错误也是 HTTP 200 + success:false
            raise AuthError(f"站点 {site.site_id} 登录失败: {e}") from e
        data = unwrap_new_api(resp, what="登录")

        if isinstance(data, dict) and data.get("access_token"):
            user = data.get("user") or {}
            session = {
                "access_token": data["access_token"],
                "access_expires_at": data.get("access_expires_at", time.time() + 3600),
                "user_id": user.get("id"),
            }
            secrets.put_session(site.site_id, session)
            return {"auth_headers": _bearer(data["access_token"], user.get("id")),
                    "user_id": user.get("id")}
        # challenge / 2FA / 其他形态
        if isinstance(data, dict) and _looks_2fa(str(data)):
            raise LoginBlockedError(
                f"站点 {site.site_id} 登录需要 2FA 验证，自动化不支持；请在安全设置创建访问令牌"
            )
        raise AuthError(f"站点 {site.site_id} 登录响应缺少 access_token（可能需要 2FA）")


def _new_api_login_payload(client: ApiClient, username: str, password: str) -> tuple[dict, dict]:
    """若站点启用密码加密，则取公钥加密；否则明文。"""
    try:
        key_data = unwrap_new_api(client.get("/api/user/login/encryption-key"),
                                  what="加密密钥")
    except ApiError:
        key_data = None
    headers: dict[str, str] = {}
    if isinstance(key_data, dict) and key_data.get("enabled"):
        encrypted = _rsa_encrypt_password(password, key_data["public_key"])
        body = {"username": username, "password_encrypted": encrypted,
                "encryption_key_id": key_data["kid"]}
    else:
        body = {"username": username, "password": password}
    return body, headers


def _rsa_encrypt_password(password: str, public_key_pem: str) -> str:
    """RSA 加密：优先 OAEP-SHA256，失败回退 PKCS1v15（上游前端 padding 未锁定，
    见 docs/upstream-contracts.md）。"""
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import padding

    try:
        pub = serialization.load_pem_public_key(public_key_pem.encode())
    except Exception as e:
        raise AuthError(f"站点公钥解析失败: {type(e).__name__}") from e
    pwd = password.encode()
    for pad in (padding.OAEP(mgf=padding.MGF1(algorithm=hashes.SHA256()),
                             algorithm=hashes.SHA256(), label=None),
                padding.PKCS1v15()):
        try:
            return base64.b64encode(pub.encrypt(pwd, pad)).decode()
        except ValueError:
            continue
    raise AuthError("密码加密失败：公钥长度与方案均不匹配")


def _bearer(token: str, user_id: Any = None) -> dict[str, str]:
    headers = {"Authorization": f"Bearer {token}"}
    if user_id is not None:
        headers["New-Api-User"] = str(user_id)
    return headers


# ---------------------------------------------------------------------------
# sub2api


class Sub2ApiAuth:
    """sub2api：POST /api/v1/auth/login → JWT Bearer。"""

    def ensure_session(self, site: SiteConfig, client: ApiClient) -> dict:
        if site.auth.mode == "api_key":
            token = secrets.get_secret(site.auth.api_key_secret_ref,
                                       prompt=f"站点 {site.site_id} 的 API Key")
            return {"auth_headers": {"Authorization": f"Bearer {token}"}, "user_id": None}

        cached = secrets.get_session(site.site_id)
        if cached and cached.get("access_token"):
            # JWT 无过期字段则直接使用；401 时由上层触发刷新
            return {"auth_headers": {"Authorization": f"Bearer {cached['access_token']}"},
                    "user_id": cached.get("user_id")}

        password = secrets.get_secret(site.auth.secret_ref,
                                      prompt=f"站点 {site.site_id} 的密码")
        email = site.auth.email or site.auth.username or ""
        body: dict[str, Any] = {"email": email, "password": password}
        # 人机校验字段允许用环境变量注入（运维提前准备），不从命令行传
        turnstile = None
        for env_name, field in (("TURNSTILE_TOKEN", "turnstile_token"),
                                ("TENCENT_CAPTCHA_TICKET", "tencent_captcha_ticket"),
                                ("TENCENT_CAPTCHA_RANDSTR", "tencent_captcha_randstr")):
            v = secrets._lookup(f"env:SUB2API_{env_name}")
            if v:
                body[field] = v
                turnstile = True
        _ = turnstile

        try:
            resp = client.post("/api/v1/auth/login", json_body=body)
        except ApiError as e:
            if _looks_blocked(str(e)) and not any(k in body for k in
                                                  ("turnstile_token", "tencent_captcha_ticket")):
                raise LoginBlockedError(
                    f"站点 {site.site_id} 登录需要人机校验令牌；可设置环境变量 "
                    "SUB2API_TURNSTILE_TOKEN 或改用 API Key 模式"
                ) from e
            raise
        data = unwrap_sub2api(resp, what="登录")
        if not isinstance(data, dict) or not data.get("access_token"):
            if data and _looks_2fa(str(data)):
                raise LoginBlockedError(f"站点 {site.site_id} 登录需要 2FA，自动化不支持")
            raise AuthError(f"站点 {site.site_id} 登录响应缺少 access_token")
        session = {
            "access_token": data["access_token"],
            "refresh_token": data.get("refresh_token"),
            "user_id": (data.get("user") or {}).get("id"),
        }
        secrets.put_session(site.site_id, session)
        return {"auth_headers": {"Authorization": f"Bearer {data['access_token']}"},
                "user_id": session["user_id"]}

    def refresh(self, site: SiteConfig, client: ApiClient) -> dict:
        cached = secrets.get_session(site.site_id) or {}
        rt = cached.get("refresh_token")
        if not rt:
            raise AuthError(f"站点 {site.site_id} 无 refresh_token，请重新登录")
        resp = client.post("/api/v1/auth/refresh", json_body={"refresh_token": rt})
        data = unwrap_sub2api(resp, what="刷新会话")
        if not isinstance(data, dict) or not data.get("access_token"):
            raise AuthError(f"站点 {site.site_id} 刷新会话失败")
        cached.update({"access_token": data["access_token"],
                       "refresh_token": data.get("refresh_token", rt)})
        secrets.put_session(site.site_id, cached)
        return {"auth_headers": {"Authorization": f"Bearer {cached['access_token']}"},
                "user_id": cached.get("user_id")}


def make_authenticator(kind: str) -> Authenticator:
    if kind == "new_api":
        return NewApiAuth()
    if kind == "sub2api":
        return Sub2ApiAuth()
    raise AuthError(f"未知站点类型: {kind}")
