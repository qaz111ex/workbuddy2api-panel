// panel_key_wiring_test.go 端到端接线闸门：管理面（/panel/*）与数据面（/v1/*）的
// 密钥必须**严格分离**（task-9）。
//
// 为什么放在 cmd/server：这里的断言走真实装配链——`server.NewHandler` + 真实
// `/panel/` 挂载 + 真实 `/v1/*` 路由，两个方向一次测全。仅测面板侧（internal/panel）
// 证明不了"/v1/* 拒绝 panel_key"，而 internal/server 是另一位队友的改动范围，
// 故在装配层做对照。
//
// 反事实：把 panel 侧 withAuth 改回 p.apiKey()，或把 /v1 侧改成读 PanelKey，
// 下面对应的断言必红。
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// newWiredHandler 复刻 main() 的密钥接线（livecfg 快照 + panel.Config + server.Config）。
func newWiredHandler(t *testing.T, apiKey, panelKey string) http.Handler {
	t.Helper()
	live := livecfg.New(livecfg.Snapshot{APIKey: apiKey, PanelKey: panelKey})
	p := pool.New("")
	pn := panel.New(panel.Config{
		Pool: p, Version: "test", APIKey: apiKey, PanelKey: panelKey, Live: live,
	})
	return server.NewHandler(server.Config{Pool: p, APIKey: apiKey, Live: live, Panel: pn})
}

func doReq(h http.Handler, method, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestWiringPanelAndDataPlaneKeysAreSeparated 两个方向都断言：
//   - /panel/api/overview：panel_key → 200，api_key → 401；
//   - /v1/stats/reset：api_key → 200，panel_key → 401。
func TestWiringPanelAndDataPlaneKeysAreSeparated(t *testing.T) {
	h := newWiredHandler(t, "data-key", "panel-key")

	// 管理面。
	if rec := doReq(h, "GET", "/panel/api/overview", "panel-key"); rec.Code != http.StatusOK {
		t.Errorf("面板 + panel_key → %d, want 200", rec.Code)
	}
	if rec := doReq(h, "GET", "/panel/api/overview", "data-key"); rec.Code != http.StatusUnauthorized {
		t.Errorf("面板 + 数据面 api_key → %d, want 401（拿到客户端 key 不等于拿到管理权）", rec.Code)
	}
	if rec := doReq(h, "GET", "/panel/api/overview", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("面板 + 无凭证 → %d, want 401", rec.Code)
	}

	// 数据面（POST /v1/stats/reset 不触碰上游，是纯鉴权探针）。
	if rec := doReq(h, "POST", "/v1/stats/reset", "data-key"); rec.Code != http.StatusOK {
		t.Errorf("/v1 + api_key → %d, want 200（数据面必须继续可用）", rec.Code)
	}
	if rec := doReq(h, "POST", "/v1/stats/reset", "panel-key"); rec.Code != http.StatusUnauthorized {
		t.Errorf("/v1 + panel_key → %d, want 401（管理面密钥不得进数据面）", rec.Code)
	}
	if rec := doReq(h, "POST", "/v1/stats/reset", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("/v1 + 无凭证 → %d, want 401", rec.Code)
	}
}

// TestWiringChatCompletionsRejectsPanelKey 任务书点名的对照项：
// `POST /v1/chat/completions` 在 panel_key 非空时**不得**接受 panel_key。
// 同一 withAuth 中间件覆盖全部 /v1 路由，这里对最主用的那条再断言一次
// （api_key 侧只断言"过了鉴权层"，空池下具体业务码与鉴权无关）。
func TestWiringChatCompletionsRejectsPanelKey(t *testing.T) {
	h := newWiredHandler(t, "data-key", "panel-key")
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`

	post := func(key string) int {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := post("panel-key"); got != http.StatusUnauthorized {
		t.Errorf("/v1/chat/completions + panel_key → %d, want 401（管理面密钥不得进数据面）", got)
	}
	if got := post(""); got != http.StatusUnauthorized {
		t.Errorf("/v1/chat/completions + 无凭证 → %d, want 401", got)
	}
	if got := post("data-key"); got == http.StatusUnauthorized {
		t.Errorf("/v1/chat/completions + api_key → %d，数据面密钥必须过鉴权层", got)
	}
}

// TestWiringPanelKeyUnsetFallsBack 未设置 panel_key = 保持历史行为：面板仍认 api_key
// （升级后不会打不开面板），数据面行为不变。
func TestWiringPanelKeyUnsetFallsBack(t *testing.T) {
	h := newWiredHandler(t, "data-key", "")
	if rec := doReq(h, "GET", "/panel/api/overview", "data-key"); rec.Code != http.StatusOK {
		t.Errorf("panel_key 未设时面板应回落认 api_key → %d, want 200", rec.Code)
	}
	if rec := doReq(h, "POST", "/v1/stats/reset", "data-key"); rec.Code != http.StatusOK {
		t.Errorf("/v1 + api_key → %d, want 200", rec.Code)
	}
}

// TestWiringBothKeysEmptyNoAuth 双 key 皆空 = 不鉴权（与现状一致）。
func TestWiringBothKeysEmptyNoAuth(t *testing.T) {
	h := newWiredHandler(t, "", "")
	if rec := doReq(h, "GET", "/panel/api/overview", ""); rec.Code != http.StatusOK {
		t.Errorf("双 key 皆空时面板应不鉴权 → %d, want 200", rec.Code)
	}
	if rec := doReq(h, "POST", "/v1/stats/reset", ""); rec.Code != http.StatusOK {
		t.Errorf("双 key 皆空时 /v1 应不鉴权 → %d, want 200", rec.Code)
	}
}
