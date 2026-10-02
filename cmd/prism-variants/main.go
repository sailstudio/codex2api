// 变体探针：用刚产出的新鲜材料，A/B/C 三种组装方式定位 400 的成因。
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
	cfg.Logf = func(f string, a ...any) { fmt.Printf("  · "+f+"\n", a...) }
	c := prism.NewDebugClient(cfg, prism.Cookie{AccessToken: at, UserAgent: dp["user_agent"]})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := c.WarmSession(ctx); err != nil {
		fmt.Println("warm err:", err)
		return
	}
	fmt.Printf("材料: %v\n\n", c.MaterialInfo())

	cases := []struct {
		name       string
		withPrefix bool
		overrideMD bool
	}{
		{"A 材料原样 + 仅 user input", false, false},
		{"B 材料原样 + system前缀 + user input", true, false},
		{"C 改 model/effort + 仅 user input", false, true},
	}
	for _, v := range cases {
		start, _, err := c.RawTurnVariant(ctx, prism.VariantArgs{
			Prompt:     "只回复三个字：收到了",
			Prompt2:    v.name,
			WithPrefix: v.withPrefix,
			OverrideMD: v.overrideMD,
		})
		var s struct {
			Status   string `json:"status"`
			Response struct {
				Payload struct {
					Message string `json:"message"`
				} `json:"payload"`
			} `json:"response"`
		}
		_ = json.Unmarshal([]byte(start), &s)
		fmt.Printf("=== %s ===\n  status=%s err=%q startLen=%d\n", v.name, s.Status, trunc(s.Response.Payload.Message, 90), len(start))
		if err != nil {
			fmt.Printf("  err=%v\n", err)
		}
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
