package prism

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 失败分类三态 + 上游劣化熔断。
//
// 为什么需要（来自 43K 行那份实现的核心洞察）：
// 上游会「整段抽风」（/api/backend/1/new 从 1.4s 变成 24~210s、start 全超时），
// 此时如果把错误当成「账号坏了」去换号 + 冷却，等于给过载的上游补刀：
//   - 换号零收益（新号同样付冷链，还放大全池预热风暴）
//   - 冷却好账号纯属误伤
// 所以必须把失败分成三类，并只对「账号相关」的失败做冷却/换号：
//
//	KindAuth    账号凭据问题        → Cool ✓  Switch ✓
//	KindQuota   账号限额/熔断        → Cool ✓  Switch ✓
//	KindRequest 请求过错（模型名/上下文）→ Cool ✗  Switch ✗ （换号无意义，快速失败）
//	KindBroken  上游整体劣化/沙箱劣化 → Cool ✗  Switch ✗ （熔断，退避重试）
//	KindCancel  客户端取消           → Cool ✗  Switch ✗
//	KindTransport 传输层抖动          → Cool ✗  Switch ✓ （原地/换号重试一次）
// ============================================================================

// FailKind 是失败类别。
type FailKind int

const (
	KindUnknown FailKind = iota
	KindAuth
	KindQuota
	KindRequest
	KindBroken
	KindCancel
	KindTransport
)

func (k FailKind) String() string {
	switch k {
	case KindAuth:
		return "auth"
	case KindQuota:
		return "quota"
	case KindRequest:
		return "request"
	case KindBroken:
		return "upstream_degraded"
	case KindCancel:
		return "client_cancel"
	case KindTransport:
		return "transport"
	default:
		return "unknown"
	}
}

// Classification 是一次失败的处置建议。
type Classification struct {
	Kind FailKind
	Cool bool // 是否冷却账号
	Swit bool // 是否值得换号重试
	Msg  string
}

// Classify 判定失败类别与处置。
func Classify(err error) Classification {
	if err == nil {
		return Classification{Kind: KindUnknown}
	}
	if errors.Is(err, errClientCancelled) {
		return Classification{Kind: KindCancel, Msg: "客户端取消"}
	}
	msg := strings.ToLower(err.Error())

	// 显式分类（上游文案优先）。
	switch {
	// ⚠️ 该分支必须排在最前。
	//
	// 上游把 Prism 会话级 403 藏在 HTTP 200 的内层 payload 里：
	//   response.status=error + payload.httpStatus=403
	//   message="Error while processing conversation (403 Forbidden). Please submit prompt again."
	// 语义是**账号级限流（带冷却期）**，而不是凭据失效，也不是单纯的上游劣化：
	//   换号/换槽**零收益**（新身份同样受限），而每次「换号重试」都会再打一次上游 ——
	//   把 N 路请求放大成 N×attempts 次调用，等于自己烧配额、把账号推入更深冷却。
	// 正确处置：冷却退避（Cool ✓）+ **不换号**（Swit ✗）+ 熔断器不计数（账号型限流
	//   不代表上游整体健康度，否则会因账号被限而误跳上游熔断）。
	//
	// 注意：原 KindBroken 分支里的 "please submit prompt again" 会让本类失败落到
	//   上游劣化上，故本分支必须前置。
	case strings.Contains(msg, "processing conversation (403"),
		strings.Contains(msg, "(403 forbidden)"):
		return Classification{Kind: KindQuota, Cool: true, Swit: false,
			Msg: "账号级限流（冷却退避，换号无收益）"}

	case strings.Contains(msg, "unsupported assistant model"),
		strings.Contains(msg, "model not found"),
		strings.Contains(msg, "context length"),
		strings.Contains(msg, "context window"),
		strings.Contains(msg, "too long"),
		strings.Contains(msg, "invalid_request"),
		strings.Contains(msg, "400 bad request"):
		return Classification{Kind: KindRequest, Msg: "请求过错（换号无收益）"}

	case strings.Contains(msg, "429"),
		strings.Contains(msg, "rate limit"),
		strings.Contains(msg, "too many requests"),
		strings.Contains(msg, "quota"):
		return Classification{Kind: KindQuota, Cool: true, Swit: true, Msg: "账号限额"}

	case strings.Contains(msg, "sandbox degraded"),
		strings.Contains(msg, "upstream degraded"),
		strings.Contains(msg, "gateway timeout"),
		strings.Contains(msg, "504"),
		strings.Contains(msg, "503"),
		strings.Contains(msg, "please submit prompt again"):
		return Classification{Kind: KindBroken, Msg: "上游劣化（熔断退避）"}

	}

	// 按结构化的 UpstreamError 判定。
	var ue *UpstreamError
	if errors.As(err, &ue) {
		switch {
		case ue.Status == http.StatusUnauthorized || ue.Status == http.StatusForbidden:
			return Classification{Kind: KindAuth, Cool: true, Swit: true, Msg: "凭据失效"}
		case ue.Status == http.StatusTooManyRequests:
			return Classification{Kind: KindQuota, Cool: true, Swit: true, Msg: "账号限额"}
		case ue.Status >= 500:
			return Classification{Kind: KindBroken, Msg: "上游 5xx"}
		case ue.Status >= 400:
			return Classification{Kind: KindRequest, Msg: "请求过错"}
		}
	}

	// 传输层抖动：原地或换号重试一次即可，不该冷却账号。
	for _, k := range []string{"timeout", "timed out", "connection reset", "broken pipe",
		"eof", "protocol_error", "no such host", "tls handshake", "connection refused"} {
		if strings.Contains(msg, k) {
			return Classification{Kind: KindTransport, Swit: true, Msg: "传输层抖动"}
		}
	}
	return Classification{Kind: KindUnknown, Swit: true}
}

// errClientCancelled 标记客户端主动取消（网关内部使用）。
var errClientCancelled = errors.New("客户端取消")

// MarkClientCancelled 包装客户端取消错误。
func MarkClientCancelled(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %v", errClientCancelled, err)
}

// ------------------------------------------------------------------ 熔断器

// BreakerStats 是熔断器状态快照。
type BreakerStats struct {
	Open         bool
	FailRate     float64
	Samples      int
	OpenedAt     time.Time
	ProbeAt      time.Time
	TotalTrips   int64
	TotalSamples int64
}

// Breaker 是「上游劣化」熔断器。
//
// 语义（来自实测参数）：
//   - 60s 滑动窗内失败率 ≥ 0.7 且样本数 ≥ 12 → 打开（开闸）
//   - 打开后每 15s 放一个探测请求，成功即闭合（自愈）
//   - 打开期间入口直接快速失败（映射成 503 + Retry-After: 15），不打上游
type Breaker struct {
	mu       sync.Mutex
	window   time.Duration
	minRate  float64
	minSamp  int
	probeGap time.Duration

	events    []breakerEvent
	open      bool
	openedAt  time.Time
	probeAt   time.Time
	trips     int64
	totalSamp int64
	now       func() time.Time
}

type breakerEvent struct {
	at   time.Time
	fail bool
}

// NewBreaker 建熔断器（零值参数使用实测默认）。
func NewBreaker(window time.Duration, minRate float64, minSamples int, probeGap time.Duration) *Breaker {
	if window <= 0 {
		window = 60 * time.Second
	}
	if minRate <= 0 {
		minRate = 0.7
	}
	if minSamples <= 0 {
		minSamples = 12
	}
	if probeGap <= 0 {
		probeGap = 15 * time.Second
	}
	return &Breaker{window: window, minRate: minRate, minSamp: minSamples, probeGap: probeGap, now: time.Now}
}

// Allow 报告是否允许放行请求。返回 false 时应快速失败。
// 打开期间每 probeGap 放行一个探测请求。
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return true
	}
	now := b.now()
	if now.Sub(b.probeAt) >= b.probeGap {
		b.probeAt = now
		return true // 探测请求
	}
	return false
}

// Record 记录一次结果（只统计「上游相关」的失败，请求过错不计入）。
//
// 注意：成功事件也要参与评估——失败率是窗口统计量，
// 若只在失败时评估，「10 失败 + 2 成功」（83%）这种窗口永远撞不到阈值。
func (b *Breaker) Record(fail bool, kind FailKind) {
	if fail {
		// 请求过错与客户端取消不代表上游健康，不计入熔断统计。
		switch kind {
		case KindBroken, KindTransport, KindUnknown:
		default:
			return
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !fail && b.open {
		// 探测成功 → 自愈：闭合并**清空窗口**。
		// 必须清空，否则残留的失败样本会在紧接着的 evaluate 里
		// 立刻二次跳闸（探测成功却关不掉断路器）。
		b.events = b.events[:0]
		b.open = false
		b.openedAt = time.Time{}
	}
	b.push(breakerEvent{at: b.now(), fail: fail})
	b.evaluate()
}

// push 追加事件并裁掉窗口外的（须持锁）。
func (b *Breaker) push(e breakerEvent) {
	b.events = append(b.events, e)
	b.totalSamp++
	cut := e.at.Add(-b.window)
	i := 0
	for i < len(b.events) && b.events[i].at.Before(cut) {
		i++
	}
	if i > 0 {
		b.events = append(b.events[:0], b.events[i:]...)
	}
}

// evaluate 评估是否开闸（须持锁）。
func (b *Breaker) evaluate() {
	if b.open {
		return
	}
	n := len(b.events)
	if n < b.minSamp {
		return
	}
	fails := 0
	for _, e := range b.events {
		if e.fail {
			fails++
		}
	}
	if float64(fails)/float64(n) >= b.minRate {
		b.open = true
		b.openedAt = b.now()
		b.probeAt = b.openedAt
		b.trips++
	}
}

// Stats 返回状态快照。
func (b *Breaker) Stats() BreakerStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.events)
	fails := 0
	for _, e := range b.events {
		if e.fail {
			fails++
		}
	}
	rate := 0.0
	if n > 0 {
		rate = float64(fails) / float64(n)
	}
	return BreakerStats{
		Open: b.open, FailRate: rate, Samples: n, OpenedAt: b.openedAt,
		ProbeAt: b.probeAt, TotalTrips: b.trips, TotalSamples: b.totalSamp,
	}
}

// DegradedError 表示上游整体劣化（映射成 503 + Retry-After）。
type DegradedError struct {
	RetryAfter time.Duration
	Stats      BreakerStats
}

func (e *DegradedError) Error() string {
	return fmt.Sprintf("上游整体劣化（60s 窗失败率 %.0f%%，样本 %d），已熔断，%s 后自动探测自愈",
		e.Stats.FailRate*100, e.Stats.Samples, e.RetryAfter)
}

// HttpStatus 给出建议的 HTTP 状态码（503）。
func (e *DegradedError) HttpStatus() int { return http.StatusServiceUnavailable }

// RetryAfterSeconds 给出 Retry-After 秒数。
func (e *DegradedError) RetryAfterSeconds() int {
	s := int(e.RetryAfter / time.Second)
	if s < 1 {
		s = 1
	}
	return s
}
