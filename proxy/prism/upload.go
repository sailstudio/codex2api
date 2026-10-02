package prism

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// ============================================================
// 图片上传（项目文件通道）—— Prism 的**真机图片路径**。
//
// 背景：此前实测发现 `/api/llm` 通道**不消费** data URL 图片（传 input_image
// 会 200 但模型看不见）。真机上传走的是**项目文件通道**：
//
//	① POST /api/project-files/upload
//	     headers: content-type=<MIME>, x-prism-file-name, x-prism-file-size,
//	              x-prism-file-id=<uuid>, x-prism-project-id, openai-sentinel-token
//	     body:    原始文件字节
//	   → 文件落盘为 /prism-uploads/<filename>
//	② start 的 input 里用 **input_file** 引用（不是 input_image！）：
//	     {"type":"input_file","filename":"x.png","project_path":"/prism-uploads/x.png"}
//
// 因此本层把下游的 input_image 转换为「上传 + input_file 引用」，
// 让模型**真能看到图片**。
// ============================================================

// 上传目录（上游固定把上传文件放在此目录）
const prismUploadDir = "/prism-uploads"

// UploadResult 是一次上传的结果。
type UploadResult struct {
	FileID      string
	Filename    string
	ProjectPath string // /prism-uploads/<filename>
	Size        int
	MimeType    string
}

// UploadFile 把文件字节上传到项目文件区，返回可用于 input_file 的路径。
//
// 与真机请求逐项对齐（含 headers 名称与 sentinel 门禁）。
func (c *Client) UploadFile(ctx context.Context, filename, mimeType string, data []byte) (*UploadResult, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("prism: 上传内容为空")
	}
	projectID, err := c.projectIDFor(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(filename) == "" {
		filename = "image"
	}
	// 文件名需安全化（上游按名落盘，带路径分隔符会被拒/污染目录）
	filename = sanitizeFilename(filename)
	if mimeType == "" {
		mimeType = mimeByExt(filename)
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	fileID := newUUID4()
	headers := map[string]string{
		"content-type":                        mimeType,
		"x-prism-file-name":                   filename,
		"x-prism-file-size":                   fmt.Sprintf("%d", len(data)),
		"x-prism-file-id":                     fileID,
		"x-prism-project-id":                  projectID,
		"x-prism-require-project-edit-access": "true",
	}

	tok, err := c.Mint(ctx)
	if err != nil {
		return nil, fmt.Errorf("prism: 上传取 sentinel 失败: %w", err)
	}
	resp, err := c.doRaw(ctx, http.MethodPost, "/api/project-files/upload", data, tok, headers)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		return nil, fmt.Errorf("prism: 文件上传失败 HTTP %d: %s", resp.StatusCode, truncStr(string(body), 200))
	}
	return &UploadResult{
		FileID:      fileID,
		Filename:    filename,
		ProjectPath: prismUploadDir + "/" + filename,
		Size:        len(data),
		MimeType:    mimeType,
	}, nil
}

// UploadImages 把请求携带的图片全部上传，返回可放进 input 的 input_file 块。
//
// 处理策略（按来源形态）：
//   - data_url  → 直接解码上传（最常见）
//   - url       → 下载后上传（限制体积，避免网关被大文件拖住）
//   - file_id   → 上游无「按 id 取内容」的公开接口 → 跳过并如实记录
//
// 返回：成功转换出的 input_file 块、失败说明（供如实告知模型）。
func (c *Client) UploadImages(ctx context.Context, imgs []ImagePart) (blocks []map[string]any, failures []string) {
	const maxBytes = 20 << 20 // 20MB 上限
	for i, im := range imgs {
		data, mime, err := c.imageBytes(ctx, im, maxBytes)
		if err != nil {
			failures = append(failures, fmt.Sprintf("第%d张(%s): %v", i+1, im.Kind, err))
			continue
		}
		name := fmt.Sprintf("upload-%s-%d%s", shortHash(data), time.Now().UnixNano()%100000, extByMime(mime))
		res, err := c.UploadFile(ctx, name, mime, data)
		if err != nil {
			failures = append(failures, fmt.Sprintf("第%d张(%s): 上传失败 %v", i+1, im.Kind, err))
			continue
		}
		c.cfg.Logf("prism: 图片已上传 %s（%dB, %s）→ %s", res.Filename, res.Size, res.MimeType, res.ProjectPath)
		blocks = append(blocks, map[string]any{
			"type":         "input_file",
			"filename":     res.Filename,
			"project_path": res.ProjectPath,
		})
	}
	return blocks, failures
}

// imageBytes 把各种来源形态取成字节。
func (c *Client) imageBytes(ctx context.Context, im ImagePart, maxBytes int) ([]byte, string, error) {
	switch im.Kind {
	case "data_url":
		i := strings.Index(im.Value, ",")
		if i < 0 {
			return nil, "", fmt.Errorf("data URL 格式非法")
		}
		meta, payload := im.Value[:i], im.Value[i+1:]
		if strings.Contains(meta, ";base64") {
			b, err := base64.StdEncoding.DecodeString(payload)
			if err != nil {
				return nil, "", fmt.Errorf("base64 解码失败: %w", err)
			}
			if len(b) > maxBytes {
				return nil, "", fmt.Errorf("图片过大 %dB（上限 %dB）", len(b), maxBytes)
			}
			return b, firstNonBlank(im.MediaType, dataURLMediaType(im.Value), "image/png"), nil
		}
		// 非 base64 的 data URL（罕见）
		b := []byte(payload)
		if len(b) > maxBytes {
			return nil, "", fmt.Errorf("图片过大 %dB", len(b))
		}
		return b, firstNonBlank(im.MediaType, dataURLMediaType(im.Value)), nil

	case "url":
		return c.fetchURLBytes(ctx, im.Value, maxBytes)

	case "file_id":
		return nil, "", fmt.Errorf("file_id 形态无法取内容（上游无公开按 id 下载接口）")

	default:
		return nil, "", fmt.Errorf("不支持的图片形态 %q", im.Kind)
	}
}

// fetchURLBytes 下载远程图片（限体积、限协议）。
func (c *Client) fetchURLBytes(ctx context.Context, rawurl string, maxBytes int) ([]byte, string, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, "", fmt.Errorf("URL 非法: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, "", fmt.Errorf("仅支持 http(s) 图片，得到 %q", u.Scheme)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", c.cookie.UserAgent)
	cli := &http.Client{Timeout: 60 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("下载 HTTP %d", resp.StatusCode)
	}
	// 多读 1 字节以便判断超限
	b, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取失败: %w", err)
	}
	if len(b) > maxBytes {
		return nil, "", fmt.Errorf("图片过大（>%dB）", maxBytes)
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" || strings.HasPrefix(mime, "text/") {
		mime = mimeByExt(u.Path)
	}
	return b, mime, nil
}

// projectIDFor 取项目 id（优先已 warm 的会话，其次**材料里的 projectId**，
// 最后才去列表接口查）。
//
// 顺序很关键：材料里就绑定了项目（且与沙箱同源），直接复用**最稳**；
// 列表接口可能返回别的项目，导致上传落到错误的项目里。
func (c *Client) projectIDFor(ctx context.Context) (string, error) {
	c.mu.Lock()
	pid := c.projectID
	c.mu.Unlock()
	if pid != "" {
		return pid, nil
	}
	// 材料里的 projectId（与沙箱同源，首选）
	if mat, err := c.material(ctx); err == nil && mat.ProjectID != "" {
		c.mu.Lock()
		c.projectID = mat.ProjectID
		c.mu.Unlock()
		return mat.ProjectID, nil
	}
	// 兜底：查项目列表
	if id, err := c.ProjectID(ctx); err == nil && id != "" {
		return id, nil
	}
	return "", fmt.Errorf("prism: 项目 id 未知（材料缺 projectId 且列表查询失败）")
}

// sanitizeFilename 安全化文件名（去掉路径分隔与控制字符）。
func sanitizeFilename(name string) string {
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == 0:
			return '-'
		case r < 0x20:
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

// mimeByExt 按扩展名推断 MIME（兜底用）。
func mimeByExt(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	case ".pdf":
		return "application/pdf"
	case ".txt":
		return "text/plain"
	}
	return ""
}

// extByMime 由 MIME 推扩展名（上传文件命名用）。
func extByMime(m string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(m, ";")[0])) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	case "image/svg+xml":
		return ".svg"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	}
	return ".bin"
}

// shortHash 取数据前缀的稳定短标识（避免同名覆盖）。
func shortHash(b []byte) string {
	var h uint32 = 2166136261
	for i := 0; i < len(b) && i < 4096; i++ {
		h ^= uint32(b[i])
		h *= 16777619
	}
	const hexd = "0123456789abcdef"
	out := make([]byte, 8)
	for i := 0; i < 8; i++ {
		out[i] = hexd[(h>>(uint(i)*4))&0xf]
	}
	return string(out)
}

var _ = bytes.MinRead
