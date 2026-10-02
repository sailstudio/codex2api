package prism

import (
	"strings"
	"testing"
)

// TestFillFromResponse_NestedPayload 锁住「payload 嵌在 response 里」这一层。
// 早期只读 response.output 会永远取到空正文，且把失败误判成成功。
func TestFillFromResponse_NestedPayload(t *testing.T) {
	resp := map[string]any{
		"status": "success",
		"payload": map[string]any{
			"id": "resp_x",
			"output": []any{
				map[string]any{
					"type": "reasoning",
					"summary": []any{
						map[string]any{"type": "summary_text", "text": "思索中"},
					},
				},
				map[string]any{
					"type": "message", "role": "assistant", "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": "收到了"}},
				},
			},
			"usage": map[string]any{
				"input_tokens": 13.0, "output_tokens": 5.0, "total_tokens": 18.0,
				"cached_input_tokens": 7.0, "reasoning_output_tokens": 2.0,
			},
		},
	}
	var turn Turn
	fillFromResponse(&turn, resp)
	if turn.Text != "收到了" {
		t.Fatalf("正文解析错误: %q（payload 层级没剥对）", turn.Text)
	}
	if turn.Reasoning != "思索中" {
		t.Fatalf("思考解析错误: %q", turn.Reasoning)
	}
	if turn.Usage.InputTokens != 13 || turn.Usage.OutputTokens != 5 || turn.Usage.TotalTokens != 18 {
		t.Fatalf("用量解析错误: %+v", turn.Usage)
	}
	if turn.Usage.CachedInputTokens != 7 || turn.Usage.ReasoningTokens != 2 {
		t.Fatalf("缓存/思考用量解析错误: %+v", turn.Usage)
	}
}

// TestFillFromResponse_FlatPayload 兼容扁平形态（无 payload 包裹）。
func TestFillFromResponse_FlatPayload(t *testing.T) {
	resp := map[string]any{
		"output": []any{
			map[string]any{"type": "message", "content": []any{map[string]any{"text": "hi"}}},
		},
	}
	var turn Turn
	fillFromResponse(&turn, resp)
	if turn.Text != "hi" {
		t.Fatalf("扁平形态解析错误: %q", turn.Text)
	}
}

// TestFillFromResponse_ErrorPropagates 上游错误必须能被读出（否则失败会被误报成成功）。
func TestFillFromResponse_ErrorPropagates(t *testing.T) {
	resp := map[string]any{
		"status": "error",
		"payload": map[string]any{
			"message": "Error while processing conversation (400 Bad Request). Please submit prompt again.",
		},
	}
	var turn Turn
	fillFromResponse(&turn, resp)
	if !strings.Contains(turn.ErrMessage, "400 Bad Request") {
		t.Fatalf("错误信息未透出: %q", turn.ErrMessage)
	}
}

// TestExtractToolCalls 工具仿真通道：<tool_call> 信封解析 + 正文清理。
func TestExtractToolCalls(t *testing.T) {
	in := "我先看看文件。\n" +
		`<tool_call>{"name":"exec_command","arguments":{"cmd":"ls"}}</tool_call>` + "\n" +
		`<tool_call>{"name":"apply_patch","arguments":{"patch":"*** Begin Patch"}}</tool_call>`
	calls, rest := extractToolCalls(in)
	if len(calls) != 2 {
		t.Fatalf("应解析出 2 个工具调用，得到 %d", len(calls))
	}
	if calls[0].Name != "exec_command" || !strings.Contains(calls[0].Arguments, `"ls"`) {
		t.Fatalf("第 1 个工具解析错误: %+v", calls[0])
	}
	if calls[0].CallID == "" || calls[1].CallID == calls[0].CallID {
		t.Fatalf("call_id 缺失或重复: %+v", calls)
	}
	if strings.Contains(rest, "<tool_call>") {
		t.Fatalf("正文未清干净: %q", rest)
	}
	if !strings.Contains(rest, "我先看看文件") {
		t.Fatalf("正文丢失: %q", rest)
	}
}

// TestIsCustomTool 自定义工具判定（走 custom_tool_call 事件）。
func TestIsCustomTool(t *testing.T) {
	for _, n := range []string{"apply_patch", "shell", "exec_command", "EXEC"} {
		if !isCustomTool(n) {
			t.Errorf("%s 应判为自定义工具", n)
		}
	}
	for _, n := range []string{"get_weather", "web_search"} {
		if isCustomTool(n) {
			t.Errorf("%s 不应判为自定义工具", n)
		}
	}
}

// TestCookieHeader 凭据头拼装（access token 同时用作两个 cookie）。
func TestCookieHeader(t *testing.T) {
	c := Cookie{AccessToken: "AT", SessionToken: "ST"}
	h := c.cookieHeader()
	for _, want := range []string{"prism_oai_access_token=AT", "oai-sc=AT", "prism_session_token=ST"} {
		if !strings.Contains(h, want) {
			t.Errorf("Cookie 头缺 %s: %s", want, h)
		}
	}
	// RawCookie 优先
	c2 := Cookie{AccessToken: "AT", RawCookie: "x=1; y=2"}
	if c2.cookieHeader() != "x=1; y=2" {
		t.Errorf("RawCookie 应优先: %s", c2.cookieHeader())
	}
}

// TestToolProtocol 工具提示词包含关键约束与工具名。
func TestToolProtocol(t *testing.T) {
	s := toolProtocol([]map[string]any{
		{"name": "exec_command", "parameters": map[string]any{"type": "object"}},
	})
	for _, want := range []string{"LOCAL MACHINE", "<tool_call>", "exec_command"} {
		if !strings.Contains(s, want) {
			t.Errorf("工具协议提示词缺 %q", want)
		}
	}
}

// TestLoadMaterial_Normalized 归一化材料（侧车写出的形态）解析。
func TestLoadMaterial_Normalized(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/material.json"
	content := `{
	  "captured_at": "2026-10-01T15:25:00Z",
	  "project_id": "proj-1",
	  "metadata": {
	    "projectId": "proj-1", "userId": "user-abc", "model": "gpt-5.6-sol",
	    "reasoning_effort": "medium",
	    "sandbox_url": "https://prism.openai.com/s/sandboxes/proxy/",
	    "sandbox_token": "tok",
	    "proxy_request_debug": "{}", "codex_listen_snapshot": "{}"
	  },
	  "input": [
	    {"type":"message","role":"system","content":[{"type":"input_text","text":"SYS"}]},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	  ]
	}`
	if err := writeFileHelper(p, content); err != nil {
		t.Fatal(err)
	}
	m, err := loadMaterial(p)
	if err != nil {
		t.Fatalf("材料加载失败: %v", err)
	}
	if m.ProjectID != "proj-1" || m.UserID != "user-abc" || m.Model != "gpt-5.6-sol" {
		t.Fatalf("材料字段错误: %+v", m)
	}
	if len(m.InputPrefix) != 1 { // 只保留 system
		t.Fatalf("system 前缀应只保留 1 条，得到 %d", len(m.InputPrefix))
	}
}

// TestLoadMaterial_TrimsBloatedPrefix 材料前缀必须截断在**首个非 system 之后**。
//
// 回归：侧车每 75s 从同一会话采一轮，历史会累积 —— 实测 input 膨胀到 67 条
// （32 份重复 system + 32 条「预热」user），材料 1KB→30KB，每轮请求白送 23K
// 字符，请求越来越慢直至超时。loadMaterial 必须只取开头连续的 system 段。
func TestLoadMaterial_TrimsBloatedPrefix(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/bloated.json"
	// 模拟膨胀：3 轮累积（每轮 = system 上下文 + user 预热）
	content := `{
	  "captured_at": "2026-10-02T12:00:00Z",
	  "metadata": {"projectId":"proj-1","userId":"u","sandbox_url":"u","sandbox_token":"t","model":"gpt-5.6-sol"},
	  "input": [
	    {"type":"message","role":"system","content":[{"type":"input_text","text":"PRISM-SYSTEM-PROMPT"}]},
	    {"type":"message","role":"system","content":[{"type":"input_text","text":"CTX"}]},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"预热"}]},
	    {"type":"message","role":"system","content":[{"type":"input_text","text":"CTX"}]},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"预热"}]},
	    {"type":"message","role":"system","content":[{"type":"input_text","text":"CTX"}]},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"预热"}]}
	  ]
	}`
	if err := writeFileHelper(p, content); err != nil {
		t.Fatal(err)
	}
	m, err := loadMaterial(p)
	if err != nil {
		t.Fatalf("材料加载失败: %v", err)
	}
	if len(m.InputPrefix) != 2 {
		t.Fatalf("膨胀材料应只保留开头 2 条 system 前缀，得到 %d（说明未截断 → 每轮白送重复历史）", len(m.InputPrefix))
	}
	for i, im := range m.InputPrefix {
		if strOf(im["role"]) != "system" {
			t.Errorf("前缀第 %d 条应为 system，得到 %v", i, im["role"])
		}
	}
}

// TestLoadMaterial_RawCapture 原始捕获形态（{start:{postData}}）。
func TestLoadMaterial_RawCapture(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/raw.json"
	content := `{"start":{"postData":"{\"input\":[{\"type\":\"message\",\"role\":\"system\",\"content\":[]}],\"metadata\":{\"projectId\":\"p2\",\"sandbox_url\":\"u\",\"sandbox_token\":\"t\"}}"}}`
	if err := writeFileHelper(p, content); err != nil {
		t.Fatal(err)
	}
	m, err := loadMaterial(p)
	if err != nil {
		t.Fatalf("原始捕获材料加载失败: %v", err)
	}
	if m.ProjectID != "p2" {
		t.Fatalf("projectId 解析错误: %s", m.ProjectID)
	}
}

// TestLoadMaterial_RejectsMissingSandbox 缺沙箱字段必须拒绝（否则会静默 400）。
func TestLoadMaterial_RejectsMissingSandbox(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/bad.json"
	content := `{"metadata":{"projectId":"p3"}}`
	_ = writeFileHelper(p, content)
	if _, err := loadMaterial(p); err == nil {
		t.Fatal("缺 sandbox_url/sandbox_token 的材料应被拒绝")
	}
}

func writeFileHelper(path, content string) error {
	return writeFileBytes(path, []byte(content))
}
