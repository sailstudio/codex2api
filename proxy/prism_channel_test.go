package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/prism"
)

// ============================================================
// Prism 渠道接线测试。
//
// 目标：证明 relay-style 汇合点（ExecuteRelayStyleProtocolRequest）确实把
// prism 账号分流到 Prism 适配器，且正文以 **Responses SSE** 形态抵达下游
// —— 即「下游零改动」这一架构承诺成立。
//
// 手法：把 prism 包的上游基址指向假上游（httptest），并用 mint 钩子跳过
// 真实 sentinel 铸造（需要 node/网络），其余全部走真实代码路径。
// ============================================================

// newFakePrismUpstream 模拟 prism.openai.com 的必要端点。
func newFakePrismUpstream(text string) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/session", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "prism_session_token", Value: "ST_FAKE"})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"user":{"id":"u1","app_metadata":{"user_id":"user-fake"}},"policy":{"user":{"openai_user_id":"user-fake"}}}`)
	})
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"projects":[{"uuid":"proj-fake"}]}`)
	})
	mux.HandleFunc("/api/llm/response_with_tools_start", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"started","request_id":"req_fake","turn_state":{"version":1,"conversation_id":"cdx1_fake"}}`)
	})
	mux.HandleFunc("/api/llm/response_with_tools_status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{
			"status": "completed",
			"response": map[string]any{
				"status": "success",
				"payload": map[string]any{
					"output": []any{map[string]any{
						"type": "message", "role": "assistant", "status": "completed",
						"content": []any{map[string]any{"type": "output_text", "text": text}},
					}},
					"usage": map[string]any{
						"input_tokens": 7.0, "output_tokens": 3.0, "total_tokens": 10.0,
						"cached_input_tokens": 4.0, "reasoning_output_tokens": 1.0,
					},
				},
			},
		})
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {})
	return httptest.NewServer(mux)
}

// setupPrismTest 装配测试环境：假上游 + 材料文件 + sentinel 钩子。
func setupPrismTest(t *testing.T, text string) (*httptest.Server, *auth.Account) {
	t.Helper()
	srv := newFakePrismUpstream(text)

	dir := t.TempDir()
	matPath := dir + "/material.json"
	content := `{
	  "captured_at": "` + time.Now().UTC().Format(time.RFC3339) + `",
	  "project_id": "proj-fake",
	  "metadata": {
	    "projectId":"proj-fake","userId":"user-fake","model":"gpt-5.6-sol",
	    "reasoning_effort":"medium",
	    "sandbox_url":"https://prism.openai.com/s/sandboxes/proxy/",
	    "sandbox_token":"tok","proxy_request_debug":"{}","codex_listen_snapshot":"{}"
	  },
	  "input": [{"type":"message","role":"system","content":[{"type":"input_text","text":"SYS"}]}]
	}`
	if err := os.WriteFile(matPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PRISM_MATERIAL_PATH", matPath)
	t.Setenv("PRISM_BASE_URL", srv.URL)
	prism.SetBaseURL(srv.URL)
	prism.SetMintHook(func(context.Context) (string, error) { return "fake-sentinel", nil })
	t.Cleanup(func() {
		prism.SetBaseURL("")
		prism.SetMintHook(nil)
		// 清空 client 缓存，避免跨测试污染
		prismClientsMu.Lock()
		for k, c := range prismClients {
			c.Close()
			delete(prismClients, k)
		}
		prismClientsMu.Unlock()
		srv.Close()
	})

	return srv, newPrismTestAccount()
}

func newPrismTestAccount() *auth.Account {
	return &auth.Account{
		DBID:         99001,
		UpstreamType: auth.UpstreamPrism,
		AccessToken:  "AT_FAKE",
	}
}

// TestPrismChannel_Awareness 渠道判定：prism 不应被误判，也不误判别人。
func TestPrismChannel_Awareness(t *testing.T) {
	acc := newPrismTestAccount()
	if !acc.IsPrismAPI() || !acc.IsRelayStyle() {
		t.Fatal("prism 账号应为 prism + relay-style（否则 handler 不走 relay 分支）")
	}
	if acc.IsGrokAPI() || acc.IsAntigravityAPI() || acc.IsClaudeOAuth() {
		t.Fatal("prism 账号被误判为其它渠道")
	}
	if ua := acc.PrismUserAgent(); !strings.Contains(ua, "codex-tui") {
		t.Fatalf("Prism UA 默认值异常: %q", ua)
	}
	if (&auth.Account{DBID: 99002, UpstreamType: auth.UpstreamOpenAIResponses, AccessToken: "x"}).IsPrismAPI() {
		t.Fatal("非 prism 账号被误判为 prism")
	}
	if (&auth.Account{DBID: 99003, UpstreamType: auth.UpstreamPrism}).IsPrismAPI() {
		t.Fatal("缺 access_token 的 prism 账号不应判为可用")
	}
}

// TestPrismChannel_RouteDispatchesToPrism 汇合点分流：prism 账号必须被
// ExecuteRelayStyleProtocolRequest 送到 Prism 适配器（而不是 openai_responses）。
//
// 判据：假上游被真实打到（start 端点命中），且返回体是 SSE。
func TestPrismChannel_RouteDispatchesToPrism(t *testing.T) {
	_, acc := setupPrismTest(t, "收到了")

	reqBody := []byte(`{"model":"gpt-5.6-sol","input":"只回复三个字：收到了"}`)
	hdr := http.Header{}
	resp, err := ExecuteRelayStyleProtocolRequest(context.Background(), acc,
		GrokProtocolResponses, reqBody, reqBody, "", hdr)
	if err != nil {
		t.Fatalf("分叉执行失败: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应为 text/event-stream，得到 %q", ct)
	}
	if got := resp.Header.Get("X-Prism-Request-Id"); !strings.HasPrefix(got, "resp_prism_") {
		t.Fatalf("缺 X-Prism-Request-Id 或形态异常: %q", got)
	}

	events, deltas := readSSE(t, resp.Body)
	joined := strings.Join(events, ",")
	for _, want := range []string{"response.created", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺事件 %s（实际: %s）", want, joined)
		}
	}
	if got := strings.Join(deltas, ""); got != "收到了" {
		t.Errorf("正文应为「收到了」，得到 %q", got)
	}
}

// TestPrismChannel_UsagePropagated 用量（含 cache/思考）必须透出到终态事件。
func TestPrismChannel_UsagePropagated(t *testing.T) {
	_, acc := setupPrismTest(t, "hi")
	reqBody := []byte(`{"input":"hi"}`)
	resp, err := ExecutePrismResponsesRequest(context.Background(), acc, reqBody)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw := readAll(t, resp.Body)
	if !strings.Contains(raw, `"input_tokens":7`) {
		t.Errorf("input_tokens 未透出: %s", truncate(raw, 400))
	}
	if !strings.Contains(raw, `"cached_tokens":4`) {
		t.Errorf("cached_tokens（读缓存）未透出: %s", truncate(raw, 400))
	}
	if !strings.Contains(raw, `"reasoning_tokens":1`) {
		t.Errorf("reasoning_tokens 未透出: %s", truncate(raw, 400))
	}
}

// TestPrismRequestFromResponses 请求翻译：Responses 体 → Prism 请求。
func TestPrismRequestFromResponses(t *testing.T) {
	acc := newPrismTestAccount()
	body := []byte(`{
	  "model": "gpt-5.6-sol",
	  "instructions": "你是助手",
	  "input": [
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"你好"}]},
	    {"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"ls\"}"},
	    {"type":"function_call_output","output":"file.txt"},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}
	  ],
	  "tools": [{"type":"function","name":"exec_command"}]
	}`)
	req, responseID, err := prismRequestFromResponses(acc, body)
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	if req.Model != "gpt-5.6-sol" || req.InstructionsText != "你是助手" {
		t.Fatalf("model/instructions 翻译错误: %+v", req)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("tools 未透传: %+v", req.Tools)
	}
	if !strings.HasPrefix(responseID, "resp_prism_") {
		t.Fatalf("responseID 形态异常: %q", responseID)
	}
	if len(req.Input) != 4 {
		b, _ := json.Marshal(req.Input)
		t.Fatalf("input 条目数应为 4，得到 %d: %s", len(req.Input), b)
	}
}

// TestPrismRequestFromResponses_ImagesCollected 图片必须被收集到 Request.Images
// （上游私协议不消费图片内容，由 prism 层如实告知模型；此处锁定接线不丢图）。
func TestPrismRequestFromResponses_ImagesCollected(t *testing.T) {
	acc := newPrismTestAccount()
	body := []byte(`{
	  "input": [{
	    "type":"message","role":"user",
	    "content":[
	      {"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo=","detail":"high"},
	      {"type":"input_image","file_id":"file-xyz"},
	      {"type":"input_text","text":"这张图里有几个人？"}
	    ]
	  }]
	}`)
	req, _, err := prismRequestFromResponses(acc, body)
	if err != nil {
		t.Fatalf("翻译失败: %v", err)
	}
	if len(req.Images) != 2 {
		t.Fatalf("应收集到 2 张图片，得到 %d（%+v）", len(req.Images), req.Images)
	}
	if req.Images[0].Kind != "data_url" || req.Images[0].MediaType != "image/png" {
		t.Errorf("第 1 张图片解析错误: %+v", req.Images[0])
	}
	if req.Images[1].Kind != "file_id" || req.Images[1].Value != "file-xyz" {
		t.Errorf("第 2 张图片解析错误: %+v", req.Images[1])
	}
	// 文本仍须保留（不能因为有图就丢掉问题）
	if len(req.Input) != 1 {
		t.Fatalf("应保留 1 条消息，得到 %d", len(req.Input))
	}
	content, _ := req.Input[0]["content"].([]any)
	if len(content) == 0 {
		t.Fatal("消息内容为空")
	}
	part, _ := content[0].(map[string]any)
	if !strings.Contains(strOfAny(part["text"]), "几个人") {
		t.Errorf("文本内容丢失: %+v", part)
	}
}

// TestPrismRequestFromResponses_ImageOnlyMessage 纯图片消息必须有占位文本，
// 否则上游会把该消息视为空。
func TestPrismRequestFromResponses_ImageOnlyMessage(t *testing.T) {
	acc := newPrismTestAccount()
	body := []byte(`{"input":[{"type":"message","role":"user","content":[
	  {"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]}]}`)
	req, _, err := prismRequestFromResponses(acc, body)
	if err != nil {
		t.Fatalf("纯图片消息应可处理，却失败: %v", err)
	}
	if len(req.Images) != 1 {
		t.Fatalf("应收集 1 张图片，得到 %d", len(req.Images))
	}
	if len(req.Input) != 1 {
		t.Fatalf("应产生 1 条占位消息，得到 %d", len(req.Input))
	}
}

func strOfAny(v any) string {
	s, _ := v.(string)
	return s
}

// TestPrismRequestFromResponses_StringInput 纯字符串 input。
func TestPrismRequestFromResponses_StringInput(t *testing.T) {
	req, _, err := prismRequestFromResponses(newPrismTestAccount(), []byte(`{"input":"直接一句"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 1 {
		t.Fatalf("应产出 1 条，得到 %d", len(req.Input))
	}
}

// TestPrismRequestFromResponses_RejectsEmpty 空 input 必须快速失败。
func TestPrismRequestFromResponses_RejectsEmpty(t *testing.T) {
	if _, _, err := prismRequestFromResponses(newPrismTestAccount(), []byte(`{"model":"m"}`)); err == nil {
		t.Fatal("空 input 应报错")
	}
}

// TestPrismChannel_RejectsNonPrismAccount 非 prism 账号不得进 Prism 适配器。
func TestPrismChannel_RejectsNonPrismAccount(t *testing.T) {
	plain := &auth.Account{DBID: 99009, UpstreamType: auth.UpstreamOpenAIResponses, AccessToken: "x"}
	if _, err := ExecutePrismResponsesRequest(context.Background(), plain, []byte(`{"input":"hi"}`)); err == nil {
		t.Fatal("非 prism 账号应被拒绝")
	}
}

// ------------------------------------------------------------------ 辅助

func readSSE(t *testing.T, r io.Reader) (events []string, deltas []string) {
	t.Helper()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
		if strings.HasPrefix(line, "data: ") {
			var m map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) == nil {
				if d, ok := m["delta"].(string); ok && m["type"] == "response.output_text.delta" {
					deltas = append(deltas, d)
				}
			}
		}
	}
	return events, deltas
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
