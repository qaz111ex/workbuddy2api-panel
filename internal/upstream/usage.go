// usage.go 出口侧 usage 缓存命中字段的别名统一（吸收上游 25016de）。
//
// 背景：部分上游把真实缓存命中量放在 prompt_tokens_details.cached_tokens，同时把
// cache_read_input_tokens / cached_tokens 这些兼容别名留为 0。严格的按 schema 解析的
// 下游会优先读那些为 0 的别名，于是「命中了」被误判成「没命中」（成本口径直接失真）。
//
// 本文件只做一件事：在响应离开网关前，把各别名与嵌套明细统一到同一个非零命中量。
// 不臆造数据——所有别名都无正值时原样返回（0 就是 0，缺失就是缺失）。
package upstream

// normalizeUsageCacheAliases 统一 usage 里的缓存命中别名：
// 取各口径中第一个正值作为权威命中量，回填到三个顶层别名与 prompt_tokens_details
// （input_tokens_details 只在响应本来就带该嵌套对象时同步——Responses API 形态才用
// 它，Chat-only 客户端不该被凭空塞一个陌生字段）。
//
// 无任何正值 → 返回入参原对象（零改写）；有正值 → 返回**新 map**（不改上游原 map，
// 调用方可能还持有它做统计）。
func normalizeUsageCacheAliases(usage map[string]any) map[string]any {
	best, ok := bestUsageCacheHitTokens(usage)
	if !ok || best <= 0 {
		return usage
	}

	out := cloneUsageMap(usage)
	out["cache_read_input_tokens"] = best
	out["cached_tokens"] = best
	out["prompt_cache_hit_tokens"] = best

	promptDetails := cloneUsageDetails(out, "prompt_tokens_details")
	promptDetails["cached_tokens"] = best
	out["prompt_tokens_details"] = promptDetails

	// 仅在上游本来就下发该嵌套对象时同步（保留形态，不发明字段）。
	if _, exists := out["input_tokens_details"]; exists {
		inputDetails := cloneUsageDetails(out, "input_tokens_details")
		inputDetails["cached_tokens"] = best
		out["input_tokens_details"] = inputDetails
	}

	return out
}

// bestUsageCacheHitTokens 按权威度顺序取第一个正值命中量：
// 嵌套明细（prompt_tokens_details.cached_tokens，实测真实值所在）优先，其次是三个
// 顶层别名，最后是 Responses 形态的 input_tokens_details.cached_tokens。
func bestUsageCacheHitTokens(usage map[string]any) (float64, bool) {
	paths := []struct {
		section string
		key     string
	}{
		{"prompt_tokens_details", "cached_tokens"},
		{"", "prompt_cache_hit_tokens"},
		{"", "cache_read_input_tokens"},
		{"", "cached_tokens"},
		{"input_tokens_details", "cached_tokens"},
	}

	for _, path := range paths {
		var value any
		if path.section == "" {
			value = usage[path.key]
		} else if details, ok := usage[path.section].(map[string]any); ok {
			value = details[path.key]
		}
		if tokens, ok := positiveUsageNumber(value); ok {
			return tokens, true
		}
	}
	return 0, false
}

// positiveUsageNumber 把 JSON 解出的各种数值形态归一为 float64，且只认 > 0
// （0 是「未命中」而非「未知」，不能当选权威值）。
func positiveUsageNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, n > 0
	case float32:
		value := float64(n)
		return value, value > 0
	case int:
		return float64(n), n > 0
	case int64:
		return float64(n), n > 0
	case int32:
		return float64(n), n > 0
	case uint:
		return float64(n), n > 0
	case uint64:
		return float64(n), n > 0
	case uint32:
		return float64(n), n > 0
	default:
		return 0, false
	}
}

// cloneUsageMap 浅拷贝 usage 顶层（嵌套对象仍是共享引用，需要改的嵌套另行 clone）。
func cloneUsageMap(usage map[string]any) map[string]any {
	out := make(map[string]any, len(usage))
	for key, value := range usage {
		out[key] = value
	}
	return out
}

// cloneUsageDetails 拷贝 usage 里某个嵌套对象；缺失/类型不符时返回空 map
// （调用方随后写入权威命中量，语义是「补一个字段」而非「保留脏数据」）。
func cloneUsageDetails(usage map[string]any, key string) map[string]any {
	out := make(map[string]any)
	details, _ := usage[key].(map[string]any)
	for detailKey, value := range details {
		out[detailKey] = value
	}
	return out
}
