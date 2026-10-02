package prism

import (
	"io"
	"os"
	"strings"
)

// 测试用辅助（与 testutil.go 同包）。

// writeFileBytes2 落盘（测试用）。
func writeFileBytes2(path string, b []byte) error { return os.WriteFile(path, b, 0o644) }

// pipePair 造一对管道，返回 (读端, 写端)；写端带 Flush（模拟 http.Flusher）。
func pipePair() (io.Reader, *pipeWriter) {
	pr, pw := io.Pipe()
	return pr, &pipeWriter{pw}
}

// pipeWriter 给 io.PipeWriter 补一个 Flush（io.Pipe 本身逐写透传，无需缓冲）。
type pipeWriter struct{ *io.PipeWriter }

func (p *pipeWriter) Flush() {}

// sbWriter 把事件写进 strings.Builder（同时实现 io.Writer 与 http.Flusher）。
type sbWriter struct{ b *strings.Builder }

func (s *sbWriter) Write(p []byte) (int, error) { return s.b.Write(p) }
func (s *sbWriter) Flush()                      {}

// sysMsgT 造一条指定角色的文本消息（测试用）。
func sysMsgT(role, text string) map[string]any {
	return map[string]any{
		"type": "message", "role": role,
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}
}
