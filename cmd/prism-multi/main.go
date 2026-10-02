// 多槽并发压测：验证材料池在多路并发下的调度正确性与吞吐。
//
// 用法：
//
//	go run ./cmd/prism-multi -account /tmp/cpa_account.json -n 8
//
// 环境变量：
//
//	PRISM_MATERIAL_PATH  材料目录（多槽）或单文件
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/proxy"
	"github.com/codex2api/proxy/prism"
)

type result struct {
	idx    int
	ok     bool
	text   string
	err    string
	ms     int64
	tokens prism.Usage
}

func main() {
	acctPath := flag.String("account", "/tmp/cpa_account.json", "账号 JSON")
	n := flag.Int("n", 8, "并发请求数")
	prompt := flag.String("prompt", "只回复你的序号数字", "提示词")
	perSlot := flag.Int("per-slot", 0, "每槽并发上限（0=用池默认）")
	noLog := flag.Bool("quiet", false, "隐藏客户端日志")
	serial := flag.Bool("serial", false, "串行执行（每次只跑 1 路，用于隔离并发 vs 身份）")
	startIdx := flag.Int("start", 0, "起始序号（配合 -n 分批跑）")
	conv := flag.String("conv", "", "强制 conversationId（验证材料与会话配对；空=每次新建）")
	stagger := flag.Duration("stagger", 0, "每路启动错峰间隔（如 300ms；0=同时齐发）")
	tlsProfile := flag.Bool("tls", true, "使用 Chrome TLS 指纹（utls）；false=裸 Go 指纹（A/B 对照）")
	inflight := flag.Int("inflight", 0, "单账号在飞上限（0=不限流；实测突发会被上游 403）")
	gap := flag.Duration("gap", 0, "相邻请求最小间隔（平滑放行；0=不限）")
	slotAttempts := flag.Int("slot-attempts", 0, "换槽重试上限（0=默认3；1=不换槽重试，避免放大上游压力）")
	flag.Parse()
	// A/B 对照：-tls=false 时关闭指纹（复现历史 403）
	if !*tlsProfile {
		os.Setenv("PRISM_TLS_PROFILE", "0")
	}

	raw, err := os.ReadFile(*acctPath)
	if err != nil {
		fatal("读账号失败: %v", err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) == 0 {
		fatal("账号 JSON 解析失败")
	}
	at, _ := arr[0]["access_token"].(string)
	var dp map[string]string
	if s, ok := arr[0]["codex_device_profile"].(string); ok {
		_ = json.Unmarshal([]byte(s), &dp)
	}

	cfg := prism.DefaultConfig()
	// 与生产一致：注入 Chrome TLS 指纹 transport（prism.openai.com 在 Cloudflare 后面）
	if os.Getenv("PRISM_TLS_PROFILE") != "0" {
		cfg.Transport = proxy.NewUTLSTransport(os.Getenv("PRISM_PROXY"))
	}
	cfg.MaxInflight = *inflight
	cfg.MinGap = *gap
	cfg.SlotAttempts = *slotAttempts
	if !*noLog {
		cfg.Logf = func(f string, a ...any) { fmt.Printf("  · "+f+"\n", a...) }
	}
	c := prism.NewClient(cfg, prism.Cookie{AccessToken: at, UserAgent: dp["user_agent"]})
	// 标定用：调每槽并发上限 + 放大等待窗口
	if p := c.Pool(); p != nil {
		if *perSlot > 0 {
			p.SetMaxPerSlot(int64(*perSlot))
		}
		p.SetWaitTimeout(5 * time.Minute)
		fmt.Printf("池: 槽数=%d 每槽上限=%d 总并发上限=%d\n",
			p.Size(), p.MaxPerSlot(), int64(p.Size())*p.MaxPerSlot())
	}

	// 池状态（压测前）
	if p := c.Pool(); p != nil {
		b, _ := json.MarshalIndent(p.Stats(), "", "  ")
		fmt.Printf("=== 压测前池状态 ===\n%s\n", b)
	}

	fmt.Printf("\n=== %d 路并发（同一提示词，看槽分配与错误）===\n", *n)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var wg sync.WaitGroup
	results := make([]result, *n)
	var okCount, failCount int64
	t0 := time.Now()

	conc := *n
	if *serial {
		conc = 1
	}
	sem := make(chan struct{}, conc)
	for i := 0; i < *n; i++ {
		wg.Add(1)
		go func(i int) {
			sem <- struct{}{}
			defer func() { <-sem }()
			// 错峰启动：把瞬时突发摊开，避开上游的突发限流
			if *stagger > 0 {
				time.Sleep(time.Duration(i) * *stagger)
			}
			defer wg.Done()
			st := time.Now()
			r := result{idx: *startIdx + i}
			turn, err := c.Run(ctx, prism.Request{
				ConversationID: *conv,
				Input: []map[string]any{{
					"type": "message", "role": "user",
					"content": []any{map[string]any{
						"type": "input_text",
						"text": fmt.Sprintf("%s（这是第 %d 路）", *prompt, *startIdx+i+1),
					}},
				}},
			})
			r.ms = time.Since(st).Milliseconds()
			if err != nil {
				r.err = err.Error()
				atomic.AddInt64(&failCount, 1)
			} else if turn.ErrMessage != "" && turn.Text == "" {
				r.err = turn.ErrMessage
				atomic.AddInt64(&failCount, 1)
			} else {
				r.ok = true
				r.text = turn.Text
				r.tokens = turn.Usage
				atomic.AddInt64(&okCount, 1)
			}
			results[i] = r
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(t0)

	// 输出（按序号排序，便于阅读）
	sort.Slice(results, func(a, b int) bool { return results[a].idx < results[b].idx })
	var lat []int64
	for _, r := range results {
		lat = append(lat, r.ms)
		if r.ok {
			txt := r.text
			if len([]rune(txt)) > 40 {
				txt = string([]rune(txt)[:40]) + "…"
			}
			fmt.Printf("  [%2d] ✓ %6dms  正文=%-42q  in=%d cached=%d out=%d\n",
				r.idx+1, r.ms, txt, r.tokens.InputTokens, r.tokens.CachedInputTokens, r.tokens.OutputTokens)
		} else {
			fmt.Printf("  [%2d] ✗ %6dms  err=%s\n", r.idx+1, r.ms, r.err)
		}
	}
	sort.Slice(lat, func(a, b int) bool { return lat[a] < lat[b] })
	fmt.Printf("\n成功 %d / 失败 %d ｜ 总耗时 %s ｜ 中位延迟 %dms ｜ 最大延迟 %dms\n",
		okCount, failCount, elapsed.Round(time.Millisecond), lat[len(lat)/2], lat[len(lat)-1])

	// 池状态（压测后：应看到各槽成功计数分布）
	if p := c.Pool(); p != nil {
		st := p.Stats()
		fmt.Printf("\n=== 压测后池状态 ===\n")
		fmt.Printf("  槽数=%v 新鲜=%v 在飞合计=%v\n", st["slots_total"], st["slots_fresh"], st["inflight_total"])
		if slots, ok := st["slots"].([]prism.SlotStat); ok {
			for _, s := range slots {
				fmt.Printf("  · 槽%d 新鲜=%v 年龄=%ds 成功=%d 失败=%d proj=%s\n",
					s.ID, s.Fresh, s.AgeSec, s.Successes, s.Failures, s.ProjectID)
			}
		}
	}
	if failCount > 0 {
		os.Exit(1)
	}
}

func fatal(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "FATAL: "+f+"\n", a...)
	os.Exit(1)
}
