package prism

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/codex2api/internal/imageproc"
)

// ============================================================
// 图片入参：**内联 base64 路径**（真机唯一走得通的路）。
//
// 真机结论（逆向站点 HAR + 多轮探针，2026-09-17 实测）：
//
//	上游**没有任何二进制/多模态入参**。input_image 会被拒或忽略；
//	input_file + project_path 语法虽被接受，但服务端只把它编成一句
//	"[project file: …]" 提示，模型会去**会话工作区**找文件 —— 而文件能否
//	出现在工作区，取决于站点编辑器把文件写进项目协作文档（Y-Sweet，WS 私有协议）。
//	我们无法复制那条路（Go 侧无成熟 Yjs 实现，且有写坏用户项目文档的风险）。
//
// 真正可行的路：**把图片 base64 内联进本轮 user 消息**，同时给出
// 「还原成工作区文件」的命令与 `view_image` 指引 —— 模型自己落盘后调用
// `view_image` 就能真看到图。实测 90×30 三色带图 → 模型正确答出「红 绿 蓝」。
//
// 预算边界（同一张噪声图只改大小，实测）：
//
//	内联 base64 ≤ ~59k 字符 → ✅ 可用（整轮较慢）
//	219k 字符              → ❌ 上游 502（codex_v2_restore_start failed）
//
// 因此这里取单图 ≤ 48k 字符、单请求 ≤ 96k 字符，超限先缩图（JPEG，阶梯降质），
// 缩不下来再如实告知「本轮未载入」，绝不假装送到。
// ============================================================

const (
	// inlineImageBudgetChars 单图内联 base64 的字符上限（实测安全区）。
	inlineImageBudgetChars = 48000
	// inlineRequestBudgetChars 单请求所有内联图片的字符上限。
	inlineRequestBudgetChars = 96000
	// inlineImageRestoreDir 让模型落盘的目录（与站点工作区习惯一致）。
	inlineImageRestoreDir = "prism-uploads"
)

// InlineImage 是一张已准备好内联的图片。
type InlineImage struct {
	Filename string // 落盘文件名（无路径）
	MimeType string
	B64      string // 标准 base64（单行，无换行）
	RawBytes int    // 解码后的字节数（诊断用）
}

// charsOf 返回内联后会占用的字符数。
func (i InlineImage) charsOf() int { return len(i.B64) }

// PrepareInlineImages 把请求里的图片解码、按预算缩图，产出可内联的图片。
//
// 返回：可内联的图片、未能载入的说明（供如实告知模型）。
// 预算按「单图 ≤ 48k 字符、累计 ≤ 96k 字符」执行，超出部分缩图；缩不下来则
// 记入 failures（绝不静默丢弃）。
func (c *Client) PrepareInlineImages(ctx context.Context, imgs []ImagePart) (out []InlineImage, failures []string) {
	const maxBytes = 20 << 20
	used := 0
	for i, im := range imgs {
		data, mime, err := c.imageBytes(ctx, im, maxBytes)
		if err != nil {
			failures = append(failures, fmt.Sprintf("第%d张(%s): %v", i+1, im.Kind, err))
			continue
		}
		// 预算内直接内联
		b64 := base64.StdEncoding.EncodeToString(data)
		if len(b64) <= inlineImageBudgetChars && used+len(b64) <= inlineRequestBudgetChars {
			used += len(b64)
			out = append(out, InlineImage{
				Filename: inlineFilename(i, mime), MimeType: mime,
				B64: b64, RawBytes: len(data),
			})
			continue
		}
		// 超预算 → 缩图（JPEG 阶梯降质），目标 = 单图预算对应的原始字节数
		budgetKB := charsToRawBytes(inlineImageBudgetChars) / 1024
		thumb, tmime, ok := imageproc.MakeThumbnail(data, budgetKB)
		if !ok {
			failures = append(failures, fmt.Sprintf("第%d张(%s): 体积 %d 字符超预算且无法缩图",
				i+1, im.Kind, len(b64)))
			continue
		}
		tb64 := base64.StdEncoding.EncodeToString(thumb)
		if len(tb64) > inlineImageBudgetChars || used+len(tb64) > inlineRequestBudgetChars {
			failures = append(failures, fmt.Sprintf("第%d张(%s): 缩图后仍超预算（%d 字符）",
				i+1, im.Kind, len(tb64)))
			continue
		}
		used += len(tb64)
		c.cfg.Logf("prism: 图片已缩图 %d→%d 字节（%s）", len(data), len(thumb), tmime)
		out = append(out, InlineImage{
			Filename: inlineFilename(i, tmime), MimeType: tmime,
			B64: tb64, RawBytes: len(thumb),
		})
	}
	return out, failures
}

// charsToRawBytes 把 base64 字符预算换算成原始字节预算（向下取整）。
func charsToRawBytes(chars int) int {
	return chars / 4 * 3
}

// inlineFilename 生成落盘文件名（扩展名跟随实际编码格式）。
func inlineFilename(idx int, mime string) string {
	return fmt.Sprintf("attached-%d%s", idx+1, extByMime(mime))
}

// attachInlineImages 把内联图片块附到最后一条 user 消息的 content 上，并返回
// 可能变长的 input（无 user 消息时会新建一条，append 可能换底层数组）。
func attachInlineImages(input []map[string]any, blocks []any) []map[string]any {
	if len(blocks) == 0 {
		return input
	}
	for i := len(input) - 1; i >= 0; i-- {
		if strOf(input[i]["role"]) != "user" {
			continue
		}
		content, _ := input[i]["content"].([]any)
		content = append(content, blocks...)
		input[i]["content"] = content
		return input
	}
	return append(input, map[string]any{
		"type": "message", "role": "user", "content": blocks,
	})
}

// inlineImageText 生成内联图片的说明文本：给出还原命令与 view_image 指引。
//
// 关键：必须明确要求模型**先落盘再 view_image**，并禁止它凭 base64/文件名猜测内容。
func inlineImageText(img InlineImage) string {
	path := inlineImageRestoreDir + "/" + img.Filename
	var b strings.Builder
	fmt.Fprintf(&b, "\n[attached image #%s — %s, %d bytes]\n", img.Filename, img.MimeType, img.RawBytes)
	b.WriteString("This channel has NO native image input, so the image is provided inline as base64 above.\n")
	b.WriteString("Before answering, restore it to your workspace and LOOK at it:\n\n")
	fmt.Fprintf(&b, "  mkdir -p %s && printf '%%s' '%s' | base64 -d > %s\n\n",
		inlineImageRestoreDir, img.B64, path)
	fmt.Fprintf(&b, "  tool: view_image {\"path\": \"%s\"}\n\n", path)
	b.WriteString("Then answer the user's question about the image. ")
	b.WriteString("Do NOT guess the image contents from the base64 text or the filename.\n")
	return b.String()
}

// inlineImageNoticeForFailures 生成「部分图片未能载入」的如实说明。
//
// 成功的内联图片模型真能看到；只有失败的需要说明。全部成功时返回空串。
func inlineImageNoticeForFailures(total int, failures []string, delivered int) string {
	if len(failures) == 0 {
		return ""
	}
	var b strings.Builder
	if delivered > 0 {
		fmt.Fprintf(&b, "NOTE: %d of %d attached image(s) were provided to you inline above "+
			"(follow the restore command, then call view_image) — you CAN see them. ", delivered, total)
	}
	fmt.Fprintf(&b, "However %d image(s) could NOT be loaded into this turn and you CANNOT see them: %s. "+
		"Do not guess what those images show; if the question depends on them, say you cannot view them.",
		len(failures), strings.Join(failures, "; "))
	return b.String()
}
