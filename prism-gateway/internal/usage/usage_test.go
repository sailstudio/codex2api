package usage

import (
	"testing"
	"time"
)

// TestUsageInvariant 验证核心不变量：Input + CacheRead + CacheCreation == 总输入。
func TestUsageInvariant(t *testing.T) {
	u := Usage{InputTokens: 100, CacheReadInputTokens: 300, CacheCreationInputTokens: 50, OutputTokens: 20}
	if got := u.TotalInput(); got != 450 {
		t.Fatalf("TotalInput 应为 450，得到 %d", got)
	}
	if got := u.Total(); got != 470 {
		t.Fatalf("Total 应为 470，得到 %d", got)
	}
}

// TestEstimator 验证估算口径（中文≈1 token/字，英文≈1 token/4 字符）。
func TestEstimator(t *testing.T) {
	e := DefaultEstimator()
	if n := e.Text("你好世界"); n < 4 || n > 6 {
		t.Errorf("4 个汉字应约 4-6 token，得到 %d", n)
	}
	if n := e.Text("hello world!"); n < 2 || n > 6 {
		t.Errorf("12 个 ASCII 应约 2-6 token，得到 %d", n)
	}
	if n := e.Text(""); n != 0 {
		t.Errorf("空串应为 0，得到 %d", n)
	}
	if n := e.Image(1024); n != e.ImageTokenFlat {
		t.Errorf("小图应按固定开销 %d，得到 %d", e.ImageTokenFlat, n)
	}
	if n := e.Image(100000); n < 20000 {
		t.Errorf("大图应按 b64/4 计，得到 %d", n)
	}
}

// TestTracker_WriteThenRead 验证「首轮写、次轮读」的缓存计数。
func TestTracker_WriteThenRead(t *testing.T) {
	tr := NewTracker(time.Minute, 128, DefaultEstimator())
	blocks := []string{"你是助手，规则如下…", "User: 第一问", "Assistant: 回答一"}

	// 首轮：全部是新建（无历史）。
	p1 := tr.Plan("test", blocks[:2], 10, 0, 0)
	u1 := p1.Result()
	if u1.CacheCreationInputTokens == 0 {
		t.Fatalf("首轮应有 cache creation，得到 0")
	}
	if u1.CacheReadInputTokens != 0 {
		t.Fatalf("首轮不应有 cache read，得到 %d", u1.CacheReadInputTokens)
	}
	p1.Commit()

	// 次轮：前缀一致 + 追加一块 → 前缀读命中，新增块写。
	p2 := tr.Plan("test", append(append([]string{}, blocks...), "User: 第二问"), 8, 0, 0)
	u2 := p2.Result()
	if u2.CacheReadInputTokens == 0 {
		t.Fatalf("次轮应有 cache read（前缀命中），得到 0")
	}
	if u2.CacheReadInputTokens <= u1.CacheCreationInputTokens {
		t.Logf("提示：读命中 %d，首轮写入 %d", u2.CacheReadInputTokens, u1.CacheCreationInputTokens)
	}
	if u2.TotalInput() == 0 {
		t.Fatalf("总输入不应为 0")
	}
	// 不变量守恒
	if u2.InputTokens+u2.CacheReadInputTokens+u2.CacheCreationInputTokens != u2.TotalInput() {
		t.Fatalf("不变量被破坏")
	}
}

// TestTracker_NamespaceIsolation 验证命名空间隔离（不同 key 不互相命中）。
func TestTracker_NamespaceIsolation(t *testing.T) {
	tr := NewTracker(time.Minute, 128, DefaultEstimator())
	blocks := []string{"共同前缀", "后续内容"}
	p := tr.Plan("ns-a", blocks, 5, 0, 0)
	p.Commit()

	p2 := tr.Plan("ns-b", blocks, 5, 0, 0)
	if p2.Result().CacheReadInputTokens != 0 {
		t.Fatalf("不同命名空间不应命中，得到读 %d", p2.Result().CacheReadInputTokens)
	}
}

// TestTracker_TTLExpiry 验证过期后不再命中。
func TestTracker_TTLExpiry(t *testing.T) {
	tr := NewTracker(10*time.Millisecond, 128, DefaultEstimator())
	blocks := []string{"前缀"}
	tr.Plan("t", blocks, 1, 0, 0).Commit()
	time.Sleep(25 * time.Millisecond)
	p := tr.Plan("t", blocks, 1, 0, 0)
	if p.Result().CacheReadInputTokens != 0 {
		t.Fatalf("过期后不应命中，得到读 %d", p.Result().CacheReadInputTokens)
	}
}

// TestTracker_Eviction 验证容量上限不导致 OOM（超限后表被清）。
func TestTracker_Eviction(t *testing.T) {
	tr := NewTracker(time.Minute, 8, DefaultEstimator())
	for i := 0; i < 200; i++ {
		b := []string{string(rune('a' + i%26)), string(rune('0' + i%10))}
		tr.Plan("small", b, 1, 0, 0).Commit()
	}
	_, _, _, entries := tr.Stats()
	if entries > 8*4 {
		t.Fatalf("容量上限未生效，entries=%d", entries)
	}
}

// TestCounters 验证读写计数的累加与分 key 隔离。
func TestCounters(t *testing.T) {
	c := NewCounters()
	c.Record("key-a", Usage{InputTokens: 100, CacheReadInputTokens: 50, CacheCreationInputTokens: 20, OutputTokens: 10}, false)
	c.Record("key-a", Usage{InputTokens: 10, CacheReadInputTokens: 90, CacheCreationInputTokens: 0, OutputTokens: 30}, false)
	c.Record("key-b", Usage{InputTokens: 1, OutputTokens: 1}, false)
	c.Record("key-b", Usage{}, true)

	global, byKey := c.Snapshot()
	if global.Requests != 4 || global.Errors != 1 {
		t.Fatalf("全局请求/错误应为 4/1，得到 %d/%d", global.Requests, global.Errors)
	}
	if global.CacheRead != 140 {
		t.Fatalf("全局缓存读应为 140，得到 %d", global.CacheRead)
	}
	if global.InputTokens != 111 {
		t.Fatalf("全局输入应为 111，得到 %d", global.InputTokens)
	}
	if byKey["key-a"].CacheRead != 140 || byKey["key-b"].CacheRead != 0 {
		t.Fatalf("分 key 计数隔离失败: %+v", byKey)
	}
	t.Logf("报告:\n%s", c.FormatReport())
}

// TestUsageJSON 验证导出的 total_tokens 与 cache_read_ratio。
func TestUsageJSON(t *testing.T) {
	u := Usage{InputTokens: 100, CacheReadInputTokens: 100, OutputTokens: 10}
	b, err := u.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"total_tokens"`, `"cache_read_ratio"`, "0.5"} {
		if !contains(s, want) {
			t.Errorf("JSON 缺少 %q：%s", want, s)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
