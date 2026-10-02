package stream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ============================================================================
// SSE 合成器。
//
// 上游事实：prism 是 start + status 轮询，**不是 token 流**——正文在 completed 时
// 一次性到达（有实现直接在 README 里承认 "not token-streaming"）。
//
// 本网关的做法（低延迟优先）：
//  1. 握手即发一个 SSE 注释，让客户端立刻拿到「已连接」信号（避免连接超时重试）。
//  2. 上游 pending 期间周期性发注释心跳，保活代理/客户端（默认 15s）。
//  3. 思考摘要（reasoning）先于正文下发——用户第一时间看到「在想什么」，
//     体感延迟显著下降（这一步是「伪造思考流」的正解：内容是真的，只是提前）。
//  4. 正文按 rune 切片、小间隔推送，形成平滑逐字感（默认 24 rune / 12ms）。
//  5. 上游沙箱执行过的工具调用按协议即时流出。
//  6. 客户端断连 → 立刻取消上游轮次（省钱）。
// ============================================================================

// Writer 是一个 SSE 写入器（并发安全，自带心跳）。
type Writer struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	mu        sync.Mutex
	wroteHead bool
	closed    bool
	broken    bool
	eventID   int
	// 观测
	bytesOut int
	events   int
}

// NewWriter 建 SSE 写入器。
func NewWriter(w http.ResponseWriter) (*Writer, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("stream: ResponseWriter 不支持 Flush，无法做 SSE")
	}
	s := &Writer{w: w, flusher: f}
	s.head()
	return s, nil
}

// head 写响应头（一次）。关键：关缓冲、禁缓存、给代理留长连接。
func (s *Writer) head() {
	if s.wroteHead {
		return
	}
	s.wroteHead = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // nginx 等反代不缓冲
	s.w.WriteHeader(http.StatusOK)
	s.flusher.Flush()
}

// Comment 写一条 SSE 注释（心跳/握手信号，客户端忽略内容）。
func (s *Writer) Comment(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.broken {
		return
	}
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		s.broken = true
		return
	}
	s.bytesOut += len(text) + 4
	s.flusher.Flush()
}

// Event 写一条命名事件。
func (s *Writer) Event(name string, data any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.broken {
		return fmt.Errorf("stream: 已关闭")
	}
	payload, err := encodeData(data)
	if err != nil {
		return err
	}
	s.eventID++
	var sb strings.Builder
	if name != "" {
		sb.WriteString("event: ")
		sb.WriteString(name)
		sb.WriteByte('\n')
	}
	fmt.Fprintf(&sb, "id: %d\n", s.eventID)
	// 多行 data 需要每行加 "data: "。
	for _, line := range strings.Split(payload, "\n") {
		sb.WriteString("data: ")
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	sb.WriteByte('\n')
	n, err := s.w.Write([]byte(sb.String()))
	if err != nil {
		s.broken = true
		return err
	}
	s.bytesOut += n
	s.events++
	s.flusher.Flush()
	return nil
}

// Data 写一条纯 data 事件（OpenAI 风格，无 event 名）。
func (s *Writer) Data(data any) error { return s.Event("", data) }

// RawData 写原始字符串 data（用于 [DONE] 之类）。
func (s *Writer) RawData(raw string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.broken {
		return fmt.Errorf("stream: 已关闭")
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", raw); err != nil {
		s.broken = true
		return err
	}
	s.flusher.Flush()
	return nil
}

// Broken 报告客户端是否已断连（写失败）。
func (s *Writer) Broken() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.broken
}

// Close 结束流（幂等）。
func (s *Writer) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// Stats 返回写出统计（观测）。
func (s *Writer) Stats() (events, bytes int, broken bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.events, s.bytesOut, s.broken
}

// KeepAlive 起一个后台心跳，返回停止函数。
// pending 期间上游可能几十秒不出结果，没有心跳会被中间代理掐断。
func (s *Writer) KeepAlive(every time.Duration) func() {
	if every <= 0 {
		every = 15 * time.Second
	}
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if s.Broken() {
					return
				}
				s.Comment("keep-alive")
			}
		}
	}()
	return func() { once.Do(func() { close(stop) }) }
}

// ------------------------------------------------------------------ 切片合成

// ChunkOptions 控制合成流的平滑度。
type ChunkOptions struct {
	Size     int           // 每次推送的 rune 数
	Interval time.Duration // 推送间隔
	// Signal 在每次推送前被调用，返回 false 表示客户端断连、应停止。
	Signal func() bool
}

// DefaultChunkOptions 给出低延迟默认（24 rune / 12ms ≈ 2000 字符/秒）。
func DefaultChunkOptions() ChunkOptions {
	return ChunkOptions{Size: 24, Interval: 12 * time.Millisecond}
}

// Chunks 把文本切成推送批次（按 rune，不切断多字节字符）。
func Chunks(text string, size int) []string {
	if size < 1 {
		size = 24
	}
	if text == "" {
		return nil
	}
	out := make([]string, 0, utf8.RuneCountInString(text)/size+1)
	var sb strings.Builder
	n := 0
	for _, r := range text {
		sb.WriteRune(r)
		n++
		if n >= size {
			out = append(out, sb.String())
			sb.Reset()
			n = 0
		}
	}
	if sb.Len() > 0 {
		out = append(out, sb.String())
	}
	return out
}

// EmitChunks 逐块推送文本，每块交给 emit 处理。
// 返回 false 表示中途被中断（客户端断连）。
func EmitChunks(text string, opt ChunkOptions, emit func(chunk string) error) bool {
	if opt.Size <= 0 {
		opt.Size = 24
	}
	if opt.Interval <= 0 {
		opt.Interval = 12 * time.Millisecond
	}
	parts := Chunks(text, opt.Size)
	if len(parts) == 0 {
		return true
	}
	// 单块直接发，不做无谓等待（低延迟）。
	if len(parts) == 1 {
		return emit(parts[0]) == nil
	}
	for _, p := range parts {
		if opt.Signal != nil && !opt.Signal() {
			return false
		}
		if err := emit(p); err != nil {
			return false
		}
		time.Sleep(opt.Interval)
	}
	return true
}

func encodeData(data any) (string, error) {
	switch v := data.(type) {
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	default:
		b, err := json.Marshal(data)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}
