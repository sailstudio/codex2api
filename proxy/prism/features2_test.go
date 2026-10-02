package prism

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================
// 并发 / 低延迟 / 流式 / 工具调用 的专项测试。
// ============================================================

// ───────────────────────── 并发安全 ─────────────────────────

// TestConcurrent_MaterialLoadIsSafe 并发读材料不得出现数据竞争/错误。
// 配合 -race 运行即为竞态检测。
func TestConcurrent_MaterialLoadIsSafe(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/m.json"
	if err := writeFileBytes2(p, []byte(`{
	  "captured_at": "`+time.Now().UTC().Format(time.RFC3339)+`",
	  "project_id": "p", "metadata": {"projectId":"p","sandbox_url":"u","sandbox_token":"t","model":"m","reasoning_effort":"low"},
	  "input": [{"type":"message","role":"system","content":[{"type":"input_text","text":"S"}]}]
	}`)); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.MaterialPath = p
	// 环境变量优先级高于配置，必须显式隔离，否则外部导出的
	// PRISM_MATERIAL_PATH（可能是目录）会串进本测试。
	t.Setenv("PRISM_MATERIAL_PATH", p)
	c := NewClient(cfg, Cookie{AccessToken: "x"})

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := c.material(ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发加载材料出错: %v", err)
	}
}

// TestConcurrent_SentinelPoolIsSafe 并发取 token 的**唯一性**语义验证。
//
// sentinel 严格一次性：任一时刻两路拿到同一枚 → 必有一路 403。
// hook 只设置一次（避免测试自身引入竞态），内部用原子计数保证每次唯一。
func TestConcurrent_SentinelPoolIsSafe(t *testing.T) {
	var seq int64
	SetMintHook(func(context.Context) (string, error) {
		return fmt.Sprintf("tok-%d", atomic.AddInt64(&seq, 1)), nil
	})
	defer SetMintHook(nil)

	cfg := DefaultConfig()
	c := NewClient(cfg, Cookie{AccessToken: "x"})

	const N = 50
	var wg sync.WaitGroup
	var got sync.Map
	var dup int64
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := c.Mint(context.Background())
			if err != nil || tok == "" {
				return
			}
			if _, loaded := got.LoadOrStore(tok, struct{}{}); loaded {
				atomic.AddInt64(&dup, 1)
			}
		}()
	}
	wg.Wait()

	var unique int64
	got.Range(func(_, _ any) bool { unique++; return true })
	if dup > 0 {
		t.Fatalf("并发取票出现 %d 次重复（sentinel 严格一次性，重复会 403）", dup)
	}
	if unique != N {
		t.Fatalf("应拿到 %d 枚互不相同的票，实际 %d", N, unique)
	}
}

// TestConcurrent_ClientCacheIsSafe 客户端缓存并发创建不得竞态。
func TestConcurrent_ClientsAreIsolated(t *testing.T) {
	cfg := DefaultConfig()
	var wg sync.WaitGroup
	clients := make([]*Client, 32)
	for i := range clients {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			clients[i] = NewClient(cfg, Cookie{AccessToken: fmt.Sprintf("at-%d", i)})
		}(i)
	}
	wg.Wait()
	for i, c := range clients {
		if c == nil || c.cookie.AccessToken != fmt.Sprintf("at-%d", i) {
			t.Fatalf("第 %d 个客户端凭据错配", i)
		}
	}
}

// ───────────────────────── 流式 + 低延迟 ─────────────────────────

// flushRecorder 记录每次 Flush 调用，用于验证「写即刷」。
type flushRecorder struct {
	mu      sync.Mutex
	writes  int
	flushes int
	sizes   []int
	buf     strings.Builder
}

func (f *flushRecorder) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	f.sizes = append(f.sizes, len(p))
	return f.buf.Write(p)
}
func (f *flushRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
}
func (f *flushRecorder) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String()
}

// TestStream_FlushPerEvent 每个事件后必须立即 Flush（低延迟关键）。
//
// 若省略 Flush，中间层（nginx/云 LB）会攒缓冲，首字延迟被拉长到整段结束。
func TestStream_FlushPerEvent(t *testing.T) {
	fr := &flushRecorder{}
	ew := NewEventWriter(fr, fr, "m")
	ew.WriteTurn(&Turn{Text: "你好世界", Usage: Usage{InputTokens: 1, OutputTokens: 2}}, "resp_a")

	if fr.flushes == 0 {
		t.Fatal("从未 Flush：流式会退化成攒批")
	}
	// 事件数与 Flush 数应一一对应（每个 emit 一次 Flush）
	events := strings.Count(fr.String(), "event: ")
	if fr.flushes != events {
		t.Errorf("Flush 次数(%d)应等于事件数(%d)", fr.flushes, events)
	}
	if events < 8 {
		t.Errorf("正文轮次应至少 8 个事件（created…completed），得到 %d", events)
	}
}

// TestStream_EventSequence 事件顺序必须符合 Responses 规范。
func TestStream_EventSequence(t *testing.T) {
	var sb strings.Builder
	w := &sbWriter{&sb}
	ew := NewEventWriter(w, w, "m")
	ew.WriteTurn(&Turn{Text: "hi"}, "resp_b")

	var events []string
	for _, line := range strings.Split(sb.String(), "\n") {
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}
	wantOrder := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	// 顺序必须一致（允许 delta 重复，故用子序列匹配）
	idx := 0
	for _, e := range events {
		if idx < len(wantOrder) && e == wantOrder[idx] {
			idx++
		}
	}
	if idx != len(wantOrder) {
		t.Fatalf("事件序列不完整，匹配到 %d/%d\nevents=%v", idx, len(wantOrder), events)
	}
}

// TestStream_SequenceNumbersMonotonic sequence_number 必须严格递增（下游据此排序）。
func TestStream_SequenceNumbersMonotonic(t *testing.T) {
	var sb strings.Builder
	w := &sbWriter{&sb}
	ew := NewEventWriter(w, w, "m")
	ew.WriteTurn(&Turn{Text: "abc"}, "resp_c")

	last := 0.0
	count := 0
	for _, line := range strings.Split(sb.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) != nil {
			continue
		}
		n, ok := m["sequence_number"].(float64)
		if !ok {
			t.Fatalf("事件缺 sequence_number: %v", m)
		}
		if n <= last {
			t.Fatalf("sequence_number 未递增: %v 在 %v 之后", n, last)
		}
		last = n
		count++
	}
	if count < 8 {
		t.Errorf("事件数偏少: %d", count)
	}
}

// TestStream_LowLatencyFirstEvent 首个事件必须**在整轮完成前**就可见（低延迟）。
//
// 用管道验证：WriteTurn 一开始写入 created，而 Read 端应立即拿到，
// 而不是等所有事件写完。
func TestStream_LowLatencyFirstEvent(t *testing.T) {
	pr, pw := pipePair()
	done := make(chan string, 1)

	go func() {
		ew := NewEventWriter(pw, pw, "m")
		ew.WriteTurn(&Turn{Text: "结果"}, "resp_d")
		_ = pw.Close()
	}()

	go func() {
		buf := make([]byte, 4096)
		n, _ := pr.Read(buf)
		done <- string(buf[:n])
	}()

	select {
	case first := <-done:
		if !strings.Contains(first, "response.created") {
			t.Fatalf("首批数据应含 response.created，得到: %q", first)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("5s 内未收到首批数据（流式失效）")
	}
}

// ───────────────────────── 工具调用 ─────────────────────────

// TestTools_ProtocolForbidsLocalExecution 工具协议必须明确「本地执行」约束。
func TestTools_ProtocolForbidsLocalExecution(t *testing.T) {
	s := toolProtocol([]map[string]any{{"name": "exec_command", "parameters": map[string]any{"type": "object"}}})
	for _, must := range []string{"LOCAL MACHINE", "Only the client can execute", "<tool_call>", "JSON Schema"} {
		if !strings.Contains(s, must) {
			t.Errorf("工具协议缺约束 %q\n---\n%s", must, s)
		}
	}
}

// TestTools_MultipleCallsParsed 多个工具调用必须都能解析出来。
func TestTools_MultipleCallsParsed(t *testing.T) {
	in := "先看看。\n" +
		`<tool_call>{"name":"exec_command","arguments":{"cmd":"ls -la"}}</tool_call>` + "\n" +
		`<tool_call>{"name":"read_file","arguments":{"path":"a.txt"}}</tool_call>` + "\n" +
		`<tool_call>{"name":"apply_patch","arguments":{"patch":"*** Begin Patch"}}</tool_call>`
	calls, rest := extractToolCalls(in)
	if len(calls) != 3 {
		t.Fatalf("应解析出 3 个工具调用，得到 %d", len(calls))
	}
	names := []string{calls[0].Name, calls[1].Name, calls[2].Name}
	if names[0] != "exec_command" || names[1] != "read_file" || names[2] != "apply_patch" {
		t.Fatalf("工具名解析错误: %v", names)
	}
	if !strings.Contains(calls[0].Arguments, "ls -la") {
		t.Errorf("参数解析错误: %s", calls[0].Arguments)
	}
	if strings.Contains(rest, "<tool_call>") {
		t.Errorf("正文未清干净: %q", rest)
	}
	if !strings.Contains(rest, "先看看") {
		t.Errorf("正文部分丢失: %q", rest)
	}
}

// TestTools_CallIDsUnique call_id 必须唯一（下游据此配对结果）。
func TestTools_CallIDsUnique(t *testing.T) {
	in := `<tool_call>{"name":"a","arguments":{}}</tool_call>` +
		`<tool_call>{"name":"b","arguments":{}}</tool_call>` +
		`<tool_call>{"name":"c","arguments":{}}</tool_call>`
	calls, _ := extractToolCalls(in)
	seen := map[string]bool{}
	for _, c := range calls {
		if c.CallID == "" {
			t.Fatal("call_id 为空")
		}
		if seen[c.CallID] {
			t.Fatalf("call_id 重复: %s", c.CallID)
		}
		seen[c.CallID] = true
	}
}

// TestTools_MalformedEnvelopeIgnored 非法信封必须被忽略（不能吞掉正文）。
func TestTools_MalformedEnvelopeIgnored(t *testing.T) {
	cases := []string{
		`<tool_call>not json</tool_call>`,
		`<tool_call>{"arguments":{}}</tool_call>`, // 缺 name
		`<tool_call>{"name":"x"</tool_call>`,      // 截断
		`<tool_call>{"name":"x"}` + "\n",          // 未闭合
	}
	for _, in := range cases {
		calls, _ := extractToolCalls(in)
		if len(calls) != 0 {
			t.Errorf("非法信封不应解析出调用: %q → %+v", in, calls)
		}
	}
}

// TestTools_CustomToolEventRouting 自定义工具走 custom_tool_call 事件。
func TestTools_CustomToolEventRouting(t *testing.T) {
	var sb strings.Builder
	w := &sbWriter{&sb}
	ew := NewEventWriter(w, w, "m")
	ew.WriteTurn(&Turn{
		ToolCalls: []ToolCall{
			{CallID: "c1", Name: "apply_patch", Arguments: `{"patch":"x"}`},
			{CallID: "c2", Name: "get_weather", Arguments: `{"city":"hz"}`},
		},
	}, "resp_e")
	out := sb.String()
	if !strings.Contains(out, "custom_tool_call") {
		t.Error("apply_patch 应走 custom_tool_call 事件")
	}
	if !strings.Contains(out, "function_call") {
		t.Error("普通工具应走 function_call 事件")
	}
	if !strings.Contains(out, "response.function_call_arguments.delta") {
		t.Error("普通工具应有参数增量事件")
	}
}

// TestTools_EmptyArgumentsBecomeObject 空参数应规范成 {}（下游 JSON 解析要求）。
func TestTools_EmptyArgumentsBecomeObject(t *testing.T) {
	calls, _ := extractToolCalls(`<tool_call>{"name":"ping"}</tool_call>`)
	if len(calls) != 1 || calls[0].Arguments != "{}" {
		t.Fatalf("空参数应规范为 {}，得到 %+v", calls)
	}
}

// ───────────────────────── 端到端并发（假上游） ─────────────────────────

// TestConcurrent_EndToEndUnderRace 高并发走完整链路（含 SSE/池/轮询），
// 配合 -race 即为端到端竞态检测。
//
// 注意：真实上游有「一沙箱一次一对话」限制，因此并发吞吐由**槽数**决定。
// 本测试用**单槽**但把等待超时放大，验证的是「排队不崩、最终全部成功、
// 无数据竞态、无 goroutine 泄漏」，而不是并行度。
func TestConcurrent_EndToEndUnderRace(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/session"):
			http.SetCookie(w, &http.Cookie{Name: "prism_session_token", Value: "ST"})
			_, _ = w.Write([]byte(`{"user":{"app_metadata":{"user_id":"user-x"}}}`))
		case strings.HasSuffix(r.URL.Path, "/api/projects"):
			_, _ = w.Write([]byte(`{"projects":[{"uuid":"p"}]}`))
		case strings.HasSuffix(r.URL.Path, "/response_with_tools_start"):
			_, _ = w.Write([]byte(`{"status":"started","request_id":"r1","turn_state":{"v":1}}`))
		case strings.HasSuffix(r.URL.Path, "/response_with_tools_status"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"completed","response":{"status":"success","payload":{
			  "output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],
			  "usage":{"input_tokens":1.0,"output_tokens":1.0}}}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	matPath := dir + "/m.json"
	_ = writeFileBytes2(matPath, []byte(`{"captured_at":"`+time.Now().UTC().Format(time.RFC3339)+`",
	  "project_id":"p","metadata":{"projectId":"p","sandbox_url":"u","sandbox_token":"t","model":"m","reasoning_effort":"low"},
	  "input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"S"}]}]}`))

	cfg := DefaultConfig()
	cfg.Base = srv.URL
	cfg.MaterialPath = matPath
	t.Setenv("PRISM_MATERIAL_PATH", matPath)
	cfg.Logf = func(string, ...any) {}
	SetMintHook(func(context.Context) (string, error) { return "tok", nil })
	defer SetMintHook(nil)

	c := NewClient(cfg, Cookie{AccessToken: "at", UserAgent: "ua"})
	// 单槽 + 放大等待上限：验证排队语义（而非并行度）
	if p := c.Pool(); p != nil {
		p.SetWaitTimeout(60 * time.Second)
	}

	const N = 24
	var wg sync.WaitGroup
	var okCount int64
	start := time.Now()
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			turn, err := c.Run(ctx, Request{Input: []map[string]any{sysMsgT("user", "hi")}})
			if err == nil && turn.Text == "ok" {
				atomic.AddInt64(&okCount, 1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	if okCount != N {
		t.Fatalf("并发只有 %d/%d 成功（单槽排队应最终全部成功）", okCount, N)
	}
	t.Logf("%d 路并发在单槽下排队完成，耗时 %s（%d 次上游命中）",
		N, elapsed.Round(time.Millisecond), atomic.LoadInt64(&hits))

	// 并发不应泄漏 goroutine
	runtime.GC()
	before := runtime.NumGoroutine()
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	if after > before+16 {
		t.Errorf("疑似 goroutine 泄漏: %d → %d", before, after)
	}
}
