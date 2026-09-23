package upstream

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestFetchGlobalModelsOverlaysKnownNames 上游目录不下发、但国际版实际可调用的
// 已知模型（deepseek-v4.1-flash，限免）必须补进名单——否则客户端在 /v1/models
// 里看不到它，只能盲猜模型名。探测条目与元数据不受补缺影响。
func TestFetchGlobalModelsOverlaysKnownNames(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"gpt-5.6-luna","name":"GPT-5.6-Luna","maxInputTokens":1000000,"maxOutputTokens":128000,"supportsReasoning":true}
			]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v2/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"gpt-5.6-luna","name":"GPT-5.6-Luna","maxInputTokens":1000000,"maxOutputTokens":128000}
			]}}`), nil
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			return jsonResp(404, `{}`), nil
		}
	})
	c.GlobalEnabled = true // 测试 fixture 缺省关闭 global 路由，本用例显式打开
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}

	names := c.FetchGlobalModels(a)
	if !containsStr(names, "deepseek-v4.1-flash") {
		t.Fatalf("global names missing deepseek-v4.1-flash: %v", names)
	}
	if !containsStr(names, "gpt-5.6-luna") {
		t.Fatalf("global names missing probed model gpt-5.6-luna: %v", names)
	}

	infos := c.FetchGlobalModelInfos(a)
	var luna, flash *ModelInfo
	for i := range infos {
		switch infos[i].ID {
		case "gpt-5.6-luna":
			luna = &infos[i]
		case "deepseek-v4.1-flash":
			flash = &infos[i]
		}
	}
	if luna == nil || luna.ContextWindow != 1000000 || luna.MaxTokens != 128000 {
		t.Fatalf("probe metadata lost for gpt-5.6-luna: %+v", infos)
	}
	if flash == nil {
		t.Fatalf("infos missing deepseek-v4.1-flash: %+v", infos)
	}
}

// TestFetchGlobalModelsNoStaticFallbackOnProbeFailure 探测全失败仍是「无名单」
// （不回落静态名单）：补缺只发生在探测成功之后（域不可用时不编造可用性）。
func TestFetchGlobalModelsNoStaticFallbackOnProbeFailure(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(500, `<html><head><title>500 Internal Server Error</title></head></html>`), nil
	})
	c.GlobalEnabled = true
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	if names := c.FetchGlobalModels(a); len(names) != 0 {
		t.Fatalf("probe failure must not fall back to static names: %v", names)
	}
	if infos := c.FetchGlobalModelInfos(a); len(infos) != 0 {
		t.Fatalf("probe failure must not fall back to static infos: %+v", infos)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestParseGlobalModelEnvelopeVariants 目录 envelope/键名漂移容忍（PR #21 实测形态）：
// data.models 标准形态之外的 items/modelId/model/contextWindow/maxTokens、顶层
// models、data 直接数组等都必须能解析——只认单一形态会把登录成功的账号误判成「无模型」。
func TestParseGlobalModelEnvelopeVariants(t *testing.T) {
	cases := []struct {
		name             string
		raw              string
		wantID           string
		wantCtx, wantOut int64
	}{
		{"data models 标准形态", `{"code":0,"data":{"models":[{"id":"m-standard","maxInputTokens":1000000,"maxOutputTokens":393216}]}}`, "m-standard", 1000000, 393216},
		{"data items + modelId", `{"code":0,"data":{"items":[{"modelId":"m-items","contextWindow":131072}]}}`, "m-items", 131072, 0},
		{"顶层 models + model 键", `{"models":[{"model":"m-top","maxTokens":4096}]}`, "m-top", 0, 4096},
		{"data 字符串数组（窄表）", `{"code":0,"data":["m-narrow"]}`, "m-narrow", 0, 0},
		{"data 对象数组", `{"code":0,"data":[{"id":"m-objarr"}]}`, "m-objarr", 0, 0},
		{"data.list 容器 + name 兜底", `{"code":0,"data":{"list":[{"name":"m-byname"}]}}`, "m-byname", 0, 0},
		// 裸顶层数组（无任何信封）：Go 把数组反序列化进 struct 会直接报错，故必须
		// 有「信封解析失败 → 整个 raw 当 payload」的回退（吸收 upstream c3cc888）。
		{"裸顶层对象数组", `[{"id":"m-bare-obj","maxInputTokens":65536}]`, "m-bare-obj", 65536, 0},
		{"裸顶层字符串数组", `["m-bare-str"]`, "m-bare-str", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			names, infos, _, _, err := parseGlobalModelNames([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parseGlobalModelNames() err = %v", err)
			}
			if len(names) != 1 || names[0] != tc.wantID {
				t.Fatalf("names = %v, want [%s]", names, tc.wantID)
			}
			if len(infos) != 1 || infos[0].ContextWindow != tc.wantCtx || infos[0].MaxTokens != tc.wantOut {
				t.Fatalf("infos = %+v, want ctx=%d out=%d", infos, tc.wantCtx, tc.wantOut)
			}
		})
	}
}

// TestParseGlobalModelNamesRejectsAndFilters 非零业务码拒绝、disabled 条目剔除。
func TestParseGlobalModelNamesRejectsAndFilters(t *testing.T) {
	if _, _, _, _, err := parseGlobalModelNames([]byte(`{"code":14017,"data":{"models":[]}}`)); err == nil {
		t.Fatal("non-zero code must fail")
	}
	names, _, _, _, err := parseGlobalModelNames([]byte(`{"code":0,"data":{"models":[
		{"id":"keep"},{"id":"drop","disabled":true}
	]}}`))
	if err != nil || len(names) != 1 || names[0] != "keep" {
		t.Fatalf("names=%v err=%v want [keep]", names, err)
	}
}

// TestParseGlobalModelNamesKeepsReasoning 思考档位与窗口元数据不能被兼容解析丢掉；
// 老模型只回 reasoning.effort 键时也要兜住默认档。
func TestParseGlobalModelNamesKeepsReasoning(t *testing.T) {
	raw := `{"code":0,"data":{"models":[{
		"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash",
		"maxInputTokens":1000000,"maxOutputTokens":393216,"maxAllowedSize":1000000,
		"supportsReasoning":true,"supportsImages":true,
		"reasoning":{"defaultEffort":"high","canDisableThinking":true,"supportedEfforts":["low","high","max"]}
	}]}}`
	_, infos, efforts, defaults, err := parseGlobalModelNames([]byte(raw))
	if err != nil || len(infos) != 1 {
		t.Fatalf("infos=%v err=%v", infos, err)
	}
	mi := infos[0]
	if mi.ContextWindow != 1000000 || mi.MaxTokens != 393216 || mi.MaxAllowedSize != 1000000 {
		t.Errorf("窗口元数据丢失: %+v", mi)
	}
	if mi.DefaultEffort != "high" || !mi.CanDisableThinking || !mi.SupportsReasoning || !mi.SupportsImages {
		t.Errorf("能力字段丢失: %+v", mi)
	}
	if len(efforts[mi.ID]) != 3 || defaults[mi.ID] != "high" {
		t.Errorf("effort 桶 = %v / %v", efforts, defaults)
	}

	// 老键 effort：DefaultEffort 与 defaults 桶都要兜住。
	_, infos2, _, defaults2, err := parseGlobalModelNames([]byte(`{"code":0,"data":{"models":[{"id":"legacy","reasoning":{"effort":"max"}}]}}`))
	if err != nil || len(infos2) != 1 || infos2[0].DefaultEffort != "max" || defaults2["legacy"] != "max" {
		t.Fatalf("legacy effort: infos=%v defaults=%v err=%v", infos2, defaults2, err)
	}
}

// snapshotTestClient 返回一个 global 探测可用的 Client：探测各路（/v3/config 的
// IDE + CLI 双 UA、/v2 企业路）都指向同一个 fake，并统计请求次数（只读快照必须零新请求）。
// 本 fork 的 global 探测是 v3 双 UA + 企业家族并发，三路都会打同一个 host。
func snapshotTestClient(t *testing.T, body string) (*Client, *int) {
	t.Helper()
	inner := rtFunc(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, body), nil
	})
	calls := 0
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return inner.RoundTrip(r)
		})},
		ChatBaseCN:     "https://chat.example",
		BillingBaseCN:  "https://billing.example",
		ChatBaseGlobal: "https://global.example",
		GlobalEnabled:  true,
	}
	return c, &calls
}

// TestGlobalModelInfosSnapshotReadOnly 只读快照的三个契约（issue #176 T1）：
//   - (a) 冷客户端（从未探测）→ 快照 nil，零上游请求（绝不主动探测）；
//   - (b) 一次 FetchGlobalModelInfos 预热（v3 双 UA + /v2 企业路并发三请求）→ 快照返回
//     同一批全字段 infos（含 credits 原文）；
//   - (c) 预热后反复读快照 → fake 请求计数不变（只读，与 Fetch* 的 miss 即探测
//     语义相反——本方法服务 /v1/stats，缓存冷就冷，不发起网络）。
//
// 本 fork 适配：fetchGlobalModelsOnce 有「静态名单补缺」——成功探测后 infos 会
// 追加 GlobalModelNames 的补缺条目（只带 ID）。故快照长度不是探测条目数，断言
// 改为「hy3 在其中且带倍率原文」，而非 sk 上游的 len==1。
func TestGlobalModelInfosSnapshotReadOnly(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	c, calls := snapshotTestClient(t, `{"code":0,"data":{"models":[{"id":"hy3","name":"Hy3","credits":"x0.05"}]}}`)
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}

	// (a) 冷：快照 nil，零上游请求。
	if got := c.GlobalModelInfosSnapshot(); got != nil {
		t.Fatalf("cold snapshot = %+v, want nil", got)
	}
	if *calls != 0 {
		t.Fatalf("cold snapshot made %d upstream calls, want 0 (must not probe)", *calls)
	}

	// (b) 预热：v3/config + /v2 企业路各一次 → 快照返回同批 infos。
	infos := c.FetchGlobalModelInfos(a)
	hy3 := findInfo(infos, "hy3")
	if hy3 == nil || hy3.Credits != "x0.05" {
		t.Fatalf("warm FetchGlobalModelInfos = %+v, want hy3 with credits x0.05", infos)
	}
	if len(infos) < 2 {
		t.Fatalf("infos=%d want ≥2（本 fork 会补 GlobalModelNames 缺失项）", len(infos))
	}
	snap := c.GlobalModelInfosSnapshot()
	snapHy3 := findInfo(snap, "hy3")
	if snapHy3 == nil || snapHy3.Credits != "x0.05" {
		t.Fatalf("snapshot = %+v, want same infos as fetch", snap)
	}
	if len(snap) != len(infos) {
		t.Errorf("snapshot len=%d want %d（与 Fetch 同一批）", len(snap), len(infos))
	}
	warmCalls := *calls

	// (c) 只读：再读 N 次零新请求。
	for i := 0; i < 5; i++ {
		got := c.GlobalModelInfosSnapshot()
		if findInfo(got, "hy3") == nil {
			t.Fatalf("snapshot read #%d = %+v, want cached infos", i, got)
		}
	}
	if *calls != warmCalls {
		t.Errorf("snapshot reads made %d new upstream calls, want 0 (read-only)", *calls-warmCalls)
	}
}

// TestGlobalModelInfosSnapshotExpired 过期快照 → nil（只读口不探测，也不返回陈旧值）。
func TestGlobalModelInfosSnapshotExpired(t *testing.T) {
	c, calls := snapshotTestClient(t, `{"code":0,"data":{"models":[{"id":"hy3","credits":"x0.05"}]}}`)
	c.globalModels.Lock()
	c.globalModels.infos = []ModelInfo{{ID: "hy3", Credits: "x0.05"}}
	c.globalModels.fetched = time.Now().Add(-2 * globalModelsTTL)
	c.globalModels.Unlock()

	if got := c.GlobalModelInfosSnapshot(); got != nil {
		t.Fatalf("expired snapshot = %+v, want nil", got)
	}
	if *calls != 0 {
		t.Errorf("expired snapshot made %d upstream calls, want 0", *calls)
	}
}

// TestGlobalModelInfosSnapshotNarrowTableNil 窄表形态（infos 为 nil，names 有值）
// → 快照 nil（无对象字段不编造）。
func TestGlobalModelInfosSnapshotNarrowTableNil(t *testing.T) {
	c, _ := snapshotTestClient(t, `{"code":0,"data":["hy3"]}`)
	c.globalModels.Lock()
	c.globalModels.names = []string{"hy3"}
	c.globalModels.infos = nil
	c.globalModels.fetched = time.Now()
	c.globalModels.Unlock()

	if got := c.GlobalModelInfosSnapshot(); got != nil {
		t.Fatalf("narrow-table snapshot = %+v, want nil", got)
	}
}

// TestGlobalModelInfosSnapshotNilClient nil Client 不得 panic（防御：调用方可能
// 在 cfg.Upstream 为 nil 时误调）。
func TestGlobalModelInfosSnapshotNilClient(t *testing.T) {
	var c *Client
	if got := c.GlobalModelInfosSnapshot(); got != nil {
		t.Fatalf("nil client snapshot = %+v, want nil", got)
	}
}

// findInfo 按 id 取条目；不存在返回 nil。
func findInfo(infos []ModelInfo, id string) *ModelInfo {
	for i := range infos {
		if infos[i].ID == id {
			return &infos[i]
		}
	}
	return nil
}

// --- /v3/config 双 UA 并发探测（upstream 9dce68a 手工适配）---

// globalProbeAuth 探测用的 global 账号：domain 后缀 .workbuddy.ai → Realm()==global。
func globalProbeAuth() *auth.Auth {
	return &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
}

// dualUAProbeClient 构造 global 探测用 Client：/v3/config 按 UA 分派 body，
// 企业端点固定 500（隔离出 v3 双路行为，使断言不受企业路影响）。
// 返回客户端与两路 v3 调用计数（探测是三路并发，计数必须原子）。
func dualUAProbeClient(t *testing.T, ideBody, cliBody string) (*Client, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var ideCalls, cliCalls atomic.Int32
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/v3/config") {
				switch r.Header.Get("User-Agent") {
				case codeBuddyIDEUA:
					ideCalls.Add(1)
					return jsonResp(200, ideBody), nil
				case codeBuddyCLIUA:
					cliCalls.Add(1)
					return jsonResp(200, cliBody), nil
				}
				t.Errorf("v3/config UA = %q, want IDE 或 CLI UA", r.Header.Get("User-Agent"))
				return jsonResp(200, `{"code":0,"data":{"models":[{"id":"unexpected-ua-model"}]}}`), nil
			}
			return jsonResp(500, `enterprise down`), nil
		})},
		ChatBaseCN:     "https://chat.example",
		ChatBaseGlobal: "https://global.example",
		GlobalEnabled:  true,
	}
	return c, &ideCalls, &cliCalls
}

// TestProbeGlobalModelsDualUAUnion 双 UA 并集（9dce68a 的核心价值）：/v3/config
// 对 IDE/CLI UA 下发的**模型集合不同**，两路各有独有模型，必须并发两路取并集
// （单纯换 UA 会丢掉另一路的独有模型）。同时验证共享 id 的字段以 IDE 路为权威
// （IDE 路单条字段更全），CLI 独有 id 的对象元数据也要一并带回。
func TestProbeGlobalModelsDualUAUnion(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	c, ideCalls, cliCalls := dualUAProbeClient(t,
		`{"code":0,"data":{"models":[
			{"id":"o4-mini","maxInputTokens":200000,"maxOutputTokens":100000},
			{"id":"shared-model","maxInputTokens":1000000,"maxOutputTokens":393216}
		]}}`,
		`{"code":0,"data":{"models":[
			{"id":"deepseek-v4.1-flash-sg","maxInputTokens":1000000,"maxOutputTokens":393216},
			{"id":"kimi-k2.8-preview","maxInputTokens":256000,"maxOutputTokens":64000},
			{"id":"shared-model","maxInputTokens":1000000,"maxOutputTokens":128000}
		]}}`)

	names, infos, _, _, err := c.probeGlobalModels(globalProbeAuth())
	if err != nil {
		t.Fatalf("probeGlobalModels() err = %v", err)
	}
	// 并集必须同时含 IDE 独有（o4-mini）与 CLI 独有（deepseek-v4.1-flash-sg /
	// kimi-k2.8-preview）——任一路独有模型丢失即回归到单路缺陷。
	for _, want := range []string{"o4-mini", "deepseek-v4.1-flash-sg", "kimi-k2.8-preview", "shared-model"} {
		if !containsStr(names, want) {
			t.Errorf("并集缺少 %s: %v", want, names)
		}
	}
	if got := ideCalls.Load(); got != 1 {
		t.Errorf("IDE UA 路探测 %d 次, want 1", got)
	}
	if got := cliCalls.Load(); got != 1 {
		t.Errorf("CLI UA 路探测 %d 次, want 1", got)
	}
	// 共享 id：字段以 IDE 路权威（CLI 路的精简值 128000 不得覆盖 IDE 的 393216）。
	if mi := findInfo(infos, "shared-model"); mi == nil || mi.MaxTokens != 393216 {
		t.Errorf("shared-model 应以 IDE 路字段为准（MaxTokens=393216）: %+v", mi)
	}
	// CLI 独有 id：补 id 的同时必须补对象元数据（不是只补名单）。
	if mi := findInfo(infos, "kimi-k2.8-preview"); mi == nil || mi.ContextWindow != 256000 || mi.MaxTokens != 64000 {
		t.Errorf("CLI 独有模型元数据丢失: %+v", mi)
	}
}

// TestProbeGlobalModelsSingleUADegradation 单路失败降级：一路非 2xx 时不得
// 拖累另一路（结果 = 成功一路），但**两路都必须被尝试过**——失败一路若不发
// 请求，就永远无从知道它有没有独有模型。
func TestProbeGlobalModelsSingleUADegradation(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	const (
		ideBody = `{"code":0,"data":{"models":[{"id":"o4-mini","maxInputTokens":200000,"maxOutputTokens":100000}]}}`
		cliBody = `{"code":0,"data":{"models":[{"id":"deepseek-v4.1-flash-sg","maxInputTokens":1000000,"maxOutputTokens":393216}]}}`
	)
	cases := []struct {
		name       string
		ideStatus  int
		ideBody    string
		cliStatus  int
		cliBody    string
		wantID     string
		wantAbsent string
	}{
		{"IDE 路失败 → 降级 CLI 路", 500, "", 200, cliBody, "deepseek-v4.1-flash-sg", "o4-mini"},
		{"CLI 路失败 → 降级 IDE 路", 200, ideBody, 500, "", "o4-mini", "deepseek-v4.1-flash-sg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ideCalls, cliCalls atomic.Int32
			c := &Client{
				HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
					if !strings.HasSuffix(r.URL.Path, "/v3/config") {
						return jsonResp(500, `enterprise down`), nil
					}
					switch r.Header.Get("User-Agent") {
					case codeBuddyIDEUA:
						ideCalls.Add(1)
						return jsonResp(tc.ideStatus, tc.ideBody), nil
					case codeBuddyCLIUA:
						cliCalls.Add(1)
						return jsonResp(tc.cliStatus, tc.cliBody), nil
					}
					t.Errorf("v3/config UA = %q, want IDE 或 CLI UA", r.Header.Get("User-Agent"))
					return jsonResp(404, `{}`), nil
				})},
				ChatBaseCN:     "https://chat.example",
				ChatBaseGlobal: "https://global.example",
				GlobalEnabled:  true,
			}
			names, _, _, _, err := c.probeGlobalModels(globalProbeAuth())
			if err != nil {
				t.Fatalf("单路成功不应报错: %v", err)
			}
			if !containsStr(names, tc.wantID) {
				t.Errorf("降级结果缺少 %s: %v", tc.wantID, names)
			}
			if containsStr(names, tc.wantAbsent) {
				t.Errorf("失败一路的模型不应出现: %v", names)
			}
			if ideCalls.Load() != 1 || cliCalls.Load() != 1 {
				t.Errorf("两路都应各探测 1 次: ide=%d cli=%d", ideCalls.Load(), cliCalls.Load())
			}
		})
	}
}

// TestFetchGlobalModelsBothUAFailNegativeCache 两路 v3 全失败 + 企业路失败 →
// 走原有负缓存语义（无名单、5min 冷却、冷却期内零上游调用）。这条同时钉住
// 「两路 v3 都要发请求」：只试一路会把另一路的独有模型永久漏掉。
func TestFetchGlobalModelsBothUAFailNegativeCache(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var v3Calls atomic.Int32
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/v3/config") {
				v3Calls.Add(1)
			}
			return jsonResp(500, `upstream down`), nil
		})},
		ChatBaseCN:     "https://chat.example",
		ChatBaseGlobal: "https://global.example",
		GlobalEnabled:  true,
	}
	if names := c.FetchGlobalModels(globalProbeAuth()); len(names) != 0 {
		t.Fatalf("全失败必须返回无名单（不回落静态）: %v", names)
	}
	if got := v3Calls.Load(); got != 2 {
		t.Errorf("/v3/config 探测 %d 次, want 2（IDE + CLI 两路）", got)
	}
	c.globalModels.Lock()
	failed := !c.globalModels.lastFail.IsZero()
	c.globalModels.Unlock()
	if !failed {
		t.Error("全失败必须落负缓存（lastFail）")
	}
	// 负缓存命中：冷却期内再调不得打上游。
	if names := c.FetchGlobalModels(globalProbeAuth()); len(names) != 0 {
		t.Fatalf("负缓存期内应返回 nil: %v", names)
	}
	if got := v3Calls.Load(); got != 2 {
		t.Errorf("负缓存期内又打了上游: %d 次", got)
	}
}
