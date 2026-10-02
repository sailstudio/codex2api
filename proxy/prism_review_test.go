package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/prismchannel"
	"github.com/gin-gonic/gin"
)

func TestPrismReviewAccountPoliciesFailClosed(t *testing.T) {
	m := prismchannel.NewMockUpstream()
	m.Immediate = true
	_, h := prismTestServer(t, m, false)
	for _, tc := range []struct {
		name string
		row  database.APIKeyRow
	}{
		{"group", database.APIKeyRow{AllowedGroupIDs: []int64{999}}},
		{"plan", database.APIKeyRow{Limits: database.APIKeyLimits{PlanAllow: []string{"not-a-real-plan"}}}},
		{"no_affinity_group", database.APIKeyRow{Limits: database.APIKeyLimits{NoAffinityGroupIDs: []int64{999}}}},
		{"scope_skip", database.APIKeyRow{Limits: database.APIKeyLimits{ScopeLimits: []database.APIKeyScopeLimit{{ScopeID: 999, Token1d: 1}}}}},
		{"scope_reject", database.APIKeyRow{Limits: database.APIKeyLimits{ScopeLimits: []database.APIKeyScopeLimit{{ScopeID: 999, Token1d: 1, OnExhausted: database.APIKeyScopeOnExhaustedReject}}}}},
		{"scope_concurrency", database.APIKeyRow{Limits: database.APIKeyLimits{ScopeLimits: []database.APIKeyScopeLimit{{ScopeID: 999, MaxConcurrency: 1}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, responses := range []bool{false, true} {
				for _, force := range []bool{false, true} {
					h.cfg.Prism.Force = force
					w := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(w)
					c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
					c.Set(contextAPIKeyRow, &tc.row)
					body := []byte(`{"model":"prism-test","input":"hi","messages":[{"role":"user","content":"hi"}]}`)
					if !h.handlePrism(c, body, responses) || w.Code != 403 || m.Starts.Load() != 0 {
						t.Fatalf("policy bypass: status=%d starts=%d", w.Code, m.Starts.Load())
					}
				}
			}
		})
	}
}

// A deadline-capable writer models blocked socket writes/flushes without network
// buffers or secrets. SetWriteDeadline must be able to wake the active operation.
type prismDeadlineWriter struct {
	*httptest.ResponseRecorder
	mu                     sync.Mutex
	deadline               time.Time
	changed                chan struct{}
	entered                chan struct{}
	once                   sync.Once
	blockWrite, blockFlush bool
	shouldBlock            func([]byte) bool
	blockCurrent           bool
}

func (w *prismDeadlineWriter) SetWriteDeadline(d time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadline = d
	close(w.changed)
	w.changed = make(chan struct{})
	return nil
}
func (w *prismDeadlineWriter) block() error {
	w.once.Do(func() { close(w.entered) })
	for {
		w.mu.Lock()
		d, ch := w.deadline, w.changed
		w.mu.Unlock()
		if d.IsZero() {
			<-ch
			continue
		}
		timer := time.NewTimer(time.Until(d))
		select {
		case <-ch:
			timer.Stop()
		case <-timer.C:
			return errors.New("mock write deadline exceeded")
		}
	}
}
func (w *prismDeadlineWriter) Write(p []byte) (int, error) {
	w.blockCurrent = w.shouldBlock == nil || w.shouldBlock(p)
	if w.blockWrite && w.blockCurrent {
		return 0, w.block()
	}
	return w.ResponseRecorder.Write(p)
}
func (w *prismDeadlineWriter) FlushError() error {
	if w.blockFlush && w.blockCurrent {
		return w.block()
	}
	w.ResponseRecorder.Flush()
	return nil
}
func (w *prismDeadlineWriter) Flush() { _ = w.FlushError() }

func TestPrismReviewBlockedSSECancellation(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		flush, active, heartbeat bool
	}{
		{"initial_write", false, false, false}, {"initial_flush", true, false, false},
		{"text_write", false, true, false}, {"heartbeat_flush", true, true, true},
	} {
		name, flush := tc.name, tc.flush
		t.Run(name, func(t *testing.T) {
			m := prismchannel.NewMockUpstream()
			m.PollCount = 100
			if tc.heartbeat {
				m.PollDelay = 200 * time.Millisecond
			}
			_, h := prismTestServer(t, m, false)
			h.cfg.Prism.StreamKeepalive = 5 * time.Millisecond
			w := &prismDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), changed: make(chan struct{}), entered: make(chan struct{}), blockWrite: !flush, blockFlush: flush}
			if tc.active {
				w.shouldBlock = func(p []byte) bool {
					return m.Starts.Load() > 0 && (!tc.heartbeat || strings.Contains(string(p), ": prism keepalive"))
				}
			}
			c, _ := gin.CreateTestContext(w)
			c.Set(contextAPIKeyID, int64(91919))
			c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 91919, Limits: database.APIKeyLimits{MaxConcurrency: 1}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			done := make(chan struct{})
			go func() {
				defer close(done)
				h.handlePrism(c, []byte(`{"model":"prism-test","input":"hi","stream":true}`), true)
			}()
			select {
			case <-w.entered:
			case <-time.After(time.Second):
				cancel()
				t.Fatal("writer not entered")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				_ = w.SetWriteDeadline(time.Now())
				<-done
				t.Fatal("cancellation did not interrupt SSE")
			}
			if h.prismClient.Metrics().Inflight.Load() != 0 || m.Active.Load() != 0 || atomic.LoadInt64(&h.apiKeyConcurrencyLimiter().counter(91919).inflight) != 0 {
				t.Fatal("canceled stream leaked slot, key permit or upstream task")
			}
			if tc.active && m.Stops.Load() != 1 {
				t.Fatal("active upstream was not stopped")
			}
		})
	}
}

func TestPrismReviewCacheUsageSeparatesLocalEstimate(t *testing.T) {
	for _, responses := range []bool{false, true} {
		for _, provider := range []int{0, 7} {
			var usage prismchannel.Usage
			if err := json.Unmarshal([]byte(fmt.Sprintf(`{"input_tokens":24,"output_tokens":8,"input_tokens_details":{"cached_tokens":%d}}`, provider)), &usage); err != nil {
				t.Fatal(err)
			}
			u := prismUsage(prismchannel.Result{Usage: usage, CacheReadTokens: 542, CacheWriteTokens: 600}, responses)
			key := "prompt_tokens_details"
			if responses {
				key = "input_tokens_details"
			}
			if u[key].(gin.H)["cached_tokens"] != int64(provider) {
				t.Fatal("provider cached_tokens mixed with local history estimate")
			}
			if u["prism_cache_read_tokens"] != int64(542) || u["prism_cache_write_tokens"] != int64(600) || u["prism_cache_estimated"] != true {
				t.Fatal("local estimates must remain separately labeled")
			}
		}
	}
}

func TestPrismReviewAuthenticatedAccountPolicies(t *testing.T) {
	m := prismchannel.NewMockUpstream()
	m.Immediate = true
	srv, h := prismTestServer(t, m, false)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "prism-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	h.db = db
	for i, options := range []database.APIKeyInput{
		{AllowedGroupIDs: []int64{999}},
		{Limits: database.APIKeyLimits{PlanAllow: []string{"impossible-plan"}}},
		{Limits: database.APIKeyLimits{ScopeLimits: []database.APIKeyScopeLimit{{ScopeType: database.APIKeyScopeTypeAccount, ScopeID: 999, Token1d: 1}}}},
		{Limits: database.APIKeyLimits{ScopeLimits: []database.APIKeyScopeLimit{{ScopeType: database.APIKeyScopeTypeGroup, ScopeID: 999, MaxConcurrency: 1}}}},
	} {
		options.Name = "mock restricted key"
		options.Key = fmt.Sprintf("mock-prism-restricted-%d", i)
		if _, err := db.InsertAPIKeyWithOptions(context.Background(), options); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/v1/responses", "/v1/chat/completions"} {
			code, _ := prismPOST(t, srv, path, `{"model":"prism-test","input":"hi","messages":[{"role":"user","content":"hi"}]}`, options.Key)
			if code != 403 || m.Starts.Load() != 0 {
				t.Fatalf("authenticated policy bypass: code=%d starts=%d", code, m.Starts.Load())
			}
		}
	}
}

func TestPrismReviewSSEWriteBudgetAndUnsupportedWriter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout bool
	}{{"write_budget", false}, {"request_budget", true}} {
		t.Run(tc.name, func(t *testing.T) {
			w := &prismDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), changed: make(chan struct{}), entered: make(chan struct{}), blockWrite: true}
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			stream := newPrismStream(c, prismchannel.Request{}, true)
			stream.writeTimeout = 20 * time.Millisecond
			if tc.timeout {
				ctx, cancel := context.WithTimeout(c.Request.Context(), 20*time.Millisecond)
				defer cancel()
				stream.ctx = ctx
				stream.writeTimeout = time.Second
			}
			done := make(chan error, 1)
			go func() { done <- stream.Emit(prismchannel.Event{Keepalive: true}) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("blocked write succeeded")
				}
			case <-time.After(time.Second):
				_ = w.SetWriteDeadline(time.Now())
				<-done
				t.Fatal("write budget did not interrupt blocked socket")
			}
		})
	}
	m := prismchannel.NewMockUpstream()
	_, h := prismTestServer(t, m, false)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	h.handlePrism(c, []byte(`{"model":"prism-test","input":"hi","stream":true}`), true)
	if w.Code != 503 || m.Starts.Load() != 0 || h.prismClient.Metrics().Inflight.Load() != 0 {
		t.Fatal("unsupported streaming writer admitted upstream")
	}
}
