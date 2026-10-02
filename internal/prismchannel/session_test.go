package prismchannel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrismMaterialProviderRefreshAndFreshTickets(t *testing.T) {
	m := NewMockUpstream()
	m.mu.Lock()
	m.boxes["mock-material-box"] = &mockBox{resource: true, y: true}
	m.mu.Unlock()
	var seenMu sync.Mutex
	seen := map[string]bool{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ticket := r.Header.Get("openai-sentinel-token")
		seenMu.Lock()
		duplicate := seen[ticket]
		seen[ticket] = true
		seenMu.Unlock()
		if ticket == "" || duplicate || r.Header.Get("Cookie") != "prism_session_token=mock-provider-session; prism_oai_access_token=mock-provider-access" {
			t.Error("missing/reused verification ticket or provider cookies")
			http.Error(w, "fixture rejected", 403)
			return
		}
		if r.URL.Path == PathStart {
			b, e := io.ReadAll(r.Body)
			if e != nil {
				t.Error(e)
			}
			r.Body = io.NopCloser(strings.NewReader(string(b)))
			if !strings.Contains(string(b), "captured_snapshot") || !strings.Contains(string(b), "mock-material-conversation") {
				t.Error("browser metadata lost")
			}
		}
		m.ServeHTTP(w, r)
	}))
	defer up.Close()
	var prepares, headers atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mock-provider-auth" {
			http.Error(w, "fixture rejected", 401)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "mock-session-only") || strings.Contains(string(b), "mock-access-only") {
			t.Error("credentials sent to provider")
		}
		var req struct {
			Operation string `json:"operation"`
			AccountID string `json:"account_id"`
			SlotID    string `json:"slot_id"`
			OwnerHash string `json:"owner_hash"`
		}
		if json.Unmarshal(b, &req) != nil || req.AccountID == "" || req.SlotID == "" || len(req.OwnerHash) != 64 {
			t.Error("provider scope missing")
			http.Error(w, "scope missing", 400)
			return
		}
		if req.Operation == "prepare" {
			prepares.Add(1)
			mockJSON(w, materialResponse{Metadata: map[string]any{"projectId": "mock-material-project", "userId": "mock-user", "sandbox_url": up.URL + "/s/sandboxes/proxy", "sandbox_token": "mock-material-box", "codex_listen_snapshot": map[string]any{"captured_snapshot": true}}, ConversationID: "mock-material-conversation", ExpiresAt: time.Now().Add(time.Minute).Unix()})
		} else {
			ticket := headers.Add(1)
			mockJSON(w, materialResponse{Headers: map[string]string{"Cookie": "prism_session_token=mock-provider-session; prism_oai_access_token=mock-provider-access", "openai-sentinel-token": string(rune('a' + ticket)), "User-Agent": "mock-browser-fixture"}})
		}
	}))
	defer provider.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = up.URL
	cfg.MaterialURL = provider.URL
	cfg.MaterialBearer = "mock-provider-auth"
	cfg.Credentials = []Credential{{ID: "fixture-account"}}
	cfg.AccountConcurrency = 1
	cfg.PollMin = time.Millisecond
	cfg.PollMax = 2 * time.Millisecond
	cfg.KeepWarm = time.Hour
	c, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	for _, owner := range []string{"a", "a", "b"} {
		if _, e = c.Run(context.Background(), owner, testRequest(), nil); e != nil {
			t.Fatal(e)
		}
	}
	if prepares.Load() != 2 || headers.Load() != 12 || m.Projects.Load() != 0 || m.Violations.Load() != 0 {
		t.Fatalf("prepare=%d headers=%d projects=%d violations=%d", prepares.Load(), headers.Load(), m.Projects.Load(), m.Violations.Load())
	}
}
func TestPrismMaterialProviderFailsClosedAndRedacts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response materialResponse
		status   int
	}{
		{"unavailable", materialResponse{}, 503},
		{"missing_context", materialResponse{}, 200},
		{"expired", materialResponse{Metadata: map[string]any{"projectId": "mock-project", "sandbox_url": "/s/sandboxes/proxy", "sandbox_token": "mock-box"}, ConversationID: "mock-conv", ExpiresAt: 1}, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status != 200 {
					http.Error(w, "SECRET_MATERIAL_DO_NOT_LEAK", tc.status)
					return
				}
				mockJSON(w, tc.response)
			}))
			defer provider.Close()
			m := NewMockUpstream()
			c := testClient(t, m, func(c *Config) { c.MaterialURL = provider.URL })
			_, e := c.Run(context.Background(), "owner", testRequest(), nil)
			if e == nil || strings.Contains(e.Error(), "SECRET_MATERIAL") || m.Starts.Load() != 0 {
				t.Fatalf("material failed open: %v", e)
			}
		})
	}
	for _, headers := range []map[string]string{{"Host": "evil.invalid"}, {"Cookie": "bad\r\nInjected: value"}, {"Cookie": "mock-cookie"}} {
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mockJSON(w, materialResponse{Headers: headers}) }))
		m := NewMockUpstream()
		c := testClient(t, m, func(c *Config) { c.MaterialURL = provider.URL })
		if _, e := c.materialHeaders(context.Background(), Credential{}, PathStart); e == nil {
			t.Fatal("invalid provider headers accepted")
		}
		provider.Close()
	}
}

func TestPrismNativeContinuityUsesSnapshotAndUpstreamID(t *testing.T) {
	m := NewMockUpstream()
	m.Immediate = true
	var conversation string
	var starts int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathStart {
			m.ServeHTTP(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		var body struct {
			Conversation string                     `json:"conversationId"`
			Previous     string                     `json:"previousResponseId"`
			Input        []Item                     `json:"input"`
			Metadata     map[string]json.RawMessage `json:"metadata"`
		}
		if json.Unmarshal(raw, &body) != nil {
			t.Error("invalid body")
		}
		starts++
		if starts == 1 {
			conversation = body.Conversation
		} else if starts < 4 {
			var encoded string
			var snapshot map[string]any
			_ = json.Unmarshal(body.Metadata["codex_listen_snapshot"], &encoded)
			_ = json.Unmarshal([]byte(encoded), &snapshot)
			if body.Conversation != conversation || body.Previous != "mock-native-response" || len(body.Input) != 2 || snapshotSession(snapshot) != "mock-session-continuity" || snapshot["last_turn_id"] != "mock-turn-continuity" || snapshot["transcript_cursor"] != float64(17) {
				t.Error("upstream continuation lost its snapshot, affinity, response ID, or incremental input")
			}
		} else if body.Conversation == conversation || body.Previous != "" {
			t.Error("fresh request reused a conversation")
		}
		recorder := httptest.NewRecorder()
		m.ServeHTTP(recorder, r)
		var env map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &env)
		payload := env["response"].(map[string]any)["payload"].(map[string]any)
		payload["id"] = "mock-native-response"
		payload["codexListenSnapshot"] = map[string]any{"codex_session_id": "mock-session-continuity", "last_turn_id": "mock-turn-continuity", "transcript_cursor": 17}
		mockJSON(w, env)
	}))
	defer up.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = up.URL
	cfg.Credentials = []Credential{{UserID: "mock-user", SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
	cfg.AccountConcurrency = 2
	cfg.KeepWarm = time.Hour
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := testRequest()
	for i := 0; i < 3; i++ {
		res, err := c.Run(context.Background(), "a", req, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.PreviousID = res.ID
	}
	if c.metrics.ContinuitySnapshots.Load() != 3 || c.metrics.ContinuitySnapshotMissing.Load() != 0 {
		t.Fatal("snapshot signal missing")
	}
	if _, err := c.Run(context.Background(), "a", testRequest(), nil); err != nil {
		t.Fatal(err)
	}
	if c.metrics.LiveVerifiedAt.Load() != 0 {
		t.Fatal("mock incorrectly marked live verified")
	}
}

func TestPrismEmbeddedAuthorizationDenialCoolsAccount(t *testing.T) {
	m := NewMockUpstream()
	m.Immediate = true
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathStart {
			m.ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		m.ServeHTTP(recorder, r)
		var env map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &env)
		response := env["response"].(map[string]any)
		response["status"] = "error"
		response["payload"] = map[string]any{"httpStatus": 403, "reason": "unknown", "message": "SECRET_UPSTREAM_BODY_MUST_NOT_LEAK"}
		mockJSON(w, env)
	}))
	defer up.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = up.URL
	cfg.Credentials = []Credential{{UserID: "mock-user", SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
	cfg.QueueTimeout = 5 * time.Millisecond
	cfg.KeepWarm = time.Hour
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Run(context.Background(), "a", testRequest(), nil)
	var upstream *Error
	if !errors.As(err, &upstream) || upstream.Status != 403 || upstream.Code != "generation_failed_http_403" || strings.Contains(err.Error(), "SECRET_") {
		t.Fatalf("denial not classified/redacted: %v", err)
	}
	if _, err = c.acquire(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("embedded denial did not cool all account slots: %v", err)
	}
	if m.Starts.Load() != 1 || c.metrics.CacheWrites.Load() != 0 || c.metrics.WarmSlots.Load() != 1 {
		t.Fatal("denied start replayed, cached, or discarded a valid warm context")
	}
}
