package prism

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ============================================================================
// 号池 + 并发控制。
//
// 设计要点（来自 12 个实现的教训）：
//   - 一个账号一个 Client（连接池/沙箱/会话都隔离，坏账号不拖累全池）。
//   - 账号内并发用信号量限制：单账号超过 ~8 并发时 start 容易被上游挂死
//     （实测单身份安全 80 RPM，429 边缘 130）。
//   - 选号：跳过冷却中的账号，按「在飞请求数 / 上次使用时间」做最小负载优先
//     （低延迟关键：避免把请求压到正在跑长任务的账号上）。
//   - 高并发关键：建沙箱走 singleflight（Client 内已实现），N 并发只付一次冷链；
//     后台预热让首个请求也走热路径（TTFB 数量级改善）。
// ============================================================================

// Pool 是账号池。
type Pool struct {
	mu       sync.RWMutex
	clients  []*Client
	sem      map[string]chan struct{} // 账号 id → 并发信号量
	inflight map[string]*int64        // 账号 id → 在飞计数
	lastUse  map[string]time.Time
	cond     *sync.Cond
	opt      PoolOptions
	closed   bool

	// 观测
	picked   int64
	rejected int64
	waited   int64

	// breaker 是「上游整体劣化」熔断器：打开期间快速失败，不打上游。
	breaker *Breaker
}

// PoolOptions 是池的可调项。
type PoolOptions struct {
	Client          Options
	PerAccountConc  int           // 单账号并发上限
	CooldownOnErr   time.Duration // 认证错误冷却
	RetryBackoffMin time.Duration
	RetryBackoffMax time.Duration
	PrewarmWorkers  int
	WarmOnStart     bool
	// SandboxTTL 沙箱热复用窗口；预热按 TTL/2 主动重铸，避免「命中即用但
	// 下一秒过期」的长尾冷启动。
	SandboxTTL time.Duration
	// BreakerWindow/MinRate/MinSamples/ProbeGap 熔断参数（零值用实测默认）。
	BreakerWindow   time.Duration
	BreakerMinRate  float64
	BreakerMinSamp  int
	BreakerProbeGap time.Duration
	// WarmSetSize 每轮预热最多处理的账号数（按 MRU 取前 K）。
	WarmSetSize int
	Logf        func(format string, args ...any)
}

// NewPool 建池。
func NewPool(accounts []*Account, opt PoolOptions) *Pool {
	if opt.PerAccountConc < 1 {
		opt.PerAccountConc = 8
	}
	if opt.CooldownOnErr <= 0 {
		opt.CooldownOnErr = 2 * time.Minute
	}
	if opt.RetryBackoffMin <= 0 {
		opt.RetryBackoffMin = 1500 * time.Millisecond
	}
	if opt.RetryBackoffMax <= 0 {
		opt.RetryBackoffMax = 10 * time.Second
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	if opt.SandboxTTL <= 0 {
		opt.SandboxTTL = 20 * time.Minute
	}
	if opt.WarmSetSize <= 0 {
		opt.WarmSetSize = 16
	}
	p := &Pool{
		sem:      make(map[string]chan struct{}, len(accounts)),
		inflight: make(map[string]*int64, len(accounts)),
		lastUse:  make(map[string]time.Time, len(accounts)),
		opt:      opt,
		breaker:  NewBreaker(opt.BreakerWindow, opt.BreakerMinRate, opt.BreakerMinSamp, opt.BreakerProbeGap),
	}
	p.cond = sync.NewCond(&p.mu)
	for _, a := range accounts {
		c := NewClient(opt.Client, a)
		p.clients = append(p.clients, c)
		p.sem[a.ID] = make(chan struct{}, opt.PerAccountConc)
		var n int64
		p.inflight[a.ID] = &n
	}
	if opt.WarmOnStart {
		p.StartWarmer(context.Background())
	}
	return p
}

// StartWarmer 启动后台预热：为每个账号预热一个热沙箱。
func (p *Pool) StartWarmer(ctx context.Context) {
	workers := p.opt.PrewarmWorkers
	if workers < 1 {
		workers = 1
	}
	go p.warmerLoop(ctx, workers)
}

// warmerLoop 周期性预热：按最近使用（MRU）取前 K 个账号，沙箱年龄超过 TTL/2 即
// 提前重铸（而不是等到过期才发现冷启动），并把「命中即刷新」补上——这样热池
// 的最长冷窗口从 TTL 降到 TTL/2。
func (p *Pool) warmerLoop(ctx context.Context, workers int) {
	// 抖动避免多实例同频（±30s 内随机）。
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	sem := make(chan struct{}, workers)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if sleepCtx(ctx, jitter(30*time.Second)) != nil {
			return
		}
		// 上游熔断打开时不预热：过载期重铸沙箱纯属补刀。
		if !p.breaker.Allow() {
			p.opt.Logf("预热跳过：上游熔断中")
			continue
		}
		for _, c := range p.warmSet() {
			if c.Account().InCooldown(time.Now()) {
				continue
			}
			// 沙箱仍新鲜（年龄 < TTL/2）则跳过。
			if age := c.SandboxAge(); age > 0 && age < p.opt.SandboxTTL/2 {
				continue
			}
			select {
			case sem <- struct{}{}:
			default:
				continue
			}
			go func(c *Client) {
				defer func() { <-sem }()
				wctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
				defer cancel()
				if err := c.EnsureSession(wctx); err != nil {
					p.opt.Logf("预热: 账号 %s session 失败: %v", c.Account().ID, err)
					return
				}
				pid, err := c.EnsureProject(wctx)
				if err != nil {
					p.opt.Logf("预热: 账号 %s 项目失败: %v", c.Account().ID, err)
					return
				}
				if _, _, err := c.EnsureSandbox(wctx, pid); err != nil {
					p.opt.Logf("预热: 账号 %s 沙箱失败: %v", c.Account().ID, err)
					return
				}
				p.opt.Logf("预热: 账号 %s 热沙箱就绪 ✓", c.Account().ID)
			}(c)
		}
	}
}

// Clients 返回全部客户端（只读快照）。
func (p *Pool) Clients() []*Client {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Client, len(p.clients))
	copy(out, p.clients)
	return out
}

// Len 返回账号数。
func (p *Pool) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.clients)
}

// Lease 是一个已占用的账号槽位（用完必须 Release）。
type Lease struct {
	Client *Client
	pool   *Pool
	once   sync.Once
	bucket chan struct{}
	acctID string
}

// Release 归还槽位（幂等）。
func (l *Lease) Release() {
	if l == nil || l.pool == nil {
		return
	}
	l.once.Do(func() {
		l.pool.mu.Lock()
		if n := l.pool.inflight[l.acctID]; n != nil {
			atomic.AddInt64(n, -1)
		}
		l.pool.lastUse[l.acctID] = time.Now()
		l.pool.cond.Broadcast()
		l.pool.mu.Unlock()
		<-l.bucket
	})
}

// Acquire 取一个账号槽位（阻塞直到有空位或 ctx 结束）。
// 选号：跳过冷却账号，取「在飞最少 → 空闲最久」者。
func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, fmt.Errorf("号池已关闭")
		}
		c := p.bestLocked()
		if c != nil && len(p.sem[c.Account().ID]) < cap(p.sem[c.Account().ID]) {
			bucket := p.sem[c.Account().ID]
			atomic.AddInt64(p.inflight[c.Account().ID], 1)
			p.lastUse[c.Account().ID] = time.Now()
			p.picked++
			p.mu.Unlock()
			select {
			case bucket <- struct{}{}:
				return &Lease{Client: c, pool: p, bucket: bucket, acctID: c.Account().ID}, nil
			case <-ctx.Done():
				p.mu.Lock()
				atomic.AddInt64(p.inflight[c.Account().ID], -1)
				p.mu.Unlock()
				return nil, ctx.Err()
			}
		}
		p.mu.Unlock()

		// 没有可用槽位：等待释放或被唤醒（带 ctx 兜底）。
		p.mu.Lock()
		allCooling := true
		for _, cc := range p.clients {
			if !cc.Account().InCooldown(time.Now()) {
				allCooling = false
				break
			}
		}
		p.mu.Unlock()
		if allCooling && p.Len() > 0 {
			return nil, fmt.Errorf("全部 %d 个账号处于错误冷却窗口", p.Len())
		}

		waitCh := make(chan struct{})
		go func() {
			p.mu.Lock()
			p.cond.Wait()
			p.mu.Unlock()
			close(waitCh)
		}()
		select {
		case <-ctx.Done():
			p.mu.Lock()
			p.cond.Broadcast()
			p.mu.Unlock()
			return nil, ctx.Err()
		case <-waitCh:
		case <-time.After(200 * time.Millisecond):
			p.mu.Lock()
			p.cond.Broadcast()
			p.mu.Unlock()
		}
		p.mu.Lock()
		p.waited++
		p.mu.Unlock()
	}
}

// bestLocked 选出最优账号（须持有 p.mu）。
func (p *Pool) bestLocked() *Client {
	now := time.Now()
	type cand struct {
		c    *Client
		fly  int64
		idle time.Duration
		hot  bool
	}
	var cands []cand
	for _, c := range p.clients {
		a := c.Account()
		if a.InCooldown(now) {
			continue
		}
		id := a.ID
		fly := int64(0)
		if n := p.inflight[id]; n != nil {
			fly = atomic.LoadInt64(n)
		}
		idle := now.Sub(p.lastUse[id])
		cands = append(cands, cand{c: c, fly: fly, idle: idle, hot: c.SandboxToken() != ""})
	}
	if len(cands) == 0 {
		return nil
	}
	sort.SliceStable(cands, func(i, j int) bool {
		// ① 有热沙箱的优先（TTFB 关键，跳过 6~18s 冷链）
		if cands[i].hot != cands[j].hot {
			return cands[i].hot
		}
		// ② 在飞少的优先
		if cands[i].fly != cands[j].fly {
			return cands[i].fly < cands[j].fly
		}
		// ③ 空闲久的优先（打散负载）
		return cands[i].idle > cands[j].idle
	})
	// 在飞已达上限的账号不再选（外层会等待）。
	best := cands[0]
	if int(best.fly) >= cap(p.sem[best.c.Account().ID]) {
		return nil
	}
	return best.c
}

// Do 是便捷入口：取号 → 执行 → 释放。
//
// 处置策略由三态分类驱动（见 failclass.go）：
//   - KindAuth / KindQuota → 账号冷却 + 换号
//   - KindRequest          → 快速失败（换号零收益）
//   - KindBroken           → 熔断退避（不打上游补刀），可换号一次
//   - KindCancel / 成功     → 直接返回
//
// 熔断打开时入口直接快速失败，返回 *DegradedError（上层映射 503 + Retry-After）。
func (p *Pool) Do(ctx context.Context, maxAttempts int, fn func(ctx context.Context, c *Client) error) error {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if !p.breaker.Allow() {
		st := p.breaker.Stats()
		return &DegradedError{RetryAfter: p.opt.BreakerProbeGap, Stats: st}
	}

	var lastErr error
	backoff := p.opt.RetryBackoffMin
	for i := 0; i < maxAttempts; i++ {
		if i > 0 {
			if err := sleepCtx(ctx, backoff); err != nil {
				return err
			}
			if backoff < p.opt.RetryBackoffMax {
				backoff *= 2
				if backoff > p.opt.RetryBackoffMax {
					backoff = p.opt.RetryBackoffMax
				}
			}
		}
		lease, err := p.Acquire(ctx)
		if err != nil {
			if lastErr != nil {
				return lastErr
			}
			return err
		}
		err = fn(ctx, lease.Client)
		if err == nil {
			lease.Client.Account().NoteOK()
			lease.Client.TouchSandbox()
			p.MarkUsed(lease.Client.Account().ID)
			p.breaker.Record(false, KindUnknown)
			lease.Release()
			return nil
		}
		lastErr = err

		cls := Classify(err)
		p.breaker.Record(true, cls.Kind)

		// 熔断刚打开：立即按劣化返回，不再重试。
		if st := p.breaker.Stats(); st.Open {
			lease.Release()
			return &DegradedError{RetryAfter: p.opt.BreakerProbeGap, Stats: st}
		}

		if cls.Cool {
			lease.Client.Account().NoteErr(err.Error(), p.opt.CooldownOnErr)
		}
		switch cls.Kind {
		case KindBroken, KindQuota, KindAuth:
			lease.Client.InvalidateSandbox()
		case KindTransport:
			// 传输层抖动：沙箱多半无辜，首次不杀（连续两犯由 Client 内部处理）。
		case KindCancel, KindRequest:
			lease.Client.Account().NoteErr(err.Error(), 0)
			lease.Release()
			return err
		}
		if !cls.Swit {
			lease.Release()
			return err
		}
		lease.Release()
	}
	return lastErr
}

// Close 关闭池（幂等）。
// Breaker 返回熔断器（观测与入口快速失败用）。
func (p *Pool) Breaker() *Breaker { return p.breaker }

// warmSet 按最近使用时间取前 K 个账号（MRU：热用户优先保温）。
func (p *Pool) warmSet() []*Client {
	k := p.opt.WarmSetSize
	p.mu.RLock()
	clients := make([]*Client, len(p.clients))
	copy(clients, p.clients)
	type ent struct {
		c    *Client
		last time.Time
	}
	ents := make([]ent, 0, len(clients))
	for _, c := range clients {
		ents = append(ents, ent{c: c, last: p.lastUse[c.Account().ID]})
	}
	p.mu.RUnlock()
	sort.SliceStable(ents, func(i, j int) bool { return ents[i].last.After(ents[j].last) })
	out := make([]*Client, 0, k)
	for i, e := range ents {
		if i >= k {
			break
		}
		out = append(out, e.c)
	}
	return out
}

// jitter 在 [0, d] 内随机等待（多实例不同频）。
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	n := binary.BigEndian.Uint64(b[:])
	return time.Duration(n % uint64(d))
}

func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

// Stats 返回池级观测。
func (p *Pool) Stats() (picked, waited, rejected int64, perAccount map[string]int64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	perAccount = make(map[string]int64, len(p.clients))
	for id, n := range p.inflight {
		perAccount[id] = atomic.LoadInt64(n)
	}
	return p.picked, p.waited, p.rejected, perAccount
}

// isTransient 判断是否为传输层抖动（超时/断连），值得换号重试。
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{"timeout", "timed out", "connection reset", "broken pipe", "eof", "protocol_error", "no such host", "tls"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// MarkUsed 刷新账号最近使用时间（MRU 预热排序依赖它）。
func (p *Pool) MarkUsed(acctID string) {
	p.mu.Lock()
	p.lastUse[acctID] = time.Now()
	p.mu.Unlock()
}
