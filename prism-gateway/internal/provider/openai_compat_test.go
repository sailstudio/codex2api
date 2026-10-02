package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeUpstream 是一个说 OpenAI 协议的假上游（模拟 codex2api）。
func fakeUpstream(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer up-key" {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "gpt-5.6-sol"}, {"id": "gpt-6-astra"}},
		})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			f := w.(http.Flusher)
			for _, s := range []string{`{"choices":[{"delta":{"content":"你"}}]}`, `{"choices":[{"delta":{"content":"好"}}]}`, "[DONE]"} {
				_, _ = w.Write([]byte("data: " + s + "\n\n"))
				f.Flush()
				time.Sleep(5 * time.Millisecond)
			}
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-up", "object": "chat.completion",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "上游回复"}}},
		})
	})
	return httptest.NewServer(mux)
}

// TestProxy_NonStream 验证非流式透传（含响应头回灌与鉴权补全）。
func TestProxy_NonStream(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	p := NewOpenAICompat("codex2api", up.URL, "up-key")
	if !p.Ready() {
		t.Fatal("应就绪")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := p.Proxy(rec, req, "/v1/chat/completions"); err != nil {
		t.Fatalf("透传失败: %v", err)
	}
	if rec.Code != 200 {
		t.Fatalf("应 200，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "上游回复") {
		t.Fatalf("正文未透传: %s", rec.Body.String())
	}
}

// TestProxy_StreamFlush 验证 SSE 逐块透传（低延迟关键：必须每块 Flush）。
func TestProxy_StreamFlush(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	p := NewOpenAICompat("codex2api", up.URL, "up-key")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5.6-sol","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if err := p.Proxy(rec, req, "/v1/chat/completions"); err != nil {
		t.Fatalf("透传失败: %v", err)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Fatalf("应透传 SSE Content-Type，得到 %q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{`"content":"你"`, `"content":"好"`, "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE 缺少 %q：%s", want, body)
		}
	}
	// httptest.ResponseRecorder 实现了 Flusher，Flushed=true 说明逐块推送发生
	if !rec.Flushed {
		t.Errorf("应发生过 Flush（否则 SSE 会被中间代理缓冲）")
	}
}

// TestProxy_AuthHeaderNotLeaked 验证客户端的鉴权头不会泄漏给上游。
func TestProxy_AuthHeaderNotLeaked(t *testing.T) {
	var gotAuth, gotKey, gotUA string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotKey = r.Header.Get("x-api-key")
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(200)
	}))
	defer up.Close()
	p := NewOpenAICompat("up", up.URL, "server-key")

	req := httptest.NewRequest(http.MethodPost, "/v1/x", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer CLIENT-SECRET")
	req.Header.Set("User-Agent", "client-agent")
	rec := httptest.NewRecorder()
	_ = p.Proxy(rec, req, "/v1/x")

	if gotAuth != "Bearer server-key" {
		t.Errorf("上游 Authorization 应为服务端 key（覆盖客户端），得到 %q", gotAuth)
	}
	if gotKey != "server-key" {
		t.Errorf("上游 x-api-key 应为服务端 key，得到 %q", gotKey)
	}
	if gotUA == "client-agent" {
		t.Errorf("不应把客户端 UA 透传给上游")
	}
}

// TestModels 验证模型目录拉取。
func TestModels(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	p := NewOpenAICompat("codex2api", up.URL, "up-key")
	ms, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0] != "gpt-5.6-sol" {
		t.Fatalf("模型目录异常: %v", ms)
	}
}

// TestHealth 验证探活。
func TestHealth(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	p := NewOpenAICompat("codex2api", up.URL, "up-key")
	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("探活应成功: %v", err)
	}
	bad := NewOpenAICompat("bad", "http://127.0.0.1:1", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := bad.Health(ctx); err == nil {
		t.Fatal("不可达的提供方探活应失败")
	}
}

// TestRouter 验证按模型名路由。
func TestRouter(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	rt := NewRouter(NewOpenAICompat("codex2api", up.URL, "k"))

	cases := []struct{ model, wantTarget, wantModel string }{
		{"gpt-5.6-sol", "codex2api", "gpt-5.6-sol"},
		{"gpt-6-astra", "codex2api", "gpt-6-astra"},
		{"gpt-5.6-sol-high", "codex2api", "gpt-5.6-sol-high"},
		{"codex2api/gpt-5.6-sol", "codex2api", "gpt-5.6-sol"},
		{"prism/gpt-5.6-sol", "prism", "gpt-5.6-sol"},
		{"o3-mini", "codex2api", "o3-mini"},
	}
	for _, c := range cases {
		d := rt.Route(c.model)
		if d.Target != c.wantTarget || d.Model != c.wantModel {
			t.Errorf("Route(%q) = {%s,%s}，期望 {%s,%s}", c.model, d.Target, d.Model, c.wantTarget, c.wantModel)
		}
	}

	// codex2api 未配置时，Codex 模型也回落到 prism。
	rt2 := NewRouter(NewOpenAICompat("codex2api", "", ""))
	if d := rt2.Route("gpt-5.6-sol"); d.Target != "prism" {
		t.Fatalf("未配置 codex2api 时应回落 prism，得到 %s", d.Target)
	}
	// 未配置时显式前缀 codex2api/ 也回落 prism
	if d := rt2.Route("codex2api/gpt-5.6-sol"); d.Target != "prism" {
		t.Fatalf("未配置时显式前缀应回落 prism，得到 %s", d.Target)
	}

	// DefaultTarget=codex2api 时：非 Codex 模型也走 codex2api，
	// 但显式 prism/ 前缀仍然走私协议（这是「codex2api 全量前置」模式）。
	rt3 := NewRouter(NewOpenAICompat("codex2api", up.URL, "k"))
	rt3.DefaultTarget = "codex2api"
	if d := rt3.Route("some-random-model"); d.Target != "codex2api" {
		t.Errorf("DefaultTarget=codex2api 时应走 codex2api，得到 %s", d.Target)
	}
	if d := rt3.Route("prism/gpt-5.6-sol"); d.Target != "prism" || d.Model != "gpt-5.6-sol" {
		t.Errorf("显式 prism/ 前缀应强制走 prism，得到 {%s,%s}", d.Target, d.Model)
	}
}

// TestProxy_ClientDisconnect 验证客户端断连时不会 panic、能干净退出。
func TestProxy_ClientDisconnect(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i := 0; i < 100; i++ {
			if _, err := io.WriteString(w, "data: x\n\n"); err != nil {
				return
			}
			f.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer up.Close()
	p := NewOpenAICompat("up", up.URL, "")

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader("{}")).WithContext(ctx)
	rec := httptest.NewRecorder()
	go func() {
		time.Sleep(60 * time.Millisecond)
		cancel() // 模拟客户端断开
	}()
	done := make(chan error, 1)
	go func() { done <- p.Proxy(rec, req, "/v1/chat") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("断连应干净退出，得到 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("断连后未及时退出（可能泄漏 goroutine）")
	}
}
