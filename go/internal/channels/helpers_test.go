package channels

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"opscollector/internal/config"
	ierr "opscollector/internal/errors"
	"opscollector/internal/logging"
	"opscollector/internal/records"
)

type config_Site = config.SiteConfig

const records_ChannelEnabled = records.ChannelEnabled

func writeJSONTest(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// patAuth 把站点认证切换为 api_key（PAT）模式（凭据条目 <siteID>:pat）。
func patAuth(siteID string) config.AuthConfig {
	return config.AuthConfig{Mode: "api_key", APIKeySecretRef: siteID + ":pat"}
}

func wallNow() time.Time { return time.Now() }

func wallSince(t time.Time) time.Duration { return time.Since(t) }

const milli = time.Millisecond

// stubMgr 固定返回预置渠道列表的 Manager（diff 逻辑测试用）。
type stubMgr struct{ chs []records.Channel }

func (m stubMgr) Kind() string { return "stub" }
func (m stubMgr) List(opts ListOptions) ([]records.Channel, int64, error) {
	return m.chs, int64(len(m.chs)), nil
}
func (m stubMgr) Get(id int64) (*records.Channel, error) {
	for i := range m.chs {
		if m.chs[i].ChannelID == id {
			return &m.chs[i], nil
		}
	}
	return nil, ierr.New(ierr.CodeAPIError, "not found")
}
func (m stubMgr) Add(body map[string]any) (*records.Channel, error) {
	return nil, ierr.New(ierr.CodeUnsupported, "stub")
}
func (m stubMgr) Update(body map[string]any) (*records.Channel, error) {
	return nil, ierr.New(ierr.CodeUnsupported, "stub")
}
func (m stubMgr) Delete(id int64) error { return nil }
func (m stubMgr) SetStatus(id int64, status records.ChannelStatus) error {
	for i := range m.chs {
		if m.chs[i].ChannelID == id {
			m.chs[i].Status = status
			return nil
		}
	}
	return nil
}
func (m stubMgr) Test(id int64, model string) (map[string]any, error) {
	return map[string]any{"time": 0.1}, nil
}

var _ = url.Values{}

func silentLogger() *logging.Logger {
	return logging.New("", "", false)
}

func timeAfter(ms int) <-chan time.Time {
	return time.After(time.Duration(ms) * time.Millisecond)
}

func osStat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

var _ = testing.Short
