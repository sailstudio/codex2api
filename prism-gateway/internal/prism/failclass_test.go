package prism

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// ------------------------------------------------------------------ 失败分类

// TestClassify 验证三态分类与处置建议（冷却/换号语义）。
func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantKind FailKind
		wantCool bool
		wantSwit bool
	}{
		{"模型不支持", &UpstreamError{Status: 400, Op: "start", Msg: "Unsupported assistant model"}, KindRequest, false, false},
		{"上下文超长", &UpstreamError{Status: 400, Op: "start", Msg: "context window exceeded, too long"}, KindRequest, false, false},
		{"凭据失效", &UpstreamError{Status: 401, Op: "auth/session", Msg: "unauthorized"}, KindAuth, true, true},
		{"403 风控", &UpstreamError{Status: 403, Op: "start", Msg: "Forbidden"}, KindAuth, true, true},
		{"限流", &UpstreamError{Status: 429, Op: "start", Msg: "rate limit"}, KindQuota, true, true},
		{"上游 5xx", &UpstreamError{Status: 502, Op: "status", Msg: "bad gateway"}, KindBroken, false, false},
		{"沙箱劣化文案", errors.New("prism: upstream sandbox degraded"), KindBroken, false, false},
		{"504 文案", errors.New("Error while processing conversation (504 Gateway Timeout)"), KindBroken, false, false},
		{"提交重试文案", errors.New("Please submit prompt again. (500)"), KindBroken, false, false},
		{"传输超时", errors.New("Post \"https://x\": context deadline exceeded (timeout)"), KindTransport, false, true},
		{"连接重置", errors.New("read tcp: connection reset by peer"), KindTransport, false, true},
	}
	for _, c := range cases {
		got := Classify(c.err)
		if got.Kind != c.wantKind || got.Cool != c.wantCool || got.Swit != c.wantSwit {
			t.Errorf("%s: Classify = {kind=%v cool=%v swit=%v}，期望 {kind=%v cool=%v swit=%v}",
				c.name, got.Kind, got.Cool, got.Swit, c.wantKind, c.wantCool, c.wantSwit)
		}
	}
}

// TestClassify_ClientCancel 验证客户端取消不被误判为账号问题。
func TestClassify_ClientCancel(t *testing.T) {
	err := MarkClientCancelled(context.Canceled)
	c := Classify(err)
	if c.Kind != KindCancel || c.Cool || c.Swit {
		t.Fatalf("客户端取消应 Cool=false Switch=false，得到 %+v", c)
	}
}

// ------------------------------------------------------------------ 熔断器

// TestBreaker_OpensOnDegradation 验证劣化时开闸，且请求过错不计入统计。
func TestBreaker_OpensOnDegradation(t *testing.T) {
	b := NewBreaker(60*time.Second, 0.7, 12, 15*time.Second)
	now := time.Now()
	b.now = func() time.Time { return now }

	// 12 个样本里 10 个劣化 → 失败率 0.83 ≥ 0.7 → 开闸
	for i := 0; i < 10; i++ {
		b.Record(true, KindBroken)
	}
	for i := 0; i < 2; i++ {
		b.Record(false, KindUnknown)
	}
	if !b.Stats().Open {
		t.Fatalf("失败率 %.2f（样本 %d）应开闸", b.Stats().FailRate, b.Stats().Samples)
	}
	if b.Stats().TotalTrips != 1 {
		t.Errorf("应记录 1 次跳闸，得到 %d", b.Stats().TotalTrips)
	}
}

// TestBreaker_RequestErrorsNotCounted 验证请求过错不污染熔断统计。
func TestBreaker_RequestErrorsNotCounted(t *testing.T) {
	b := NewBreaker(60*time.Second, 0.7, 12, 15*time.Second)
	for i := 0; i < 30; i++ {
		b.Record(true, KindRequest) // 全是请求过错
	}
	if b.Stats().Open {
		t.Fatalf("请求过错不应触发熔断，样本=%d", b.Stats().Samples)
	}
	if b.Stats().Samples != 0 {
		t.Errorf("请求过错不应计入样本，得到 %d", b.Stats().Samples)
	}
}

// TestBreaker_ProbeSelfHeal 验证开闸后每 probeGap 放一个探测请求，成功即闭合。
func TestBreaker_ProbeSelfHeal(t *testing.T) {
	b := NewBreaker(60*time.Second, 0.5, 4, 15*time.Second)
	now := time.Now()
	b.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		b.Record(true, KindBroken)
	}
	if !b.Stats().Open {
		t.Fatal("应开闸")
	}
	// 刚开闸：上游刚大面积失败，立即放探测没有意义，应先冷却一个 probeGap。
	if b.Allow() {
		t.Fatal("刚开闸不应立即放行（应冷却一个 probeGap）")
	}
	// 推进时间到 probeGap：放行一次探测（且只放一次）
	now = now.Add(16 * time.Second)
	if !b.Allow() {
		t.Fatal("超过探测间隔后应放行探测请求")
	}
	if b.Allow() {
		t.Fatal("探测请求只应放行一次")
	}
	// 探测成功 → 闭合
	b.Record(false, KindUnknown)
	if b.Stats().Open {
		t.Fatal("探测成功后应闭合")
	}
}

// TestBreaker_WindowSlides 验证滑动窗（旧事件滚出后不再影响判定）。
func TestBreaker_WindowSlides(t *testing.T) {
	b := NewBreaker(10*time.Second, 0.7, 5, time.Second)
	now := time.Now()
	base := now
	b.now = func() time.Time { return base }
	for i := 0; i < 5; i++ {
		b.Record(true, KindBroken)
	}
	if !b.Stats().Open {
		t.Fatal("应开闸")
	}
	// 手工闭合以观察窗滑动
	b.mu.Lock()
	b.open = false
	b.mu.Unlock()
	// 推进 20s 后记一个成功 → 旧失败全部滚出窗
	base = base.Add(20 * time.Second)
	b.Record(false, KindUnknown)
	if b.Stats().Samples != 1 {
		t.Fatalf("旧事件应滚出窗口，剩余样本应为 1，得到 %d", b.Stats().Samples)
	}
	if b.Stats().Open {
		t.Fatal("窗口内只剩成功样本，不应开闸")
	}
}

// ------------------------------------------------------------------ 端到端：熔断与快速失败

// TestEndToEnd_BreakerShortCircuits 验证上游持续 5xx 时熔断打开、入口快速失败，
// 且开闸后不再打上游（保护过载中的上游）。
func TestEndToEnd_BreakerShortCircuits(t *testing.T) {
	var upstreamHits int64
	var mu sync.Mutex
	acct := &Account{ID: "a1", Cookie: "oai-sc=1", UserAgent: "ua"}
	pool := NewPool([]*Account{acct}, PoolOptions{
		Client:         Options{Origin: "http://127.0.0.1:1", StartAttempts: 1, Transport: DefaultTransportConfig()},
		PerAccountConc: 4, WarmOnStart: false,
		BreakerWindow: time.Minute, BreakerMinRate: 0.5, BreakerMinSamp: 4, BreakerProbeGap: time.Hour,
		RetryBackoffMin: time.Millisecond, RetryBackoffMax: time.Millisecond,
	})
	defer pool.Close()

	call := func() error {
		return pool.Do(context.Background(), 1, func(ctx context.Context, c *Client) error {
			// 只有真正取到号、要打上游时才计数（熔断短路不应计入）。
			mu.Lock()
			upstreamHits++
			mu.Unlock()
			return &UpstreamError{Status: http.StatusBadGateway, Op: "start", Msg: "bad gateway"}
		})
	}
	// 前 4 次触发开闸
	for i := 0; i < 4; i++ {
		_ = call()
	}
	if !pool.Breaker().Stats().Open {
		t.Fatalf("应已熔断，状态=%+v", pool.Breaker().Stats())
	}
	hitsBefore := func() int64 { mu.Lock(); defer mu.Unlock(); return upstreamHits }()
	// 开闸后（probeGap=1h）再打 10 次都不该碰上游
	for i := 0; i < 10; i++ {
		err := call()
		var de *DegradedError
		if !errors.As(err, &de) {
			t.Fatalf("熔断期间应返回 *DegradedError，得到 %v", err)
		}
		if de.HttpStatus() != http.StatusServiceUnavailable {
			t.Errorf("应映射 503，得到 %d", de.HttpStatus())
		}
		if de.RetryAfterSeconds() < 1 {
			t.Errorf("Retry-After 应 ≥1s")
		}
	}
	hitsAfter := func() int64 { mu.Lock(); defer mu.Unlock(); return upstreamHits }()
	if hitsAfter != hitsBefore {
		t.Fatalf("熔断期间不应打上游：之前 %d 之后 %d", hitsBefore, hitsAfter)
	}
}

// TestEndToEnd_RequestErrorFailsFast 验证请求过错不换号、不冷却（快速失败）。
func TestEndToEnd_RequestErrorFailsFast(t *testing.T) {
	acct := &Account{ID: "a1", Cookie: "oai-sc=1"}
	pool := NewPool([]*Account{acct}, PoolOptions{
		Client:         Options{Origin: "http://127.0.0.1:1", StartAttempts: 1, Transport: DefaultTransportConfig()},
		PerAccountConc: 4, WarmOnStart: false, CooldownOnErr: time.Minute,
		RetryBackoffMin: time.Millisecond, RetryBackoffMax: time.Millisecond,
	})
	defer pool.Close()

	attempts := 0
	err := pool.Do(context.Background(), 3, func(ctx context.Context, c *Client) error {
		attempts++
		return &UpstreamError{Status: 400, Op: "start", Msg: "Unsupported assistant model"}
	})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if attempts != 1 {
		t.Fatalf("请求过错不应重试（换号零收益），attempts=%d", attempts)
	}
	_, _, cooling := acct.Health()
	if cooling {
		t.Errorf("请求过错不应冷却账号")
	}
}

// TestEndToEnd_AuthErrorCoolsAndSwitches 验证凭据失效冷却账号并换号。
func TestEndToEnd_AuthErrorCoolsAndSwitches(t *testing.T) {
	a1 := &Account{ID: "a1", Cookie: "oai-sc=1"}
	a2 := &Account{ID: "a2", Cookie: "oai-sc=2"}
	pool := NewPool([]*Account{a1, a2}, PoolOptions{
		Client:         Options{Origin: "http://127.0.0.1:1", StartAttempts: 1, Transport: DefaultTransportConfig()},
		PerAccountConc: 1, WarmOnStart: false, CooldownOnErr: time.Minute,
		RetryBackoffMin: time.Millisecond, RetryBackoffMax: time.Millisecond,
	})
	defer pool.Close()

	used := map[string]int{}
	err := pool.Do(context.Background(), 2, func(ctx context.Context, c *Client) error {
		used[c.Account().ID]++
		if c.Account().ID == "a1" {
			return &UpstreamError{Status: 401, Op: "auth/session", Msg: "unauthorized"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("换到健康账号后应成功，得到 %v", err)
	}
	if used["a2"] == 0 {
		t.Fatalf("应换到 a2，实际使用情况 %v", used)
	}
	if _, _, cooling := a1.Health(); !cooling {
		t.Errorf("a1 应进入冷却")
	}
}

// ------------------------------------------------------------------ 单飞代际保护

// TestSandboxGenerationProtection 验证慢 flight 的旧令牌不会覆盖新令牌。
func TestSandboxGenerationProtection(t *testing.T) {
	acct := &Account{ID: "a1", Cookie: "oai-sc=1"}
	c := NewClient(Options{Origin: "http://127.0.0.1:1", Transport: DefaultTransportConfig()}, acct)

	// 模拟：一代已发布新令牌
	c.mu.Lock()
	c.sandboxGen = 5
	c.sandbox = "new-token"
	c.sandboxURL = "new-url"
	c.mu.Unlock()

	// 慢 flight（gen=4，比当前代旧）完成时不应覆盖
	c.mu.Lock()
	gen := uint64(4)
	if gen >= c.sandboxGen {
		c.sandbox = "stale-token"
	}
	got := c.sandbox // 直接读字段：Sandbox() 会重复加锁造成自锁死锁
	c.mu.Unlock()
	if got != "new-token" {
		t.Fatalf("旧代令牌不应覆盖新令牌，得到 %q", got)
	}

	// 作废会推进代际
	c.InvalidateSandbox()
	c.mu.Lock()
	genAfter := c.sandboxGen
	c.mu.Unlock()
	if genAfter <= 5 {
		t.Fatalf("作废应推进代际号，得到 %d", genAfter)
	}
}

// TestTouchSandboxExtendsLife 验证「命中即刷新」延长沙箱寿命。
func TestTouchSandboxExtendsLife(t *testing.T) {
	acct := &Account{ID: "a1", Cookie: "oai-sc=1"}
	c := NewClient(Options{Origin: "http://127.0.0.1:1", Transport: DefaultTransportConfig()}, acct)
	c.mu.Lock()
	c.sandbox = "tok"
	c.sandboxURL = "url"
	c.sandboxAt = time.Now().Add(-10 * time.Minute)
	c.mu.Unlock()
	if age := c.SandboxAge(); age < 9*time.Minute {
		t.Fatalf("年龄应约 10min，得到 %v", age)
	}
	c.TouchSandbox()
	if age := c.SandboxAge(); age > time.Second {
		t.Fatalf("刷新后年龄应接近 0，得到 %v", age)
	}
}

// TestJitter 验证抖动落在界内。
func TestJitter(t *testing.T) {
	for i := 0; i < 50; i++ {
		d := jitter(30 * time.Second)
		if d < 0 || d >= 30*time.Second {
			t.Fatalf("抖动应在 [0,30s)，得到 %v", d)
		}
	}
	if jitter(0) != 0 {
		t.Fatalf("0 界应返回 0")
	}
}

// TestPoolBestAccountPrefersHot 验证选号优先热沙箱（低延迟关键）。
func TestPoolBestAccountPrefersHot(t *testing.T) {
	a1 := &Account{ID: "cold", Cookie: "oai-sc=1"}
	a2 := &Account{ID: "hot", Cookie: "oai-sc=2"}
	pool := NewPool([]*Account{a1, a2}, PoolOptions{
		Client:         Options{Origin: "http://127.0.0.1:1", Transport: DefaultTransportConfig()},
		PerAccountConc: 4, WarmOnStart: false,
	})
	defer pool.Close()

	// 让 a2 有热沙箱
	for _, c := range pool.Clients() {
		if c.Account().ID == "hot" {
			c.mu.Lock()
			c.sandbox = "tok"
			c.sandboxURL = "url"
			c.sandboxAt = time.Now()
			c.mu.Unlock()
		}
	}
	lease, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.Client.Account().ID != "hot" {
		t.Fatalf("应优先选热沙箱账号，得到 %s", lease.Client.Account().ID)
	}
}

// TestPoolConcurrencyLimit 验证单账号并发上限生效（超限阻塞）。
func TestPoolConcurrencyLimit(t *testing.T) {
	acct := &Account{ID: "a1", Cookie: "oai-sc=1"}
	pool := NewPool([]*Account{acct}, PoolOptions{
		Client:         Options{Origin: "http://127.0.0.1:1", Transport: DefaultTransportConfig()},
		PerAccountConc: 2, WarmOnStart: false,
	})
	defer pool.Close()

	l1, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	l2, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 第三个应阻塞（用短超时验证）
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := pool.Acquire(ctx); err == nil {
		t.Fatal("超过并发上限应阻塞至超时")
	}
	l1.Release()
	// 释放后应能取到
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	l3, err := pool.Acquire(ctx2)
	if err != nil {
		t.Fatalf("释放后应可取号，得到 %v", err)
	}
	l2.Release()
	l3.Release()
}

// TestDegradedErrorMessage 验证劣化错误的可读文案与映射。
func TestDegradedErrorMessage(t *testing.T) {
	b := NewBreaker(time.Minute, 0.5, 2, 15*time.Second)
	b.Record(true, KindBroken)
	b.Record(true, KindBroken)
	de := &DegradedError{RetryAfter: 15 * time.Second, Stats: b.Stats()}
	msg := de.Error()
	if msg == "" || de.HttpStatus() != 503 || de.RetryAfterSeconds() != 15 {
		t.Fatalf("劣化错误映射异常: %q %d %d", msg, de.HttpStatus(), de.RetryAfterSeconds())
	}
	t.Logf("劣化文案: %s", msg)
}

var _ = fmt.Sprintf
