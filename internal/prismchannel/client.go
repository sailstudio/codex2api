package prismchannel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type slot struct {
	id             string
	shared         *sandboxContext
	unmetered      bool
	metadata       map[string]any
	conversationID string
	credential     Credential
	account        *accountState
	owner, project string
	sandbox        sandbox
	expires        time.Time
	warm           bool
	generation     string
}
type sandboxContext struct {
	mu              sync.Mutex // only guards validGeneration; never held over I/O
	gate            chan struct{}
	validGeneration string
	base            *slot // exclusively owned by gate
}
type accountState struct {
	unavailableUntil atomic.Int64
	rateMu           sync.Mutex
	starts           []time.Time
}
type slotContextKey struct{}
type Client struct {
	cfg        Config
	http       *http.Client
	slots      chan *slot
	rateWindow time.Duration
	waiters    atomic.Int64
	metrics    Metrics
	cache      *TokenCache
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	closeOnce  sync.Once
}

func New(c Config) (*Client, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 512
	t.MaxIdleConnsPerHost = 256
	t.MaxConnsPerHost = 512
	t.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.ResponseHeaderTimeout = c.HTTPTimeout
	t.TLSHandshakeTimeout = 10 * time.Second
	t.IdleConnTimeout = 90 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cl := &Client{cfg: c, http: &http.Client{Transport: t, Timeout: c.HTTPTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, slots: make(chan *slot, len(c.Credentials)*c.AccountConcurrency), ctx: ctx, cancel: cancel}
	cl.rateWindow = time.Minute + 3*time.Second
	cl.cache = newTokenCache(c, &cl.metrics)
	for index, cr := range c.Credentials {
		if cr.ID == "" {
			cr.ID = fmt.Sprintf("account-%d", index)
		}
		account := &accountState{}
		var pool []*sandboxContext
		for i := 0; i < c.SandboxPoolSize; i++ {
			pool = append(pool, &sandboxContext{gate: make(chan struct{}, 1), base: &slot{id: uuid.NewString(), credential: cr, account: account, unmetered: true}})
		}
		for i := 0; i < c.AccountConcurrency; i++ {
			s := &slot{id: uuid.NewString(), credential: cr, account: account}
			if len(pool) > 0 {
				s.shared = pool[i%len(pool)]
			}
			cl.slots <- s
		}
	}
	cl.wg.Add(1)
	go cl.keepWarm()
	return cl, nil
}
func (c *Client) Close() {
	c.closeOnce.Do(func() { c.cancel(); c.wg.Wait(); c.http.CloseIdleConnections() })
}
func (c *Client) Metrics() *Metrics  { return &c.metrics }
func (c *Client) Cache() *TokenCache { return c.cache }
func (c *Client) Capacity() int      { return cap(c.slots) }

func (c *Client) readySlot(target string) *slot {
	for i, n := 0, len(c.slots); i < n; i++ {
		select {
		case s := <-c.slots:
			if s.account.unavailableUntil.Load() <= time.Now().UnixNano() && (target == "" || s.id == target) {
				return s
			}
			c.slots <- s
		default:
			return nil
		}
	}
	return nil
}
func (c *Client) acquire(ctx context.Context) (*slot, error) {
	return c.acquireFor(ctx, "", nil)
}
func (c *Client) acquireFor(ctx context.Context, target string, waiting func() error) (*slot, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if c.ctx.Err() != nil {
		return nil, context.Canceled
	}
	if s := c.readySlot(target); s != nil {
		return s, nil
	}
	if c.waiters.Add(1) > int64(c.cfg.MaxWaiters) {
		c.waiters.Add(-1)
		c.metrics.Rejected.Add(1)
		return nil, ErrBusy
	}
	c.metrics.Queued.Add(1)
	defer c.waiters.Add(-1)
	defer c.metrics.Queued.Add(-1)
	// Flush lifecycle events once bounded queue admission succeeds, so accepted
	// streams receive heartbeats while waiting. Full queues still reject before SSE.
	if waiting != nil {
		if err := waiting(); err != nil {
			return nil, err
		}
	}
	t := time.NewTimer(c.cfg.QueueTimeout)
	deadline := time.Now().Add(c.cfg.QueueTimeout)
	defer t.Stop()
	for {
		if !time.Now().Before(deadline) {
			c.metrics.Rejected.Add(1)
			return nil, ErrBusy
		}
		select {
		case s := <-c.slots:
			if s.account.unavailableUntil.Load() <= time.Now().UnixNano() && (target == "" || s.id == target) {
				return s, nil
			}
			c.slots <- s
			if e := sleep(ctx, min(20*time.Millisecond, time.Until(deadline))); e != nil {
				return nil, e
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.ctx.Done():
			return nil, context.Canceled
		case <-t.C:
			c.metrics.Rejected.Add(1)
			return nil, ErrBusy
		}
	}
}

// Pace start attempts across a rolling window. The small guard allows for
// ticket/provider latency jitter; turn execution and polling remain concurrent.
func (c *Client) pace(parent context.Context, account *accountState) error {
	if c.cfg.AccountRPM == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, c.cfg.QueueTimeout)
	defer cancel()
	waiting := false
	defer func() {
		if waiting {
			c.metrics.PacedWaiters.Add(-1)
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			if parent.Err() != nil {
				return parent.Err()
			}
			c.metrics.Rejected.Add(1)
			return ErrBusy
		}
		account.rateMu.Lock()
		now := time.Now()
		first := 0
		for first < len(account.starts) && !now.Before(account.starts[first].Add(c.rateWindow)) {
			first++
		}
		account.starts = append(account.starts[:0], account.starts[first:]...)
		if len(account.starts) < c.cfg.AccountRPM {
			account.starts = append(account.starts, now)
			account.rateMu.Unlock()
			return nil
		}
		delay := time.Until(account.starts[0].Add(c.rateWindow))
		account.rateMu.Unlock()
		if !waiting {
			waiting = true
			c.metrics.PacedWaiters.Add(1)
		}
		if err := sleep(ctx, delay); err != nil {
			if parent.Err() != nil {
				return parent.Err()
			}
			c.metrics.Rejected.Add(1)
			return ErrBusy
		}
	}
}

func (c *Client) do(ctx context.Context, cr Credential, method, path string, body []byte, contentType, token string, extra ...http.Header) ([]byte, error) {
	u, e := url.Parse(path)
	if e != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(path, "/") {
		return nil, &Error{502, "invalid_upstream_path", ""}
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.cfg.BaseURL, "/")+path, bytes.NewReader(body))
	if e != nil {
		return nil, &Error{502, "invalid_upstream_request", ""}
	}
	materialHeaders, e := c.materialHeaders(ctx, cr, path)
	if e != nil {
		return nil, e
	}
	req.AddCookie(&http.Cookie{Name: "prism_session_token", Value: cr.SessionToken})
	req.AddCookie(&http.Cookie{Name: "prism_oai_access_token", Value: cr.AccessToken})
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", strings.TrimRight(c.cfg.BaseURL, "/"))
	req.Header.Set("Referer", strings.TrimRight(c.cfg.BaseURL, "/")+"/")
	req.Header.Set("User-Agent", "codex2api-prism/1")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("X-Crixet-Sandbox-Token", token)
	}
	for _, headers := range extra {
		for k, vs := range headers {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
	}
	for name, values := range materialHeaders {
		req.Header[name] = values
	}
	resp, e := c.http.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &Error{502, "upstream_transport", ""}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, &Error{resp.StatusCode, "upstream_http", resp.Header.Get("Retry-After")}
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if e != nil || len(b) > 8<<20 {
		return nil, &Error{502, "upstream_body", ""}
	}
	return b, nil
}
func (c *Client) json(ctx context.Context, s *slot, method, path string, in, out any, token string) error {
	ctx = context.WithValue(ctx, slotContextKey{}, s)
	var b []byte
	var e error
	if in != nil {
		b, e = json.Marshal(in)
		if e != nil {
			return e
		}
	}
	b, e = c.do(ctx, s.credential, method, path, b, "application/json", token)
	if e != nil {
		var ae *Error
		if errors.As(e, &ae) {
			switch path {
			case "/api/backend/1/new":
				c.metrics.BackendNewErrors.Add(1)
				if ae.Status == 429 {
					c.metrics.BackendNew429.Add(1)
				}
			case PathStart:
				c.metrics.StartErrors.Add(1)
				if ae.Status == 429 {
					c.metrics.Start429.Add(1)
				}
			}
		}
		return e
	}
	if out != nil && len(b) > 0 {
		if json.Unmarshal(b, out) != nil {
			return &Error{502, "upstream_json", ""}
		}
	}
	return nil
}

type envelope struct {
	Snapshot       json.RawMessage `json:"codex_listen_snapshot"`
	ConversationID string          `json:"conversation_id"`
	Status         string          `json:"status"`
	RequestID      string          `json:"request_id"`
	TurnState      json.RawMessage `json:"turn_state"`
	Usage          *Usage          `json:"usage"`
	Response       *struct {
		Status  string `json:"status"`
		Payload struct {
			HTTPStatus     int             `json:"httpStatus"`
			RootCause      string          `json:"rootCause"`
			Reason         string          `json:"reason"`
			Message        string          `json:"message"`
			ID             string          `json:"id"`
			Snapshot       json.RawMessage `json:"codexListenSnapshot"`
			ConversationID string          `json:"conversationId"`
			Output         []Item          `json:"output"`
			Usage          *Usage          `json:"usage"`
		} `json:"payload"`
	} `json:"response"`
}

func (e envelope) output() (string, []Item) {
	var b strings.Builder
	var calls []Item
	if e.Response != nil {
		for _, it := range e.Response.Payload.Output {
			if it.Type == "function_call" {
				calls = append(calls, it)
			}
			if it.Type == "message" && (it.Role == "assistant" || it.Role == "") {
				b.WriteString(it.Text)
				for _, v := range it.Content {
					if v.Type == "output_text" || v.Type == "text" || v.Type == "input_text" {
						b.WriteString(v.Text)
					}
				}
			}
		}
	}
	return b.String(), calls
}

func listenSnapshot(userID, projectID, conv, sandboxURL, sandboxToken string) string {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	raw, _ := json.Marshal(map[string]any{
		"user_id": userID, "project_id": projectID, "conversation_id": conv,
		"sandbox_url": sandboxURL, "sandbox_token": sandboxToken,
		"workspace_session_id": strings.TrimPrefix(conv, "cdx1_"),
		"codex_session_id":     nil, "last_turn_id": nil, "endpoint_identity": nil,
		"last_exec_at": nil, "transcript_cursor": 0,
		"created_at": now, "updated_at": now, "last_saved_at": nil,
	})
	return string(raw)
}

func (e envelope) failed() bool {
	return e.Status == "error" || e.Status == "failed" || e.Status == "cancelled" || e.Response != nil && (e.Response.Status == "error" || e.Response.Status == "failed")
}

// Classify only known structural causes. Never expose raw upstream messages.
func (e envelope) failureCode() string {
	if e.Response == nil {
		return "generation_failed"
	}
	p := e.Response.Payload
	if p.HTTPStatus >= 400 && p.HTTPStatus <= 599 {
		return fmt.Sprintf("generation_failed_http_%d", p.HTTPStatus)
	}
	cause := strings.ToLower(p.RootCause + " " + p.Reason + " " + p.Message)
	for _, v := range []struct{ match, code string }{
		{"project conversation lookup failed", "project_lookup"},
		{"sandbox_reconnecting", "sandbox_reconnecting"},
		{"codex_v2_restore_start failed (400", "restore_400"},
		{"codex_v2_restore_start failed (429", "restore_429"},
		{"valid prism storage", "image_storage"},
		{"context length", "context_length"},
	} {
		if strings.Contains(cause, v.match) {
			return "generation_failed_" + v.code
		}
	}
	return "generation_failed"
}

func validState(b json.RawMessage) bool { return len(b) > 0 && string(b) != "null" && json.Valid(b) }
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run owns a slot for the complete turn, including image upload and cancellation.
func (c *Client) Run(parent context.Context, owner string, r Request, emit func(Event) error) (result Result, err error) {
	result.CreatedAt = r.CreatedAt
	if result.CreatedAt == 0 {
		result.CreatedAt = time.Now().Unix()
	}
	c.metrics.Requests.Add(1)
	c.metrics.meter(1, 0)
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, c.cfg.RequestTimeout)
	defer cancel()
	stopClose := context.AfterFunc(c.ctx, cancel)
	defer stopClose()
	defer func() {
		if err != nil {
			c.metrics.Errors.Add(1)
		}
	}()
	if owner == "" {
		return result, &Error{400, "owner_required", ""}
	}
	input := append([]Item(nil), r.Input...)
	var prior *continuity
	if r.PreviousID != "" {
		history, tokens, cont, ok := c.cache.getTurn(owner, r.PreviousID)
		if !ok {
			return result, &Error{400, "previous_response_unavailable", ""}
		}
		input = append(history, input...)
		result.CacheReadTokens = tokens
		prior = cont
	}
	// Reconstructed chains have the same bounded memory budget as a cache entry.
	historyBytes, _ := json.Marshal(input)
	if int64(len(historyBytes)) > c.cfg.CacheBytes/4 {
		return result, &Error{413, "context_too_large", ""}
	}
	target := ""
	if prior != nil {
		target = prior.SlotID
	}
	var waiting func() error
	if emit != nil {
		waiting = func() error { return emit(Event{Keepalive: true}) }
	}
	s, e := c.acquireFor(ctx, target, waiting)
	if e != nil {
		return result, e
	}
	defer func() { c.slots <- s }()
	ctx = context.WithValue(ctx, slotContextKey{}, s)
	defer func() {
		var ae *Error
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			// A project can disappear upstream. Do not pin subsequent turns to it.
			c.invalidate(s)
			s.project = ""
			s.metadata = nil
			s.conversationID = ""
		}
		if errors.As(err, &ae) && (ae.Status == 429 || ae.Status == 401 || ae.Status == 403) {
			until := time.Now().Add(30 * time.Second)
			if ae.Status != 429 {
				until = time.Now().Add(5 * time.Minute)
			}
			if seconds, e := time.ParseDuration(ae.RetryAfter + "s"); e == nil && seconds > 0 {
				until = time.Now().Add(min(seconds, time.Hour))
			} else if date, e := http.ParseTime(ae.RetryAfter); e == nil && date.After(time.Now()) {
				until = minTime(date, time.Now().Add(time.Hour))
			}
			for old := s.account.unavailableUntil.Load(); old < until.UnixNano() && !s.account.unavailableUntil.CompareAndSwap(old, until.UnixNano()); old = s.account.unavailableUntil.Load() {
			}
		}
	}()
	c.metrics.Inflight.Add(1)
	defer c.metrics.Inflight.Add(-1)
	if emit != nil {
		if e = emit(Event{Keepalive: true}); e != nil {
			return result, e
		}
	}
	if e = c.prepare(ctx, s, owner); e != nil {
		return result, e
	}
	if prior != nil && (prior.ProjectID != s.project || prior.SandboxGeneration == "" || prior.SandboxGeneration != s.generation) {
		return result, &Error{400, "previous_response_context_expired", ""}
	}
	// Store the materialized attachments, so follow-ups reuse the same references
	// instead of downloading/uploading historical image URLs again.
	input, e = c.preprocessImages(ctx, s, input)
	if e != nil {
		return result, e
	}
	upstream := append([]Item(nil), input...)
	// Some upstreams restore workspace state without replaying textual history.
	// Retain complete facade history as well as native session continuity.
	if r.Instructions != "" {
		upstream = append([]Item{textItem("system", r.Instructions)}, upstream...)
	}
	if len(r.Tools) > 0 {
		upstream = append([]Item{textItem("system", toolContract(r))}, upstream...)
	}
	meta := make(map[string]any, len(s.metadata)+8)
	for k, v := range s.metadata {
		meta[k] = v
	}
	sandboxURL := s.sandbox.URL
	if sandboxURL != "" && !strings.HasSuffix(sandboxURL, "/") {
		sandboxURL += "/"
	}
	conversationID := ""
	if prior != nil {
		conversationID = prior.ConversationID
	} else if c.cfg.MaterialURL != "" && !c.cfg.MaterialHeadersOnly {
		conversationID = s.conversationID
	}
	if conversationID == "" {
		conversationID = "cdx1_" + uuid.NewString()
		s.conversationID = conversationID
	}
	meta["model"] = strings.TrimPrefix(r.Model, "prism-")
	if c.cfg.MaterialURL == "" || c.cfg.MaterialHeadersOnly {
		meta["projectId"] = s.project
		meta["userId"] = s.credential.UserID
		meta["sandbox_url"] = sandboxURL
		meta["sandbox_token"] = s.sandbox.Token
		meta["frontend_origin"] = strings.TrimRight(c.cfg.BaseURL, "/")
	}
	if prior != nil && prior.Snapshot != nil {
		snapshot := prior.Snapshot
		b, _ := json.Marshal(snapshot)
		meta["codex_listen_snapshot"] = string(b)
	}
	if _, ok := meta["codex_listen_snapshot"]; !ok {
		meta["codex_listen_snapshot"] = listenSnapshot(fmt.Sprint(meta["userId"]), s.project, conversationID, sandboxURL, s.sandbox.Token)
	}
	if r.Effort != "" {
		meta["reasoning_effort"] = r.Effort
	}
	var env envelope
	startBody := map[string]any{"input": prismInput(foldTools(upstream)), "metadata": meta, "conversationId": conversationID}
	if prior != nil && prior.UpstreamID != "" {
		startBody["previousResponseId"] = prior.UpstreamID
	}
	if e = c.pace(ctx, s.account); e != nil {
		return result, e
	}
	e = c.json(ctx, s, http.MethodPost, PathStart, startBody, &env, "")
	if e != nil {
		var ae *Error
		if !errors.As(e, &ae) || (ae.Status != 401 && ae.Status != 403 && ae.Status != 429) {
			c.invalidate(s)
		}
		return result, e
	}
	requestID := env.RequestID
	state := env.TurnState
	snapshot := responseSnapshot(env, nil)
	if env.ConversationID != "" {
		conversationID = env.ConversationID
	}
	done := env.Status == "completed" || env.failed()
	defer func() {
		if !done && requestID != "" {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), min(3*time.Second, c.cfg.HTTPTimeout))
			defer stopCancel()
			c.metrics.Stops.Add(1)
			if c.json(stopCtx, s, http.MethodPost, PathStop, map[string]any{"request_id": requestID, "turn_state": state}, nil, "") != nil {
				c.metrics.StopErrors.Add(1)
			}
			c.invalidate(s)
		}
	}()
	interval := c.cfg.PollMin
	visible := ""
	first := false
	heartbeat := time.Now()
	retries := 0
	usageKnown := false
	for {
		if env.failed() {
			status := 502
			if env.Response != nil {
				switch env.Response.Payload.HTTPStatus {
				case 401, 403, 429:
					status = env.Response.Payload.HTTPStatus
				}
			}
			if status != 401 && status != 403 && status != 429 {
				c.invalidate(s)
			}
			return result, &Error{status, env.failureCode(), ""}
		}
		text, calls := env.output()
		if done && env.Response != nil {
			for _, it := range env.Response.Payload.Output {
				if it.Type == "refusal" {
					c.invalidate(s)
					return result, &Error{502, "unsupported_refusal_output", ""}
				}
				for _, block := range it.Content {
					if block.Type == "refusal" {
						c.invalidate(s)
						return result, &Error{502, "unsupported_refusal_output", ""}
					}
				}
			}
		}
		if env.Usage != nil {
			result.Usage = *env.Usage
			usageKnown = true
		}
		if env.Response != nil && env.Response.Payload.Usage != nil {
			result.Usage = *env.Response.Payload.Usage
			usageKnown = true
		}
		if emit != nil && len(r.Tools) == 0 && (text != "" || done && visible != "") {
			if !strings.HasPrefix(text, visible) {
				return result, &Error{502, "non_monotonic_output", ""}
			}
			if delta := text[len(visible):]; delta != "" {
				if e = emit(Event{Text: delta}); e != nil {
					return result, e
				}
				visible = text
				interval = c.cfg.PollMin
				if !first {
					first = true
					c.metrics.TTFTCount.Add(1)
					c.metrics.TTFTMicros.Add(uint64(time.Since(started).Microseconds()))
				}
			}
		}
		if done {
			if env.Response == nil || env.Response.Status != "success" || env.Response.Payload.Output == nil {
				c.invalidate(s)
				return result, &Error{502, "missing_terminal_payload", ""}
			}
			result.ID = r.ResponseID
			if result.ID == "" {
				result.ID = "resp_prism_" + uuid.NewString()
			}
			result.Text = text
			result.Text, result.Calls, e = parseTools(text, calls, r)
			if e != nil {
				return result, e
			}
			if result.Text == "" && len(result.Calls) == 0 {
				c.invalidate(s)
				return result, &Error{502, "empty_or_unsupported_output", ""}
			}
			if emit != nil && len(r.Tools) > 0 && result.Text != "" {
				if e = emit(Event{Text: result.Text}); e != nil {
					return result, e
				}
			}
			if emit != nil && !first && (result.Text != "" || len(result.Calls) > 0) {
				c.metrics.TTFTCount.Add(1)
				c.metrics.TTFTMicros.Add(uint64(time.Since(started).Microseconds()))
			}
			if result.Usage.InputTokens < 0 || result.Usage.OutputTokens < 0 || result.Usage.InputTokensDetails.CachedTokens < 0 || result.Usage.InputTokensDetails.CachedTokens > result.Usage.InputTokens {
				return result, &Error{502, "invalid_usage", ""}
			}
			if !usageKnown {
				b, _ := json.Marshal(foldTools(upstream))
				result.Usage.InputTokens = EstimateTokens(b)
				result.Usage.OutputTokens = EstimateTokens([]byte(result.Text))
				if len(result.Calls) > 0 {
					b, _ = json.Marshal(result.Calls)
					result.Usage.OutputTokens += EstimateTokens(b)
				}
				result.Estimated = true
				c.metrics.EstimatedUsage.Add(1)
			}
			result.Usage.TotalTokens = result.Usage.InputTokens + result.Usage.OutputTokens
			c.metrics.PromptTokens.Add(uint64(result.Usage.InputTokens))
			c.metrics.CompletionTokens.Add(uint64(result.Usage.OutputTokens))
			c.metrics.meter(0, uint64(result.Usage.TotalTokens))
			c.metrics.Completed.Add(1)
			if u, parseErr := url.Parse(c.cfg.BaseURL); parseErr == nil && u.Scheme == "https" && u.Hostname() == "prism.openai.com" {
				c.metrics.LiveVerifiedAt.Store(time.Now().Unix())
			}
			if r.Store {
				history := append([]Item(nil), input...)
				if result.Text != "" {
					history = append(history, textItem("assistant", result.Text))
				}
				history = append(history, result.Calls...)
				if env.Response.Payload.ConversationID != "" {
					conversationID = env.Response.Payload.ConversationID
				}
				cont := &continuity{SlotID: s.id, ProjectID: s.project, SandboxGeneration: s.generation, ConversationID: conversationID, UpstreamID: env.Response.Payload.ID}
				// The runtime snapshot includes codex_session_id/last_turn_id, which opaque
				// poll state alone does not necessarily expose. Failures never erase history.
				cont.Snapshot = snapshot
				var debug struct {
					Snapshot map[string]any `json:"snapshot"`
				}
				debugCtx, debugCancel := context.WithTimeout(ctx, min(5*time.Second, c.cfg.HTTPTimeout))
				if snapshotSession(cont.Snapshot) == "" && c.json(debugCtx, s, http.MethodGet, "/api/codex/runtime/debug?conversation_id="+url.QueryEscape(conversationID), nil, &debug, "") == nil && snapshotSession(debug.Snapshot) != "" {
					cont.Snapshot = debug.Snapshot
				}
				if snapshotSession(cont.Snapshot) != "" {
					c.metrics.ContinuitySnapshots.Add(1)
				} else {
					c.metrics.ContinuitySnapshotMissing.Add(1)
				}
				debugCancel()
				if writeTokens, ok := c.cache.putTurn(owner, result.ID, history, cont); ok {
					result.CacheWriteTokens = writeTokens
				}
			}
			return result, nil
		}
		if requestID == "" || !validState(state) {
			return result, &Error{502, "missing_turn_state", ""}
		}
		if emit != nil && time.Since(heartbeat) >= c.cfg.StreamKeepalive {
			if e = emit(Event{Keepalive: true}); e != nil {
				return result, e
			}
			heartbeat = time.Now()
		}
		if e = sleep(ctx, interval); e != nil {
			return result, e
		}
		var next envelope
		c.metrics.Polls.Add(1)
		e = c.json(ctx, s, http.MethodPost, PathStatus, map[string]any{"request_id": requestID, "turn_state": state}, &next, "")
		if e != nil {
			var ae *Error
			if errors.As(e, &ae) && (ae.Status >= 500 || ae.Status == 408) && retries < 2 {
				retries++
				interval = c.cfg.PollMax
				continue
			}
			return result, e
		}
		retries = 0
		if next.RequestID != "" && next.RequestID != requestID {
			return result, &Error{502, "request_id_changed", ""}
		}
		if validState(next.TurnState) {
			state = next.TurnState
		}
		snapshot = responseSnapshot(next, snapshot)
		env = next
		done = env.Status == "completed" || env.failed()
		interval = min(interval*2, c.cfg.PollMax)
	}
}
