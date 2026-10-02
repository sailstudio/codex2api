// Package provider 抽象「上游提供方」。
//
// 本网关有两条完全不同的上游路径：
//
//  1. prism   —— prism.openai.com 私协议（start/poll + 沙箱 + Cookie），
//     需要自己合成 SSE、仿真工具调用。见 internal/prism。
//  2. openai-compat —— 任何说 OpenAI/Anthropic 协议的上游（如本地部署的
//     codex2api:8080），直接透传即可，且它自带账号池/OAuth/计费。
//
// 本包只负责第 2 类，并提供按模型名的路由器：命中 codex 模型走 codex2api，
// 其余走 prism。这样一套 /v1 门面同时服务两种后端。
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAICompat 是一个 OpenAI/Anthropic 兼容的上游（透传型）。
//
// 为什么需要它：codex2api 这类项目已经把「Codex 账号 → OpenAI/Anthropic 协议」
// 做完了，网关再翻译一遍纯属重复劳动且有信息损失。正确做法是**原样透传**，
// 只补鉴权与路由——这也是「透明反代」在网关内部的正当用法。
type OpenAICompat struct {
	Name       string            // 提供方名（日志/观测用）
	BaseURL    string            // 如 http://127.0.0.1:8080
	APIKey     string            // 上游要求的 key（会以 Bearer 转发）
	ExtraHdrs  map[string]string // 额外 header（如 x-api-key）
	HTTPClient *http.Client
	Timeout    time.Duration
}

// NewOpenAICompat 建一个透传提供方。
func NewOpenAICompat(name, baseURL, apiKey string) *OpenAICompat {
	return &OpenAICompat{
		Name:    name,
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTPClient: &http.Client{
			// 不设总超时：流式响应可能持续数分钟，超时由 ctx 与 IdleTimeout 管。
			Timeout: 0,
		},
		Timeout: 10 * time.Minute,
	}
}

// Ready 报告是否可用（未配置 base 则不可用）。
func (p *OpenAICompat) Ready() bool { return p != nil && strings.TrimSpace(p.BaseURL) != "" }

// Proxy 把一次请求原样转发给上游，并把响应（含 SSE 流）逐块回灌。
//
// 关键点：
//   - 逐块 copy，不缓冲：SSE 才能实时到达客户端（低延迟）。
//   - 立即 Flush：否则中间代理会攒满缓冲才发。
//   - 客户端断连 → 取消上游请求（省上游额度）。
//   - 透传 hop-by-hop 之外的所有响应头（含 Content-Type / x-request-id）。
func (p *OpenAICompat) Proxy(w http.ResponseWriter, r *http.Request, path string) error {
	if !p.Ready() {
		return fmt.Errorf("提供方 %s 未配置", p.Name)
	}
	ctx := r.Context()
	if p.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.Timeout)
		defer cancel()
	}

	url := p.BaseURL + path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, url, r.Body)
	if err != nil {
		return err
	}
	// 只透传必要 header，避免把客户端的鉴权/主机信息泄漏给上游。
	for _, h := range []string{"Content-Type", "Accept", "OpenAI-Beta", "anthropic-version", "anthropic-beta"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
		// 兼容 Anthropic 风格的上游
		req.Header.Set("x-api-key", p.APIKey)
	}
	for k, v := range p.ExtraHdrs {
		req.Header.Set(k, v)
	}

	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 回灌响应头（跳过 hop-by-hop 与长度类，让 Go 自己算）。
	for k, vals := range resp.Header {
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "transfer-encoding", "content-length", "upgrade":
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// 逐块 copy + Flush：SSE 实时性关键。
	buf := make([]byte, 16<<10)
	flusher, _ := w.(http.Flusher)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return nil // 客户端断连：正常结束
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			// ctx 取消（客户端断开）不算错误
			if ctx.Err() != nil {
				return nil
			}
			return rerr
		}
	}
}

// Models 拉取上游模型目录（用于本网关 /v1/models 的合并展示）。
func (p *OpenAICompat) Models(ctx context.Context) ([]string, error) {
	if !p.Ready() {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	resp, err := p.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("上游 /v1/models 状态 %d", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// Health 探活（/health 是 codex2api 的健康端点）。
func (p *OpenAICompat) Health(ctx context.Context) error {
	for _, ep := range []string{"/health", "/healthz"} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+ep, nil)
		if err != nil {
			continue
		}
		resp, err := p.HTTPClient.Do(req)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 400 {
			return nil
		}
	}
	return fmt.Errorf("提供方 %s 健康检查失败", p.Name)
}

// ------------------------------------------------------------------ 路由

// Router 按模型名决定请求走哪个上游。
//
// 规则（可扩展）：
//   - 模型名命中 Codex 家族（gpt-5.x / gpt-6 / codex-* / o1-astra 等）
//     且 codex2api 可用 → 走 codex2api（它管账号池，最省事）
//   - 其余 → 走 prism 私协议通道
//   - 显式前缀 `codex2api/xxx` 或 `prism/xxx` 可强制指定（去掉前缀后转发）
type Router struct {
	Codex2API *OpenAICompat
	// CodexPatterns 是判定为「Codex 家族」的子串（小写匹配）。
	CodexPatterns []string
	// DefaultTarget 是「既不命中 Codex 家族也没写前缀」时的默认去处。
	// 空 = "prism"（保守）。设成 "codex2api" 可让网关变成 codex2api 的
	// 全量前置（此时只有显式 prism/ 前缀的请求才走私协议）。
	DefaultTarget string
}

// NewRouter 建路由器（默认模式集覆盖实测见过的 Codex 模型名）。
func NewRouter(codex2api *OpenAICompat) *Router {
	return &Router{
		Codex2API: codex2api,
		CodexPatterns: []string{
			"gpt-5", "gpt-6", "codex", "astra", "sol", "terra",
			"o1-", "o3-", "o4-",
		},
	}
}

// Decision 是一次路由决策。
type Decision struct {
	Target string // "codex2api" | "prism"
	Model  string // 去掉显式前缀后的模型名
	Reason string
}

// Route 判定模型该走哪条路。
func (rt *Router) Route(model string) Decision {
	m := strings.TrimSpace(model)
	lower := strings.ToLower(m)

	// ① 显式前缀优先（运维可强制指定）。
	if i := strings.Index(lower, "/"); i > 0 {
		prefix := lower[:i]
		rest := m[i+1:]
		switch prefix {
		case "codex2api", "codex":
			if rt.Codex2API.Ready() {
				return Decision{Target: "codex2api", Model: rest, Reason: "显式前缀 codex2api/"}
			}
		case "prism":
			return Decision{Target: "prism", Model: rest, Reason: "显式前缀 prism/"}
		}
	}

	// ② Codex 家族 → codex2api（若可用）。
	if rt.Codex2API.Ready() {
		for _, pat := range rt.CodexPatterns {
			if strings.Contains(lower, pat) {
				return Decision{Target: "codex2api", Model: m, Reason: "模型名命中 Codex 家族 " + pat}
			}
		}
	}

	// ③ 其余走 prism。
	target := "prism"
	reason := "默认走 prism 私协议"
	if strings.EqualFold(rt.DefaultTarget, "codex2api") && rt.Codex2API.Ready() {
		target = "codex2api"
		reason = "默认目标配置为 codex2api"
	}
	return Decision{Target: target, Model: m, Reason: reason}
}
