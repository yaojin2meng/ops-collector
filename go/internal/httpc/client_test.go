package httpc

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ierr "opscollector/internal/errors"
)

func tinyOpts(retries int) Options {
	return Options{TimeoutS: 5, Retries: retries, MinIntervalS: 0,
		BackoffBaseS: 0.001}
}

func TestRetryOn500ThenSuccess(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok": true}`))
	}))
	defer srv.Close()
	c := New(srv.URL, tinyOpts(3))
	v, err := c.Get("/api/thing", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := v.(map[string]any); !ok || m["ok"] != true {
		t.Fatalf("got %v", v)
	}
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
}

func Test401NoRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(401)
	}))
	defer srv.Close()
	c := New(srv.URL, tinyOpts(3))
	if _, err := c.Get("/api/thing", nil, nil); ierr.CodeOf(err) != ierr.CodeAuthFailed {
		t.Fatalf("期望 AUTH_FAILED，得到 %v", err)
	}
	if calls != 1 {
		t.Fatalf("401 不应重试, calls=%d", calls)
	}
}

func Test429WithRetryAfter(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	start := time.Now()
	c := New(srv.URL, tinyOpts(2))
	if _, err := c.Get("/api/thing", nil, nil); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Retry-After=0 应立即重试，耗时 %v", elapsed)
	}
}

func Test400Permanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
	}))
	defer srv.Close()
	c := New(srv.URL, tinyOpts(2))
	_, err := c.Get("/api/thing", nil, nil)
	if err == nil || ierr.CodeOf(err) != "HTTP_400" {
		t.Fatalf("期望 HTTP_400，得到 %v", err)
	}
}

func TestInvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>not json</html>"))
	}))
	defer srv.Close()
	c := New(srv.URL, tinyOpts(0))
	_, err := c.Get("/api/thing", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "不是合法 JSON") {
		t.Fatalf("期望 JSON 解析错误，得到 %v", err)
	}
}

func TestEnvelopes(t *testing.T) {
	if v, err := UnwrapNewAPI(map[string]any{"success": true, "data": 5}, "t"); err != nil || v != 5 {
		t.Fatalf("%v %v", v, err)
	}
	if _, err := UnwrapNewAPI(map[string]any{"success": false, "message": "bad"}, "t"); err == nil {
		t.Fatal("期望失败")
	}
	if v, err := UnwrapSub2API(map[string]any{"code": float64(0), "data": []any{1}}, "t"); err != nil || v == nil {
		t.Fatalf("%v %v", v, err)
	}
	if _, err := UnwrapSub2API(map[string]any{"code": float64(1), "message": "bad"}, "t"); err == nil {
		t.Fatal("期望失败")
	}
}

func TestPaginatedShapes(t *testing.T) {
	items, total, err := PaginatedItems(map[string]any{"items": []any{1, 2}, "total": float64(2)}, "t")
	if err != nil || len(items) != 2 || total != 2 {
		t.Fatalf("%v %d %v", items, total, err)
	}
	items, total, err = PaginatedItems([]any{9, 8}, "t")
	if err != nil || len(items) != 2 || total != 2 {
		t.Fatalf("%v %d %v", items, total, err)
	}
	if _, _, err := PaginatedItems(map[string]any{"nope": 1}, "t"); err == nil {
		t.Fatal("期望错误")
	}
}

func TestAsHelpers(t *testing.T) {
	if v, ok := AsInt(float64(42)); !ok || v != 42 {
		t.Fatal("AsInt")
	}
	if v, ok := AsInt("17"); !ok || v != 17 {
		t.Fatal("AsInt string")
	}
	if v, ok := AsF64(float64(1.5)); !ok || v != 1.5 {
		t.Fatal("AsF64")
	}
	if v, ok := AsBool(true); !ok || !v {
		t.Fatal("AsBool")
	}
}
