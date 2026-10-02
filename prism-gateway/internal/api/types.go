package api

import (
	"encoding/json"
	"strings"
)

// ============================================================================
// 客户端协议类型（OpenAI Chat Completions / Anthropic Messages / OpenAI Responses）。
// 统一用「宽松解析 + 归一化」策略：content 允许 string 或 block 数组，
// 先把三种协议都归一化成 prism.Message，再交给内部翻译层。
// ============================================================================

// ------------------------------------------------------------------ OpenAI Chat

// ChatRequest 是 POST /v1/chat/completions 的请求。
type ChatRequest struct {
	Model            string          `json:"model"`
	Messages         []ChatMessage   `json:"messages"`
	Tools            json.RawMessage `json:"tools,omitempty"`
	ToolChoice       json.RawMessage `json:"tool_choice,omitempty"`
	Stream           bool            `json:"stream,omitempty"`
	StreamOptions    *StreamOptions  `json:"stream_options,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	MaxTokens        int             `json:"max_tokens,omitempty"`
	MaxCompletion    int             `json:"max_completion_tokens,omitempty"`
	ReasoningEffort  string          `json:"reasoning_effort,omitempty"`
	ParallelToolCall *bool           `json:"parallel_tool_calls,omitempty"`
	User             string          `json:"user,omitempty"`
	Metadata         map[string]any  `json:"metadata,omitempty"`
}

// StreamOptions 控制是否在流里附带 usage。
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// ChatMessage 是 OpenAI 消息（content 宽松类型）。
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  []ChatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	// 兼容部分客户端用的 reasoning_content 回灌。
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ChatToolCall 是 OpenAI 形状的工具调用。
type ChatToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// ChatResponse 是 OpenAI 非流式响应。
type ChatResponse struct {
	ID                string       `json:"id"`
	Object            string       `json:"object"`
	Created           int64        `json:"created"`
	Model             string       `json:"model"`
	Choices           []ChatChoice `json:"choices"`
	Usage             *ChatUsage   `json:"usage,omitempty"`
	SystemFingerprint string       `json:"system_fingerprint,omitempty"`
}

// ChatChoice 是响应里的一个选择。
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatRespMsg `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatRespMsg 是响应消息。
type ChatRespMsg struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
}

// ChatUsage 是 OpenAI usage（含 cached_tokens 明细）。
type ChatUsage struct {
	PromptTokens        int                `json:"prompt_tokens"`
	CompletionTokens    int                `json:"completion_tokens"`
	TotalTokens         int                `json:"total_tokens"`
	PromptTokensDetails *PromptTokenDetail `json:"prompt_tokens_details,omitempty"`
	CompletionDetails   *CompletionDetail  `json:"completion_tokens_details,omitempty"`
}

// PromptTokenDetail 承载缓存读命中。
type PromptTokenDetail struct {
	CachedTokens int `json:"cached_tokens"`
}

// CompletionDetail 承载思考 token。
type CompletionDetail struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// ChatStreamChunk 是流式分片。
type ChatStreamChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []ChatDelta `json:"choices"`
	Usage   *ChatUsage  `json:"usage,omitempty"`
}

// ChatDelta 是流式增量。
type ChatDelta struct {
	Index        int         `json:"index"`
	Delta        ChatRespMsg `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

// ------------------------------------------------------------------ Anthropic

// AnthropicRequest 是 POST /v1/messages 的请求。
type AnthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []AnthropicMessage `json:"messages"`
	Tools         []AnthropicTool    `json:"tools,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	MaxTokens     int                `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Thinking      *AnthropicThinking `json:"thinking,omitempty"`
	Metadata      map[string]any     `json:"metadata,omitempty"`
}

// AnthropicThinking 是扩展思考配置。
type AnthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// AnthropicMessage 是 Anthropic 消息（content 宽松类型）。
type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// AnthropicTool 是 Anthropic 工具声明。
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// AnthropicBlock 是 content 里的一个块（宽松）。
type AnthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *AnthropicSrc   `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
}

// AnthropicSrc 是图片源。
type AnthropicSrc struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// AnthropicUsage 是 Anthropic usage（缓存字段）。
type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	// 明细（部分客户端会读）
	CacheCreation *AnthropicCacheDetail `json:"cache_creation,omitempty"`
}

// AnthropicCacheDetail 是 5m/1h 缓存拆分。
type AnthropicCacheDetail struct {
	Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
}

// AnthropicResponse 是非流式响应。
type AnthropicResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []map[string]any `json:"content"`
	StopReason   string           `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        AnthropicUsage   `json:"usage"`
}

// ------------------------------------------------------------------ Responses

// ResponsesRequest 是 POST /v1/responses 的请求。
type ResponsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       json.RawMessage `json:"instructions,omitempty"`
	Tools              json.RawMessage `json:"tools,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	MaxOutputTokens    int             `json:"max_output_tokens,omitempty"`
	Temperature        *float64        `json:"temperature,omitempty"`
	Reasoning          json.RawMessage `json:"reasoning,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Metadata           map[string]any  `json:"metadata,omitempty"`
}

// ResponsesUsage 是 Responses 形状的 usage（命名与 Chat 不同）。
type ResponsesUsage struct {
	InputTokens         int                `json:"input_tokens"`
	OutputTokens        int                `json:"output_tokens"`
	TotalTokens         int                `json:"total_tokens"`
	InputTokensDetails  *PromptTokenDetail `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *CompletionDetail  `json:"output_tokens_details,omitempty"`
}

// ------------------------------------------------------------------ 通用

// ErrorBody 是统一的错误响应。
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail 是错误明细。
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

// ModelObject 是 /v1/models 列表项（OpenAI 形状）。
type ModelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelsResponse 是 /v1/models 响应。
type ModelsResponse struct {
	Object string        `json:"object"`
	Data   []ModelObject `json:"data"`
}

// ------------------------------------------------------------------ 解析工具

// rawToString 把宽松 content 解成纯文本（string / block 数组 / 单对象都兼容）。
func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	if s == "null" {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) == nil {
		var sb strings.Builder
		for _, b := range blocks {
			switch t, _ := b["type"].(string); t {
			case "text", "input_text", "output_text":
				if v, _ := b["text"].(string); v != "" {
					sb.WriteString(v)
				}
			case "thinking":
				if v, _ := b["thinking"].(string); v != "" {
					sb.WriteString(v)
				}
			}
		}
		return sb.String()
	}
	var one map[string]any
	if json.Unmarshal(raw, &one) == nil {
		if v, _ := one["text"].(string); v != "" {
			return v
		}
	}
	return ""
}

// rawToStringList 把宽松 content 解成多行文本（用于 tool_result 数组）。
func rawToStringList(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if t, _ := b["type"].(string); t == "text" || t == "input_text" {
				if v, _ := b["text"].(string); v != "" {
					parts = append(parts, v)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return rawToString(raw)
}
