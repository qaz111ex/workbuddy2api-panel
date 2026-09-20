package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ─── /v1/stats 倍率透出（issue #176）──────────────────────────────────────
//
// 与 /v1/models 同源（模型目录只读缓存），缺失≠免费：目录未下发 / 缓存冷 /
// 无匹配条目 → JSON 整体省略 credits 键，绝不输出 "x0.00"。
//
// 本 fork 适配：sk 上游的 metrics_credits_test.go 依赖 3 个本仓库不存在的 helper
// （resetModelsCache / fullFieldsModelsBody / newGlobalModelsHandlerFake），此处按
// 本 fork 既有形态补齐（testPoolWith / newFakeUpstream 直接复用，模块路径已改为
// github.com/linguo2625469/workbuddy2api-panel）。

// resetModelsCache 清空 package 级 CN 动态模型缓存（fetchDynamicModels 是全包共享
// 状态：不复位会让「冷缓存」用例被先前测试的缓存污染，也可能反过来污染 /v1/models
// 断言）。与 handler_test.go 内联复位写法同口径，抽成 helper 供本文件复用。
func resetModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// fullFieldsModelsBody CN /console 动态目录全字段样本（hy3 → credits "x0.05"），
// 与 internal/upstream 的全字段 fixture 同源口径。
const fullFieldsModelsBody = `{"code":0,"data":{"models":[
	{"id":"hy3","name":"Hy3","descriptionZh":"混元思考模型，具有增强的推理能力","credits":"x0.05","tags":["craft"],"vendor":"j","maxInputTokens":192000,"maxOutputTokens":64000,"supportsReasoning":true,"reasoning":{"defaultEffort":"high","supportedEfforts":["low","high"]}}
],"agents":[{"name":"cli","models":["hy3"]}]}}`

// globalModelsHandlerFake 模型目录探测 fake：CN / global 走同一 httptest 服务
// （按 Authorization 头分流），可控响应体。只服务目录端点（本文件不触发 chat）。
type globalModelsHandlerFake struct {
	up *upstream.Client

	mu   sync.Mutex
	cnt  int    // global 探测请求计数
	path string // 最近一次探测路径
	auth string // 最近一次探测鉴权头

	status int
	body   string
}

// newGlobalModelsHandlerFake 起一个 httptest 服务并接线 CN/global 两个 base。
// CN 请求（Bearer at_cn）返回裸 ID 表；其余（global 探测）返回 status/body。
func newGlobalModelsHandlerFake(t *testing.T, status int, body string) *globalModelsHandlerFake {
	t.Helper()
	cf := &globalModelsHandlerFake{status: status, body: body}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isCN := r.Header.Get("Authorization") == "Bearer at_cn"
		cf.mu.Lock()
		if !isCN {
			cf.cnt++
			cf.path = r.URL.Path
			cf.auth = r.Header.Get("Authorization")
		}
		cf.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if isCN {
			// CN 动态拉取：返回含 cli agent 的目录，让 CN 面在纯动态下有产出。
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"code":0,"data":{"models":[{"id":"cn-dyn-model","maxInputTokens":65536,"maxOutputTokens":8192}],"agents":[{"name":"cli","models":["cn-dyn-model"]}]}}`)
			return
		}
		w.WriteHeader(cf.status)
		_, _ = io.WriteString(w, cf.body)
	}))
	t.Cleanup(ts.Close)
	base := strings.TrimSuffix(ts.URL, "/")
	cf.up = &upstream.Client{
		HTTP:           &http.Client{},
		ChatBaseCN:     base,
		ChatBaseGlobal: base,
		GlobalEnabled:  true,
	}
	return cf
}

func (cf *globalModelsHandlerFake) snapshot() (cnt int, path, authz string) {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	return cf.cnt, cf.path, cf.auth
}

// warmCNCatalog 用 fullFieldsModelsBody（hy3 → "x0.05"）预热 CN 目录缓存：
// 一次 modelList 即拉取并落缓存（fetchDynamicModels 内部触发）。
func warmCNCatalog(t *testing.T, h *Handler) {
	t.Helper()
	got := h.modelList()
	found := false
	for _, m := range got {
		if id, ok := m["id"].(string); ok && id == "cn:hy3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("预热失败：modelList 无 cn:hy3：%v", got)
	}
}

// findModelRow 从快照里取指定模型的载荷行。
func findModelRow(t *testing.T, snap MetricsSnapshot, model string) ModelStatPayload {
	t.Helper()
	for _, m := range snap.Models {
		if m.Model == model {
			return m
		}
	}
	t.Fatalf("快照缺模型 %q：%+v", model, snap.Models)
	return ModelStatPayload{}
}

// TestStatsCreditsEnrichedFromCNCatalog CN 目录命中：cn:hy3 行透出倍率原文
// "x0.05"；目录外模型 / total 行保持空。
func TestStatsCreditsEnrichedFromCNCatalog(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	resetMetricsForTest(t)

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, fullFieldsModelsBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})
	warmCNCatalog(t, h)

	recordChatMetric(&chatStat{model: "cn:hy3", mode: "sync", status: 200}, time.Second)
	recordChatMetric(&chatStat{model: "cn:not-in-catalog", mode: "sync", status: 200}, time.Second)

	snap := MetricsSnapshotOf()
	h.enrichCredits(&snap)

	if got := findModelRow(t, snap, "cn:hy3").Credits; got != "x0.05" {
		t.Errorf("cn:hy3 credits = %q, want x0.05（目录命中，原文透出）", got)
	}
	if got := findModelRow(t, snap, "cn:not-in-catalog").Credits; got != "" {
		t.Errorf("cn:not-in-catalog credits = %q, want 空串（目录外模型省略）", got)
	}
	if snap.Total.Credits != "" {
		t.Errorf("total credits = %q, want 空串（跨倍率聚合无意义）", snap.Total.Credits)
	}
}

// TestStatsCreditsOmittedWhenCacheCold 目录缓存冷：credits 键整体不出现在
// JSON 里（缺失≠免费，不是 "x0.00" 也不是 ""），且零上游调用。
func TestStatsCreditsOmittedWhenCacheCold(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	resetMetricsForTest(t)

	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, fullFieldsModelsBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	recordChatMetric(&chatStat{model: "cn:hy3", mode: "sync", status: 200}, time.Second)

	snap := MetricsSnapshotOf()
	h.enrichCredits(&snap)

	raw, err := json.Marshal(snap.Models[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"credits"`) {
		t.Errorf("冷缓存下 JSON 应整体省略 credits 键（缺失≠免费），得到 %s", raw)
	}
	if calls != 0 {
		t.Errorf("enrichCredits 发起 %d 次上游调用，want 0（只读快照，绝不探测）", calls)
	}
}

// TestStatsCreditsGlobalRealm global realm：global: 前缀键查 global 目录
// （v3/config + /v2 并发探测对象形态）→ 倍率命中。
func TestStatsCreditsGlobalRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	resetMetricsForTest(t)

	cf := newGlobalModelsHandlerFake(t, 200, fullFieldsModelsBody)
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})
	// 预热 global 目录：一次 modelList 触发探测并落 Client 缓存。
	got := h.modelList()
	found := false
	for _, m := range got {
		if id, ok := m["id"].(string); ok && id == "global:hy3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("预热失败：modelList 无 global:hy3：%v", got)
	}

	recordChatMetric(&chatStat{model: "global:hy3", mode: "sync", status: 200}, time.Second)

	snap := MetricsSnapshotOf()
	h.enrichCredits(&snap)

	if got := findModelRow(t, snap, "global:hy3").Credits; got != "x0.05" {
		t.Errorf("global:hy3 credits = %q, want x0.05（global 目录命中）", got)
	}
	// 只读快照不得再打上游：enrichCredits 前后探测计数不变。
	cntBefore, _, _ := cf.snapshot()
	_ = MetricsSnapshotOf()
	h.enrichCredits(&snap)
	if cntAfter, _, _ := cf.snapshot(); cntAfter != cntBefore {
		t.Errorf("enrichCredits 新增 %d 次 global 探测，want 0（只读快照）", cntAfter-cntBefore)
	}
}

// TestStatsCreditsKeyNormalization 键归一：裸名（→cn realm）与 cn: 前缀同获倍率；
// "-" 与未知前缀查不到 → 空串。
func TestStatsCreditsKeyNormalization(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	resetMetricsForTest(t)

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, fullFieldsModelsBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})
	warmCNCatalog(t, h)

	for _, key := range []string{"hy3", "cn:hy3", "-", "weird:hy3"} {
		recordChatMetric(&chatStat{model: key, mode: "sync", status: 200}, time.Second)
	}

	snap := MetricsSnapshotOf()
	h.enrichCredits(&snap)

	if got := findModelRow(t, snap, "hy3").Credits; got != "x0.05" {
		t.Errorf("裸名 hy3 credits = %q, want x0.05（裸名 → cn realm）", got)
	}
	if got := findModelRow(t, snap, "cn:hy3").Credits; got != "x0.05" {
		t.Errorf("cn:hy3 credits = %q, want x0.05", got)
	}
	if got := findModelRow(t, snap, "-").Credits; got != "" {
		t.Errorf("\"-\" credits = %q, want 空串（不查目录）", got)
	}
	if got := findModelRow(t, snap, "weird:hy3").Credits; got != "" {
		t.Errorf("weird:hy3 credits = %q, want 空串（未知前缀查不到）", got)
	}
}

// TestStatsEndpointRegistered 端点接线：GET /v1/stats 返回按模型聚合载荷、
// POST /v1/stats/reset 清空累计（withAuth 在空 api_key 下放行）。
func TestStatsEndpointRegistered(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	resetMetricsForTest(t)

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, fullFieldsModelsBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	recordChatMetric(&chatStat{model: "cn:hy3", mode: "stream", status: 200, toks: 3, hasUsage: true, prompt: 2}, time.Second)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/stats code=%d body=%s", rec.Code, rec.Body)
	}
	var snap MetricsSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	if !snap.Enabled || snap.Total.Requests != 1 || len(snap.Models) != 1 {
		t.Fatalf("stats 载荷异常：enabled=%v requests=%d models=%d", snap.Enabled, snap.Total.Requests, len(snap.Models))
	}
	if snap.Models[0].Model != "cn:hy3" || snap.Models[0].TotalTokens != 5 {
		t.Errorf("stats 模型行 = %+v want cn:hy3 tokens=5", snap.Models[0])
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/stats/reset", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/stats/reset code=%d body=%s", rec.Code, rec.Body)
	}
	if got := MetricsSnapshotOf(); got.Total.Requests != 0 || len(got.Models) != 0 {
		t.Errorf("reset 后 requests=%d models=%d want 0/0", got.Total.Requests, len(got.Models))
	}
}

// TestStatsEndpointAuthRejects 有 api_key 时缺 Bearer 必须 401（与其余 /v1 端点同闸）。
func TestStatsEndpointAuthRejects(t *testing.T) {
	resetMetricsForTest(t)

	h := NewHandler(Config{Pool: testPoolWith(), Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, fullElementsBodyStub, false
	}), APIKey: "sk-test"})

	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/stats"},
		{"POST", "/v1/stats/reset"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s code=%d want 401", tc.method, tc.path, rec.Code)
		}
	}
}

// fullElementsBodyStub 仅供鉴权用例占位的目录响应（不应被调用）。
const fullElementsBodyStub = `{"code":0,"data":{"models":[],"agents":[]}}`
