// global 模型目录探测：纯动态产出模型名及其窗口 / 能力元数据（v3-config-merge）。
//
// 探测两路并发：/v3/config（主路，IDE UA 完整能力版）+ 企业端点家族
// （/v2 → /console 补缺），并集 = v3 条目为主、企业端点补 v3 缺失的 id
// （如 gpt-5.3-codex 只在 /v2 下发）。倍率字段（credits）虽随目录下发，但
// 只透出展示，不注入 costTier、不参与选号。
//
// 纯动态：不再回落任何静态名单——拉不出目录即意味着该域上游不可用，
// 假名单只会让客户端选到 11102 的模型（产品决策：无兜底）。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// GlobalModelNames 国际版（global realm）已知可用模型名单（PLAN §7.2 附录 21 名）。
// 不作基底/失败兜底（拉不出目录仍视为域不可用）；只在探测成功时**补缺**：上游目录
// 不下发但产品实际可用（含限免）的模型（如 deepseek-v4.1-flash）经此进并集，
// 否则客户端在 /v1/models 里看不到、只能盲猜模型名。
var GlobalModelNames = []string{
	"default-model",
	"fast-model",
	"balanced-model",
	"primary-model",
	"hy4-preview",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"deep-model",
	"deepseek-v4.1-flash",
	"gpt-6-astra",
	"hy4-preview-f",
	"hy3",
	"glm-5.2",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.3-codex",
	"gemini-3.5-flash",
	"glm-5.3",
	"kimi-k3",
	"kimi-k2.6",
}

// fetchGlobalModelsCache 探测结果缓存（语义参照 CN 侧 handler.dynamicModelsCache：1h TTL +
// 5min 失败负缓存）。按 Client 实例持有（effortsMu 同模式），测试新建 Client 即隔离。
// Mutex 内嵌，与 modelList 无并发读路径竞争（唯一读写点本文件内）。
type fetchGlobalModelsCache struct {
	sync.Mutex
	names    []string    // 成功缓存：并集模型名（已去重）；nil = 未探测/失败
	infos    []ModelInfo // 成功缓存：对象形态的全字段条目（窄表/失败形态为 nil）
	fetched  time.Time
	lastFail time.Time
}

// globalModelsTTL / globalModelsFailCooldown 探测缓存时长：成功 1h，失败 5min 负缓存。
const (
	globalModelsTTL          = time.Hour
	globalModelsFailCooldown = 5 * time.Minute
)

// globalModelsProbePaths global 企业模型目录端点候选序列（按 realm 切 base，路径"家族"）：
// /v2 家族优先（实测 /v2/enterprises/personal/models 200 含完整模型表），
// /console 作 fallback（同域旧路径，或 500）。v3-config-merge 后该家族降为企业补充路
// （/v3/config 为主路，与家族并发探测；gpt-5.3-codex 等家族独有模型经此进并集）。
var globalModelsProbePaths = []string{
	"/v2/enterprises/personal/models",
	"/console/enterprises/personal/models",
}

// FetchGlobalModels 探测 global 账号的模型名目录并返回**模型名列表**（无元数据）。
//
// 成功返回并集结果（去重 = 探测结果 ∪ 已知可用名单补缺），缓存 1h；失败（两路全非 2xx /
// 解析失败 / 空列表）记 5min 负缓存，返回 nil（无静态回落）。缓存/负缓存命中：直接返回，零上游调用。
//
// 调用方负责：仅在有 global 账号时调用（无则不探测）；GlobalEnabled 关闭时（逃生门）
// 不得调用——本方法由 globalOn(a) 内部兜底，若账号因开关回落 cn 则返回 nil。
func (c *Client) FetchGlobalModels(a *auth.Auth) []string {
	names, _ := c.fetchGlobalModelsOnce(a)
	return names
}

// FetchGlobalModelInfos 探测 global 账号的模型目录并返回全字段 ModelInfo 列表。
// 与 FetchGlobalModels 共享同一次探测与缓存（names + infos 一体落缓存）：
// 对象形态 200 → 全字段条目；窄表形态 / 探测失败 / 负缓存 / 非 global 路由账号
// → nil（调用方按 ID 名单输出裸条目，不编造字段）。
// 账号因 GlobalEnabled 开关回落 cn 时不探测（globalOn 兜底，零上游调用）。
func (c *Client) FetchGlobalModelInfos(a *auth.Auth) []ModelInfo {
	_, infos := c.fetchGlobalModelsOnce(a)
	return infos
}

// GlobalModelInfosSnapshot 只读 global 模型目录全字段缓存（TTL 内快照）；
// 冷 / 过期 / 窄表形态 / 未探测 → nil。不发起任何上游探测——与
// FetchGlobalModelInfos 的差异点（那个在 miss 时触发探测，服务 /v1/models；
// 本方法服务 /v1/stats 的倍率透出，只读已有数据）。
//
// 注意本 fork 的 fetchGlobalModelsOnce 有「静态名单补缺」：成功探测后 infos 里
// 会追加 GlobalModelNames 的补缺条目（只带 ID、Credits 为空）——本方法原样返回
// 同一批 infos，调用方按 id 查 credits，补缺条目的倍率为空（缺失≠免费）。
func (c *Client) GlobalModelInfosSnapshot() []ModelInfo {
	if c == nil {
		return nil
	}
	c.globalModels.Lock()
	defer c.globalModels.Unlock()
	if len(c.globalModels.infos) == 0 || time.Since(c.globalModels.fetched) >= globalModelsTTL {
		return nil
	}
	return c.globalModels.infos
}

// fetchGlobalModelsOnce 单次探测决策（缓存命中/负缓存/触发探测），返回 (names, infos)。
// 纯动态：成功 = 并集结果去重；一切失败 = nil（不回落静态）。
// infos 仅对象形态成功探测时非 nil。
func (c *Client) fetchGlobalModelsOnce(a *auth.Auth) (names []string, infos []ModelInfo) {
	if !c.globalOn(a) {
		// 逃生门兜底：账号不路由 global 上游 → 不探测（零上游调用）。
		return nil, nil
	}

	c.globalModels.Lock()
	if len(c.globalModels.names) > 0 && time.Since(c.globalModels.fetched) < globalModelsTTL {
		names, infos := c.globalModels.names, c.globalModels.infos
		c.globalModels.Unlock()
		return names, infos
	}
	if !c.globalModels.lastFail.IsZero() && time.Since(c.globalModels.lastFail) < globalModelsFailCooldown {
		// 负缓存冷却期内：避免反复打上游，直接按失败处理（无静态回落）。
		c.globalModels.Unlock()
		return nil, nil
	}
	c.globalModels.Unlock()

	names, infos, efforts, defaults, err := c.probeGlobalModels(a)
	if err != nil || len(names) == 0 {
		// 探测失败：负缓存 + 返回 nil（effort 桶不写，prepareBody 走 globalEffortMap 静态兜底）。
		c.globalModels.Lock()
		c.globalModels.lastFail = time.Now()
		c.globalModels.names = nil
		c.globalModels.infos = nil
		c.globalModels.Unlock()
		return nil, nil
	}
	// global 域 effort 能力：探测下发的 supportedEfforts/defaultEffort 权威写入 global 桶
	// （raw remote，不并入静态表——静态兜底在 prepareBody 的 globalEffortMap 与
	// /v1/models 的 EffortListing 里按需 fallback）。空探测不写（防清既有桶）。
	if len(efforts) > 0 || len(defaults) > 0 {
		c.storeEfforts("global", efforts, defaults)
	}

	// 成功：探测结果去重。names/infos 均落缓存；倍率等选号敏感字段只透出展示，
	// 不注入 costTier。
	seen := make(map[string]bool, len(names))
	merged := make([]string, 0, len(names))
	for _, id := range names {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		merged = append(merged, id)
	}
	// 已知可用名单补缺：上游目录不下发、但产品侧可正常调用（且多为限免）的
	// 国际版模型（deepseek-v4.1-flash 等）——探测结果为准，静态只补缺失项。
	// 补进来的条目只带 ID：窗口走 ContextWindowListingV4 四级查找链兜底，
	// 档位走 globalEffortFallback（deepseek-v4.1-flash 国际版仅 high）。
	// infos 为 nil（窄表形态）时保持 nil——调用方按 ID 名单输出裸条目。
	for _, id := range GlobalModelNames {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		merged = append(merged, id)
		if infos != nil {
			infos = append(infos, ModelInfo{ID: id})
		}
	}

	c.globalModels.Lock()
	c.globalModels.names = merged
	c.globalModels.infos = infos
	c.globalModels.fetched = time.Now()
	c.globalModels.lastFail = time.Time{}
	c.globalModels.Unlock()
	return merged, infos
}

// GlobalModelsSnapshot 只读返回 global 模型目录缓存（**不探测、不发起任何上游调用**）。
// ok=false 表示缓存未填充（无 global 账号 / 从未探测 / 负缓存期内）→ 调用方不设限。
// 供 chat 路由做「该域是否提供此模型」的判定（跨域回落的准入检查）。
func (c *Client) GlobalModelsSnapshot() ([]string, bool) {
	if c == nil {
		return nil, false
	}
	c.globalModels.Lock()
	defer c.globalModels.Unlock()
	if len(c.globalModels.names) == 0 {
		return nil, false
	}
	return append([]string(nil), c.globalModels.names...), true
}

// probeGlobalModels 发起一次 global 模型目录探测（v3-config-merge）：
// /v3/config **双 UA**（IDE + CLI，见 codeBuddyCLIUA）与企业端点家族（/v2 → /console
// 兜底，补缺）**并发**探测后并集合并。返回模型名列表（已合并、未再去重——去重在
// fetchGlobalModelsOnce）、全字段 ModelInfo（对象形态；窄表为 nil）及 effort
// 能力桶（supportedEfforts/defaultEffort，可为空）。合并口径：v3 条目为主
// （credits 等字段以 v3 为准），企业端点只补 v3 缺失的模型 id；去重 key =
// 模型 id，输出顺序稳定。两路全失败才返回错误（等价原「家族端点全非 2xx」
// 负缓存语义）；单路失败降级为另一路结果 + warn 日志，互不拖累。
//
// v3 侧为什么必须两路：该端点对不同 UA 下发的**模型集合不同**，两路各有独有
// 模型（IDE 路独有 o4-mini / enhance-1.0 / auto-chat；CLI 路独有 deepseek 系列 /
// gpt-6-astra / kimi-k2.8-preview），故并发两路取并集，而非换 UA 单路。
func (c *Client) probeGlobalModels(a *auth.Auth) (names []string, infos []ModelInfo, efforts map[string][]string, defaults map[string]string, err error) {
	type probeResult struct {
		names []string
		infos []ModelInfo
		err   error
	}
	// probeV3 单次 /v3/config 探测（UA 参数化）。该端点对不同 UA 下发**不同模型集合**：
	// IDE UA 与 CLI UA 各有独有模型（见 codeBuddyCLIUA 注释），故并发两路取并集。
	probeV3 := func(ua string) chan probeResult {
		ch := make(chan probeResult, 1)
		go func() {
			// chatBase 已按 realm 切 global base。
			byID, perr := c.fetchV3ConfigModelMap(a, ua)
			if perr != nil {
				ch <- probeResult{err: perr}
				return
			}
			ids := make([]string, 0, len(byID))
			outInfos := make([]ModelInfo, 0, len(byID))
			for _, mi := range byID {
				if nonChatModel(mi.ID, mi.MaxTokens, mi.Tags) {
					continue
				}
				ids = append(ids, mi.ID)
				outInfos = append(outInfos, mi)
			}
			sort.Strings(ids) // map 迭代序随机，排序保输出稳定
			ch <- probeResult{names: ids, infos: outInfos}
		}()
		return ch
	}
	v3IDECh := probeV3(codeBuddyIDEUA)
	v3CLICh := probeV3(codeBuddyCLIUA)
	enterpriseCh := make(chan probeResult, 1)
	go func() {
		// 企业端点家族：/v2 首选 → /console 兜底（既有探活序，零回归）。
		var lastErr error
		for _, path := range globalModelsProbePaths {
			names, infos, perr := c.globalModelsOnce(a, path)
			if perr != nil {
				lastErr = perr
				continue
			}
			enterpriseCh <- probeResult{names: names, infos: infos}
			return
		}
		enterpriseCh <- probeResult{err: lastErr}
	}()
	v3IDE := <-v3IDECh
	v3CLI := <-v3CLICh
	enterprise := <-enterpriseCh
	// v3 两路自合并：IDE 路字段权威（响应更大、单条字段更全），CLI 路只补缺失的模型 id。
	// 单路成功即用该路；两路全失败才带 err 进入下游降级判断。
	var v3 probeResult
	switch {
	case v3IDE.err != nil && v3CLI.err != nil:
		v3 = probeResult{err: v3IDE.err}
	case v3IDE.err != nil:
		log.Printf("WARN: [upstream] global models: v3/config IDE-UA probe failed (CLI-UA only): %v", v3IDE.err)
		v3 = v3CLI
	case v3CLI.err != nil:
		log.Printf("WARN: [upstream] global models: v3/config CLI-UA probe failed (IDE-UA only): %v", v3CLI.err)
		v3 = v3IDE
	default:
		vn, vi := mergeGlobalCatalog(v3IDE.names, v3IDE.infos, v3CLI.names, v3CLI.infos)
		v3 = probeResult{names: vn, infos: vi}
	}

	if v3.err != nil && enterprise.err != nil {
		// 两路全失败 → 负缓存语义（等价原家族端点全非 2xx）。
		return nil, nil, nil, nil, v3.err
	}
	if v3.err != nil {
		// /v3 失败降级：不拖累企业端点结果（降级仅企业端点 + warn）。
		log.Printf("WARN: [upstream] global models: v3/config probe failed (degraded to enterprise endpoint): %v", v3.err)
		names, infos, efforts, defaults = extractEfforts(enterprise.infos)
		return names, infos, efforts, defaults, nil
	}
	if enterprise.err != nil {
		log.Printf("WARN: [upstream] global models: enterprise endpoint failed (v3/config only): %v", enterprise.err)
		names, infos, efforts, defaults = extractEfforts(v3.infos)
		return names, infos, efforts, defaults, nil
	}
	// 两路皆成功：v3 为主、企业端点补缺合并（含 effort 桶合并，v3 权威）。
	v3Names, v3Infos, v3Efforts, v3Defaults := extractEfforts(v3.infos)
	if len(v3Names) == 0 {
		v3Names = v3.names
	}
	entNames, entInfos, entEfforts, entDefaults := extractEfforts(enterprise.infos)
	if len(entNames) == 0 {
		entNames = enterprise.names
	}
	names, infos = mergeGlobalCatalog(v3Names, v3Infos, entNames, entInfos)
	efforts = mergeEffortBuckets(v3Efforts, entEfforts)
	defaults = mergeEffortDefaults(v3Defaults, entDefaults)
	return names, infos, efforts, defaults, nil
}

// extractEfforts 从条目列表抽取 effort 能力桶（supportedEfforts 数组优先；
// 缺数组但 defaultEffort 单档非空也入 defaults 桶）并顺带返回有序 names。
func extractEfforts(infos []ModelInfo) (names []string, out []ModelInfo, efforts map[string][]string, defaults map[string]string) {
	names = make([]string, 0, len(infos))
	for _, mi := range infos {
		if mi.ID == "" {
			continue
		}
		names = append(names, mi.ID)
		out = append(out, mi)
		if len(mi.Efforts) > 0 {
			if efforts == nil {
				efforts = make(map[string][]string)
			}
			efforts[mi.ID] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			if defaults == nil {
				defaults = make(map[string]string)
			}
			defaults[mi.ID] = mi.DefaultEffort
		}
	}
	return names, out, efforts, defaults
}

// mergeGlobalCatalog 两路合并（v3 主、企业补缺）：names 按 id 去重（v3 原序在前、
// 企业端点补充项在其原序后追加——稳定输出）；infos 同步合并（v3 条目字段权威，
// 企业端点条目只在 id 缺失时进并集）。
// 窄表形态（infos nil）时保持 nil——无对象字段不编造。
func mergeGlobalCatalog(primaryNames []string, primaryInfos []ModelInfo, secondaryNames []string, secondaryInfos []ModelInfo) (names []string, infos []ModelInfo) {
	if len(secondaryNames) == 0 {
		return primaryNames, primaryInfos
	}
	seen := make(map[string]bool, len(primaryNames)+len(secondaryNames))
	out := make([]string, 0, len(primaryNames)+len(secondaryNames))
	for _, id := range primaryNames {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	var outInfos []ModelInfo
	if primaryInfos != nil {
		outInfos = make([]ModelInfo, 0, len(primaryInfos)+len(secondaryInfos))
		outInfos = append(outInfos, primaryInfos...)
	}
	for _, id := range secondaryNames {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		// 窄表企业响应（secondaryInfos nil / 超出条目数）时该 id 无对象字段，
		// infos 保持原样（调用方按 id 名单输出裸条目，不编造字段）。
		for _, mi := range secondaryInfos {
			if mi.ID == id {
				outInfos = append(outInfos, mi)
				break
			}
		}
	}
	return out, outInfos
}

// mergeEffortBuckets 合并两路 effort 桶：主路（v3）权威，企业端点只补主路缺失的模型档位。
func mergeEffortBuckets(primary, secondary map[string][]string) map[string][]string {
	if len(secondary) == 0 {
		return primary
	}
	out := primary
	if out == nil {
		out = make(map[string][]string, len(secondary))
	}
	for id, v := range secondary {
		if _, ok := out[id]; !ok {
			out[id] = v
		}
	}
	return out
}

// mergeEffortDefaults 合并两路 defaultEffort：主路（v3）权威，企业端点只补缺失。
func mergeEffortDefaults(primary, secondary map[string]string) map[string]string {
	if len(secondary) == 0 {
		return primary
	}
	out := primary
	if out == nil {
		out = make(map[string]string, len(secondary))
	}
	for id, v := range secondary {
		if _, ok := out[id]; !ok {
			out[id] = v
		}
	}
	return out
}

// globalModelsOnce 单端点探测。2xx + 解析出非空名单 → (names, infos, nil)；否则 (nil, nil, err)。
func (c *Client) globalModelsOnce(a *auth.Auth, path string) ([]string, []ModelInfo, error) {
	url := c.chatBase(a) + path // 按 realm 切 base：global 账号 → global base
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	c.CommonHeaders(req, a) // 共享请求头（Origin/Referer/UA），与 FetchModels 同款
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// 读失败 → 传输层错误：半截 body 不进解析（探测负缓存走 lastFail，不罚号）。
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("global models status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	names, infos, _, _, err := parseGlobalModelNames(raw)
	return names, infos, err
}

// globalModelAltEntry 国际站目录的宽松条目：兼容键名漂移（PR #21 实测形态）。
// 标准键（id/maxInputTokens/maxOutputTokens/reasoning.* 等）由内嵌 dynModelEntry
// 解析；别名键在此兜底：id → modelId → model → name；窗口 → contextWindow / maxTokens。
type globalModelAltEntry struct {
	dynModelEntry
	ModelID       string `json:"modelId"`
	Model         string `json:"model"`
	ContextWindow int64  `json:"contextWindow"`
	MaxTokensAlt  int64  `json:"maxTokens"`
}

// modelInfo 按条目构造 ModelInfo（id 与窗口的别名键兜底，其余字段口径与 CN 一致）。
func (m globalModelAltEntry) modelInfo() ModelInfo {
	mi := m.dynModelEntry.modelInfo()
	if mi.ID == "" {
		mi.ID = strings.TrimSpace(m.ModelID)
	}
	if mi.ID == "" {
		mi.ID = strings.TrimSpace(m.Model)
	}
	if mi.ID == "" {
		mi.ID = strings.TrimSpace(m.Name)
	}
	if mi.ContextWindow == 0 {
		mi.ContextWindow = m.ContextWindow
	}
	if mi.MaxTokens == 0 {
		mi.MaxTokens = m.MaxTokensAlt
	}
	return mi
}

// parseGlobalModelNames 解析国际站模型目录，产出模型名、全字段条目与 effort 能力桶
// （supportedEfforts/defaultEffort）。解析成功但名单为空 → 返回错误（等价"该端点没给全"）。
//
// envelope 容忍（PR #21 实测国际站曾切换下发位置）：payload 依次尝试
// data（直接数组 / data.models / data.items / data.list）→ 顶层 models / items / list /
// result；条目可以是字符串（裸 ID）或对象（id 依次回退 id → modelId → model → name，
// 窗口键兼容 contextWindow / maxTokens）；disabled 条目剔除。
// 首个解析出非空名单的候选即选中——只认单一形态会把登录成功的账号误判成"无模型"。
//
// 裸顶层数组（`[ {...}, ... ]` 无任何信封）也支持：Go 把数组反序列化进 struct 会直接
// 报错（cannot unmarshal array into Go value of type struct），故信封解析失败时回退把
// **整个 raw** 当作一个候选 payload 解析（吸收 upstream c3cc888 的该项修复）。
func parseGlobalModelNames(raw []byte) (names []string, infos []ModelInfo, efforts map[string][]string, defaults map[string]string, err error) {
	emit := func(entries []ModelInfo) ([]string, []ModelInfo, map[string][]string, map[string]string, error) {
		names = make([]string, 0, len(entries))
		infos = make([]ModelInfo, 0, len(entries))
		for _, mi := range entries {
			names = append(names, mi.ID)
			infos = append(infos, mi)
			if len(mi.Efforts) > 0 {
				if efforts == nil {
					efforts = make(map[string][]string)
				}
				efforts[mi.ID] = mi.Efforts
			}
			if mi.DefaultEffort != "" {
				if defaults == nil {
					defaults = make(map[string]string)
				}
				defaults[mi.ID] = mi.DefaultEffort
			}
		}
		return names, infos, efforts, defaults, nil
	}

	var env struct {
		Code   int             `json:"code"`
		Data   json.RawMessage `json:"data"`
		Models json.RawMessage `json:"models"`
		Items  json.RawMessage `json:"items"`
		List   json.RawMessage `json:"list"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		// 裸顶层数组：信封解析失败 → 整个 body 当作 payload 再试一次。
		if entries, ok := parseGlobalModelPayload(raw); ok && len(entries) > 0 {
			return emit(entries)
		}
		return nil, nil, nil, nil, fmt.Errorf("global models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, nil, nil, nil, fmt.Errorf("global models code=%d", env.Code)
	}
	for _, payload := range []json.RawMessage{env.Data, env.Models, env.Items, env.List, env.Result} {
		entries, ok := parseGlobalModelPayload(payload)
		if !ok || len(entries) == 0 {
			continue
		}
		return emit(entries)
	}
	return nil, nil, nil, nil, fmt.Errorf("global models empty list")
}

// parseGlobalModelPayload 解析一个候选 payload：字符串数组（窄表）、对象数组
// （元素为字符串或对象）、或容器对象（models / items / list / result，递归解析）。
// 解析出非空条目 → ok=true。
func parseGlobalModelPayload(raw json.RawMessage) (out []ModelInfo, ok bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, false
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, id := range arr {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, ModelInfo{ID: id})
			}
		}
		return out, len(out) > 0
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err == nil {
		for _, it := range items {
			if mi, okItem := parseGlobalModelItem(it); okItem {
				out = append(out, mi)
			}
		}
		return out, len(out) > 0
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	for _, key := range []string{"models", "items", "list", "result"} {
		if v, okKey := obj[key]; okKey {
			if sub, okSub := parseGlobalModelPayload(v); okSub {
				return sub, true
			}
		}
	}
	return nil, false
}

// parseGlobalModelItem 单个条目：字符串 → 裸 ID；对象 → globalModelAltEntry
// （disabled / 空 id 剔除）。
func parseGlobalModelItem(raw json.RawMessage) (ModelInfo, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		return ModelInfo{ID: s}, s != ""
	}
	var m globalModelAltEntry
	if err := json.Unmarshal(raw, &m); err != nil {
		return ModelInfo{}, false
	}
	if m.Disabled {
		return ModelInfo{}, false
	}
	mi := m.modelInfo()
	if mi.ID == "" {
		return ModelInfo{}, false
	}
	return mi, true
}
