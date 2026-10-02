package prism

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ============================================================================
// 传输层：一个账号一个 http.Client（独立连接池 + cookie 由调用方逐请求带上）。
// 生产实测坑：Cloudflare 上 Go 的 HTTP/2 连接复用偶发 PROTOCOL_ERROR，
// 因此默认强制 HTTP/1.1，并对传输层错误重试。
// ============================================================================

// TransportConfig 是传输层可调项。
type TransportConfig struct {
	// Profile 让出站 TLS 指纹与**材料来源浏览器**保持一致。
	//
	// 为什么需要：上游（Cloudflare）按 JA3/JA4 判定客户端是否为浏览器，并把
	// cf_clearance 绑定到 IP+UA+TLS 指纹。若这里用 Go 原生指纹，就与材料里的
	// cf_clearance 不匹配 → 403。所以生产环境必须注入浏览器指纹 transport。
	//
	// 取值：空 / "go" = 原生指纹（仅本地明文上游、单测可用）；"chrome" = Chrome 指纹。
	Profile         string
	ForceHTTP1      bool
	MaxIdleConns    int
	IdleConnTimeout time.Duration
	DialTimeout     time.Duration
	TLSHandshake    time.Duration
	ResponseTimeout time.Duration
}

// ProfileGo 是 Go 原生 TLS 指纹（默认；单测/明文上游用）。
const ProfileGo = "go"

// ProfileChrome 是 Chrome TLS 指纹（生产默认，与材料来源浏览器一致）。
const ProfileChrome = "chrome"

// DefaultTransportConfig 给出生产默认值。
func DefaultTransportConfig() TransportConfig {
	return TransportConfig{
		// ⚠️ 默认必须是 ProfileGo：本 module 是**零第三方依赖**（go.mod 无 require），
		// 自身无法做 TLS 指纹伪装。想要 Chrome 指纹，必须由宿主程序注入
		// FingerprintInjector（见 NewHTTPClient 注释与 docs）。
		Profile:         ProfileGo,
		ForceHTTP1:      true,
		MaxIdleConns:    64,
		IdleConnTimeout: 90 * time.Second,
		DialTimeout:     10 * time.Second,
		TLSHandshake:    10 * time.Second,
		ResponseTimeout: 0, // 由每请求 ctx 控制
	}
}

// FingerprintInjector 由调用方提供浏览器指纹 transport 构造器
// （prism-gateway 本体零第三方依赖，故用注入的方式接入 utls 等实现）。
//
// 返回 nil 表示「按 Profile 无法构造」→ 调用方应报错而不是静默退回原生指纹，
// 否则会出现「看起来在跑、实际每次 403」的隐蔽故障。
type FingerprintInjector func(proxyURL string) (http.RoundTripper, error)

// NewHTTPClient 为一个账号建独立客户端（连接池隔离，避免一个坏账号拖累全池）。
//
// url 是上游基址：**只有 https 上游才注入指纹** —— 本地单测常用明文 httptest
// 上游，给明文套 uTLS 无法握手。
func NewHTTPClient(tc TransportConfig, url string, inject FingerprintInjector) (*http.Client, error) {
	if wantsFingerprint := tc.Profile != "" && tc.Profile != ProfileGo; wantsFingerprint &&
		strings.HasPrefix(strings.ToLower(url), "https://") {
		if inject == nil {
			// 绝不静默退回原生指纹：那会让每次请求都 403，属于
			// 「看起来在跑、实际全废」的隐蔽故障。宁可在启动时炸响。
			return nil, fmt.Errorf(
				"要求 TLS 指纹 profile=%q 但未注入实现（本 module 零第三方依赖，"+
					"需宿主程序注入 FingerprintInjector）", tc.Profile)
		}
		rt, err := inject("")
		if err != nil {
			return nil, fmt.Errorf("注入 TLS 指纹(%s)失败: %w", tc.Profile, err)
		}
		return &http.Client{
			Transport: rt,
			Timeout:   tc.ResponseTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}, nil
	}

	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   tc.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          tc.MaxIdleConns,
		MaxIdleConnsPerHost:   tc.MaxIdleConns,
		IdleConnTimeout:       tc.IdleConnTimeout,
		TLSHandshakeTimeout:   tc.TLSHandshake,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     !tc.ForceHTTP1,
	}
	if tc.ForceHTTP1 {
		// 显式清空 TLSNextProto 会禁掉 h2 升级（Go 里 ForceAttemptHTTP2=false
		// 已足够，但某些中间件仍会协商 h2；这里双保险）。
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   tc.ResponseTimeout,
		// 不跟重定向：上游的 3xx 是信号，不是要跟的路。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

// Request 是一次上游调用的描述。
type Request struct {
	Method  string
	URL     string
	Body    any               // 非 nil 时 JSON 序列化
	RawBody []byte            // 与 Body 二选一
	Headers map[string]string // 额外 header
	Account *Account          // 提供 Cookie 与 UA
	Timeout time.Duration     // 单次调用上限
}

// Response 是归一化后的上游响应。
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSON 把响应体解成 map（失败返回 nil，不报错——上游错误页可能是 HTML）。
func (r *Response) JSON() map[string]any {
	var m map[string]any
	if r == nil || len(r.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.Body, &m); err != nil {
		return nil
	}
	return m
}

// Snippet 返回响应体前 n 字节（日志用，防泄漏：只给前缀）。
func (r *Response) Snippet(n int) string {
	if r == nil {
		return ""
	}
	s := strings.TrimSpace(string(r.Body))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// UpstreamError 是带状态码的上游错误，便于失败分类。
type UpstreamError struct {
	Status int
	Op     string
	Msg    string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream %s: status=%d %s", e.Op, e.Status, e.Msg)
}

// AuthError 报告认证类失败（401/403），触发账号冷却。
func (e *UpstreamError) AuthError() bool { return e.Status == 401 || e.Status == 403 }

// Retryable 报告是否值得重试（5xx / 网关超时 / 429）。
func (e *UpstreamError) Retryable() bool {
	return e.Status >= 500 || e.Status == http.StatusTooManyRequests || e.Status == http.StatusGatewayTimeout
}

// ------------------------------------------------------------------ 执行

// Do 执行一次上游请求；networkRetries 指传输层错误（超时/断连）的额外重试次数。
func (c *Client) Do(ctx context.Context, req Request, networkRetries int) (*Response, error) {
	if networkRetries < 0 {
		networkRetries = 0
	}
	var lastErr error
	for attempt := 0; attempt <= networkRetries; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, time.Duration(attempt)*700*time.Millisecond); err != nil {
				return nil, err
			}
		}
		resp, err := c.doOnce(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// doOnce 单次执行，含超时与 header 组装。
func (c *Client) doOnce(ctx context.Context, req Request) (*Response, error) {
	callCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	var bodyReader io.Reader
	switch {
	case len(req.RawBody) > 0:
		bodyReader = bytes.NewReader(req.RawBody)
	case req.Body != nil:
		b, err := json.Marshal(req.Body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	httpReq, err := http.NewRequestWithContext(callCtx, req.Method, req.URL, bodyReader)
	if err != nil {
		return nil, err
	}
	// 浏览器身份：UA + Cookie 必须与 cf_clearance 绑定的一致。
	ua := c.opt.UserAgent
	cookie := ""
	if req.Account != nil {
		if strings.TrimSpace(req.Account.UserAgent) != "" {
			ua = req.Account.UserAgent
		}
		cookie = req.Account.Cookie
	}
	if ua != "" {
		httpReq.Header.Set(HeaderUserAgent, ua)
	}
	if cookie != "" {
		httpReq.Header.Set("Cookie", cookie)
	}
	httpReq.Header.Set("Accept", "application/json, text/plain, */*")
	httpReq.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	if req.Body != nil || len(req.RawBody) > 0 {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	// 站点身份头（真机抓包：浏览器页面请求会带）。
	httpReq.Header.Set("Origin", c.opt.Origin)
	httpReq.Header.Set("Referer", c.opt.Origin+"/")
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// 上限保护：上游异常时可能吐超大 body。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// ------------------------------------------------------------------ 小工具

// sleepCtx 可取消的 sleep。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// cacheBust 生成 cache-bust 查询值（上游沙箱面要求绕缓存）。
func cacheBust() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%d", time.Now().UnixNano()/1e6) + hex.EncodeToString(b)[:6]
}

// uuid4 生成 v4 UUID（会话 id 用，前缀 cdx1_）。
func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewConversationID 生成上游风格的会话 id。
func NewConversationID() string { return "cdx1_" + uuid4() }

// NewCallID 生成工具调用 id（客户端要拿它回灌 tool_call_id）。
func NewCallID() string { return "call_" + strings.ReplaceAll(uuid4(), "-", "") }

// JoinURL 拼接 base 与 path（避免双斜杠）。
func JoinURL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}
