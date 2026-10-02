package prism

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// ============================================================
// 外部 sentinel 铸造：两条取票路径。
//
// 背景：sentinel 铸造依赖 node（跑 SDK 算 proof），而 codex2api 的官方镜像是
// Go 构建的轻量镜像，**容器内没有 node**。因此支持把铸造外置：
//
//	PRISM_SENTINEL_URL  → 直连宿主常驻服务的 HTTP 接口（最简单）
//	PRISM_SENTINEL_CMD  → 任意命令，取 stdout 末行作 token（最通用，
//	                      容器内可用 curl 打通 host.docker.internal）
//
// 两者都做了：末行提取（容忍命令附带日志）、超时、错误透出。
// ============================================================

// mintViaURL 从外部铸造服务取一枚 token（GET <url>/token 或 <url>）。
func (c *Client) mintViaURL(ctx context.Context, base string) (string, error) {
	endpoint := strings.TrimRight(base, "/")
	if !strings.HasSuffix(endpoint, "/token") {
		endpoint += "/token"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	cli := &http.Client{Timeout: 90 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return "", fmt.Errorf("prism: 外部 sentinel 服务不可达（%s）: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	tok := lastNonEmptyLine(string(body))
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("prism: 外部 sentinel 服务返回 %d: %s", resp.StatusCode, truncStr(tok, 200))
	}
	if tok == "" {
		return "", fmt.Errorf("prism: 外部 sentinel 服务返回空 token")
	}
	return tok, nil
}

// mintViaCommand 执行一个命令，取 stdout 末个非空行作为 token。
func (c *Client) mintViaCommand(ctx context.Context, cmdline string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", cmdline)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("prism: sentinel 命令失败: %w", err)
	}
	tok := lastNonEmptyLine(string(out))
	if tok == "" {
		return "", fmt.Errorf("prism: sentinel 命令未输出 token")
	}
	return tok, nil
}

// lastNonEmptyLine 取最后一个非空行（容忍命令输出附带日志/JSON）。
func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		// 命令可能返回 JSON（如 {"token":"..."}），兼容剥一层。
		v := strings.TrimSpace(lines[i])
		if v == "" {
			continue
		}
		if strings.HasPrefix(v, "{") {
			if t := jsonField(v, "token"); t != "" {
				return t
			}
		}
		return v
	}
	return ""
}

// jsonField 用极简方式取 JSON 顶层字符串字段（避免为了一个字段引入结构体）。
func jsonField(s, key string) string {
	needle := `"` + key + `"`
	i := strings.Index(s, needle)
	if i < 0 {
		return ""
	}
	rest := s[i+len(needle):]
	j := strings.Index(rest, ":")
	if j < 0 {
		return ""
	}
	rest = strings.TrimSpace(rest[j+1:])
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	rest = rest[1:]
	if k := strings.Index(rest, `"`); k >= 0 {
		return rest[:k]
	}
	return ""
}

var _ = time.Second
