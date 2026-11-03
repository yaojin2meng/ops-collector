"""统一错误类型。

设计约束：
- 认证类错误不重试（密码错、令牌失效且无法刷新、人机校验拦截）。
- 网络瞬断 / 429 / 5xx 属 TransientError，由 http 层重试。
- 所有错误消息都不得包含凭据或完整响应体。
"""

from __future__ import annotations


class OpsCollectorError(Exception):
    """基础错误。code 是稳定的机器可读错误码，供运营 agent 判断。"""

    code = "INTERNAL_ERROR"

    def __init__(self, message: str, *, code: str | None = None):
        super().__init__(message)
        if code:
            self.code = code


class ConfigError(OpsCollectorError):
    code = "CONFIG_INVALID"


class SecretError(OpsCollectorError):
    code = "SECRET_UNAVAILABLE"


class AuthError(OpsCollectorError):
    """认证失败：不重试。"""

    code = "AUTH_FAILED"


class LoginBlockedError(AuthError):
    """站点开启了人机校验 / 2FA / 关闭密码登录，自动化登录被阻断。

    提示运营者改用站点"安全设置"里创建的访问令牌（PAT）。
    """

    code = "LOGIN_BLOCKED"


class TransientError(OpsCollectorError):
    """瞬时错误（超时、5xx、429），可重试。"""

    code = "TRANSIENT"


class ApiError(OpsCollectorError):
    """上游返回永久性错误（4xx 等），不重试。"""

    code = "API_ERROR"

    def __init__(self, message: str, *, status: int | None = None, code: str | None = None):
        self.status = status
        super().__init__(message, code=code or (f"HTTP_{status}" if status else None))


class UnsupportedError(OpsCollectorError):
    """站点/版本不支持某项数据。"""

    code = "UNSUPPORTED"
