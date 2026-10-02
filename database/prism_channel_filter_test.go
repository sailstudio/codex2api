package database

import (
	"strings"
	"testing"
)

// TestAccountChannelFilterSQL_PrismIsolated 钉死 Prism 渠道的隔离性。
//
// 背景：Prism 是 relay-style 上游（见 proxy/prism_channel.go），与 Codex 的
// 上游协议完全不同。Codex 通道用「反向排除」表达（NOT IN (...)），若忘记把
// prism 加进排除列表，Prism 账号会被 Codex 调度器选中，随后被 OpenAI 适配器
// 以"不是 codex 账号"为由拒绝 —— 表现为账号看似在线却永远失败。
func TestAccountChannelFilterSQL_PrismIsolated(t *testing.T) {
	const expr = `upstream_type`

	codex := accountChannelFilterSQL(UpstreamChannelCodex, expr)
	if !strings.Contains(codex, "NOT IN") {
		t.Fatalf("codex filter should be a NOT IN exclusion list, got %q", codex)
	}
	if !strings.Contains(codex, "'prism'") {
		t.Fatalf("codex filter must exclude prism rows, got %q", codex)
	}
	for _, other := range []string{"'grok'", "'antigravity'", "'claude'"} {
		if !strings.Contains(codex, other) {
			t.Fatalf("codex filter lost existing exclusion %s: %q", other, codex)
		}
	}

	prism := accountChannelFilterSQL(UpstreamChannelPrism, expr)
	if !strings.Contains(prism, "= 'prism'") {
		t.Fatalf("prism filter should select prism rows, got %q", prism)
	}
	if strings.Contains(prism, "NOT IN") {
		t.Fatalf("prism filter must not be an exclusion list, got %q", prism)
	}

	// 未限定（auto）时不加任何过滤，保持调度器可跨渠道挑选。
	if got := accountChannelFilterSQL(UpstreamChannelAuto, expr); got != "" {
		t.Fatalf("auto channel should add no filter, got %q", got)
	}
}

// TestUpstreamChannelPrismConstant 钉死常量取值（凭据里的字面量，不能随手改）。
func TestUpstreamChannelPrismConstant(t *testing.T) {
	if UpstreamChannelPrism != "prism" {
		t.Fatalf("UpstreamChannelPrism = %q, want \"prism\"", UpstreamChannelPrism)
	}
}
