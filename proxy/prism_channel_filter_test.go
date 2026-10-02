package proxy

import (
	"testing"

	"github.com/codex2api/auth"
)

func prismTestAccount() *auth.Account {
	return &auth.Account{DBID: 900, UpstreamType: auth.UpstreamPrism, AccessToken: "at-prism"}
}

func codexTestAccount() *auth.Account {
	return &auth.Account{DBID: 901, RefreshToken: "rt-codex"}
}

// TestPrismChannelAccountFilter 钉死 prism 渠道 Key 只选 Prism 账号。
func TestPrismChannelAccountFilter(t *testing.T) {
	f := prismChannelAccountFilter("gpt-5.6-sol")
	if !f(prismTestAccount()) {
		t.Fatal("Prism 账号被 prism 渠道过滤器拒绝")
	}
	if f(codexTestAccount()) {
		t.Fatal("Codex 账号被放进 prism 渠道")
	}
	if f(nil) {
		t.Fatal("nil 账号被放行")
	}
	// 未指定模型时同样放行：Prism 的模型由对话材料决定（见 prism/responses.go）。
	if !prismChannelAccountFilter("")(prismTestAccount()) {
		t.Fatal("未指定模型时 Prism 账号被拒绝")
	}
}

// TestDefaultModelFilterExcludesPrism 钉死 auto 路径（未限定 upstream_channel
// 的 Key）不会选中 Prism 账号。
//
// 背景：accountFilterForModel 显式排除所有 relay-style 账号。Prism 属于
// relay-style，因此必须靠 upstream_channel=prism 的 Key 才能被选到 —— 这条
// 约束保证了 Codex 流量永远不会落到 Prism 适配器上。
func TestDefaultModelFilterExcludesPrism(t *testing.T) {
	f := accountFilterForModel("gpt-5.6-sol")
	if f(prismTestAccount()) {
		t.Fatal("Prism 账号被默认（auto）模型过滤器选中；Codex 流量会误入 Prism 通道")
	}
}

// TestPrismIsRelayStyle 钉死 Prism 被归类为 relay-style。
// auto 路径的排除逻辑依赖该归类，改错会让 Prism 账号混进 Codex 调度。
func TestPrismIsRelayStyle(t *testing.T) {
	if !prismTestAccount().IsRelayStyle() {
		t.Fatal("Prism 账号未被归类为 relay-style")
	}
	if !prismTestAccount().IsPrismAPI() {
		t.Fatal("Prism 账号未被 IsPrismAPI 识别")
	}
}
