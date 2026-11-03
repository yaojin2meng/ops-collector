// Package testmock: 测试用 mock 站点服务器（复刻 Python 版 fixtures）。
package testmock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	NAPIPassword = "pw-newapi-123456"
	SUBPassword  = "pw-sub2api-654321"
	NAPIToken    = "napi-access-token-xyz"
	SUBToken     = "sub-jwt-token-abc"
	SUBRefresh   = "sub-refresh-abc"
	UserID       = 42
	SUBUserID    = 7
)

var Now = time.Now().Unix()

// ---- new-api 渠道（管理面）可变状态（有锁：httptest handler 并发） ----

var (
	napiChannelsMu    sync.RWMutex
	napiChannelsState       = map[int64]map[string]any{}
	napiChannelsSeq   int64 = 100
	// NapiValidTokens 管理面接受的 Bearer 令牌（测试可注入失效场景）
	NapiValidTokens = map[string]bool{NAPIToken: true, "pat-token-1": true}
)

func napiNextChannelID() int64 {
	napiChannelsMu.Lock()
	defer napiChannelsMu.Unlock()
	napiChannelsSeq++
	return napiChannelsSeq
}

// ResetNapiChannels 重置渠道状态（每个测试独立）。
func ResetNapiChannels() {
	napiChannelsMu.Lock()
	defer napiChannelsMu.Unlock()
	napiChannelsState = map[int64]map[string]any{
		1: {"id": 1, "type": 1, "status": 1, "name": "openai-main",
			"models": "gpt-4o-mini,gpt-4o", "group": "default",
			"response_time": 320, "test_time": Now - 600,
			"used_quota": 500000, "balance": 12.5, "priority": 0,
			"weight": 0, "base_url": "https://api.openai.com",
			"key": "sk-SECRET-should-not-leak"},
		2: {"id": 2, "type": 14, "status": 3, "name": "claude-backup",
			"models": "claude-sonnet-4", "group": "default,vip",
			"response_time": 4800, "test_time": Now - 1200,
			"used_quota": 1234567, "balance": 3.2, "priority": 5,
			"weight": 10, "key": "sk-SECRET-2"},
	}
}

// napiChannelsSnapshot 返回按 id 排序的渠道切片（copy，避免并发写泄漏）。
func napiChannelsSnapshot() []any {
	napiChannelsMu.RLock()
	defer napiChannelsMu.RUnlock()
	out := []any{}
	ids := []int64{}
	for id := range napiChannelsState {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		cp := map[string]any{}
		for k, v := range napiChannelsState[id] {
			cp[k] = v
		}
		out = append(out, cp)
	}
	return out
}

// napiAuthed 校验管理面 Bearer 令牌。
func napiAuthed(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	return strings.HasPrefix(h, "Bearer ") &&
		NapiValidTokens[strings.TrimPrefix(h, "Bearer ")]
}

// napiRequireAuth 写回 401 并返回 false。
func napiRequireAuth(w http.ResponseWriter, r *http.Request) bool {
	if napiAuthed(r) {
		return true
	}
	w.WriteHeader(401)
	writeJSON(w, map[string]any{"success": false,
		"message": "Unauthorized, not logged in and no access token provided"})
	return false
}

func napiLog(i int, over map[string]any) map[string]any {
	m := map[string]any{
		"id": 1000 + i, "user_id": UserID, "created_at": Now - 3600*int64(i+1),
		"type": 1, "username": "opuser", "token_name": "agent-key",
		"model_name": "gpt-4o-mini", "quota": 1234,
		"prompt_tokens": 500 + i, "completion_tokens": 200 + i,
		"use_time": 3, "is_stream": true, "channel": 1, "group": "default",
	}
	for k, v := range over {
		m[k] = v
	}
	return m
}

func subUsage(i int, over map[string]any) map[string]any {
	m := map[string]any{
		"id": 5000 + i, "request_id": fmt.Sprintf("req-%d", i),
		"user_id": SUBUserID, "api_key_id": 11, "account_id": 2,
		"model": "claude-sonnet-4", "group_id": 3,
		"input_tokens": 1200 + i, "output_tokens": 800 + i,
		"cache_read_tokens": 100, "cache_creation_tokens": 0,
		// 成本拆分随 token 线性缩放：input 单价恒为 $2.5/1M，output 恒为 $15/1M
		"input_cost":  float64(1200+i) * 2.5 / 1e6,
		"output_cost": float64(800+i) * 15.0 / 1e6,
		"total_cost":  0.021, "actual_cost": 0.02, "rate_multiplier": 1.0,
		"request_type": "chat", "stream": true,
		"duration_ms": 9000 + i*100, "first_token_ms": 350 + i*10,
		"created_at":       Now - 1800*int64(i+1),
		"inbound_endpoint": "/v1/messages",
	}
	for k, v := range over {
		m[k] = v
	}
	return m
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// NewApiSite 返回 new-api mock 站点。
func NewApiSite() *httptest.Server {
	ResetNapiChannels()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/user/login/encryption-key", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"success": true, "message": "",
			"data": map[string]any{"enabled": false}})
	})
	mux.HandleFunc("/api/user/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Username == "opuser" && body.Password == NAPIPassword {
			writeJSON(w, map[string]any{"success": true, "message": "",
				"data": map[string]any{
					"access_token": NAPIToken, "token_type": "bearer",
					"access_expires_at": Now + 3600,
					"session":           map[string]any{"id": "s1"},
					"user": map[string]any{"id": UserID, "username": "opuser",
						"role": 1},
				}})
			return
		}
		writeJSON(w, map[string]any{"success": false,
			"message": "Username or password is incorrect, or user has been banned"})
	})
	mux.HandleFunc("/api/user/self", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"success": true, "message": "",
			"data": map[string]any{"id": UserID, "username": "opuser",
				"role": 1, "quota": 500000, "group": "default"}})
	})
	mux.HandleFunc("/api/log/self", func(w http.ResponseWriter, r *http.Request) {
		p, _ := strconv.Atoi(r.URL.Query().Get("p"))
		if p < 1 {
			p = 1
		}
		ps, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
		if ps < 1 {
			ps = 100
		}
		all := []map[string]any{}
		for i := 0; i < 12; i++ {
			all = append(all, napiLog(i, nil))
		}
		all = append(all, napiLog(90, map[string]any{
			"model_name": "claude-sonnet-4", "group": "vip", "channel": 2,
			"prompt_tokens": 900, "completion_tokens": 700, "use_time": 8}))
		// 混入一条充值记录（type=2）：实现应过滤掉，不能混进调用记录
		all = append(all, napiLog(95, map[string]any{
			"type": 2, "model_name": "", "quota": -100000,
			"prompt_tokens": 0, "completion_tokens": 0, "use_time": 0}))
		// start_timestamp 过滤（增量语义）
		if st := r.URL.Query().Get("start_timestamp"); st != "" {
			if ts, err := strconv.ParseInt(st, 10, 64); err == nil {
				f := all[:0]
				for _, l := range all {
					if l["created_at"].(int64) >= ts {
						f = append(f, l)
					}
				}
				all = f
			}
		}
		start := (p - 1) * ps
		end := start + ps
		if start > len(all) {
			start = len(all)
		}
		if end > len(all) {
			end = len(all)
		}
		writeJSON(w, map[string]any{"success": true, "message": "",
			"data": map[string]any{"page": p, "page_size": ps,
				"total": len(all), "items": all[start:end]}})
	})
	mux.HandleFunc("/api/pricing", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"success": true, "message": "",
			"data": []any{
				map[string]any{"model_name": "gpt-4o-mini", "quota_type": 0,
					"model_ratio": 0.75, "completion_ratio": 1.0,
					"cache_ratio": 0.5, "create_cache_ratio": 1.25,
					"enable_groups": []any{"default", "vip"}},
				map[string]any{"model_name": "claude-sonnet-4", "quota_type": 0,
					"model_ratio": 3.0, "completion_ratio": 5.0,
					"cache_ratio":   0.1,
					"enable_groups": []any{"default"},
					"group_ratio":   map[string]any{"vip": 0.5}},
				map[string]any{"model_name": "sora-video", "quota_type": 1,
					"model_price":   0.5,
					"enable_groups": []any{"default"}},
			},
			"vendors":            []any{},
			"group_ratio":        map[string]any{"default": 1.0, "vip": 0.8},
			"usable_group":       map[string]any{"default": map[string]any{"desc": "默认分组", "ratio": 1.0}, "vip": map[string]any{"desc": "VIP 分组", "ratio": 0.8}},
			"supported_endpoint": map[string]any{},
			"auto_groups":        []any{"default"},
		})
	})
	mux.HandleFunc("/api/ratio_config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"success": true, "message": "",
			"data": map[string]any{
				"model_ratio":      map[string]any{"gpt-4o-mini": 0.75, "claude-sonnet-4": 3.0},
				"completion_ratio": map[string]any{"gpt-4o-mini": 1.0, "claude-sonnet-4": 5.0},
				"model_price":      map[string]any{"sora-video": 0.5},
			}})
	})
	// ---- 管理面：渠道 CRUD（校验 Bearer；401 触发自愈路径） ----
	mux.HandleFunc("/api/channel/", func(w http.ResponseWriter, r *http.Request) {
		if !napiRequireAuth(w, r) {
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/channel/":
			// 分页切片：忠实模拟上游分页（护栏：翻页循环 0 覆盖的教训）
			items := napiChannelsSnapshot()
			p, _ := strconv.Atoi(r.URL.Query().Get("p"))
			if p < 1 {
				p = 1
			}
			ps, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
			if ps < 1 {
				ps = 100
			}
			start := (p - 1) * ps
			end := start + ps
			if start > len(items) {
				start = len(items)
			}
			if end > len(items) {
				end = len(items)
			}
			writeJSON(w, map[string]any{"success": true, "message": "",
				"data": map[string]any{"items": items[start:end], "total": len(items),
					"page": p, "page_size": ps, "type_counts": map[string]any{}}})
		case r.Method == "POST" && r.URL.Path == "/api/channel/":
			var body struct {
				Channel map[string]any `json:"channel"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id := napiNextChannelID()
			body.Channel["id"] = id
			body.Channel["status"] = 1
			napiChannelsMu.Lock()
			napiChannelsState[id] = body.Channel
			napiChannelsMu.Unlock()
			writeJSON(w, map[string]any{"success": true, "message": "",
				"data": map[string]any{"id": id}})
		default:
			rest := strings.TrimPrefix(r.URL.Path, "/api/channel/")
			if i := strings.Index(rest, "/"); i >= 0 {
				idStr, action := rest[:i], rest[i+1:]
				// 真实路由：GET /api/channel/test/:id（test 在前）
				if idStr == "test" && action != "" {
					writeJSON(w, map[string]any{"success": true, "message": "",
						"data": map[string]any{"time": 0.35, "message": ""}})
					return
				}
				id, _ := strconv.ParseInt(idStr, 10, 64)
				switch action {
				case "status":
					var body struct {
						Status int64 `json:"status"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					napiChannelsMu.Lock()
					ch, ok := napiChannelsState[id]
					if ok {
						ch["status"] = body.Status
					}
					napiChannelsMu.Unlock()
					if ok {
						writeJSON(w, map[string]any{"success": true, "message": "", "data": true})
						return
					}
					writeJSON(w, map[string]any{"success": false, "message": "channel not found"})
				default:
					writeJSON(w, map[string]any{"success": false, "message": "not found"})
				}
				return
			}
			id, _ := strconv.ParseInt(rest, 10, 64)
			switch r.Method {
			case "GET":
				napiChannelsMu.RLock()
				ch, ok := napiChannelsState[id]
				var cp map[string]any
				if ok {
					cp = map[string]any{}
					for k, v := range ch {
						cp[k] = v
					}
				}
				napiChannelsMu.RUnlock()
				if ok {
					writeJSON(w, map[string]any{"success": true, "message": "", "data": cp})
					return
				}
				writeJSON(w, map[string]any{"success": false, "message": "channel not found"})
			case "DELETE":
				napiChannelsMu.Lock()
				_, ok := napiChannelsState[id]
				if ok {
					delete(napiChannelsState, id)
				}
				napiChannelsMu.Unlock()
				if ok {
					writeJSON(w, map[string]any{"success": true, "message": "", "data": true})
					return
				}
				writeJSON(w, map[string]any{"success": false, "message": "channel not found"})
			case "PUT":
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				napiChannelsMu.Lock()
				ch, ok := napiChannelsState[id]
				if ok {
					for k, v := range body {
						ch[k] = v
					}
				}
				napiChannelsMu.Unlock()
				if ok {
					writeJSON(w, map[string]any{"success": true, "message": "", "data": true})
					return
				}
				writeJSON(w, map[string]any{"success": false, "message": "channel not found"})
			default:
				writeJSON(w, map[string]any{"success": false, "message": "method not allowed"})
			}
		}
	})
	mux.HandleFunc("/api/perf-metrics/summary", func(w http.ResponseWriter, r *http.Request) {
		hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))
		scale := 1.0
		if hours > 24 {
			scale = 0.8
		}
		writeJSON(w, map[string]any{"success": true, "message": "",
			"data": map[string]any{
				"summary":      map[string]any{"avg_latency_ms": 1200, "success_rate": 99.2, "avg_tps": 45.0},
				"window_start": Now - int64(hours)*3600, "window_end": Now,
				"models": []any{
					map[string]any{"model_name": "gpt-4o-mini",
						"avg_latency_ms": 900, "success_rate": 99.5 * scale,
						"avg_tps": 52.0, "request_count": 12},
					map[string]any{"model_name": "claude-sonnet-4",
						"avg_latency_ms": 2100, "success_rate": 97.0 * scale,
						"avg_tps": 31.0, "request_count": 1},
				}}})
	})
	mux.HandleFunc("/api/perf-metrics", func(w http.ResponseWriter, r *http.Request) {
		model := r.URL.Query().Get("model")
		groups := []any{
			map[string]any{"group": "default", "avg_ttft_ms": 640,
				"avg_latency_ms": 2100, "success_rate": 97.0,
				"avg_tps": 31.0, "series": []any{}},
		}
		if model == "gpt-4o-mini" {
			groups = []any{
				map[string]any{"group": "default", "avg_ttft_ms": 420,
					"avg_latency_ms": 900, "success_rate": 99.6,
					"avg_tps": 55.0, "series": []any{}},
				map[string]any{"group": "vip", "avg_ttft_ms": 380,
					"avg_latency_ms": 850, "success_rate": 99.9,
					"avg_tps": 60.0, "series": []any{}},
			}
		}
		writeJSON(w, map[string]any{"success": true, "message": "",
			"data": map[string]any{"model_name": model, "summary": nil,
				"series": []any{}, "window_start": Now - 86400,
				"window_end": Now, "series_schema": "v1", "groups": groups}})
	})
	return httptest.NewServer(mux)
}

// Sub2ApiSite 返回 sub2api mock 站点。broken=true 时登录接口 500。
func Sub2ApiSite(broken bool) *httptest.Server {
	ResetSub2ApiChannels()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if broken {
			w.WriteHeader(500)
			writeJSON(w, map[string]any{"code": 500, "message": "db down"})
			return
		}
		var body struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Email == "ops@example.com" && body.Password == SUBPassword {
			writeJSON(w, map[string]any{"code": 0, "message": "success",
				"data": map[string]any{
					"access_token": SUBToken, "refresh_token": SUBRefresh,
					"expires_in": 86400, "token_type": "Bearer",
					"user": map[string]any{"id": SUBUserID,
						"email": "ops@example.com", "name": "运营账号"},
				}})
			return
		}
		w.WriteHeader(401)
		writeJSON(w, map[string]any{"code": 401, "message": "invalid email or password"})
	})
	mux.HandleFunc("/api/v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.RefreshToken == SUBRefresh {
			writeJSON(w, map[string]any{"code": 0, "message": "success",
				"data": map[string]any{"access_token": SUBToken,
					"refresh_token": "new-rt"}})
			return
		}
		w.WriteHeader(401)
		writeJSON(w, map[string]any{"code": 401, "message": "invalid refresh token"})
	})
	mux.HandleFunc("/api/v1/user/profile", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "message": "success",
			"data": map[string]any{"id": SUBUserID, "email": "ops@example.com",
				"name": "运营账号", "role": "user", "balance": 12.5}})
	})
	mux.HandleFunc("/api/v1/usage", func(w http.ResponseWriter, r *http.Request) {
		p, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if p < 1 {
			p = 1
		}
		ps, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
		if ps < 1 {
			ps = 100
		}
		all := []map[string]any{}
		for i := 0; i < 8; i++ {
			all = append(all, subUsage(i, nil))
		}
		start := (p - 1) * ps
		end := start + ps
		if start > len(all) {
			start = len(all)
		}
		if end > len(all) {
			end = len(all)
		}
		writeJSON(w, map[string]any{"code": 0, "message": "success",
			"data": map[string]any{"items": all[start:end], "total": len(all),
				"page": p, "page_size": ps,
				"pages": (len(all) + ps - 1) / ps}})
	})
	mux.HandleFunc("/api/v1/usage/errors", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "message": "success",
			"data": map[string]any{"items": []any{map[string]any{
				"id": 9001, "model": "claude-sonnet-4", "group_id": 3,
				"error": "upstream 500", "created_at": Now - 600}},
				"total": 1, "page": 1, "page_size": 100, "pages": 1}})
	})
	mux.HandleFunc("/api/v1/groups/available", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "message": "success",
			"data": []any{map[string]any{"id": 3, "name": "claude-pro",
				"platform": "claude", "rate_multiplier": 0.8,
				"status": "active", "is_exclusive": false,
				"description": "Claude 专属分组"}}})
	})
	mux.HandleFunc("/api/v1/groups/rates", func(w http.ResponseWriter, r *http.Request) {
		// 契约（frontend/src/api/groups.ts）：Record<group_id, rate_multiplier>
		writeJSON(w, map[string]any{"code": 0, "message": "success",
			"data": map[string]any{"3": 0.75}})
	})
	// ---- 管理面：渠道 CRUD ----
	mux.HandleFunc("/api/v1/admin/channels", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			writeJSON(w, map[string]any{"code": 0, "message": "success",
				"data": map[string]any{"items": subChannelsSnapshot(), "total": len(subChannelsState),
					"page": 1, "page_size": 100, "pages": 1}})
		case "POST":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			id := subNextChannelID()
			body["id"] = id
			if _, ok := body["status"].(string); !ok {
				body["status"] = "active"
			}
			subChannelsState[id] = body
			writeJSON(w, map[string]any{"code": 0, "message": "success",
				"data": subChannelsState[id]})
		default:
			w.WriteHeader(405)
		}
	})
	mux.HandleFunc("/api/v1/admin/channels/", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/api/v1/admin/channels/"), 10, 64)
		switch r.Method {
		case "GET":
			if ch, ok := subChannelsState[id]; ok {
				writeJSON(w, map[string]any{"code": 0, "message": "success", "data": ch})
				return
			}
			w.WriteHeader(404)
			writeJSON(w, map[string]any{"code": 404, "message": "channel not found"})
		case "PUT":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if ch, ok := subChannelsState[id]; ok {
				for k, v := range body {
					ch[k] = v
				}
				writeJSON(w, map[string]any{"code": 0, "message": "success", "data": ch})
				return
			}
			w.WriteHeader(404)
			writeJSON(w, map[string]any{"code": 404, "message": "channel not found"})
		case "DELETE":
			delete(subChannelsState, id)
			writeJSON(w, map[string]any{"code": 0, "message": "success", "data": true})
		default:
			w.WriteHeader(405)
		}
	})
	return httptest.NewServer(mux)
}

// ---- sub2api 渠道（管理面）可变状态 ----

var subChannelsState = map[int64]map[string]any{}
var subChannelsSeq int64 = 50

func subNextChannelID() int64 {
	subChannelsSeq++
	return subChannelsSeq
}

// ResetSub2ApiChannels 重置渠道状态（每个测试独立）。
func ResetSub2ApiChannels() {
	subChannelsState = map[int64]map[string]any{
		11: {"id": 11, "name": "claude-channel-1", "description": "主力",
			"status": "active", "group_ids": []any{3},
			"model_pricing": []any{map[string]any{
				"platform": "claude", "models": []any{"claude-sonnet-4"},
				"billing_mode": "token", "input_price": 3.0, "output_price": 15.0,
			}}},
		12: {"id": 12, "name": "claude-channel-2", "status": "disabled",
			"group_ids": []any{3},
			"model_pricing": []any{map[string]any{
				"platform": "claude", "models": []any{"claude-opus-4"},
				"billing_mode": "token", "input_price": 15.0, "output_price": 75.0,
			}}},
	}
}

func subChannelsSnapshot() []any {
	out := []any{}
	ids := []int64{}
	for id := range subChannelsState {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		out = append(out, subChannelsState[id])
	}
	return out
}
