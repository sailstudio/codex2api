package prismchannel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests adapted from the independent review diagnostics; all material is synthetic.
func TestPrismReviewMaterialPreservesBrowserIdentity(t *testing.T) {
	m := NewMockUpstream()
	m.Immediate = true
	m.boxes["mock-provider-box"] = &mockBox{resource: true, y: true}
	observed := "unset"
	var preserved bool
	var up *httptest.Server
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathStart {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			var b struct {
				Metadata map[string]any `json:"metadata"`
			}
			_ = json.Unmarshal(raw, &b)
			observed, _ = b.Metadata["userId"].(string)
			snapshot, ok := b.Metadata["codex_listen_snapshot"].(map[string]any)
			preserved = ok && snapshot["user_id"] == "mock-user" && snapshot["sandbox_token"] == "mock-provider-box" &&
				b.Metadata["sandbox_url"] == up.URL+"/s/sandboxes/proxy" && b.Metadata["frontend_origin"] == "mock-browser-origin" &&
				b.Metadata["extra_context"] == "mock-extra" && b.Metadata["model"] == "mock-model" && b.Metadata["reasoning_effort"] == "low"
		}
		m.ServeHTTP(w, r)
	}))
	defer up.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Operation string `json:"operation"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.Operation == "prepare" {
			mockJSON(w, materialResponse{Metadata: map[string]any{"projectId": "mock-project", "userId": "mock-user", "sandbox_url": up.URL + "/s/sandboxes/proxy", "sandbox_token": "mock-provider-box", "frontend_origin": "mock-browser-origin", "extra_context": "mock-extra", "model": "captured-model", "reasoning_effort": "high", "codex_listen_snapshot": map[string]any{"user_id": "mock-user", "project_id": "mock-project", "conversation_id": "mock-conversation", "sandbox_url": up.URL + "/s/sandboxes/proxy", "sandbox_token": "mock-provider-box"}}, ConversationID: "mock-conversation", ExpiresAt: time.Now().Add(time.Minute).Unix()})
		} else {
			mockJSON(w, materialResponse{Headers: map[string]string{"Openai-Sentinel-Token": "mock-ticket"}})
		}
	}))
	defer provider.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = up.URL
	cfg.MaterialURL = provider.URL
	cfg.AccountConcurrency = 1
	cfg.KeepWarm = time.Hour
	cfg.Credentials = []Credential{{SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
	c, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	req := testRequest()
	req.Effort = "low"
	if _, e = c.Run(context.Background(), "owner", req, nil); e != nil {
		t.Fatal(e)
	}
	if observed != "mock-user" || !preserved {
		t.Fatal("provider userId was overwritten")
	}
}

func TestPrismReviewHistoricalFilePreserved(t *testing.T) {
	m := NewMockUpstream()
	m.Immediate = true
	c := testClient(t, m, func(cfg *Config) { cfg.AccountConcurrency = 1 })
	r := testRequest()
	r.Input[0].Content = append(r.Input[0].Content, Content{Type: "input_image", ImageURL: pngData(t)})
	first, e := c.Run(context.Background(), "owner", r, nil)
	if e != nil {
		t.Fatal(e)
	}
	q := testRequest()
	q.PreviousID = first.ID
	_, e = c.Run(context.Background(), "owner", q, nil)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(m.Inputs()[1])
	if !strings.Contains(string(b), "input_file") || !strings.Contains(string(b), "prism-uploads") {
		t.Fatal("historical image reference lost")
	}
	if m.Uploads.Load() != 1 {
		t.Fatal("historical image redundantly uploaded")
	}
}

func TestPrismReviewExpiredSandboxRejectsSnapshot(t *testing.T) {
	m := NewMockUpstream()
	m.Immediate = true
	var mismatch bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathStart {
			m.ServeHTTP(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		var b struct {
			Metadata map[string]any `json:"metadata"`
		}
		_ = json.Unmarshal(raw, &b)
		var snapshot map[string]any
		_ = json.Unmarshal([]byte(b.Metadata["codex_listen_snapshot"].(string)), &snapshot)
		if snapshotSession(snapshot) != "" && snapshot["sandbox_token"] != b.Metadata["sandbox_token"] {
			mismatch = true
		}
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, r)
		var env map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		payload := env["response"].(map[string]any)["payload"].(map[string]any)
		snapshot["codex_session_id"] = "mock-session"
		payload["codexListenSnapshot"] = snapshot
		mockJSON(w, env)
	}))
	defer up.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = up.URL
	cfg.AccountConcurrency = 1
	cfg.KeepWarm = time.Hour
	cfg.Credentials = []Credential{{UserID: "mock-user", SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
	c, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	first, e := c.Run(context.Background(), "owner", testRequest(), nil)
	if e != nil {
		t.Fatal(e)
	}
	s := <-c.slots
	s.expires = time.Now().Add(-time.Second)
	c.slots <- s
	q := testRequest()
	q.PreviousID = first.ID
	if _, e = c.Run(context.Background(), "owner", q, nil); e == nil || !strings.Contains(e.Error(), "previous_response_context_expired") {
		t.Fatal("rotated sandbox accepted continuation")
	}
	if mismatch || m.Starts.Load() != 1 {
		t.Fatal("old snapshot reached a rotated sandbox")
	}
}

func TestPrismReviewStrictToolsRejected(t *testing.T) {
	for _, responses := range []bool{false, true} {
		tool := `{"type":"function","name":"lookup","parameters":{"type":"object"},"strict":true}`
		if !responses {
			tool = `{"type":"function","function":{"name":"lookup","parameters":{"type":"object"},"strict":true}}`
		}
		body := `{"model":"prism-test","input":"hi","messages":[{"role":"user","content":"hi"}],"tools":[` + tool + `]}`
		if _, err := Decode([]byte(body), responses); err == nil || !strings.Contains(err.Error(), "strict") {
			t.Fatal("strict tools silently downgraded")
		}
	}
	for _, parameters := range []string{`null`, `[]`, `{"type":"string"}`, `{"type":"object","properties":[]}`, `{"type":"object","required":[42]}`} {
		body := `{"model":"prism-test","input":"hi","tools":[{"type":"function","name":"lookup","parameters":` + parameters + `}]}`
		if _, err := Decode([]byte(body), true); err == nil {
			t.Fatal("malformed tool declaration accepted")
		}
	}
}

func TestPrismReviewTerminalOutputContract(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
		valid              bool
	}{
		{"empty", `[]`, "", false},
		{"unknown", `[{"type":"unknown"}]`, "", false},
		{"refusal", `[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"mock refusal"}]}]`, "", false},
		{"reasoning_only", `[{"type":"reasoning","summary":[]}]`, "", false},
		{"text_variant", `[{"type":"message","content":[{"type":"text","text":"variant answer"}]}]`, "variant answer", true},
		{"top_level_text", `[{"type":"message","role":"assistant","text":"fallback answer"}]`, "fallback answer", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMockUpstream()
			m.Immediate = true
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != PathStart {
					m.ServeHTTP(w, r)
					return
				}
				rec := httptest.NewRecorder()
				m.ServeHTTP(rec, r)
				var env map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &env)
				var output any
				_ = json.Unmarshal([]byte(tc.output), &output)
				env["response"].(map[string]any)["payload"].(map[string]any)["output"] = output
				mockJSON(w, env)
			}))
			defer up.Close()
			cfg := DefaultConfig()
			cfg.BaseURL = up.URL
			cfg.KeepWarm = time.Hour
			cfg.Credentials = []Credential{{UserID: "mock-user", SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
			c, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			result, err := c.Run(context.Background(), "owner", testRequest(), nil)
			if tc.valid {
				if err != nil || result.Text != tc.want {
					t.Fatalf("valid output lost: %v", err)
				}
			} else if err == nil || c.metrics.Completed.Load() != 0 || c.metrics.CacheWrites.Load() != 0 {
				t.Fatal("unsupported terminal output became empty success")
			}
		})
	}
}

func TestPrismReviewSharedPrepareWaitCancellation(t *testing.T) {
	m := NewMockUpstream()
	c := testClient(t, m, func(cfg *Config) { cfg.SandboxPoolSize = 1; cfg.AccountConcurrency = 1 })
	s := <-c.slots
	defer func() { c.slots <- s }()
	s.shared.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.prepare(ctx, s, "owner") }()
	cancel()
	select {
	case err := <-done:
		<-s.shared.gate
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		<-s.shared.gate
		<-done
		t.Fatal("canceled follower waited for leader mutex")
	}
}

func TestPrismReviewMaterialIdentityFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, credentialUser string
		modify               func(map[string]any)
	}{
		{"missing_user", "", func(meta map[string]any) { delete(meta, "userId") }},
		{"credential_mismatch", "different-mock-user", func(map[string]any) {}},
		{"snapshot_mismatch", "", func(meta map[string]any) {
			meta["codex_listen_snapshot"] = map[string]any{"user_id": "different-mock-user"}
		}},
		{"snapshot_sandbox_mismatch", "", func(meta map[string]any) {
			meta["codex_listen_snapshot"] = map[string]any{"sandbox_token": "different-mock-box"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMockUpstream()
			var origin string
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				meta := map[string]any{"projectId": "mock-project", "userId": "mock-user", "sandbox_url": origin + "/s/sandboxes/proxy", "sandbox_token": "mock-box"}
				tc.modify(meta)
				mockJSON(w, materialResponse{Metadata: meta, ConversationID: "mock-conversation", ExpiresAt: time.Now().Add(time.Minute).Unix()})
			}))
			defer provider.Close()
			c := testClient(t, m, func(cfg *Config) {
				origin = cfg.BaseURL
				cfg.MaterialURL = provider.URL
				cfg.Credentials = []Credential{{ID: "mock-account", UserID: tc.credentialUser}}
			})
			if _, err := c.Run(context.Background(), "owner", testRequest(), nil); err == nil || m.Starts.Load() != 0 {
				t.Fatal("inconsistent material reached upstream start")
			}
		})
	}
	for _, headersOnly := range []bool{false, true} {
		cfg := DefaultConfig()
		cfg.Credentials = []Credential{{SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
		if headersOnly {
			cfg.MaterialURL = "http://127.0.0.1/mock-provider"
			cfg.MaterialHeadersOnly = true
		}
		if err := cfg.Validate(); err == nil {
			t.Fatal("locally built context accepted empty user identity")
		}
	}
}

func TestPrismReviewSharedPrepareCanceledFollowerKeepsLeader(t *testing.T) {
	m := NewMockUpstream()
	m.Immediate = true
	entered, release := make(chan struct{}), make(chan struct{})
	var projects atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/projects" && projects.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		m.ServeHTTP(w, r)
	}))
	defer up.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = up.URL
	cfg.KeepWarm = time.Hour
	cfg.AccountConcurrency = 2
	cfg.SandboxPoolSize = 1
	cfg.Credentials = []Credential{{UserID: "mock-user", SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	leader := make(chan error, 1)
	go func() { _, err := c.Run(context.Background(), "owner", testRequest(), nil); leader <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("leader did not prepare")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	follower := make(chan error, 1)
	go func() { _, err := c.Run(ctx, "owner", testRequest(), nil); follower <- err }()
	deadline := time.Now().Add(time.Second)
	for c.metrics.Inflight.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-follower:
		if err != context.Canceled {
			close(release)
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(release)
		<-leader
		<-follower
		t.Fatal("canceled follower blocked on remote preparation")
	}
	if c.metrics.Inflight.Load() != 1 {
		close(release)
		t.Fatal("follower retained slot")
	}
	close(release)
	if err := <-leader; err != nil {
		t.Fatal(err)
	}
	if _, err := c.Run(context.Background(), "owner", testRequest(), nil); err != nil {
		t.Fatal(err)
	}
	if m.Sandboxes.Load() != 1 || c.metrics.Inflight.Load() != 0 {
		t.Fatal("follower cancellation broke shared leader or leaked slots")
	}
}

func TestPrismReviewInvalidationExpiresContinuation(t *testing.T) {
	for _, shared := range []bool{false, true} {
		m := NewMockUpstream()
		m.Immediate = true
		c := testClient(t, m, func(cfg *Config) {
			cfg.AccountConcurrency = 1
			if shared {
				cfg.SandboxPoolSize = 1
			}
		})
		first, err := c.Run(context.Background(), "owner", testRequest(), nil)
		if err != nil {
			t.Fatal(err)
		}
		s := <-c.slots
		c.invalidate(s)
		c.slots <- s
		r := testRequest()
		r.PreviousID = first.ID
		if _, err = c.Run(context.Background(), "owner", r, nil); err == nil || !strings.Contains(err.Error(), "previous_response_context_expired") {
			t.Fatal("invalidated sandbox accepted native continuation")
		}
		if m.Starts.Load() != 1 {
			t.Fatal("expired continuation reached upstream")
		}
	}
}

func TestPrismReviewProviderCacheUsage(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		cached      int64
		valid       bool
	}{
		{"missing", `{"input_tokens":24,"output_tokens":8}`, 0, true},
		{"confirmed", `{"input_tokens":24,"output_tokens":8,"input_tokens_details":{"cached_tokens":7}}`, 7, true},
		{"negative", `{"input_tokens":24,"output_tokens":8,"input_tokens_details":{"cached_tokens":-1}}`, 0, false},
		{"above_input", `{"input_tokens":24,"output_tokens":8,"input_tokens_details":{"cached_tokens":25}}`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMockUpstream()
			m.Immediate = true
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != PathStart {
					m.ServeHTTP(w, r)
					return
				}
				rec := httptest.NewRecorder()
				m.ServeHTTP(rec, r)
				var env map[string]any
				_ = json.Unmarshal(rec.Body.Bytes(), &env)
				var usage any
				_ = json.Unmarshal([]byte(tc.usage), &usage)
				env["response"].(map[string]any)["payload"].(map[string]any)["usage"] = usage
				mockJSON(w, env)
			}))
			defer up.Close()
			cfg := DefaultConfig()
			cfg.BaseURL = up.URL
			cfg.KeepWarm = time.Hour
			cfg.Credentials = []Credential{{UserID: "mock-user", SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
			c, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			result, err := c.Run(context.Background(), "owner", testRequest(), nil)
			if tc.valid {
				if err != nil || result.Estimated || result.Usage.InputTokensDetails.CachedTokens != tc.cached {
					t.Fatalf("provider usage lost: %v", err)
				}
			} else if err == nil || c.metrics.Completed.Load() != 0 || c.metrics.CacheWrites.Load() != 0 {
				t.Fatal("invalid provider cached_tokens accepted")
			}
		})
	}
}

func TestPrismReviewLocalIdentityModes(t *testing.T) {
	for _, mode := range []string{"token_only", "headers_only"} {
		t.Run(mode, func(t *testing.T) {
			m := NewMockUpstream()
			m.Immediate = true
			var identityOK atomic.Bool
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == PathStart {
					raw, _ := io.ReadAll(r.Body)
					r.Body = io.NopCloser(strings.NewReader(string(raw)))
					var body struct {
						Metadata map[string]any `json:"metadata"`
					}
					_ = json.Unmarshal(raw, &body)
					encoded, _ := body.Metadata["codex_listen_snapshot"].(string)
					var snapshot map[string]any
					_ = json.Unmarshal([]byte(encoded), &snapshot)
					identityOK.Store(body.Metadata["userId"] == "mock-user" && snapshot["user_id"] == "mock-user")
				}
				m.ServeHTTP(w, r)
			}))
			defer up.Close()
			var prepares atomic.Int64
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Operation string `json:"operation"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body.Operation == "prepare" {
					prepares.Add(1)
				}
				mockJSON(w, materialResponse{Headers: map[string]string{"Openai-Sentinel-Token": "mock-ticket"}})
			}))
			defer provider.Close()
			cfg := DefaultConfig()
			cfg.BaseURL = up.URL
			cfg.KeepWarm = time.Hour
			cfg.Credentials = []Credential{{UserID: "mock-user", SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
			if mode == "headers_only" {
				cfg.MaterialURL = provider.URL
				cfg.MaterialHeadersOnly = true
			}
			c, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.Run(context.Background(), "owner", testRequest(), nil); err != nil {
				t.Fatal(err)
			}
			if !identityOK.Load() || prepares.Load() != 0 {
				t.Fatal("local identity missing or header provider used to prepare context")
			}
		})
	}
}
