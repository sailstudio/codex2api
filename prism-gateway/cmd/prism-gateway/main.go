// prism-gateway 是一个对接 prism.openai.com 私协议的高性能反代网关。
//
// 能力：高并发（号池 + 单飞预热）、低延迟（热沙箱 + 边到边合成流）、
// 工具调用仿真、流式响应、图片解析（base64 还原通道）、读写 token 缓存计数。
// 零第三方依赖：仅 Go 标准库，便于三架构交叉编译。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"prism-gateway/internal/api"
	"prism-gateway/internal/config"
	"prism-gateway/internal/prism"
	"prism-gateway/internal/usage"
)

var (
	flagConfig     = flag.String("config", "", "配置文件路径（JSON，可选；环境变量优先级更高）")
	flagListen     = flag.String("listen", "", "监听地址，覆盖配置（如 127.0.0.1:8787）")
	flagAccounts   = flag.String("accounts", "", "账号文件路径（accounts.json）")
	flagDoctor     = flag.Bool("doctor", false, "自检模式：校验账号 cookie / 会话 / 沙箱预热链后退出")
	flagDoctorAcct = flag.Int("doctor-account", 0, "自检时只测第 N 个账号（1 起；0 = 全部）")
	flagVersion    = flag.Bool("version", false, "打印版本并退出")
	flagDumpPrompt = flag.Bool("dump-prompt", false, "打印生效的系统指令并退出")
)

// version 由 -ldflags 注入（构建脚本）。
var version = "dev"

func main() {
	flag.Parse()

	if *flagVersion {
		fmt.Printf("prism-gateway %s (%s/%s, %s)\n", version, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return
	}

	cfg, err := config.Load(*flagConfig)
	if err != nil {
		fatal("配置加载失败: %v", err)
	}
	if *flagListen != "" {
		cfg.Listen = *flagListen
	}
	if *flagDumpPrompt {
		sp := strings.TrimSpace(cfg.SystemPrompt)
		if sp == "" && cfg.SystemPromptFile != "" {
			if b, err := os.ReadFile(cfg.SystemPromptFile); err == nil {
				sp = string(b)
			}
		}
		if sp == "" {
			sp = prism.DefaultSystemPrompt
		}
		fmt.Println(sp)
		return
	}
	setupLogging(cfg.LogLevel)

	// ---- 账号装载 ----
	accounts, err := prism.LoadAccounts(*flagAccounts, cfg.InlineCookies())
	if err != nil {
		fatal("账号装载失败: %v\n提示：设置 PG_ACCOUNTS_FILE=<accounts.json> 或 PG_COOKIES=\"<cookie串>\"", err)
	}
	log.Printf("已装载 %d 个账号", len(accounts))
	for i, a := range accounts {
		if miss := prism.ValidateAccount(a); len(miss) > 0 {
			log.Printf("⚠ 账号 %d（%s）缺少关键 cookie: %s —— 缺 oai-sc 会导致 401", i+1, a.ID, strings.Join(miss, ", "))
		}
	}

	// ---- sentinel 池（对话面风控，需登录侧车）----
	sn := prism.NewSentinel(prism.SentinelOptions{
		PoolSize: prism.SentinelPoolSizeFromEnv(),
		Logf:     log.Printf,
	})
	prism.SetSentinel(sn)
	if sn != nil {
		log.Printf("sentinel 池已启用（浅池削峰，新鲜窗 45s）")
	} else {
		log.Printf("ℹ sentinel 池未启用：未配置 PG_SENTINEL_SIDECARS。" +
			"若上游强制校验 openai-sentinel-token，start 会 403 —— 见 README「登录配合」章节")
	}

	// ---- 号池 ----
	tc := prism.DefaultTransportConfig()
	tc.ForceHTTP1 = cfg.ForceHTTP1
	pool := prism.NewPool(accounts, prism.PoolOptions{
		Client: prism.Options{
			Origin:          cfg.Origin,
			UserAgent:       cfg.UserAgent,
			StartTimeout:    cfg.StartTimeout,
			PollCallTimeout: cfg.PollCallTimeout,
			PollBudget:      cfg.PollBudget,
			SyncPollTries:   cfg.SyncPollTries,
			StartAttempts:   cfg.StartAttempts,
			MaxReconnects:   cfg.MaxReconnects,
			Transport:       tc,
		},
		PerAccountConc:  cfg.PerAccountConc,
		CooldownOnErr:   cfg.CooldownOnAuthErr,
		RetryBackoffMin: cfg.RetryBackoffMin,
		RetryBackoffMax: cfg.RetryBackoffMax,
		PrewarmWorkers:  cfg.PrewarmWorkers,
		WarmOnStart:     cfg.WarmOnStart && !*flagDoctor,
		Logf:            log.Printf,
	})
	defer pool.Close()

	// ---- 自检模式 ----
	if *flagDoctor {
		os.Exit(runDoctor(pool, *flagDoctorAcct, cfg))
	}

	// ---- 计数与缓存 ----
	est := usage.DefaultEstimator()
	tracker := usage.NewTracker(cfg.CacheTTL, cfg.CacheMaxEntries, est)
	counters := usage.NewCounters()

	// 缓存后台清理。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		t := time.NewTicker(2 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if n := tracker.Purge(); n > 0 {
					log.Printf("缓存清理: 移除 %d 条过期前缀", n)
				}
			}
		}
	}()

	// ---- HTTP 服务 ----
	srv := api.New(api.Deps{
		Config:   cfg,
		Pool:     pool,
		Catalog:  prism.DefaultModels(),
		Cache:    tracker,
		Counters: counters,
	})
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		// 注意：不设 WriteTimeout —— 生成任务可能跑数分钟（上游劣化时更久），
		// 写超时会拦腰掐断 SSE。改用每轮 PollBudget / UpstreamTimeout 控制。
		IdleTimeout: 120 * time.Second,
	}

	// pprof（可选，仅本机调试）。
	if cfg.PprofListen != "" {
		go func() {
			log.Printf("pprof 监听 %s", cfg.PprofListen)
			_ = http.ListenAndServe(cfg.PprofListen, nil)
		}()
	}

	go func() {
		log.Printf("prism-gateway %s 启动，监听 http://%s", version, cfg.Listen)
		log.Printf("  上游: %s | 账号: %d | 单号并发: %d | 合成流: %d rune/%s",
			cfg.Origin, pool.Len(), cfg.PerAccountConc, cfg.ChunkSize, cfg.ChunkInterval)
		log.Printf("  端点: POST /v1/chat/completions | /v1/messages | /v1/responses | GET /v1/models | GET /metrics /admin/usage /healthz")
		if len(cfg.APIKeys) == 0 {
			log.Printf("  ⚠ 未配置 PG_API_KEYS：不校验客户端 key（请勿暴露到公网）")
		}
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("HTTP 服务退出: %v", err)
		}
	}()

	// ---- 优雅退出 ----
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("收到退出信号，等待在飞请求结束（最多 %s）...", cfg.ShutdownGrace)
	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel2()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("优雅退出超时: %v", err)
	}
	log.Printf("已退出。最终计数：\n%s", counters.FormatReport())
}

// ------------------------------------------------------------------ 自检

// runDoctor 校验账号可用性：cookie → 会话 → 项目 → 沙箱预热链 → 一次真实 start。
// 返回退出码：0 全通过；1 有失败。
func runDoctor(pool *prism.Pool, only int, cfg config.Config) int {
	clients := pool.Clients()
	if len(clients) == 0 {
		fmt.Println("✗ 无账号可测")
		return 1
	}
	failed := 0
	for i, c := range clients {
		if only > 0 && i+1 != only {
			continue
		}
		a := c.Account()
		fmt.Printf("\n===== 账号 %d/%d：%s（%s）\n", i+1, len(clients), a.ID, labelOr(a.Label, "无标签"))

		miss := prism.ValidateAccount(a)
		if len(miss) > 0 {
			fmt.Printf("  [!] 缺少关键 cookie: %s\n", strings.Join(miss, ", "))
			fmt.Printf("      → 缺 oai-sc 会得到 401 'Could not parse your authentication token'\n")
		} else {
			fmt.Printf("  [✓] 关键 cookie 齐备（%s）\n", strings.Join(prism.CookieNames, ", "))
		}

		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		// ① 会话
		if err := c.EnsureSession(ctx); err != nil {
			fmt.Printf("  [✗] /auth/session 失败: %v\n", err)
			fmt.Printf("      → cookie 可能已过期，或 cf_clearance 与新出口 IP/UA 不匹配（换网络会 403）\n")
			cancel()
			failed++
			continue
		}
		fmt.Printf("  [✓] 会话有效（userId=%s）\n", c.UserID())

		// ② 权益
		if snap, err := c.FetchUsage(ctx); err == nil && snap != nil {
			fmt.Printf("  [✓] 权益：plan=%s email=%s\n", labelOr(snap.PlanType, "未知"), labelOr(snap.Email, "未知"))
		}

		// ③ 项目
		pid, err := c.EnsureProject(ctx)
		if err != nil {
			fmt.Printf("  [✗] 项目不可用: %v\n", err)
			cancel()
			failed++
			continue
		}
		fmt.Printf("  [✓] 项目：%s\n", short(pid, 12))

		// ④ 沙箱预热链
		start := time.Now()
		tok, base, err := c.EnsureSandbox(ctx, pid)
		if err != nil {
			fmt.Printf("  [✗] 沙箱预热失败（耗时 %s）: %v\n", time.Since(start).Round(time.Second), err)
			fmt.Printf("      → 若卡在 wait-for-sync/syncing，说明 Y-Sweet 令牌交棒没走通\n")
			cancel()
			failed++
			continue
		}
		fmt.Printf("  [✓] 沙箱就绪（耗时 %s）：%s（token %s…）\n",
			time.Since(start).Round(time.Second), short(base, 48), short(tok, 10))

		// ⑤ 探活
		if err := c.Heartbeat(ctx); err != nil {
			fmt.Printf("  [!] 沙箱探活异常: %v\n", err)
		} else {
			fmt.Printf("  [✓] 沙箱探活正常\n")
		}

		// ⑥ 真实 start（最小轮次）
		turn := &prism.Turn{
			Input:          []prism.InputItem{{Type: "message", Role: "user", Content: []prism.ContentPart{{Type: "input_text", Text: "只回复两个字：收到"}}}},
			ConversationID: prism.NewConversationID(),
			Model:          firstServerModel(),
			Effort:         "low",
			ProjectID:      pid,
		}
		st, err := c.StartTurn(ctx, turn)
		if err != nil {
			fmt.Printf("  [✗] start 失败: %v\n", err)
			fmt.Printf("      → 若为 403 Forbidden，需要 sentinel token（见 README 登录配合）\n")
			cancel()
			failed++
			continue
		}
		fmt.Printf("  [✓] start 成功（request_id=%s）\n", short(st.RequestID, 16))
		res, err := c.PollTurn(ctx, st, nil)
		if err != nil {
			fmt.Printf("  [✗] 轮询失败: %v\n", err)
			cancel()
			failed++
			continue
		}
		if res.Err != "" {
			fmt.Printf("  [✗] 上游返回错误: %s\n", res.Err)
			cancel()
			failed++
			continue
		}
		fmt.Printf("  [✓] 端到端闭环成功！polls=%d 模型回复: %q\n", res.Polls, short(res.Text, 80))
		if res.Reasoning != "" {
			fmt.Printf("      思考摘要（%d 字符）：%s\n", len(res.Reasoning), short(res.Reasoning, 60))
		}
		cancel()
	}

	fmt.Println()
	if failed == 0 {
		fmt.Println("自检通过：所有被测账号端到端可用 ✓")
		return 0
	}
	fmt.Printf("自检结束：%d 个账号失败\n", failed)
	return 1
}

func firstServerModel() string {
	ms := prism.DefaultModels()
	if len(ms) == 0 {
		return "gpt-5.6-sol"
	}
	if s := strings.TrimSpace(ms[0].ServerModelName); s != "" {
		return s
	}
	return ms[0].ID
}

// ------------------------------------------------------------------ 杂项

func setupLogging(level string) {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix("[prism] ")
	switch strings.ToLower(level) {
	case "debug":
		log.SetOutput(os.Stdout)
	default:
		log.SetOutput(os.Stderr)
	}
}

func fatal(format string, args ...any) {
	log.Printf("FATAL: "+format, args...)
	os.Exit(1)
}

func labelOr(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func short(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// 保证 encoding/json 被引用（doctor 打印用）。
var _ = json.Marshal
