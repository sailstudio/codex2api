package prism

import (
	"encoding/json"
	"strings"
	"time"
)

// ============================================================================
// 上游 prism.openai.com 私协议（全部来自 12 个开源实现的 HAR 抓包 + 真机实测）。
// 本文件是「协议单一事实来源」：字段名、路径、header 名一律不散落在业务代码里。
// ============================================================================

// ---------------------------------------------------------------- 路径常量

const (
	// 对话：start + status 轮询模型（上游没有 SSE）。
	PathStart  = "/api/llm/response_with_tools_start"
	PathStatus = "/api/llm/response_with_tools_status"
	PathStop   = "/api/llm/response_with_tools_stop"

	// 认证与权益。
	PathSession      = "/auth/session"
	PathEntitlements = "/auth/entitlements"

	// 沙箱预热链（缺一步 start 会无限挂起）。
	PathBackendNew     = "/api/backend/1/new"
	PathResourcesToken = "/api/projects/%s/sandbox/resources-token" // %s = projectId
	PathYSweet         = "/api/y"
	PathResourceBase   = "/s/sandbox-resources/"

	// 拼在沙箱基址之后。
	PathSandboxBase    = "/s/sandboxes/proxy"
	PathProxyResources = "/resources-token"
	PathProxyToken     = "/token"
	PathProxyHeartbeat = "/heartbeat"
	PathWaitForSync    = "/wait-for-sync?wait_ms=10000"

	// 项目与会话登记（best-effort）。
	PathProjectList         = "/api/file-management/projects?section=your_projects"
	PathProjects            = "/api/projects"
	PathConversationHistory = "/api/codex/conversation-history"
)

// ---------------------------------------------------------------- header 常量

const (
	// 沙箱面：所有拼在沙箱基址后的请求都要带。
	HeaderSandboxToken = "X-Crixet-Sandbox-Token"
	// 对话面：2026-09-19 起强制校验，缺失或复用一律 403；严格一次性 + 45s 新鲜窗。
	HeaderSentinelToken = "openai-sentinel-token"
	// 可选：项目编辑权限提升。
	HeaderProjectEditAccess = "x-prism-require-project-edit-access"
	HeaderProjectID         = "x-prism-project-id"

	// 浏览器身份（cf_clearance 与出口 IP + UA 绑定，换任一即 403）。
	HeaderUserAgent = "User-Agent"
)

// CookieNames 是必须齐备的 cookie 名；缺 oai-sc 会得到
// "401 Could not parse your authentication token"。
var CookieNames = []string{
	"oai-sc",
	"prism_session_token",
	"prism_oai_access_token",
	"cf_clearance",
	"__cf_bm",
}

// ---------------------------------------------------------------- 请求体

// Metadata 是 start 请求的 metadata。上游把 start 与页面上下文强绑定：
// 自建会话 + 预热沙箱的 metadata 会被判 400，唯一被接受的 body 来自浏览器页面
// （故本网关支持「材料包」模式，见 pool 包）。
type Metadata struct {
	ProjectID           string `json:"projectId,omitempty"`
	UserID              string `json:"userId,omitempty"`
	Model               string `json:"model"`
	ReasoningEffort     string `json:"reasoning_effort,omitempty"`
	FrontendOrigin      string `json:"frontend_origin,omitempty"`
	SandboxURL          string `json:"sandbox_url,omitempty"`
	SandboxToken        string `json:"sandbox_token,omitempty"`
	CodexListenSnapshot string `json:"codex_listen_snapshot,omitempty"`
}

// StartRequest 是 POST /api/llm/response_with_tools_start 的请求体。
// 顶层字段只有 input / metadata / conversationId / tools —— 上游忽略 tools
// （实测回 no_tool_available），工具只能靠提示词 + 文本解析仿真。
type StartRequest struct {
	Input          []InputItem     `json:"input"`
	Metadata       *Metadata       `json:"metadata,omitempty"`
	ConversationID string          `json:"conversationId,omitempty"`
	Tools          json.RawMessage `json:"tools,omitempty"`
}

// InputItem 是 Responses 形状的 input 项。
type InputItem struct {
	Type    string        `json:"type"`
	Role    string        `json:"role,omitempty"`
	Content []ContentPart `json:"content,omitempty"`
}

// ContentPart 是 input 项里的内容块；input_text 用于文本，
// input_image 要求 "valid Prism storage URL"（实测 data:/公网 URL/file_id 全被拒）。
type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	URL  string `json:"image_url,omitempty"`
}

// StatusRequest 是 POST /api/llm/response_with_tools_status 的请求体。
// turn_state 必须逐轮把上一次响应里的值**原样**回传，否则 401。
type StatusRequest struct {
	RequestID string          `json:"request_id"`
	TurnState json.RawMessage `json:"turn_state,omitempty"`
}

// StopRequest 用于客户端断连时取消上游轮次（省钱）。
type StopRequest struct {
	RequestID      string          `json:"request_id"`
	ConversationID string          `json:"conversation_id,omitempty"`
	TurnState      json.RawMessage `json:"turn_state,omitempty"`
}

// ---------------------------------------------------------------- 响应体

// StartResponse 是 start 的响应。两种形态：
//   - started：带 request_id + turn_state，需要继续轮询
//   - completed / error：上游以 200 立刻返回失败（如 500/400 文案），没有 turn_state
type StartResponse struct {
	Status         string          `json:"status"`
	RequestID      string          `json:"request_id"`
	ConversationID string          `json:"conversation_id"`
	TurnState      json.RawMessage `json:"turn_state"`
	Response       *TurnResponse   `json:"response"`
	Message        string          `json:"message"`
	// 沙箱重连中：HTTP 200 但 reason=sandbox_reconnecting、sandbox_token_present=false，
	// 这种状态**永远等不到回答**，必须等 codex ready 后用同一沙箱重试。
	Reason              string `json:"reason"`
	SandboxTokenPresent *bool  `json:"sandbox_token_present"`
}

// StatusResponse 是 status 的响应。
type StatusResponse struct {
	Status    string          `json:"status"` // pending | started | completed | failed | error
	TurnState json.RawMessage `json:"turn_state"`
	Response  *TurnResponse   `json:"response"`
	Message   string          `json:"message"`
}

// TurnResponse 包住 payload。
type TurnResponse struct {
	Status  string       `json:"status"`
	Payload *TurnPayload `json:"payload"`
	Message string       `json:"message"`
}

// TurnPayload 是真正的产物：output 里可能有 message / reasoning /
// function_call 等多种 item。注意：这里的 function_call 是**上游 Codex 沙箱
// 自己执行过的**工具调用，本网关只搬运不重放。
type TurnPayload struct {
	Output []OutputItem `json:"output"`
	Status string       `json:"status"`
}

// OutputItem 是 output 数组里的一项。
type OutputItem struct {
	Type      string        `json:"type"` // message | reasoning | function_call | custom_tool_call | tool_call
	Role      string        `json:"role,omitempty"`
	Content   []ContentPart `json:"content,omitempty"`
	Summary   []SummaryPart `json:"summary,omitempty"` // type==reasoning 时
	Name      string        `json:"name,omitempty"`
	CallID    string        `json:"call_id,omitempty"`
	ID        string        `json:"id,omitempty"`
	Arguments string        `json:"arguments,omitempty"`
}

// SummaryPart 是 reasoning item 的 summary 块。
type SummaryPart struct {
	Type string `json:"type"` // summary_text
	Text string `json:"text"`
}

// Text 汇总 output 里所有 message 项的正文。
func (p *TurnPayload) Text() string {
	if p == nil {
		return ""
	}
	var sb strings.Builder
	for _, it := range p.Output {
		if it.Type != "" && it.Type != "message" {
			continue
		}
		for _, c := range it.Content {
			switch c.Type {
			case "output_text", "text":
				sb.WriteString(c.Text)
			}
		}
	}
	return sb.String()
}

// Reasoning 汇总 reasoning item 的思考摘要（多段用换行拼接）。
func (p *TurnPayload) Reasoning() string {
	if p == nil {
		return ""
	}
	var sb strings.Builder
	for _, it := range p.Output {
		if it.Type != "reasoning" {
			continue
		}
		for _, s := range it.Summary {
			if s.Type != "summary_text" || s.Text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(s.Text)
		}
	}
	return sb.String()
}

// UpstreamCalls 汇总 output 里由上游沙箱执行过的工具调用。
func (p *TurnPayload) UpstreamCalls() []UpstreamCall {
	if p == nil {
		return nil
	}
	var out []UpstreamCall
	for _, it := range p.Output {
		switch it.Type {
		case "function_call", "custom_tool_call", "tool_call":
		default:
			continue
		}
		if it.Name == "" {
			continue
		}
		id := it.CallID
		if id == "" {
			id = it.ID
		}
		args := strings.TrimSpace(it.Arguments)
		if args == "" {
			args = "{}"
		}
		out = append(out, UpstreamCall{Name: it.Name, ID: id, Arguments: args, Kind: it.Type})
	}
	return out
}

// UpstreamCall 是上游沙箱执行过的工具调用（仅用于透传展示）。
type UpstreamCall struct {
	Name      string
	ID        string
	Arguments string
	Kind      string
}

// ItemTypes 汇总 output 里 item 类型计数，用于诊断上游行为。
func (p *TurnPayload) ItemTypes() map[string]int {
	out := map[string]int{}
	if p == nil {
		return out
	}
	for _, it := range p.Output {
		t := it.Type
		if t == "" {
			t = "?"
		}
		out[t]++
	}
	return out
}

// ---------------------------------------------------------------- 沙箱

// BackendNewResponse 是 POST /api/backend/1/new 的响应。
// 正常 1.4~2.5s，劣化期 24~210s（此时重铸沙箱纯属给过载上游补刀）。
type BackendNewResponse struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// ResourcesTokenResponse 是项目级 → 沙箱级资源令牌的响应。
type ResourcesTokenResponse struct {
	AccessToken string `json:"access_token"`
}

// WaitForSyncResponse 是 wait-for-sync 的响应。沙箱卡在 syncing 时会把缺什么全列出来
// （hasResourceToken=false 就是没做 Y-Sweet 令牌交棒）。
type WaitForSyncResponse struct {
	Status                string          `json:"status"` // syncing | synced
	ReadinessCapabilities []string        `json:"readinessCapabilities"`
	Tokens                map[string]bool `json:"tokens"`
}

// Ready 判断沙箱是否已同步完成。
func (w *WaitForSyncResponse) Ready() bool { return w != nil && w.Status == "synced" }

// ---------------------------------------------------------------- 会话

// SessionResponse 是 GET /auth/session 的响应（身份信息）。
type SessionResponse struct {
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

// ConversationRegistration 是会话登记请求体（best-effort）。
type ConversationRegistration struct {
	ConversationID string `json:"conversationId"`
	Order          string `json:"order"`
	Limit          int    `json:"limit"`
	UserID         string `json:"userId"`
	ProjectID      string `json:"projectId"`
}

// ---------------------------------------------------------------- 模型

// ModelSpec 是上游白名单里的一个模型。
// 上游只认完整 ID：'gpt-5.6-sol' 通过，别名与带思考后缀的名字（gpt-5.6-sol-high）
// 会被判 400。模型白名单随时会变（2026-09-17 曾白天在下架 astra）。
type ModelSpec struct {
	ID              string   `json:"id"`                // 对客户端暴露的名字
	ServerModelName string   `json:"server_model_name"` // 发给上游的完整 ID
	Aliases         []string `json:"aliases,omitempty"`
	Efforts         []string `json:"efforts,omitempty"` // 支持的思考档位
}

// DefaultModels 是内置目录（含实测见过的名字）。上游没有列表接口，
// 目录只能静态维护；返回 400 Unsupported assistant model 时自动回退到下一个。
func DefaultModels() []ModelSpec {
	return []ModelSpec{
		{ID: "gpt-6-astra", ServerModelName: "gpt-6-astra", Aliases: []string{"astra", "prism-astra"},
			Efforts: []string{"low", "medium", "high", "xhigh"}},
		{ID: "gpt-5.6-sol", ServerModelName: "gpt-5.6-sol", Aliases: []string{"sol", "prism-sol"},
			Efforts: []string{"low", "medium", "high", "xhigh"}},
		{ID: "gpt-5.6-terra", ServerModelName: "gpt-5.6-terra", Aliases: []string{"terra", "prism-terra"},
			Efforts: []string{"low", "medium", "high", "xhigh"}},
	}
}

// ParseModelID 把 "gpt-5.6-sol-high" 拆成 ("gpt-5.6-sol","high")。
// 支持 -low/-medium/-high/-xhigh/-max 后缀，也可用 ":effort" 形式。
func ParseModelID(model string) (base, effort string) {
	m := strings.TrimSpace(model)
	if m == "" {
		return "", ""
	}
	if i := strings.LastIndex(m, ":"); i > 0 {
		if lv := normalizeEffort(m[i+1:]); lv != "" {
			return m[:i], lv
		}
	}
	for _, sfx := range []string{"-xhigh", "-medium", "-high", "-minimal", "-low", "-max"} {
		if strings.HasSuffix(strings.ToLower(m), sfx) {
			return m[:len(m)-len(sfx)], normalizeEffort(strings.TrimPrefix(sfx, "-"))
		}
	}
	return m, ""
}

// normalizeEffort 归一思考档位到上游认的四个值（xhigh 是实验档，max 暂归一到 xhigh）。
func normalizeEffort(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "low", "minimal":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "max":
		return "xhigh"
	case "none", "off":
		return ""
	}
	return ""
}

// Sandbox 是一个已就绪（或正在建）的沙箱。
type Sandbox struct {
	URL       string // 沙箱基址，通常 = origin + PathSandboxBase
	Token     string // X-Crixet-Sandbox-Token
	AccountID string // 归属账号
	ProjectID string // 建沙箱时用的项目
	CreatedAt time.Time
	LastUsed  time.Time
}
