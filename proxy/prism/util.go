package prism

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// 小工具集（避免引入第三方依赖，与仓库既有风格一致）。

func statFile(p string) (os.FileInfo, error) { return os.Stat(p) }

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// readJSONLine 从流里读一行 JSON（带超时；超时返回 error）。
func readJSONLine(r io.Reader, timeout time.Duration) (map[string]any, error) {
	type res struct {
		m   map[string]any
		err error
	}
	ch := make(chan res, 1)
	go func() {
		br := bufio.NewReader(r)
		line, err := br.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			ch <- res{nil, err}
			return
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			ch <- res{nil, fmt.Errorf("非 JSON 行: %s", truncStr(string(line), 120))}
			return
		}
		ch <- res{m, nil}
	}()
	select {
	case v := <-ch:
		return v.m, v.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("读 runner 输出超时(%s)", timeout)
	}
}

func truncStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// firstNonBlank 返回第一个非空（去空白后）的字符串。
func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// newUUID4 生成 v4 UUID（避免引入 uuid 依赖）。
func newUUID4() string {
	b := make([]byte, 16)
	f, err := os.Open("/dev/urandom")
	if err == nil {
		_, _ = f.Read(b)
		_ = f.Close()
	} else {
		now := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(now >> (i * 4))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
