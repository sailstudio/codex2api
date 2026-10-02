package prism

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ============================================================
// sentinel token 铸造（2026-09-19 起 /api/* 的强制一次性门禁）。
//
// 机制：openai-sentinel-token 头，**严格一次性**——复用一律
// 403 "Request verification failed"（与 IP/账号/请求形状无关）。
//
// 关键结论（本机实测）：**不需要浏览器**。sentinel SDK 是纯 JS，
// 用 node 的 vm 就地执行即可算出 proof；再与 sentinel.openai.com
// 换 challenge、二次计算得 token。因此网关可以在纯 Go + node 的
// 形态下自铸，无需 Playwright 参与。
//
// 与上游各实现（chatgpt-prism2api 的 sidecar）的关键差别：
//   上游：Python sidecar（curl_cffi 拉 assets + node runner）
//   本包：Go 拉 assets + node runner，且直接内嵌 runner 脚本，
//         避免依赖外部仓库目录。
// ============================================================

// 池化：铸造一个约 2~4s（node 冷启 + 两次往返），用浅池削峰。
// token 有短时效（实测分钟级老化），故池只削峰不囤货。
const (
	sentinelFreshTTL = 45 * time.Second
	sentinelPoolSize = 8
	mintFailBackoff  = 30 * time.Second
)

var (
	poolOnce sync.Once
	pool     chan sentinelTok
	// mint 失败负缓存：sidecar/node 故障时避免重试链连环打爆。
	mintFailMu sync.Mutex
	mintFailAt time.Time
)

type sentinelTok struct {
	val string
	at  time.Time
}

// Mint 取一枚 sentinel token：优先池中新鲜票，否则现铸。
// 调用方**每个请求都必须取一枚新的**（一次性）。
func (c *Client) Mint(ctx context.Context) (string, error) {
	if mintHook != nil {
		return mintHook(ctx) // 测试接缝
	}
	poolOnce.Do(func() {
		pool = make(chan sentinelTok, sentinelPoolSize)
		go c.refillLoop()
	})
	for {
		select {
		case t := <-pool:
			if time.Since(t.at) <= sentinelFreshTTL {
				return t.val, nil
			}
			continue // 老化票丢弃
		default:
		}
		break
	}
	// 池空：短等补货，超时自己铸。
	mintFailMu.Lock()
	backoff := time.Since(mintFailAt) < mintFailBackoff
	mintFailMu.Unlock()
	if !backoff {
		wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		select {
		case t := <-pool:
			cancel()
			if time.Since(t.at) <= sentinelFreshTTL {
				return t.val, nil
			}
		case <-wctx.Done():
			cancel()
		}
	}
	tok, err := c.mintNow(ctx)
	if err != nil {
		mintFailMu.Lock()
		mintFailAt = time.Now()
		mintFailMu.Unlock()
		return "", err
	}
	return tok, nil
}

// refillLoop 后台保温（铸一枚歇 500ms，不积压）。
func (c *Client) refillLoop() {
	bo := time.Duration(0)
	for {
		if len(pool) >= cap(pool) {
			time.Sleep(10 * time.Second)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		tok, err := c.mintNow(ctx)
		cancel()
		if err != nil {
			if bo < 60*time.Second {
				bo = mintFailBackoff
			}
			c.cfg.Logf("prism: sentinel 铸造失败（退避 %s）: %v", bo, err)
			time.Sleep(bo)
			continue
		}
		bo = 0
		select {
		case pool <- sentinelTok{val: tok, at: time.Now()}:
		default:
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// mintNow 现铸一枚 sentinel token。
//
// 流程（与上游 sidecar 一致）：
//  1. 拉 sentinel 域 assets 建立 cookie 会话（Go 侧完成，复用 client 的 http）
//  2. node <runner> --challenge-stdin 就地算 proof（stdin 协议）
//  3. POST {sentinel}/backend-api/sentinel/req {p,id,flow} → challenge
//  4. 把 challenge 回灌 runner → 产出 token
func (c *Client) mintNow(ctx context.Context) (string, error) {
	runner := envOr("PRISM_SENTINEL_RUNNER", c.cfg.SentinelRunner)
	if runner == "" {
		runner = "/tmp/prism_sidecar/sentinel-runner.js"
	}
	sdk := envOr("PRISM_SENTINEL_SDK", c.cfg.SDKPath)
	nodeBin := envOr("PRISM_NODE_BIN", c.cfg.NodeBin)
	if strings.TrimSpace(nodeBin) == "" {
		nodeBin = "node"
	}

	// ── 路径 A：外部铸造服务 ─────────────────────────────────────────────
	// 官方镜像内**没有 node**，无法本地铸造。此时把铸造放在宿主常驻服务
	// （cmd/prism-sentinel/daemon.js），这里只做 HTTP 取票：
	//   PRISM_SENTINEL_URL=http://127.0.0.1:8791
	// 或用一个任意命令（更通用，容器里可写 curl）：
	//   PRISM_SENTINEL_CMD="curl -sS --max-time 60 http://host.docker.internal:8791/token"
	if u := strings.TrimSpace(os.Getenv("PRISM_SENTINEL_URL")); u != "" {
		return c.mintViaURL(ctx, u)
	}
	if cmdline := strings.TrimSpace(os.Getenv("PRISM_SENTINEL_CMD")); cmdline != "" {
		return c.mintViaCommand(ctx, cmdline)
	}

	// ── 路径 B：本地 node 铸造（宿主直接跑时最省事）─────────────────────
	if _, err := statFile(runner); err != nil {
		return "", fmt.Errorf("prism: 缺 sentinel-runner.js（%s）且未配置 "+
			"PRISM_SENTINEL_URL / PRISM_SENTINEL_CMD（容器内无 node 时必须配其一）: %w", runner, err)
	}

	devID := newUUID4()
	flow := "prism_inference"

	// 1) 拉 assets（建立 sentinel 域 cookie；失败不致命，仅告警）
	if err := c.fetchSentinelAssets(ctx); err != nil {
		c.cfg.Logf("prism: sentinel assets 拉取告警: %v", err)
	}

	// 2) 起 runner
	args := []string{runner, "--challenge-stdin",
		"--flow", flow, "--device-id", devID,
		"--page-url", strings.TrimRight(c.cfg.Base, "/") + "/",
		"--user-agent", c.cookie.UserAgent,
		"--sdk", sdk,
		"--script-src", sentinelOrigin + "/sentinel/" + sentinelSV + "/sdk.js",
		"--width", "1920", "--height", "1080", "--cores", "32",
		"--language", "zh-CN", "--languages", "zh-CN,zh,en-US,en", "--no-cookie"}
	cmd := exec.CommandContext(ctx, nodeBin, args...)
	cmd.Env = append(cmd.Environ(), "SENTINEL_CONFIG=__none__")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("prism: 启动 node runner 失败: %w", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	// 3) 读一行 proof
	proof, err := readJSONLine(stdout, 60*time.Second)
	if err != nil {
		return "", fmt.Errorf("prism: runner proof 读取失败: %w", err)
	}
	if proof["type"] != "proof" || strOf(proof["proof"]) == "" {
		return "", fmt.Errorf("prism: runner 未产出 proof: %v", proof)
	}

	// 4) 换 challenge
	reqBody, _ := json.Marshal(map[string]any{"p": proof["proof"], "id": devID, "flow": flow})
	resp, err := c.do(ctx, "POST", sentinelOrigin+"/backend-api/sentinel/req", string(reqBody), "",
		map[string]string{
			"Origin":       sentinelOrigin,
			"Referer":      sentinelOrigin + "/backend-api/sentinel/frame.html?sv=" + sentinelSV,
			"Content-Type": "text/plain;charset=UTF-8",
		})
	if err != nil {
		return "", fmt.Errorf("prism: sentinel/req: %w", err)
	}
	var challenge map[string]any
	err = json.NewDecoder(resp.Body).Decode(&challenge)
	_ = resp.Body.Close()
	if err != nil {
		return "", fmt.Errorf("prism: sentinel/req 解析: %w", err)
	}
	if strOf(challenge["token"]) == "" {
		return "", fmt.Errorf("prism: sentinel/req 无 challenge token")
	}

	// 5) 回灌 challenge → token
	line, _ := json.Marshal(map[string]any{"type": "challenge", "challenge": challenge})
	if _, err := stdin.Write(append(line, '\n')); err != nil {
		return "", err
	}
	_ = stdin.Close()

	token := ""
	for {
		m, err := readJSONLine(stdout, 60*time.Second)
		if err != nil {
			break
		}
		if m["type"] == "token" && strOf(m["token"]) != "" {
			token = strOf(m["token"])
			break
		}
	}
	_ = cmd.Wait()
	if token == "" {
		return "", fmt.Errorf("prism: runner 未产出 sentinel token")
	}
	return token, nil
}

const (
	sentinelOrigin = "https://sentinel.openai.com"
	sentinelSV     = "20260219f9f6"
)

// fetchSentinelAssets 按 HAR 顺序拉 assets（只为拿 sentinel 域 cookie）。
func (c *Client) fetchSentinelAssets(ctx context.Context) error {
	page := strings.TrimRight(c.cfg.Base, "/") + "/"
	items := []struct{ url, accept, referer string }{
		{sentinelOrigin + "/backend-api/sentinel/sdk.js", "*/*", page},
		{sentinelOrigin + "/sentinel/" + sentinelSV + "/sdk.js", "*/*", page},
		{sentinelOrigin + "/backend-api/sentinel/frame.html?sv=" + sentinelSV,
			"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", page},
		{sentinelOrigin + "/sentinel/" + sentinelSV + "/sdk.js", "*/*",
			sentinelOrigin + "/backend-api/sentinel/frame.html?sv=" + sentinelSV},
	}
	for _, it := range items {
		resp, err := c.do(ctx, "GET", it.url, "", "", map[string]string{"Accept": it.accept, "Referer": it.referer})
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 && resp.StatusCode != 304 {
			return fmt.Errorf("asset %s HTTP %d", it.url, resp.StatusCode)
		}
	}
	return nil
}
