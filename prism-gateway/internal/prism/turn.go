package prism

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ============================================================================
// 轮次：start（提交）+ status（轮询）。
// 上游没有 SSE，正文在 completed 时一次性到达；流式是网关合成的（见 stream 包）。
// ============================================================================

// Turn 是一轮的输入（已由适配层翻译成上游形状）。
type Turn struct {
	Input          []InputItem
	ConversationID string
	Model          string          // 上游认的完整模型名
	Effort         string          // low|medium|high|xhigh
	Tools          json.RawMessage // 原样透传（上游会忽略，仅作提示词素材）
	// SandboxToken / SandboxURL 允许调用方指定（材料包模式）。
	SandboxToken string
	SandboxURL   string
	ProjectID    string
}

// StartResult 是 start 的产物。
type StartResult struct {
	RequestID  string
	ConvID     string
	TurnState  json.RawMessage
	StartedMs  int64
	Reconnects int
}

// TurnResult 是一轮完成的产物。
type TurnResult struct {
	Text      string
	Reasoning string
	Upstream  []UpstreamCall
	// Emulated 是从正文里解析出的**客户端工具调用**（模型按工具协议输出的块）。
	// 与 Upstream 的区别：Upstream 是上游 Codex 沙箱自己执行过的（只透传展示），
	// Emulated 需要客户端在它自己的机器上执行。
	Emulated  []EmulatedCall
	ItemTypes map[string]int
	Polls     int
	PollMs    int64
	Err       string          // 上游以 completed+error 形式返回的失败文案
	Raw       json.RawMessage // 原始 completed 响应（观测用）
}

// StartTurn 发起一轮生成。
// 沙箱没预热好时 start 会挂死，所以按超时重试；sandbox_reconnecting 时**重试同一沙箱**
// （换沙箱等于白付一次 6~18s 预热）。
func (c *Client) StartTurn(ctx context.Context, t *Turn) (*StartResult, error) {
	attempts := c.opt.StartAttempts
	projectID := t.ProjectID
	if projectID == "" {
		var err error
		projectID, err = c.EnsureProject(ctx)
		if err != nil {
			return nil, err
		}
	}

	var lastErr error
	reconnects := 0
	transportFails := 0
	var msSandbox, msStart int64

	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, time.Duration(attempt)*1500*time.Millisecond); err != nil {
				return nil, err
			}
		}
		// 会话必须与沙箱同批：重试会换沙箱，旧会话在上游会变孤儿。
		conv := t.ConversationID
		if conv == "" {
			conv = NewConversationID()
		}

		// 沙箱：热路径 0 RTT；冷路径走单飞预热（并发合并）。
		sandbox := t.SandboxToken
		if sandbox == "" {
			tSb := time.Now()
			tok, _, err := c.EnsureSandbox(ctx, projectID)
			msSandbox += time.Since(tSb).Milliseconds()
			if err != nil {
				return nil, err
			}
			sandbox = tok
		}

		// 会话登记 best-effort，不阻塞 start。
		go func(pid, cv string) { _ = c.RegisterConversation(ctx, pid, cv) }(projectID, conv)

		// metadata：优先材料包（上游把 start 与页面上下文强绑定）。
		meta := &Metadata{
			ProjectID:       projectID,
			UserID:          c.UserID(),
			Model:           t.Model,
			ReasoningEffort: t.Effort,
			FrontendOrigin:  c.opt.Origin,
			SandboxURL:      firstNonEmpty(t.SandboxURL, c.opt.Origin+PathSandboxBase+"/"),
			SandboxToken:    sandbox,
		}
		if c.opt.MaterialFunc != nil {
			if m, err := c.opt.MaterialFunc(ctx); err == nil && m != nil {
				m.Model, m.ReasoningEffort = t.Model, t.Effort
				meta = m
			}
		}
		meta.CodexListenSnapshot = listenSnapshot(meta.UserID, projectID, conv, meta.SandboxURL, sandbox)

		body := &StartRequest{
			Input:          t.Input,
			Metadata:       meta,
			ConversationID: conv,
			Tools:          t.Tools,
		}

		startCtx, cancel := context.WithTimeout(ctx, c.opt.StartTimeout)
		tStart := time.Now()
		resp, err := c.Do(startCtx, Request{
			Method:  http.MethodPost,
			URL:     JoinURL(c.opt.Origin, PathStart),
			Body:    body,
			Headers: c.sentinelHeader(),
			Account: c.acct,
			Timeout: c.opt.StartTimeout,
		}, 0)
		cancel()
		msStart += time.Since(tStart).Milliseconds()
		if err != nil {
			lastErr = err
			transportFails++
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			// 连续两次传输层失败才作废沙箱（上游挂死时沙箱多半是无辜的）。
			if transportFails >= 2 {
				c.InvalidateSandbox()
			}
			continue
		}
		transportFails = 0

		if resp.Status >= 300 {
			ue := &UpstreamError{Status: resp.Status, Op: "start", Msg: resp.Snippet(300)}
			lastErr = ue
			if resp.Status >= 500 {
				c.InvalidateSandbox()
				continue
			}
			return nil, ue
		}

		var sr StartResponse
		_ = json.Unmarshal(resp.Body, &sr)
		if sr.RequestID != "" && len(sr.TurnState) > 0 && sr.TurnState[0] == '{' {
			c.mu.Lock()
			c.stats.Starts++
			c.stats.SandboxMs += msSandbox
			c.stats.StartMs += msStart
			c.mu.Unlock()
			return &StartResult{
				RequestID: sr.RequestID, ConvID: firstNonEmpty(sr.ConversationID, conv),
				TurnState: sr.TurnState, StartedMs: msStart, Reconnects: reconnects,
			}, nil
		}

		// 上游会以 200 + 内联错误立刻返回失败（没有 turn_state）。
		msg := startErrText(&sr)
		// 沙箱重连中：等 codex ready 后**重试同一沙箱**。
		if isSandboxReconnecting(&sr) && reconnects < c.opt.MaxReconnects {
			reconnects++
			c.waitCodexReady(ctx, sandbox)
			if ctx.Err() == nil {
				_ = sleepCtx(ctx, 3*time.Second)
			}
			attempt-- // 重试同一沙箱不消耗 attempt
			continue
		}
		lastErr = &UpstreamError{Status: http.StatusBadGateway, Op: "start", Msg: msg}
		if !startErrRetryable(msg) {
			return nil, lastErr
		}
		c.InvalidateSandbox()
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("start failed")
	}
	c.mu.Lock()
	c.stats.Failures++
	c.mu.Unlock()
	return nil, lastErr
}

// startErrText 抽取 start 内联失败文案。
func startErrText(sr *StartResponse) string {
	if sr == nil {
		return "unknown upstream error"
	}
	if sr.Response != nil {
		if sr.Response.Payload != nil && sr.Response.Payload.Status != "" {
			// payload 里通常没有 message，退回 status
		}
		if sr.Response.Message != "" {
			return sr.Response.Message
		}
	}
	if sr.Message != "" {
		return sr.Message
	}
	return "unknown upstream error"
}

// isSandboxReconnecting 判断是否沙箱重连中（HTTP 200 但永远等不到回答）。
func isSandboxReconnecting(sr *StartResponse) bool {
	if sr == nil {
		return false
	}
	if strings.EqualFold(sr.Reason, "sandbox_reconnecting") {
		return true
	}
	if sr.SandboxTokenPresent != nil && !*sr.SandboxTokenPresent && sr.RequestID == "" {
		return true
	}
	return false
}

// startErrRetryable 判断 start 内联错误是否值得换沙箱重试。
// 请求过错（模型名不在清单、上下文超长）换沙箱毫无意义。
func startErrRetryable(msg string) bool {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "unsupported assistant model"),
		strings.Contains(m, "400 bad request"),
		strings.Contains(m, "context"),
		strings.Contains(m, "too long"),
		strings.Contains(m, "invalid"):
		return false
	}
	return true
}

// waitCodexReady 等沙箱 codex 就绪（best-effort，最多 8s）。
func (c *Client) waitCodexReady(ctx context.Context, sandbox string) {
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return
		}
		if err := c.Heartbeat(ctx); err == nil {
			return
		}
		if err := sleepCtx(ctx, 1500*time.Millisecond); err != nil {
			return
		}
	}
}

// PollTurn 轮询到 completed/失败。turn_state 必须逐轮回填，否则 401。
// onPending 每次 pending 时回调（可用于保活/进度）。
func (c *Client) PollTurn(ctx context.Context, st *StartResult, onPending func(polls int, turnState json.RawMessage)) (*TurnResult, error) {
	tAll := time.Now()
	deadline := tAll.Add(c.opt.PollBudget)
	attempt, failures := 0, 0
	for {
		attempt++
		body := StatusRequest{RequestID: st.RequestID, TurnState: st.TurnState}
		callCtx, cancel := context.WithTimeout(ctx, c.opt.PollCallTimeout)
		resp, err := c.Do(callCtx, Request{
			Method:  http.MethodPost,
			URL:     JoinURL(c.opt.Origin, PathStatus),
			Body:    body,
			Account: c.acct,
			Timeout: c.opt.PollCallTimeout,
		}, 0)
		cancel()

		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			failures++
			if failures > 10 {
				return nil, fmt.Errorf("status 轮询失败: %w", err)
			}
			if err := sleepCtx(ctx, pollInterval(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		if resp.Status >= 500 {
			failures++
			if failures > 10 {
				return nil, &UpstreamError{Status: resp.Status, Op: "status", Msg: resp.Snippet(200)}
			}
			if err := sleepCtx(ctx, pollInterval(attempt)); err != nil {
				return nil, err
			}
			continue
		}
		if resp.Status >= 300 {
			return nil, &UpstreamError{Status: resp.Status, Op: "status", Msg: resp.Snippet(200)}
		}
		failures = 0

		var sr StatusResponse
		_ = json.Unmarshal(resp.Body, &sr)
		if len(sr.TurnState) > 0 && string(sr.TurnState) != "null" {
			st.TurnState = sr.TurnState
		}
		switch strings.ToLower(strings.TrimSpace(sr.Status)) {
		case "completed":
			c.mu.Lock()
			c.stats.Polls += int64(attempt)
			c.stats.PollMs += time.Since(tAll).Milliseconds()
			c.mu.Unlock()
			payload := (*TurnPayload)(nil)
			if sr.Response != nil {
				payload = sr.Response.Payload
			}
			return &TurnResult{
				Text:      payload.Text(),
				Reasoning: payload.Reasoning(),
				Upstream:  payload.UpstreamCalls(),
				ItemTypes: payload.ItemTypes(),
				Polls:     attempt,
				PollMs:    time.Since(tAll).Milliseconds(),
				Raw:       json.RawMessage(resp.Body),
			}, nil
		case "failed", "error":
			c.mu.Lock()
			c.stats.Polls += int64(attempt)
			c.mu.Unlock()
			return &TurnResult{Err: resp.Snippet(400), Polls: attempt, PollMs: time.Since(tAll).Milliseconds()}, nil
		default: // pending | started | ""
			if onPending != nil {
				onPending(attempt, st.TurnState)
			}
		}
		if time.Now().After(deadline) {
			return nil, &UpstreamError{
				Status: http.StatusGatewayTimeout, Op: "status",
				Msg: fmt.Sprintf("轮询预算 %s 用尽（上游劣化时会拖满数分钟）", c.opt.PollBudget),
			}
		}
		if err := sleepCtx(ctx, pollInterval(attempt)); err != nil {
			return nil, err
		}
	}
}

// StopTurn 取消上游轮次（客户端断连时调用，省钱）。
func (c *Client) StopTurn(ctx context.Context, st *StartResult, conv string) error {
	if st == nil || st.RequestID == "" {
		return nil
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := c.Do(sctx, Request{
		Method: http.MethodPost,
		URL:    JoinURL(c.opt.Origin, PathStop),
		Body: StopRequest{
			RequestID: st.RequestID, ConversationID: conv, TurnState: st.TurnState,
		},
		Account: c.acct,
		Timeout: 5 * time.Second,
	}, 0)
	return err
}

// ------------------------------------------------------------------ 用量与模型

// UsageSnapshot 是账号权益快照（上游没有额度百分比，只填身份与套餐）。
type UsageSnapshot struct {
	Email     string
	UserID    string
	PlanType  string
	FetchedAt time.Time
}

// FetchUsage 拉取权益信息。
func (c *Client) FetchUsage(ctx context.Context) (*UsageSnapshot, error) {
	if err := c.EnsureSession(ctx); err != nil {
		return nil, err
	}
	snap := &UsageSnapshot{FetchedAt: time.Now()}
	resp, err := c.Do(ctx, Request{
		Method: http.MethodGet, URL: JoinURL(c.opt.Origin, PathSession),
		Account: c.acct, Timeout: 20 * time.Second,
	}, 2)
	if err == nil && resp.Status < 300 {
		if v := resp.JSON(); v != nil {
			if user, ok := v["user"].(map[string]any); ok {
				snap.Email, _ = user["email"].(string)
				snap.UserID, _ = user["id"].(string)
			}
		}
	}
	resp, err = c.Do(ctx, Request{
		Method: http.MethodGet, URL: JoinURL(c.opt.Origin, PathEntitlements),
		Account: c.acct, Timeout: 20 * time.Second,
	}, 2)
	if err == nil && resp.Status < 300 {
		if v := resp.JSON(); v != nil {
			for _, k := range []string{"planType", "plan_type", "plan"} {
				if s, _ := v[k].(string); s != "" {
					snap.PlanType = s
					break
				}
			}
		}
	}
	return snap, nil
}

// sentinelHeader 返回对话面风控 header。
// 2026-09-19 起 upstream 强制校验 openai-sentinel-token（缺失/复用一律 403），
// 由登录侧车本地铸造、严格一次性。若未接侧车则返回空（上游会 403，需降级到 relay）。
func (c *Client) sentinelHeader() map[string]string {
	if sentinel == nil {
		return nil
	}
	tok, err := sentinel.Take()
	if err != nil || tok == "" {
		return nil
	}
	return map[string]string{HeaderSentinelToken: tok}
}

// pollInterval 是第 n 次 status 调用后的等待时长：
// 前 3 次急 poll 100ms，之后 0.4s 起步 +0.4s 到 2s 封顶。
func pollInterval(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	if n <= 3 {
		return 100 * time.Millisecond
	}
	d := time.Duration(n-3) * 400 * time.Millisecond
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// listenSnapshot 是服务端要求的会话快照（首轮 codex_session_id/last_turn_id 为空）。
func listenSnapshot(userID, projectID, conv, sandboxURL, sandboxToken string) string {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	snap := map[string]any{
		"user_id":              userID,
		"project_id":           projectID,
		"conversation_id":      conv,
		"sandbox_url":          sandboxURL,
		"sandbox_token":        sandboxToken,
		"workspace_session_id": strings.TrimPrefix(conv, "cdx1_"),
		"codex_session_id":     nil,
		"last_turn_id":         nil,
		"endpoint_identity":    nil,
		"last_exec_at":         nil,
		"transcript_cursor":    0,
		"created_at":           now,
		"updated_at":           now,
		"last_saved_at":        nil,
	}
	raw, _ := json.Marshal(snap)
	return string(raw)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
