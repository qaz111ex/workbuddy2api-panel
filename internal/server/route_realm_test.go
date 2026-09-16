package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func globalAuth(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
}

func cnAuth(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999}
}

// withCNCatalog 临时把 CN 动态目录缓存置为给定模型（模拟 /v1/models 已拉取过的
// 既有缓存形态）；测试结束恢复原值。
func withCNCatalog(t *testing.T, ids ...string) {
	t.Helper()
	dynamicModelsCache.Lock()
	oldIDs, oldFetched := dynamicModelsCache.ids, dynamicModelsCache.fetched
	infos := make([]upstream.ModelInfo, 0, len(ids))
	for _, id := range ids {
		infos = append(infos, upstream.ModelInfo{ID: id})
	}
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids, dynamicModelsCache.fetched = oldIDs, oldFetched
		dynamicModelsCache.Unlock()
	})
}

// newRealmFakeUpstream 按 chat 路径分流返回的假上游：global 走 /console/chat/completions，
// cn 走 /v2/chat/completions。fn 返回该路径下的响应（status/body/isStream）。
func newRealmFakeUpstream(t *testing.T, fn func(path string) (int, string, bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			status, body, isStream := fn(r.URL.Path)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:     "https://cn.example",
		ChatBaseGlobal: "https://global.example",
		GlobalEnabled:  true,
	}
}

// TestRealmOrderFor 跨域候选顺序：显式前缀钉首选域（回落开启时附另一域）、
// 裸名按目录、逃生门恒 cn。
func TestRealmOrderFor(t *testing.T) {
	up := upstream.New()
	h := NewHandler(Config{Pool: testPoolWith(globalAuth("g1"), cnAuth("c1")), Upstream: up, GlobalEnabled: true, RealmFallback: true})

	// 目录数据不足（未拉取）：显式前缀 = [首选, 另一域]，裸名 = [cn, global]。
	if got := h.realmOrderFor("global:deepseek-v4.1-flash", "deepseek-v4.1-flash"); strings.Join(got, ",") != "global,cn" {
		t.Errorf("pinned global order=%v want [global cn]", got)
	}
	if got := h.realmOrderFor("cn:glm-5.2", "glm-5.2"); strings.Join(got, ",") != "cn,global" {
		t.Errorf("pinned cn order=%v want [cn global]", got)
	}
	if got := h.realmOrderFor("deepseek-v4.1-flash", "deepseek-v4.1-flash"); strings.Join(got, ",") != "cn,global" {
		t.Errorf("bare order=%v want [cn global]", got)
	}

	// 回落关闭：显式前缀硬钉单域（现状语义），裸名也收敛为 cn 优先单域候选。
	hNo := NewHandler(Config{Pool: testPoolWith(globalAuth("g1"), cnAuth("c1")), Upstream: up, GlobalEnabled: true, RealmFallback: false})
	if got := hNo.realmOrderFor("global:glm-5.2", "glm-5.2"); strings.Join(got, ",") != "global" {
		t.Errorf("fallback off pinned order=%v want [global]", got)
	}

	// 逃生门（global.enabled=false）：恒 cn（auth.Realm() 同口径，global 号判为 cn）。
	hOff := NewHandler(Config{Pool: testPoolWith(globalAuth("g1"), cnAuth("c1")), Upstream: up, GlobalEnabled: false, RealmFallback: true})
	if got := hOff.realmOrderFor("global:glm-5.2", "glm-5.2"); strings.Join(got, ",") != "cn" {
		t.Errorf("escape hatch order=%v want [cn]", got)
	}
}

// TestRealmModelStateCatalog 目录三态判定：CN 目录命中/未命中/未知。
func TestRealmModelStateCatalog(t *testing.T) {
	up := upstream.New()
	h := NewHandler(Config{Pool: testPoolWith(cnAuth("c1")), Upstream: up, GlobalEnabled: true, RealmFallback: true})

	withCNCatalog(t, "glm-5.2")

	if st := h.realmModelState("cn", "glm-5.2"); st != 1 {
		t.Errorf("cn glm-5.2 state=%d want 1", st)
	}
	if st := h.realmModelState("cn", "deepseek-v4.1-flash"); st != 0 {
		t.Errorf("cn deepseek-v4.1-flash state=%d want 0", st)
	}
	if st := h.realmModelState("global", "glm-5.2"); st != -1 {
		t.Errorf("global unknown state=%d want -1", st)
	}
	// 裸名只在 global 目录有时：候选顺序收敛为 [global]（不先白撞 CN）。
	if got := h.realmOrderFor("deepseek-v4.1-flash", "deepseek-v4.1-flash"); strings.Join(got, ",") != "global" {
		t.Errorf("order=%v want [global] (global-only catalog)", got)
	}
}

// TestChooseRealmSkipsExhaustedAndBlocked 域选择：优先顺序 + 试过账号/整域封锁/配额跳过。
func TestChooseRealmSkipsExhaustedAndBlocked(t *testing.T) {
	p := testPoolWith(globalAuth("g1"), cnAuth("c1"))
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), GlobalEnabled: true, RealmFallback: true})
	order := []string{"cn", "global"}

	if got := h.chooseRealm(order, nil, nil, "m", map[string]bool{}); got != "cn" {
		t.Errorf("choose=%q want cn", got)
	}
	// cn 号已试过（本轮耗尽）→ 切 global。
	if got := h.chooseRealm(order, nil, nil, "m", map[string]bool{"c1": true}); got != "global" {
		t.Errorf("choose=%q want global", got)
	}
	// 整域封锁（11102 + 目录确认）→ 跳过 cn。
	if got := h.chooseRealm(order, map[string]bool{"cn": true}, nil, "m", map[string]bool{}); got != "global" {
		t.Errorf("choose=%q want global (cn blocked)", got)
	}
	if got := h.chooseRealm(order, map[string]bool{"cn": true, "global": true}, nil, "m", map[string]bool{}); got != "" {
		t.Errorf("choose=%q want empty (all blocked)", got)
	}
	// 配额跳过：首选域仍有候选，但显式 skip 后让给备选域。
	if got := h.chooseRealm(order, nil, map[string]bool{"cn": true}, "m", map[string]bool{}); got != "global" {
		t.Errorf("choose=%q want global (cn skipped by quota)", got)
	}
}

// TestChatPinnedGlobalFallsBackToCN 端到端：显式 global: 前缀的国际版账号被限流后，
// 同请求自动改用国内版账号继续服务（客户端无需改模型名）。
func TestChatPinnedGlobalFallsBackToCN(t *testing.T) {
	up := newRealmFakeUpstream(t, func(path string) (int, string, bool) {
		if path == "/console/chat/completions" {
			return 429, `{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(globalAuth("g1"), cnAuth("c1"))
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true, RealmFallback: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (pinned global must fall back to cn)", rec.Code, rec.Body)
	}
	if st, _ := p.Status("g1"); !st.Cooling {
		t.Fatalf("global account should be cooling after 429: %+v", st)
	}
}

// TestChatPinnedCNFallsBackToGlobal 端到端：显式 cn: 前缀的国内版账号失败后，
// 同请求自动改用国际版账号继续服务。
func TestChatPinnedCNFallsBackToGlobal(t *testing.T) {
	up := newRealmFakeUpstream(t, func(path string) (int, string, bool) {
		if path == "/v2/chat/completions" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(globalAuth("g1"), cnAuth("c1"))
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true, RealmFallback: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (pinned cn must fall back to global)", rec.Code, rec.Body)
	}
	if st, _ := p.Status("c1"); !st.Cooling {
		t.Fatalf("cn account should be cooling after 402: %+v", st)
	}
}

// TestChatRealmFallbackDisabledPinsRealm 回落关闭：显式前缀硬钉，首选域不可用时
// 不再借用另一域（保持严格域路由语义）。
func TestChatRealmFallbackDisabledPinsRealm(t *testing.T) {
	var usedGlobalPath bool
	up := newRealmFakeUpstream(t, func(path string) (int, string, bool) {
		if path == "/console/chat/completions" {
			usedGlobalPath = true
			return 429, `{"code":11140,"msg":"rate limit"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(globalAuth("g1"), cnAuth("c1"))
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true, RealmFallback: false})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	if rec.Code != 429 {
		t.Fatalf("code=%d body=%s want 429 (fallback disabled)", rec.Code, rec.Body)
	}
	if !usedGlobalPath {
		t.Fatal("expected the global upstream path to be used at least once")
	}
	if st, _ := p.Status("c1"); st.Cooling || st.Disabled {
		t.Fatalf("cn account must not be touched when fallback is disabled: %+v", st)
	}
}

// TestChatBareModelGlobalOnlyPoolRoutesGlobal 纯国际版池 + 裸模型名照常走 global
// （跨域顺序 [cn, global]，cn 无可用账号 → 直接 global）。
func TestChatBareModelGlobalOnlyPoolRoutesGlobal(t *testing.T) {
	var gotPath string
	up := newRealmFakeUpstream(t, func(path string) (int, string, bool) {
		gotPath = path
		return 200, sseOK, true
	})
	p := testPoolWith(globalAuth("g1"))
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true, RealmFallback: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (bare model on global-only pool must not 503)", rec.Code, rec.Body)
	}
	if gotPath != "/console/chat/completions" {
		t.Fatalf("upstream path=%q want /console/chat/completions (global route)", gotPath)
	}
}

// TestChooseRealmOrderPreference 域序偏好：候选顺序第一个可用域被选中（cn 优先）。
func TestChooseRealmOrderPreference(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(globalAuth("g1"), cnAuth("c1")), Upstream: upstream.New(), GlobalEnabled: true, RealmFallback: true})
	if got := h.chooseRealm([]string{"global", "cn"}, nil, nil, "m", map[string]bool{}); got != "global" {
		t.Errorf("chooseRealm=%q want global (order preference)", got)
	}
}

// TestChatAllCNDisabledUsesGlobal 场景一：池中同时有 cn/global 账号但国内版全部被禁用
// 时，裸名请求应正常落到国际版账号（禁用号不参与任何选号，跨域回落立即生效）。
// CN 目录缓存预热为「含该模型」——这是禁用前的真实状态，验证回落不依赖目录是否过期。
func TestChatAllCNDisabledUsesGlobal(t *testing.T) {
	withCNCatalog(t, "deepseek-v4.1-flash")
	var gotPath string
	up := newRealmFakeUpstream(t, func(path string) (int, string, bool) {
		gotPath = path
		return 200, sseOK, true
	})
	p := testPoolWith(globalAuth("g1"), cnAuth("c1"))
	p.Disable("c1", "manual disable (test)")
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true, RealmFallback: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (all cn disabled must use global)", rec.Code, rec.Body)
	}
	if gotPath != "/console/chat/completions" {
		t.Fatalf("upstream path=%q want /console/chat/completions (global route)", gotPath)
	}
}

// TestChatAllCNFailingReachesGlobal 场景二：国内版账号很多且全部报错时，裸名请求
// 仍要轮到国际版（首选域尝试配额 + 跨域回落）。5 个 cn 账号全部 5xx，global 一次成功。
func TestChatAllCNFailingReachesGlobal(t *testing.T) {
	var cnHits, globalHits int
	up := newRealmFakeUpstream(t, func(path string) (int, string, bool) {
		if path == "/console/chat/completions" {
			globalHits++
			return 200, sseOK, true
		}
		cnHits++
		return 500, `{"code":1,"msg":"internal error"}`, false
	})
	g := globalAuth("g1")
	cn := []*auth.Auth{cnAuth("c1"), cnAuth("c2"), cnAuth("c3"), cnAuth("c4"), cnAuth("c5")}
	p := testPoolWith(append([]*auth.Auth{g}, cn...)...)
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true, RealmFallback: true})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (all cn failing must reach global)", rec.Code, rec.Body)
	}
	if globalHits != 1 {
		t.Fatalf("globalHits=%d want 1 (fallback realm must be tried)", globalHits)
	}
	// 首选域尝试配额 = MaxRotate-1 = 2：第 3 次起让位给备选域（否则 5 个 cn 号会
	// 把 4 次预算耗尽，global 永远轮不到）。
	if cnHits != 2 {
		t.Fatalf("cnHits=%d want 2 (primary realm quota)", cnHits)
	}
}
