// 图片真机验证：data URL → 上传（项目文件通道）→ input_file 引用 → 模型是否真看见。
//
// 判据用「位置识别」：图是白底 + **左上角黑块**，若模型真看到会答"左上"而非乱猜。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/codex2api/proxy/prism"
)

func main() {
	imgPath := "/tmp/prism_sidecar/quad_image.png"
	if len(os.Args) > 1 {
		imgPath = os.Args[1]
	}
	raw, _ := os.ReadFile("/tmp/cpa_account.json")
	var arr []map[string]any
	_ = json.Unmarshal(raw, &arr)
	at, _ := arr[0]["access_token"].(string)
	var dp map[string]string
	if s, ok := arr[0]["codex_device_profile"].(string); ok {
		_ = json.Unmarshal([]byte(s), &dp)
	}

	imgBytes, err := os.ReadFile(imgPath)
	if err != nil {
		panic(err)
	}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imgBytes)
	fmt.Printf("图片 %s（%d 字节）\n", imgPath, len(imgBytes))

	cfg := prism.DefaultConfig()
	cfg.Logf = func(f string, a ...any) { fmt.Printf("  · "+f+"\n", a...) }
	c := prism.NewClient(cfg, prism.Cookie{AccessToken: at, UserAgent: dp["user_agent"]})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := c.WarmSession(ctx); err != nil {
		fmt.Println("warm err:", err)
		return
	}

	// ① 先单独验证上传 API
	fmt.Println("\n=== ① 上传到项目文件区 ===")
	res, err := c.UploadFile(ctx, "quad-verify.png", "image/png", imgBytes)
	if err != nil {
		fmt.Printf("  ✗ 上传失败: %v\n", err)
		return
	}
	fmt.Printf("  ✓ 上传成功 id=%s name=%s path=%s size=%d mime=%s\n",
		res.FileID, res.Filename, res.ProjectPath, res.Size, res.MimeType)

	// ② 走完整链路：带图请求，模型应能看见
	fmt.Println("\n=== ② 带图对话（模型能否真看见？）===")
	question := "这张图里，黑色方块位于画面的哪个位置？只回答：左上/右上/左下/右下/居中 之一。"
	turn, err := c.Run(ctx, prism.Request{
		Input: []map[string]any{{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": question}},
		}},
		Images: []prism.ImagePart{{
			Kind: "data_url", Value: dataURL, MediaType: "image/png",
		}},
	})
	if err != nil {
		fmt.Printf("  ✗ 失败: %v\n", err)
		return
	}
	fmt.Printf("  status=%s\n", turn.Status)
	if turn.ErrMessage != "" {
		fmt.Printf("  上游错误: %s\n", turn.ErrMessage)
	}
	text := turn.Text
	if len([]rune(text)) > 300 {
		text = string([]rune(text)[:300]) + "…"
	}
	fmt.Printf("  【模型回答】%s\n", text)

	// 判定
	hit := false
	for _, k := range []string{"左上", "左上方", "top-left", "upper-left", "top left"} {
		if containsFold(text, k) {
			hit = true
			break
		}
	}
	fmt.Printf("\n判定: ")
	if hit {
		fmt.Println("✅ 模型**真的看到了图片**（正确识别黑块在左上）—— 图片真机路径打通！")
	} else {
		fmt.Println("⚠️ 未能确认看到图片（回答未含「左上」）")
	}
}

func containsFold(s, sub string) bool {
	return len(sub) > 0 && (indexOf(s, sub) >= 0 || indexOf(lower(s), lower(sub)) >= 0)
}
func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
