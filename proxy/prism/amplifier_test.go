package prism

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestSlotRetry_NotAmplifiedOnRateLimit 钉死一个**生产级行为**：
//
// 上游限流型错误（"Error while processing conversation (403 …). Please submit prompt again."）
// **不得**触发「换槽重试」。
//
// 为什么：换槽只是换材料/身份，**绕不开账号级限流**；而每次重试都会再打一次上游 ——
// 会把 N 路请求放大成 N×attempts 次调用，等于自己烧配额、把账号推入更深冷却。
// 实测踩过：15 路 × 3 次换槽 = 45+ 次上游调用，反而全线 403。
func TestSlotRetry_NotAmplifiedOnRateLimit(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/session":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/llm/response_with_tools_start":
			atomic.AddInt64(&hits, 1)
			w.Header().Set("Content-Type", "application/json")
			// 上游把错误藏在 HTTP 200 的内层 payload 里
			_, _ = w.Write([]byte(`{"status":"completed","request_id":"r1",
			  "response":{"status":"error","payload":{
			    "message":"Error while processing conversation (403 Forbidden). Please submit prompt again.",
			    "httpStatus":403}}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	// 3 个槽 → 若发生换槽重试，会打出 3 次上游请求
	matPath := writeMaterial(t, dir, "m.json", "p1", 0)
	_ = matPath

	cfg := DefaultConfig()
	cfg.Base = srv.URL
	cfg.MaterialDir = dir
	cfg.Logf = func(string, ...any) {}
	SetMintHook(func(context.Context) (string, error) { return "tok", nil })
	defer SetMintHook(nil)

	c := NewClient(cfg, Cookie{AccessToken: "at", UserAgent: "ua"})
	if p := c.Pool(); p != nil {
		p.SetWaitTimeout(5 * time.Second)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := c.Run(ctx, Request{Input: []map[string]any{sysMsgT("user", "hi")}})
	if err == nil {
		t.Fatal("限流型错误应上抛，而非当作成功")
	}

	if n := atomic.LoadInt64(&hits); n > 1 {
		t.Fatalf("限流型错误发生了换槽重试：上游被打了 %d 次（应为 1 次）。"+
			"这正是把 N 路放大成 N×attempts 的放大器 bug。", n)
	}
	t.Logf("限流型错误只打上游 %d 次（未放大）✓  err=%v", atomic.LoadInt64(&hits), err)
}

// TestSlotRetry_StillRetriesNonRateLimit 反向保证：**非**限流型失败仍然换槽重试
// （多身份轮转的价值不能被误伤）。
func TestSlotRetry_StillRetriesNonRateLimit(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/session":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/llm/response_with_tools_start":
			n := atomic.AddInt64(&hits, 1)
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				// 第一槽：非限流型失败（如材料/沙箱问题）
				_, _ = w.Write([]byte(`{"status":"completed","request_id":"r1",
				  "response":{"status":"error","payload":{"message":"sandbox expired","httpStatus":500}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"completed","request_id":"r2",
			  "response":{"status":"success","payload":{
			    "output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],
			    "usage":{"input_tokens":1.0,"output_tokens":1.0}}}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	_ = writeMaterial(t, dir, "a.json", "proj-a", 0)
	_ = writeMaterial(t, dir, "b.json", "proj-b", 0)

	cfg := DefaultConfig()
	cfg.Base = srv.URL
	cfg.MaterialDir = dir
	cfg.Logf = func(string, ...any) {}
	SetMintHook(func(context.Context) (string, error) { return "tok", nil })
	defer SetMintHook(nil)

	c := NewClient(cfg, Cookie{AccessToken: "at", UserAgent: "ua"})
	if p := c.Pool(); p != nil {
		p.SetWaitTimeout(10 * time.Second)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	turn, err := c.Run(ctx, Request{Input: []map[string]any{sysMsgT("user", "hi")}})
	if err != nil {
		t.Fatalf("非限流型失败应换槽重试并最终成功: %v", err)
	}
	if turn.Text != "ok" {
		t.Fatalf("正文 = %q，期望 ok", turn.Text)
	}
	if n := atomic.LoadInt64(&hits); n < 2 {
		t.Fatalf("非限流型失败未换槽重试（上游只打 %d 次）", n)
	}
	t.Logf("非限流型失败成功换槽重试（上游 %d 次）✓", atomic.LoadInt64(&hits))
}
