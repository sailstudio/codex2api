package prismchannel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, m *MockUpstream, modify func(*Config)) *Client {
	t.Helper()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	c := DefaultConfig()
	c.BaseURL = srv.URL
	c.Credentials = []Credential{{UserID: "mock-user", SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
	c.PollMin = time.Millisecond
	c.PollMax = 2 * time.Millisecond
	c.KeepWarm = time.Hour
	if modify != nil {
		modify(&c)
	}
	cl, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(cl.Close)
	return cl
}
func testRequest() Request {
	return Request{Model: "prism-mock-model", Input: []Item{textItem("user", "hello")}, Store: true, Parallel: true, ToolChoice: "auto"}
}

func TestPrismProtocolStreamCache(t *testing.T) {
	m := NewMockUpstream()
	m.PollCount = 4
	c := testClient(t, m, nil)
	var text strings.Builder
	r, e := c.Run(context.Background(), "owner-a", testRequest(), func(ev Event) error { text.WriteString(ev.Text); return nil })
	if e != nil {
		t.Fatal(e)
	}
	if text.String() != m.Text || r.Text != m.Text || r.Usage.InputTokens != 24 || r.Estimated || m.Polls.Load() != 4 || m.Violations.Load() != 0 {
		t.Fatalf("result=%+v text=%q polls=%d violations=%d", r, text.String(), m.Polls.Load(), m.Violations.Load())
	}
	q := testRequest()
	q.PreviousID = r.ID
	q.Input = []Item{textItem("user", "followup")}
	next, e := c.Run(context.Background(), "owner-a", q, nil)
	if e != nil {
		t.Fatal(e)
	}
	if next.CacheReadTokens == 0 || c.metrics.CacheHits.Load() != 1 || c.metrics.CacheWriteTokens.Load() == 0 || c.metrics.CacheReadBytes.Load() == 0 {
		t.Fatal("cache counters missing")
	}
	inputs := m.Inputs()
	if len(inputs[1]) != 2 || !strings.Contains(inputs[1][0].Content[0].Text, "[assistant]") {
		t.Fatalf("history not restored: %+v", inputs)
	}
	_, e = c.Run(context.Background(), "owner-b", q, nil)
	var ae *Error
	if !errors.As(e, &ae) || ae.Code != "previous_response_unavailable" || c.metrics.CacheMisses.Load() != 1 || m.Starts.Load() != 2 {
		t.Fatalf("cross-owner cache admitted: %v", e)
	}
	var metrics bytes.Buffer
	c.metrics.Write(&metrics, c.cache)
	for _, s := range []string{"prism_cache_hits 1", "prism_cache_misses 1", "prism_completion_tokens 16", "prism_rpm 3", "prism_ttft_seconds_count 1"} {
		if !strings.Contains(metrics.String(), s) {
			t.Fatalf("metric absent: %s\n%s", s, metrics.String())
		}
	}
}
func TestPrismToolsRoundTrip(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(map[bool]string{false: "envelope", true: "native"}[native], func(t *testing.T) {
			m := NewMockUpstream()
			m.ToolName = "weather"
			m.NativeCall = native
			c := testClient(t, m, nil)
			r := testRequest()
			r.Tools = []Tool{{Type: "function", Name: "weather", Parameters: json.RawMessage(`{"type":"object"}`)}}
			var streamed strings.Builder
			out, e := c.Run(context.Background(), "owner", r, func(ev Event) error { streamed.WriteString(ev.Text); return nil })
			if e != nil {
				t.Fatal(e)
			}
			if len(out.Calls) != 1 || out.Calls[0].Name != "weather" || out.Calls[0].CallID == "" || streamed.Len() != 0 {
				t.Fatalf("tool envelope leaked or call missing: %+v / %q", out, streamed.String())
			}
			r.PreviousID = out.ID
			r.Input = []Item{{Type: "function_call_output", CallID: out.Calls[0].CallID, Output: "sunny"}}
			_, e = c.Run(context.Background(), "owner", r, nil)
			if e != nil {
				t.Fatal(e)
			}
			b, _ := json.Marshal(m.Inputs())
			if !bytes.Contains(b, []byte("sunny")) || !bytes.Contains(b, []byte(out.Calls[0].CallID)) {
				t.Fatal("tool call/result history lost")
			}
		})
	}
}
func TestPrismCancelStopsLatestState(t *testing.T) {
	m := NewMockUpstream()
	m.PollCount = 1000
	c := testClient(t, m, nil)
	ctx, cancel := context.WithCancel(context.Background())
	_, e := c.Run(ctx, "owner", testRequest(), func(ev Event) error {
		if ev.Text != "" {
			cancel()
		}
		return nil
	})
	if !errors.Is(e, context.Canceled) || m.Stops.Load() != 1 || m.Active.Load() != 0 || c.metrics.Inflight.Load() != 0 || c.metrics.Stops.Load() != 1 {
		t.Fatalf("cancel=%v stop=%d active=%d", e, m.Stops.Load(), m.Active.Load())
	}
}
func TestPrismImmediateFailuresAndEstimatedUsage(t *testing.T) {
	for _, tc := range []struct {
		name      string
		setup     func(*MockUpstream)
		code      string
		estimated bool
	}{
		{"immediate", func(m *MockUpstream) { m.Immediate = true }, "", false},
		{"business_failure", func(m *MockUpstream) { m.Immediate = true; m.Fail = true }, "generation_failed", false},
		{"missing_state", func(m *MockUpstream) { m.MissingState = true }, "missing_turn_state", false},
		{"estimated", func(m *MockUpstream) { m.NoUsage = true }, "", true},
		{"rate_limit", func(m *MockUpstream) { m.HTTPStatus = 429 }, "upstream_http", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMockUpstream()
			tc.setup(m)
			c := testClient(t, m, nil)
			r, e := c.Run(context.Background(), "owner", testRequest(), nil)
			if tc.code != "" {
				var ae *Error
				if !errors.As(e, &ae) || ae.Code != tc.code {
					t.Fatalf("error=%v", e)
				}
				if c.metrics.CacheWrites.Load() != 0 {
					t.Fatal("failed request cached")
				}
			} else if e != nil || r.Estimated != tc.estimated || r.Usage.TotalTokens < 1 {
				t.Fatalf("result=%+v error=%v", r, e)
			}
		})
	}
}
func TestPrismConcurrencyAndAdmission(t *testing.T) {
	m := NewMockUpstream()
	m.PollDelay = 10 * time.Millisecond
	c := testClient(t, m, func(c *Config) { c.AccountConcurrency = 2; c.MaxWaiters = 64 })
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := c.Run(context.Background(), "owner", testRequest(), nil); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if m.MaxActive.Load() != 2 || m.Projects.Load() != 2 || m.Sandboxes.Load() != 2 || m.Violations.Load() != 0 || c.metrics.Inflight.Load() != 0 || c.metrics.Queued.Load() != 0 {
		t.Fatalf("max_active=%d projects=%d sandboxes=%d violations=%d", m.MaxActive.Load(), m.Projects.Load(), m.Sandboxes.Load(), m.Violations.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s1, e := c.acquire(ctx)
	if e != nil {
		t.Fatal(e)
	}
	s2, e := c.acquire(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { c.slots <- s1; c.slots <- s2 }()
	c.cfg.QueueTimeout = 5 * time.Millisecond
	_, e = c.acquire(ctx)
	if !errors.Is(e, ErrBusy) {
		t.Fatalf("queue timeout=%v", e)
	}
	c.waiters.Store(int64(c.cfg.MaxWaiters))
	_, e = c.acquire(ctx)
	if !errors.Is(e, ErrBusy) {
		t.Fatalf("waiter limit=%v", e)
	}
	c.waiters.Store(0)
}
func TestPrismOwnerProjectIsolationAndWarmReuse(t *testing.T) {
	m := NewMockUpstream()
	m.Immediate = true
	c := testClient(t, m, func(c *Config) { c.AccountConcurrency = 1 })
	for _, owner := range []string{"a", "a", "b"} {
		if _, e := c.Run(context.Background(), owner, testRequest(), nil); e != nil {
			t.Fatal(e)
		}
	}
	if m.Projects.Load() != 2 || m.Sandboxes.Load() != 2 {
		t.Fatalf("projects=%d sandboxes=%d", m.Projects.Load(), m.Sandboxes.Load())
	}
	c.cfg.SandboxTTL = time.Millisecond
	s := <-c.slots
	s.expires = time.Now().Add(-time.Second)
	c.slots <- s
	if _, e := c.Run(context.Background(), "b", testRequest(), nil); e != nil {
		t.Fatal(e)
	}
	if m.Projects.Load() != 2 || m.Sandboxes.Load() != 3 {
		t.Fatal("expiry did not refresh sandbox")
	}
}

func TestPrismAccountCooldownSkipsAllSlotsOfLimitedAccount(t *testing.T) {
	m := NewMockUpstream()
	m.Immediate = true
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, _ := r.Cookie("prism_session_token")
		if r.URL.Path == PathStart && cookie != nil && cookie.Value == "mock-account-a" {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "mock rate limited", 429)
			return
		}
		m.ServeHTTP(w, r)
	}))
	defer up.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = up.URL
	cfg.AccountConcurrency = 2
	cfg.KeepWarm = time.Hour
	cfg.Credentials = []Credential{{ID: "a", UserID: "mock-user-a", SessionToken: "mock-account-a", AccessToken: "mock-access-a"}, {ID: "b", UserID: "mock-user-b", SessionToken: "mock-account-b", AccessToken: "mock-access-b"}}
	c, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	_, e = c.Run(context.Background(), "owner", testRequest(), nil)
	var ae *Error
	if !errors.As(e, &ae) || ae.Status != 429 {
		t.Fatalf("limited account result: %v", e)
	}
	for i := 0; i < 4; i++ {
		if _, e = c.Run(context.Background(), "owner", testRequest(), nil); e != nil {
			t.Fatalf("healthy account not selected: %v", e)
		}
	}
	for i := 0; i < c.Capacity(); i++ {
		s := <-c.slots
		if s.credential.ID == "a" && time.Until(time.Unix(0, s.account.unavailableUntil.Load())) < 50*time.Second {
			t.Error("cooldown was not shared or Retry-After lost")
		}
		c.slots <- s
	}
	if m.Starts.Load() != 4 || m.Violations.Load() != 0 {
		t.Fatalf("starts=%d violations=%d", m.Starts.Load(), m.Violations.Load())
	}
}

func TestPrismDeletedProjectRecoversOnNextTurn(t *testing.T) {
	m := NewMockUpstream()
	m.Immediate = true
	var resources atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/projects/") && strings.HasSuffix(r.URL.Path, "/sandbox/resources-token") && resources.Add(1) == 1 {
			http.Error(w, "mock deleted project", 404)
			return
		}
		m.ServeHTTP(w, r)
	}))
	defer up.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = up.URL
	cfg.Credentials = []Credential{{UserID: "mock-user", SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
	cfg.AccountConcurrency = 1
	cfg.KeepWarm = time.Hour
	c, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	_, e = c.Run(context.Background(), "owner", testRequest(), nil)
	var ae *Error
	if !errors.As(e, &ae) || ae.Status != 404 {
		t.Fatalf("deleted project error=%v", e)
	}
	if _, e = c.Run(context.Background(), "owner", testRequest(), nil); e != nil {
		t.Fatal(e)
	}
	if m.Projects.Load() != 2 || m.Starts.Load() != 1 || m.Violations.Load() != 0 {
		t.Fatal("deleted project stayed pinned")
	}
}
func pngData(t *testing.T) string {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 1, 1))
	im.Set(0, 0, color.White)
	var b bytes.Buffer
	if e := png.Encode(&b, im); e != nil {
		t.Fatal(e)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b.Bytes())
}
func TestPrismImageUploadAndParse(t *testing.T) {
	m := NewMockUpstream()
	c := testClient(t, m, nil)
	r := testRequest()
	r.Input[0].Content = append(r.Input[0].Content, Content{Type: "input_image", ImageURL: pngData(t)})
	if _, e := c.Run(context.Background(), "owner", r, nil); e != nil {
		t.Fatal(e)
	}
	if m.Uploads.Load() != 1 || c.metrics.Uploads.Load() != 1 || c.metrics.UploadStorageReferences.Load() != 1 || c.metrics.UploadURLs.Load() != 0 {
		t.Fatal("upload not counted")
	}
	blocks := m.Inputs()[0][0].Content
	if blocks[1].Type != "input_file" || !strings.HasPrefix(blocks[1].ProjectPath, "/prism-uploads/image_") || blocks[1].ImageURL != "" {
		t.Fatalf("image not rewritten: %+v", blocks)
	}
	for _, raw := range []string{"data:image/png;base64,!!", "data:image/png;base64,aGVsbG8=", "https://127.0.0.1/private", "file:///etc/passwd"} {
		if _, _, e := c.imageData(context.Background(), raw); e == nil {
			t.Fatalf("bad image accepted: %s", raw)
		}
	}
	c.cfg.ImageBytes = 1
	if _, _, e := c.imageData(context.Background(), pngData(t)); e == nil {
		t.Fatal("oversize image accepted")
	}
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "192.168.1.1", "169.254.169.254", "::1", "fc00::1", "0.0.0.0", "100.64.0.1"} {
		if publicImageIP(net.ParseIP(ip)) {
			t.Fatalf("unsafe IP admitted: %s", ip)
		}
	}
}
func TestPrismHTTPRedactionRedirectAndSize(t *testing.T) {
	targetHits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits++ }))
	defer target.Close()
	for _, tc := range []string{"redirect", "body", "error"} {
		t.Run(tc, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch tc {
				case "redirect":
					http.Redirect(w, r, target.URL, 302)
				case "body":
					_, _ = io.Copy(w, io.LimitReader(zeroReader{}, 8<<20+1))
				case "error":
					http.Error(w, "SECRET_COOKIE_MUST_NOT_LEAK", 401)
				}
			}))
			defer up.Close()
			cfg := DefaultConfig()
			cfg.BaseURL = up.URL
			cfg.Credentials = []Credential{{UserID: "mock-user", SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
			c, e := New(cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			_, e = c.do(context.Background(), cfg.Credentials[0], "GET", "/check", nil, "", "")
			if e == nil || strings.Contains(e.Error(), "SECRET_COOKIE") {
				t.Fatalf("bad redaction/size check: %v", e)
			}
		})
	}
	if targetHits != 0 {
		t.Fatal("upstream redirected credentials")
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

func TestPrismCacheBoundsExpiryAndCopies(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CacheEntries = 1
	cfg.CacheBytes = 4096
	cfg.CacheTTL = time.Millisecond
	m := &Metrics{}
	c := newTokenCache(cfg, m)
	items := []Item{textItem("user", "first")}
	if !c.Put("a", "1", items) {
		t.Fatal("put failed")
	}
	items[0].Content[0].Text = "mutated"
	read, _, ok := c.Get("a", "1")
	if !ok || read[0].Content[0].Text != "first" {
		t.Fatal("cache aliases writer")
	}
	read[0].Content[0].Text = "mutated read"
	again, _, _ := c.Get("a", "1")
	if again[0].Content[0].Text != "first" {
		t.Fatal("cache aliases reader")
	}
	if !c.Put("a", "2", []Item{textItem("user", "second")}) || m.CacheEvictions.Load() != 1 {
		t.Fatal("entry bound ignored")
	}
	time.Sleep(3 * time.Millisecond)
	if _, _, ok = c.Get("a", "2"); ok {
		t.Fatal("expired cache hit")
	}
	if n, b := c.Size(); n != 0 || b != 0 {
		t.Fatal("expired bytes retained")
	}
	if c.Put("a", "huge", []Item{textItem("user", strings.Repeat("x", 2048))}) || m.CacheSkipped.Load() != 1 {
		t.Fatal("entry byte bound ignored")
	}
}

func TestPrismCacheConcurrentOwnersAndEviction(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CacheBytes = 4096
	cfg.CacheEntries = 4
	m := &Metrics{}
	c := newTokenCache(cfg, m)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := strconv.Itoa(i)
			for j := 0; j < 200; j++ {
				if !c.Put(owner, "same-id", []Item{textItem("user", owner)}) {
					t.Error("bounded write rejected")
				}
				if items, _, ok := c.Get(owner, "same-id"); ok && items[0].Content[0].Text != owner {
					t.Error("concurrent owner context mixed")
				}
			}
		}(i)
	}
	wg.Wait()
	if m.CacheHits.Load()+m.CacheMisses.Load() != 1600 || m.CacheWrites.Load() != 1600 {
		t.Fatal("concurrent cache counters lost updates")
	}
	if n, b := c.Size(); n > 4 || b > 4096 {
		t.Fatal("concurrent cache admission exceeded bounds")
	}
}

func BenchmarkPrismWarmTurn(b *testing.B) {
	m := NewMockUpstream()
	m.Immediate = true
	up := httptest.NewServer(m)
	defer up.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = up.URL
	cfg.Credentials = []Credential{{UserID: "mock-user", SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
	cfg.AccountConcurrency = 16
	cfg.MaxWaiters = 4096
	cfg.KeepWarm = time.Hour
	c, e := New(cfg)
	if e != nil {
		b.Fatal(e)
	}
	defer c.Close()
	r := testRequest()
	r.Store = false
	for i := 0; i < c.Capacity(); i++ {
		if _, e = c.Run(context.Background(), "bench", r, nil); e != nil {
			b.Fatal(e)
		}
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, e := c.Run(context.Background(), "bench", r, nil); e != nil {
				b.Error(e)
			}
		}
	})
}

func TestPrismSharedSandboxPrewarmAndOwnerIsolation(t *testing.T) {
	m := NewMockUpstream()
	m.PollDelay = 10 * time.Millisecond
	c := testClient(t, m, func(cfg *Config) { cfg.AccountConcurrency = 10; cfg.SandboxPoolSize = 2; cfg.PrewarmSlots = 10 })
	deadline := time.Now().Add(time.Second)
	for c.metrics.WarmSlots.Load() != 10 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if c.metrics.WarmSlots.Load() != 10 || m.Sandboxes.Load() != 2 {
		t.Fatalf("prewarm slots=%d physical=%d", c.metrics.WarmSlots.Load(), m.Sandboxes.Load())
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Run(context.Background(), "a", testRequest(), nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if m.MaxActive.Load() != 10 || m.Sandboxes.Load() != 2 || m.Projects.Load() != 2 || m.Violations.Load() != 0 {
		t.Fatalf("active=%d sandboxes=%d projects=%d violations=%d", m.MaxActive.Load(), m.Sandboxes.Load(), m.Projects.Load(), m.Violations.Load())
	}
	if _, err := c.Run(context.Background(), "b", testRequest(), nil); err != nil {
		t.Fatal(err)
	}
	if m.Projects.Load() != 3 || m.Sandboxes.Load() != 3 {
		t.Fatal("different owner reused project")
	}
}

func TestPrismImageInlineAfterValidatedUpload(t *testing.T) {
	m := NewMockUpstream()
	c := testClient(t, m, func(cfg *Config) { cfg.ImageInline = true })
	r := testRequest()
	r.Input[0].Content = append(r.Input[0].Content, Content{Type: "input_image", ImageURL: pngData(t)})
	if _, err := c.Run(context.Background(), "a", r, nil); err != nil {
		t.Fatal(err)
	}
	block := m.Inputs()[0][0].Content[0]
	if block.Type != "input_text" || !strings.Contains(block.Text, "view_image") || !strings.Contains(block.Text, "base64 -d") || m.Uploads.Load() != 1 {
		t.Fatal("inline restoration or upload missing")
	}
}

func TestPrismPacedAdmissionProtectsUpstreamWindow(t *testing.T) {
	m := NewMockUpstream()
	m.Immediate = true
	var mu sync.Mutex
	var starts []time.Time
	var rejected atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathStart {
			mu.Lock()
			now := time.Now()
			var recent []time.Time
			for _, ts := range starts {
				if now.Sub(ts) < 20*time.Millisecond {
					recent = append(recent, ts)
				}
			}
			if len(recent) >= 2 {
				mu.Unlock()
				rejected.Add(1)
				http.Error(w, "fixture rate limit", 429)
				return
			}
			starts = append(recent, now)
			mu.Unlock()
		}
		m.ServeHTTP(w, r)
	}))
	defer up.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = up.URL
	cfg.Credentials = []Credential{{UserID: "mock-user", SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
	cfg.AccountConcurrency = 10
	cfg.AccountRPM = 2
	cfg.SandboxPoolSize = 2
	cfg.KeepWarm = time.Hour
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Short test windows retain a generous network scheduling guard.
	c.rateWindow = 80 * time.Millisecond
	started := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := testRequest()
			r.Store = false
			if _, err := c.Run(context.Background(), "a", r, nil); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if m.Starts.Load() != 10 || rejected.Load() != 0 || c.metrics.PacedWaiters.Load() != 0 || time.Since(started) < 320*time.Millisecond {
		t.Fatal("pacing failed to protect upstream admission or release waiters")
	}
}
