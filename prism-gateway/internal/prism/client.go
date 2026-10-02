package prism

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Client = 一个账号的上游客户端。承载完整的预热链与对话轮次。
//
// 关键事实（12 个实现的一致结论）：
//  1. 上游没有 SSE，一轮对话 = start（提交）+ status（轮询），正文一次性到达。
//  2. start 前必须先走完沙箱预热链，缺任何一步 start 会无限挂起。
//  3. turn_state 必须逐轮原样回传，否则 401。
//  4. tools 字段被上游忽略，客户端工具只能靠提示词仿真 + 文本解析。
// ============================================================================

// Options 是 Client 的可调项（与 config.Config 解耦，便于单测）。
type Options struct {
	Origin          string
	UserAgent       string
	StartTimeout    time.Duration
	PollCallTimeout time.Duration
	PollBudget      time.Duration
	SyncPollTries   int
	StartAttempts   int
	// Fingerprint 是 TLS 指纹 transport 注入器（生产应注入浏览器指纹，
	// 与材料来源浏览器一致；本地明文上游/单测可留 nil）。
	Fingerprint   FingerprintInjector
	MaxReconnects int
	Transport     TransportConfig
	// PreferMaterialMetadata 为 true 时，start 的 metadata 优先由外部材料包提供
	// （上游把 start 与页面上下文强绑定，材料包是唯一被接受的来源）。
	MaterialFunc func(ctx context.Context) (*Metadata, error)
}

// Client 是一个账号的上游会话。并发安全。
type Client struct {
	opt  Options
	acct *Account
	hc   *http.Client

	mu         sync.Mutex
	projectID  string
	sandbox    string // X-Crixet-Sandbox-Token
	sandboxURL string
	sandboxAt  time.Time
	// sandboxGen 是沙箱令牌的代际号。慢 flight 拿到的旧令牌不允许覆盖
	// 快 flight 已发布的新令牌（否则会把过期令牌写回去给上游，表现为
	// sandbox_reconnecting 或 401）。
	sandboxGen uint64
	userID     string
	sessionAt  time.Time

	// 建沙箱的 singleflight：高并发下 N 个请求只付一次冷链（6~18s）。
	inflight *sandboxFlight

	stats Stats
}

// Stats 是账号级观测计数。
type Stats struct {
	Starts    int64
	Polls     int64
	SandboxMs int64
	StartMs   int64
	PollMs    int64
	Streams   int64
	Failures  int64
	TokensIn  int64
	TokensOut int64
	CacheRead int64
}

// NewClient 为账号建客户端。
func NewClient(opt Options, acct *Account) *Client {
	if opt.Origin == "" {
		opt.Origin = "https://prism.openai.com"
	}
	opt.Origin = strings.TrimRight(opt.Origin, "/")
	if opt.UserAgent == "" {
		opt.UserAgent = defaultUserAgent
	}
	if opt.StartTimeout == 0 {
		opt.StartTimeout = 75 * time.Second
	}
	if opt.PollCallTimeout == 0 {
		opt.PollCallTimeout = 30 * time.Second
	}
	if opt.PollBudget == 0 {
		opt.PollBudget = 240 * time.Second
	}
	if opt.SyncPollTries == 0 {
		opt.SyncPollTries = 4
	}
	if opt.StartAttempts == 0 {
		opt.StartAttempts = 3
	}
	if opt.MaxReconnects == 0 {
		opt.MaxReconnects = 2
	}
	if acct.UserAgent == "" {
		acct.UserAgent = opt.UserAgent
	}
	return &Client{
		opt:  opt,
		acct: acct,
		hc:   mustHTTPClient(opt),
	}
}

// Account 返回归属账号。
func (c *Client) Account() *Account { return c.acct }

// Origin 返回上游基址。
func (c *Client) Origin() string { return c.opt.Origin }

// SnapshotStats 返回并清零增量统计（观测用）。
func (c *Client) SnapshotStats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	c.stats = Stats{}
	return s
}

// ------------------------------------------------------------------ 会话

// EnsureSession 校验 cookie 并取回身份（best-effort，失败不阻断）。
// 认证不是 Bearer：cookie 缺 oai-sc 会得到
// "401 Could not parse your authentication token"。
func (c *Client) EnsureSession(ctx context.Context) error {
	c.mu.Lock()
	fresh := time.Since(c.sessionAt) < 3*time.Minute && c.userID != ""
	c.mu.Unlock()
	if fresh {
		return nil
	}
	resp, err := c.Do(ctx, Request{
		Method:  http.MethodGet,
		URL:     JoinURL(c.opt.Origin, PathSession),
		Account: c.acct,
		Timeout: 20 * time.Second,
	}, 2)
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	if resp.Status >= 300 {
		return &UpstreamError{Status: resp.Status, Op: "auth/session", Msg: resp.Snippet(200)}
	}
	v := resp.JSON()
	if v == nil {
		return nil
	}
	if user, ok := v["user"].(map[string]any); ok {
		if id, _ := user["id"].(string); id != "" {
			c.mu.Lock()
			c.userID = id
			if c.acct.UserID == "" {
				c.acct.UserID = id
			}
			if email, _ := user["email"].(string); email != "" && c.acct.Label == "" {
				c.acct.Label = email
			}
			c.mu.Unlock()
		}
	}
	c.mu.Lock()
	c.sessionAt = time.Now()
	c.mu.Unlock()
	return nil
}

// UserID 返回账号 userId（未知时回退账号 id）。
func (c *Client) UserID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.userID != "" {
		return c.userID
	}
	return c.acct.UserIDOr()
}

// ------------------------------------------------------------------ 项目

// EnsureProject 确保有一个可用项目 id。
// 实测坑：上游会清理 API 自建项目（旧项目 resources-token 404 "Invalid URL"，
// 实为项目不存在），此时必须重建项目后重试一次。
func (c *Client) EnsureProject(ctx context.Context) (string, error) {
	c.mu.Lock()
	if c.projectID != "" {
		pid := c.projectID
		c.mu.Unlock()
		return pid, nil
	}
	seed := c.acct.ProjectID
	c.mu.Unlock()

	if seed != "" {
		c.mu.Lock()
		c.projectID = seed
		c.mu.Unlock()
		return seed, nil
	}

	resp, err := c.Do(ctx, Request{
		Method:  http.MethodGet,
		URL:     JoinURL(c.opt.Origin, PathProjectList),
		Account: c.acct,
		Timeout: 20 * time.Second,
	}, 2)
	if err == nil && resp.Status < 300 {
		if pid := firstProjectID(resp.Body); pid != "" {
			c.setProject(pid)
			return pid, nil
		}
	}

	// 列表为空：自建一个项目。
	resp, err = c.Do(ctx, Request{
		Method:  http.MethodPost,
		URL:     JoinURL(c.opt.Origin, PathProjects),
		Body:    map[string]any{"name": "gateway", "type": "latex"},
		Account: c.acct,
		Timeout: 25 * time.Second,
	}, 2)
	if err != nil {
		return "", fmt.Errorf("create project: %w", err)
	}
	if resp.Status >= 300 {
		return "", &UpstreamError{Status: resp.Status, Op: "api/projects", Msg: resp.Snippet(200)}
	}
	if pid := firstProjectID(resp.Body); pid != "" {
		c.setProject(pid)
		return pid, nil
	}
	if v := resp.JSON(); v != nil {
		for _, k := range []string{"id", "projectId", "project_id"} {
			if s, _ := v[k].(string); s != "" {
				c.setProject(s)
				return s, nil
			}
		}
	}
	return "", fmt.Errorf("create project: 上游未返回项目 id")
}

// firstProjectID 从项目列表/创建响应里挖出第一个项目 id（容错多种形状）。
func firstProjectID(body []byte) string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	var walk func(x any, depth int) string
	walk = func(x any, depth int) string {
		if depth > 4 {
			return ""
		}
		switch t := x.(type) {
		case []any:
			for _, e := range t {
				if s := walk(e, depth+1); s != "" {
					return s
				}
			}
		case map[string]any:
			for _, k := range []string{"projectId", "project_id", "id", "uuid"} {
				if s, ok := t[k].(string); ok && looksLikeID(s) {
					return s
				}
			}
			for _, k := range []string{"projects", "data", "items", "results"} {
				if sub, ok := t[k]; ok {
					if s := walk(sub, depth+1); s != "" {
						return s
					}
				}
			}
		}
		return ""
	}
	return walk(v, 0)
}

// looksLikeID 判断一个字符串像不像项目 id。
// 上游项目 id 形态不统一（24 位十六进制、uuid、带前缀的短 id 都见过），
// 所以这里只排除明显不是 id 的值，不做长度下限的过度约束。
func looksLikeID(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 3 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	// 排除常见非 id 字面量。
	switch strings.ToLower(s) {
	case "true", "false", "null", "none", "undefined", "latex", "default":
		return false
	}
	return true
}

func (c *Client) setProject(pid string) {
	c.mu.Lock()
	c.projectID = pid
	c.mu.Unlock()
}

// ------------------------------------------------------------------ 沙箱

// Sandbox 返回当前热沙箱（token + url），无则空。
func (c *Client) Sandbox() (token, url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sandbox, c.sandboxURL
}

// SandboxToken 返回当前沙箱 token（热路径判断用）。
func (c *Client) SandboxToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sandbox
}

// InvalidateSandbox 作废当前沙箱（start 失败/5xx 时调用，下次重新预热）。
func (c *Client) InvalidateSandbox() {
	c.mu.Lock()
	c.sandbox = ""
	c.sandboxURL = ""
	c.sandboxAt = time.Time{}
	c.sandboxGen++
	c.mu.Unlock()
}

// sandboxFlight 是建沙箱的单飞：并发请求共享同一次冷链。
type sandboxFlight struct {
	done chan struct{}
	tok  string
	url  string
	err  error
}

// EnsureSandbox 返回一个已同步的沙箱；并发调用会合并成一次预热。
func (c *Client) EnsureSandbox(ctx context.Context, projectID string) (string, string, error) {
	c.mu.Lock()
	if c.sandbox != "" {
		tok, u := c.sandbox, c.sandboxURL
		c.mu.Unlock()
		return tok, u, nil
	}
	if fl := c.inflight; fl != nil {
		c.mu.Unlock()
		select {
		case <-fl.done:
			return fl.tok, fl.url, fl.err
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	fl := &sandboxFlight{done: make(chan struct{})}
	c.inflight = fl
	c.sandboxGen++
	gen := c.sandboxGen
	c.mu.Unlock()

	tok, url, err := c.prepareSandbox(ctx, projectID)

	c.mu.Lock()
	fl.tok, fl.url, fl.err = tok, url, err
	// 代际保护：只有比已发布令牌更新的一代才允许写入。
	if err == nil && gen >= c.sandboxGen {
		c.sandbox, c.sandboxURL, c.sandboxAt = tok, url, time.Now()
	} else if err == nil {
		// 已有更新的一代发布过令牌：丢弃自己的旧令牌，返回当前生效的那个。
		tok, url = c.sandbox, c.sandboxURL
		fl.tok, fl.url = tok, url
	}
	c.inflight = nil
	c.mu.Unlock()
	close(fl.done)
	return tok, url, err
}

// prepareSandbox 走完整预热链。缺任何一步 wait-for-sync 会永远停在 syncing。
func (c *Client) prepareSandbox(ctx context.Context, projectID string) (string, string, error) {
	// ① 建沙箱（正常 1.4~2.5s，劣化期 24~210s）。
	resp, err := c.Do(ctx, Request{
		Method:  http.MethodPost,
		URL:     JoinURL(c.opt.Origin, PathBackendNew),
		Body:    map[string]any{},
		Account: c.acct,
		Timeout: 60 * time.Second,
	}, 2)
	if err != nil {
		return "", "", fmt.Errorf("backend/1/new: %w", err)
	}
	if resp.Status >= 300 {
		return "", "", &UpstreamError{Status: resp.Status, Op: "backend/1/new", Msg: resp.Snippet(200)}
	}
	v := resp.JSON()
	tok, _ := v["token"].(string)
	if tok == "" {
		return "", "", fmt.Errorf("backend/1/new 未返回 sandbox token")
	}
	sandboxBase := c.opt.Origin + PathSandboxBase
	if u, _ := v["url"].(string); strings.TrimSpace(u) != "" {
		sandboxBase = strings.TrimRight(strings.TrimSpace(u), "/")
	}

	// ② 项目级 → 沙箱级资源令牌。
	resourceToken, err := c.resourcesToken(ctx, projectID, tok)
	if err != nil {
		// 项目被上游清理：重建后重试一次。
		if ue, ok := err.(*UpstreamError); ok && ue.Status == http.StatusNotFound {
			c.setProject("")
			newPID, perr := c.EnsureProject(ctx)
			if perr != nil {
				return "", "", fmt.Errorf("recreate project: %w", perr)
			}
			resourceToken, err = c.resourcesToken(ctx, newPID, tok)
			if err != nil {
				return "", "", err
			}
			projectID = newPID
		} else {
			return "", "", err
		}
	}

	// ③ 注册资源令牌到沙箱。
	if err := c.registerResourceToken(ctx, sandboxBase, tok, resourceToken, projectID); err != nil {
		return "", "", err
	}

	// ④ Y-Sweet（协作文件）令牌交棒：缺这一步 wait-for-sync 永远停在 syncing。
	if err := c.handoffYSweet(ctx, sandboxBase, tok, projectID); err != nil {
		return "", "", err
	}

	// ⑤ 等同步完成。
	if err := c.waitForSync(ctx, sandboxBase, tok); err != nil {
		return "", "", err
	}
	return tok, sandboxBase, nil
}

func (c *Client) resourcesToken(ctx context.Context, projectID, tok string) (string, error) {
	resp, err := c.Do(ctx, Request{
		Method:  http.MethodPost,
		URL:     c.opt.Origin + fmt.Sprintf(PathResourcesToken, projectID),
		Body:    map[string]any{"sandbox_session_id": nil, "sandbox_token": tok},
		Account: c.acct,
		Timeout: 30 * time.Second,
	}, 3)
	if err != nil {
		return "", fmt.Errorf("sandbox/resources-token: %w", err)
	}
	if resp.Status >= 300 {
		return "", &UpstreamError{Status: resp.Status, Op: "sandbox/resources-token", Msg: resp.Snippet(200)}
	}
	rt, _ := resp.JSON()["access_token"].(string)
	if rt == "" {
		return "", fmt.Errorf("sandbox/resources-token 未返回 access_token")
	}
	return rt, nil
}

func (c *Client) registerResourceToken(ctx context.Context, sandboxBase, tok, resourceToken, projectID string) error {
	resp, err := c.Do(ctx, Request{
		Method: http.MethodPost,
		URL:    sandboxBase + PathProxyResources + "?prism_cache_bust=" + cacheBust(),
		Body: map[string]any{
			"token":           resourceToken,
			"resourceBaseUrl": c.opt.Origin + PathResourceBase,
			"projectId":       projectID,
		},
		Headers: map[string]string{HeaderSandboxToken: tok},
		Account: c.acct,
		Timeout: 30 * time.Second,
	}, 3)
	if err != nil {
		return fmt.Errorf("proxy/resources-token: %w", err)
	}
	if resp.Status >= 300 {
		return &UpstreamError{Status: resp.Status, Op: "proxy/resources-token", Msg: resp.Snippet(200)}
	}
	return nil
}

func (c *Client) handoffYSweet(ctx context.Context, sandboxBase, tok, projectID string) error {
	resp, err := c.Do(ctx, Request{
		Method:  http.MethodPost,
		URL:     JoinURL(c.opt.Origin, PathYSweet),
		Body:    map[string]any{"docId": projectID},
		Account: c.acct,
		Timeout: 25 * time.Second,
	}, 3)
	if err != nil {
		return fmt.Errorf("api/y: %w", err)
	}
	if resp.Status >= 300 {
		return &UpstreamError{Status: resp.Status, Op: "api/y", Msg: resp.Snippet(200)}
	}
	y := resp.JSON()
	resp, err = c.Do(ctx, Request{
		Method: http.MethodPost,
		URL:    sandboxBase + PathProxyToken + "?prism_cache_bust=" + cacheBust(),
		Body: map[string]any{
			"url":           y["url"],
			"baseUrl":       y["baseUrl"],
			"docId":         projectID,
			"token":         y["token"],
			"authorization": y["authorization"],
		},
		Headers: map[string]string{HeaderSandboxToken: tok},
		Account: c.acct,
		Timeout: 30 * time.Second,
	}, 3)
	if err != nil {
		return fmt.Errorf("proxy/token: %w", err)
	}
	if resp.Status >= 300 {
		return &UpstreamError{Status: resp.Status, Op: "proxy/token", Msg: resp.Snippet(200)}
	}
	return nil
}

// waitForSync 轮询到 status=synced。
func (c *Client) waitForSync(ctx context.Context, sandboxBase, tok string) error {
	last := ""
	for i := 0; i < c.opt.SyncPollTries; i++ {
		resp, err := c.Do(ctx, Request{
			Method:  http.MethodGet,
			URL:     sandboxBase + PathWaitForSync,
			Headers: map[string]string{HeaderSandboxToken: tok},
			Account: c.acct,
			Timeout: 20 * time.Second,
		}, 2)
		if err != nil {
			return fmt.Errorf("wait-for-sync: %w", err)
		}
		if resp.Status >= 300 {
			return &UpstreamError{Status: resp.Status, Op: "wait-for-sync", Msg: resp.Snippet(200)}
		}
		v := resp.JSON()
		last, _ = v["status"].(string)
		if last == "synced" {
			return nil
		}
		if err := sleepCtx(ctx, 1200*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("sandbox 未同步（末次 status=%q，缺 Y-Sweet 令牌交棒时会一直停在这里）", last)
}

// Heartbeat 沙箱探活/预热（低成本，可用于后台保活）。
func (c *Client) Heartbeat(ctx context.Context) error {
	tok, base := c.Sandbox()
	if tok == "" {
		return fmt.Errorf("heartbeat: 无热沙箱")
	}
	resp, err := c.Do(ctx, Request{
		Method:  http.MethodGet,
		URL:     base + PathProxyHeartbeat + "?prism_cache_bust=" + cacheBust(),
		Headers: map[string]string{HeaderSandboxToken: tok},
		Account: c.acct,
		Timeout: 15 * time.Second,
	}, 1)
	if err != nil {
		c.InvalidateSandbox()
		return err
	}
	if resp.Status >= 300 {
		if resp.Status == 401 || resp.Status == 403 || resp.Status == 404 {
			c.InvalidateSandbox()
		}
		return &UpstreamError{Status: resp.Status, Op: "heartbeat", Msg: resp.Snippet(160)}
	}
	return nil
}

// ------------------------------------------------------------------ 会话登记

// RegisterConversation 让后端登记会话 id（best-effort，失败不阻断）。
func (c *Client) RegisterConversation(ctx context.Context, projectID, conv string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 6*time.Second)
	defer cancel()
	resp, err := c.Do(ctx, Request{
		Method: http.MethodPost,
		URL:    JoinURL(c.opt.Origin, PathConversationHistory),
		Body: ConversationRegistration{
			ConversationID: conv, Order: "desc", Limit: 50,
			UserID: c.UserID(), ProjectID: projectID,
		},
		Account: c.acct,
		Timeout: 5 * time.Second,
	}, 0)
	if err != nil {
		return nil
	}
	if resp.Status == 401 || resp.Status == 403 {
		return &UpstreamError{Status: resp.Status, Op: "conversation-history", Msg: resp.Snippet(200)}
	}
	return nil
}

// SandboxAge 返回当前沙箱已存在多久；无沙箱返回 0。
func (c *Client) SandboxAge() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sandboxAt.IsZero() {
		return 0
	}
	return time.Since(c.sandboxAt)
}

// TouchSandbox 刷新沙箱最近使用时间（命中即刷新，避免被 TTL/2 提前重铸）。
func (c *Client) TouchSandbox() {
	c.mu.Lock()
	if c.sandbox != "" {
		c.sandboxAt = time.Now()
	}
	c.mu.Unlock()
}

// mustHTTPClient 按 Options 构造出站客户端。
//
// 若配置要求浏览器指纹却注入失败（例如构建漏了对应实现），**直接 panic**：
// 这种情况静默退回原生指纹只会让每次请求都 403，属于「看起来在跑、实际全废」
// 的隐蔽故障 —— 宁可在启动时炸响。
func mustHTTPClient(opt Options) *http.Client {
	hc, err := NewHTTPClient(opt.Transport, opt.Origin, opt.Fingerprint)
	if err != nil {
		panic(fmt.Sprintf("prism: 构造出站客户端失败: %v", err))
	}
	return hc
}
