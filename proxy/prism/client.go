// Package prism 实现 codex2api 的 Prism 通道。
//
// Prism（prism.openai.com）是 OpenAI 的 LaTeX 编辑器站点，其内置对话走后端私协议
// （start/poll），与 Codex 的 chatgpt.com/backend-api/codex/responses 完全不兼容。
// 本包把这个私协议适配成 codex2api 内部统一的 Responses 形态，从而：
//
//	下游（/v1/responses、/v1/chat/completions、/v1/messages）零改动
//
// 私协议链（每条都经真机实测钉死，见 closed-loop/FINDINGS.md）：
//
//	① Cookie: prism_oai_access_token + oai-sc（同一枚 access_token）
//	   GET/POST /auth/session → 200 并自举 prism_session_token（无需手搓）
//	② header: openai-sentinel-token —— 严格一次性；由 node 跑 sentinel SDK
//	   就地算 proof 铸造，不需要浏览器（sentinel.go）
//	③ POST /api/backend/1/new → {url, token}（沙箱）
//	④ POST /api/llm/response_with_tools_start   body{input,metadata,conversationId}
//	⑤ POST /api/llm/response_with_tools_status  body{request_id,turn_state}
//	   turn_state 必须逐轮原样回传，否则 400 "turn_state is required"
//
// 三道门禁（任一不满足即失败）：
//
//	sentinel        每次现铸（复用 → 403 Request verification failed）
//	conversationId  每次全新（复用材料里的 conv → 403 processing conversation）
//	metadata.sandbox_* 必须与材料同源（自铸新沙箱 → 400 Please submit prompt again）
package prism

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Cookie 是一份 Prism 凭据：access token 同时用作两个 cookie。
type Cookie struct {
	// AccessToken 来自 OpenAI OAuth（即 Codex 账号的 access_token）。
	AccessToken string
	// UserAgent 必须与铸造 sentinel / 复用材料时一致（上游按浏览器身份限流）。
	UserAgent string
	// SessionToken 由 /auth/session 自举，无需人工提供。
	SessionToken string
	// RawCookie 可选：直接给出完整 Cookie 头（调试/兼容旧凭据用）。
	RawCookie string
}

// cookieHeader 拼出请求用 Cookie 头。
func (c *Cookie) cookieHeader() string {
	if strings.TrimSpace(c.RawCookie) != "" {
		return c.RawCookie
	}
	parts := []string{}
	if c.AccessToken != "" {
		parts = append(parts,
			"prism_oai_access_token="+c.AccessToken,
			"oai-sc="+c.AccessToken)
	}
	if c.SessionToken != "" {
		parts = append(parts, "prism_session_token="+c.SessionToken)
	}
	return strings.Join(parts, "; ")
}

// Config 是通道配置。
type Config struct {
	Base            string // 默认 https://prism.openai.com
	Model           string // 默认 gpt-6-astra
	ReasoningEffort string // low/medium/high，默认 medium
	MintCommand     string // sentinel 铸造命令（默认走内置 node runner）
	NodeBin         string // node 可执行文件路径
	SentinelRunner  string // sentinel-runner.js 路径
	SDKPath         string // 本地 sdk.js 缓存路径
	MaterialPath    string // 材料 JSON 路径（由浏览器侧车写入）；指向目录即为多槽模式
	MaterialDir     string // 材料目录（多槽）；优先级高于 MaterialPath
	MaterialTTL     time.Duration
	Timeout         time.Duration
	// MaxInflight 是「单账号在飞上限」。实测上游只放行 ~4 路并发，超出的会立刻
	// 403 "Error while processing conversation"（秒拒 ~1.9s）。因此把请求排在
	// 此闸门之后，让高并发变成**排队**而不是被拒。<=0 表示不限流。
	MaxInflight int
	// MinGap 是相邻两次上游请求的**最小间隔**（平滑放行）。实测突发会触发上游
	// 限流冷却期（此后一律秒拒 403），因此高并发必须错峰而不是同时打进。
	// <=0 表示不限制。
	MinGap time.Duration
	// CooldownOnRateLimit：上游返回「可重试的限流型 403」
	// （Error while processing conversation (403 …). Please submit prompt again.）
	// 时，账号级**熔断冷却**时长。实测突发后会进入一段冷却期，其间一律秒拒；
	// 主动退避比硬打更划算。<=0 表示不熔断。
	CooldownOnRateLimit time.Duration
	// SlotAttempts 是「换槽重试」上限（默认 3）。
	// ⚠️ 这是**上游调用放大器**：突发场景下它会把 N 路请求放大成 N×SlotAttempts 次上游
	// 调用，反而把自己的配额烧光、把账号推进更深的限流。因此在突发/限流敏感场景
	// 应下调（甚至设为 1）。<=0 用默认 3。
	SlotAttempts int
	// Transport 是可选的自定义 RoundTripper。
	// ⚠️ 生产环境**必须**注入 Chrome 指纹的 utls transport（proxy.NewUTLSTransport）：
	// 上游 prism.openai.com 在 Cloudflare 后面，会校验 JA3/JA4 TLS 指纹；裸 Go 指纹
	// 会被判定为非浏览器流量 → 403 "Error while processing conversation (403 Forbidden)"。
	// 更关键的是 cf_clearance cookie 是**绑定 IP + UA + TLS 指纹**的：材料（真 Chrome
	// 产出）里带着 cf_clearance，而请求却是 Go 指纹 —— 指纹不匹配即被拒。
	Transport http.RoundTripper
	Debug     bool
	Logf      func(string, ...any)
}

// DefaultConfig 返回可用默认值（路径按本机实测布局）。
func DefaultConfig() Config {
	return Config{
		Base:            "https://prism.openai.com",
		Model:           "gpt-6-astra",
		ReasoningEffort: "medium",
		SentinelRunner:  "/tmp/prism_sidecar/sentinel-runner.js",
		SDKPath:         "/tmp/prism_sidecar/assets/sdk.js",
		MaterialPath:    "/tmp/prism_sidecar/material.json",
		MaterialTTL:     2 * time.Minute,
		Timeout:         240 * time.Second,
	}
}

// Client 是单账号的 Prism 上游客户端。
type Client struct {
	cfg    Config
	cookie Cookie
	http   *http.Client

	// warmMu 串行化登录：防止 N 路并发各自 POST /auth/session（并发惊群会触发风控）。
	warmMu    sync.Mutex
	mu        sync.Mutex
	projectID string
	userID    string
	warmed    bool

	// matStore 是本 Client 专属的材料仓库（单份模式用）。
	// 必须 per-Client：多账号共享会导致身份错配。
	matStore     *materialStore
	materialOnce sync.Once

	// pool 是可选的材料池（多槽）。为 nil 时按需惰性创建（见 Pool()）。
	pool         *MaterialPool
	poolOnce     sync.Once
	poolDisabled bool // 显式禁用（测试用）

	// 账号级在飞闸门（单账号并发上限）。见 Config.MaxInflight。
	gate *inflightGate
}

// inflightGate 是「单账号在飞请求数」闸门：满员时新请求**排队等待**而非被上游 403。
type inflightGate struct {
	max      int
	minGap   time.Duration
	mu       sync.Mutex
	inflight int
	waiters  []chan struct{}

	paceMu    sync.Mutex // 串行化「放行时刻」，实现最小间隔
	lastStart time.Time

	coolMu    sync.Mutex // 账号级冷却
	coolUntil time.Time
	trips     int // 连续触发次数（递增退避；成功清零）
}

func newInflightGate(max int, minGap time.Duration) *inflightGate {
	if max <= 0 {
		return nil
	}
	return &inflightGate{max: max, minGap: minGap}
}

// tripCooldown 让账号进入冷却（**递增退避**）：第 n 次连续触发冷却 = base<<(n-1)，
// 上限 maxCooldown。上游限流越频繁说明压力越大，等得越久越划算。
func (g *inflightGate) tripCooldown(base time.Duration) {
	if base <= 0 {
		return
	}
	const maxCooldown = 15 * time.Minute
	g.coolMu.Lock()
	g.trips++
	shift := g.trips - 1
	if shift > 6 {
		shift = 6
	}
	d := base << shift
	if d > maxCooldown || d <= 0 {
		d = maxCooldown
	}
	if until := time.Now().Add(d); until.After(g.coolUntil) {
		g.coolUntil = until
	}
	g.coolMu.Unlock()
}

// resetTrips 在请求成功后清零连续触发计数。
func (g *inflightGate) resetTrips() {
	g.coolMu.Lock()
	g.trips = 0
	g.coolMu.Unlock()
}

// cooldownRemaining 返回当前剩余冷却时间（0 表示不在冷却）。
func (g *inflightGate) cooldownRemaining() time.Duration {
	g.coolMu.Lock()
	defer g.coolMu.Unlock()
	if r := time.Until(g.coolUntil); r > 0 {
		return r
	}
	return 0
}

// pace 让相邻请求的放行时刻至少相隔 minGap（平滑错峰）。返回后即可发请求。
func (g *inflightGate) pace(ctx context.Context) error {
	if g.minGap <= 0 {
		return nil
	}
	g.paceMu.Lock()
	defer g.paceMu.Unlock()
	if !g.lastStart.IsZero() {
		if wait := g.minGap - time.Since(g.lastStart); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	g.lastStart = time.Now()
	return nil
}

// acquire 取一个名额（ctx 取消时放弃）。
func (g *inflightGate) acquire(ctx context.Context) (func(), error) {
	// 账号级冷却：在冷却期内排队等待，而不是硬打被秒拒。
	if r := g.cooldownRemaining(); r > 0 {
		select {
		case <-time.After(r):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := g.pace(ctx); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.inflight < g.max {
		g.inflight++
		g.mu.Unlock()
		return g.release, nil
	}
	ch := make(chan struct{}, 1)
	g.waiters = append(g.waiters, ch)
	g.mu.Unlock()
	select {
	case <-ch:
		return g.release, nil
	case <-ctx.Done():
		// 已被唤醒则归还名额，否则从队列摘除
		g.mu.Lock()
		for i, w := range g.waiters {
			if w == ch {
				g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
				g.mu.Unlock()
				return nil, ctx.Err()
			}
		}
		g.mu.Unlock()
		// 名额已在唤醒时转交，需释放
		g.release()
		return nil, ctx.Err()
	}
}

func (g *inflightGate) release() {
	g.mu.Lock()
	if n := len(g.waiters); n > 0 {
		ch := g.waiters[0]
		g.waiters = g.waiters[1:]
		g.mu.Unlock()
		ch <- struct{}{} // 名额转交，inflight 不变
		return
	}
	g.inflight--
	g.mu.Unlock()
}

// Pool 返回材料池；未显式设置时按配置**惰性创建**（单文件 → 单槽池）。
//
// 多槽用法：把 PRISM_MATERIAL_PATH 指向**目录**（每份材料一个 .json），
// 或调用 SetMaterialPool 显式注入。
func (c *Client) Pool() *MaterialPool {
	c.poolOnce.Do(func() {
		if c.poolDisabled {
			return
		}
		src := envOr("PRISM_MATERIAL_PATH", c.cfg.MaterialPath)
		if c.cfg.MaterialDir != "" {
			src = c.cfg.MaterialDir
		}
		c.pool = NewMaterialPool(src, c.materialTTL(), c.cfg.Logf)
	})
	return c.pool
}

// SetMaterialPool 显式注入材料池（为 nil 表示禁用多槽，回到单份材料）。
func (c *Client) SetMaterialPool(p *MaterialPool) {
	c.poolOnce.Do(func() {})
	if p == nil {
		c.poolDisabled = true
		c.pool = nil
		return
	}
	c.pool = p
}

// materialTTL 取材料 TTL（环境变量优先）。
func (c *Client) materialTTL() time.Duration {
	if v := strings.TrimSpace(os.Getenv("PRISM_MATERIAL_TTL")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	if c.cfg.MaterialTTL > 0 {
		return c.cfg.MaterialTTL
	}
	return 2 * time.Minute
}

// NewClient 建客户端。
func NewClient(cfg Config, ck Cookie) *Client {
	if baseURLOverride != "" {
		cfg.Base = baseURLOverride // 测试接缝
	}
	if cfg.Base == "" {
		cfg.Base = "https://prism.openai.com"
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-6-astra"
	}
	if cfg.ReasoningEffort == "" {
		cfg.ReasoningEffort = "medium"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 240 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	httpCli := &http.Client{Timeout: 0} // 流式/长轮询：超时由 ctx 管
	if cfg.Transport != nil {
		httpCli.Transport = cfg.Transport
	}
	return &Client{
		cfg:    cfg,
		cookie: ck,
		http:   httpCli,
		gate:   newInflightGate(cfg.MaxInflight, cfg.MinGap),
	}
}

// isRetryableUpstreamErr 判断上游错误是否为「可重试的限流/处理型」错误。
// 实测文案：Error while processing conversation (403 Forbidden). Please submit prompt again.
// 上游自己提示「再提交一次」，因此这类错误应重试 + 账号级退避，而不是直接失败。
func isRetryableUpstreamErr(msg string) bool {
	if msg == "" {
		return false
	}
	l := strings.ToLower(msg)
	for _, k := range []string{
		"error while processing conversation",
		"please submit prompt again",
		"(403 forbidden)",
		"(429",
		"rate limit",
		"too many requests",
	} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

// noteUpstreamSuccess 在请求成功后清零连续触发计数（退避复位）。
func (c *Client) noteUpstreamSuccess() {
	if c.gate != nil {
		c.gate.resetTrips()
	}
}

// noteUpstreamError 在收到上游错误文本时更新账号级冷却（熔断）。
func (c *Client) noteUpstreamError(msg string) {
	if c.gate == nil || !isRetryableUpstreamErr(msg) {
		return
	}
	c.cfg.Logf("prism: 上游限流型错误，账号冷却 %s（%s）",
		c.cfg.CooldownOnRateLimit, shortErr(errors.New(msg)))
	c.gate.tripCooldown(c.cfg.CooldownOnRateLimit)
}

// ------------------------------------------------------------------ 基础设施

// do 发一次带 Prism 身份的请求。
func (c *Client) do(ctx context.Context, method, path, body, sentinel string, extra map[string]string) (*http.Response, error) {
	return c.doRaw(ctx, method, path, []byte(body), sentinel, extra)
}

// doRaw 与 do 相同，但 body 是**原始字节**（文件上传用）。
// headers 里的键会覆盖默认（如 content-type 要设成 image/png 而非 json）。
func (c *Client) doRaw(ctx context.Context, method, path string, body []byte, sentinel string, extra map[string]string) (*http.Response, error) {
	// 单账号在飞闸门：满员则排队等待，避免被上游秒拒 403（实测在飞上限 ~4）。
	if c.gate != nil {
		done, err := c.gate.acquire(ctx)
		if err != nil {
			return nil, err
		}
		defer done()
	}
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	url := path
	if !strings.HasPrefix(path, "http") {
		url = strings.TrimRight(c.cfg.Base, "/") + path
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.cookie.UserAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", strings.TrimRight(c.cfg.Base, "/"))
	req.Header.Set("Referer", strings.TrimRight(c.cfg.Base, "/")+"/")
	req.Header.Set("frontend-origin", strings.TrimRight(c.cfg.Base, "/"))
	req.Header.Set("Cookie", c.cookie.cookieHeader())
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if sentinel != "" {
		req.Header.Set("openai-sentinel-token", sentinel)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	return c.http.Do(req)
}

// WarmSession 建立会话（自举 prism_session_token）并缓存 projectId/userId。
//
// ⚠️ 必须**防并发惊群**：若 N 路并发各自 POST /auth/session，会瞬间向上游
// 打 N 个登录请求。实测：批量并发探测后账号随即开始 403（风控）。
// 因此这里用「双重检查 + 串行化」：只放行一个真登录，其余复用结果；
// 失败不缓存（下次可重试）。
func (c *Client) WarmSession(ctx context.Context) error {
	c.mu.Lock()
	if c.warmed {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	c.warmMu.Lock()
	defer c.warmMu.Unlock()
	// 二次检查：等锁期间可能已被别的 goroutine 完成
	c.mu.Lock()
	if c.warmed {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	return c.doWarm(ctx)
}

// doWarm 是实际的登录流程（调用方需持有 warmMu）。
func (c *Client) doWarm(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/auth/session", "", "", nil)
	if err != nil {
		return fmt.Errorf("prism: auth/session: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("prism: auth/session 状态 %d（凭据可能失效）", resp.StatusCode)
	}
	var out struct {
		User struct {
			ID          string `json:"id"`
			AppMetadata struct {
				UserID string `json:"user_id"`
			} `json:"app_metadata"`
		} `json:"user"`
		Policy struct {
			User struct {
				OpenAIUserID string `json:"openai_user_id"`
			} `json:"user"`
		} `json:"policy"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("prism: auth/session 解析: %w", err)
	}
	// 自举 prism_session_token
	for _, sc := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(sc, "prism_session_token=") {
			if i := strings.Index(sc, ";"); i > 0 {
				c.cookie.SessionToken = strings.TrimPrefix(sc[:i], "prism_session_token=")
			}
		}
	}
	// userId 必须是 user- 前缀形态（实测：用 account UUID 会 400）
	uid := out.User.AppMetadata.UserID
	if uid == "" {
		uid = out.Policy.User.OpenAIUserID
	}
	c.mu.Lock()
	c.userID = uid
	c.warmed = true
	c.mu.Unlock()
	c.cfg.Logf("prism: 会话就绪 userId=%s session_token=%v", uid, c.cookie.SessionToken != "")
	return nil
}

// ProjectID 返回（并缓存）首个项目 uuid。
func (c *Client) ProjectID(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.projectID != "" {
		id := c.projectID
		c.mu.Unlock()
		return id, nil
	}
	c.mu.Unlock()

	s, err := c.Mint(ctx)
	if err != nil {
		return "", err
	}
	resp, err := c.do(ctx, http.MethodGet, "/api/projects", "", s, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("prism: /api/projects 状态 %d", resp.StatusCode)
	}
	var out struct {
		Projects []struct {
			UUID string `json:"uuid"`
		} `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Projects) == 0 {
		return "", fmt.Errorf("prism: 账号下无项目（请先在 Prism 建一个）")
	}
	c.mu.Lock()
	c.projectID = out.Projects[0].UUID
	id := c.projectID
	c.mu.Unlock()
	return id, nil
}

// UserID 返回缓存的 userId（须先 WarmSession）。
func (c *Client) UserID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.userID
}

// Close 释放资源（当前无长连接，占位以便未来复用连接池）。
func (c *Client) Close() {}

// envOr 读环境变量兜底。
func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
