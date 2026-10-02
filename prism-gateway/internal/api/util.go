package api

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"prism-gateway/internal/prism"
)

// ============================================================================
// 小工具：文件打开、header 写入检测、观测辅助。
// ============================================================================

// openFile 打开文件（抽出来便于测试替换）。
var openFile = func(path string) (*os.File, error) { return os.Open(path) }

// headerWritten 判断响应头是否已写（panic 恢复时决定是否还能写错误体）。
// 标准库没有公开 API，这里用包装类型探测。
type writeTracked interface {
	Written() bool
}

func headerWritten(w http.ResponseWriter) bool {
	if t, ok := w.(writeTracked); ok {
		return t.Written()
	}
	return false
}

// cacheHits 取缓存命中数（观测用）。
func cacheHits(s *Server) int64 {
	h, _, _, _ := s.cache.Stats()
	return h
}

// sentinelStatus 返回 sentinel 池状态（观测用）。
func sentinelStatus() map[string]any {
	sp := prism.GetSentinel()
	if sp == nil || !sp.Enabled() {
		return map[string]any{"enabled": false}
	}
	starts, minted, failed, fromPool, depth := sp.Stats()
	return map[string]any{
		"enabled": true, "takes": starts, "minted": minted,
		"failed": failed, "from_pool": fromPool, "depth": depth,
	}
}

// breakerStatus 返回熔断器状态（观测用）。
func breakerStatus(pool *prism.Pool) map[string]any {
	st := pool.Breaker().Stats()
	return map[string]any{
		"open": st.Open, "fail_rate": st.FailRate, "samples": st.Samples,
		"trips": st.TotalTrips, "total_samples": st.TotalSamples,
	}
}

// modelIDs 返回目录里的模型 id。
func modelIDs(cat []prism.ModelSpec) []string {
	out := make([]string, 0, len(cat))
	for _, m := range cat {
		out = append(out, m.ID)
	}
	return out
}

// writeAccountHealth 输出账号健康表（纯文本，管理端点用）。
func writeAccountHealth(w http.ResponseWriter, pool *prism.Pool) {
	fmt.Fprint(w, "\n账号健康:\n")
	for _, c := range pool.Clients() {
		a := c.Account()
		errCount, lastErr, cooling := a.Health()
		hot := "冷"
		if c.SandboxToken() != "" {
			hot = "热"
		}
		state := "ok"
		if cooling {
			state = "冷却"
		}
		label := strings.TrimSpace(a.Label)
		if label == "" {
			label = a.ID
		}
		fmt.Fprintf(w, "  %-24s %s 沙箱=%s 连续错误=%d 状态=%s", maskClient(label), a.ID, hot, errCount, state)
		if lastErr != "" {
			fmt.Fprintf(w, " 末次错误=%s", trimErr(lastErr))
		}
		fmt.Fprint(w, "\n")
	}
}
