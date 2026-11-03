package auth

import (
	ierr "opscollector/internal/errors"
	"opscollector/internal/httpc"
	"opscollector/internal/secrets"
)

// WithSelfHeal 包装一次受 401 自愈保护的请求（收敛此前四份复制实现）：
//
//  1. EnsureSession 取会话；
//  2. 执行 do(headers)；
//  3. 若 401/403（AUTH_FAILED）：sub2api 先试 refresh，失败则清会话重新登录，
//     然后用新会话再执行一次 do。
//
// api_key（PAT）模式没有会话可清，自愈降级为直接重试一次
// （令牌被吊销时错误会如实上抛）。
func WithSelfHeal(site SiteAuthInfo, client *httpc.Client, a Authenticator,
	do func(headers map[string]string) (any, error)) (any, error) {
	ctx, err := a.EnsureSession(site, client)
	if err != nil {
		return nil, err
	}
	v, err := do(ctx.Headers)
	if err == nil || ierr.CodeOf(err) != ierr.CodeAuthFailed {
		return v, err
	}
	if refresher, ok := a.(interface {
		Refresh(SiteAuthInfo, *httpc.Client) (AuthCtx, error)
	}); ok {
		if ctx2, rerr := refresher.Refresh(site, client); rerr == nil {
			return do(ctx2.Headers)
		}
	}
	if site.GetAuthMode() == "api_key" {
		// PAT 无会话缓存可清，重试无意义，如实上抛
		return nil, err
	}
	secrets.DropSession(site.GetSiteID())
	ctx3, err3 := a.EnsureSession(site, client)
	if err3 != nil {
		return nil, err3
	}
	return do(ctx3.Headers)
}
