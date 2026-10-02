// jachk：对比「裸 Go」与「utls Chrome 指纹」的 JA3/JA4，验证 Cloudflare 判据。
// 用法：PRISM_PROXY=http://127.0.0.1:7890 go run ./cmd/jachk
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/codex2api/proxy"
)

// 指纹回显服务（返回客户端 JA3/JA4）
const echo = "https://tls.peet.ws/api/all"

func fetch(c *http.Client, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", echo, nil)
	req.Header.Set("User-Agent", "codex-tui/0.156.0 (Mac OS 15.5.0; arm64) xterm-256color")
	resp, err := c.Do(req)
	if err != nil {
		fmt.Printf("[%s] 请求失败: %v\n", name, err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	body := string(b)

	fmt.Printf("\n===== %s =====\nHTTP %d\n", name, resp.StatusCode)
	for _, k := range []string{"ja3_hash", "ja3", "ja4", "peetprint_hash"} {
		re := regexp.MustCompile(`"` + k + `"\s*:\s*("([^"]*)"|(\[[^\]]*\]|[0-9]+))`)
		if m := re.FindStringSubmatch(body); m != nil {
			v := m[1]
			if len(v) > 150 {
				v = v[:150] + "…"
			}
			fmt.Printf("  %s = %s\n", k, v)
		}
	}
}

func main() {
	px := os.Getenv("PRISM_PROXY")
	fmt.Printf("代理: %q  目标: %s\n", px, echo)

	// ① 裸 Go（默认 transport）——当前 Prism 通道的历史行为
	fetch(&http.Client{Timeout: 0}, "① 裸 Go（net/http 默认）")

	// ② utls Chrome 指纹——本轮修复后 Prism 通道将使用的 transport
	fetch(&http.Client{Transport: proxy.NewUTLSTransport(px), Timeout: 0}, "② utls Chrome 指纹（修复后）")
}
