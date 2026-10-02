package prism

// ============================================================
// usage 解析：读缓存 / 写缓存 / 思考 / 总量。
//
// 上游字段名会变（不同时期叫过 cached_input_tokens / cache_read_input_tokens
// 等），因此这里做**多别名兼容**，并且把原文留在 Usage.Raw 里便于对账。
//
// 「写缓存」尤其重要：不同上游对 cache creation 的上报策略不同。
// 未上报时记 CacheWriteReported=false，下游据此区分「确实是 0」与
// 「上游没说」——避免缓存计费口径被静默歪曲。
// ============================================================

// 别名表（按优先级）。命中第一个非零/存在的键。
var (
	cacheReadAliases = []string{
		"cached_input_tokens", "cache_read_input_tokens",
		"input_tokens_cached", "cached_tokens",
	}
	cacheWriteAliases = []string{
		"cache_creation_input_tokens", "cache_write_input_tokens",
		"cache_creation_tokens", "input_tokens_cache_creation",
	}
	inputAliases  = []string{"input_tokens", "prompt_tokens"}
	outputAliases = []string{"output_tokens", "completion_tokens"}
	totalAliases  = []string{"total_tokens"}
	reasonAliases = []string{"reasoning_output_tokens", "reasoning_tokens"}
)

// parseUsage 从上游 usage 对象解析出统一口径。
func parseUsage(u map[string]any) Usage {
	if u == nil {
		return Usage{}
	}
	out := Usage{Raw: u, Reported: true}
	out.InputTokens = pickInt(u, inputAliases...)
	out.OutputTokens = pickInt(u, outputAliases...)
	out.TotalTokens = pickInt(u, totalAliases...)

	// 读缓存
	if v, ok := pickIntOK(u, cacheReadAliases...); ok {
		out.CachedInputTokens = v
	}
	// 写缓存（区分「未上报」与「0」）
	if v, ok := pickIntOK(u, cacheWriteAliases...); ok {
		out.CacheWriteTokens = v
		out.CacheWriteReported = true
	}
	// 嵌套形态：input_tokens_details.cached_tokens （OpenAI Responses 风格）
	// 注意：读缓存与写缓存必须**分别**取，不能共用一个 pickIntOK——那会把
	// cache_creation 的值当成 cached 读出来（曾因此把写缓存算进读缓存）。
	if d, ok := u["input_tokens_details"].(map[string]any); ok {
		if v, ok := pickIntOK(d, "cached_tokens", "cache_read_input_tokens"); ok {
			out.CachedInputTokens = v
		}
		if v, ok := pickIntOK(d, "cache_creation_tokens", "cache_creation_input_tokens"); ok {
			out.CacheWriteTokens = v
			out.CacheWriteReported = true
		}
	}
	if d, ok := u["output_tokens_details"].(map[string]any); ok {
		out.ReasoningTokens = pickInt(d, reasonAliases...)
	} else {
		out.ReasoningTokens = pickInt(u, reasonAliases...)
	}

	// total 缺失时自算（读+写+新输入 = 总输入；再叠加输出）
	if out.TotalTokens == 0 {
		out.TotalTokens = out.InputTokens + out.OutputTokens
	}
	return out
}

// EffectiveInput 是「总输入」口径：新输入 + 读缓存 + 写缓存。
// 上游不变量（实测）：这三者之和即总的输入 token 数。
func (u Usage) EffectiveInput() int {
	return u.InputTokens + u.CachedInputTokens + u.CacheWriteTokens
}

// CacheHitRate 缓存命中率（读缓存 / 总输入）。无输入时返回 0。
func (u Usage) CacheHitRate() float64 {
	total := u.EffectiveInput()
	if total <= 0 {
		return 0
	}
	return float64(u.CachedInputTokens) / float64(total)
}

// pickInt 取第一个命中别名的整数值（缺席按 0）。
func pickInt(m map[string]any, keys ...string) int {
	v, _ := pickIntOK(m, keys...)
	return v
}

// pickIntOK 取第一个**存在**键的整数值（用于区分"缺席"与"0"）。
func pickIntOK(m map[string]any, keys ...string) (int, bool) {
	for _, k := range keys {
		if raw, exists := m[k]; exists {
			return intOf(raw), true
		}
	}
	return 0, false
}
