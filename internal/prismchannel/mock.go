package prismchannel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MockUpstream implements the calibrated wire shapes without external accounts.
// Its fixture tokens are deliberately unusable outside this handler.
type MockUpstream struct {
	Text                                                           string
	ToolName                                                       string
	PollCount                                                      int
	PollDelay                                                      time.Duration
	Immediate                                                      bool
	Fail                                                           bool
	MissingState                                                   bool
	NativeCall                                                     bool
	NoUsage                                                        bool
	HTTPStatus                                                     int
	Starts, Polls, Stops, Uploads, Projects, Sandboxes, Violations atomic.Int64
	Active, MaxActive                                              atomic.Int64
	mu                                                             sync.Mutex
	turns                                                          map[string]*mockTurn
	boxes                                                          map[string]*mockBox
	inputs                                                         [][]Item
}
type mockTurn struct {
	seq, polls int
	state      json.RawMessage
	active     bool
}
type mockBox struct{ resource, y bool }

func NewMockUpstream() *MockUpstream {
	return &MockUpstream{Text: "Hello from mock Prism", PollCount: 2, turns: make(map[string]*mockTurn), boxes: make(map[string]*mockBox)}
}
func mockState(id string, seq int) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"opaque": []any{id, seq, map[string]any{"never_reconstruct": true}}})
	return b
}
func (m *MockUpstream) Inputs() [][]Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, _ := json.Marshal(m.inputs)
	var out [][]Item
	_ = json.Unmarshal(b, &out)
	return out
}
func (m *MockUpstream) violation(w http.ResponseWriter) {
	m.Violations.Add(1)
	http.Error(w, "mock protocol violation", 400)
}
func mockJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func (m *MockUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, e := r.Cookie("prism_session_token"); e != nil {
		m.violation(w)
		return
	}
	if _, e := r.Cookie("prism_oai_access_token"); e != nil {
		m.violation(w)
		return
	}
	if m.HTTPStatus > 0 && (r.URL.Path == PathStart || r.URL.Path == PathStatus) {
		http.Error(w, "mock rejected", m.HTTPStatus)
		return
	}
	if r.URL.Path == PathStatus && m.PollDelay > 0 {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(m.PollDelay):
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var body map[string]json.RawMessage
	if r.Method == http.MethodPost && r.URL.Path != "/api/project-files/upload" {
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body) != nil {
			m.violation(w)
			return
		}
	}
	switch {
	case r.URL.Path == "/api/codex/runtime/debug":
		mockJSON(w, map[string]any{"snapshot": map[string]any{"codex_session_id": "mock-codex-session", "last_turn_id": "mock-last-turn", "transcript_cursor": 1}})
	case r.URL.Path == "/api/projects":
		var id, title string
		_ = json.Unmarshal(body["project_uuid"], &id)
		_ = json.Unmarshal(body["title"], &title)
		if id == "" || title == "" {
			m.violation(w)
			return
		}
		m.Projects.Add(1)
		mockJSON(w, map[string]any{"uuid": id})
	case r.URL.Path == "/api/backend/1/new":
		n := m.Sandboxes.Add(1)
		token := fmt.Sprintf("mock-sandbox-%d", n)
		m.boxes[token] = &mockBox{}
		mockJSON(w, map[string]any{"url": "/s/sandboxes/proxy", "token": token})
	case strings.HasPrefix(r.URL.Path, "/api/projects/") && strings.HasSuffix(r.URL.Path, "/sandbox/resources-token"):
		var token string
		_ = json.Unmarshal(body["sandbox_token"], &token)
		if m.boxes[token] == nil {
			m.violation(w)
			return
		}
		mockJSON(w, map[string]any{"access_token": "mock-resource-only", "max_age_seconds": 3600})
	case r.URL.Path == "/api/y":
		var doc string
		_ = json.Unmarshal(body["docId"], &doc)
		if doc == "" {
			m.violation(w)
			return
		}
		mockJSON(w, map[string]any{"token": "mock-ysweet-only", "url": "wss://mock.invalid/unused", "extra_opaque": []any{1, "preserve"}})
	case strings.HasPrefix(r.URL.Path, "/s/sandboxes/proxy/"):
		box := m.boxes[r.Header.Get("X-Crixet-Sandbox-Token")]
		if box == nil {
			m.violation(w)
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/s/sandboxes/proxy/") {
		case "resources-token":
			if body["token"] == nil || body["projectId"] == nil || body["resourceBaseUrl"] == nil {
				m.violation(w)
				return
			}
			box.resource = true
			mockJSON(w, map[string]bool{"success": true})
		case "token":
			if body["extra_opaque"] == nil {
				m.violation(w)
				return
			}
			box.y = true
			mockJSON(w, map[string]bool{"success": true})
		case "wait-for-sync":
			status := "syncing"
			if box.resource && box.y {
				status = "synced"
			}
			mockJSON(w, map[string]any{"status": status, "readinessCapabilities": []string{"current_y_sweet_provider"}, "tokens": map[string]bool{"hasCurrentYSweetToken": box.y, "hasSyncedYSweetProvider": box.y}})
		default:
			m.violation(w)
		}
	case r.URL.Path == "/api/project-files/upload":
		data, e := io.ReadAll(io.LimitReader(r.Body, 11<<20))
		config, format, decodeErr := image.DecodeConfig(bytes.NewReader(data))
		name := r.Header.Get("X-Prism-File-Name")
		fid := r.Header.Get("X-Prism-File-Id")
		if e != nil || decodeErr != nil || config.Width < 1 || format != "png" && format != "jpeg" && format != "gif" && format != "webp" || !strings.HasPrefix(r.Header.Get("Content-Type"), "image/") || fid == "" || r.Header.Get("X-Prism-Project-Id") == "" || name == "" || r.Header.Get("X-Prism-File-Size") != strconv.Itoa(len(data)) || r.Header.Get("X-Prism-Require-Project-Edit-Access") != "true" {
			m.violation(w)
			return
		}
		m.Uploads.Add(1)
		mockJSON(w, map[string]any{"id": fid, "fileUuid": fid, "didSanitize": false, "sedimentFileId": "mock-sediment-file"})
	case r.URL.Path == PathStart:
		var input []Item
		var meta struct {
			Project      string `json:"projectId"`
			SandboxToken string `json:"sandbox_token"`
			Model        string `json:"model"`
			URL          string `json:"sandbox_url"`
		}
		_ = json.Unmarshal(body["metadata"], &meta)
		_ = json.Unmarshal(body["input"], &input)
		box := m.boxes[meta.SandboxToken]
		if box == nil || !box.resource || !box.y || meta.Project == "" || meta.URL == "" || meta.Model == "" || len(input) == 0 || body["conversationId"] == nil {
			m.violation(w)
			return
		}
		for _, it := range input {
			if it.Type != "message" {
				m.violation(w)
				return
			}
			for _, v := range it.Content {
				if v.Type != "input_text" && v.Type != "input_file" {
					m.violation(w)
					return
				}
			}
		}
		m.inputs = append(m.inputs, input)
		n := m.Starts.Add(1)
		id := fmt.Sprintf("mock-turn-%d", n)
		t := &mockTurn{state: mockState(id, 0), active: true}
		m.turns[id] = t
		active := m.Active.Add(1)
		for old := m.MaxActive.Load(); active > old && !m.MaxActive.CompareAndSwap(old, active); old = m.MaxActive.Load() {
		}
		if m.Immediate {
			m.complete(w, id, t)
			return
		}
		if m.MissingState {
			mockJSON(w, map[string]any{"status": "started", "request_id": id})
			return
		}
		mockJSON(w, map[string]any{"status": "started", "request_id": id, "turn_state": t.state})
	case r.URL.Path == PathStatus || r.URL.Path == PathStop:
		var id string
		_ = json.Unmarshal(body["request_id"], &id)
		t := m.turns[id]
		if t == nil {
			m.violation(w)
			return
		}
		if r.URL.Path == PathStop {
			if !m.MissingState && string(body["turn_state"]) != string(t.state) {
				m.violation(w)
				return
			}
			m.Stops.Add(1)
			if t.active {
				t.active = false
				m.Active.Add(-1)
			}
			mockJSON(w, map[string]bool{"stopped": true})
			return
		}
		if string(body["turn_state"]) != string(t.state) {
			m.violation(w)
			return
		}
		m.Polls.Add(1)
		t.polls++
		t.seq++
		t.state = mockState(id, t.seq)
		if t.polls >= m.PollCount {
			m.complete(w, id, t)
			return
		}
		text := m.Text
		if m.ToolName != "" {
			text = "<tool_call>"
		}
		if len(text) > 5 {
			text = text[:5]
		}
		mockJSON(w, map[string]any{"status": "pending", "request_id": id, "turn_state": t.state, "response": map[string]any{"status": "success", "payload": map[string]any{"output": []Item{{Type: "message", Role: "assistant", Content: []Content{{Type: "output_text", Text: text}}}}}}})
	default:
		http.NotFound(w, r)
	}
}
func (m *MockUpstream) complete(w http.ResponseWriter, id string, t *mockTurn) {
	if t.active {
		t.active = false
		m.Active.Add(-1)
	}
	text := m.Text
	if m.ToolName != "" {
		b, _ := json.Marshal(map[string]any{"name": m.ToolName, "arguments": map[string]any{"city": "Shanghai"}})
		text = "<tool_call>" + string(b) + "</tool_call>"
	}
	output := []Item{{Type: "message", Role: "assistant", Content: []Content{{Type: "output_text", Text: text}}}}
	if m.NativeCall {
		output = []Item{{Type: "function_call", Name: m.ToolName, CallID: "mock-call", Arguments: `{"city":"Shanghai"}`}}
	}
	status := "success"
	if m.Fail {
		status = "error"
	}
	payload := map[string]any{"output": output}
	if !m.NoUsage {
		payload["usage"] = Usage{InputTokens: 24, OutputTokens: 8, TotalTokens: 32}
	}
	mockJSON(w, map[string]any{"status": "completed", "request_id": id, "turn_state": t.state, "response": map[string]any{"status": status, "payload": payload}})
}
