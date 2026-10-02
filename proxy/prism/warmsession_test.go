package prism

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestWarmSession_SingleFlight 并发登录必须**只打一次** /auth/session。
//
// 为什么重要：并发惊群（N 路各自登录）会瞬间向上游打 N 个登录请求，
// 实测会触发账号风控（随后所有请求 403）。这里锁住「只登录一次」的语义。
func TestWarmSession_SingleFlight(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/session" {
			atomic.AddInt64(&hits, 1)
			time.Sleep(50 * time.Millisecond) // 放大竞态窗口
			http.SetCookie(w, &http.Cookie{Name: "prism_session_token", Value: "ST"})
			_, _ = io.WriteString(w, `{"user":{"app_metadata":{"user_id":"user-x"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Base = srv.URL
	cfg.Logf = func(string, ...any) {}
	c := NewClient(cfg, Cookie{AccessToken: "AT", UserAgent: "ua"})

	const N = 32
	var wg sync.WaitGroup
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = c.WarmSession(context.Background())
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("第 %d 路登录失败: %v", i, e)
		}
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("%d 路并发应只登录 1 次（防惊群），实际 %d 次", N, got)
	}
	// 之后再次调用不应产生新登录
	if err := c.WarmSession(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("已 warm 后不应再登录，累计 %d 次", got)
	}
}

// TestWarmSession_FailureNotCached 登录失败**不得**被缓存（否则永久不可用）。
func TestWarmSession_FailureNotCached(t *testing.T) {
	var hits int64
	fail := int64(1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/session" {
			n := atomic.AddInt64(&hits, 1)
			if n == 1 && atomic.LoadInt64(&fail) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "prism_session_token", Value: "ST"})
			_, _ = io.WriteString(w, `{"user":{"app_metadata":{"user_id":"user-x"}}}`)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	cfg := DefaultConfig()
	cfg.Base = srv.URL
	cfg.Logf = func(string, ...any) {}
	c := NewClient(cfg, Cookie{AccessToken: "AT", UserAgent: "ua"})

	if err := c.WarmSession(context.Background()); err == nil {
		t.Fatal("首次登录应失败")
	}
	// 第二次必须真的重试（不能因为缓存了失败而直接返回成功/永久失败）
	if err := c.WarmSession(context.Background()); err != nil {
		t.Fatalf("失败不应被缓存，第二次应成功: %v", err)
	}
	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Errorf("应发生 2 次登录尝试，实际 %d", got)
	}
}
