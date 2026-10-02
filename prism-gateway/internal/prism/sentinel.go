package prism

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// sentinel token 产线（对话面风控）。
//
// 实测（2026-09-19 上线日）：上游 start 开始强制校验 openai-sentinel-token header，
// 缺失或复用一律应用层 403（"Error while processing conversation (403 Forbidden)"），
// 与 IP/账号/请求形状无关（服务器直连、住宅代理、重放浏览器 header 全 403，
// 唯有真浏览器成功）。token 由登录侧车（真浏览器）本地铸造，具备两个硬性质：
//   - 严格一次性：第二次使用即 403；
//   - 短时效：池内放置分钟级的 token 发 start 一律 403，必须新鲜。
//
// 因此这里做的是一个「浅池削峰」而不是「囤货」：铸一个约 2~4s，池深默认 8，
// 新鲜窗 45s，过期即弃；侧车故障时进入负缓存窗口，避免重试链把侧车打爆。
// ============================================================================

// Sentinel 是 sentinel token 池。
type Sentinel struct {
	mu        sync.Mutex
	pool      chan sentinelTok
	client    *http.Client
	sidecars  []string // 侧车地址池（多容器产能翻倍）
	rr        uint64
	deviceID  string
	ua        string
	enabled   bool
	poolSize  int
	freshTTL  time.Duration
	timeout   time.Duration
	failUntil time.Time
	backoff   time.Duration
	logf      func(format string, args ...any)
	starts    int64
	minted    int64
	failed    int64
	fromPool  int64
}

type sentinelTok struct {
	val string
	at  time.Time
}

// SentinelOptions 配置。
type SentinelOptions struct {
	Sidecars  []string // 如 ["http://127.0.0.1:8898"]，支持多容器
	UserAgent string
	PoolSize  int
	FreshTTL  time.Duration
	Timeout   time.Duration
	Logf      func(format string, args ...any)
}

// sentinel 是包级单例（turn.go 的 sentinelHeader 使用）。
var sentinel *Sentinel

// SetSentinel 安装全局 sentinel 池（main 启动时调用；nil = 未启用）。
func SetSentinel(s *Sentinel) { sentinel = s }

// GetSentinel 返回当前全局池（观测用）。
func GetSentinel() *Sentinel { return sentinel }

// NewSentinel 建池。Sidecars 为空或 PG_SENTINEL=off 时返回 nil（未启用）。
func NewSentinel(opt SentinelOptions) *Sentinel {
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("PG_SENTINEL"))); v == "0" || v == "false" || v == "off" {
		return nil
	}
	if len(opt.Sidecars) == 0 {
		if v := strings.TrimSpace(os.Getenv("PG_SENTINEL_SIDECARS")); v != "" {
			opt.Sidecars = splitAny(v)
		}
	}
	if len(opt.Sidecars) == 0 {
		return nil
	}
	if opt.UserAgent == "" {
		opt.UserAgent = defaultUserAgent
	}
	if opt.PoolSize <= 0 {
		opt.PoolSize = 8
	}
	if opt.PoolSize > 32 {
		opt.PoolSize = 32
	}
	if opt.FreshTTL <= 0 {
		opt.FreshTTL = 45 * time.Second
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 60 * time.Second
	}
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	s := &Sentinel{
		pool:     make(chan sentinelTok, opt.PoolSize),
		client:   mustSentinelClient(),
		sidecars: opt.Sidecars,
		deviceID: uuid4(), // 裸 uuid：真机 header 的 id 字段即此形态（cdx1_ 前缀会被 400）
		ua:       opt.UserAgent,
		enabled:  true,
		poolSize: opt.PoolSize,
		freshTTL: opt.FreshTTL,
		timeout:  opt.Timeout,
		logf:     opt.Logf,
	}
	go s.refillLoop()
	return s
}

// Enabled 报告池是否启用。
func (s *Sentinel) Enabled() bool { return s != nil && s.enabled }

// Stats 返回观测计数。
func (s *Sentinel) Stats() (starts, minted, failed, fromPool, depth int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.starts, s.minted, s.failed, s.fromPool, int64(len(s.pool))
}

// Take 取一个新鲜 token：优先池内新鲜票，否则限时等补货，最后自己铸。
func (s *Sentinel) Take() (string, error) {
	if !s.Enabled() {
		return "", fmt.Errorf("sentinel 未启用")
	}
	s.mu.Lock()
	s.starts++
	backoff := time.Now().Before(s.failUntil)
	s.mu.Unlock()
	if backoff {
		return "", fmt.Errorf("sentinel 处于铸币退避窗口")
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	// 第一圈：池内新鲜票。
	for {
		select {
		case tok := <-s.pool:
			if time.Since(tok.at) <= s.freshTTL {
				s.mu.Lock()
				s.fromPool++
				s.mu.Unlock()
				return tok.val, nil
			}
			continue // 过期票丢弃
		default:
		}
		break
	}
	// 池空（或全是过期票）：限时等一次补货。
	waitCtx, cancel2 := context.WithTimeout(ctx, 3*time.Second)
	select {
	case tok := <-s.pool:
		cancel2()
		if time.Since(tok.at) <= s.freshTTL {
			return tok.val, nil
		}
	case <-waitCtx.Done():
		cancel2()
	}
	// 兜底：现铸一个。
	tok, err := s.mint(ctx)
	if err != nil {
		s.mu.Lock()
		s.failed++
		s.failUntil = time.Now().Add(s.negativeBackoff())
		s.mu.Unlock()
		return "", err
	}
	return tok, nil
}

// negativeBackoff 返回铸币失败后的静默窗口（30s 起，上限 5min）。
func (s *Sentinel) negativeBackoff() time.Duration {
	if s.backoff < 30*time.Second {
		s.backoff = 30 * time.Second
	} else if s.backoff < 5*time.Minute {
		s.backoff *= 2
	}
	return s.backoff
}

// refillLoop 后台补池：维持浅池流动新鲜（铸一个歇 500ms，不积压）。
func (s *Sentinel) refillLoop() {
	for {
		if len(s.pool) >= cap(s.pool) {
			time.Sleep(10 * time.Second)
			continue
		}
		s.mu.Lock()
		cooling := time.Now().Before(s.failUntil)
		s.mu.Unlock()
		if cooling {
			time.Sleep(5 * time.Second)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		tok, err := s.mint(ctx)
		cancel()
		if err != nil {
			s.mu.Lock()
			s.failed++
			s.failUntil = time.Now().Add(s.negativeBackoff())
			s.mu.Unlock()
			s.logf("sentinel 补池失败（退避 %s）: %v", s.backoff, err)
			time.Sleep(s.backoff)
			continue
		}
		s.mu.Lock()
		s.backoff = 0
		s.mu.Unlock()
		select {
		case s.pool <- sentinelTok{val: tok, at: time.Now()}:
		default: // 并发下被补满：丢弃
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// mint 调侧车的 sentinel_token action，从 NDJSON 事件流里取 done.token。
func (s *Sentinel) mint(ctx context.Context) (string, error) {
	base := s.pickSidecar()
	body, _ := json.Marshal(map[string]any{
		"action":     "sentinel_token",
		"flow":       "prism_inference",
		"page_url":   "https://prism.openai.com/",
		"device_id":  s.deviceID,
		"user_agent": s.ua,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sidecar %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return "", fmt.Errorf("sidecar 状态 %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	token, errMsg, err := parseNDJSON(resp.Body)
	if err != nil {
		return "", err
	}
	if token != "" {
		s.mu.Lock()
		s.minted++
		s.mu.Unlock()
		return token, nil
	}
	if errMsg != "" {
		return "", fmt.Errorf("sidecar 错误: %s", errMsg)
	}
	return "", fmt.Errorf("sidecar 未产出 token")
}

// parseNDJSON 解析侧车的 NDJSON 事件流（state/done/error）。
func parseNDJSON(r io.Reader) (token, errMsg string, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Event   string `json:"event"`
			Token   string `json:"token"`
			Message string `json:"message"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Event {
		case "done":
			token = ev.Token
		case "error":
			errMsg = ev.Message
		}
	}
	return token, errMsg, sc.Err()
}

func (s *Sentinel) pickSidecar() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sidecars) == 0 {
		return ""
	}
	i := s.rr
	s.rr++
	return strings.TrimRight(s.sidecars[i%uint64(len(s.sidecars))], "/")
}

// splitAny 按逗号/空白/换行切分。
func splitAny(v string) []string {
	fields := strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if s := strings.TrimSpace(f); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// SentinelPoolSizeFromEnv 读池深环境变量（PG_SENTINEL_POOL）。
func SentinelPoolSizeFromEnv() int {
	if v := strings.TrimSpace(os.Getenv("PG_SENTINEL_POOL")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 32 {
			return n
		}
	}
	return 8
}

// mustSentinelClient 造 sentinel 铸造用的出站客户端。
//
// sentinel 是独立链路（不携带材料里的 cf_clearance），保持 Go 原生指纹即可；
// 若后续发现它也需要浏览器指纹，改这里的 Profile 并注入 FingerprintInjector。
func mustSentinelClient() *http.Client {
	hc, err := NewHTTPClient(TransportConfig{
		Profile: ProfileGo, ForceHTTP1: true, MaxIdleConns: 16,
		DialTimeout: 5 * time.Second, TLSHandshake: 8 * time.Second,
	}, "", nil)
	if err != nil {
		panic(fmt.Sprintf("prism: 构造 sentinel 客户端失败: %v", err))
	}
	return hc
}
