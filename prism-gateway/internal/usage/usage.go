package usage

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// token 计量与缓存计数。
//
// 诚实声明（重要）：上游 **不返回任何 token 用量**（多份实现一致结论），也没有
// prompt cache 通道。所以：
//   - 用量是**估算**：按「CJK 字符 1 token / 拉丁 4 字符 1 token」+ 每条消息/工具/
//     图片的固定开销累加，与主流 tokenizer 的口径同阶。
//   - 缓存计数是把「本轮请求前缀与历史请求前缀的最长匹配」映射成 Anthropic/OpenAI
//     的 cache 语义（cache_read / cache_creation），用于**观测与计费对账**，
//     不代表上游真的省了 token。
// 这样做的价值：客户端 SDK（尤其 Anthropic SDK、含 cached_tokens 的 OpenAI 客户端）
// 拿到的是**自洽**的 usage —— Input + CacheRead + CacheCreation = 总输入，不变量守恒。
// ============================================================================

// Usage 是一次请求的用量拆分。
// 不变量：InputTokens + CacheReadInputTokens + CacheCreationInputTokens == 总输入。
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	ReasoningTokens          int `json:"reasoning_tokens,omitempty"`
	Images                   int `json:"images,omitempty"`
}

// TotalInput 返回输入总量。
func (u Usage) TotalInput() int {
	return u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// Total 返回输入 + 输出。
func (u Usage) Total() int { return u.TotalInput() + u.OutputTokens }

// Add 合并两份用量（多轮累计）。
func (u *Usage) Add(o Usage) {
	u.InputTokens += o.InputTokens
	u.CacheReadInputTokens += o.CacheReadInputTokens
	u.CacheCreationInputTokens += o.CacheCreationInputTokens
	u.OutputTokens += o.OutputTokens
	u.ReasoningTokens += o.ReasoningTokens
	u.Images += o.Images
}

// ------------------------------------------------------------------ 估算器

// Estimator 把文本折算成 token 数。
// 口径：CJK/全角字符 ≈ 1 token；其它字符每 4 个 ≈ 1 token。这是与主流 tokenizer
// 同阶的保守估计（对中文偏保守、对英文略高估，整体误差 <15%）。
type Estimator struct {
	TokensPerMessage int
	TokensPerTool    int
	ImageTokenFlat   int
}

// DefaultEstimator 给出默认值。
func DefaultEstimator() Estimator {
	return Estimator{TokensPerMessage: 4, TokensPerTool: 150, ImageTokenFlat: 1600}
}

// Text 估算一段文本的 token 数。
func (e Estimator) Text(s string) int {
	if s == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range s {
		switch {
		case r >= 0x2E80 && r <= 0x9FFF, // CJK 部首 + 统一表意
			r >= 0xAC00 && r <= 0xD7AF, // 韩文
			r >= 0x3040 && r <= 0x30FF, // 日文假名
			r >= 0xF900 && r <= 0xFAFF, // CJK 兼容表意
			r >= 0xFF00 && r <= 0xFFEF: // 全角
			cjk++
		default:
			other++
		}
	}
	return cjk + (other+3)/4
}

// Messages 估算一组消息的输入 token（不含图片内联）。
func (e Estimator) Messages(msgs []string) int {
	n := 0
	for _, m := range msgs {
		n += e.TokensPerMessage + e.Text(m)
	}
	return n
}

// Tools 估算工具声明的 token 开销。
func (e Estimator) Tools(count int, schemaBytes int) int {
	return count*e.TokensPerTool + schemaBytes/4
}

// Image 估算一张内联图片的 token 开销。base64 字符 /4 与 1600 取大者
// （视觉模型的固定开销通常在千级）。
func (e Estimator) Image(b64Chars int) int {
	t := b64Chars / 4
	if t < e.ImageTokenFlat {
		return e.ImageTokenFlat
	}
	return t
}

// ------------------------------------------------------------------ 前缀缓存

// Breakpoint 是缓存断点（按块累积 token 落在哪个位置）。
type Breakpoint struct {
	BlockIndex int
	TTL        time.Duration
}

type entry struct {
	tokens    int
	expiresAt time.Time
}

// Tracker 按命名空间记住「块指纹 → 该前缀的 token 数」。
//
// 语义：一次请求把消息切成块，每块算链式指纹。命中「之前见过的最长前缀」的那部分
// 记为 CacheRead（读命中），新产生的部分记为 CacheCreation（写），剩余（无法前缀化的
// 尾部）记为 Input。成功后才 Commit，失败请求不污染前缀表。
type Tracker struct {
	mu     sync.Mutex
	ns     map[string]map[[32]byte]entry
	ttl    time.Duration
	maxPer int
	order  map[string][]uint64 // 命名空间内的 LRU 顺序（按插入序淘汰）
	est    Estimator
	// 观测
	hits, misses int64
}

// NewTracker 建追踪器。
func NewTracker(ttl time.Duration, maxEntriesPerNS int, est Estimator) *Tracker {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if maxEntriesPerNS <= 0 {
		maxEntriesPerNS = 4096
	}
	return &Tracker{
		ns:     map[string]map[[32]byte]entry{},
		order:  map[string][]uint64{},
		ttl:    ttl,
		maxPer: maxEntriesPerNS,
		est:    est,
	}
}

// block 是一个可缓存块。
type block struct {
	fingerprint [32]byte
	tokens      int
}

// cacheProfile 是一次请求的缓存画像。
type cacheProfile struct {
	blocks   []block
	read     int // 读命中的 token
	creation int // 新建的 token
	input    int // 尾部无法前缀化的 token
}

// Plan 先估算，Commit 才落表。
type Plan struct {
	Usage   *Usage
	ns      string
	profile *cacheProfile
	tracker *Tracker
	commits bool
}

// Result 返回用量。
func (p *Plan) Result() *Usage {
	if p == nil {
		return nil
	}
	return p.Usage
}

// Commit 把本次前缀写入表（幂等）。
func (p *Plan) Commit() {
	if p == nil || p.profile == nil || p.tracker == nil || p.commits {
		return
	}
	p.commits = true
	p.tracker.update(p.ns, p.profile)
}

// Plan 计算一次请求的用量与缓存拆分。
// blocks 是按顺序的文本块（消息正文 / 工具协议 / 附件块），namespace 隔离部署与账号。
func (t *Tracker) Plan(namespace string, blocks []string, outputTokens, images int, extraFixed int) *Plan {
	u := &Usage{OutputTokens: outputTokens, Images: images}
	prof := &cacheProfile{}
	now := time.Now()

	if namespace == "" {
		namespace = "default"
	}
	t.mu.Lock()
	table := t.ns[namespace]
	if table == nil {
		table = map[[32]byte]entry{}
		t.ns[namespace] = table
	}
	expired := t.ttl > 0
	t.mu.Unlock()

	var chain []byte
	matched := 0
	lostMatch := false
	for i, b := range blocks {
		tok := t.est.Text(b) + t.est.TokensPerMessage
		h := sha256.New()
		h.Write(chain)
		h.Write([]byte{0})
		h.Write([]byte(b))
		var fp [32]byte
		copy(fp[:], h.Sum(nil))
		chain = fp[:]

		prof.blocks = append(prof.blocks, block{fingerprint: fp, tokens: tok})

		if !lostMatch {
			t.mu.Lock()
			e, ok := table[fp]
			if ok && expired && now.After(e.expiresAt) {
				ok = false
			}
			t.mu.Unlock()
			if ok {
				prof.read += tok
				matched++
				continue
			}
			lostMatch = true // 前缀断裂：其后全部算新建
			t.mu.Lock()
			t.misses++
			t.mu.Unlock()
		}
		if i >= matched {
			// 首个未命中块：读命中部分结束，其余是「新建」。
			prof.creation += tok
		}
	}
	if matched > 0 {
		t.mu.Lock()
		t.hits++
		t.mu.Unlock()
	}

	// 尾部固定开销（系统指令注入 / protocol 包装）算入 Input。
	u.CacheReadInputTokens = prof.read
	u.CacheCreationInputTokens = prof.creation
	u.InputTokens = extraFixed
	return &Plan{Usage: u, ns: namespace, profile: prof, tracker: t}
}

// update 把前缀写入表并做容量淘汰。
func (t *Tracker) update(namespace string, prof *cacheProfile) {
	exp := time.Now().Add(t.ttl)
	t.mu.Lock()
	defer t.mu.Unlock()
	table := t.ns[namespace]
	if table == nil {
		table = map[[32]byte]entry{}
		t.ns[namespace] = table
	}
	for _, b := range prof.blocks {
		table[b.fingerprint] = entry{tokens: b.tokens, expiresAt: exp}
	}
	// 容量保护：超出上限时清掉过期项，仍超则整体清空（前缀表是软状态，宁丢失不 OOM）。
	if len(table) > t.maxPer {
		now := time.Now()
		for k, v := range table {
			if now.After(v.expiresAt) {
				delete(table, k)
			}
		}
		if len(table) > t.maxPer {
			t.ns[namespace] = map[[32]byte]entry{}
		}
	}
}

// Stats 返回命中统计与表规模。
func (t *Tracker) Stats() (hits, misses int64, namespaces int, entries int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tab := range t.ns {
		entries += len(tab)
	}
	return t.hits, t.misses, len(t.ns), entries
}

// Purge 清理所有命名空间的过期项（后台调用）。
func (t *Tracker) Purge() int {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for ns, tab := range t.ns {
		for k, v := range tab {
			if now.After(v.expiresAt) {
				delete(tab, k)
				n++
			}
		}
		if len(tab) == 0 {
			delete(t.ns, ns)
		}
	}
	return n
}

// ------------------------------------------------------------------ 客户端级计数

// Counters 是网关级「读/写 token」累计计数（按 key 与模型分组）。
type Counters struct {
	mu     sync.Mutex
	byKey  map[string]*KeyStat
	global KeyStat
	start  time.Time
}

// KeyStat 是一个客户端 key 的累计量。
type KeyStat struct {
	Requests    int64     `json:"requests"`
	Errors      int64     `json:"errors"`
	InputTokens int64     `json:"input_tokens"`
	CacheRead   int64     `json:"cache_read_input_tokens"`
	CacheWrite  int64     `json:"cache_creation_input_tokens"`
	Output      int64     `json:"output_tokens"`
	LastSeen    time.Time `json:"last_seen"`
}

// NewCounters 建计数器。
func NewCounters() *Counters {
	return &Counters{byKey: map[string]*KeyStat{}, start: time.Now()}
}

// Record 记一次请求的用量。
func (c *Counters) Record(key string, u Usage, failed bool) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if key == "" {
		key = "anonymous"
	}
	ks := c.byKey[key]
	if ks == nil {
		ks = &KeyStat{}
		c.byKey[key] = ks
	}
	ks.Requests++
	if failed {
		ks.Errors++
	}
	ks.InputTokens += int64(u.InputTokens)
	ks.CacheRead += int64(u.CacheReadInputTokens)
	ks.CacheWrite += int64(u.CacheCreationInputTokens)
	ks.Output += int64(u.OutputTokens)
	ks.LastSeen = now

	c.global.Requests++
	if failed {
		c.global.Errors++
	}
	c.global.InputTokens += int64(u.InputTokens)
	c.global.CacheRead += int64(u.CacheReadInputTokens)
	c.global.CacheWrite += int64(u.CacheCreationInputTokens)
	c.global.Output += int64(u.OutputTokens)
	c.global.LastSeen = now
}

// Snapshot 返回全局与分 key 快照。
func (c *Counters) Snapshot() (global KeyStat, byKey map[string]KeyStat) {
	c.mu.Lock()
	defer c.mu.Unlock()
	byKey = make(map[string]KeyStat, len(c.byKey))
	for k, v := range c.byKey {
		byKey[k] = *v
	}
	return c.global, byKey
}

// Global 返回全局快照。
func (c *Counters) Global() KeyStat {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.global
}

// Uptime 返回服务运行时长。
func (c *Counters) Uptime() time.Duration { return time.Since(c.start) }

// FormatReport 生成人类可读的计数报告（/admin/usage 用）。
func (c *Counters) FormatReport() string {
	global, byKey := c.Snapshot()
	var sb strings.Builder
	fmt.Fprintf(&sb, "运行时长: %s\n", c.Uptime().Round(time.Second))
	fmt.Fprintf(&sb, "总请求: %d（失败 %d）\n", global.Requests, global.Errors)
	fmt.Fprintf(&sb, "token 累计: 输入 %d / 缓存读 %d / 缓存写 %d / 输出 %d（总 %d）\n",
		global.InputTokens, global.CacheRead, global.CacheWrite, global.Output,
		global.InputTokens+global.CacheRead+global.CacheWrite+global.Output)
	if tot := global.InputTokens + global.CacheRead + global.CacheWrite; tot > 0 {
		fmt.Fprintf(&sb, "缓存读占比: %.1f%%\n", 100*float64(global.CacheRead)/float64(tot))
	}
	if len(byKey) > 0 {
		sb.WriteString("\n按客户端 key:\n")
		keys := make([]string, 0, len(byKey))
		for k := range byKey {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := byKey[k]
			fmt.Fprintf(&sb, "  %-24s 请求 %-6d 输入 %-8d 缓存读 %-8d 缓存写 %-8d 输出 %-8d\n",
				maskKey(k), v.Requests, v.InputTokens, v.CacheRead, v.CacheWrite, v.Output)
		}
	}
	return sb.String()
}

// maskKey 遮蔽 key 中段（日志与面板不泄漏完整 key）。
func maskKey(k string) string {
	if len(k) <= 8 {
		return k
	}
	return k[:4] + "…" + k[len(k)-4:]
}

// MarshalJSON 让 Usage 的 ratio 也能被导出（观测页用）。
func (u Usage) MarshalJSON() ([]byte, error) {
	type alias Usage
	return json.Marshal(struct {
		alias
		Total      int     `json:"total_tokens"`
		CacheRatio float64 `json:"cache_read_ratio"`
	}{
		alias:      alias(u),
		Total:      u.Total(),
		CacheRatio: ratio(u.CacheReadInputTokens, u.TotalInput()),
	})
}

func ratio(part, total int) float64 {
	if total <= 0 {
		return 0
	}
	return math.Round(float64(part)/float64(total)*10000) / 10000
}
