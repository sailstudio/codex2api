// Package prismchannel adapts Prism's private polling protocol to an OpenAI facade.
package prismchannel

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	PathStart  = "/api/llm/response_with_tools_start"
	PathStatus = "/api/llm/response_with_tools_status"
	PathStop   = "/api/llm/response_with_tools_stop"
)

var ErrBusy = errors.New("Prism admission capacity exhausted")

// Error deliberately omits upstream bodies: those can contain session credentials.
type Error struct {
	Status     int
	Code       string
	RetryAfter string
}

func (e *Error) Error() string { return fmt.Sprintf("Prism %s (HTTP %d)", e.Code, e.Status) }

type Credential struct {
	ID           string `json:"id,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	SessionToken string `json:"session_token"`
	AccessToken  string `json:"access_token"`
}
type Content struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	ImageURL    string `json:"image_url,omitempty"`
	Filename    string `json:"filename,omitempty"`
	ProjectPath string `json:"project_path,omitempty"`
}
type Item struct {
	Type      string    `json:"type"`
	Role      string    `json:"role,omitempty"`
	Content   []Content `json:"content,omitempty"`
	ID        string    `json:"id,omitempty"`
	CallID    string    `json:"call_id,omitempty"`
	Name      string    `json:"name,omitempty"`
	Arguments string    `json:"arguments,omitempty"`
	Output    string    `json:"output,omitempty"`
	Text      string    `json:"text,omitempty"`
}
type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}
type Request struct {
	CreatedAt    int64
	ResponseID   string
	Model        string
	Input        []Item
	Tools        []Tool
	ToolChoice   string
	Parallel     bool
	PreviousID   string
	Instructions string
	Effort       string
	Stream       bool
	IncludeUsage bool
	Store        bool
}
type Usage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	TotalTokens        int64 `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}
type Result struct {
	CreatedAt        int64
	ID               string
	Text             string
	Reasoning        string
	Calls            []Item
	Usage            Usage
	Estimated        bool
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// Event is a monotonic text delta, a progress delta, or a keepalive.
type Event struct {
	Text      string
	Reasoning string
	Keepalive bool
}

func textItem(role, text string) Item {
	return Item{Type: "message", Role: role, Content: []Content{{Type: "input_text", Text: text}}}
}

// EstimateTokens is an explicitly approximate UTF-8 byte estimator, not billing.
func EstimateTokens(b []byte) int64 { return int64((len(b) + 3) / 4) }
