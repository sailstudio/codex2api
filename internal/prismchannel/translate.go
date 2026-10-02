package prismchannel

import (
	"encoding/json"
	"strings"
)

type rawRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCallID string          `json:"tool_call_id"`
		ToolCalls  []struct {
			ID       string `json:"id"`
			Type     string `json:"type"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Input         json.RawMessage   `json:"input"`
	Tools         []json.RawMessage `json:"tools"`
	ToolChoice    json.RawMessage   `json:"tool_choice"`
	Parallel      *bool             `json:"parallel_tool_calls"`
	PreviousID    string            `json:"previous_response_id"`
	Instructions  string            `json:"instructions"`
	Stream        bool              `json:"stream"`
	Store         *bool             `json:"store"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Reasoning struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	Effort string `json:"reasoning_effort"`
}

func invalid(code string) error { return &Error{400, code, ""} }

// Decode accepts the shared function tool and image surfaces of Chat/Responses.
// Unsupported native items fail explicitly instead of silently dropping context.
func Decode(body []byte, responses bool) (Request, error) {
	var options map[string]json.RawMessage
	if json.Unmarshal(body, &options) != nil || options == nil {
		return Request{}, invalid("invalid_json")
	}
	for _, name := range []string{"temperature", "top_p", "max_tokens", "max_completion_tokens", "max_output_tokens", "response_format", "text", "stop", "n", "logprobs", "logit_bias", "seed", "presence_penalty", "frequency_penalty"} {
		if v, exists := options[name]; exists && string(v) != "null" {
			return Request{}, invalid("unsupported_generation_option_" + name)
		}
	}
	var raw rawRequest
	if json.Unmarshal(body, &raw) != nil {
		return Request{}, invalid("invalid_json")
	}
	r := Request{Model: raw.Model, PreviousID: raw.PreviousID, Instructions: raw.Instructions, Effort: raw.Effort, Stream: raw.Stream, IncludeUsage: raw.StreamOptions.IncludeUsage, Parallel: true, Store: true, ToolChoice: "auto"}
	if raw.Reasoning.Effort != "" {
		r.Effort = raw.Reasoning.Effort
	}
	if raw.Parallel != nil {
		r.Parallel = *raw.Parallel
	}
	if raw.Store != nil {
		r.Store = *raw.Store
	}
	if raw.Model == "" {
		return r, invalid("model_required")
	}
	if responses && (len(raw.Input) == 0 || string(raw.Input) == "null") {
		return r, invalid("input_required")
	}
	if responses {
		var text string
		if json.Unmarshal(raw.Input, &text) == nil {
			r.Input = []Item{textItem("user", text)}
		} else {
			var items []json.RawMessage
			if json.Unmarshal(raw.Input, &items) != nil {
				return r, invalid("invalid_input")
			}
			for _, b := range items {
				var v struct {
					Type      string          `json:"type"`
					Role      string          `json:"role"`
					Content   json.RawMessage `json:"content"`
					CallID    string          `json:"call_id"`
					Name      string          `json:"name"`
					Arguments string          `json:"arguments"`
					Output    string          `json:"output"`
				}
				if json.Unmarshal(b, &v) != nil {
					return r, invalid("invalid_input_item")
				}
				switch v.Type {
				case "", "message":
					cs, e := decodeContent(v.Content)
					if e != nil {
						return r, e
					}
					if !validRole(v.Role) {
						return r, invalid("invalid_role")
					}
					r.Input = append(r.Input, Item{Type: "message", Role: v.Role, Content: cs})
				case "function_call":
					if v.CallID == "" || v.Name == "" || !json.Valid([]byte(v.Arguments)) {
						return r, invalid("invalid_function_call")
					}
					r.Input = append(r.Input, Item{Type: v.Type, CallID: v.CallID, Name: v.Name, Arguments: v.Arguments})
				case "function_call_output":
					if v.CallID == "" {
						return r, invalid("call_id_required")
					}
					r.Input = append(r.Input, Item{Type: v.Type, CallID: v.CallID, Output: v.Output})
				default:
					return r, invalid("unsupported_input_item")
				}
			}
		}
	} else {
		if len(raw.Messages) == 0 {
			return r, invalid("messages_required")
		}
		for _, v := range raw.Messages {
			if v.Role == "tool" {
				var output string
				if json.Unmarshal(v.Content, &output) != nil || v.ToolCallID == "" {
					return r, invalid("invalid_tool_result")
				}
				r.Input = append(r.Input, Item{Type: "function_call_output", CallID: v.ToolCallID, Output: output})
				continue
			}
			if !validRole(v.Role) {
				return r, invalid("invalid_role")
			}
			if len(v.Content) > 0 && string(v.Content) != "null" {
				cs, e := decodeContent(v.Content)
				if e != nil {
					return r, e
				}
				r.Input = append(r.Input, Item{Type: "message", Role: v.Role, Content: cs})
			}
			for _, tc := range v.ToolCalls {
				if tc.Type != "function" || tc.ID == "" || tc.Function.Name == "" || !json.Valid([]byte(tc.Function.Arguments)) {
					return r, invalid("invalid_function_call")
				}
				r.Input = append(r.Input, Item{Type: "function_call", CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
			}
		}
	}
	if len(r.Input) == 0 && r.PreviousID == "" {
		return r, invalid("input_required")
	}
	names := map[string]bool{}
	for _, b := range raw.Tools {
		var t Tool
		var wrapper struct {
			Type     string `json:"type"`
			Function Tool   `json:"function"`
		}
		if json.Unmarshal(b, &t) != nil {
			return r, invalid("invalid_tool")
		}
		if !responses {
			if json.Unmarshal(b, &wrapper) != nil {
				return r, invalid("invalid_tool")
			}
			t = wrapper.Function
			t.Type = wrapper.Type
		}
		if t.Type != "function" || t.Name == "" || len(t.Name) > 128 || names[t.Name] {
			return r, invalid("unsupported_or_duplicate_tool")
		}
		if t.Strict {
			return r, invalid("unsupported_strict_tool")
		}
		if len(t.Parameters) == 0 {
			t.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		if !validToolParameters(t.Parameters) {
			return r, invalid("invalid_tool_parameters")
		}
		names[t.Name] = true
		r.Tools = append(r.Tools, t)
	}
	if len(raw.ToolChoice) > 0 {
		if json.Unmarshal(raw.ToolChoice, &r.ToolChoice) != nil {
			var v struct {
				Type     string `json:"type"`
				Name     string `json:"name"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(raw.ToolChoice, &v) != nil || v.Type != "function" {
				return r, invalid("invalid_tool_choice")
			}
			r.ToolChoice = v.Name
			if !responses {
				r.ToolChoice = v.Function.Name
			}
			if !names[r.ToolChoice] {
				return r, invalid("unknown_tool_choice")
			}
		}
		if r.ToolChoice != "auto" && r.ToolChoice != "none" && r.ToolChoice != "required" && !names[r.ToolChoice] {
			return r, invalid("invalid_tool_choice")
		}
	}
	if len(r.Tools) == 0 && r.ToolChoice != "auto" && r.ToolChoice != "none" {
		return r, invalid("tool_choice_requires_tools")
	}
	return r, nil
}
func validRole(s string) bool {
	return s == "system" || s == "developer" || s == "user" || s == "assistant"
}
func decodeContent(b json.RawMessage) ([]Content, error) {
	var text string
	if json.Unmarshal(b, &text) == nil {
		return []Content{{Type: "input_text", Text: text}}, nil
	}
	var blocks []json.RawMessage
	if json.Unmarshal(b, &blocks) != nil {
		return nil, invalid("invalid_content")
	}
	out := make([]Content, 0, len(blocks))
	for _, b := range blocks {
		var v struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			ImageURL json.RawMessage `json:"image_url"`
		}
		if json.Unmarshal(b, &v) != nil {
			return nil, invalid("invalid_content_block")
		}
		switch v.Type {
		case "text", "input_text", "output_text":
			out = append(out, Content{Type: "input_text", Text: v.Text})
		case "image_url", "input_image":
			var uri string
			if json.Unmarshal(v.ImageURL, &uri) != nil {
				var obj struct {
					URL string `json:"url"`
				}
				if json.Unmarshal(v.ImageURL, &obj) != nil {
					return nil, invalid("invalid_image_url")
				}
				uri = obj.URL
			}
			if strings.TrimSpace(uri) == "" {
				return nil, invalid("image_url_required")
			}
			out = append(out, Content{Type: "input_image", ImageURL: uri})
		default:
			return nil, invalid("unsupported_content_block")
		}
	}
	return out, nil
}

// Prism's sandbox prompt builder does not faithfully replay an OpenAI message
// array: earlier user/assistant messages and separate text blocks can be lost.
// Supply one system block containing instructions and quoted history, followed
// by one current user block with all its text joined. Keep native files separate.
func prismInput(input []Item) []Item {
	lastUser := -1
	for i, it := range input {
		if it.Role == "user" {
			lastUser = i
		}
	}
	if lastUser < 0 {
		return input
	}
	var system, history []string
	var historicalFiles []Content
	for i, it := range input {
		if i == lastUser {
			continue
		}
		var texts []string
		for _, block := range it.Content {
			if block.Type == "input_file" {
				historicalFiles = append(historicalFiles, block)
				texts = append(texts, "Image attachment: "+block.ProjectPath)
			}
			if block.Text != "" {
				texts = append(texts, block.Text)
			}
		}
		text := strings.Join(texts, "\n\n")
		if it.Role == "system" || it.Role == "developer" {
			system = append(system, text)
		} else {
			history = append(history, "["+it.Role+"]\n"+text)
		}
	}
	if len(history) > 0 {
		system = append(system, "Prior conversation (quoted history):\n"+strings.Join(history, "\n\n"))
	}
	var out []Item
	if len(system) > 0 {
		out = append(out, textItem("system", strings.Join(system, "\n\n")))
	}
	current := input[lastUser]
	var texts []string
	var files []Content
	for _, block := range current.Content {
		if block.Type == "input_text" {
			texts = append(texts, block.Text)
		} else {
			files = append(files, block)
		}
	}
	current.Content = nil
	if len(texts) > 0 {
		current.Content = append(current.Content, Content{Type: "input_text", Text: strings.Join(texts, "\n\n")})
	}
	current.Content = append(current.Content, files...)
	current.Content = append(current.Content, historicalFiles...)
	return append(out, current)
}

// Check declaration shape only. Argument validation against full JSON Schema is
// not supported by this prompt tool bridge; strict declarations are rejected.
func validToolParameters(raw json.RawMessage) bool {
	var schema map[string]json.RawMessage
	if json.Unmarshal(raw, &schema) != nil || schema == nil {
		return false
	}
	var kind string
	if json.Unmarshal(schema["type"], &kind) != nil || kind != "object" {
		return false
	}
	if raw, ok := schema["properties"]; ok {
		var properties map[string]json.RawMessage
		if json.Unmarshal(raw, &properties) != nil || properties == nil {
			return false
		}
		for _, property := range properties {
			var shape map[string]any
			if json.Unmarshal(property, &shape) != nil || shape == nil {
				return false
			}
		}
	}
	if raw, ok := schema["required"]; ok {
		var names []string
		if json.Unmarshal(raw, &names) != nil || names == nil {
			return false
		}
	}
	if raw, ok := schema["additionalProperties"]; ok {
		if string(raw) == "null" {
			return false
		}
		var flag bool
		var shape map[string]any
		if json.Unmarshal(raw, &flag) != nil && (json.Unmarshal(raw, &shape) != nil || shape == nil) {
			return false
		}
	}
	return true
}
