package prism

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// NewDebugClient 与 NewClient 等价（暴露给调试 CLI 用）。
func NewDebugClient(cfg Config, ck Cookie) *Client { return NewClient(cfg, ck) }

// RawTurn 执行一轮并返回 start / 每次 status 的**原始 JSON 字符串**，
// 便于定位「正文为空」这类问题（不做任何解析过滤）。
func (c *Client) RawTurn(ctx context.Context, prompt, model, effort string) (string, []string, error) {
	if err := c.WarmSession(ctx); err != nil {
		return "", nil, err
	}
	mat, err := c.material(ctx)
	if err != nil {
		return "", nil, err
	}
	if model == "" {
		model = strings.TrimSpace(mat.Model) // 沿用材料
	}
	if model == "" {
		model = c.cfg.Model
	}
	if effort == "" {
		effort = firstNonBlank(strOf(mat.Metadata["reasoning_effort"]), c.cfg.ReasoningEffort)
	}
	input := make([]map[string]any, 0, len(mat.InputPrefix)+1)
	input = append(input, mat.InputPrefix...)
	input = append(input, map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": prompt}},
	})
	meta := map[string]any{}
	for k, v := range mat.Metadata {
		meta[k] = v
	}
	meta["model"] = model
	meta["reasoning_effort"] = effort
	meta["projectId"] = mat.ProjectID
	if mat.UserID != "" {
		meta["userId"] = mat.UserID
	} else {
		meta["userId"] = c.UserID()
	}
	body, _ := json.Marshal(map[string]any{
		"input": input, "metadata": meta, "conversationId": "cdx1_" + newUUID4(),
	})

	s, err := c.Mint(ctx)
	if err != nil {
		return "", nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/llm/response_with_tools_start", string(body), s, nil)
	if err != nil {
		return "", nil, err
	}
	rawStart, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

	var start struct {
		Status    string         `json:"status"`
		RequestID string         `json:"request_id"`
		TurnState map[string]any `json:"turn_state"`
	}
	_ = json.Unmarshal(rawStart, &start)
	if start.RequestID == "" || start.TurnState == nil {
		return string(rawStart), nil, nil
	}

	var polls []string
	state := start.TurnState
	deadline := time.Now().Add(c.cfg.Timeout)
	for time.Now().Before(deadline) {
		pb, _ := json.Marshal(map[string]any{"request_id": start.RequestID, "turn_state": state})
		s2, err := c.Mint(ctx)
		if err != nil {
			return string(rawStart), polls, err
		}
		r2, err := c.do(ctx, http.MethodPost, "/api/llm/response_with_tools_status", string(pb), s2, nil)
		if err != nil {
			return string(rawStart), polls, err
		}
		rb, _ := io.ReadAll(io.LimitReader(r2.Body, 1<<20))
		_ = r2.Body.Close()
		polls = append(polls, string(rb))

		var st struct {
			Status    string         `json:"status"`
			TurnState map[string]any `json:"turn_state"`
		}
		_ = json.Unmarshal(rb, &st)
		if st.TurnState != nil {
			state = st.TurnState
		}
		if st.Status == "completed" || st.Status == "error" || st.Status == "failed" {
			break
		}
		select {
		case <-ctx.Done():
			return string(rawStart), polls, ctx.Err()
		case <-time.After(1200 * time.Millisecond):
		}
	}
	return string(rawStart), polls, nil
}

// RawTurnWithInput 用给定的 input 条目打一轮（复用材料 metadata），
// 用于探测上游对各种 content 形态（尤其图片）的接受度。
func (c *Client) RawTurnWithInput(ctx context.Context, input []map[string]any) (string, []string, error) {
	if err := c.WarmSession(ctx); err != nil {
		return "", nil, err
	}
	mat, err := c.material(ctx)
	if err != nil {
		return "", nil, err
	}
	full := make([]map[string]any, 0, len(mat.InputPrefix)+len(input))
	full = append(full, mat.InputPrefix...)
	full = append(full, input...)

	meta := map[string]any{}
	for k, v := range mat.Metadata {
		meta[k] = v
	}
	meta["projectId"] = mat.ProjectID
	if mat.UserID != "" {
		meta["userId"] = mat.UserID
	} else {
		meta["userId"] = c.UserID()
	}

	body, _ := json.Marshal(map[string]any{
		"input": full, "metadata": meta, "conversationId": "cdx1_" + newUUID4(),
	})
	s, err := c.Mint(ctx)
	if err != nil {
		return "", nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/llm/response_with_tools_start", string(body), s, nil)
	if err != nil {
		return "", nil, err
	}
	rawStart, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

	var start struct {
		RequestID string         `json:"request_id"`
		TurnState map[string]any `json:"turn_state"`
	}
	_ = json.Unmarshal(rawStart, &start)
	if start.RequestID == "" || start.TurnState == nil {
		return string(rawStart), nil, nil
	}

	var polls []string
	state := start.TurnState
	deadline := time.Now().Add(c.cfg.Timeout)
	for time.Now().Before(deadline) {
		pb, _ := json.Marshal(map[string]any{"request_id": start.RequestID, "turn_state": state})
		s2, err := c.Mint(ctx)
		if err != nil {
			return string(rawStart), polls, err
		}
		r2, err := c.do(ctx, http.MethodPost, "/api/llm/response_with_tools_status", string(pb), s2, nil)
		if err != nil {
			return string(rawStart), polls, err
		}
		rb, _ := io.ReadAll(io.LimitReader(r2.Body, 1<<20))
		_ = r2.Body.Close()
		polls = append(polls, string(rb))
		var st struct {
			Status    string         `json:"status"`
			TurnState map[string]any `json:"turn_state"`
		}
		_ = json.Unmarshal(rb, &st)
		if st.TurnState != nil {
			state = st.TurnState
		}
		if st.Status == "completed" || st.Status == "error" || st.Status == "failed" {
			break
		}
		select {
		case <-ctx.Done():
			return string(rawStart), polls, ctx.Err()
		case <-time.After(1200 * time.Millisecond):
		}
	}
	return string(rawStart), polls, nil
}

var _ = fmt.Sprintf
var _ = strings.TrimSpace
