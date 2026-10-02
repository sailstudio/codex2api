package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"prism-gateway/internal/prism"
	"prism-gateway/internal/stream"
	"prism-gateway/internal/usage"
)

// ============================================================================
// 三个协议 handler。共用「取号 → start →（心跳/pending）→ 合成流」骨架。
// ============================================================================

// serverRun 封装一次完整的网关轮次（含 SSE 合成与计数）。
type serverRun struct {
	server   *Server
	n        *Normalized
	reqID    string
	clientID string
	started  time.Time
	convID   string
}

// prepare 归一化后做共通处理：解析模型、生成会话 id。
func (s *Server) prepare(n *Normalized) *serverRun {
	conv := strings.TrimSpace(n.Conversation)
	if conv == "" {
		conv = prism.NewConversationID()
	}
	return &serverRun{
		server:   s,
		n:        n,
		reqID:    newRequestID(),
		clientID: clientIDFromContext(context.Background()),
		started:  time.Now(),
		convID:   conv,
	}
}

// run 执行一轮，返回结果与用量。
// onPending 在上游 pending 阶段被回调（可做 SSE 保活/进度）。
// onReasoning / onText / onToolCall 用于流式下发（可为 nil，则只聚合）。
func (r *serverRun) run(ctx context.Context,
	onPending func(polls int),
	onReasoning, onText func(string) error,
	onToolCall func(prism.EmulatedCall, int) error) (*prism.TurnResult, usage.Usage, error) {

	s := r.server
	n := r.n
	// 模型与档位解析（含白名单回退）。
	serverModel, effort, clientModel := prism.ResolveModel(s.catalog, n.Model, n.Effort)
	n.Model = clientModel

	// 注入系统指令（网关默认 + 客户端自带）。
	sysPrompt := s.effectiveSystemPrompt(n.SystemPrompt)

	models := modelOrder(s.catalog, serverModel)
	var lastErr error
	for _, mdl := range models {
		var result *prism.TurnResult
		var usedAcct string
		err := s.pool.Do(ctx, 2, func(cctx context.Context, c *prism.Client) error {
			turn := prism.PrepareTurn(s.pool, nil, n.Messages, n.Tools, sysPrompt, mdl, effort, r.convID, s.budget, s.catalog)
			st, err := c.StartTurn(cctx, turn)
			if err != nil {
				return err
			}
			res, err := c.PollTurn(cctx, st, func(polls int, _ json.RawMessage) {
				if onPending != nil {
					onPending(polls)
				}
			})
			if err != nil {
				if cctx.Err() != nil {
					// 客户端断连：取消上游轮次省钱。
					_ = c.StopTurn(context.Background(), st, turn.ConversationID)
				}
				return err
			}
			result, usedAcct = res, c.Account().ID
			return nil
		})
		if err != nil {
			lastErr = err
			if isModelUnsupportedErr(err) {
				continue // 换下一个模型
			}
			return nil, usage.Usage{}, err
		}
		if result == nil {
			return nil, usage.Usage{}, fmt.Errorf("上游未返回结果")
		}
		if result.Err != "" {
			if strings.Contains(strings.ToLower(result.Err), "unsupported assistant model") {
				lastErr = fmt.Errorf("%s", result.Err)
				continue
			}
			return nil, usage.Usage{}, fmt.Errorf("上游轮次失败: %s", trimErr(result.Err))
		}
		_ = usedAcct

		// ---- 文本合成：思考先出、正文再出、工具块即时出 ----
		chunkOpt := stream.ChunkOptions{Size: s.cfg.ChunkSize, Interval: s.cfg.ChunkInterval}
		if onReasoning != nil && s.cfg.EmitReasoning && strings.TrimSpace(result.Reasoning) != "" {
			if !stream.EmitChunks(result.Reasoning, chunkOpt, onReasoning) {
				return result, usage.Usage{}, errClientGone
			}
		}
		// 从正文里抽出仿真工具调用块。
		text := result.Text
		var emulated []prism.EmulatedCall
		if len(n.Tools) > 0 {
			if calls, cleaned := prism.ExtractToolCalls(result.Text); len(calls) > 0 {
				emulated, text = calls, cleaned
			}
		}
		result.Emulated = emulated
		if onText != nil && text != "" {
			if !stream.EmitChunks(text, chunkOpt, onText) {
				return result, usage.Usage{}, errClientGone
			}
		}
		// onToolCall 只下发**客户端需要执行的**仿真调用；
		// 上游沙箱自己执行过的调用由各协议 handler 单独透传（避免重复下发）。
		if onToolCall != nil {
			for i, c := range emulated {
				if err := onToolCall(c, i); err != nil {
					return result, usage.Usage{}, err
				}
			}
		}
		result.Text = text

		// ---- 用量与缓存计数 ----
		u := s.countUsage(r.clientID, n, sysPrompt, result, emulated)
		return result, u, nil
	}
	return nil, usage.Usage{}, lastErr
}

// errClientGone 表示客户端断连（不是错误，只是停止）。
var errClientGone = fmt.Errorf("客户端已断开")

// IsClientGone 判断是否客户端断连。
func IsClientGone(err error) bool { return err == errClientGone }

// countUsage 估算用量并落计数（含前缀缓存拆分）。
func (s *Server) countUsage(clientID string, n *Normalized, sysPrompt string,
	result *prism.TurnResult, emulated []prism.EmulatedCall) usage.Usage {

	// 构造与 BuildInput 同序的块序列，用于前缀指纹。
	blocks := make([]string, 0, len(n.Messages)+4)
	if sp := strings.TrimSpace(sysPrompt); sp != "" {
		blocks = append(blocks, "[系统指令]\n"+sp)
	}
	for _, m := range n.Messages {
		seg := m.Role + ": " + m.Content
		for _, f := range m.Files {
			if f.Text != "" {
				seg += "\n" + f.Text
			} else if len(f.Data) > 0 {
				// 图片按内容摘要参与指纹（同一张图重复出现才算命中），
				// 但 token 按固定开销计，不按 base64 长度。
				seg += "\n[image:" + prism.AttachmentKey(f.Data) + "]"
			}
		}
		for _, tc := range m.ToolCalls {
			seg += "\n[tool_call:" + tc.Name + "]" + tc.Arguments
		}
		blocks = append(blocks, seg)
	}
	if len(n.Tools) > 0 {
		blocks = append(blocks, prism.ToolContract(n.Tools))
	}
	b64 := 0
	for _, m := range n.Messages {
		for _, f := range m.Files {
			if len(f.Data) > 0 && prism.IsImage(f.Mime) {
				b64 += len(f.Data) * 4 / 3
			}
		}
	}
	out := s.est.Text(result.Text) + s.est.Text(result.Reasoning) + s.est.Tools(len(n.Tools), 0)
	plan := s.cache.Plan(s.cfg.CacheNamespace+":"+maskClient(clientID), blocks, out, countImages(n), 0)
	u := *plan.Result()
	// 图片与工具声明的固定开销并入 Input，保证不变量守恒。
	u.InputTokens += s.est.Image(b64) + s.est.TokensPerTool*len(n.Tools)
	plan.Usage = &u
	plan.Commit()
	s.counters.Record(clientID, u, false)
	return u
}

func countImages(n *Normalized) int {
	c := 0
	for _, m := range n.Messages {
		for _, f := range m.Files {
			if len(f.Data) > 0 && prism.IsImage(f.Mime) {
				c++
			}
		}
	}
	return c
}

// ------------------------------------------------------------------ Chat Completions

// handleChat 处理 POST /v1/chat/completions。
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req ChatRequest
	if err := decodeJSON(r, &req, s.cfg.MaxBodyBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}
	n, err := FromChat(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	run := s.prepare(n)

	if !n.Stream {
		s.chatNonStream(w, r, run)
		return
	}
	s.chatStream(w, r, run)
}

func (s *Server) chatNonStream(w http.ResponseWriter, r *http.Request, run *serverRun) {
	ctx := r.Context()
	res, u, err := run.run(ctx, nil, nil, nil, nil)
	if err != nil {
		s.writeUpstreamErr(w, err)
		if !IsClientGone(err) {
			s.counters.Record(run.clientID, usage.Usage{}, true)
		}
		return
	}
	msg := ChatRespMsg{Role: "assistant", Content: res.Text, ReasoningContent: res.Reasoning}
	for i, c := range allToolCalls(res) {
		var tc ChatToolCall
		tc.Index = &i
		tc.ID = c.ID
		tc.Type = "function"
		tc.Function.Name = c.Name
		tc.Function.Arguments = c.Arguments
		msg.ToolCalls = append(msg.ToolCalls, tc)
	}
	writeJSON(w, http.StatusOK, ChatResponse{
		ID: run.reqID, Object: "chat.completion", Created: time.Now().Unix(),
		Model:   run.n.Model,
		Choices: []ChatChoice{{Index: 0, Message: msg, FinishReason: finishReason(msg.ToolCalls)}},
		Usage:   chatUsageFrom(u),
	})
}

func (s *Server) chatStream(w http.ResponseWriter, r *http.Request, run *serverRun) {
	ctx := r.Context()
	sw, err := stream.NewWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	defer sw.Close()
	stopKA := sw.KeepAlive(s.cfg.KeepAliveEvery)
	defer stopKA()
	sw.Comment("prism-gateway")

	base := func() map[string]any {
		return map[string]any{
			"id": run.reqID, "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": run.n.Model,
		}
	}
	sendDelta := func(delta map[string]any, finish *string) error {
		chunk := base()
		chunk["choices"] = []map[string]any{{
			"index": 0, "delta": delta, "finish_reason": finish,
		}}
		return sw.Data(chunk)
	}

	// 首帧：角色声明（客户端据此初始化消息）。
	if err := sendDelta(map[string]any{"role": "assistant", "content": ""}, nil); err != nil {
		return
	}

	var toolIdx int
	res, u, err := run.run(ctx,
		func(polls int) { sw.Comment(fmt.Sprintf("pending poll=%d", polls)) },
		func(delta string) error {
			if s.cfg.EmitReasoning {
				return sendDelta(map[string]any{"reasoning_content": delta}, nil)
			}
			return nil
		},
		func(delta string) error {
			return sendDelta(map[string]any{"content": delta}, nil)
		},
		func(c prism.EmulatedCall, idx int) error {
			return sendDelta(map[string]any{"tool_calls": []map[string]any{{
				"index": idx, "id": c.ID, "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": c.Arguments},
			}}}, nil)
		},
	)
	_ = toolIdx
	if err != nil {
		if !IsClientGone(err) {
			s.counters.Record(run.clientID, u, true)
			_ = sw.Data(map[string]any{"error": ErrorDetail{
				Message: err.Error(), Type: "upstream_error",
			}})
		}
		return
	}
	// 上游沙箱自己执行过的调用：编号接在仿真调用之后，避免 index 冲突。
	if res != nil && len(res.Upstream) > 0 {
		base := len(res.Emulated)
		for i := range res.Upstream {
			if err := sendDelta(map[string]any{"tool_calls": []map[string]any{
				prism.UpstreamCallToOpenAI(res.Upstream[i], base+i),
			}}, nil); err != nil {
				return
			}
		}
	}
	stop := "stop"
	if len(res.Emulated)+len(res.Upstream) > 0 {
		stop = "tool_calls"
	}
	if err := sendDelta(map[string]any{}, &stop); err != nil {
		return
	}
	if run.n.IncludeUsage {
		chunk := base()
		chunk["choices"] = []map[string]any{}
		chunk["usage"] = chatUsageFrom(u)
		_ = sw.Data(chunk)
	}
	_ = sw.RawData("[DONE]")
}

// ------------------------------------------------------------------ Anthropic

// handleMessages 处理 POST /v1/messages。
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	var req AnthropicRequest
	if err := decodeJSON(r, &req, s.cfg.MaxBodyBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "messages 不能为空")
		return
	}
	n, err := FromAnthropic(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	run := s.prepare(n)
	if !n.Stream {
		s.messagesNonStream(w, r, run)
		return
	}
	s.messagesStream(w, r, run)
}

func (s *Server) messagesNonStream(w http.ResponseWriter, r *http.Request, run *serverRun) {
	res, u, err := run.run(r.Context(), nil, nil, nil, nil)
	if err != nil {
		s.writeUpstreamErr(w, err)
		if !IsClientGone(err) {
			s.counters.Record(run.clientID, usage.Usage{}, true)
		}
		return
	}
	content := []map[string]any{}
	if strings.TrimSpace(res.Reasoning) != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": res.Reasoning})
	}
	if res.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": res.Text})
	}
	for _, c := range upstreamAsEmulated(res) {
		content = append(content, map[string]any{
			"type": "tool_use", "id": c.ID, "name": c.Name,
			"input": rawOrEmpty(c.Arguments),
		})
	}
	for _, c := range res.Emulated {
		content = append(content, map[string]any{
			"type": "tool_use", "id": c.ID, "name": c.Name,
			"input": rawOrEmpty(c.Arguments),
		})
	}
	writeJSON(w, http.StatusOK, AnthropicResponse{
		ID: run.reqID, Type: "message", Role: "assistant", Model: run.n.Model,
		Content: content, StopReason: anthropicStopReason(content), StopSequence: nil,
		Usage: anthropicUsageFrom(u),
	})
}

func (s *Server) messagesStream(w http.ResponseWriter, r *http.Request, run *serverRun) {
	ctx := r.Context()
	sw, err := stream.NewWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	defer sw.Close()
	stopKA := sw.KeepAlive(s.cfg.KeepAliveEvery)
	defer stopKA()

	emit := func(name string, payload map[string]any) error {
		payload["type"] = name
		return sw.Event(name, payload)
	}
	// message_start
	if err := emit("message_start", map[string]any{
		"message": map[string]any{
			"id": run.reqID, "type": "message", "role": "assistant",
			"model": run.n.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}); err != nil {
		return
	}
	// 思考块先开（若启用）
	thinkingOpen := false
	if s.cfg.EmitReasoning {
		if err := emit("content_block_start", map[string]any{
			"index": 0, "content_block": map[string]any{"type": "thinking", "thinking": ""},
		}); err != nil {
			return
		}
		thinkingOpen = true
	}
	textIndex := 0
	if thinkingOpen {
		textIndex = 1
	}
	textOpen := false
	toolBase := textIndex + 1

	res, u, err := run.run(ctx,
		func(polls int) { sw.Comment(fmt.Sprintf("pending poll=%d", polls)) },
		func(delta string) error {
			if !thinkingOpen {
				return nil
			}
			return emit("content_block_delta", map[string]any{
				"index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": delta},
			})
		},
		func(delta string) error {
			if !textOpen {
				textOpen = true
				if err := emit("content_block_start", map[string]any{
					"index": textIndex, "content_block": map[string]any{"type": "text", "text": ""},
				}); err != nil {
					return err
				}
			}
			return emit("content_block_delta", map[string]any{
				"index": textIndex, "delta": map[string]any{"type": "text_delta", "text": delta},
			})
		},
		func(c prism.EmulatedCall, idx int) error {
			i := toolBase + idx
			if err := emit("content_block_start", map[string]any{
				"index": i, "content_block": map[string]any{
					"type": "tool_use", "id": c.ID, "name": c.Name, "input": map[string]any{},
				},
			}); err != nil {
				return err
			}
			return emit("content_block_delta", map[string]any{
				"index": i, "delta": map[string]any{
					"type": "input_json_delta", "partial_json": c.Arguments,
				},
			})
		},
	)
	if err != nil {
		if !IsClientGone(err) {
			s.counters.Record(run.clientID, u, true)
			_ = emit("error", map[string]any{"error": map[string]any{
				"type": "upstream_error", "message": err.Error(),
			}})
		}
		return
	}
	// 上游沙箱执行过的工具调用（透传展示）
	extra := len(res.Upstream)
	upstreamBase := toolBase + len(res.Emulated)
	for i, c := range res.Upstream {
		j := upstreamBase + i
		if emit("content_block_start", map[string]any{
			"index": j, "content_block": map[string]any{
				"type": "tool_use", "id": c.ID, "name": c.Name, "input": map[string]any{},
			},
		}) != nil {
			return
		}
		if emit("content_block_delta", map[string]any{
			"index": j, "delta": map[string]any{"type": "input_json_delta", "partial_json": c.Arguments},
		}) != nil {
			return
		}
		if emit("content_block_stop", map[string]any{"index": j}) != nil {
			return
		}
	}
	// 关闭块
	if textOpen {
		_ = emit("content_block_stop", map[string]any{"index": textIndex})
	}
	if thinkingOpen {
		_ = emit("content_block_stop", map[string]any{"index": 0})
	}
	stop := "end_turn"
	if len(res.Upstream) > 0 || extra > 0 || len(res.Emulated) > 0 {
		stop = "tool_use"
	}
	_ = emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": u.OutputTokens},
	})
	_ = emit("message_stop", map[string]any{})
}

// ------------------------------------------------------------------ Responses

// handleResponses 处理 POST /v1/responses。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	var req ResponsesRequest
	if err := decodeJSON(r, &req, s.cfg.MaxBodyBytes); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	n, err := FromResponses(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if len(n.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "input 不能为空")
		return
	}
	run := s.prepare(n)
	if !n.Stream {
		res, u, err := run.run(r.Context(), nil, nil, nil, nil)
		if err != nil {
			s.writeUpstreamErr(w, err)
			if !IsClientGone(err) {
				s.counters.Record(run.clientID, usage.Usage{}, true)
			}
			return
		}
		writeJSON(w, http.StatusOK, responsesPayload(run, res, u))
		return
	}
	s.responsesStream(w, r, run)
}

func (s *Server) responsesStream(w http.ResponseWriter, r *http.Request, run *serverRun) {
	ctx := r.Context()
	sw, err := stream.NewWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	defer sw.Close()
	stopKA := sw.KeepAlive(s.cfg.KeepAliveEvery)
	defer stopKA()

	emit := func(name string, payload map[string]any) error {
		payload["type"] = name
		payload["sequence_number"] = 0
		return sw.Event(name, payload)
	}
	_ = emit("response.created", map[string]any{"response": map[string]any{
		"id": run.reqID, "object": "response", "status": "in_progress", "model": run.n.Model,
	}})

	reasoningIdx := 0
	textIdx := 1
	toolStart := 2
	textOpen := false

	res, u, err := run.run(ctx,
		func(polls int) { sw.Comment(fmt.Sprintf("pending poll=%d", polls)) },
		func(delta string) error {
			if !s.cfg.EmitReasoning {
				return nil
			}
			return emit("response.reasoning_summary_text.delta", map[string]any{
				"item_id": "rs_" + run.reqID, "output_index": reasoningIdx, "delta": delta,
			})
		},
		func(delta string) error {
			if !textOpen {
				textOpen = true
			}
			return emit("response.output_text.delta", map[string]any{
				"item_id": "msg_" + run.reqID, "output_index": textIdx, "delta": delta,
			})
		},
		func(c prism.EmulatedCall, idx int) error {
			return emit("response.output_item.added", map[string]any{
				"output_index": toolStart + idx,
				"item": map[string]any{
					"type": "function_call", "id": c.ID, "call_id": c.ID,
					"name": c.Name, "arguments": "",
				},
			})
		},
	)
	if err != nil {
		if !IsClientGone(err) {
			s.counters.Record(run.clientID, u, true)
			_ = emit("error", map[string]any{"message": err.Error()})
		}
		return
	}
	_ = res
	_ = emit("response.completed", map[string]any{"response": responsesPayload(run, res, u)})
}

// responsesPayload 组装 Responses 结束/非流式的完整对象。
func responsesPayload(run *serverRun, res *prism.TurnResult, u usage.Usage) map[string]any {
	output := []map[string]any{}
	if strings.TrimSpace(res.Reasoning) != "" {
		output = append(output, map[string]any{
			"type": "reasoning", "id": "rs_" + run.reqID,
			"summary": []map[string]any{{"type": "summary_text", "text": res.Reasoning}},
		})
	}
	if res.Text != "" {
		output = append(output, map[string]any{
			"type": "message", "id": "msg_" + run.reqID, "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": res.Text, "annotations": []any{}}},
		})
	}
	for _, c := range res.Upstream {
		output = append(output, map[string]any{
			"type": "function_call", "id": c.ID, "call_id": c.ID,
			"name": c.Name, "arguments": c.Arguments,
		})
	}
	for _, c := range res.Emulated {
		output = append(output, map[string]any{
			"type": "function_call", "id": c.ID, "call_id": c.ID,
			"name": c.Name, "arguments": c.Arguments,
		})
	}
	return map[string]any{
		"id": run.reqID, "object": "response", "created_at": run.started.Unix(),
		"status": "completed", "model": run.n.Model, "output": output,
		"usage": responsesUsageFrom(u),
	}
}

// ------------------------------------------------------------------ 错误与工具

// writeUpstreamErr 把内部错误映射成 HTTP 错误响应（流式场景由调用方处理）。
func (s *Server) writeUpstreamErr(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		return
	case IsClientGone(err):
		return
	case isDegraded(err): // 上游整体劣化：503 + Retry-After，让客户端退避而不是重打
		var de *prism.DegradedError
		if errors.As(err, &de) {
			w.Header().Set("Retry-After", strconv.Itoa(de.RetryAfterSeconds()))
			writeError(w, de.HttpStatus(), "upstream_degraded", de.Error())
			return
		}
	case strings.Contains(err.Error(), "冷却"):
		writeError(w, http.StatusServiceUnavailable, "upstream_unavailable", err.Error())
	case strings.Contains(err.Error(), "预算"):
		writeError(w, http.StatusGatewayTimeout, "timeout", err.Error())
	default:
		writeError(w, http.StatusBadGateway, "upstream_error", trimErr(err.Error()))
	}
}

// allToolCalls 汇总「上游沙箱执行过的调用 + 需客户端执行的仿真调用」。
// 顺序固定：先仿真（客户端要执行的），后上游（仅展示），与流式下发的编号一致。
func allToolCalls(res *prism.TurnResult) []prism.EmulatedCall {
	out := make([]prism.EmulatedCall, 0, len(res.Emulated)+len(res.Upstream))
	out = append(out, res.Emulated...)
	for _, c := range res.Upstream {
		out = append(out, prism.EmulatedCall{Name: c.Name, Arguments: c.Arguments, ID: c.ID})
	}
	return out
}

// upstreamAsEmulated 只取上游沙箱执行过的调用（用于需要区分来源的场景）。
func upstreamAsEmulated(res *prism.TurnResult) []prism.EmulatedCall {
	out := make([]prism.EmulatedCall, 0, len(res.Upstream))
	for _, c := range res.Upstream {
		out = append(out, prism.EmulatedCall{Name: c.Name, Arguments: c.Arguments, ID: c.ID})
	}
	return out
}

func finishReason(calls []ChatToolCall) string {
	if len(calls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func anthropicStopReason(content []map[string]any) string {
	for _, b := range content {
		if t, _ := b["type"].(string); t == "tool_use" {
			return "tool_use"
		}
	}
	return "end_turn"
}

func chatUsageFrom(u usage.Usage) *ChatUsage {
	return &ChatUsage{
		PromptTokens:     u.TotalInput(),
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.Total(),
		PromptTokensDetails: &PromptTokenDetail{
			CachedTokens: u.CacheReadInputTokens,
		},
		CompletionDetails: &CompletionDetail{ReasoningTokens: u.ReasoningTokens},
	}
}

func anthropicUsageFrom(u usage.Usage) AnthropicUsage {
	return AnthropicUsage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
	}
}

func responsesUsageFrom(u usage.Usage) ResponsesUsage {
	return ResponsesUsage{
		InputTokens:         u.TotalInput(),
		OutputTokens:        u.OutputTokens,
		TotalTokens:         u.Total(),
		InputTokensDetails:  &PromptTokenDetail{CachedTokens: u.CacheReadInputTokens},
		OutputTokensDetails: &CompletionDetail{ReasoningTokens: u.ReasoningTokens},
	}
}

func rawOrEmpty(args string) json.RawMessage {
	if strings.TrimSpace(args) == "" {
		return json.RawMessage("{}")
	}
	if !json.Valid([]byte(args)) {
		b, _ := json.Marshal(map[string]any{"input": args})
		return b
	}
	return json.RawMessage(args)
}

// isDegraded 判断是否为「上游整体劣化」熔断错误。
func isDegraded(err error) bool {
	var de *prism.DegradedError
	return errors.As(err, &de)
}

func isModelUnsupportedErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "unsupported assistant model") || strings.Contains(s, "model not found")
}

// modelOrder 生成模型尝试顺序（首选 → 目录其余，用于白名单回退）。
func modelOrder(catalog []prism.ModelSpec, server string) []string {
	out := []string{server}
	for _, mi := range catalog {
		sm := strings.TrimSpace(mi.ServerModelName)
		if sm == "" {
			sm = mi.ID
		}
		if sm != server {
			out = append(out, sm)
		}
	}
	return out
}

func trimErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

func newRequestID() string { return "chatcmpl-" + randHex(24) }
