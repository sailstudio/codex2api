package prism

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================
// 材料池（多槽）。
//
// 为什么需要：上游按**浏览器身份**限流（单身份实测约 80 RPM），而一份材料
// 就绑定一个身份。要提吞吐只能**多身份轮转** —— N 份材料（N 个浏览器上下文
// / N 个沙箱）并行供票，理论吞吐 N×80 RPM。
//
// 选择策略（三级）：
//  1. **只挑新鲜槽**（超过 TTL 的槽直接跳过；全过期则返回错误而非用坏材料）
//  2. **优先在飞请求少的槽**（负载均衡，避免热点槽被限流）
//  3. **同负载时轮转（round-robin）**（摊平长期分布）
//
// 失败处理：某槽出错 → 记录失败计数并**换下一个槽重试**（由调用方决定次数），
// 避免单槽坏掉拖垮整个通道。
// ============================================================

// Slot 是材料池里的一槽（对应一份材料 / 一个浏览器身份）。
type Slot struct {
	ID   int
	Path string

	mu       sync.RWMutex
	mat      *Material
	loadedAt time.Time
	loadErr  error

	inflight  int64 // 在飞请求数
	successes int64
	failures  int64
	lastUsed  time.Time

	// 熔断（槽被上游拒过一次就短暂冷却）：
	// 上游对「同一身份打太快」会返回 403 "Please submit prompt again"，
	// 对刚失败的槽继续打只会继续失败，因此退避后再放行。
	failMu     sync.Mutex
	cooldownAt time.Time
	cooldownD  time.Duration // 当前退避时长（递增）
}

// MaterialPool 管理多槽材料。
type MaterialPool struct {
	mu      sync.RWMutex
	source  string // 目录或单文件
	isDir   bool
	ttl     time.Duration
	slots   []*Slot
	rr      int
	logf    func(string, ...any)
	scanned time.Time

	// waitTimeout 是等待槽空闲的最长时间（0 = 不等待，立即返回错误）。
	waitTimeout time.Duration

	// maxPerSlot 是**每槽允许的在飞请求数**（默认 1）。
	//
	// 默认 1 的依据：早期压测里并发打同一槽出现过 403
	// "Error while processing conversation"。但后来查明那次并发失败
	// 的真因是 **sentinel token 被复用**（铸造服务单飞去重）——
	// 修掉之后需要重新标定真实上限。本参数供标定与生产调优使用：
	// 上游允许的话，单账号也能靠"多会话并发"提升吞吐，而不必多账号。
	maxPerSlot int64
}

// NewMaterialPool 建池。source 可以是：
//   - 目录：扫描其中所有 *.json 作为槽（推荐，多身份）
//   - 单文件：退化为单槽（向后兼容）
func NewMaterialPool(source string, ttl time.Duration, logf func(string, ...any)) *MaterialPool {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	st, err := os.Stat(source)
	isDir := err == nil && st.IsDir()
	return &MaterialPool{source: source, isDir: isDir, ttl: ttl, logf: logf}
}

// Scan 重新扫描材料源（幂等；目录模式下会拾取新增/删除的槽）。
func (p *MaterialPool) Scan() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var paths []string
	if p.isDir {
		entries, err := os.ReadDir(p.source)
		if err != nil {
			return 0, fmt.Errorf("prism: 材料目录不可读（%s）: %w", p.source, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			if strings.HasSuffix(e.Name(), ".tmp") {
				continue
			}
			paths = append(paths, filepath.Join(p.source, e.Name()))
		}
	} else {
		paths = []string{p.source}
	}
	// 稳定顺序，保证槽 ID 可复现
	sort.Strings(paths)

	// 复用已有槽（按路径），保留其统计
	byPath := map[string]*Slot{}
	for _, s := range p.slots {
		byPath[s.Path] = s
	}
	var next []*Slot
	for i, path := range paths {
		if old, ok := byPath[path]; ok {
			old.ID = i
			next = append(next, old)
			delete(byPath, path)
		} else {
			next = append(next, &Slot{ID: i, Path: path})
		}
	}
	p.slots = next
	p.scanned = time.Now()
	if len(next) > 1 {
		p.logf("prism: 材料池已装载 %d 槽（%s）", len(next), p.source)
	}
	return len(next), nil
}

// Size 返回槽数。
func (p *MaterialPool) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.slots)
}

// SetWaitTimeout 设置等待槽空闲的最长时间（默认 90s；0 表示不等待）。
func (p *MaterialPool) SetWaitTimeout(d time.Duration) {
	p.mu.Lock()
	p.waitTimeout = d
	p.mu.Unlock()
}

// SetMaxPerSlot 设置每槽允许的在飞请求数（默认 1；≤0 视为 1）。
//
// 上游是否真的限制"一沙箱一对话"需要实验标定：若允许 N>1，
// 单账号即可通过多会话并发提升吞吐（见 docs 里的标定结论）。
func (p *MaterialPool) SetMaxPerSlot(n int64) {
	if n < 1 {
		n = 1
	}
	p.mu.Lock()
	p.maxPerSlot = n
	p.mu.Unlock()
}

// MaxPerSlot 返回当前每槽并发上限。
func (p *MaterialPool) MaxPerSlot() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.maxPerSlot < 1 {
		return 1
	}
	return p.maxPerSlot
}

// Acquire 取一个可用槽并返回其材料。
//
// 准入规则：槽的在飞请求数 < maxPerSlot（默认 1；见 SetMaxPerSlot）。
// 早期把默认设为 1，是因为并发打同一槽出现过 403
// "Error while processing conversation"；但那次失败的真因是
// sentinel token 被复用（已修）。真实上限由标定实验确定。
func (p *MaterialPool) Acquire(ctx context.Context) (*Material, *Slot, error) {
	if p.Size() == 0 {
		if _, err := p.Scan(); err != nil {
			return nil, nil, err
		}
		if p.Size() == 0 {
			return nil, nil, fmt.Errorf("prism: 材料池为空（源: %s）", p.source)
		}
	}

	// 周期性重扫目录（拾取新增槽 / 丢弃删除槽）
	p.mu.RLock()
	needRescan := p.isDir && time.Since(p.scanned) > 15*time.Second
	waitTimeout := p.waitTimeout
	maxPer := p.maxPerSlot
	p.mu.RUnlock()
	if needRescan {
		_, _ = p.Scan()
	}
	if waitTimeout <= 0 {
		waitTimeout = 90 * time.Second
	}
	if maxPer < 1 {
		maxPer = 1
	}

	p.mu.RLock()
	candidates := make([]*Slot, len(p.slots))
	copy(candidates, p.slots)
	p.mu.RUnlock()

	deadline := time.Now().Add(waitTimeout)
	var reasons []string
	for {
		// 先挑新鲜且**未超每槽上限**的槽（轮转以摊平长期分布）
		type cand struct {
			slot *Slot
			mat  *Material
		}
		var avail []cand
		reasons = reasons[:0]

		for _, s := range candidates {
			if atomic.LoadInt64(&s.inflight) >= maxPer {
				continue // 该槽达上限（默认 1 → 一沙箱一对话）
			}
			if s.inCooldown() {
				reasons = append(reasons, fmt.Sprintf("槽%d:退避中", s.ID))
				continue // 刚失败过 → 冷却，避免继续打（否则必然再 403）
			}
			mat, err := s.load(p.ttl)
			if err != nil {
				reasons = append(reasons, fmt.Sprintf("槽%d:%v", s.ID, shortErr(err)))
				continue
			}
			avail = append(avail, cand{slot: s, mat: mat})
		}

		if len(avail) > 0 {
			p.mu.Lock()
			idx := p.rr % len(avail)
			p.rr++
			p.mu.Unlock()
			chosen := avail[idx]

			// 抢占用权：只要仍在上限内就占（CAS 防两 goroutine 同时认为可用）
			for {
				cur := atomic.LoadInt64(&chosen.slot.inflight)
				if cur >= maxPer {
					break // 被别人抢满，重挑
				}
				if atomic.CompareAndSwapInt64(&chosen.slot.inflight, cur, cur+1) {
					chosen.slot.mu.Lock()
					chosen.slot.lastUsed = time.Now()
					chosen.slot.mu.Unlock()
					return chosen.mat, chosen.slot, nil
				}
			}
			continue
		}

		// 没有可用槽：先区分「槽忙」与「槽不可用」。
		if len(reasons) == len(candidates) && len(candidates) > 0 {
			return nil, nil, fmt.Errorf("prism: 无可用材料槽（%d 槽全部不可用: %s）",
				len(candidates), strings.Join(reasons, "; "))
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("prism: 全部 %d 个材料槽均达并发上限（上限 %d/槽，"+
				"总并发 %d），已等待 %s —— 可增加槽数（PRISM_SLOTS）或调高每槽上限",
				len(candidates), maxPer, int64(len(candidates))*maxPer, waitTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(40 * time.Millisecond):
		}
	}
}

// Release 归还槽占用。err 非空表示本轮失败 → 触发槽级退避。
//
// 退避的意义：上游对同一身份的高频请求会 403；失败后立刻再打
// 只会继续失败（实测）。让槽冷却一小段时间，再放行下一个请求。
func (s *Slot) Release(err error) {
	if s == nil {
		return
	}
	atomic.AddInt64(&s.inflight, -1)
	if err != nil {
		atomic.AddInt64(&s.failures, 1)
		s.markFailure()
	} else {
		atomic.AddInt64(&s.successes, 1)
		s.clearFailure()
	}
}

// markFailure 记录一次失败并设置递增退避（上限 5s）。
func (s *Slot) markFailure() {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	if s.cooldownD < 500*time.Millisecond {
		s.cooldownD = 500 * time.Millisecond
	} else {
		s.cooldownD *= 2
	}
	if s.cooldownD > 5*time.Second {
		s.cooldownD = 5 * time.Second
	}
	s.cooldownAt = time.Now().Add(s.cooldownD)
}

// clearFailure 一次成功后清零退避（身份又健康了）。
func (s *Slot) clearFailure() {
	s.failMu.Lock()
	s.cooldownD = 0
	s.cooldownAt = time.Time{}
	s.failMu.Unlock()
}

// inCooldown 返回槽是否仍在退避窗口内。
func (s *Slot) inCooldown() bool {
	s.failMu.Lock()
	defer s.failMu.Unlock()
	return time.Now().Before(s.cooldownAt)
}

// load 读并缓存材料（含 TTL 判定）。
func (s *Slot) load(ttl time.Duration) (*Material, error) {
	s.mu.RLock()
	mat, loadedAt, err := s.mat, s.loadedAt, s.loadErr
	s.mu.RUnlock()

	// 命中缓存：文件未更新且未过期
	if err == nil && mat != nil && time.Since(loadedAt) < ttl {
		return mat, nil
	}

	m, e := loadMaterial(s.Path)
	s.mu.Lock()
	s.mat, s.loadedAt, s.loadErr = m, time.Now(), e
	s.mu.Unlock()
	if e != nil {
		return nil, e
	}
	// 检查材料自身的时间戳新鲜度（侧车写盘时间）。
	//
	// ⚠️ 过期判定**必须缓存**：只 return error 而不记 loadErr 的话，
	// 下一次调用会命中「mat!=nil && loadErr==nil」的缓存分支，
	// 把这份过期材料当成新鲜的交出去（曾因此把坏材料发给上游 → 400）。
	if !m.CapturedAt.IsZero() {
		if age := time.Since(m.CapturedAt); age > ttl {
			expired := fmt.Errorf("材料已过期 %s（TTL %s）", age.Round(time.Second), ttl)
			s.mu.Lock()
			s.loadErr = expired
			s.mu.Unlock()
			return nil, expired
		}
	}
	return m, nil
}

// SlotStat 是单槽诊断信息。
type SlotStat struct {
	ID        int    `json:"id"`
	Path      string `json:"path"`
	Fresh     bool   `json:"fresh"`
	AgeSec    int    `json:"age_seconds"`
	ProjectID string `json:"project_id,omitempty"`
	Inflight  int64  `json:"inflight"`
	Successes int64  `json:"successes"`
	Failures  int64  `json:"failures"`
	Err       string `json:"error,omitempty"`
}

// Stats 返回全池诊断（供管理台/排障）。
func (p *MaterialPool) Stats() map[string]any {
	p.mu.RLock()
	empty := len(p.slots) == 0
	scannedZero := p.scanned.IsZero()
	p.mu.RUnlock()
	// 从未扫描（或扫出 0 槽）时先扫一次，否则管理台会看到空池。
	if scannedZero || empty {
		_, _ = p.Scan()
	}

	p.mu.RLock()
	slots := make([]*Slot, len(p.slots))
	copy(slots, p.slots)
	isDir := p.isDir
	source := p.source
	p.mu.RUnlock()

	out := make([]SlotStat, 0, len(slots))
	freshN, totalInflight := 0, int64(0)
	for _, s := range slots {
		st := SlotStat{
			ID: s.ID, Path: s.Path,
			Inflight:  atomic.LoadInt64(&s.inflight),
			Successes: atomic.LoadInt64(&s.successes),
			Failures:  atomic.LoadInt64(&s.failures),
		}
		totalInflight += st.Inflight

		// 必须先加载，才能拿到材料与其时间戳（未加载的槽 loadedAt 为零值，
		// 直接判新鲜度会把它误判为过期）。
		m, err := s.load(p.ttl)
		if err != nil {
			st.Err = err.Error()
			// 即使加载失败也报出磁盘上的年龄，便于排障
			if raw, rerr := loadMaterial(s.Path); rerr == nil && !raw.CapturedAt.IsZero() {
				st.AgeSec = int(time.Since(raw.CapturedAt).Seconds())
			}
		} else {
			st.ProjectID = m.ProjectID
			// 用材料自身的捕获时间计算年龄（比"加载时刻"准确：
			// 材料可能 100 秒前写出、刚刚才被加载，按加载时刻会低报年龄）。
			if !m.CapturedAt.IsZero() {
				st.AgeSec = int(time.Since(m.CapturedAt).Seconds())
			} else {
				s.mu.RLock()
				if !s.loadedAt.IsZero() {
					st.AgeSec = int(time.Since(s.loadedAt).Seconds())
				}
				s.mu.RUnlock()
			}
			st.Fresh = st.AgeSec < int(p.ttl.Seconds())
			if st.Fresh {
				freshN++
			}
		}
		out = append(out, st)
	}
	return map[string]any{
		"mode":           map[bool]string{true: "multi_slot", false: "single_file"}[isDir],
		"source":         source,
		"slots_total":    len(out),
		"slots_fresh":    freshN,
		"inflight_total": totalInflight,
		"ttl_seconds":    int(p.ttl.Seconds()),
		"slots":          out,
	}
}

func shortErr(err error) string {
	if err == nil {
		return ""
	}
	m := err.Error()
	if len(m) > 60 {
		return m[:60] + "…"
	}
	return m
}

// Inflight 返回当前在飞请求数。
func (s *Slot) Inflight() int64 { return atomic.LoadInt64(&s.inflight) }

// Successes 返回成功计数。
func (s *Slot) Successes() int64 { return atomic.LoadInt64(&s.successes) }

// Failures 返回失败计数。
func (s *Slot) Failures() int64 { return atomic.LoadInt64(&s.failures) }
