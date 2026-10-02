package prism

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"strings"
	"testing"
)

// makePNG 生成一张可解码的 PNG（w×h，随机噪点以保证不可压缩 → 体积真实）。
func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(42))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), uint8(rnd.Intn(256)), 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("生成 PNG 失败: %v", err)
	}
	return buf.Bytes()
}

func dataURL(b []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b)
}

// TestPrepareInlineImages_SmallStaysInline 小图应原样内联（不缩图）。
func TestPrepareInlineImages_SmallStaysInline(t *testing.T) {
	_, c := setupUploadTest(t)
	raw := makePNG(t, 32, 32)
	got, failures := c.PrepareInlineImages(context.Background(), []ImagePart{
		{Kind: "data_url", Value: dataURL(raw), MediaType: "image/png"},
	})
	if len(failures) != 0 {
		t.Fatalf("小图不应有失败: %v", failures)
	}
	if len(got) != 1 {
		t.Fatalf("应有 1 张内联图片，得到 %d", len(got))
	}
	if got[0].B64 != base64.StdEncoding.EncodeToString(raw) {
		t.Error("小图应原样内联（未经缩图）")
	}
	if got[0].RawBytes != len(raw) {
		t.Errorf("RawBytes 应为 %d，得到 %d", len(raw), got[0].RawBytes)
	}
	// 必须真的在预算内，否则内联会撑爆上游
	if got[0].charsOf() > inlineImageBudgetChars {
		t.Errorf("内联字符数 %d 超过单图预算 %d", got[0].charsOf(), inlineImageBudgetChars)
	}
}

// TestPrepareInlineImages_OversizeDownscaled 超预算的大图必须缩到预算内（否则上游 502）。
func TestPrepareInlineImages_OversizeDownscaled(t *testing.T) {
	_, c := setupUploadTest(t)
	raw := makePNG(t, 1600, 1600) // 噪点图，远大于单图预算
	if base64.StdEncoding.EncodeToString(raw) == "" {
		t.Fatal("前置条件失败")
	}
	origChars := len(base64.StdEncoding.EncodeToString(raw))
	if origChars <= inlineImageBudgetChars {
		t.Skipf("生成的图未超预算（%d 字符），跳过", origChars)
	}

	got, failures := c.PrepareInlineImages(context.Background(), []ImagePart{
		{Kind: "data_url", Value: dataURL(raw), MediaType: "image/png"},
	})
	if len(failures) != 0 {
		t.Fatalf("可缩图的图不应失败: %v", failures)
	}
	if len(got) != 1 {
		t.Fatalf("应有 1 张内联图片，得到 %d", len(got))
	}
	if got[0].charsOf() > inlineImageBudgetChars {
		t.Errorf("缩图后仍超预算: %d > %d", got[0].charsOf(), inlineImageBudgetChars)
	}
	if got[0].RawBytes >= len(raw) {
		t.Errorf("应真的缩小（原始 %d 字节 → 得到 %d 字节）", len(raw), got[0].RawBytes)
	}
	if !strings.HasSuffix(got[0].Filename, ".jpg") {
		t.Errorf("缩图后应为 JPEG，文件名 %q", got[0].Filename)
	}
}

// TestPrepareInlineImages_BadPayloadReported 坏图必须如实记入 failures（不静默丢弃）。
func TestPrepareInlineImages_BadPayloadReported(t *testing.T) {
	_, c := setupUploadTest(t)
	got, failures := c.PrepareInlineImages(context.Background(), []ImagePart{
		{Kind: "file_id", Value: "file-abc"}, // 上游无按 id 取内容接口
	})
	if len(got) != 0 {
		t.Errorf("坏图不应产出内联图片，得到 %d", len(got))
	}
	if len(failures) != 1 {
		t.Fatalf("应如实记录 1 条失败，得到 %d: %v", len(failures), failures)
	}
}

// TestInlineImageText_HasRestoreAndViewImage 说明文本必须含还原命令与 view_image 指引 ——
// 这是模型「真能看到图」的关键；缺一不可，也不得诱导它凭 base64 猜内容。
func TestInlineImageText_HasRestoreAndViewImage(t *testing.T) {
	im := InlineImage{Filename: "attached-1.png", MimeType: "image/png", B64: "iVBORw0KGgo=", RawBytes: 7}
	txt := inlineImageText(im)
	for _, want := range []string{"base64 -d", inlineImageRestoreDir, "view_image", im.B64, im.Filename} {
		if !strings.Contains(txt, want) {
			t.Errorf("说明文本应含 %q:\n%s", want, txt)
		}
	}
	if !strings.Contains(txt, "Do NOT guess") {
		t.Error("说明文本必须明确禁止凭 base64/文件名猜测内容")
	}
}

// TestAttachInlineImages_LastUserMessage 内联块必须附到**最后一条** user 消息。
func TestAttachInlineImages_LastUserMessage(t *testing.T) {
	blk := map[string]any{"type": "input_text", "text": "IMG"}
	input := []map[string]any{
		{"type": "message", "role": "system", "content": []any{map[string]any{"type": "input_text", "text": "sys"}}},
		{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "第一句"}}},
		{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "最后一句"}}},
	}
	out := attachInlineImages(input, []any{blk})
	last := out[len(out)-1]
	cs, _ := last["content"].([]any)
	if len(cs) != 2 {
		t.Fatalf("最后一条 user 消息应被追加为 2 个块，得到 %d", len(cs))
	}
}

// TestAttachInlineImages_NoUserMessageCreatesOne 没有 user 消息时必须新建一条（接返回值）。
func TestAttachInlineImages_NoUserMessageCreatesOne(t *testing.T) {
	blk := map[string]any{"type": "input_text", "text": "IMG"}
	input := []map[string]any{
		{"type": "message", "role": "system", "content": []any{map[string]any{"type": "input_text", "text": "sys"}}},
	}
	out := attachInlineImages(input, []any{blk})
	if len(out) != 2 {
		t.Fatalf("应新建一条 user 消息，长度 %d", len(out))
	}
	if out[1]["role"] != "user" {
		t.Errorf("新建消息 role 应为 user，得到 %v", out[1]["role"])
	}
}

// TestInlineImageNoticeForFailures 部分失败时必须如实说明，全部成功时不打扰模型。
func TestInlineImageNoticeForFailures(t *testing.T) {
	if s := inlineImageNoticeForFailures(2, nil, 2); s != "" {
		t.Errorf("全部成功时不应有提示，得到 %q", s)
	}
	s := inlineImageNoticeForFailures(2, []string{"第2张: 坏图"}, 1)
	if s == "" {
		t.Fatal("有失败时应给出提示")
	}
	for _, want := range []string{"CAN see", "CANNOT see"} {
		if !strings.Contains(s, want) {
			t.Errorf("提示应含 %q:\n%s", want, s)
		}
	}
}
