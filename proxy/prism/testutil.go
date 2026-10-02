package prism

import "os"

// writeFileBytes 落盘小工具（测试用）。
func writeFileBytes(path string, b []byte) error { return os.WriteFile(path, b, 0o644) }
