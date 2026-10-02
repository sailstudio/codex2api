package prism

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"strings"
)

// ============================================================================
// 图片 / 附件通道。
//
// 上游事实（多份真机实测一致）：
//   - 上游**没有任何二进制/多模态入参**。input_image 要求 "valid Prism storage URL"，
//     data: / 公网 URL / file_id 全被拒；沙箱出网被代理拦（Domain forbidden）。
//   - 站点自己的「上传成项目文件 + project_path」只在浏览器编辑器把文件写进项目
//     文档时才成立；headless 上传的文件进不了文件树，模型在工作区看不到。
//
// 唯一走通的路：把内容当**文本**塞进提示词——图片给 base64 + 一条还原命令，
// 让模型自己在沙箱里还原成文件再用 view_image 看；文本/PDF 直接给抽取好的正文。
// 本文件实现该通道 + 缩图（在预算内尽量保住可读性）。
// ============================================================================

// AttachmentBudget 控制内联开销（字符数）。
type AttachmentBudget struct {
	TotalChars   int // 单请求所有图片的 base64 总预算
	PerFileChars int // 单张图片的 base64 上限
	MaxDim       int // 缩图最长边
	JPEGQuality  int // 起始 JPEG 质量
}

// DefaultAttachmentBudget 给出生产默认（与实测安全区一致：59k 可用，219k 上游 502）。
func DefaultAttachmentBudget() AttachmentBudget {
	return AttachmentBudget{TotalChars: 96 << 10, PerFileChars: 48 << 10, MaxDim: 1400, JPEGQuality: 82}
}

// Attachment 是一个待送入模型的附件。
type Attachment struct {
	Name string
	Mime string
	Data []byte
	Text string // 若上游（api 层）已抽好正文，填这里，图片走 Data
}

// IsImage 判断 MIME 是否图片。
func IsImage(mime string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(mime)), "image/")
}

// AttachmentKey 是附件在请求内的标识（内容摘要，同名不同内容可区分）。
func AttachmentKey(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// RenderBlocks 把一批附件渲染成提示词文本块，并从预算里扣减。
// seq 是起始序号（多消息场景下保持全局连续）。
func RenderBlocks(files []Attachment, budget *AttachmentBudget, seq int) []string {
	out := make([]string, 0, len(files))
	for i, f := range files {
		block, cost := renderOne(f, seq+i, budget)
		budget.TotalChars -= cost
		out = append(out, block)
	}
	return out
}

// renderOne 渲染单个附件；cost 为本块消耗的 base64 预算。
func renderOne(f Attachment, seq int, budget *AttachmentBudget) (string, int) {
	name := SafeFileName(f.Name, f.Mime, seq)

	// ① 已有抽取正文（PDF/文本）：直接给文本，零预算。
	if strings.TrimSpace(f.Text) != "" {
		return fmt.Sprintf("[文档附件 %d] %s（%s，%d 字节）正文如下：\n%s",
			seq+1, name, mimeOrGuess(f.Mime, f.Data), len(f.Data), strings.TrimSpace(f.Text)), 0
	}

	// ② 图片：base64 + 还原命令 + 查看说明。
	if IsImage(f.Mime) {
		return renderImage(f, name, seq, budget)
	}

	// ③ 其他二进制：说明无法直送，请用户提供文本版本。
	return fmt.Sprintf("[附件 %d] %s（%s，%d 字节）：二进制内容无法直接送入模型，可按需向用户索取文本版本。",
		seq+1, name, mimeOrGuess(f.Mime, f.Data), len(f.Data)), 0
}

// renderImage 把图片渲染成「base64 + 还原命令 + 查看说明」块。
func renderImage(f Attachment, name string, seq int, budget *AttachmentBudget) (string, int) {
	if len(f.Data) == 0 {
		return fmt.Sprintf("[图片附件 %d] %s（%s）：内容为空，未载入。", seq+1, name, mimeOrGuess(f.Mime, f.Data)), 0
	}
	limit := budget.PerFileChars
	if budget.TotalChars < limit {
		limit = budget.TotalChars
	}
	if limit <= 0 {
		return fmt.Sprintf("[图片附件 %d] %s（%s）：本轮内联预算已用尽，未载入；如需查看请让用户单独重发这一张。",
			seq+1, name, mimeOrGuess(f.Mime, f.Data)), 0
	}

	b64 := base64.StdEncoding.EncodeToString(f.Data)
	note := ""
	if len(b64) > limit {
		src, ok := decodeImage(f.Data)
		if !ok {
			return fmt.Sprintf("[图片附件 %d] %s（%s，%d 字节）：该格式没有内置解码器（只支持 PNG/JPEG/GIF），"+
				"且原图超过内联上限，本轮无法把图片内容交给模型。请让用户转成 PNG/JPEG 后重发。",
				seq+1, name, mimeOrGuess(f.Mime, f.Data), len(f.Data)), 0
		}
		small, ok := encodeWithin(src, limit, budget.MaxDim, budget.JPEGQuality)
		if !ok {
			return fmt.Sprintf("[图片附件 %d] %s（%s）：图片过大，缩到内联上限后仍放不下，本轮未载入；如需查看请让用户单独重发这一张。",
				seq+1, name, mimeOrGuess(f.Mime, f.Data)), 0
		}
		note = fmt.Sprintf("（原图 %d 字节，为传输已缩为 %d 字节）", len(f.Data), len(small))
		b64 = base64.StdEncoding.EncodeToString(small)
	}

	return fmt.Sprintf(`[图片附件 %d] %s（%s）%s
图片内容只在这段 base64 里，直接读 base64 看不到图像。请先在工作区还原成文件，再查看它：

mkdir -p prism-uploads && printf '%%s' '%s' | base64 -d > prism-uploads/%s

然后用 view_image 查看 prism-uploads/%s（该工具不可用时，用你手头能看图的工具），再回答用户。
不要把 base64 复述进回答。`, seq+1, name, mimeOrGuess(f.Mime, f.Data), note, b64, name, name), len(b64)
}

// FileMarkers 把没内联到位的附件折成一行标记（历史里的图片超预算时用）。
func FileMarkers(files []Attachment) string {
	names := make([]string, 0, len(files))
	for i, f := range files {
		names = append(names, SafeFileName(f.Name, f.Mime, i))
	}
	if len(names) == 0 {
		return ""
	}
	return "[此前发送的附件（本轮未载入）：" + strings.Join(names, ", ") + "]"
}

// SafeFileName 取一个安全的文件名（客户端常给空名或带路径的名）。
func SafeFileName(name, mime string, seq int) string {
	n := sanitizeFileName(name)
	if n == "" {
		kind := "file"
		if IsImage(mime) {
			kind = "image"
		}
		n = fmt.Sprintf("%s-%d", kind, seq+1)
	}
	if !strings.Contains(n, ".") {
		n += mimeExtension(mime)
	}
	return n
}

// sanitizeFileName 去路径与控制字符并限长（文件名会进 shell 命令与提示词）。
func sanitizeFileName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return -1
		case strings.ContainsRune(":*?\"<>|'`$&;()[]{}#!", r):
			return '_'
		}
		return r
	}, name)
	if rs := []rune(name); len(rs) > 80 {
		name = string(rs[:80])
	}
	return name
}

var mimeExtensions = map[string]string{
	"image/png": ".png", "image/jpeg": ".jpg", "image/jpg": ".jpg",
	"image/webp": ".webp", "image/gif": ".gif", "image/bmp": ".bmp",
	"application/pdf": ".pdf", "text/plain": ".txt", "text/markdown": ".md",
	"text/csv": ".csv", "application/json": ".json", "application/zip": ".zip",
}

func mimeExtension(mime string) string {
	return mimeExtensions[strings.ToLower(strings.TrimSpace(mime))]
}

// mimeOrGuess 用 MIME，缺失时按内容嗅探（只取主类型，它要进提示词）。
func mimeOrGuess(mime string, data []byte) string {
	if m := strings.TrimSpace(mime); m != "" {
		return m
	}
	if len(data) == 0 {
		return "application/octet-stream"
	}
	return strings.TrimSpace(strings.Split(http.DetectContentType(data), ";")[0])
}

// encodedLen 是 base64 编码后的长度（预算判断用）。
func encodedLen(n int) int {
	if n == 0 {
		return 0
	}
	return (n + 2) / 3 * 4
}

// decodeImage 解出图片（只认标准库带解码器的格式：PNG/JPEG/GIF）。
func decodeImage(data []byte) (image.Image, bool) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	if src.Bounds().Dx() == 0 || src.Bounds().Dy() == 0 {
		return nil, false
	}
	return src, true
}

// encodeWithin 把图片压到 base64 长度不超过 budget 为止（先限边长，再降比例/质量）。
func encodeWithin(src image.Image, budgetChars, maxDim, startQuality int) ([]byte, bool) {
	if maxDim <= 0 {
		maxDim = 1400
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if long := maxInt(w, h); long > maxDim {
		scale := float64(maxDim) / float64(long)
		w, h = maxInt(1, int(float64(w)*scale)), maxInt(1, int(float64(h)*scale))
	}
	src = flatten(src)
	qualities := []int{startQuality, 70, 55}
	if startQuality <= 0 {
		qualities = []int{82, 70, 55}
	}
	for _, q := range qualities {
		for _, factor := range []float64{1, 0.75, 0.55, 0.4, 0.28, 0.2} {
			tw, th := maxInt(1, int(float64(w)*factor)), maxInt(1, int(float64(h)*factor))
			var out bytes.Buffer
			if err := jpeg.Encode(&out, downscale(src, tw, th), &jpeg.Options{Quality: q}); err != nil {
				continue
			}
			if encodedLen(out.Len()) <= budgetChars {
				return out.Bytes(), true
			}
		}
	}
	return nil, false
}

// flatten 把透明像素合成到白底（JPEG 没有 alpha，透明区会变黑）。
func flatten(src image.Image) image.Image {
	if o, ok := src.(interface{ Opaque() bool }); ok && o.Opaque() {
		return src
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Over)
	return dst
}

// downscale 盒式平均缩放（无第三方依赖；缩文字截图比最近邻清楚）。
func downscale(src image.Image, w, h int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0, y1 := b.Min.Y+y*sh/h, b.Min.Y+(y+1)*sh/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < w; x++ {
			x0, x1 := b.Min.X+x*sw/w, b.Min.X+(x+1)*sw/w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint32
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					cr, cg, cb, ca := src.At(xx, yy).RGBA()
					r, g, bl, a, n = r+cr, g+cg, bl+cb, a+ca, n+1
				}
			}
			if n == 0 {
				continue
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), uint8(a / n >> 8)})
		}
	}
	return dst
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
