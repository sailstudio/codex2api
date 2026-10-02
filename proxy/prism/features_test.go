package prism

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// ============================================================
// 六项原始需求的专项测试：并发 / 低延迟 / 工具 / 流式 / 图片 / 缓存计数。
// ============================================================

// ───────────────────────── 图片解析 ─────────────────────────

// TestImage_DataURLParsed data URL 形态解析（含 MIME 与字节估算）。
func TestImage_DataURLParsed(t *testing.T) {
	raw := []byte{0x89, 'P', 'N', 'G', 1, 2, 3, 4, 5, 6, 7, 8}
	durl := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	img, ok := ParseImagePart(map[string]any{"type": "input_image", "image_url": durl, "detail": "high"})
	if !ok {
		t.Fatal("data URL 图片应被解析")
	}
	if img.Kind != "data_url" || img.MediaType != "image/png" || img.Detail != "high" {
		t.Fatalf("解析结果错误: %+v", img)
	}
}

// TestImage_HTTPURLParsed http URL 形态。
func TestImage_HTTPURLParsed(t *testing.T) {
	img, ok := ParseImagePart(map[string]any{"image_url": "https://example.com/a.jpg"})
	if !ok || img.Kind != "url" {
		t.Fatalf("http URL 应解析为 url，得到 %+v ok=%v", img, ok)
	}
}

// TestImage_FileIDParsed file_id 形态（OpenAI Files 引用）。
func TestImage_FileIDParsed(t *testing.T) {
	img, ok := ParseImagePart(map[string]any{"file_id": "file-abc123"})
	if !ok || img.Kind != "file_id" || img.Value != "file-abc123" {
		t.Fatalf("file_id 应解析，得到 %+v ok=%v", img, ok)
	}
}

// TestImage_NestedURLParsed 旧/嵌套形态 image_url:{url}。
func TestImage_NestedURLParsed(t *testing.T) {
	img, ok := ParseImagePart(map[string]any{"image_url": map[string]any{"url": "data:image/jpeg;base64,AAAA"}})
	if !ok || img.Kind != "data_url" || img.MediaType != "image/jpeg" {
		t.Fatalf("嵌套形态应解析，得到 %+v ok=%v", img, ok)
	}
}

// TestImage_RejectsNonImage 非图片内容块必须被拒绝（不能误吞文本）。
func TestImage_RejectsNonImage(t *testing.T) {
	for _, part := range []map[string]any{
		nil,
		{"type": "input_text", "text": "hello"},
		{"type": "input_image"},
		{},
	} {
		if _, ok := ParseImagePart(part); ok {
			t.Errorf("非图片内容不应被解析: %+v", part)
		}
	}
}

// TestImage_Summarize 汇总统计（种类 / 字节 / MIME）。
func TestImage_Summarize(t *testing.T) {
	raw := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	durl := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
	imgs := []ImagePart{
		{Kind: "data_url", Value: durl, MediaType: "image/png"},
		{Kind: "url", Value: "https://x/y.png"},
		{Kind: "file_id", Value: "f1"},
	}
	st := SummarizeImages(imgs)
	if st.Count != 3 || st.Kinds["data_url"] != 1 || st.Kinds["url"] != 1 || st.Kinds["file_id"] != 1 {
		t.Fatalf("汇总错误: %+v", st)
	}
	if st.BytesTotal != len(raw) {
		t.Fatalf("字节估算错误: 期望 %d 得到 %d", len(raw), st.BytesTotal)
	}
}

// TestImage_NoticeIsHonest 能力说明必须**如实**（不得暗示模型能看图）。
//
// 这是本轮最重要的"诚实性"断言：上游私协议不消费 data URL 图片，
// 提示词必须明确告知模型看不见，避免它对着不存在的图像胡编。
func TestImage_NoticeIsHonest(t *testing.T) {
	raw := []byte{1, 2, 3, 4}
	imgs := []ImagePart{{Kind: "data_url", Value: "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)}}
	n := imageNotice(imgs)
	for _, must := range []string{"1 image", "CANNOT see", "Do not guess"} {
		if !strings.Contains(n, must) {
			t.Errorf("能力说明缺关键约束 %q: %s", must, n)
		}
	}
	// 无图片时必须为空（不能凭空注入）
	if imageNotice(nil) != "" {
		t.Error("无图片时不应产生说明")
	}
}

// ───────────────────────── 缓存计数 ─────────────────────────

// TestUsage_CacheReadWrite 读缓存 + 写缓存都要正确解析。
func TestUsage_CacheReadWrite(t *testing.T) {
	u := parseUsage(map[string]any{
		"input_tokens": 100.0, "output_tokens": 20.0, "total_tokens": 120.0,
		"cached_input_tokens":         60.0,
		"cache_creation_input_tokens": 15.0,
		"reasoning_output_tokens":     7.0,
	})
	if u.CachedInputTokens != 60 {
		t.Errorf("读缓存应为 60，得到 %d", u.CachedInputTokens)
	}
	if u.CacheWriteTokens != 15 || !u.CacheWriteReported {
		t.Errorf("写缓存应为 15 且标记已上报，得到 %d reported=%v", u.CacheWriteTokens, u.CacheWriteReported)
	}
	if u.ReasoningTokens != 7 {
		t.Errorf("思考 token 应为 7，得到 %d", u.ReasoningTokens)
	}
}

// TestUsage_DistinguishesAbsentFromZero 未上报写缓存 ≠ 写缓存为 0。
//
// 这个区分直接决定计费口径：若把"上游没说"当成 0，会把缓存写成本算漏。
func TestUsage_DistinguishesAbsentFromZero(t *testing.T) {
	absent := parseUsage(map[string]any{"input_tokens": 10.0})
	if absent.CacheWriteReported {
		t.Error("未上报写缓存时 CacheWriteReported 应为 false")
	}
	explicitZero := parseUsage(map[string]any{"input_tokens": 10.0, "cache_creation_input_tokens": 0.0})
	if !explicitZero.CacheWriteReported {
		t.Error("显式上报 0 时 CacheWriteReported 应为 true")
	}
}

// TestUsage_AliasCompatibility 上游字段名会变，别名必须兼容。
func TestUsage_AliasCompatibility(t *testing.T) {
	// 读缓存别名
	for _, k := range []string{"cached_input_tokens", "cache_read_input_tokens", "cached_tokens"} {
		u := parseUsage(map[string]any{k: 33.0})
		if u.CachedInputTokens != 33 {
			t.Errorf("别名 %s 未生效（得到 %d）", k, u.CachedInputTokens)
		}
	}
	// 写缓存别名
	for _, k := range []string{"cache_creation_input_tokens", "cache_write_input_tokens"} {
		u := parseUsage(map[string]any{k: 44.0})
		if u.CacheWriteTokens != 44 || !u.CacheWriteReported {
			t.Errorf("别名 %s 未生效（得到 %d）", k, u.CacheWriteTokens)
		}
	}
	// 输入/输出别名（OpenAI chat 风格）
	u := parseUsage(map[string]any{"prompt_tokens": 5.0, "completion_tokens": 6.0})
	if u.InputTokens != 5 || u.OutputTokens != 6 {
		t.Errorf("prompt/completion 别名未生效: %+v", u)
	}
}

// TestUsage_NestedDetails Responses 风格嵌套明细。
func TestUsage_NestedDetails(t *testing.T) {
	u := parseUsage(map[string]any{
		"input_tokens":          10.0,
		"output_tokens":         2.0,
		"input_tokens_details":  map[string]any{"cached_tokens": 8.0, "cache_creation_tokens": 3.0},
		"output_tokens_details": map[string]any{"reasoning_tokens": 1.0},
	})
	if u.CachedInputTokens != 8 || u.CacheWriteTokens != 3 || u.ReasoningTokens != 1 {
		t.Fatalf("嵌套明细解析错误: %+v", u)
	}
}

// TestUsage_EffectiveInputInvariant 不变量：新输入+读+写 = 总输入。
func TestUsage_EffectiveInputInvariant(t *testing.T) {
	u := Usage{InputTokens: 10, CachedInputTokens: 60, CacheWriteTokens: 30}
	if u.EffectiveInput() != 100 {
		t.Errorf("EffectiveInput 应为 100，得到 %d", u.EffectiveInput())
	}
	if r := u.CacheHitRate(); r != 0.6 {
		t.Errorf("命中率应为 0.6，得到 %v", r)
	}
	// 无输入不 panic
	var zero Usage
	if zero.EffectiveInput() != 0 || zero.CacheHitRate() != 0 {
		t.Error("零值 usage 应安全返回 0")
	}
}

// TestUsage_RawPreserved 原文必须保留（便于对账/排查字段名差异）。
func TestUsage_RawPreserved(t *testing.T) {
	src := map[string]any{"input_tokens": 1.0, "weird_field_xyz": 9.0}
	u := parseUsage(src)
	if u.Raw == nil || u.Raw["weird_field_xyz"] != 9.0 {
		t.Fatal("usage 原文未被保留")
	}
}

// TestUsage_TotalFallback total 缺失时自算。
func TestUsage_TotalFallback(t *testing.T) {
	u := parseUsage(map[string]any{"input_tokens": 7.0, "output_tokens": 3.0})
	if u.TotalTokens != 10 {
		t.Errorf("total 缺失应自算为 10，得到 %d", u.TotalTokens)
	}
}

// TestUsage_EmittedInSSE 读/写缓存与汇总口径必须出现在 SSE 终态事件里。
func TestUsage_EmittedInSSE(t *testing.T) {
	turn := &Turn{
		Text: "ok",
		Usage: Usage{
			InputTokens: 10, OutputTokens: 2, TotalTokens: 102,
			CachedInputTokens: 60, CacheWriteTokens: 30, CacheWriteReported: true,
			ReasoningTokens: 1,
			Reported:        true, // 上游确有上报（真实 Prism 私协议不给 usage）
		},
	}
	var sb strings.Builder
	w := &sbWriter{&sb}
	ew := NewEventWriter(w, w, "m")
	ew.WriteTurn(turn, "resp_x")
	out := sb.String()

	var completed map[string]any
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) != nil {
			continue
		}
		if m["type"] == "response.completed" {
			completed, _ = m["response"].(map[string]any)
		}
	}
	if completed == nil {
		t.Fatal("未找到 response.completed")
	}
	usage, _ := completed["usage"].(map[string]any)
	if usage == nil {
		t.Fatal("终态缺 usage")
	}
	det, _ := usage["input_tokens_details"].(map[string]any)
	if det["cached_tokens"] != float64(60) {
		t.Errorf("读缓存未透出: %+v", det)
	}
	if det["cache_creation_tokens"] != float64(30) {
		t.Errorf("写缓存未透出: %+v", det)
	}
	if det["cache_write_reported"] != true {
		t.Errorf("写缓存上报标记未透出: %+v", det)
	}
	pr, _ := usage["prism"].(map[string]any)
	if pr["cache_read_tokens"] != float64(60) || pr["cache_write_tokens"] != float64(30) {
		t.Errorf("汇总口径未透出: %+v", pr)
	}
	if pr["cache_hit_rate"] != 0.6 {
		t.Errorf("命中率应为 0.6，得到 %v", pr["cache_hit_rate"])
	}
}

// TestUsage_NotReportedIsHonest 上游未上报 usage 时，终态事件必须如实标注
// reported=false 且计数字段为 null —— 绝不能用零值冒充真实用量（那等于伪造
// 计费数据）。实测 prism.openai.com 私协议终态 payload 就不含 usage 字段。
func TestUsage_NotReportedIsHonest(t *testing.T) {
	var sb strings.Builder
	w := &sbWriter{&sb}
	ew := NewEventWriter(w, w, "m")
	// 零值 Usage：Reported=false，全部计数为 0。
	ew.WriteTurn(&Turn{Text: "ok"}, "resp_x")

	var completed map[string]any
	for _, line := range strings.Split(sb.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &m) != nil {
			continue
		}
		if m["type"] == "response.completed" {
			completed, _ = m["response"].(map[string]any)
		}
	}
	if completed == nil {
		t.Fatal("未找到 response.completed")
	}
	usage, _ := completed["usage"].(map[string]any)
	if usage == nil {
		t.Fatal("终态缺 usage（应给出带 reported=false 的说明块）")
	}
	if usage["reported"] != false {
		t.Errorf("未上报时必须 reported=false，得到 %v", usage["reported"])
	}
	if usage["input_tokens"] != nil || usage["output_tokens"] != nil || usage["total_tokens"] != nil {
		t.Errorf("未上报时计数字段必须为 null，不能冒充 0：%+v", usage)
	}
	if usage["prism"] != nil {
		t.Errorf("未上报时不得给出汇总口径：%+v", usage["prism"])
	}
}
