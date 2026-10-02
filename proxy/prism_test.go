package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/prismchannel"
	"github.com/gin-gonic/gin"
)

func prismTestServer(t *testing.T, m *prismchannel.MockUpstream, anonymous bool) (*httptest.Server, *Handler) {
	t.Helper()
	up := httptest.NewServer(m)
	t.Cleanup(up.Close)
	pc := prismchannel.DefaultConfig()
	pc.Enabled = true
	pc.BaseURL = up.URL
	pc.Credentials = []prismchannel.Credential{{UserID: "mock-user", SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
	pc.PollMin = time.Millisecond
	pc.PollMax = 2 * time.Millisecond
	pc.KeepWarm = time.Hour
	cl, e := prismchannel.New(pc)
	if e != nil {
		t.Fatal(e)
	}
	h := &Handler{cfg: &config.Config{Prism: pc, AllowAnonymousV1: anonymous}, prismClient: cl, configKeys: map[string]bool{"mock-client-a": true, "mock-client-b": true}}
	t.Cleanup(h.ClosePrism)
	r := gin.New()
	h.RegisterRoutes(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, h
}
func prismPOST(t *testing.T, srv *httptest.Server, path, body, key string) (int, string) {
	t.Helper()
	req, e := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, e := srv.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(resp.Body)
	if e != nil {
		t.Fatal(e)
	}
	return resp.StatusCode, string(b)
}
func TestPrismMockSmokeStreamToolsVisionCache(t *testing.T) {
	m := prismchannel.NewMockUpstream()
	srv, h := prismTestServer(t, m, false)
	for _, tc := range []struct {
		path, body string
		want       []string
	}{
		{"/v1/chat/completions", `{"model":"prism-mock-model","messages":[{"role":"user","content":"hello"}],"stream":true,"stream_options":{"include_usage":true}}`, []string{`"role":"assistant"`, `"content":"Hello"`, `"finish_reason":"stop"`, `"completion_tokens":8`, `data: [DONE]`}},
		{"/v1/responses", `{"model":"prism-mock-model","input":"hello","stream":true}`, []string{"event: response.created", "event: response.output_text.delta", "event: response.output_text.done", "event: response.completed", `"output_text":"Hello from mock Prism"`}},
	} {
		code, b := prismPOST(t, srv, tc.path, tc.body, "mock-client-a")
		if code != 200 {
			t.Fatalf("HTTP %d: %s", code, b)
		}
		for _, want := range tc.want {
			if !strings.Contains(b, want) {
				t.Fatalf("missing %s: %s", want, b)
			}
		}
		if strings.Contains(b, "mock-sandbox") || strings.Contains(b, "opaque") {
			t.Fatal("upstream secret/state leaked")
		}
	}
	code, b := prismPOST(t, srv, "/v1/responses", `{"model":"prism-mock-model","input":"hello"}`, "mock-client-a")
	var response struct {
		ID string `json:"id"`
	}
	if code != 200 || json.Unmarshal([]byte(b), &response) != nil || response.ID == "" {
		t.Fatalf("response=%s", b)
	}
	q := `{"model":"prism-mock-model","input":"followup","previous_response_id":"` + response.ID + `"}`
	code, b = prismPOST(t, srv, "/v1/responses", q, "mock-client-a")
	if code != 200 || !strings.Contains(b, `"prism_cache_read_tokens":`) || !strings.Contains(b, `"prism_cache_write_tokens":`) {
		t.Fatalf("cached response=%d %s", code, b)
	}
	if !strings.Contains(b, `"cached_tokens":0`) {
		t.Fatalf("local cache estimate reported as provider cached_tokens: %s", b)
	}
	code, b = prismPOST(t, srv, "/v1/responses", q, "mock-client-b")
	if code != 400 {
		t.Fatalf("cross-key history accepted: %d %s", code, b)
	}
	var pngBytes bytes.Buffer
	if e := png.Encode(&pngBytes, image.NewRGBA(image.Rect(0, 0, 1, 1))); e != nil {
		t.Fatal(e)
	}
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes.Bytes())
	vision := `{"model":"prism-mock-model","input":[{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"` + uri + `"}]}]}`
	code, b = prismPOST(t, srv, "/v1/responses", vision, "mock-client-a")
	if code != 200 || m.Uploads.Load() != 1 {
		t.Fatalf("vision failed: %d %s", code, b)
	}
	// Changing mock settings happens only after all previous handlers completed.
	m.ToolName = "weather"
	for _, tc := range []struct {
		path, body string
		want       []string
	}{
		{"/v1/chat/completions", `{"model":"prism-mock-model","messages":[{"role":"user","content":"weather"}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}],"stream":true}`, []string{`"tool_calls"`, `"name":"weather"`, `"finish_reason":"tool_calls"`}},
		{"/v1/responses", `{"model":"prism-mock-model","input":"weather","tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"stream":true}`, []string{"event: response.function_call_arguments.delta", "event: response.function_call_arguments.done", `"type":"function_call"`}},
	} {
		code, b = prismPOST(t, srv, tc.path, tc.body, "mock-client-a")
		if code != 200 {
			t.Fatalf("tools HTTP %d: %s", code, b)
		}
		for _, want := range tc.want {
			if !strings.Contains(b, want) {
				t.Fatalf("tool stream missing %s: %s", want, b)
			}
		}
		if strings.Contains(b, "<tool_call>") {
			t.Fatal("tool envelope leaked")
		}
	}
	if m.Violations.Load() != 0 || h.prismClient.Metrics().CacheHits.Load() != 1 {
		t.Fatalf("violations=%d cache_hits=%d", m.Violations.Load(), h.prismClient.Metrics().CacheHits.Load())
	}
	t.Logf("mock smoke: starts=%d polls=%d uploads=%d cache_hits=%d cache_read_tokens=%d cache_write_tokens=%d", m.Starts.Load(), m.Polls.Load(), m.Uploads.Load(), h.prismClient.Metrics().CacheHits.Load(), h.prismClient.Metrics().CacheReadTokens.Load(), h.prismClient.Metrics().CacheWriteTokens.Load())
}
func TestPrismAuthHealthMetricsAndRestrictions(t *testing.T) {
	m := prismchannel.NewMockUpstream()
	srv, h := prismTestServer(t, m, false)
	code, _ := prismPOST(t, srv, "/v1/responses", `{"model":"prism-test","input":"hi"}`, "")
	if code != 401 || m.Starts.Load() != 0 {
		t.Fatal("auth bypass")
	}
	for _, path := range []string{"/health/prism", "/metrics/prism"} {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		if path == "/metrics/prism" {
			req.Header.Set("Authorization", "Bearer mock-client-a")
		}
		resp, e := srv.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || strings.Contains(string(b), "mock-session") {
			t.Fatalf("health/metrics %d %s", resp.StatusCode, b)
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Set(contextAPIKeyRow, &database.APIKeyRow{Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelCodex}})
	if !h.handlePrism(c, []byte(`{"model":"prism-test","input":"hi"}`), true) || w.Code != 403 {
		t.Fatalf("restricted key routed: %d %s", w.Code, w.Body)
	}
	h.cfg.Prism.Force = true
	code, b := prismPOST(t, srv, "/v1/responses", `{"model":"mock-model","input":"hi"}`, "mock-client-a")
	if code != 200 {
		t.Fatalf("force routing %d %s", code, b)
	}
	_, e := h.prismClient.Run(context.Background(), "owner", prismchannel.Request{Model: "prism-test", Input: []prismchannel.Item{{Type: "message", Role: "user", Content: []prismchannel.Content{{Type: "input_text", Text: "hi"}}}}}, nil)
	if e != nil {
		t.Fatal(e)
	}
}
func TestPrismDisabledAndOrdinaryRouting(t *testing.T) {
	h := &Handler{}
	for _, tc := range []struct {
		model   string
		handled bool
		status  int
	}{{"prism-test", true, 503}, {"gpt-5", false, 200}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		b := []byte(`{"model":"` + tc.model + `","input":"hi"}`)
		handled := h.handlePrism(c, b, true)
		if handled != tc.handled || w.Code != tc.status {
			t.Fatalf("routing %s: %t %d", tc.model, handled, w.Code)
		}
	}
}
func TestPrismStoreFalseAndStreamFailure(t *testing.T) {
	m := prismchannel.NewMockUpstream()
	srv, h := prismTestServer(t, m, false)
	code, b := prismPOST(t, srv, "/v1/responses", `{"model":"prism-test","input":"hi","store":false}`, "mock-client-a")
	if code != 200 || h.prismClient.Metrics().CacheWrites.Load() != 0 {
		t.Fatalf("store=false ignored %d %s", code, b)
	}
	m.Fail = true
	code, b = prismPOST(t, srv, "/v1/responses", `{"model":"prism-test","input":"hi","stream":true}`, "mock-client-a")
	if code != 200 || !strings.Contains(b, "event: response.failed") || strings.Contains(b, "event: response.completed") {
		t.Fatalf("stream business failure not surfaced: %d %s", code, b)
	}
}

func TestPrismStreamingFlushKeepaliveAndDisconnect(t *testing.T) {
	m := prismchannel.NewMockUpstream()
	m.PollDelay = 300 * time.Millisecond
	m.PollCount = 100
	srv, h := prismTestServer(t, m, false)
	h.cfg.Prism.StreamKeepalive = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/responses", strings.NewReader(`{"model":"prism-test","input":"hi","stream":true}`))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Authorization", "Bearer mock-client-a")
	req.Header.Set("Content-Type", "application/json")
	resp, e := srv.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	line, e := reader.ReadString('\n')
	if e != nil || line != "event: response.created\n" {
		t.Fatalf("early stream missing: %q %v", line, e)
	}
	heartbeats := 0
	for heartbeats < 2 {
		line, e = reader.ReadString('\n')
		if e != nil {
			t.Fatal(e)
		}
		if strings.HasPrefix(line, ": prism keepalive") {
			heartbeats++
		}
	}
	if m.Polls.Load() != 0 {
		t.Fatal("keepalive arrived only after upstream response")
	}
	deadline := time.Now().Add(time.Second)
	for m.Starts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Starts.Load() == 0 {
		t.Fatal("mock start never admitted")
	}
	resp.Body.Close()
	cancel()
	for m.Stops.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if m.Stops.Load() != 1 || m.Active.Load() != 0 || m.Violations.Load() != 0 {
		t.Fatalf("disconnect stop=%d active=%d violations=%d", m.Stops.Load(), m.Active.Load(), m.Violations.Load())
	}
}
