package prism

import (
	"context"
	"testing"
	"time"
)

// TestIsRetryableUpstreamErr 钉死「哪些上游错误该重试」。
// 实测文案：Error while processing conversation (403 Forbidden). Please submit prompt again.
func TestIsRetryableUpstreamErr(t *testing.T) {
	yes := []string{
		"Error while processing conversation (403 Forbidden). Please submit prompt again.",
		"error while processing conversation",
		"Please submit prompt again",
		"HTTP 429 Too Many Requests",
		"rate limit exceeded",
	}
	for _, s := range yes {
		if !isRetryableUpstreamErr(s) {
			t.Errorf("应判为可重试: %q", s)
		}
	}
	no := []string{
		"",
		"400 Bad Request: invalid model",
		"material expired",
		"unrelated text",
	}
	for _, s := range no {
		if isRetryableUpstreamErr(s) {
			t.Errorf("不应判为可重试: %q", s)
		}
	}
}

// TestGate_CooldownTripAndWait 验证：触发熔断后，新请求会被**压住**直到冷却结束
// （而不是硬打被上游秒拒）。
func TestGate_CooldownTripAndWait(t *testing.T) {
	g := newInflightGate(1, 0)
	g.tripCooldown(120 * time.Millisecond)
	if r := g.cooldownRemaining(); r <= 0 {
		t.Fatal("应处于冷却中")
	}
	start := time.Now()
	done, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire 失败: %v", err)
	}
	done()
	if el := time.Since(start); el < 100*time.Millisecond {
		t.Fatalf("冷却未生效（仅等 %v）", el)
	}
	if r := g.cooldownRemaining(); r > 0 {
		t.Fatalf("冷却应已结束，剩余 %v", r)
	}
}

// TestGate_CooldownBacksOff 验证：**连续**触发 → 冷却递增退避；成功后清零复位。
func TestGate_CooldownBacksOff(t *testing.T) {
	g := newInflightGate(1, 0)
	base := 40 * time.Millisecond

	g.tripCooldown(base)
	r1 := g.cooldownRemaining()
	g.tripCooldown(base) // 连续第 2 次 → 加倍
	r2 := g.cooldownRemaining()
	if r2 < r1*3/2 {
		t.Fatalf("第 2 次退避(%v)应明显大于第 1 次(%v)", r2, r1)
	}

	g.resetTrips() // 成功后复位
	time.Sleep(r2 + 20*time.Millisecond)
	g.tripCooldown(base)
	r3 := g.cooldownRemaining()
	if r3 > r1*2 {
		t.Fatalf("复位后退避(%v)应回到基础档(%v 附近)", r3, r1)
	}
}

// TestGate_CooldownCtxCancel 验证：等冷却期间 ctx 取消能及时返回。
func TestGate_CooldownCtxCancel(t *testing.T) {
	g := newInflightGate(1, 0)
	g.tripCooldown(10 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := g.acquire(ctx); err == nil {
		t.Fatal("应因 ctx 超时返回错误")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("ctx 取消未被及时响应（%v）", el)
	}
}

// TestGate_CooldownDisabled 验证：未触发时不影响正常放行。
func TestGate_CooldownDisabled(t *testing.T) {
	g := newInflightGate(2, 0)
	start := time.Now()
	for i := 0; i < 5; i++ {
		done, err := g.acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		done()
	}
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Fatalf("无冷却时不应有额外等待（%v）", el)
	}
}
