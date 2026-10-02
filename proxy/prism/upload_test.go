package prism

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ============================================================
// 图片上传（项目文件通道）测试。
//
// 这是本轮打通「模型真能看见图片」的关键路径，涉及：
//   - 多来源形态取字节（data_url / url / file_id）
//   - 上传请求的 headers 逐项对齐（真机抓包得出）
//   - input_file 块拼装到同一条 user 消息
//   - 失败时**如实告知**（不谎称已送达）
// ============================================================

// fakeUploadUpstream 记录上传请求并回成功。
type fakeUploadUpstream struct {
	mu       sync.Mutex
	uploaded map[string][]byte // filename → bytes
	headers  []map[string]string
	mimes    []string
}

func newFakeUploadUpstream(t *testing.T) (*httptest.Server, *fakeUploadUpstream) {
	t.Helper()
	f := &fakeUploadUpstream{uploaded: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/session", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "prism_session_token", Value: "ST"})
		_, _ = io.WriteString(w, `{"user":{"app_metadata":{"user_id":"user-x"}}}`)
	})
	mux.HandleFunc("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"projects":[{"uuid":"proj-from-list"}]}`)
	})
	mux.HandleFunc("/api/project-files/upload", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		hdr := map[string]string{}
		for _, k := range []string{
			"X-Prism-File-Name", "X-Prism-File-Size", "X-Prism-File-Id",
			"X-Prism-Project-Id", "X-Prism-Require-Project-Edit-Access",
			"Content-Type", "Openai-Sentinel-Token",
		} {
			hdr[k] = r.Header.Get(k)
		}
		f.headers = append(f.headers, hdr)
		f.mimes = append(f.mimes, r.Header.Get("Content-Type"))
		f.uploaded[r.Header.Get("X-Prism-File-Name")] = body
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"success"}`)
	})
	mux.HandleFunc("/api/llm/response_with_tools_start", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.headers = append(f.headers, map[string]string{"__start_body__": string(body)})
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"status":"started","request_id":"r1","turn_state":{"v":1}}`)
	})
	mux.HandleFunc("/api/llm/response_with_tools_status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"completed","response":{"status":"success","payload":{
		  "output":[{"type":"message","content":[{"type":"output_text","text":"左上"}]}],
		  "usage":{"input_tokens":9.0,"output_tokens":2.0}}}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, f
}

// setupUploadTest 装配：假上游 + 材料（含 projectId）。
func setupUploadTest(t *testing.T) (*fakeUploadUpstream, *Client) {
	t.Helper()
	srv, f := newFakeUploadUpstream(t)

	dir := t.TempDir()
	matPath := dir + "/m.json"
	_ = writeFileBytes2(matPath, []byte(`{
	  "captured_at":"`+time.Now().UTC().Format(time.RFC3339)+`",
	  "project_id":"proj-material",
	  "metadata":{"projectId":"proj-material","userId":"user-x","model":"m",
	    "reasoning_effort":"low","sandbox_url":"https://p/s/proxy/","sandbox_token":"t",
	    "proxy_request_debug":"{}","codex_listen_snapshot":"{}"},
	  "input":[]
	}`))

	cfg := DefaultConfig()
	cfg.Base = srv.URL
	cfg.MaterialPath = matPath
	t.Setenv("PRISM_MATERIAL_PATH", matPath)
	cfg.Logf = func(string, ...any) {}
	SetBaseURL(srv.URL)
	SetMintHook(func(context.Context) (string, error) { return "fake-sentinel", nil })
	t.Cleanup(func() {
		SetBaseURL("")
		SetMintHook(nil)
	})

	c := NewClient(cfg, Cookie{AccessToken: "AT", UserAgent: "ua"})
	c.SetMaterialPool(nil) // 单份材料路径
	return f, c
}

// TestUpload_HeadersMatchRealCapture 上传请求的 headers 必须与真机抓包逐项一致。
//
// 这些 header 名是从真机捕获的（x-prism-file-name / -size / -id / -project-id /
// -require-project-edit-access + sentinel 门禁），少一个都会被上游拒。
func TestUpload_HeadersMatchRealCapture(t *testing.T) {
	f, c := setupUploadTest(t)
	data := []byte{0x89, 'P', 'N', 'G', 1, 2, 3, 4, 5}

	res, err := c.UploadFile(context.Background(), "shot.png", "image/png", data)
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if res.ProjectPath != "/prism-uploads/shot.png" {
		t.Errorf("project_path 应为 /prism-uploads/shot.png，得到 %q", res.ProjectPath)
	}
	if res.Size != len(data) {
		t.Errorf("size 应为 %d，得到 %d", len(data), res.Size)
	}
	if res.FileID == "" {
		t.Error("file_id 不应为空")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.headers) == 0 {
		t.Fatal("未收到上传请求")
	}
	h := f.headers[0]
	checks := map[string]string{
		"X-Prism-File-Name":                   "shot.png",
		"X-Prism-File-Size":                   "9",
		"X-Prism-File-Id":                     res.FileID,
		"X-Prism-Project-Id":                  "proj-material",
		"X-Prism-Require-Project-Edit-Access": "true",
		"Content-Type":                        "image/png",
		"Openai-Sentinel-Token":               "fake-sentinel",
	}
	for k, want := range checks {
		if got := h[k]; got != want {
			t.Errorf("header %s 应为 %q，得到 %q", k, want, got)
		}
	}
	// body 必须是**原始字节**（不能是 JSON/base64）
	if got := f.uploaded["shot.png"]; string(got) != string(data) {
		t.Errorf("上传 body 应为原始字节，得到 %v", got)
	}
}

// TestUpload_ProjectIDPrefersMaterial 项目 id 必须优先取**材料**里的
// （与沙箱同源），而不是列表接口的（落到别的项目会导致引用失败）。
func TestUpload_ProjectIDPrefersMaterial(t *testing.T) {
	f, c := setupUploadTest(t)
	if _, err := c.UploadFile(context.Background(), "a.png", "image/png", []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.headers[0]["X-Prism-Project-Id"]; got != "proj-material" {
		t.Errorf("应优先用材料里的 projectId（proj-material），得到 %q", got)
	}
}

// TestUpload_MimeInferredFromFilename 未给 MIME 时应按扩展名推断。
func TestUpload_MimeInferredFromFilename(t *testing.T) {
	f, c := setupUploadTest(t)
	if _, err := c.UploadFile(context.Background(), "photo.jpeg", "", []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.mimes[0]; got != "image/jpeg" {
		t.Errorf("应由扩展名推断 image/jpeg，得到 %q", got)
	}
}

// TestUpload_FilenameSanitized 文件名需安全化（路径分隔/控制字符）。
func TestUpload_FilenameSanitized(t *testing.T) {
	f, c := setupUploadTest(t)
	res, err := c.UploadFile(context.Background(), "../../etc/pa\nsswd.png", "image/png", []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(res.Filename, "/\\\n") {
		t.Errorf("文件名未安全化: %q", res.Filename)
	}
	if !strings.HasPrefix(res.ProjectPath, "/prism-uploads/") {
		t.Errorf("路径应固定在 /prism-uploads/，得到 %q", res.ProjectPath)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// 落盘名不得越出上传目录
	for name := range f.uploaded {
		if strings.ContainsAny(name, "/\\") {
			t.Errorf("上游收到的文件名含路径分隔: %q", name)
		}
	}
}

// TestUpload_RejectsEmpty 空内容必须拒绝（不产生无效请求）。
func TestUpload_RejectsEmpty(t *testing.T) {
	_, c := setupUploadTest(t)
	if _, err := c.UploadFile(context.Background(), "x.png", "image/png", nil); err == nil {
		t.Fatal("空内容应被拒绝")
	}
}

// TestUploadImages_DataURL 走完整图片转换链路（data URL → 上传 → input_file）。
func TestUploadImages_DataURL(t *testing.T) {
	f, c := setupUploadTest(t)
	data := []byte{0x89, 'P', 'N', 'G', 9, 8, 7}
	durl := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)

	blocks, failures := c.UploadImages(context.Background(), []ImagePart{
		{Kind: "data_url", Value: durl, MediaType: "image/png"},
	})
	if len(failures) != 0 {
		t.Fatalf("不应有失败: %v", failures)
	}
	if len(blocks) != 1 {
		t.Fatalf("应产出 1 个 input_file 块，得到 %d", len(blocks))
	}
	b := blocks[0]
	if b["type"] != "input_file" {
		t.Errorf("块类型应为 input_file（真机形态），得到 %v", b["type"])
	}
	if b["filename"] == nil || b["project_path"] == nil {
		t.Errorf("块缺 filename/project_path: %+v", b)
	}
	pp, _ := b["project_path"].(string)
	if !strings.HasPrefix(pp, "/prism-uploads/") {
		t.Errorf("project_path 应在 /prism-uploads/ 下: %q", pp)
	}
	// 上传内容必须与原字节一致
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.uploaded) != 1 {
		t.Fatalf("应有 1 次上传，得到 %d", len(f.uploaded))
	}
	for _, got := range f.uploaded {
		if string(got) != string(data) {
			t.Errorf("上传字节不符: %v", got)
		}
	}
}

// TestUploadImages_FileIDFailsHonestly file_id 无法取内容时必须**如实失败**
// （不能假装成功 → 模型会对着不存在的图胡编）。
func TestUploadImages_FileIDFailsHonestly(t *testing.T) {
	_, c := setupUploadTest(t)
	blocks, failures := c.UploadImages(context.Background(), []ImagePart{
		{Kind: "file_id", Value: "file-abc"},
	})
	if len(blocks) != 0 {
		t.Error("file_id 不应产出成功块")
	}
	if len(failures) != 1 {
		t.Fatalf("应记录 1 条失败，得到 %d", len(failures))
	}
	if !strings.Contains(failures[0], "file_id") {
		t.Errorf("失败说明应指明原因: %s", failures[0])
	}
}

// TestUploadImages_PartialFailureReports 部分成功时：成功的送达、失败的如实说明。
func TestUploadImages_PartialFailureReports(t *testing.T) {
	_, c := setupUploadTest(t)
	durl := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{1, 2, 3})
	imgs := []ImagePart{
		{Kind: "data_url", Value: durl, MediaType: "image/png"},
		{Kind: "file_id", Value: "file-xyz"},
	}
	blocks, failures := c.UploadImages(context.Background(), imgs)
	if len(blocks) != 1 || len(failures) != 1 {
		t.Fatalf("应 1 成功 1 失败，得到 %d/%d", len(blocks), len(failures))
	}
	notice := imageNoticeForFailures(imgs, failures, len(blocks))
	if !strings.Contains(notice, "CAN see them") {
		t.Errorf("说明应告诉模型已送达的能看见: %s", notice)
	}
	if !strings.Contains(notice, "CANNOT see them") {
		t.Errorf("说明应告诉模型未送达的看不见: %s", notice)
	}
	if !strings.Contains(notice, "Do not guess") {
		t.Errorf("说明应禁止猜测: %s", notice)
	}
}

// TestUploadImages_AllDeliveredNoNotice 全部成功时不得注入任何说明（不打扰模型）。
func TestUploadImages_AllDeliveredNoNotice(t *testing.T) {
	if n := imageNoticeForFailures([]ImagePart{{Kind: "data_url"}}, nil, 1); n != "" {
		t.Errorf("全部成功时不应有说明，得到: %q", n)
	}
}

// TestAttachImagesToLastUserMessage input_file 必须附到**最后一条 user 消息**
// （上游要求文件引用与文本同处一条消息）。
func TestAttachImagesToLastUserMessage(t *testing.T) {
	input := []map[string]any{
		sysMsgT("system", "S"),
		sysMsgT("user", "第一条"),
		sysMsgT("user", "第二条"),
	}
	blocks := []map[string]any{{"type": "input_file", "filename": "a.png", "project_path": "/prism-uploads/a.png"}}
	input = attachImagesToInput(input, blocks)

	last := input[2]
	content, _ := last["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("最后一条 user 应有 2 个 content 项（文本+文件），得到 %d", len(content))
	}
	// 第二条不应被动（不能附加到旧消息上）
	prev, _ := input[1]["content"].([]any)
	if len(prev) != 1 {
		t.Errorf("不应修改更早的 user 消息，得到 %d 项", len(prev))
	}
}

// TestAttachImages_NoUserMessageCreatesOne 没有 user 消息时新建一条文件消息。
func TestAttachImages_NoUserMessageCreatesOne(t *testing.T) {
	input := []map[string]any{sysMsgT("system", "S")}
	blocks := []map[string]any{{"type": "input_file", "filename": "a.png", "project_path": "/p/a.png"}}
	input = attachImagesToInput(input, blocks)
	if len(input) != 2 || input[1]["role"] != "user" {
		t.Fatalf("应新建一条 user 消息，得到 %+v", input)
	}
}

// TestUpload_EndToEndImageVisible 端到端：带图请求 → start body 里出现
// input_file（真机形态）→ 模型看到图（假上游回「左上」）。
func TestUpload_EndToEndImageVisible(t *testing.T) {
	f, c := setupUploadTest(t)
	data := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	durl := "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)

	turn, err := c.Run(context.Background(), Request{
		Input: []map[string]any{{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "黑块在哪？"}},
		}},
		Images: []ImagePart{{Kind: "data_url", Value: durl, MediaType: "image/png"}},
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if turn.Text != "左上" {
		t.Errorf("正文应为「左上」，得到 %q", turn.Text)
	}

	// start body 必须把图片 **base64 内联**进去（真机唯一走得通的路），
	// 并给出「落盘命令 + view_image 指引」。input_file/input_image 都不行。
	f.mu.Lock()
	defer f.mu.Unlock()
	var startBody string
	for _, h := range f.headers {
		if b, ok := h["__start_body__"]; ok {
			startBody = b
		}
	}
	if startBody == "" {
		t.Fatal("未捕获 start body")
	}
	// ① 图片内容必须真在请求里（base64 原文）
	b64 := base64.StdEncoding.EncodeToString(data)
	if !strings.Contains(startBody, b64) {
		t.Errorf("start body 应内联图片 base64（%s）: %s", b64, truncStr(startBody, 600))
	}
	// ② 必须给出还原命令与 view_image 指引，否则模型看不到图
	for _, want := range []string{"base64 -d", inlineImageRestoreDir, "view_image", "input_text"} {
		if !strings.Contains(startBody, want) {
			t.Errorf("start body 应含 %q: %s", want, truncStr(startBody, 600))
		}
	}
	// ③ 不得再走已被证伪的两条路
	for _, bad := range []string{`"input_image"`, `"input_file"`} {
		if strings.Contains(startBody, bad) {
			t.Errorf("不应再传 %s（上游不消费/模型看不到）: %s", bad, truncStr(startBody, 600))
		}
	}
	// ④ 内联说明必须与用户文本同处一条 user 消息
	var doc struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal([]byte(startBody), &doc); err != nil {
		t.Fatalf("start body 解析失败: %v", err)
	}
	found := false
	for _, it := range doc.Input {
		if it["role"] != "user" {
			continue
		}
		cs, _ := it["content"].([]any)
		hasPrompt, hasInline := false, false
		for _, c2 := range cs {
			cm, _ := c2.(map[string]any)
			if cm["type"] != "input_text" {
				continue
			}
			txt, _ := cm["text"].(string)
			if txt == "黑块在哪？" {
				hasPrompt = true
			}
			if strings.Contains(txt, b64) {
				hasInline = true
			}
		}
		if hasPrompt && hasInline {
			found = true
		}
	}
	if !found {
		t.Error("内联图片说明必须与用户文本同处一条 user 消息（上游要求）")
	}
}

// TestImageBytes_URLRejectsNonHTTP 远程图片只允许 http(s)。
func TestImageBytes_URLRejectsNonHTTP(t *testing.T) {
	_, c := setupUploadTest(t)
	for _, bad := range []string{"file:///etc/passwd", "ftp://x/y.png", "javascript:alert(1)"} {
		if _, _, err := c.imageBytes(context.Background(), ImagePart{Kind: "url", Value: bad}, 1<<20); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

// TestImageBytes_URLSizeLimit 远程图片超限必须拒绝（避免拖垮网关）。
func TestImageBytes_URLSizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(make([]byte, 5000))
	}))
	defer srv.Close()

	_, c := setupUploadTest(t)
	_, _, err := c.imageBytes(context.Background(), ImagePart{Kind: "url", Value: srv.URL + "/big.png"}, 1000)
	if err == nil {
		t.Fatal("超限图片应被拒绝")
	}
	if !strings.Contains(err.Error(), "过大") {
		t.Errorf("错误应说明过大: %v", err)
	}
}
