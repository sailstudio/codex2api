package auth

import "strings"

// UpstreamPrism 是 Prism（prism.openai.com）渠道的上游类型标识。
//
// Prism 是 OpenAI 的 LaTeX 编辑器站点，其内置对话走后端私协议
// （/api/llm/response_with_tools_start + status 轮询），与 Codex 的
// chatgpt.com/backend-api/codex/responses 完全不兼容，因此单独作为一类上游。
// 凭据形态与 Codex 相同：复用 access_token 作 Cookie，另需 sentinel token
// （由网关侧 node 铸造，不需要浏览器）。
const UpstreamPrism = "prism"

// isPrismAPILocked 判断账号是否为 Prism 上游（调用方需持锁）。
func (a *Account) isPrismAPILocked() bool {
	if a == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(a.UpstreamType), UpstreamPrism) {
		return false
	}
	// Prism 只需 access_token（作 Cookie）；不依赖 project_id / api_key。
	return strings.TrimSpace(a.AccessToken) != ""
}

// IsPrismAPI 判断账号是否为 Prism 上游账号。
func (a *Account) IsPrismAPI() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isPrismAPILocked()
}

// PrismUserAgent 返回 Prism 渠道应使用的 User-Agent。
//
// 上游按「浏览器身份」做排队/限流，UA 必须与铸造 sentinel、产出材料时一致，
// 否则会出现身份错配（材料来自另一身份）。默认沿用真机实测的 Codex TUI 形态；
// 账号可通过 credentials.user_agent（进 CustomHeaders）覆盖。
func (a *Account) PrismUserAgent() string {
	if a == nil {
		return defaultPrismUserAgent
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.CustomHeaders != nil {
		for k, v := range a.CustomHeaders {
			if strings.EqualFold(strings.TrimSpace(k), "user-agent") && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return defaultPrismUserAgent
}

// defaultPrismUserAgent 与真机实测（CPA 账号 codex_device_profile）一致。
const defaultPrismUserAgent = "codex-tui/0.156.0 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.156.0)"
