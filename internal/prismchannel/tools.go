package prismchannel

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

var toolEnvelope = regexp.MustCompile("(?s)<tool_call>\\s*(.*?)\\s*</tool_call>|```(?:codex-exec|tool_call)\\s*\\n(.*?)\\n```")

func toolContract(r Request) string {
	b, _ := json.Marshal(r.Tools)
	return "Client tool protocol: these functions execute on the client. Do not execute them in the sandbox. To request a function, emit <tool_call>{\"name\":\"declared_name\",\"arguments\":{}}</tool_call>. Arguments must be valid JSON objects matching the declared schema. Multiple envelopes request parallel calls. Tool results in the history are supplied by the client. Do not fabricate results. tool_choice=" + r.ToolChoice + "; parallel_tool_calls=" + map[bool]string{true: "true", false: "false"}[r.Parallel] + ". Available functions: " + string(b)
}

// foldTools keeps call IDs and arguments in prompt history, since Prism cannot
// accept arbitrary client tools as its own executable sandbox tools.
func foldTools(input []Item) []Item {
	out := make([]Item, 0, len(input))
	for _, it := range input {
		switch it.Type {
		case "function_call":
			b, _ := json.Marshal(map[string]any{"name": it.Name, "arguments": json.RawMessage(it.Arguments), "call_id": it.CallID})
			out = append(out, textItem("assistant", "<tool_call>"+string(b)+"</tool_call>"))
		case "function_call_output":
			out = append(out, textItem("user", "Tool result for call_id="+it.CallID+":\n"+it.Output))
		default:
			out = append(out, it)
		}
	}
	return out
}

func parseTools(text string, native []Item, r Request) (string, []Item, error) {
	if len(r.Tools) == 0 {
		return text, nil, nil
	}
	allowed := make(map[string]bool)
	for _, t := range r.Tools {
		allowed[t.Name] = true
	}
	var calls []Item
	validate := func(it Item) error {
		if !allowed[it.Name] || r.ToolChoice == "none" || r.ToolChoice != "" && r.ToolChoice != "auto" && r.ToolChoice != "required" && r.ToolChoice != it.Name {
			return &Error{502, "undeclared_tool_call", ""}
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(it.Arguments), &obj) != nil || obj == nil {
			return &Error{502, "invalid_tool_arguments", ""}
		}
		it.Type = "function_call"
		if it.CallID == "" {
			it.CallID = "call_" + uuid.NewString()
		}
		it.ID = "fc_" + uuid.NewString()
		calls = append(calls, it)
		return nil
	}
	for _, it := range native {
		if e := validate(it); e != nil {
			return "", nil, e
		}
	}
	matches := toolEnvelope.FindAllStringSubmatchIndex(text, -1)
	var clean strings.Builder
	pos := 0
	for _, loc := range matches {
		start, end := loc[2], loc[3]
		if start < 0 {
			start, end = loc[4], loc[5]
		}
		var v struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			CallID    string          `json:"call_id"`
		}
		if json.Unmarshal([]byte(text[start:end]), &v) != nil {
			return "", nil, &Error{502, "invalid_tool_envelope", ""}
		}
		args := string(v.Arguments)
		if len(v.Arguments) > 0 && v.Arguments[0] == '"' {
			if json.Unmarshal(v.Arguments, &args) != nil {
				return "", nil, &Error{502, "invalid_tool_arguments", ""}
			}
		}
		if e := validate(Item{Name: v.Name, Arguments: args, CallID: v.CallID}); e != nil {
			return "", nil, e
		}
		clean.WriteString(text[pos:loc[0]])
		pos = loc[1]
	}
	clean.WriteString(text[pos:])
	if strings.Contains(clean.String(), "<tool_call>") {
		return "", nil, &Error{502, "incomplete_tool_envelope", ""}
	}
	if !r.Parallel && len(calls) > 1 {
		return "", nil, &Error{502, "parallel_tools_disabled", ""}
	}
	if r.ToolChoice != "" && r.ToolChoice != "none" && r.ToolChoice != "auto" && len(calls) == 0 {
		return "", nil, &Error{502, "required_tool_missing", ""}
	}
	return strings.TrimSpace(clean.String()), calls, nil
}
