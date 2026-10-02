package prism

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseModelID 验证模型名后缀解析（上游只认裸名 + metadata 里的 effort）。
func TestParseModelID(t *testing.T) {
	cases := []struct{ in, base, effort string }{
		{"gpt-5.6-sol", "gpt-5.6-sol", ""},
		{"gpt-5.6-sol-high", "gpt-5.6-sol", "high"},
		{"gpt-6-astra-xhigh", "gpt-6-astra", "xhigh"},
		{"gpt-5.6-sol:medium", "gpt-5.6-sol", "medium"},
		{"gpt-5.6-terra-low", "gpt-5.6-terra", "low"},
		{"", "", ""},
	}
	for _, c := range cases {
		base, eff := ParseModelID(c.in)
		if base != c.base || eff != c.effort {
			t.Errorf("ParseModelID(%q) = (%q,%q)，期望 (%q,%q)", c.in, base, eff, c.base, c.effort)
		}
	}
}

// TestResolveModel 验证别名映射与目录外名字兜底。
func TestResolveModel(t *testing.T) {
	cat := DefaultModels()
	// 别名 sol → gpt-5.6-sol
	sm, eff, id := ResolveModel(cat, "sol", "")
	if sm != "gpt-5.6-sol" || id != "gpt-5.6-sol" {
		t.Errorf("别名 sol 应映射到 gpt-5.6-sol，得到 %q/%q", sm, id)
	}
	if eff != "medium" {
		t.Errorf("未指定档位应默认 medium，得到 %q", eff)
	}
	// 带后缀 + 显式档位（显式优先）
	sm, eff, _ = ResolveModel(cat, "gpt-5.6-sol-low", "xhigh")
	if sm != "gpt-5.6-sol" || eff != "xhigh" {
		t.Errorf("显式档位应优先，得到 %q/%q", sm, eff)
	}
	// 目录外的名字兜底到默认模型（否则上游 400）
	sm, _, _ = ResolveModel(cat, "gpt-99-unknown", "")
	if sm != cat[0].ServerModelName {
		t.Errorf("目录外名字应兜底到 %q，得到 %q", cat[0].ServerModelName, sm)
	}
}

// TestBuildInput_NoTools 验证无 tools 时：所有前文合并成一条 system + 一条 user。
func TestBuildInput_NoTools(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "客户端系统指令"},
		{Role: RoleUser, Content: "第一问"},
		{Role: RoleAssistant, Content: "第一答"},
		{Role: RoleUser, Content: "第二问"},
	}
	in := BuildInput(msgs, nil, "网关指令", DefaultAttachmentBudget())
	if len(in) != 2 {
		t.Fatalf("应产出 2 个 item（system + user），得到 %d", len(in))
	}
	if in[0].Role != RoleSystem || in[1].Role != RoleUser {
		t.Fatalf("角色顺序应为 system,user，得到 %s,%s", in[0].Role, in[1].Role)
	}
	sysText := in[0].Content[0].Text
	if !strings.Contains(sysText, "网关指令") {
		t.Errorf("system 应含网关注入指令")
	}
	if !strings.Contains(sysText, "[客户端指令]") || !strings.Contains(sysText, "客户端系统指令") {
		t.Errorf("system 应含客户端指令块：%s", sysText)
	}
	if !strings.Contains(sysText, "[对话历史]") || !strings.Contains(sysText, "第一问") {
		t.Errorf("system 应含对话历史：%s", sysText)
	}
	if in[1].Content[0].Text != "第二问" {
		t.Errorf("user item 应只含本轮请求，得到 %q", in[1].Content[0].Text)
	}
}

// TestBuildInput_WithTools 验证有 tools 时：上游不采信 system，全部并进 user。
func TestBuildInput_WithTools(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "客户端指令"},
		{Role: RoleUser, Content: "帮我读文件"},
	}
	tools := []ToolSpec{{Name: "read_file", Description: "读文件",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}}
	in := BuildInput(msgs, tools, "网关指令", DefaultAttachmentBudget())

	if len(in) != 1 {
		t.Fatalf("有 tools 时应只产出 1 个 user item，得到 %d", len(in))
	}
	if in[0].Role != RoleUser {
		t.Fatalf("应为 user item，得到 %s", in[0].Role)
	}
	text := in[0].Content[0].Text
	for _, want := range []string{"[系统指令]", "网关指令", "[客户端指令]", "[工具协议]", "read_file", "[用户当前消息]", "帮我读文件"} {
		if !strings.Contains(text, want) {
			t.Errorf("user item 缺少 %q", want)
		}
	}
	// 具名示例（避免模型改名）与必填键形状
	if !strings.Contains(text, `"name":"read_file"`) {
		t.Errorf("工具示例应用真实工具名：%s", text)
	}
	if !strings.Contains(text, `"path"`) {
		t.Errorf("工具示例应使用 schema 必填键 path")
	}
}

// TestBuildInput_ToolResults 验证工具结果归属与「已执行结果」块。
func TestBuildInput_ToolResults(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: "读一下"},
		{Role: RoleAssistant, ToolCalls: []ToolCallRef{{ID: "call_1", Name: "read_file", Arguments: `{"path":"a.txt"}`}}},
		{Role: RoleTool, ToolCallID: "call_1", Content: "file content here"},
		{Role: RoleUser, Content: "总结一下"},
	}
	tools := []ToolSpec{{Name: "read_file"}}
	in := BuildInput(msgs, tools, "", DefaultAttachmentBudget())
	text := in[0].Content[0].Text
	if !strings.Contains(text, "[已执行工具的结果") {
		t.Errorf("应含已执行结果块：%s", text)
	}
	// 结果应归属到工具名（按 tool_call_id 反查）
	if !strings.Contains(text, "- read_file: file content here") {
		t.Errorf("工具结果应归属到 read_file：%s", text)
	}
}

// TestExtractToolCalls 验证信封解析（含并行多块与非法块保留原文）。
func TestExtractToolCalls(t *testing.T) {
	text := `我先查两个东西。
<tool_call>{"name":"get_weather","arguments":{"city":"杭州"}}</tool_call>
<tool_call>{"tool":"get_time","arguments":"{\"tz\":\"CST\"}"}</tool_call>
以上。`
	calls, cleaned := ExtractToolCalls(text)
	if len(calls) != 2 {
		t.Fatalf("应解析出 2 个调用，得到 %d", len(calls))
	}
	if calls[0].Name != "get_weather" || !strings.Contains(calls[0].Arguments, "杭州") {
		t.Errorf("第一个调用不匹配: %+v", calls[0])
	}
	// {tool, arguments-as-string} 兼容
	if calls[1].Name != "get_time" || !strings.Contains(calls[1].Arguments, "CST") {
		t.Errorf("第二个调用（字符串 arguments）不匹配: %+v", calls[1])
	}
	if strings.Contains(cleaned, "<tool_call>") {
		t.Errorf("正文应剥掉信封：%s", cleaned)
	}
	if !strings.Contains(cleaned, "我先查两个东西") || !strings.Contains(cleaned, "以上") {
		t.Errorf("正文其它内容应保留：%s", cleaned)
	}
	if calls[0].ID == "" || !strings.HasPrefix(calls[0].ID, "call_") {
		t.Errorf("应生成 call_ 前缀的 id，得到 %q", calls[0].ID)
	}

	// 非法 JSON：不吞内容
	bad := `说明文字 <tool_call>{not-json}</tool_call> 结尾`
	c2, kept := ExtractToolCalls(bad)
	if len(c2) != 0 {
		t.Errorf("非法块不应解析出调用")
	}
	if !strings.Contains(kept, "not-json") {
		t.Errorf("非法块应原样保留：%s", kept)
	}
}

// TestAttachmentImageBlock 验证图片通道的还原命令与预算扣减。
func TestAttachmentImageBlock(t *testing.T) {
	// 构造一张真实 PNG（1x1）
	pngBytes := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9C, 0x63, 0x60, 0x60, 0x60, 0x60,
		0x00, 0x00, 0x00, 0x05, 0x00, 0x01, 0xE6, 0x5A,
		0x3F, 0x0C, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
		0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
	}
	b := DefaultAttachmentBudget()
	blocks := RenderBlocks([]Attachment{{Name: "shot.png", Mime: "image/png", Data: pngBytes}}, &b, 0)
	if len(blocks) != 1 {
		t.Fatalf("应产出 1 个块，得到 %d", len(blocks))
	}
	blk := blocks[0]
	for _, want := range []string{"[图片附件 1]", "shot.png", "base64 -d", "prism-uploads/shot.png", "view_image"} {
		if !strings.Contains(blk, want) {
			t.Errorf("图片块缺少 %q\n%s", want, blk)
		}
	}
	if b.TotalChars >= DefaultAttachmentBudget().TotalChars {
		t.Errorf("预算未扣减")
	}
}

// TestAttachmentTextAndBinary 验证文本附件直送、二进制附件给提示。
func TestAttachmentTextAndBinary(t *testing.T) {
	b := DefaultAttachmentBudget()
	blocks := RenderBlocks([]Attachment{
		{Name: "doc.pdf", Mime: "application/pdf", Data: []byte("..."), Text: "这是 PDF 正文"},
		{Name: "blob.bin", Mime: "application/octet-stream", Data: []byte{0, 1, 2}},
	}, &b, 0)
	if !strings.Contains(blocks[0], "这是 PDF 正文") {
		t.Errorf("文本附件应直送正文: %s", blocks[0])
	}
	if !strings.Contains(blocks[1], "无法直接送入模型") {
		t.Errorf("二进制附件应给提示: %s", blocks[1])
	}
}

// TestSafeFileName 验证文件名净化（防注入 shell）。
func TestSafeFileName(t *testing.T) {
	got := SafeFileName("../../etc/passwd; rm -rf /", "image/png", 0)
	if strings.ContainsAny(got, "/;") {
		t.Fatalf("文件名应被净化，得到 %q", got)
	}
	if !strings.HasSuffix(got, ".png") {
		t.Errorf("应补上扩展名，得到 %q", got)
	}
}

// TestToolSpecsFromRaw 验证两种 tools 形状归一化。
func TestToolSpecsFromRaw(t *testing.T) {
	openai := json.RawMessage(`[{"type":"function","function":{"name":"a","description":"d","parameters":{"type":"object"}}}]`)
	flat := json.RawMessage(`[{"type":"function","name":"b","description":"e","parameters":{"type":"object"}}]`)
	anthropicLike := json.RawMessage(`[{"name":"c","description":"f","input_schema":{"type":"object"}}]`)

	for name, raw := range map[string]json.RawMessage{"openai": openai, "flat": flat, "anthropic": anthropicLike} {
		specs := ToolSpecsFromRaw(raw)
		if len(specs) != 1 {
			t.Fatalf("%s: 应解析 1 个工具，得到 %d", name, len(specs))
		}
		if specs[0].Name == "" {
			t.Errorf("%s: 工具名不应为空", name)
		}
		if len(specs[0].Parameters) == 0 {
			t.Errorf("%s: 参数 schema 不应为空", name)
		}
	}
}

// TestToolsToUpstreamShape 验证上游形状（平铺，不套 function 层）。
func TestToolsToUpstreamShape(t *testing.T) {
	raw := ToolsToUpstreamShape([]ToolSpec{{Name: "x", Description: "d",
		Parameters: json.RawMessage(`{"type":"object"}`)}})
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatal(err)
	}
	if arr[0]["name"] != "x" {
		t.Errorf("应平铺 name，得到 %v", arr[0])
	}
	if _, nested := arr[0]["function"]; nested {
		t.Errorf("不应嵌套 function 层")
	}
}

// TestMissingCookies 验证关键 cookie 校验。
func TestMissingCookies(t *testing.T) {
	full := "oai-sc=1; prism_session_token=2; prism_oai_access_token=3; cf_clearance=4; __cf_bm=5"
	if miss := MissingCookies(full); len(miss) != 0 {
		t.Errorf("齐全时不应报缺失，得到 %v", miss)
	}
	partial := "prism_session_token=2; cf_clearance=4"
	miss := MissingCookies(partial)
	if len(miss) != 3 {
		t.Errorf("应报 3 个缺失，得到 %v", miss)
	}
	found := false
	for _, m := range miss {
		if m == "oai-sc" {
			found = true
		}
	}
	if !found {
		t.Errorf("应包含 oai-sc")
	}
}

// TestCookieHeaderNormalize 验证 cookie 归一化与取值。
func TestCookieHeaderNormalize(t *testing.T) {
	raw := "a=1;\n b=2 ;\na=3"
	h := CookieHeader(raw)
	if strings.Count(h, "a=") != 1 {
		t.Errorf("应去重，得到 %q", h)
	}
	if CookieValue(raw, "b") != "2" {
		t.Errorf("应能取值 b=2，得到 %q", CookieValue(raw, "b"))
	}
}

// TestListenSnapshot 验证会话快照字段齐备（缺字段上游会 400）。
func TestListenSnapshot(t *testing.T) {
	s := listenSnapshot("u1", "p1", "cdx1_abc", "https://x/s/sandboxes/proxy/", "tok")
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"user_id", "project_id", "conversation_id", "sandbox_url",
		"sandbox_token", "workspace_session_id", "transcript_cursor", "created_at", "updated_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("快照缺少字段 %q", k)
		}
	}
	if m["workspace_session_id"] != "abc" {
		t.Errorf("workspace_session_id 应剥掉 cdx1_ 前缀，得到 %v", m["workspace_session_id"])
	}
}

// TestPollInterval 验证轮询节奏（前 3 次急 poll，之后退避封顶 2s）。
func TestPollInterval(t *testing.T) {
	if d := pollInterval(1); d.Milliseconds() != 100 {
		t.Errorf("第 1 次应 100ms，得到 %v", d)
	}
	if d := pollInterval(3); d.Milliseconds() != 100 {
		t.Errorf("第 3 次应 100ms，得到 %v", d)
	}
	if d := pollInterval(4); d.Milliseconds() != 400 {
		t.Errorf("第 4 次应 400ms，得到 %v", d)
	}
	if d := pollInterval(100); d != 2*1e9 {
		t.Logf("第 100 次=%v（应封顶 2s）", d)
	}
}

// TestStartErrClassify 验证 start 内联错误分类（请求过错不该换沙箱重试）。
func TestStartErrClassify(t *testing.T) {
	if startErrRetryable("Unsupported assistant model") {
		t.Errorf("模型不支持不应重试（换沙箱无意义）")
	}
	if !startErrRetryable("Please submit prompt again. (500)") {
		t.Errorf("上游抖动应可重试")
	}
	if startErrRetryable("context window exceeded, too long") {
		t.Errorf("上下文超长不应重试")
	}
	if startErrRetryable("Error while processing conversation (400 Bad Request)") {
		t.Errorf("400 请求过错不应重试")
	}
}
