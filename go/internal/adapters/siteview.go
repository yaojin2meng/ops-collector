package adapters

import (
	"opscollector/internal/config"
)

// siteView 实现 auth.SiteAuthInfo，把 *config.SiteConfig 适配成最小视图。
type siteView struct{ s *config.SiteConfig }

func (v siteView) GetSiteID() string          { return v.s.SiteID }
func (v siteView) GetKind() string            { return v.s.Kind }
func (v siteView) GetAuthMode() string        { return v.s.Auth.Mode }
func (v siteView) GetUsername() string        { return v.s.Auth.Username }
func (v siteView) GetEmail() string           { return v.s.Auth.Email }
func (v siteView) GetSecretRef() string       { return v.s.Auth.SecretRef }
func (v siteView) GetAPIKeySecretRef() string { return v.s.Auth.APIKeySecretRef }
