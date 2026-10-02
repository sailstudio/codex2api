package prism

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestInflightGate_LimitsConcurrency 验证：闸门把「同时」在飞压到 max 以内，
// 并且**所有**请求最终都完成（排队而非丢弃）。
//
// 这是单账号 ≥15 路的关键：上游只放行 ~4 路并发，超出的秒拒 403；
// 闸门让 15 路变成「4 路在飞 + 11 路排队」，从而全部成功。
func TestInflightGate_LimitsConcurrency(t *testing.T) {
	const max = 3
	const total = 12
	g := newInflightGate(max, 0)

	var cur, peak int64
	var completed int64
	var wg sync.WaitGroup

	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done, err := g.acquire(context.Background())
			if err != nil {
				t.Errorf("acquire 失败: %v", err)
				return
			}
			defer done()

			n := atomic.AddInt64(&cur, 1)
			for {
				p := atomic.LoadInt64(&peak)
				if n <= p || atomic.CompareAndSwapInt64(&peak, p, n) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond) // 模拟一次上游请求
			atomic.AddInt64(&cur, -1)
			atomic.AddInt64(&completed, 1)
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&peak); got > max {
		t.Fatalf("在飞峰值 %d 超过上限 %d（闸门失效）", got, max)
	}
	if got := atomic.LoadInt64(&completed); got != total {
		t.Fatalf("完成数 %d != %d（有请求被丢弃，应为排队）", got, total)
	}
	t.Logf("峰值在飞=%d（上限 %d），完成 %d/%d ✓", atomic.LoadInt64(&peak), max, completed, total)
}

// TestInflightGate_ContextCancelDropsWaiter 验证：排队中 ctx 取消 → 立刻返回错误，
// 且不泄漏名额（后续请求仍能正常取到名额）。
func TestInflightGate_ContextCancelDropsWaiter(t *testing.T) {
	g := newInflightGate(1, 0)

	done1, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("首个 acquire 失败: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := g.acquire(ctx)
		errCh <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("ctx 取消后 acquire 应返回错误")
		}
	case <-time.After(time.Second):
		t.Fatal("ctx 取消后 acquire 未及时返回（卡死）")
	}

	done1() // 归还名额

	// 取消者不应占用名额：新请求必须拿到
	done2, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("取消后名额泄漏，新请求取不到: %v", err)
	}
	done2()
}

// TestInflightGate_DisabledByDefault 验证：MaxInflight<=0 → 不建闸门（不限流）。
func TestInflightGate_DisabledByDefault(t *testing.T) {
	if newInflightGate(0, 0) != nil {
		t.Fatal("MaxInflight=0 应为 nil（不限流）")
	}
	c := NewClient(Config{Base: "http://127.0.0.1:1"}, Cookie{UserAgent: "x"})
	if c.gate != nil {
		t.Fatal("默认 Client 不应有闸门")
	}
}

// TestInflightGate_ClientWired 验证：Config.MaxInflight 会真正接到 Client 上。
func TestInflightGate_ClientWired(t *testing.T) {
	c := NewClient(Config{Base: "http://127.0.0.1:1", MaxInflight: 5}, Cookie{UserAgent: "x"})
	if c.gate == nil {
		t.Fatal("MaxInflight=5 应建闸门")
	}
	if c.gate.max != 5 {
		t.Fatalf("闸门上限 = %d，期望 5", c.gate.max)
	}
}

// TestInflightGate_MinGapPaces 验证：最小间隔会让相邻请求错峰（总耗时 ≥ (n-1)*gap）。
func TestInflightGate_MinGapPaces(t *testing.T) {
	const gap = 30 * time.Millisecond
	g := newInflightGate(1, gap)
	start := time.Now()
	for i := 0; i < 4; i++ {
		done, err := g.acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d 失败: %v", i, err)
		}
		done()
	}
	el := time.Since(start)
	if el < 3*gap {
		t.Fatalf("4 次请求耗时 %v，未体现最小间隔 %v（应 ≥ %v）", el, gap, 3*gap)
	}
	t.Logf("4 次请求耗时 %v（间隔 %v，符合平滑放行）✓", el, gap)
}

// TestInflightGate_MinGapCtxCancel 验证：等间隔时 ctx 取消能及时返回。
func TestInflightGate_MinGapCtxCancel(t *testing.T) {
	g := newInflightGate(1, time.Hour)                         // 极大间隔
	if _, err := g.acquire(context.Background()); err != nil { // 首次放行，设定 lastStart
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := g.acquire(ctx); err == nil {
		t.Fatal("应因 ctx 超时返回错误")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("ctx 取消未被及时响应（耗时 %v）", el)
	}
}
