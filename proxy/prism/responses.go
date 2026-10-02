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

// ============================================================
// Prism 私协议 → OpenAI Responses SSE 适配器。
//
// 这是让「下游零改动」成立的核心：codex2api 的下游（/v1/responses、
// /v1/chat/completions、/v1/messages）统一消费 Responses SSE 事件，
// 因此本包只需把 Prism 的 start/poll 结果翻译成同一套事件即可。
//
// 上游事实（实测）：Prism **没有真流式** —— start 立刻返回 turn_state，
// 之后靠 status 轮询；整轮完成后 payload 里才出现完整 output。
// 所以本适配器的策略是：
//   - 拿到 completed 后，一次性发正文（不伪造 sleep 分块，避免拖慢长答案）
//   - reasoning 摘要若存在，作为 reasoning 事件先发（这是真增量）
//   - 保留 usage（含 reasoning_tokens）
// ============================================================

// Request 是一次 Prism 对话请求（已从 Responses 体转换而来）。
type Request struct {
	Input            []map[string]any // 已折叠好的 input 条目
	ConversationID   string           // 空则自动生成 cdx1_<uuid>
	Model            string
	ReasoningEffort  string
	Tools            []map[string]any // 客户端工具（上游无客户端 tool 通道，走提示词仿真）
	InstructionsText string
	// Images 是本次请求携带的图片。
	//
	// ⚠️ 上游限制（实测）：Prism 的 /api/llm 通道**不消费** data URL 图片——
	// 传 input_image + image_url(data:...) 会 200 但模型看不见（对黑白图仍
	// 答"蓝色"）。真机上传走的是项目文件通道（资源令牌 + 文件引用），
	// 那条链路尚未接入。因此本层采取**诚实降级**：不伪造能力，
	// 而是把图片数量以系统提示告知模型，让它可以明确说明"看不到图片"，
	// 避免它对着不存在的图像胡编。
	Images []ImagePart
}

// ImagePart 是请求里的一张图片。
type ImagePart struct {
	Kind      string // data_url | url | file_id | unknown
	Value     string // data URL 或 http URL 或 file id
	MediaType string // 从 data URL 解析出的 MIME（如 image/png）
	Detail    string // low/high/auto
}

// Turn 是一轮对话的结果。
type Turn struct {
	RequestID  string
	Text       string
	Reasoning  string
	ToolCalls  []ToolCall
	Usage      Usage
	Status     string
	ErrMessage string
	// RawFinal 是终态 status 响应的原文（排查「正文为空」用）。
	RawFinal string
}

// finishTurn 统一「一轮结果」的收尾语义。
//
// ⚠️ 存在的理由：上游会把错误藏在 **HTTP 200 的内层 payload** 里
// （`response.status=error` + `payload.message`），此时 `err == nil` 但
// `turn.ErrMessage != ""`。若各调用路径各写一套判断，极易出现
// **「把上游报错当成成功返回」** 且 **熔断不触发** 的静默缺陷
// （单槽路径就曾如此）。所以两条路径都必须过这里。
//
// 返回：成功 (turn, nil)；失败 (turn, err)，并顺带触发账号级熔断/复位。
func (c *Client) finishTurn(turn *Turn, err error) (*Turn, error) {
	if err == nil && (turn == nil || turn.ErrMessage == "") {
		c.noteUpstreamSuccess() // 成功 → 退避复位
		return turn, nil
	}
	if turn != nil && turn.ErrMessage != "" {
		c.noteUpstreamError(turn.ErrMessage)
	} else if err != nil {
		c.noteUpstreamError(err.Error())
	}
	if err == nil {
		err = fmt.Errorf("%s", turn.ErrMessage)
	}
	return turn, err
}

// ToolCall 是一次工具调用（由提示词仿真解析出来）。
type ToolCall struct {
	CallID    string
	Name      string
	Arguments string
}

// Usage 是上游返回的用量（Prism 会给出 cache 相关字段，直接透传）。
type Usage struct {
	InputTokens       int
	OutputTokens      int
	CachedInputTokens int // 读缓存（cache read）
	CacheWriteTokens  int // 写缓存（cache creation）
	// CacheWriteReported=false 表示上游未上报写缓存字段（与"确实为 0"区分）。
	CacheWriteReported bool
	ReasoningTokens    int
	TotalTokens        int
	// Reported=false 表示上游**整段未上报 usage**，此时所有计数都是零值，
	// 不代表真的用了 0 个 token。
	//
	// 实测（prism.openai.com，start + 轮询私协议）：终态 payload 里根本没有
	// usage 字段（键只有 id / output / conversationId / codexDebug /
	// codexListenSnapshot）。因此对外**不得**把零值当真实用量发出——那等于
	// 伪造计费数据。下游看到 Reported=false 应自行按长度估算或标记未知。
	Reported bool
	// Raw 保留上游 usage 原文，便于排查字段名差异与对账。
	Raw map[string]any
}

// Run 执行一轮：start → 轮询 → 解析成 Turn。
//
// 若客户端装配了材料池（SetMaterialPool），会**从池中挑新鲜槽**执行，
// 失败时自动换槽重试（最多 maxSlotAttempts 次），从而实现多身份轮转提吞吐。
func (c *Client) Run(ctx context.Context, req Request) (*Turn, error) {
	if err := c.WarmSession(ctx); err != nil {
		return nil, err
	}

	pool := c.Pool()
	// 池可能尚未扫描（首次调用）→ 先扫一次再决定路径。
	poolSize := 0
	if pool != nil {
		if poolSize = pool.Size(); poolSize == 0 {
			poolSize, _ = pool.Scan()
		}
	}
	if pool == nil || poolSize == 0 {
		// 退化路径（无池或池为空）：走单份材料仓库。
		return c.runWithMaterial(ctx, req, nil)
	}
	if poolSize == 1 {
		// 单槽：也走池路径（统一新鲜度判定与统计）。
		mat, slot, err := pool.Acquire(ctx)
		if err != nil {
			return c.runWithMaterial(ctx, req, nil) // 池不可用则退回单份材料
		}
		turn, rerr := c.runWithMaterial(ctx, req, mat)
		slot.Release(rerr)
		// 必须过 finishTurn：否则上游内层错误会被当成成功返回，且熔断不触发。
		return c.finishTurn(turn, rerr)
	}

	maxSlotAttempts := c.cfg.SlotAttempts
	if maxSlotAttempts <= 0 {
		maxSlotAttempts = 3
	}
	var lastErr error
	var lastTurn *Turn
	tried := map[int]bool{}

	for attempt := 0; attempt < maxSlotAttempts; attempt++ {
		mat, slot, err := pool.Acquire(ctx)
		if err != nil {
			if lastErr != nil {
				return lastTurn, fmt.Errorf("%w（换槽重试后仍失败: %v）", lastErr, err)
			}
			return nil, err
		}
		// 本轮已试过的槽，避免在坏槽上反复打转
		if tried[slot.ID] && attempt > 0 {
			slot.Release(nil)
			continue
		}
		tried[slot.ID] = true

		turn, err := c.runWithMaterial(ctx, req, mat)
		slot.Release(err)
		turn, err = c.finishTurn(turn, err)
		if err == nil {
			return turn, nil
		}
		lastTurn, lastErr = turn, err
		// ⭐ 限流型错误不再「换槽重试」：换槽只是换材料/身份，并不能绕开
		// **账号级**限流，而每次重试都会再打一次上游 —— 把 N 路请求放大成
		// N×attempts 次调用，等于自己烧配额、把账号推入更深冷却。
		// 这类错误应立即上抛，由在飞闸门 + 平滑放行 + 熔断退避去治理。
		if lastErr != nil && isRetryableUpstreamErr(lastErr.Error()) {
			c.cfg.Logf("prism: 槽%d 命中限流型错误，不换槽重试（避免放大上游压力）", slot.ID)
			return lastTurn, lastErr
		}
		c.cfg.Logf("prism: 槽%d 失败（%s），换槽重试 (%d/%d)",
			slot.ID, shortErr(lastErr), attempt+1, maxSlotAttempts)
	}
	if lastErr != nil {
		return lastTurn, fmt.Errorf("prism: %d 个材料槽均失败，最后错误: %w", maxSlotAttempts, lastErr)
	}
	return lastTurn, nil
}

// runWithMaterial 用指定材料跑一轮（mat 为 nil 时走单份材料仓库）。
func (c *Client) runWithMaterial(ctx context.Context, req Request, mat *Material) (*Turn, error) {
	var err error
	if mat == nil {
		if mat, err = c.material(ctx); err != nil {
			return nil, err
		}
	}

	conv := strings.TrimSpace(req.ConversationID)
	if conv == "" {
		conv = "cdx1_" + newUUID4()
	}
	// 模型与思考档位：**默认必须沿用材料里的值**。
	// 实测（变体 C）：把 metadata.model / reasoning_effort 改写掉，
	// start 会立刻以 400 "Please submit prompt again" 失败——这两项与
	// 材料里的沙箱/会话快照是绑定的，属于材料的一部分，不能凭空改。
	// 只有当调用方显式指定模型时才覆盖（最佳努力，上游若拒绝会如实上报）。
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = strings.TrimSpace(mat.Model)
	}
	if model == "" {
		model = c.cfg.Model
	}
	effort := strings.TrimSpace(req.ReasoningEffort)
	if effort == "" {
		effort = firstNonBlank(
			strOf(mat.Metadata["reasoning_effort"]),
			c.cfg.ReasoningEffort,
		)
	}

	// input：材料的 system 前缀（真机抓到的 Prism Chat 系统提示词 + 上下文块）
	// 之后接本次请求的会话内容。上游对 input 结构宽容，但 role/type 必须正确。
	input := make([]map[string]any, 0, len(mat.InputPrefix)+len(req.Input)+1)
	input = append(input, mat.InputPrefix...)
	if s := strings.TrimSpace(req.InstructionsText); s != "" {
		input = append(input, sysMsg(s))
	}
	input = append(input, req.Input...)
	// ── 图片：走**内联 base64**（真机唯一走得通的路）────────────────────
	//
	// 实测结论（逆向站点 HAR + 多轮探针）：上游**没有多模态入参**。
	//   · input_image + data URL → 报 "not a valid Prism storage URL"
	//   · input_file + project_path → 语法接受，但模型去**会话工作区**找文件，
	//     而文件能否出现在工作区取决于站点编辑器写项目协作文档（Y-Sweet WS），
	//     我们复制不了 ⇒ 实测模型答"看不到图片"。
	// 真正可行：把 base64 **内联进本轮 user 消息**，附「落盘命令 + view_image
	// 指引」，模型自己还原后 view_image 就能真看到（实测三色带图答对）。
	if len(req.Images) > 0 {
		inlines, failures := c.PrepareInlineImages(ctx, req.Images)
		blocks := make([]any, 0, len(inlines)*2)
		for _, im := range inlines {
			blocks = append(blocks, map[string]any{
				"type": "input_text", "text": inlineImageText(im),
			})
		}
		if len(blocks) > 0 {
			// 附到最后一条 user 消息上（接返回值：无 user 消息时会新建一条）。
			input = attachInlineImages(input, blocks)
		}
		if notice := inlineImageNoticeForFailures(len(req.Images), failures, len(inlines)); notice != "" {
			input = append(input, sysMsg(notice))
			c.cfg.Logf("prism: %d 张图片中 %d 张未能载入，已如实告知模型",
				len(req.Images), len(failures))
		}
	}
	if len(req.Tools) > 0 {
		input = append(input, sysMsg(toolProtocol(req.Tools)))
	}

	meta := map[string]any{}
	for k, v := range mat.Metadata {
		meta[k] = v
	}
	meta["model"] = model
	meta["reasoning_effort"] = effort
	meta["projectId"] = mat.ProjectID
	meta["userId"] = c.UserID()
	if strings.TrimSpace(mat.UserID) != "" {
		meta["userId"] = mat.UserID
	}

	startBody, _ := json.Marshal(map[string]any{
		"input":          input,
		"metadata":       meta,
		"conversationId": conv,
	})

	// 每次请求现铸一枚 sentinel（严格一次性）。
	s, err := c.Mint(ctx)
	if err != nil {
		return nil, fmt.Errorf("prism: 取 sentinel 失败: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/api/llm/response_with_tools_start", string(startBody), s, nil)
	if err != nil {
		return nil, fmt.Errorf("prism: start: %w", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

	var start struct {
		Status       string         `json:"status"`
		RequestID    string         `json:"request_id"`
		TurnState    map[string]any `json:"turn_state"`
		Conversation string         `json:"conversation_id"`
		Response     map[string]any `json:"response"`
	}
	if err := json.Unmarshal(raw, &start); err != nil {
		return nil, fmt.Errorf("prism: start 响应解析: %w (%s)", err, truncStr(string(raw), 200))
	}
	if start.RequestID == "" {
		return nil, fmt.Errorf("prism: start 未返回 request_id: %s", truncStr(string(raw), 300))
	}

	turn := &Turn{RequestID: start.RequestID, Status: start.Status}

	// 上游有时「start 即终态」（劣化/请求过错）：无 turn_state 直接解析响应。
	if start.TurnState == nil {
		fillFromResponse(turn, start.Response)
		return turn, nil
	}

	// 轮询
	state := start.TurnState
	deadline := time.Now().Add(c.cfg.Timeout)
	for {
		if ctx.Err() != nil {
			return turn, ctx.Err()
		}
		if time.Now().After(deadline) {
			return turn, fmt.Errorf("prism: 轮询超时（%s）", c.cfg.Timeout)
		}
		pb, _ := json.Marshal(map[string]any{"request_id": start.RequestID, "turn_state": state})
		s2, err := c.Mint(ctx)
		if err != nil {
			return turn, err
		}
		resp, err := c.do(ctx, http.MethodPost, "/api/llm/response_with_tools_status", string(pb), s2, nil)
		if err != nil {
			return turn, fmt.Errorf("prism: status: %w", err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()

		var st struct {
			Status    string         `json:"status"`
			TurnState map[string]any `json:"turn_state"`
			Response  map[string]any `json:"response"`
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			return turn, fmt.Errorf("prism: status 解析: %w (%s)", err, truncStr(string(raw), 200))
		}
		if st.TurnState != nil {
			state = st.TurnState // 必须原样回传
		}
		turn.Status = st.Status

		switch st.Status {
		case "completed":
			turn.RawFinal = string(raw)
			fillFromResponse(turn, st.Response)
			return turn, nil
		case "error", "failed":
			turn.RawFinal = string(raw)
			fillFromResponse(turn, st.Response)
			if turn.ErrMessage == "" {
				turn.ErrMessage = "prism: 上游返回 error"
			}
			return turn, nil
		}
		select {
		case <-ctx.Done():
			return turn, ctx.Err()
		case <-time.After(1200 * time.Millisecond):
		}
	}
}

// Stop 取消一轮（上游省钱：未完成的轮次会持续消耗沙箱额度）。
func (c *Client) Stop(ctx context.Context, requestID string, turnState map[string]any) {
	if requestID == "" {
		return
	}
	s, err := c.Mint(ctx)
	if err != nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"request_id": requestID, "turn_state": turnState})
	resp, err := c.do(ctx, http.MethodPost, "/api/llm/response_with_tools_stop", string(b), s, nil)
	if err == nil {
		_ = resp.Body.Close()
	}
}

// prismUsagePayload 组装对外的 usage 块。
//
// ⚠️ 诚实计数：Prism 的私协议终态 payload **不含 usage 字段**（实测），此时
// 绝不能发一组零值冒充真实用量——那会污染下游的计费/缓存统计，也违反"新增
// 指标必须用真实数据计算"的约定。未上报时所有计数字段一律置 null，并用
// reported=false 明确标注；确实上报时才给出实数。
func prismUsagePayload(u Usage) map[string]any {
	if !u.Reported {
		return map[string]any{
			"reported":              false,
			"note":                  "上游未上报 usage（Prism 私协议终态 payload 无 usage 字段）",
			"input_tokens":          nil,
			"output_tokens":         nil,
			"total_tokens":          nil,
			"input_tokens_details":  nil,
			"output_tokens_details": nil,
			"prism":                 nil,
		}
	}
	return map[string]any{
		"reported":      true,
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
		"total_tokens":  u.TotalTokens,
		"input_tokens_details": map[string]any{
			"cached_tokens":         u.CachedInputTokens,
			"cache_creation_tokens": u.CacheWriteTokens,
			"cache_write_reported":  u.CacheWriteReported,
		},
		"output_tokens_details": map[string]any{"reasoning_tokens": u.ReasoningTokens},
		// 汇总口径，便于下游直接取用，不必自己拼。
		"prism": map[string]any{
			"effective_input_tokens": u.EffectiveInput(),
			"cache_read_tokens":      u.CachedInputTokens,
			"cache_write_tokens":     u.CacheWriteTokens,
			"cache_hit_rate":         u.CacheHitRate(),
		},
	}
}

// ------------------------------------------------------------------ 解析

// fillFromResponse 从终态响应里抽正文 / 思考 / 工具调用 / 用量。
//
// ⚠️ 结构层级：上游的 payload 嵌在 response 里，即
//
//	{"status":"completed","response":{"status":"success","payload":{
//	    "output":[…],"usage":{…},"message":"…"}}}
//
// 早期只读 response.output 会永远取到空正文（且错误信息也被埋在这层，
// 导致把失败误判成"成功"）——必须剥到 response.payload。
func fillFromResponse(t *Turn, resp map[string]any) {
	if resp == nil {
		return
	}
	payload, _ := resp["payload"].(map[string]any)
	if payload == nil {
		payload = resp // 兼容扁平形态
	}
	if msg := strOf(payload["message"]); msg != "" {
		t.ErrMessage = msg
	}
	if u, ok := payload["usage"].(map[string]any); ok {
		t.Usage = parseUsage(u)
	}
	out, _ := payload["output"].([]any)
	var text, reasoning strings.Builder
	for _, it := range out {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		switch strOf(m["type"]) {
		case "message":
			cs, _ := m["content"].([]any)
			for _, cc := range cs {
				cm, _ := cc.(map[string]any)
				if cm == nil {
					continue
				}
				text.WriteString(strOf(cm["text"]))
			}
		case "reasoning":
			sm, _ := m["summary"].([]any)
			for _, s := range sm {
				sm2, _ := s.(map[string]any)
				if sm2 != nil {
					reasoning.WriteString(strOf(sm2["text"]))
				}
			}
			// 有些形态把思考放在 content
			if reasoning.Len() == 0 {
				cs, _ := m["content"].([]any)
				for _, cc := range cs {
					cm, _ := cc.(map[string]any)
					if cm != nil {
						reasoning.WriteString(strOf(cm["text"]))
					}
				}
			}
		}
	}
	t.Reasoning = reasoning.String()
	t.Text = text.String()

	// 工具仿真：正文里出现 <tool_call>{...}</tool_call> 则提取
	if calls, cleaned := extractToolCalls(t.Text); len(calls) > 0 {
		t.ToolCalls = calls
		t.Text = cleaned
	}
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// ------------------------------------------------------------------ SSE 输出

// EventWriter 是 Responses SSE 事件写手。
type EventWriter struct {
	w       io.Writer
	flusher http.Flusher
	seq     int
	model   string
}

// NewEventWriter 建事件写手。
func NewEventWriter(w io.Writer, flusher http.Flusher, model string) *EventWriter {
	return &EventWriter{w: w, flusher: flusher, model: model}
}

// emit 写一个 SSE 事件并立即 Flush（低延迟关键：否则中间层会攒缓冲）。
func (e *EventWriter) emit(eventType string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	if _, ok := payload["type"]; !ok {
		payload["type"] = eventType
	}
	if _, ok := payload["sequence_number"]; !ok {
		e.seq++
		payload["sequence_number"] = e.seq
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", eventType, b)
	if e.flusher != nil {
		e.flusher.Flush()
	}
}

// WriteFailure 写一个终态失败事件（传输层/材料层失败时用），
// 让下游能干净收尾而不是挂等。
func (e *EventWriter) WriteFailure(responseID, message string) {
	e.emit("response.created", map[string]any{
		"response": map[string]any{
			"id": responseID, "object": "response", "status": "in_progress",
			"model": e.model, "output": []any{},
		},
	})
	e.emit("response.failed", map[string]any{
		"response": map[string]any{
			"id": responseID, "status": "failed", "model": e.model,
			"error": map[string]any{"code": "prism_upstream_error", "message": message},
		},
	})
}

// WriteTurn 把一轮结果写成完整的 Responses 事件序列。
// responseID 是本次 Responses 的 id（由调用方生成，用于事件引用）。
func (e *EventWriter) WriteTurn(turn *Turn, responseID string) {
	itemID := "msg_" + responseID

	// 1) created
	e.emit("response.created", map[string]any{
		"response": map[string]any{
			"id": responseID, "object": "response", "status": "in_progress",
			"model": e.model, "output": []any{},
		},
	})
	e.emit("response.in_progress", map[string]any{
		"response": map[string]any{"id": responseID, "status": "in_progress", "model": e.model},
	})

	// 2) 思考（真增量：只有上游真的给了才发）
	if strings.TrimSpace(turn.Reasoning) != "" {
		rid := "rs_" + responseID
		e.emit("response.output_item.added", map[string]any{
			"output_index": 0,
			"item":         map[string]any{"id": rid, "type": "reasoning", "summary": []any{}},
		})
		e.emit("response.reasoning_summary_text.delta", map[string]any{
			"item_id": rid, "output_index": 0, "summary_index": 0, "delta": turn.Reasoning,
		})
		e.emit("response.reasoning_summary_text.done", map[string]any{
			"item_id": rid, "output_index": 0, "summary_index": 0, "text": turn.Reasoning,
		})
		e.emit("response.output_item.done", map[string]any{
			"output_index": 0,
			"item":         map[string]any{"id": rid, "type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": turn.Reasoning}}},
		})
	}

	// 3) 正文（一次性发：上游无真流式，不做假 sleep 分块）
	outputIndex := 0
	if strings.TrimSpace(turn.Reasoning) != "" {
		outputIndex = 1
	}
	if turn.Text != "" {
		e.emit("response.output_item.added", map[string]any{
			"output_index": outputIndex,
			"item": map[string]any{
				"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{},
			},
		})
		e.emit("response.content_part.added", map[string]any{
			"item_id": itemID, "output_index": outputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
		e.emit("response.output_text.delta", map[string]any{
			"item_id": itemID, "output_index": outputIndex, "content_index": 0, "delta": turn.Text,
		})
		e.emit("response.output_text.done", map[string]any{
			"item_id": itemID, "output_index": outputIndex, "content_index": 0, "text": turn.Text,
		})
		e.emit("response.content_part.done", map[string]any{
			"item_id": itemID, "output_index": outputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": turn.Text, "annotations": []any{}},
		})
		e.emit("response.output_item.done", map[string]any{
			"output_index": outputIndex,
			"item": map[string]any{
				"id": itemID, "type": "message", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": turn.Text, "annotations": []any{}}},
			},
		})
	}

	// 4) 工具调用（仿真通道解析出来的）
	for i, tc := range turn.ToolCalls {
		idx := outputIndex + 1 + i
		// 自定义工具（apply_patch 等）走 custom_tool_call
		if isCustomTool(tc.Name) {
			e.emit("response.output_item.done", map[string]any{
				"output_index": idx,
				"item": map[string]any{
					"id": tc.CallID, "type": "custom_tool_call", "call_id": tc.CallID,
					"name": tc.Name, "input": tc.Arguments, "status": "completed",
				},
			})
			continue
		}
		e.emit("response.output_item.added", map[string]any{
			"output_index": idx,
			"item": map[string]any{
				"id": tc.CallID, "type": "function_call", "call_id": tc.CallID,
				"name": tc.Name, "arguments": "", "status": "in_progress",
			},
		})
		e.emit("response.function_call_arguments.delta", map[string]any{
			"item_id": tc.CallID, "output_index": idx, "delta": tc.Arguments,
		})
		e.emit("response.function_call_arguments.done", map[string]any{
			"item_id": tc.CallID, "output_index": idx, "arguments": tc.Arguments,
		})
		e.emit("response.output_item.done", map[string]any{
			"output_index": idx,
			"item": map[string]any{
				"id": tc.CallID, "type": "function_call", "call_id": tc.CallID,
				"name": tc.Name, "arguments": tc.Arguments, "status": "completed",
			},
		})
	}

	// 5) 终态
	status := "completed"
	if turn.ErrMessage != "" {
		status = "failed"
	}
	final := map[string]any{
		"id": responseID, "object": "response", "status": status, "model": e.model,
		"usage": prismUsagePayload(turn.Usage),
	}
	if len(turn.ToolCalls) > 0 {
		final["status"] = "completed"
	}
	if status == "failed" {
		e.emit("response.failed", map[string]any{
			"response": map[string]any{"id": responseID, "status": "failed", "model": e.model,
				"error": map[string]any{"code": "upstream_error", "message": turn.ErrMessage}},
		})
		return
	}
	e.emit("response.completed", map[string]any{"response": final})
}
