package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"prism-gateway/internal/prism"
)

// ============================================================================
// 三种客户端协议 → 内部归一化消息。
// 这一层是「百家之长」的整合点：把 OpenAI / Anthropic / Responses 的差异
// （content 形状、tool call 表示、图片编码、system 位置）全部抹平，
// 下游只需处理一种形状。
// ============================================================================

// Normalized 是一次请求归一化后的结果。
type Normalized struct {
	Messages     []prism.Message
	Tools        []prism.ToolSpec
	SystemPrompt string // 客户端自带 system（会与网关注入指令叠加）
	Stream       bool
	Model        string
	Effort       string
	IncludeUsage bool
	MaxTokens    int
	Conversation string // 上游会话 id（客户端可带，用于多轮续接）
}

// ------------------------------------------------------------------ OpenAI

// FromChat 把 Chat Completions 请求归一化。
func FromChat(r *ChatRequest) (*Normalized, error) {
	n := &Normalized{
		Stream:    r.Stream,
		Model:     r.Model,
		Effort:    r.ReasoningEffort,
		MaxTokens: firstPositive(r.MaxCompletion, r.MaxTokens),
	}
	if r.StreamOptions != nil {
		n.IncludeUsage = r.StreamOptions.IncludeUsage
	}
	tools := prism.ToolSpecsFromRaw(r.Tools)
	n.Tools = tools

	for _, m := range r.Messages {
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			if t := rawToString(m.Content); t != "" {
				if n.SystemPrompt != "" {
					n.SystemPrompt += "\n\n"
				}
				n.SystemPrompt += t
			}
		case "assistant":
			msg := prism.Message{Role: prism.RoleAssistant, Content: rawToString(m.Content)}
			for _, tc := range m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, prism.ToolCallRef{
					ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
				})
			}
			n.Messages = append(n.Messages, msg)
		case "tool", "function":
			n.Messages = append(n.Messages, prism.Message{
				Role: prism.RoleTool, Content: rawToStringList(m.Content),
				ToolCallID: m.ToolCallID, Name: m.Name,
			})
		default: // user
			content, files := parseChatContent(m.Content)
			n.Messages = append(n.Messages, prism.Message{Role: prism.RoleUser, Content: content, Files: files})
		}
	}
	return n, nil
}

// parseChatContent 解析 OpenAI user content（string 或 blocks，含 image_url）。
func parseChatContent(raw json.RawMessage) (string, []prism.Attachment) {
	if len(raw) == 0 {
		return "", nil
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str, nil
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return rawToString(raw), nil
	}
	var text strings.Builder
	var files []prism.Attachment
	for _, b := range blocks {
		switch t, _ := b["type"].(string); t {
		case "text", "input_text":
			if v, _ := b["text"].(string); v != "" {
				text.WriteString(v)
			}
		case "image_url", "input_image":
			url := ""
			if m, ok := b["image_url"].(map[string]any); ok {
				url, _ = m["url"].(string)
			}
			if url == "" {
				url, _ = b["image_url"].(string)
			}
			if url == "" {
				url, _ = b["url"].(string)
			}
			if f, ok := attachmentFromURL(url); ok {
				files = append(files, f)
			}
		case "file", "input_file":
			if f, ok := attachmentFromFileBlock(b); ok {
				files = append(files, f)
			}
		}
	}
	return text.String(), files
}

// ------------------------------------------------------------------ Anthropic

// FromAnthropic 把 Anthropic Messages 请求归一化。
func FromAnthropic(r *AnthropicRequest) (*Normalized, error) {
	n := &Normalized{
		Stream:    r.Stream,
		Model:     r.Model,
		MaxTokens: r.MaxTokens,
	}
	// system 可能是 string 或 blocks 数组。
	n.SystemPrompt = rawToString(r.System)

	for _, t := range r.Tools {
		n.Tools = append(n.Tools, prism.ToolSpec{
			Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
		})
	}
	// 扩展思考 → 上游 reasoning_effort（budget 大小映射档位）。
	if r.Thinking != nil && strings.EqualFold(r.Thinking.Type, "enabled") {
		switch {
		case r.Thinking.BudgetTokens >= 16000:
			n.Effort = "xhigh"
		case r.Thinking.BudgetTokens >= 8000:
			n.Effort = "high"
		case r.Thinking.BudgetTokens >= 2000:
			n.Effort = "medium"
		default:
			n.Effort = "low"
		}
	}

	for _, m := range r.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		blocks := parseAnthropicBlocks(m.Content)
		switch role {
		case "assistant":
			msg := prism.Message{Role: prism.RoleAssistant}
			var texts []string
			for _, b := range blocks {
				switch b.Type {
				case "text":
					texts = append(texts, b.Text)
				case "tool_use":
					args := "{}"
					if len(b.Input) > 0 {
						args = string(b.Input)
					}
					msg.ToolCalls = append(msg.ToolCalls, prism.ToolCallRef{ID: b.ID, Name: b.Name, Arguments: args})
				}
			}
			msg.Content = strings.Join(texts, "\n")
			n.Messages = append(n.Messages, msg)
		default: // user
			// user 消息里可能含 tool_result 块（Anthropic 把工具结果放 user 里）。
			var texts []string
			var files []prism.Attachment
			for _, b := range blocks {
				switch b.Type {
				case "text":
					texts = append(texts, b.Text)
				case "tool_result":
					n.Messages = append(n.Messages, prism.Message{
						Role: prism.RoleTool, Content: rawToStringList(b.Content),
						ToolCallID: b.ToolUseID,
					})
				case "image":
					if f, ok := attachmentFromAnthropicSrc(b.Source); ok {
						files = append(files, f)
					}
				case "document":
					if f, ok := attachmentFromAnthropicSrc(b.Source); ok {
						files = append(files, f)
					}
				}
			}
			if len(texts) > 0 || len(files) > 0 {
				n.Messages = append(n.Messages, prism.Message{
					Role: prism.RoleUser, Content: strings.Join(texts, "\n"), Files: files,
				})
			}
		}
	}
	return n, nil
}

// parseAnthropicBlocks 解析 Anthropic content（string 或 block 数组）。
func parseAnthropicBlocks(raw json.RawMessage) []AnthropicBlock {
	if len(raw) == 0 {
		return nil
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return []AnthropicBlock{{Type: "text", Text: str}}
	}
	var blocks []AnthropicBlock
	if json.Unmarshal(raw, &blocks) == nil {
		return blocks
	}
	return nil
}

// attachmentFromAnthropicSrc 把 Anthropic 的 source 转成附件。
func attachmentFromAnthropicSrc(src *AnthropicSrc) (prism.Attachment, bool) {
	if src == nil {
		return prism.Attachment{}, false
	}
	switch strings.ToLower(src.Type) {
	case "base64":
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(src.Data))
		if err != nil {
			return prism.Attachment{}, false
		}
		return prism.Attachment{Mime: src.MediaType, Data: data}, true
	case "url":
		return attachmentFromURL(src.URL)
	default:
		return prism.Attachment{}, false
	}
}

// ------------------------------------------------------------------ Responses

// FromResponses 把 Responses 请求归一化。input 可能是 string、
// 消息数组，或带 function_call/function_call_output 的项数组。
func FromResponses(r *ResponsesRequest) (*Normalized, error) {
	n := &Normalized{
		Stream:    r.Stream,
		Model:     r.Model,
		MaxTokens: r.MaxOutputTokens,
	}
	n.SystemPrompt = rawToString(r.Instructions)
	n.Tools = prism.ToolSpecsFromRaw(r.Tools)
	if len(r.Reasoning) > 0 {
		var rr struct {
			Effort string `json:"effort"`
		}
		if json.Unmarshal(r.Reasoning, &rr) == nil && rr.Effort != "" {
			n.Effort = rr.Effort
		}
	}
	if s := decodeInputItems(r.Input, n); s != "" {
		n.Messages = append(n.Messages, prism.Message{Role: prism.RoleUser, Content: s})
	}
	return n, nil
}

// decodeInputItems 解析 Responses 的 input（返回尾部的纯文本 user 内容）。
func decodeInputItems(raw json.RawMessage, n *Normalized) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var items []map[string]any
	if json.Unmarshal(raw, &items) != nil {
		return rawToString(raw)
	}
	var tail strings.Builder
	for _, it := range items {
		typ, _ := it["type"].(string)
		role, _ := it["role"].(string)
		switch typ {
		case "message", "":
			content := anyToRaw(it["content"])
			text, files := parseChatContent(content)
			switch strings.ToLower(role) {
			case "assistant":
				n.Messages = append(n.Messages, prism.Message{Role: prism.RoleAssistant, Content: text})
			case "system", "developer":
				if text != "" {
					if n.SystemPrompt != "" {
						n.SystemPrompt += "\n\n"
					}
					n.SystemPrompt += text
				}
			default:
				n.Messages = append(n.Messages, prism.Message{Role: prism.RoleUser, Content: text, Files: files})
				tail.Reset()
				tail.WriteString(text)
			}
		case "function_call", "custom_tool_call":
			name, _ := it["name"].(string)
			args, _ := it["arguments"].(string)
			id := firstNonEmpty(asString(it["call_id"]), asString(it["id"]))
			// 挂到最近一条 assistant 消息上；没有就新建一条。
			if len(n.Messages) > 0 && n.Messages[len(n.Messages)-1].Role == prism.RoleAssistant {
				last := &n.Messages[len(n.Messages)-1]
				last.ToolCalls = append(last.ToolCalls, prism.ToolCallRef{ID: id, Name: name, Arguments: args})
			} else {
				n.Messages = append(n.Messages, prism.Message{
					Role: prism.RoleAssistant, Content: "",
					ToolCalls: []prism.ToolCallRef{{ID: id, Name: name, Arguments: args}},
				})
			}
		case "function_call_output", "custom_tool_call_output":
			out := anyToRaw(it["output"])
			n.Messages = append(n.Messages, prism.Message{
				Role: prism.RoleTool, Content: rawToString(out),
				ToolCallID: firstNonEmpty(asString(it["call_id"]), asString(it["id"])),
			})
		case "input_text", "text":
			if v, _ := it["text"].(string); v != "" {
				tail.WriteString(v)
			}
		case "input_image", "image":
			if f, ok := attachmentFromURL(firstNonEmpty(asString(it["image_url"]), asString(it["url"]))); ok {
				n.Messages = append(n.Messages, prism.Message{Role: prism.RoleUser, Files: []prism.Attachment{f}})
			}
		}
	}
	return strings.TrimSpace(tail.String())
}

// ------------------------------------------------------------------ 附件

// attachmentFromURL 解析 data: URL、http(s) URL，或裸 base64。
// 上游不接受任何 URL/二进制入参，所以这里统一取到字节，走「base64 + 还原命令」通道。
func attachmentFromURL(u string) (prism.Attachment, bool) {
	u = strings.TrimSpace(u)
	if u == "" {
		return prism.Attachment{}, false
	}
	if strings.HasPrefix(u, "data:") {
		// data:<mime>;base64,<payload>
		rest := strings.TrimPrefix(u, "data:")
		i := strings.Index(rest, ",")
		if i < 0 {
			return prism.Attachment{}, false
		}
		meta, payload := rest[:i], rest[i+1:]
		mime := strings.TrimSuffix(strings.Split(meta, ";")[0], ";")
		if !strings.Contains(meta, "base64") {
			return prism.Attachment{}, false
		}
		data, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			return prism.Attachment{}, false
		}
		if mime == "" {
			mime = http.DetectContentType(data)
		}
		return prism.Attachment{Mime: mime, Data: data}, true
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		// 本网关不代为抓取外网（避免 SSRF 与延迟不可控），只记录为标记。
		// 如需支持可在此实现受限下载；默认让模型向用户索要文件。
		return prism.Attachment{
			Name: "remote-file",
			Mime: guessMimeFromURL(u),
			Text: fmt.Sprintf("（客户端提供的远程附件地址：%s，本网关未代为下载；如需内容请让用户把文件直接附加到对话里。）", u),
		}, true
	}
	// 裸 base64（部分客户端只给 data 字段）。
	if len(u) > 64 && !strings.ContainsAny(u, " :/") {
		if data, err := base64.StdEncoding.DecodeString(u); err == nil {
			return prism.Attachment{Mime: http.DetectContentType(data), Data: data}, true
		}
	}
	return prism.Attachment{}, false
}

// attachmentFromFileBlock 解析 Responses 的 file/input_file 块。
func attachmentFromFileBlock(b map[string]any) (prism.Attachment, bool) {
	name := firstNonEmpty(asString(b["filename"]), asString(b["name"]))
	fileData := firstNonEmpty(asString(b["file_data"]), asString(b["data"]))
	if fileData == "" {
		return prism.Attachment{}, false
	}
	att, ok := attachmentFromURL(fileData)
	if !ok {
		return prism.Attachment{}, false
	}
	if name != "" {
		att.Name = name
	}
	if att.Mime == "" || att.Mime == "application/octet-stream" {
		if m := guessMimeFromURL(name); m != "" {
			att.Mime = m
		}
	}
	return att, true
}

func guessMimeFromURL(u string) string {
	l := strings.ToLower(u)
	for _, ext := range []struct{ suf, mime string }{
		{".png", "image/png"}, {".jpg", "image/jpeg"}, {".jpeg", "image/jpeg"},
		{".webp", "image/webp"}, {".gif", "image/gif"}, {".bmp", "image/bmp"},
		{".pdf", "application/pdf"}, {".txt", "text/plain"}, {".md", "text/markdown"},
		{".csv", "text/csv"}, {".json", "application/json"},
	} {
		if strings.Contains(l, ext.suf) {
			return ext.mime
		}
	}
	return ""
}

// ------------------------------------------------------------------ 小工具

// anyToRaw 把 any 重新编码成 json.RawMessage（宽松解析用）。
func anyToRaw(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}
