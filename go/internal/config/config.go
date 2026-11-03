// Package config: 版本化多站点配置（与 Python 版 schema 完全兼容）。
//
// 凭据不写进配置文件；配置里只放 secret_ref（凭据管理器条目名或环境变量名）。
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	ierr "opscollector/internal/errors"
)

const SchemaVersion = 1

var siteIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type AuthConfig struct {
	Mode            string `json:"mode"` // password | api_key
	Username        string `json:"username,omitempty"`
	Email           string `json:"email,omitempty"`
	SecretRef       string `json:"secret_ref,omitempty"`
	APIKeySecretRef string `json:"api_key_secret_ref,omitempty"`
}

type SiteConfig struct {
	SiteID  string     `json:"site_id"`
	Name    string     `json:"name,omitempty"`
	Kind    string     `json:"kind"` // new_api | sub2api
	BaseURL string     `json:"base_url"`
	Auth    AuthConfig `json:"auth"`
	// Enabled 缺省视为 true（指针以区分显式 false）
	Enabled *bool `json:"enabled,omitempty"`
	// Admin 显式开启管理员面能力（渠道增删/状态管理）。
	// 仅当运营者对该站点拥有管理员账号时才允许 true。
	Admin        *bool    `json:"admin,omitempty"`
	VerifyTLS    *bool    `json:"verify_tls,omitempty"`
	TimeoutS     float64  `json:"timeout_s,omitempty"`
	MinIntervalS *float64 `json:"min_interval_s,omitempty"`
	Notes        string   `json:"notes,omitempty"`
}

func (s *SiteConfig) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

func (s *SiteConfig) AdminEnabled() bool {
	return s.Admin != nil && *s.Admin
}

func (s *SiteConfig) TLSEnabled() bool {
	return s.VerifyTLS == nil || *s.VerifyTLS
}

type DefaultsConfig struct {
	WindowsHours        []int   `json:"windows_hours"`
	PageSize            int     `json:"page_size"`
	MaxPagesPerResource int     `json:"max_pages_per_resource"`
	Concurrency         int     `json:"concurrency"`
	TimeoutS            float64 `json:"timeout_s"`
	Retries             int     `json:"retries"`
	// MinIntervalS 用指针区分"显式 0"（不限速）与"未设置"（默认 0.4s）
	MinIntervalS *float64 `json:"min_interval_s,omitempty"`
	BackoffBaseS float64  `json:"backoff_base_s"`
}

// Normalize 填充零值默认（Load 与直接构造 Config 的场景都应调用）。
func (d *DefaultsConfig) Normalize() {
	if len(d.WindowsHours) == 0 {
		d.WindowsHours = []int{1, 24, 168}
	}
	if d.PageSize == 0 {
		d.PageSize = 100
	}
	if d.MaxPagesPerResource == 0 {
		d.MaxPagesPerResource = 50
	}
	if d.Concurrency == 0 {
		d.Concurrency = 4
	}
	if d.TimeoutS == 0 {
		d.TimeoutS = 20
	}
	if d.Retries == 0 {
		d.Retries = 3
	}
	if d.MinIntervalS == nil {
		v := 0.4
		d.MinIntervalS = &v
	}
	if d.BackoffBaseS == 0 {
		d.BackoffBaseS = 1.2
	}
}

type Config struct {
	SchemaVersion int            `json:"schema_version"`
	Defaults      DefaultsConfig `json:"defaults"`
	Sites         []*SiteConfig  `json:"sites"`
}

// Load 读取并校验配置。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ierr.New(ierr.CodeConfigNotFound, "配置文件不存在: "+path)
		}
		return nil, ierr.Wrap(ierr.CodeConfigInvalid, "读取配置失败", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, ierr.Wrap(ierr.CodeConfigInvalid, "配置不是合法 JSON", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate 校验配置（在 Defaults 填充默认值之后调用）。
func (c *Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return ierr.Configf("不支持的配置 schema_version=%d，当前支持 %d", c.SchemaVersion, SchemaVersion)
	}
	c.Defaults.Normalize()
	seen := map[string]bool{}
	for _, s := range c.Sites {
		if !siteIDRe.MatchString(s.SiteID) {
			return ierr.Configf("site_id 不合法: %q（仅字母数字_-，≤64字符）", s.SiteID)
		}
		if seen[s.SiteID] {
			return ierr.Configf("site_id 重复: %s", s.SiteID)
		}
		seen[s.SiteID] = true
		if s.Kind != "new_api" && s.Kind != "sub2api" {
			return ierr.Configf("站点 %s: kind 必须是 new_api 或 sub2api", s.SiteID)
		}
		if err := validateBaseURL(s.SiteID, s.BaseURL); err != nil {
			return err
		}
		if s.Auth.Mode == "" {
			s.Auth.Mode = "password"
		}
		switch s.Auth.Mode {
		case "password":
			if s.Auth.SecretRef == "" {
				return ierr.Configf("站点 %s: password 模式必须提供 secret_ref", s.SiteID)
			}
		case "api_key":
			if s.Auth.APIKeySecretRef == "" {
				return ierr.Configf("站点 %s: api_key 模式必须提供 api_key_secret_ref", s.SiteID)
			}
		default:
			return ierr.Configf("站点 %s: auth.mode 必须是 password 或 api_key", s.SiteID)
		}
	}
	return nil
}

func validateBaseURL(siteID, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ierr.Configf("站点 %s: base_url 必须以 http(s):// 开头", siteID)
	}
	if u.Scheme == "https" {
		return nil
	}
	// HTTP 仅放行本机/可信内网：按解析出的 IP 判定（防 10.evil.com 式字符串前缀绕过）
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil && (ip.IsPrivate() || ip.IsLoopback()) {
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return nil
	}
	return ierr.Configf("站点 %s: 生产站点必须使用 HTTPS；如确为可信内网，"+
		"host 需为本机或私网 IP/localhost/.local 形式", siteID)
}

// EnabledSites 返回启用站点。
func (c *Config) EnabledSites() []*SiteConfig {
	out := make([]*SiteConfig, 0, len(c.Sites))
	for _, s := range c.Sites {
		if s.IsEnabled() {
			out = append(out, s)
		}
	}
	return out
}

// Save 序列化写回（UTF-8、两空格缩进、不转义 HTML）。
func (c *Config) Save(path string) error {
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return ierr.Wrap(ierr.CodeConfigInvalid, "创建配置目录失败", err)
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(c); err != nil {
		return ierr.Wrap(ierr.CodeConfigInvalid, "序列化配置失败", err)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return ierr.Wrap(ierr.CodeConfigInvalid, "写入配置失败", err)
	}
	return nil
}

func dirOf(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[:i]
	}
	return "."
}

var _ = fmt.Sprintf // keep fmt import even if unused in future edits
