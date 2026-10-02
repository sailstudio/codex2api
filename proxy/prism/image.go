package prism

import (
	"fmt"
	"strings"
)

// ============================================================
// 图片处理。
//
// ⚠️ 上游能力边界（实测钉死，勿轻信文档）：
//
//	传 input_image + image_url="data:image/png;base64,..." → HTTP 200
//	但**模型看不见**：对「白底 + 左上黑块」的图，模型仍答"蓝色"，
//	说明图片内容根本没进上下文。
//
// 真机上传走的是**项目文件通道**（`POST /api/projects/{id}/sandbox/resources-token`
// + 文件引用），那条链路尚未接入本通道。
//
// 因此本层不做「假装支持」：图片会被解析、计数并在提示词里如实说明，
// 让模型能明确告知用户"我暂时看不到这张图"，而不是对着不存在的图像胡编。
// 一旦文件通道接入，只需替换 imageNotice 为真实的 image_url 内容块即可。
// ============================================================

// ParseImagePart 解析一个 Responses 的 input_image 内容块。
//
// 支持的来源形态：
//
//	{"type":"input_image","image_url":"data:image/png;base64,..."}
//	{"type":"input_image","image_url":"https://..."}
//	{"type":"input_image","file_id":"file-abc"}          ← OpenAI Files 引用
//	{"type":"input_image","image_url":{"url":"..."}}     ← 旧/嵌套形态
func ParseImagePart(part map[string]any) (ImagePart, bool) {
	if part == nil {
		return ImagePart{}, false
	}
	img := ImagePart{Detail: strOf(part["detail"])}

	// file_id 形态
	if fid := strOf(part["file_id"]); fid != "" {
		img.Kind = "file_id"
		img.Value = fid
		return img, true
	}

	url := strOf(part["image_url"])
	if url == "" {
		// 旧/嵌套形态：image_url 是个对象
		if m, ok := part["image_url"].(map[string]any); ok {
			url = strOf(m["url"])
		}
	}
	if url == "" {
		return ImagePart{}, false
	}

	switch {
	case strings.HasPrefix(url, "data:"):
		img.Kind = "data_url"
		img.Value = url
		img.MediaType = dataURLMediaType(url)
	case strings.HasPrefix(url, "http://"), strings.HasPrefix(url, "https://"):
		img.Kind = "url"
		img.Value = url
	default:
		img.Kind = "unknown"
		img.Value = url
	}
	return img, true
}

// dataURLMediaType 从 data URL 头部解析 MIME（如 data:image/png;base64,... → image/png）。
func dataURLMediaType(u string) string {
	rest := strings.TrimPrefix(u, "data:")
	if i := strings.IndexAny(rest, ",;"); i >= 0 {
		return rest[:i]
	}
	if len(rest) > 64 {
		return rest[:64]
	}
	return rest
}

// dataURLPayloadBytes 估算 data URL 的原始字节数（用于日志/诊断，不实际解码）。
//
// base64 的 DecodedLen 会向上取整（8 字节会被算成 9），这里按 4 的倍数
// 扣除 padding 精确计算。
func dataURLPayloadBytes(u string) int {
	i := strings.Index(u, ",")
	if i < 0 {
		return 0
	}
	payload := u[i+1:]
	if !strings.Contains(u[:i], ";base64") {
		return len(payload)
	}
	pad := 0
	for k := len(payload) - 1; k >= 0 && payload[k] == '='; k-- {
		pad++
	}
	n := len(payload) / 4 * 3
	switch pad {
	case 1:
		n--
	case 2:
		n -= 2
	}
	if n < 0 {
		return 0
	}
	return n
}

// ImageStats 汇总图片信息（诊断/报告用）。
type ImageStats struct {
	Count      int
	Kinds      map[string]int // kind → 数量
	BytesTotal int            // data URL 估算原始字节
	MediaTypes map[string]int // MIME → 数量
}

// SummarizeImages 汇总图片。
func SummarizeImages(imgs []ImagePart) ImageStats {
	st := ImageStats{Kinds: map[string]int{}, MediaTypes: map[string]int{}}
	st.Count = len(imgs)
	for _, im := range imgs {
		st.Kinds[im.Kind]++
		if im.MediaType != "" {
			st.MediaTypes[im.MediaType]++
		}
		if im.Kind == "data_url" {
			st.BytesTotal += dataURLPayloadBytes(im.Value)
		}
	}
	return st
}

// attachImagesToInput 把 input_file 块附到最后一条 user 消息的 content 上，
// 并**返回**（可能变长的）input 切片 —— 若没有 user 消息需要新建一条，
// append 会返回新底层数组，调用方必须接收返回值，否则新消息会静默丢失。
//
// 上游要求文件引用与文本**同处一条消息**（真机 start body 即如此：
// role=user 的 content = [input_text, input_file]）。
func attachImagesToInput(input []map[string]any, blocks []map[string]any) []map[string]any {
	for i := len(input) - 1; i >= 0; i-- {
		if strOf(input[i]["role"]) != "user" {
			continue
		}
		content, _ := input[i]["content"].([]any)
		for _, b := range blocks {
			content = append(content, b)
		}
		input[i]["content"] = content
		return input
	}
	content := make([]any, 0, len(blocks))
	for _, b := range blocks {
		content = append(content, b)
	}
	return append(input, map[string]any{
		"type": "message", "role": "user", "content": content,
	})
}

// imageNotice 生成如实的能力说明（附在 input 末尾）。
//
// 这不是「假装支持」，而是把真实限制告诉模型：图片**完全无法送达**时
// （如 file_id 形态、上传失败），模型应当明确说明看不到，而不是编造内容。
func imageNotice(imgs []ImagePart) string {
	if len(imgs) == 0 {
		return ""
	}
	kinds := make([]string, 0)
	for _, im := range imgs {
		kinds = append(kinds, im.Kind)
	}
	return fmt.Sprintf(
		"NOTE: The client attached %d image(s) to this request (%s), "+
			"but they could NOT be delivered to you — you CANNOT see them. "+
			"Do not guess or invent what the image shows. "+
			"If the user's question depends on the image, briefly say you cannot view "+
			"the image on this channel and ask them to describe it in text.",
		len(imgs), strings.Join(kinds, ", "))
}

// imageNoticeForFailures 生成「部分图片未能送达」的如实说明。
//
// 成功的图片已用 input_file 送达（模型真能看到），只有失败的才需要说明；
// 全部成功时返回空串（不打扰模型）。
func imageNoticeForFailures(imgs []ImagePart, failures []string, delivered int) string {
	if len(failures) == 0 {
		return ""
	}
	var b strings.Builder
	if delivered > 0 {
		fmt.Fprintf(&b, "NOTE: %d of %d attached image(s) were delivered to you above "+
			"(as project-file references) and you CAN see them. ", delivered, len(imgs))
	}
	fmt.Fprintf(&b, "However %d image(s) could NOT be delivered and you CANNOT see them: %s. "+
		"Do not guess what those undelivered images show; if the user's question depends on them, "+
		"say you cannot view them.",
		len(failures), strings.Join(failures, "; "))
	return b.String()
}
