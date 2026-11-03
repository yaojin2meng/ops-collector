// Package errors 定义统一错误类型与稳定错误码。
//
// 设计约束：
//   - 认证类错误不重试（密码错、令牌失效、人机校验拦截）。
//   - 网络瞬断 / 429 / 5xx 属 Transient，由 httpc 重试。
//   - 错误消息不得包含凭据或完整响应体。
package errors

import (
	"errors"
	"fmt"
)

// 错误码（稳定，供运营 agent 判断）。
const (
	CodeInternal        = "INTERNAL_ERROR"
	CodeUsage           = "USAGE" // 命令行用法错误（退出码 64）
	CodeConfigInvalid   = "CONFIG_INVALID"
	CodeConfigNotFound  = "CONFIG_NOT_FOUND"
	CodeConfigExists    = "CONFIG_EXISTS"
	CodeSiteNotFound    = "SITE_NOT_FOUND"
	CodeSiteDisabled    = "SITE_DISABLED"
	CodeSecret          = "SECRET_UNAVAILABLE"
	CodeSecretNotFound  = "SECRET_NOT_FOUND"
	CodeAuthFailed      = "AUTH_FAILED"
	CodeLoginBlocked    = "LOGIN_BLOCKED"
	CodeTransient       = "TRANSIENT"
	CodeAPIError        = "API_ERROR"
	CodeUnsupported     = "UNSUPPORTED"
	CodeAdminRequired   = "ADMIN_REQUIRED"
	CodeConfirmRequired = "CONFIRM_REQUIRED"
	CodeAddRespUnknown  = "ADD_RESPONSE_UNKNOWN"
	CodePanic           = "PANIC"
)

// OpsError 是采集器统一错误。
type OpsError struct {
	Code string
	Msg  string
	Err  error
}

func (e *OpsError) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *OpsError) Unwrap() error { return e.Err }

func New(code, msg string) *OpsError { return &OpsError{Code: code, Msg: msg} }

func Wrap(code, msg string, err error) *OpsError {
	return &OpsError{Code: code, Msg: msg, Err: err}
}

func Configf(format string, a ...any) *OpsError {
	return &OpsError{Code: CodeConfigInvalid, Msg: fmt.Sprintf(format, a...)}
}

func Transientf(format string, a ...any) *OpsError {
	return &OpsError{Code: CodeTransient, Msg: fmt.Sprintf(format, a...)}
}

// APIError 是上游永久性错误（4xx 等），不重试。
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

func (e *APIError) OpsCode() string {
	if e.Status > 0 {
		return fmt.Sprintf("HTTP_%d", e.Status)
	}
	return CodeAPIError
}

// CodeOf 提取任意错误的稳定错误码。
func CodeOf(err error) string {
	var oe *OpsError
	if errors.As(err, &oe) {
		return oe.Code
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.OpsCode()
	}
	return CodeInternal
}
