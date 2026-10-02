package prism

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ============================================================
// 材料池（多槽）测试。
//
// 目标：验证「多身份轮转提吞吐」的正确性与健壮性 ——
// 新鲜度筛选、负载均衡、并发安全、坏槽跳过。
// ============================================================

// writeMaterial 造一份材料文件并落盘。
func writeMaterial(t *testing.T, dir, name, projectID string, age time.Duration) string {
	t.Helper()
	ts := time.Now().Add(-age).UTC().Format(time.RFC3339)
	content := fmt.Sprintf(`{
	  "captured_at": %q,
	  "project_id": %q,
	  "metadata": {"projectId":%q,"userId":"user-x","model":"gpt-5.6-sol",
	    "reasoning_effort":"medium","sandbox_url":"https://p/s/proxy/","sandbox_token":"t",
	    "proxy_request_debug":"{}","codex_listen_snapshot":"{}"},
	  "input": [{"type":"message","role":"system","content":[{"type":"input_text","text":"S"}]}]
	}`, ts, projectID, projectID)
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPool_DirScanning 目录模式：拾取所有 .json 槽（忽略 .tmp 与子目录）。
func TestPool_DirScanning(t *testing.T) {
	dir := t.TempDir()
	writeMaterial(t, dir, "a.json", "proj-a", 0)
	writeMaterial(t, dir, "b.json", "proj-b", 0)
	writeMaterial(t, dir, "c.json", "proj-c", 0)
	// 干扰项：临时文件与子目录
	_ = os.WriteFile(filepath.Join(dir, "d.json.tmp"), []byte("{}"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)

	p := NewMaterialPool(dir, 2*time.Minute, func(string, ...any) {})
	n, err := p.Scan()
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("应装载 3 槽（.tmp 与目录需忽略），得到 %d", n)
	}
}

// TestPool_SingleFileDegradesToSingleSlot 单文件退化为单槽（向后兼容）。
func TestPool_SingleFileDegradesToSingleSlot(t *testing.T) {
	dir := t.TempDir()
	f := writeMaterial(t, dir, "one.json", "proj-one", 0)
	p := NewMaterialPool(f, 2*time.Minute, func(string, ...any) {})
	n, err := p.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("单文件应装载 1 槽，得到 %d", n)
	}
	mat, slot, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("取槽失败: %v", err)
	}
	defer slot.Release(nil)
	if mat.ProjectID != "proj-one" {
		t.Fatalf("材料错配: %s", mat.ProjectID)
	}
}

// TestPool_RejectsExpiredSlots 过期槽必须被拒绝（用坏材料会 400）。
func TestPool_RejectsExpiredSlots(t *testing.T) {
	dir := t.TempDir()
	writeMaterial(t, dir, "old.json", "proj-old", 10*time.Minute) // 远超 TTL
	p := NewMaterialPool(dir, 2*time.Minute, func(string, ...any) {})
	_, err := p.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Acquire(context.Background()); err == nil {
		t.Fatal("全部过期时应返回错误，而不是交出过期材料")
	}
	// 加一份新鲜的 → 应能取到
	writeMaterial(t, dir, "new.json", "proj-new", 0)
	p2 := NewMaterialPool(dir, 2*time.Minute, func(string, ...any) {})
	if _, err := p2.Scan(); err != nil {
		t.Fatal(err)
	}
	mat, slot, err := p2.Acquire(context.Background())
	if err != nil {
		t.Fatalf("有新鲜槽时应可取: %v", err)
	}
	defer slot.Release(nil)
	if mat.ProjectID != "proj-new" {
		t.Fatalf("应取到新鲜槽，却是 %s", mat.ProjectID)
	}
}

// TestPool_SkipsBrokenSlot 坏槽（缺 sandbox 字段）必须被跳过，不拖垮整池。
func TestPool_SkipsBrokenSlot(t *testing.T) {
	dir := t.TempDir()
	writeMaterial(t, dir, "good.json", "proj-good", 0)
	// 坏材料：缺 sandbox_url / sandbox_token
	_ = os.WriteFile(filepath.Join(dir, "bad.json"), []byte(`{"captured_at":"`+
		time.Now().UTC().Format(time.RFC3339)+`","project_id":"bad","metadata":{"projectId":"bad"}}`), 0o644)

	p := NewMaterialPool(dir, 2*time.Minute, func(string, ...any) {})
	if _, err := p.Scan(); err != nil {
		t.Fatal(err)
	}
	// 取 20 次：每次都应拿到 good（坏槽被跳过）
	for i := 0; i < 20; i++ {
		mat, slot, err := p.Acquire(context.Background())
		if err != nil {
			t.Fatalf("第 %d 次取槽失败: %v", i, err)
		}
		if mat.ProjectID != "proj-good" {
			t.Fatalf("取到坏槽: %s", mat.ProjectID)
		}
		slot.Release(nil)
	}
}

// TestPool_RoundRobinSpread 单并发时应轮转（摊平长期分布）。
func TestPool_RoundRobinSpread(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		writeMaterial(t, dir, n+".json", "proj-"+n, 0)
	}
	p := NewMaterialPool(dir, 2*time.Minute, func(string, ...any) {})
	if _, err := p.Scan(); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	const N = 300
	for i := 0; i < N; i++ {
		mat, slot, err := p.Acquire(context.Background())
		if err != nil {
			t.Fatalf("取槽失败: %v", err)
		}
		counts[mat.ProjectID]++
		slot.Release(nil) // 立即归还 → 负载恒为 0 → 纯轮转
	}
	if len(counts) != 3 {
		t.Fatalf("3 槽都应被用到，实际 %v", counts)
	}
	// 轮转应均匀（各约 N/3）
	for k, v := range counts {
		if v < N/3-5 || v > N/3+5 {
			t.Errorf("槽 %s 分布不均: %d（总 %d）", k, v, N)
		}
	}
}

// TestPool_SerializesPerSlot 一个沙箱同时只能跑一个对话：
// 槽被占用时必须等待（而不是并发打同一个槽 → 403），空闲后立刻放行。
func TestPool_SerializesPerSlot(t *testing.T) {
	dir := t.TempDir()
	writeMaterial(t, dir, "a.json", "proj-a", 0) // 只有一个槽
	p := NewMaterialPool(dir, 2*time.Minute, func(string, ...any) {})
	if _, err := p.Scan(); err != nil {
		t.Fatal(err)
	}

	// 占住唯一的槽
	_, held, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// 第二个请求必须等待（短超时验证它会阻塞而非立刻失败/并发进入）
	p.SetWaitTimeout(400 * time.Millisecond)
	start := time.Now()
	_, _, err = p.Acquire(context.Background())
	waited := time.Since(start)
	if err == nil {
		t.Fatal("槽忙时应等待超时报错，而不是并发进入同一沙箱")
	}
	if waited < 300*time.Millisecond {
		t.Errorf("应等待到超时（~400ms），实际仅 %s", waited.Round(time.Millisecond))
	}
	if !strings.Contains(err.Error(), "并发上限") {
		t.Errorf("错误信息应说明已达并发上限: %v", err)
	}

	// 释放后应立即能拿到
	held.Release(nil)
	p.SetWaitTimeout(2 * time.Second)
	start = time.Now()
	_, slot2, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("释放后应可获取: %v", err)
	}
	if waited2 := time.Since(start); waited2 > 500*time.Millisecond {
		t.Errorf("释放后应立刻获取，实际等了 %s", waited2.Round(time.Millisecond))
	}
	slot2.Release(nil)
}

// TestPool_MultiSlotEnablesParallel 多槽时多路并发可同时推进（真正的吞吐来源）。
func TestPool_MultiSlotEnablesParallel(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		writeMaterial(t, dir, n+".json", "proj-"+n, 0)
	}
	p := NewMaterialPool(dir, 2*time.Minute, func(string, ...any) {})
	if _, err := p.Scan(); err != nil {
		t.Fatal(err)
	}
	p.SetWaitTimeout(2 * time.Second)

	// 同时占住 3 个槽 → 3 路并发都应立即成功
	slots := make([]*Slot, 0, 3)
	for i := 0; i < 3; i++ {
		start := time.Now()
		_, slot, err := p.Acquire(context.Background())
		if err != nil {
			t.Fatalf("第 %d 路在 3 槽下应立即可得: %v", i+1, err)
		}
		if d := time.Since(start); d > 300*time.Millisecond {
			t.Errorf("第 %d 路等待过久: %s", i+1, d.Round(time.Millisecond))
		}
		slots = append(slots, slot)
	}
	// 三槽互不相同
	seen := map[int]bool{}
	for _, s := range slots {
		if seen[s.ID] {
			t.Fatalf("重复分配槽%d", s.ID)
		}
		seen[s.ID] = true
	}
	// 第 4 路必须等（无空闲槽）
	p.SetWaitTimeout(300 * time.Millisecond)
	if _, _, err := p.Acquire(context.Background()); err == nil {
		t.Error("3 槽全占满时第 4 路应等待超时")
	}
	for _, s := range slots {
		s.Release(nil)
	}
}

// TestPool_ConcurrentAcquireIsSafe 并发取槽/归还必须安全（配 -race）。
func TestPool_ConcurrentAcquireIsSafe(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 4; i++ {
		writeMaterial(t, dir, fmt.Sprintf("s%d.json", i), fmt.Sprintf("proj-%d", i), 0)
	}
	p := NewMaterialPool(dir, 2*time.Minute, func(string, ...any) {})
	if _, err := p.Scan(); err != nil {
		t.Fatal(err)
	}

	const N = 200
	var wg sync.WaitGroup
	var mu sync.Mutex
	used := map[int]int{}
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mat, slot, err := p.Acquire(context.Background())
			if err != nil {
				return
			}
			if mat == nil {
				t.Error("取到 nil 材料")
				slot.Release(nil)
				return
			}
			mu.Lock()
			used[slot.ID]++
			mu.Unlock()
			slot.Release(nil)
		}()
	}
	wg.Wait()
	if len(used) != 4 {
		t.Errorf("4 槽都应被并发用到，实际用到 %d 个", len(used))
	}
	// 归还后所有槽在飞应为 0（无泄漏）
	for _, s := range p.slots {
		if n := s.Inflight(); n != 0 {
			t.Errorf("槽%d 归还后仍有 %d 在飞", s.ID, n)
		}
	}
}

// TestPool_DetectsRescanAfterSlotAppears 目录出现新槽后应能自动拾取。
func TestPool_DetectsRescanAfterSlotAppears(t *testing.T) {
	dir := t.TempDir()
	writeMaterial(t, dir, "a.json", "proj-a", 0)
	p := NewMaterialPool(dir, 2*time.Minute, func(string, ...any) {})
	if _, err := p.Scan(); err != nil {
		t.Fatal(err)
	}
	if p.Size() != 1 {
		t.Fatalf("初始应 1 槽，得到 %d", p.Size())
	}
	// 新增槽 + 强制重扫
	writeMaterial(t, dir, "b.json", "proj-b", 0)
	if _, err := p.Scan(); err != nil {
		t.Fatal(err)
	}
	if p.Size() != 2 {
		t.Fatalf("新增后应 2 槽，得到 %d", p.Size())
	}
}

// TestPool_SlotStatsPersistAcrossRescan 重扫应保留已有槽的统计（不丢计数）。
func TestPool_SlotStatsPersistAcrossRescan(t *testing.T) {
	dir := t.TempDir()
	writeMaterial(t, dir, "a.json", "proj-a", 0)
	p := NewMaterialPool(dir, 2*time.Minute, func(string, ...any) {})
	if _, err := p.Scan(); err != nil {
		t.Fatal(err)
	}
	_, slot, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slot.Release(nil)

	// 重扫后统计应保留
	if _, err := p.Scan(); err != nil {
		t.Fatal(err)
	}
	if got := p.slots[0].Successes(); got != 1 {
		t.Fatalf("重扫后成功计数丢失: %d", got)
	}
}

// TestPool_FailureCounting 失败计数 + **失败后退避**（槽级熔断）。
//
// 新行为：某槽失败后进入退避窗口，期间不再放行（因为上游对刚失败的身份
// 立刻再打必然继续 403）。退避结束后恢复可用。
func TestPool_FailureCounting(t *testing.T) {
	dir := t.TempDir()
	writeMaterial(t, dir, "a.json", "proj-a", 0)
	p := NewMaterialPool(dir, 2*time.Minute, func(string, ...any) {})
	if _, err := p.Scan(); err != nil {
		t.Fatal(err)
	}
	_, slot, _ := p.Acquire(context.Background())
	slot.Release(fmt.Errorf("模拟上游 403"))

	// 失败后：该槽应进入退避 → 立即再取应失败（不再白打上游）
	if _, _, err := p.Acquire(context.Background()); err == nil {
		t.Error("失败后槽应退避，Acquire 应返回错误")
	}
	if !slot.inCooldown() {
		t.Error("失败后槽应处于退避窗口")
	}

	// 等退避结束 → 恢复可用，且成功一次即清零退避
	time.Sleep(slot.cooldownD + 60*time.Millisecond)
	_, slot2, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("退避结束后应恢复可用: %v", err)
	}
	slot2.Release(nil)
	if slot2.inCooldown() {
		t.Error("成功一次后应清零退避")
	}

	st := p.Stats()
	slots, _ := st["slots"].([]SlotStat)
	if len(slots) != 1 {
		t.Fatalf("stats 应有 1 槽，得到 %d", len(slots))
	}
	if slots[0].Failures != 1 || slots[0].Successes != 1 {
		t.Errorf("统计错误: %+v", slots[0])
	}
	if st["mode"] != "multi_slot" && st["mode"] != "single_file" {
		t.Errorf("mode 异常: %v", st["mode"])
	}
}

// TestPool_CooldownBacksOff 连续失败时退避时长须**递增**（上限 5s），
// 成功一次即清零 —— 这是抗上游限流的关键。
func TestPool_CooldownBacksOff(t *testing.T) {
	dir := t.TempDir()
	writeMaterial(t, dir, "a.json", "proj-a", 0)
	p := NewMaterialPool(dir, 2*time.Minute, func(string, ...any) {})
	if _, err := p.Scan(); err != nil {
		t.Fatal(err)
	}
	_, slot, _ := p.Acquire(context.Background())

	var prev time.Duration
	for i := 0; i < 5; i++ {
		slot.Release(fmt.Errorf("403"))
		slot.failMu.Lock()
		d := slot.cooldownD
		slot.failMu.Unlock()
		if d < prev {
			t.Errorf("退避应递增: 第 %d 次 %s < 上次 %s", i, d, prev)
		}
		if d > 5*time.Second {
			t.Errorf("退避不应超过 5s，得到 %s", d)
		}
		prev = d
	}
	slot.clearFailure()
	slot.failMu.Lock()
	d := slot.cooldownD
	slot.failMu.Unlock()
	if d != 0 {
		t.Errorf("成功后退避应清零，得到 %s", d)
	}
}

// TestPool_StatsShape 诊断输出结构（管理台消费）。
func TestPool_StatsShape(t *testing.T) {
	dir := t.TempDir()
	writeMaterial(t, dir, "a.json", "proj-a", 0)
	writeMaterial(t, dir, "b.json", "proj-b", 10*time.Minute) // 过期
	p := NewMaterialPool(dir, 2*time.Minute, func(string, ...any) {})
	if _, err := p.Scan(); err != nil {
		t.Fatal(err)
	}
	st := p.Stats()
	if st["slots_total"] != 2 {
		t.Errorf("slots_total 应为 2: %v", st["slots_total"])
	}
	if st["slots_fresh"] != 1 {
		t.Errorf("slots_fresh 应为 1（另一槽已过期）: %v", st["slots_fresh"])
	}
	if st["ttl_seconds"] != 120 {
		t.Errorf("ttl_seconds 应为 120: %v", st["ttl_seconds"])
	}
}
