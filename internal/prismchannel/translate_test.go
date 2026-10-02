package prismchannel

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrismDecodeChatAndResponses(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		responses  bool
	}{
		{"chat", `{"model":"prism-test","messages":[{"role":"system","content":"instructions"},{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]},{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"weather","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call1","content":"sunny"}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"weather"}},"stream":true,"stream_options":{"include_usage":true}}`, false},
		{"responses", `{"model":"prism-test","input":[{"role":"system","content":"instructions"},{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AA=="}]},{"type":"function_call","name":"weather","call_id":"call1","arguments":"{}"},{"type":"function_call_output","call_id":"call1","output":"sunny"}],"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"weather"},"stream":true}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, e := Decode([]byte(tc.body), tc.responses)
			if e != nil {
				t.Fatal(e)
			}
			if len(r.Input) != 4 || r.Input[1].Content[1].Type != "input_image" || r.Input[2].Type != "function_call" || r.Input[3].Output != "sunny" || r.ToolChoice != "weather" || !r.Stream || !r.Store {
				t.Fatalf("decode=%+v", r)
			}
		})
	}
	for _, body := range []string{`{`, `{"model":"prism-test","input":[{"type":"computer_call"}]}`, `{"model":"prism-test","input":[{"role":"user","content":[{"type":"input_image"}]}]}`, `{"model":"prism-test","input":"hi","tools":[{"type":"web_search"}]}`, `{"model":"prism-test","input":"hi","tool_choice":"required"}`, `{"model":"prism-test","input":"hi","stream":"true"}`} {
		if _, e := Decode([]byte(body), true); e == nil {
			t.Fatalf("invalid body accepted: %s", body)
		}
	}
}
func TestPrismToolValidation(t *testing.T) {
	r := testRequest()
	r.Tools = []Tool{{Type: "function", Name: "weather"}}
	valid := `<tool_call>{"name":"weather","arguments":{"city":"Shanghai"}}</tool_call>`
	text, calls, e := parseTools("before\n"+valid+"\nafter", nil, r)
	if e != nil || text != "before\n\nafter" || len(calls) != 1 || !json.Valid([]byte(calls[0].Arguments)) {
		t.Fatalf("parse=%q %+v %v", text, calls, e)
	}
	for _, bad := range []string{`<tool_call>{"name":"unknown","arguments":{}}</tool_call>`, `<tool_call>{"name":"weather","arguments":null}</tool_call>`, `<tool_call>{"name":"weather","arguments":[]}</tool_call>`, `<tool_call>{bad}</tool_call>`, `<tool_call>{"name":"weather"}`} {
		if _, _, e := parseTools(bad, nil, r); e == nil {
			t.Fatalf("bad call accepted: %s", bad)
		}
	}
	r.ToolChoice = "none"
	if _, _, e := parseTools(valid, nil, r); e == nil {
		t.Fatal("none allowed call")
	}
	r.ToolChoice = "required"
	if _, _, e := parseTools("no call", nil, r); e == nil {
		t.Fatal("required allowed no call")
	}
	r.ToolChoice = "auto"
	r.Parallel = false
	if _, _, e := parseTools(valid+valid, nil, r); e == nil {
		t.Fatal("parallel=false allowed multiple")
	}
	if _, calls, e := parseTools("```codex-exec\n{\"name\":\"weather\",\"arguments\":{}}\n```", nil, r); e != nil || len(calls) != 1 {
		t.Fatalf("fence parse=%v", e)
	}
}
func TestPrismConfigFailClosed(t *testing.T) {
	t.Setenv("PRISM_ENABLED", "false")
	t.Setenv("PRISM_FORCE", "false")
	t.Setenv("PRISM_SESSION_TOKEN", "")
	t.Setenv("PRISM_ACCESS_TOKEN", "")
	if c, e := LoadEnv(); e != nil || c.Enabled {
		t.Fatalf("disabled config: %+v %v", c, e)
	}
	t.Setenv("PRISM_ENABLED", "true")
	if _, e := LoadEnv(); e == nil || strings.Contains(e.Error(), "mock-session-only") {
		t.Fatal("missing credentials accepted")
	}
	t.Setenv("PRISM_USER_ID", "mock-user")
	t.Setenv("PRISM_SESSION_TOKEN", "mock-session-only")
	t.Setenv("PRISM_ACCESS_TOKEN", "mock-access-only")
	if _, e := LoadEnv(); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PRISM_POLL_MIN", "2s")
	t.Setenv("PRISM_POLL_MAX", "1s")
	if _, e := LoadEnv(); e == nil {
		t.Fatal("invalid poll range accepted")
	}
	for _, base := range []string{"http://example.com", "https://user:secret@example.com", "https://example.com/path", "https://example.com?token=secret"} {
		c := DefaultConfig()
		c.BaseURL = base
		c.Credentials = []Credential{{UserID: "mock-user", SessionToken: "mock-session-only", AccessToken: "mock-access-only"}}
		if e := c.Validate(); e == nil || strings.Contains(e.Error(), "secret") {
			t.Fatal("invalid origin accepted or leaked")
		}
	}
}
