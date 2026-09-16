// effort_catalog.go 推理档位（reasoning effort）产品级静态兜底表。
//
// 为什么必须有静态表：上游模型 API 只回固定 `reasoning: {effort: "high"}`，
// **不下发 supportedEfforts**（issue #84 实测），档位信息只能由网关侧兜底，
// 否则 /v1/models 无法暴露选择器（客户端不显示档位）。
//
// 数据来源三级：
//   - 远端 FetchModels / global 探测已解析的 supportedEfforts/defaultEffort 桶（权威，优先）；
//   - 本文件的静态兜底表（远端缺失时补齐）；
//   - 两者皆无 → 不在 /v1/models 输出 effort 字段（omitted，不是空数组）。
//
// 两域关系：**同名模型两域走同一后端，档位能力一致**——国际版生效表
// （globalEffortFallbackAll）= CN 表为基 + 国际版专有模型覆盖（fast-model / gpt-5.6-* 等）。
// 历史版本按 realm 分成两张互不相通的表，并给国际版 deepseek-v4.1-flash 写了仅 ['high']，
// 依据是 WorkBuddy **产品 UI** 的档位选择器缓存（product.ts WORKBUDDY_FALLBACK_MODELS）——
// 那是"UI 提供哪几档"，不是"API 接受哪几档"。实测 low/high/max 服务端真实生效
// （issue #84；参考实现 dsh-codearts 对 cn/intl 用同一张档位表，且注释明确
// "实测 low/high/max 会显著改变 reasoning_content 长度，服务端真实生效"），
// 故国际版专有以外的模型一律继承 CN 档位。
//
// 只列**可枚举**档位的模型；仅有固定默认档（glm-5.1/kimi-* 的 medium）同样入表，
// 但其档位是「可枚举单档」而非「无选择器」，如实暴露。
package upstream

// effortCap 一个模型的档位能力（对齐产品兜底表条目 reasoningEfforts + defaultReasoningEffort）。
type effortCap struct {
	efforts       []string
	defaultEffort string
}

// cnEffortFallback CN / CodeBuddy 面静态兜底表（reference: product.ts CODEBUDDY_FALLBACK_MODELS
// ∪ buddy-adapter.ts REASONING_EFFORTS；issue #84 逐条实测确认）。
var cnEffortFallback = map[string]effortCap{
	"deepseek-v4-flash":   {efforts: []string{"low", "high", "max"}},
	"deepseek-v4.1-flash": {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"deepseek-v4-pro":     {efforts: []string{"low", "high", "xhigh"}, defaultEffort: "high"},
	"hy4-preview":         {efforts: []string{"high"}, defaultEffort: "high"},
	"hy4-preview-x":       {efforts: []string{"high"}},
	"hy3":                 {efforts: []string{"low", "high"}, defaultEffort: "high"},
	"hy3-x":               {efforts: []string{"low", "high"}, defaultEffort: "high"},
	"glm-5.3":             {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.3-flash":       {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.2":             {efforts: []string{"high", "xhigh"}, defaultEffort: "high"},
	"glm-5.1":             {efforts: []string{"medium"}},
	"glm-5v-turbo":        {efforts: []string{"medium"}},
	"kimi-k3-1":           {efforts: []string{"medium"}},
	"kimi-k2.7":           {efforts: []string{"medium"}},
	"kimi-k2.6":           {efforts: []string{"medium"}},
	"minimax-m3":          {efforts: []string{"medium"}},
}

// globalEffortFallback 国际版**专有**模型静态兜底表（WorkBuddy 独有 / 命名别名）。
// 同名共享模型不在此重复列出——由 globalEffortFallbackAll 从 CN 表继承（见文件头注释）。
var globalEffortFallback = map[string]effortCap{
	"fast-model":       {efforts: []string{"medium"}},
	"balanced-model":   {efforts: []string{"medium"}},
	"primary-model":    {efforts: []string{"high"}},
	"hy4-preview-f":    {efforts: []string{"high"}, defaultEffort: "high"},
	"gpt-6-astra":      {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.6-sol":      {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.6-terra":    {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.6-luna":     {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.5":          {efforts: []string{"low", "medium", "high", "xhigh"}, defaultEffort: "high"},
	"gpt-5.4":          {efforts: []string{"low", "medium", "high", "xhigh"}, defaultEffort: "high"},
	"gpt-5.3-codex":    {efforts: []string{"medium"}},
	"gemini-3.5-flash": {efforts: []string{"medium"}},
	"kimi-k3":          {efforts: []string{"medium"}},
}

// globalEffortFallbackAll 国际版生效静态表 = CN 表为基 + 国际版专有条目覆盖。
// 启动时合成一次（只读，运行期零开销）：同名模型档位一致，专有模型以国际表为准。
var globalEffortFallbackAll = mergeEffortFallback(cnEffortFallback, globalEffortFallback)

// mergeEffortFallback 以 base 为基、override 覆盖，返回新表（不修改入参）。
func mergeEffortFallback(base, override map[string]effortCap) map[string]effortCap {
	out := make(map[string]effortCap, len(base)+len(override))
	for id, cap := range base {
		out[id] = cap
	}
	for id, cap := range override {
		out[id] = cap
	}
	return out
}

// staticEffortCap 按 realm 取静态兜底条目；未命中返回 zero effortCap（efforts=nil）。
// realm 经 realmKey 归一化（空 → "cn"），与 efforts 缓存桶同口径。
func staticEffortCap(realm, model string) effortCap {
	if realmKey(realm) == "global" {
		return globalEffortFallbackAll[model]
	}
	return cnEffortFallback[model]
}

// EffortListing 计算模型在 /v1/models 应暴露的 effort 能力（三级查找 + 默认档防御）。
//
// remoteEfforts/remoteDefault 为远端（FetchModels / global 探测）已解析值；
// remoteEfforts 非空时以其为权威（不回落到静态表），否则落到产品级静态兜底表；
// 两者皆无 → efforts 返回 nil（调用方省略字段，不输出空数组）。
//
// defaultEffort 仅在「efforts 非空且 default 命中 efforts」时才返回
// （对齐参考仓库 resolveModel 的 `defaultEffort ∈ efforts` 防御：不宣称不支持的默认档）。
// remoteDefault 空串不回落到静态默认档——默认档随档位表同源：remote 有档位就用 remote 默认档，
// 静态兜底档位就用静态默认档，避免跨源拼接出「档位是静态、默认档是 remote」的矛盾组合。
func EffortListing(realm, model string, remoteEfforts []string, remoteDefault string) (efforts []string, defaultEffort string) {
	var src effortCap
	switch {
	case len(remoteEfforts) > 0:
		src = effortCap{efforts: remoteEfforts, defaultEffort: remoteDefault}
	default:
		src = staticEffortCap(realm, model)
	}
	if len(src.efforts) == 0 {
		return nil, ""
	}
	efforts = append([]string(nil), src.efforts...)
	if src.defaultEffort != "" && containsEffort(efforts, src.defaultEffort) {
		defaultEffort = src.defaultEffort
	}
	return efforts, defaultEffort
}

// containsEffort 档位成员判定（精确匹配，对齐参考仓库 `efforts.includes(defaultEffort)`）。
func containsEffort(efforts []string, want string) bool {
	for _, e := range efforts {
		if e == want {
			return true
		}
	}
	return false
}

// globalEffortMap global 域降级用的 effort 能力表：静态兜底表（含 CN 继承）为基，
// 远端桶覆盖（权威优先）。
//
// prepareBody 对 global 请求调此函数（而非直接用远端桶），因为上游不下发
// supportedEfforts——此时也必须按静态表降级/放行。国际版共享模型与 CN 同档位
// （issue #84：deepseek-v4.1-flash low/high/max 服务端真实生效），国际版专有模型
// 以 globalEffortFallback 为准（gpt-5.6-* 等）。
// 语义对齐参考仓库 effortsFor（remoteMeta → productFallback → 静态表），只取前两级：
// 远端桶（探测已解析）→ 静态表（globalEffortFallbackAll）。
func globalEffortMap(remoteEfforts map[string][]string, remoteDefaults map[string]string) (map[string][]string, map[string]string) {
	efforts := make(map[string][]string, len(globalEffortFallbackAll)+len(remoteEfforts))
	defs := make(map[string]string, len(globalEffortFallbackAll)+len(remoteDefaults))
	// 静态兜底为基（CN 继承 + 国际版专有覆盖）。
	for id, cap := range globalEffortFallbackAll {
		efforts[id] = append([]string(nil), cap.efforts...)
		if cap.defaultEffort != "" {
			defs[id] = cap.defaultEffort
		}
	}
	// 远端权威覆盖（仅当远端确实下发了该模型档位）。
	for id, v := range remoteEfforts {
		if len(v) > 0 {
			efforts[id] = v
		}
	}
	for id, v := range remoteDefaults {
		if v != "" {
			defs[id] = v
		}
	}
	return efforts, defs
}
