package httpc

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// POST 非幂等：上游 500 时绝不自动重试（防重复建渠道）。
func TestPostNeverRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	c := New(srv.URL, tinyOpts(3))
	_, err := c.Post("/api/channel/", map[string]any{"name": "x"}, nil)
	if err == nil {
		t.Fatal("期望错误")
	}
	if calls != 1 {
		t.Fatalf("POST 不应重试, calls=%d", calls)
	}
}

// Retry-After 超过钳制上限（86400s）→ 按普通退避处理，测试必须秒级完成。
func TestRetryAfterClamped(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "86400")
			w.WriteHeader(429)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	start := time.Now()
	c := New(srv.URL, Options{TimeoutS: 5, Retries: 1, MinIntervalS: 0,
		BackoffBaseS: 0.01})
	if _, err := c.Get("/api/x", nil, nil); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Retry-After 未被钳制，耗时 %v", elapsed)
	}
}

// 客户端基础 ctx 取消：等待中的退避可被打断。
func TestBaseContextCancelsSleep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	c := New(srv.URL, Options{TimeoutS: 5, Retries: 2, MinIntervalS: 0,
		BackoffBaseS: 0.01})
	c.WithBaseContext(testCancelAfter(100 * time.Millisecond))
	start := time.Now()
	_, err := c.Get("/api/x", nil, nil)
	if err == nil {
		t.Fatal("期望取消错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("ctx 取消未打断退避等待，耗时 %v", elapsed)
	}
}
