// 调试：打印 prism start/status 的原始响应，定位为何正文为空。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/codex2api/proxy/prism"
)

func main() {
	raw, _ := os.ReadFile("/tmp/cpa_account.json")
	var arr []map[string]any
	_ = json.Unmarshal(raw, &arr)
	acct := arr[0]
	at, _ := acct["access_token"].(string)
	var dp map[string]string
	_ = json.Unmarshal([]byte(acct["codex_device_profile"].(string)), &dp)

	cfg := prism.DefaultConfig()
	cfg.Debug = true
	cfg.Logf = func(f string, a ...any) { fmt.Printf("  · "+f+"\n", a...) }
	// 用调试入口
	c := prism.NewDebugClient(cfg, prism.Cookie{AccessToken: at, UserAgent: dp["user_agent"]})

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	fmt.Println("=== WarmSession ===")
	if err := c.WarmSession(ctx); err != nil {
		fmt.Println("err:", err)
		return
	}
	fmt.Println("userId:", c.UserID())

	fmt.Println("\n=== 材料 ===")
	fmt.Printf("%v\n", c.MaterialInfo())

	fmt.Println("\n=== Raw Run ===")
	start, polls, err := c.RawTurn(ctx, "只回复三个字：收到了", "", "low")
	fmt.Println("--- START ---")
	fmt.Println(start)
	fmt.Printf("\n--- POLLS (%d) ---\n", len(polls))
	for i, p := range polls {
		fmt.Printf("[%d] %s\n", i, trunc(p, 900))
	}
	if err != nil {
		fmt.Println("err:", err)
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
