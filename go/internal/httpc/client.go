// Package httpc: HTTP 客户端。超时、限速、有限重试（429/5xx + Retry-After 钳制）、
// 包络解析。
//
// 重试语义（agent 可编程依赖）：
//   - GET/PUT/DELETE：429/5xx/网络错误自动重试（指数退避+抖动，封顶 30s）。
//   - POST：非幂等，**永不自动重试**（避免重复建渠道等副作用）。
//   - 401/403 与其它 4xx：不重试。
//   - Retry-After 仅在 [1,120] 秒内采纳，超出按普通退避处理。
//   - 所有等待可被客户端基础 context 取消（WithBaseContext）。
package httpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	ierr "opscollector/internal/errors"
)

var retryableStatus = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true}

const (
	maxRetryAfterS  = 120      // Retry-After 超过该秒数视为不可信，按普通退避
	maxBackoffS     = 30       // 退避封顶（秒）
	maxResponseBody = 64 << 20 // 响应体解析上限 64MB
	maxRetries      = 10       // 单请求最大重试次数（防御配置失控）
)

type Options struct {
	TimeoutS     float64
	Retries      int
	MinIntervalS float64
	BackoffBaseS float64
	ExtraHeaders map[string]string
}

type Client struct {
	hc          *http.Client
	baseURL     string
	retries     int
	backoffBase float64
	minInterval time.Duration
	baseCtx     context.Context
	mu          sync.Mutex
	lastReq     time.Time
}

// New 返回 *Client（可链式 WithBaseContext 注入取消通道）。
func New(baseURL string, o Options) *Client {
	return newClient(baseURL, o)
}

func newClient(baseURL string, o Options) *Client {
	if o.TimeoutS <= 0 {
		o.TimeoutS = 20
	}
	if o.BackoffBaseS <= 0 {
		o.BackoffBaseS = 1.2
	}
	if o.Retries > maxRetries {
		o.Retries = maxRetries
	}
	if o.Retries < 0 {
		o.Retries = 0
	}
	hc := &http.Client{Timeout: time.Duration(o.TimeoutS * float64(time.Second))}
	return &Client{
		hc:          hc,
		baseURL:     strings.TrimRight(baseURL, "/"),
		retries:     o.Retries,
		backoffBase: o.BackoffBaseS,
		minInterval: time.Duration(o.MinIntervalS * float64(time.Second)),
		baseCtx:     context.Background(),
	}
}

// WithBaseContext 设置基础 context：所有后续请求与等待均可被其取消
// （watch/collect 的 Ctrl+C 贯穿通道）。
func (c *Client) WithBaseContext(ctx context.Context) *Client {
	c.baseCtx = ctx
	return c
}

func (c *Client) throttle() {
	if c.minInterval <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	wait := c.minInterval - time.Since(c.lastReq)
	if wait > 0 {
		c.sleep(wait)
	}
	c.lastReq = time.Now()
}

// sleep 可被基础 context 取消的等待。
func (c *Client) sleep(d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-c.baseCtx.Done():
	}
}

// DoJSON 执行请求并解析 JSON 响应（object 或 array）。
func (c *Client) DoJSON(method, path string, params url.Values, body any,
	headers map[string]string) (any, error) {
	return c.doJSON(c.baseCtx, method, path, params, body, headers)
}

// doJSON 核心实现。
func (c *Client) doJSON(ctx context.Context, method, path string, params url.Values,
	body any, headers map[string]string) (any, error) {
	if ctx == nil {
		ctx = c.baseCtx
	}
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, ierr.Wrap(ierr.CodeInternal, "序列化请求体失败", err)
		}
		bodyBytes = b
	}
	// POST 非幂等：服务端可能已生效但响应丢失，重试会造成重复创建
	attempts := c.retries
	if method == "POST" {
		attempts = 0
	}
	var lastErr error
	for attempt := 0; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, ierr.Wrap(ierr.CodeTransient, "已取消", err)
		}
		c.throttle()
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path,
			bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, ierr.Wrap(ierr.CodeInternal, "构造请求失败", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "ops-collector-go/0.1 (+ops-data-collection)")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		if params != nil {
			req.URL.RawQuery = params.Encode()
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ierr.Wrap(ierr.CodeTransient, "已取消", ctx.Err())
			}
			lastErr = ierr.Transientf("网络错误: %v", err)
		} else {
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				drain(resp)
				return nil, ierr.New(ierr.CodeAuthFailed,
					fmt.Sprintf("认证被拒绝 (HTTP %d)", resp.StatusCode))
			}
			if retryableStatus[resp.StatusCode] {
				delay := c.backoff(attempt)
				if ra := resp.Header.Get("Retry-After"); ra != "" {
					if sec, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil &&
						sec > 0 && sec <= maxRetryAfterS {
						delay = time.Duration(sec) * time.Second
					}
				}
				drain(resp)
				lastErr = ierr.Transientf("上游暂时不可用 (HTTP %d)", resp.StatusCode)
				if attempt < attempts {
					c.sleep(delay)
				}
				continue
			}
			if resp.StatusCode >= 400 {
				drain(resp)
				return nil, &ierr.APIError{Status: resp.StatusCode,
					Msg: fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)}
			}
			return parseJSON(resp)
		}
		if attempt < attempts {
			c.sleep(c.backoff(attempt))
		}
	}
	return nil, lastErr
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

// backoff 指数退避 + 抖动，封顶 maxBackoffS。
func (c *Client) backoff(attempt int) time.Duration {
	shift := attempt
	if shift > 16 {
		shift = 16
	}
	d := c.backoffBase * float64(uint(1)<<shift)
	d += rand.Float64() * 0.4
	if d > maxBackoffS {
		d = maxBackoffS
	}
	return time.Duration(d * float64(time.Second))
}

func (c *Client) Get(path string, params url.Values,
	headers map[string]string) (any, error) {
	return c.DoJSON("GET", path, params, nil, headers)
}

func (c *Client) Post(path string, body any, headers map[string]string) (any, error) {
	return c.DoJSON("POST", path, nil, body, headers)
}

func parseJSON(resp *http.Response) (any, error) {
	defer resp.Body.Close()
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody))
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, &ierr.APIError{Status: resp.StatusCode,
			Msg: fmt.Sprintf("响应不是合法 JSON (HTTP %d)", resp.StatusCode)}
	}
	return v, nil
}

// ---- 包络解析 ----

// UnwrapNewAPI 解 new-api 包络 {success, message, data}。
func UnwrapNewAPI(payload any, what string) (any, error) {
	m, ok := payload.(map[string]any)
	if !ok {
		return nil, &ierr.APIError{Msg: what + ": 包络不是对象"}
	}
	if m["success"] != true {
		msg := "unknown"
		if s, ok := m["message"].(string); ok && s != "" {
			msg = s
		}
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, &ierr.APIError{Msg: what + ": " + msg}
	}
	return m["data"], nil
}

// UnwrapSub2API 解 sub2api 包络 {code, message, data}。code 必须显式存在且为 0，
// 缺失或非数值一律视为包络错误（不静默猜测）。
func UnwrapSub2API(payload any, what string) (any, error) {
	m, ok := payload.(map[string]any)
	if !ok {
		return nil, &ierr.APIError{Msg: what + ": 包络不是对象"}
	}
	codeVal, present := m["code"]
	if !present {
		return nil, &ierr.APIError{Msg: what + ": 包络缺少 code 字段"}
	}
	code, isNum := codeVal.(float64)
	if !isNum {
		return nil, &ierr.APIError{Msg: what + ": 包络 code 字段不是数值"}
	}
	if code != 0 {
		msg := "unknown"
		if s, ok := m["message"].(string); ok && s != "" {
			msg = s
		}
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, &ierr.APIError{Msg: what + ": " + msg}
	}
	return m["data"], nil
}

// PaginatedItems 从分页 data 里取 (items, total)。兼容 {items,total} 与裸数组。
func PaginatedItems(data any, what string) ([]any, int64, error) {
	switch d := data.(type) {
	case map[string]any:
		items, ok := d["items"].([]any)
		if !ok {
			if sub, ok2 := d["data"].([]any); ok2 {
				items = sub
			} else {
				return nil, 0, &ierr.APIError{Msg: what + ": 分页数据缺少 items 数组"}
			}
		}
		total := float64(len(items))
		if t, ok := d["total"].(float64); ok {
			total = t
		}
		return items, int64(total), nil
	case []any:
		return d, int64(len(d)), nil
	default:
		return nil, 0, &ierr.APIError{Msg: what + ": 未知分页数据形态"}
	}
}

// AsInt / AsF64 / AsStr / AsBool：JSON any 宽松取值助手。
func AsInt(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i, true
		}
	}
	return 0, false
}

func AsF64(v any) (float64, bool) {
	if n, ok := v.(float64); ok {
		return n, true
	}
	return 0, false
}

func AsStr(v any) (string, bool) {
	if s, ok := v.(string); ok {
		return s, true
	}
	return "", false
}

func AsBool(v any) (bool, bool) {
	if b, ok := v.(bool); ok {
		return b, true
	}
	return false, false
}
