package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"prism-gateway/internal/api"
	"prism-gateway/internal/config"
	"prism-gateway/internal/prism"
	"prism-gateway/internal/usage"
)

// ============================================================================
// 端到端测试：用 httptest 起一个「假 prism 上游」，跑真实的网关栈。
// 覆盖：预热链、start/poll、SSE 合成、工具仿真、图片通道、缓存计数、高并发单飞。
// ============================================================================

type mockUpstream struct {
	t          *testing.T
	srv        *httptest.Server
	pendingFor int // 前 N 次 status 返回 pending
	text       string
	reasoning  string

	mu          sync.Mutex
	backendNew  int
	starts      int
	polls       int
	lastStart   map[string]any
	lastHeaders http.Header
	lastStatus  map[string]any
}

func newMockUpstream(t *testing.T, pendingFor int, text, reasoning string) *mockUpstream {
	m := &mockUpstream{t: t, pendingFor: pendingFor, text: text, reasoning: reasoning}
	mux := http.NewServeMux()

	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/api/backend/1/new", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.backendNew++
		m.mu.Unlock()
		writeJSON(w, map[string]any{"url": m.srv.URL + "/s/sandboxes/proxy/", "token": "sbx-token-abc"})
	})
	mux.HandleFunc("/api/projects/proj-1/sandbox/resources-token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"access_token": "resource-token"})
	})
	mux.HandleFunc("/s/sandboxes/proxy/resources-token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("/api/y", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"url": "y-url", "baseUrl": "y-base", "token": "y-token", "authorization": "y-auth"})
	})
	mux.HandleFunc("/s/sandboxes/proxy/token", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("/s/sandboxes/proxy/wait-for-sync", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"status": "synced"})
	})
	mux.HandleFunc("/s/sandboxes/proxy/heartbeat", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("/api/codex/conversation-history", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("/auth/session", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"user": map[string]any{"id": "user-1", "email": "test@example.com"}})
	})
	mux.HandleFunc("/auth/entitlements", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"planType": "pro"})
	})
	mux.HandleFunc("/api/file-management/projects", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"projects": []map[string]any{{"id": "proj-1", "name": "t"}}})
	})
	mux.HandleFunc("/api/llm/response_with_tools_start", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.starts++
		m.lastStart = body
		m.lastHeaders = r.Header.Clone()
		m.mu.Unlock()
		writeJSON(w, map[string]any{
			"status": "started", "request_id": "req-1", "conversation_id": "conv-1",
			"turn_state": map[string]any{"seq": 1},
		})
	})
	mux.HandleFunc("/api/llm/response_with_tools_status", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.polls++
		n := m.polls
		m.lastStatus = body
		m.mu.Unlock()
		if n <= m.pendingFor {
			writeJSON(w, map[string]any{
				"status": "pending", "turn_state": map[string]any{"seq": n + 1},
			})
			return
		}
		writeJSON(w, map[string]any{
			"status": "completed",
			"response": map[string]any{
				"status": "completed",
				"payload": map[string]any{
					"output": []map[string]any{
						{"type": "reasoning", "summary": []map[string]any{{"type": "summary_text", "text": m.reasoning}}},
						{"type": "message", "role": "assistant", "content": []map[string]any{{"type": "output_text", "text": m.text}}},
					},
				},
			},
		})
	})
	m.srv = httptest.NewServer(mux)
	return m
}

func (m *mockUpstream) close() { m.srv.Close() }

func (m *mockUpstream) counts() (backendNew, starts, polls int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.backendNew, m.starts, m.polls
}

// newGateway 用假上游起一个完整网关。
func newGateway(t *testing.T, up *mockUpstream, keys []string, warm bool) *api.Server {
	cfg := config.Default()
	cfg.Origin = up.srv.URL
	cfg.APIKeys = keys
	cfg.WarmOnStart = warm
	cfg.ChunkSize = 8
	cfg.ChunkInterval = time.Millisecond
	cfg.KeepAliveEvery = time.Second
	cfg.PollCallTimeout = 5 * time.Second
	cfg.PollBudget = 20 * time.Second
	cfg.StartTimeout = 10 * time.Second

	acct := &prism.Account{ID: "acct-1", Cookie: "oai-sc=1; prism_session_token=2; prism_oai_access_token=3; cf_clearance=4; __cf_bm=5", UserAgent: "test-agent"}
	tc := prism.DefaultTransportConfig()
	pool := prism.NewPool([]*prism.Account{acct}, prism.PoolOptions{
		Client: prism.Options{
			Origin: cfg.Origin, UserAgent: cfg.UserAgent,
			StartTimeout: cfg.StartTimeout, PollCallTimeout: cfg.PollCallTimeout,
			PollBudget: cfg.PollBudget, SyncPollTries: 2, StartAttempts: 2, MaxReconnects: 1,
			Transport: tc,
		},
		PerAccountConc: 32, WarmOnStart: warm, PrewarmWorkers: 1,
	})
	t.Cleanup(pool.Close)

	est := usage.DefaultEstimator()
	return api.New(api.Deps{
		Config: cfg, Pool: pool, Catalog: prism.DefaultModels(),
		Cache: usage.NewTracker(time.Minute, 128, est), Counters: usage.NewCounters(),
	})
}

// ------------------------------------------------------------------ 测试

// TestE2E_NonStream 验证非流式闭环 + usage 计数 + 预热链只走一次。
func TestE2E_NonStream(t *testing.T) {
	up := newMockUpstream(t, 1, "你好，我是网关。", "先想一下。")
	defer up.close()
	srv := newGateway(t, up, nil, false)

	// 预热链必须完整走一次
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	poolClients := srv // 通过 HTTP 触发更真实
	_ = poolClients

	body := `{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"你好"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if resp["object"] != "chat.completion" {
		t.Fatalf("object 应为 chat.completion，得到 %v", resp["object"])
	}
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好，我是网关。" {
		t.Fatalf("正文不匹配: %v", msg["content"])
	}
	if msg["reasoning_content"] != "先想一下。" {
		t.Fatalf("思考摘要不匹配: %v", msg["reasoning_content"])
	}
	u := resp["usage"].(map[string]any)
	if u["prompt_tokens"].(float64) <= 0 || u["completion_tokens"].(float64) <= 0 {
		t.Fatalf("usage 计数异常: %v", u)
	}
	t.Logf("usage: prompt=%v completion=%v total=%v cached=%v",
		u["prompt_tokens"], u["completion_tokens"], u["total_tokens"],
		u["prompt_tokens_details"].(map[string]any)["cached_tokens"])

	bn, st, pl := up.counts()
	t.Logf("上游调用: backendNew=%d starts=%d polls=%d", bn, st, pl)
	if bn != 1 {
		t.Fatalf("建沙箱应只 1 次，实际 %d", bn)
	}
	_ = ctx
}

// TestE2E_Stream 验证 SSE 合成：角色帧 → 思考增量 → 正文增量 → finish → [DONE]。
func TestE2E_Stream(t *testing.T) {
	up := newMockUpstream(t, 2, "流式正文内容一二三四五六七八九十", "思考中")
	defer up.close()
	srv := newGateway(t, up, nil, false)

	body := `{"model":"gpt-5.6-sol","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应为 SSE，得到 %q", ct)
	}
	raw := rec.Body.String()
	for _, want := range []string{
		": prism-gateway",        // 握手注释
		`"role":"assistant"`,     // 首帧
		"reasoning_content",      // 思考增量
		"content",                // 正文增量
		`"finish_reason":"stop"`, // 结束帧
		"usage",                  // include_usage
		"data: [DONE]",           // 终止符
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("SSE 缺少 %q", want)
		}
	}
	// 增量应被切成多块（ChunkSize=8）。
	n := strings.Count(raw, `"content":"`)
	if n < 2 {
		t.Errorf("正文应被切成多块推送，实际含 content 的帧数=%d", n)
	}
	if !strings.Contains(raw, ": pending poll=") {
		t.Errorf("pending 期间应有心跳注释")
	}
	t.Logf("SSE 帧数=%d，正文块数=%d，字节=%d", strings.Count(raw, "data: "), n, len(raw))
}

// TestE2E_ToolEmulation 验证工具仿真：文本里的 <tool_call> 被解析成 tool_calls。
func TestE2E_ToolEmulation(t *testing.T) {
	text := "我来读一下文件。\n<tool_call>{\"name\":\"read_file\",\"arguments\":{\"path\":\"README.md\"}}</tool_call>"
	up := newMockUpstream(t, 0, text, "")
	defer up.close()
	srv := newGateway(t, up, nil, false)

	body := `{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"看下 README"}],
	  "tools":[{"type":"function","function":{"name":"read_file","description":"读文件",
	  "parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	choices := resp["choices"].([]any)
	ch := choices[0].(map[string]any)
	msg := ch["message"].(map[string]any)

	if ch["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason 应为 tool_calls，得到 %v", ch["finish_reason"])
	}
	tcs, ok := msg["tool_calls"].([]any)
	if !ok || len(tcs) == 0 {
		t.Fatalf("应解析出 tool_calls，得到 %v", msg["tool_calls"])
	}
	tc := tcs[0].(map[string]any)["function"].(map[string]any)
	if tc["name"] != "read_file" {
		t.Fatalf("工具名应为 read_file，得到 %v", tc["name"])
	}
	args := tc["arguments"].(string)
	if !strings.Contains(args, "README.md") {
		t.Fatalf("参数应含 README.md，得到 %v", args)
	}
	// 正文里不应残留信封
	if strings.Contains(msg["content"].(string), "<tool_call>") {
		t.Fatalf("正文应已剥掉 tool_call 块")
	}

	// 工具协议应被注入到上游 input（上游不采信 system，故并入 user）
	up.mu.Lock()
	start := up.lastStart
	up.mu.Unlock()
	inputJSON, _ := json.Marshal(start["input"])
	if !strings.Contains(string(inputJSON), "[工具协议]") {
		t.Fatalf("上游 input 应含工具协议说明")
	}
	if !strings.Contains(string(inputJSON), "read_file") {
		t.Fatalf("上游 input 应含工具名")
	}
}

// TestE2E_ImageChannel 验证图片通道：data URL → base64 还原命令进入 input。
func TestE2E_ImageChannel(t *testing.T) {
	up := newMockUpstream(t, 0, "我看到图了。", "")
	defer up.close()
	srv := newGateway(t, up, nil, false)

	// 1x1 PNG
	pngB64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8AAAwAB/gGZ0QAAAAAASUVORK5CYII="
	body := fmt.Sprintf(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":[
	  {"type":"text","text":"这张图是什么"},
	  {"type":"image_url","image_url":{"url":"data:image/png;base64,%s"}}]}]}`, pngB64)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	up.mu.Lock()
	inputJSON, _ := json.Marshal(up.lastStart["input"])
	up.mu.Unlock()
	s := string(inputJSON)
	for _, want := range []string{"[图片附件 1]", "base64 -d", "prism-uploads/", "view_image"} {
		if !strings.Contains(s, want) {
			t.Errorf("图片通道缺少 %q\ninput=%s", want, truncate(s, 1200))
		}
	}
}

// TestE2E_HighConcurrency 高并发：64 并发只付一次冷链（singleflight），全部成功。
func TestE2E_HighConcurrency(t *testing.T) {
	up := newMockUpstream(t, 1, "并发回复", "")
	defer up.close()
	srv := newGateway(t, up, nil, false)

	const n = 64
	var wg sync.WaitGroup
	var ok, fail int64
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"并发 %d"}]}`, i)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				atomic.AddInt64(&ok, 1)
			} else {
				atomic.AddInt64(&fail, 1)
				t.Logf("请求 %d 失败: %d %s", i, rec.Code, truncate(rec.Body.String(), 200))
			}
		}(i)
	}
	wg.Wait()
	dur := time.Since(start)

	bn, st, _ := up.counts()
	t.Logf("并发 %d：成功=%d 失败=%d 耗时=%s | 上游 backendNew=%d starts=%d", n, ok, fail, dur.Round(time.Millisecond), bn, st)
	if fail > 0 {
		t.Fatalf("有 %d 个请求失败", fail)
	}
	if bn != 1 {
		t.Fatalf("高并发下建沙箱应被单飞合并为 1 次，实际 %d 次", bn)
	}
	if ok != n {
		t.Fatalf("应全部成功，实际 %d/%d", ok, n)
	}
}

// TestE2E_Auth 验证客户端 key 鉴权。
func TestE2E_Auth(t *testing.T) {
	up := newMockUpstream(t, 0, "ok", "")
	defer up.close()
	srv := newGateway(t, up, []string{"sk-test-key"}, false)

	call := func(auth string) int {
		body := `{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := call(""); code != http.StatusUnauthorized {
		t.Fatalf("无 key 应 401，得到 %d", code)
	}
	if code := call("Bearer wrong"); code != http.StatusUnauthorized {
		t.Fatalf("错误 key 应 401，得到 %d", code)
	}
	if code := call("Bearer sk-test-key"); code != http.StatusOK {
		t.Fatalf("正确 key 应 200，得到 %d", code)
	}
	// x-api-key 形态（Anthropic SDK）
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gpt-5.6-sol","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "sk-test-key")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("x-api-key 应通过，得到 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestE2E_Anthropic 验证 Anthropic 协议（含 tool_use 流）。
func TestE2E_Anthropic(t *testing.T) {
	text := "查一下天气。<tool_call>{\"name\":\"get_weather\",\"arguments\":{\"city\":\"杭州\"}}</tool_call>"
	up := newMockUpstream(t, 0, text, "推理中")
	defer up.close()
	srv := newGateway(t, up, nil, false)

	body := `{"model":"gpt-5.6-sol","stream":true,"max_tokens":1024,
	  "system":"你是助手","messages":[{"role":"user","content":[{"type":"text","text":"杭州天气"}]}],
	  "tools":[{"name":"get_weather","description":"查天气","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	for _, want := range []string{
		"event: message_start", "event: content_block_start", "thinking_delta",
		"text_delta", "tool_use", "input_json_delta", "event: message_delta", "event: message_stop",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("Anthropic SSE 缺少 %q", want)
		}
	}
	if !strings.Contains(raw, `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason 应为 tool_use")
	}
}

// TestE2E_Responses 验证 Responses 协议。
func TestE2E_Responses(t *testing.T) {
	up := newMockUpstream(t, 0, "responses 正文", "思考")
	defer up.close()
	srv := newGateway(t, up, nil, false)

	body := `{"model":"gpt-5.6-sol","instructions":"简短","input":"你好"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "response" {
		t.Fatalf("object 应为 response，得到 %v", resp["object"])
	}
	output := resp["output"].([]any)
	if len(output) < 2 {
		t.Fatalf("output 应含 reasoning + message，得到 %d 项", len(output))
	}
	msg := output[len(output)-1].(map[string]any)
	if msg["type"] != "message" {
		t.Fatalf("末项应为 message，得到 %v", msg["type"])
	}
	u := resp["usage"].(map[string]any)
	if u["input_tokens"].(float64) <= 0 {
		t.Fatalf("usage 异常: %v", u)
	}
}

// TestE2E_Models 验证模型目录。
func TestE2E_Models(t *testing.T) {
	up := newMockUpstream(t, 0, "x", "")
	defer up.close()
	srv := newGateway(t, up, nil, false)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) < 3 {
		t.Fatalf("模型目录应含多个模型，得到 %d", len(data))
	}
	// 含别名（如 sol / astra）
	ids := map[string]bool{}
	for _, d := range data {
		ids[d.(map[string]any)["id"].(string)] = true
	}
	if !ids["gpt-5.6-sol"] {
		t.Fatalf("目录应含 gpt-5.6-sol")
	}
}

// TestE2E_Health 验证观测端点。
func TestE2E_Health(t *testing.T) {
	up := newMockUpstream(t, 0, "x", "")
	defer up.close()
	srv := newGateway(t, up, nil, false)

	for _, ep := range []string{"/healthz", "/metrics", "/admin/usage"} {
		req := httptest.NewRequest(http.MethodGet, ep, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 期望 200，得到 %d", ep, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("%s 响应体为空", ep)
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
