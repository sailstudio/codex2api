package prism

import "context"

// ============================================================
// 测试接缝（seams）。
//
// 生产路径不依赖这些钩子；它们只让上层（proxy 包）的接线测试能够：
//   - 把上游基址指向假上游（httptest），
//   - 跳过真实 sentinel 铸造（需要 node + 网络）。
//
// 之所以放在这里而不是测试文件里：proxy 包是外部包，无法访问 prism 的
// 未导出符号，而接线测试必须验证真实分叉路径。
// ============================================================

// mintHook 若非 nil，则替代真实的 sentinel 铸造。
var mintHook func(ctx context.Context) (string, error)

// SetMintHook 设置 sentinel 铸造钩子（仅测试使用；传 nil 恢复默认）。
func SetMintHook(h func(ctx context.Context) (string, error)) { mintHook = h }

// BaseURLForTest 允许把上游基址改为假上游（仅测试使用）。
var baseURLOverride string

// SetBaseURL 覆盖上游基址（仅测试使用；传空串恢复）。
func SetBaseURL(u string) { baseURLOverride = u }

// CurrentBase 返回当前生效的上游基址。
func CurrentBase(cfg Config) string {
	if baseURLOverride != "" {
		return baseURLOverride
	}
	return cfg.Base
}
