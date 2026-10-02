package prism

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// VariantArgs 描述一次变体实验的组装方式（用于定位上游 400）。
type VariantArgs struct {
	Prompt     string
	Prompt2    string // 仅用于日志区分
	WithPrefix bool   // 是否带材料的 system 前缀
	OverrideMD bool   // 是否覆盖 model/reasoning_effort
	Extra      map[string]any
}

// RawTurnVariant 按给定组装方式打一次 start，返回原始响应。
func (c *Client) RawTurnVariant(ctx context.Context, a VariantArgs) (string, []string, error) {
	if err := c.WarmSession(ctx); err != nil {
		return "", nil, err
	}
	mat, err := c.material(ctx)
	if err != nil {
		return "", nil, err
	}

	prompt := a.Prompt
	if prompt == "" {
		prompt = "只回复三个字：收到了"
	}
	input := []map[string]any{}
	if a.WithPrefix {
		input = append(input, mat.InputPrefix...)
	}
	input = append(input, map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": prompt}},
	})

	meta := map[string]any{}
	for k, v := range mat.Metadata {
		meta[k] = v
	}
	if a.OverrideMD {
		meta["model"] = c.cfg.Model
		meta["reasoning_effort"] = c.cfg.ReasoningEffort
	}
	for k, v := range a.Extra {
		meta[k] = v
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
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

	var st struct {
		RequestID string         `json:"request_id"`
		TurnState map[string]any `json:"turn_state"`
	}
	_ = json.Unmarshal(raw, &st)
	if st.RequestID == "" || st.TurnState == nil {
		return string(raw), nil, nil
	}

	// 轮询到终态
	var polls []string
	state := st.TurnState
	deadline := time.Now().Add(c.cfg.Timeout)
	for time.Now().Before(deadline) {
		pb, _ := json.Marshal(map[string]any{"request_id": st.RequestID, "turn_state": state})
		s2, err := c.Mint(ctx)
		if err != nil {
			return string(raw), polls, err
		}
		r2, err := c.do(ctx, http.MethodPost, "/api/llm/response_with_tools_status", string(pb), s2, nil)
		if err != nil {
			return string(raw), polls, err
		}
		rb, _ := io.ReadAll(io.LimitReader(r2.Body, 1<<20))
		_ = r2.Body.Close()
		polls = append(polls, string(rb))
		var sr struct {
			Status    string         `json:"status"`
			TurnState map[string]any `json:"turn_state"`
		}
		_ = json.Unmarshal(rb, &sr)
		if sr.TurnState != nil {
			state = sr.TurnState
		}
		if sr.Status == "completed" || sr.Status == "error" || sr.Status == "failed" {
			break
		}
		select {
		case <-ctx.Done():
			return string(raw), polls, ctx.Err()
		case <-time.After(1200 * time.Millisecond):
		}
	}
	return string(raw), polls, nil
}

var _ = fmt.Sprintf
