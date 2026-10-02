// prism-live 是一个闭环验证 CLI：用真实账号跑一次完整的 Prism 对话。
//
// 它证明「codex2api 的 prism 通道」端到端可用：
//
//	读账号 → 建会话(自举 session_token) → 铸 sentinel(node) → 取材料
//	→ start → 轮询(turn_state 原样回传) → 输出正文/思考/用量
//
// 用法：
//
//	go run ./cmd/prism-live -account /tmp/cpa_account.json -prompt "只回复三个字：收到了"
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/codex2api/proxy/prism"
)

func main() {
	acctPath := flag.String("account", "/tmp/cpa_account.json", "CPA/codex 账号 JSON（数组或对象）")
	prompt := flag.String("prompt", "只回复三个字：收到了", "用户消息")
	model := flag.String("model", "", "模型覆盖（默认沿用材料里的模型，切勿随意改）")
	effort := flag.String("effort", "", "reasoning_effort 覆盖（默认沿用材料；改写会 400）")
	stream := flag.Bool("stream", false, "演示 Responses SSE 事件序列")
	tools := flag.String("tools", "", "JSON 数组：客户端工具定义（走提示词仿真通道）")
	flag.Parse()

	raw, err := os.ReadFile(*acctPath)
	must(err)
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		var one map[string]any
		must(json.Unmarshal(raw, &one))
		arr = []map[string]any{one}
	}
	if len(arr) == 0 {
		fail("账号文件为空")
	}
	acct := arr[0]
	at, _ := acct["access_token"].(string)
	if at == "" {
		fail("账号缺 access_token")
	}
	ua := ""
	if dp, ok := acct["codex_device_profile"].(string); ok && dp != "" {
		var m map[string]string
		_ = json.Unmarshal([]byte(dp), &m)
		ua = m["user_agent"]
	}
	if ua == "" {
		ua = "codex-tui/0.156.0 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.156.0)"
	}

	cfg := prism.DefaultConfig()
	cfg.Debug = true
	cfg.Logf = func(f string, a ...any) { fmt.Printf("  · "+f+"\n", a...) }

	c := prism.NewClient(cfg, prism.Cookie{AccessToken: at, UserAgent: ua})
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	fmt.Println("=== 1. 建立会话（自举 prism_session_token）===")
	if err := c.WarmSession(ctx); err != nil {
		fail("会话失败: %v", err)
	}
	fmt.Printf("  userId=%s\n", c.UserID())

	fmt.Println("\n=== 2. 取对话材料包 ===")
	fmt.Printf("  材料状态: %v\n", c.MaterialInfo())

	fmt.Println("\n=== 3. 铸造 sentinel token（node 就地算 proof）===")
	s, err := c.Mint(ctx)
	if err != nil {
		fail("sentinel 铸造失败: %v", err)
	}
	fmt.Printf("  token len=%d\n", len(s))

	var toolDefs []map[string]any
	if strings.TrimSpace(*tools) != "" {
		if err := json.Unmarshal([]byte(*tools), &toolDefs); err != nil {
			fail("tools JSON 解析失败: %v", err)
		}
	}

	fmt.Printf("\n=== 4. 对话（prompt=%q tools=%d）===\n", *prompt, len(toolDefs))
	t0 := time.Now()
	turn, err := c.Run(ctx, prism.Request{
		Input: []map[string]any{{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": *prompt}},
		}},
		Model:           *model,
		ReasoningEffort: *effort,
		Tools:           toolDefs,
	})
	if err != nil {
		fail("对话失败: %v", err)
	}
	fmt.Printf("  耗时 %s | status=%s request_id=%s\n", time.Since(t0).Round(time.Millisecond), turn.Status, turn.RequestID)
	if turn.Reasoning != "" {
		fmt.Printf("  [思考] %s\n", turn.Reasoning)
	}
	fmt.Printf("  [正文] %s\n", turn.Text)
	if turn.ErrMessage != "" {
		fmt.Printf("  [上游错误] %s\n", turn.ErrMessage)
	}
	if turn.Text == "" && turn.RawFinal != "" {
		r := turn.RawFinal
		if len(r) > 1200 {
			r = r[:1200] + "…"
		}
		fmt.Printf("  [终态原文] %s\n", r)
	}
	if len(turn.ToolCalls) > 0 {
		b, _ := json.Marshal(turn.ToolCalls)
		fmt.Printf("  [工具调用] %s\n", b)
	}
	fmt.Printf("  [用量] input=%d cached=%d output=%d reasoning=%d total=%d\n",
		turn.Usage.InputTokens, turn.Usage.CachedInputTokens,
		turn.Usage.OutputTokens, turn.Usage.ReasoningTokens, turn.Usage.TotalTokens)

	if *stream {
		fmt.Println("\n=== 5. Responses SSE 事件序列（下游消费的形态）===")
		var sb strings.Builder
		buf := &sbWriter{&sb}
		ew := prism.NewEventWriter(buf, buf, firstNonEmpty(*model, "gpt-6-astra"))
		ew.WriteTurn(turn, "resp_live_demo")
		fmt.Println(sb.String())
	}

	if turn.Text == "" && turn.ErrMessage != "" {
		fmt.Println("\n结论: 链路可达但本轮上游报错（多为材料过期或沙箱劣化）。")
		os.Exit(2)
	}
	fmt.Println("\n✅ 闭环成功：Prism 通道端到端可用")
}

// sbWriter 把 SSE 写进 strings.Builder（演示用）。
type sbWriter struct{ b *strings.Builder }

func (s *sbWriter) Write(p []byte) (int, error) { return s.b.Write(p) }
func (s *sbWriter) Flush()                      {}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}
func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "✗ "+f+"\n", a...)
	os.Exit(1)
}
