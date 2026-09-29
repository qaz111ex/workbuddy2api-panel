// panel_key_test.go 管理面/数据面密钥分离（task-9 / fork 调研结论 H4）的行为闸门。
//
// 缺陷：面板与 /v1/* 复用同一个 api_key，而 api_key 是**必须外发给客户端**的数据面
// 凭证（opencode / Claude Code / SDK 都要填）——等于任何拿到它的客户端/日志/截图都
// 拿到整个账号池的管理权（/panel/api/overview 会吐出全部账号 uid/昵称/余额，面板
// 端点还能改账号池与配置）。
//
// 修复：新增 panel_key。非空 = 面板只认它（严格分离，两个方向都断言）；空 = 回落
// 复用 api_key（现有用户升级后不会打不开面板）；都空 = 不鉴权。热改走既有 livecfg。
//
// 反事实：把 withAuth 改回 p.apiKey() 时，"面板拒绝 api_key" 与 "面板接受 panel_key"
// 两条断言必红。
package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// panelCall 用给定 Bearer 打一个面板路由并返回状态码（key 为空 = 不带 Authorization）。
func panelCall(p *Panel, path, key string) int {
	req := httptest.NewRequest("GET", path, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec.Code
}

// newKeyPanel 构造一个带空池的面板（overview 可正常返回 200，不依赖上游）。
func newKeyPanel(cfg Config) *Panel {
	if cfg.Pool == nil {
		cfg.Pool = pool.New("")
	}
	if cfg.Version == "" {
		cfg.Version = "test"
	}
	return New(cfg)
}

// TestPanelKeyStrictSeparation 两方向都断言：panel_key 非空时
//   - 面板：panel_key → 200，api_key → 401（否则"其实还是互认"会漏过去）；
//   - 无凭证 / 错 key → 401。
func TestPanelKeyStrictSeparation(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{APIKey: "data-key", PanelKey: "panel-key"})
	p := newKeyPanel(Config{APIKey: "data-key", PanelKey: "panel-key", Live: live})

	cases := []struct {
		name string
		key  string
		want int
	}{
		{"panel_key 必须被接受", "panel-key", http.StatusOK},
		{"数据面 api_key 必须被面板拒绝", "data-key", http.StatusUnauthorized},
		{"无关密钥被拒绝", "nope", http.StatusUnauthorized},
		{"无凭证被拒绝", "", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := panelCall(p, "/panel/api/overview", tc.key); got != tc.want {
				t.Errorf("GET /panel/api/overview key=%q → %d, want %d", tc.key, got, tc.want)
			}
		})
	}
	// 静态字段（Live 为 nil）同样分离：这是裸用/测试装配路径。
	p2 := newKeyPanel(Config{APIKey: "data-key", PanelKey: "panel-key"})
	if got := panelCall(p2, "/panel/api/overview", "panel-key"); got != http.StatusOK {
		t.Errorf("Live=nil: panel_key → %d, want 200", got)
	}
	if got := panelCall(p2, "/panel/api/overview", "data-key"); got != http.StatusUnauthorized {
		t.Errorf("Live=nil: api_key 必须被面板拒绝 → %d, want 401", got)
	}
}

// TestPanelKeyFallsBackToAPIKeyWhenUnset panel_key 为空 = 保持历史行为（回落复用
// api_key）——现有用户升级后不设也不会打不开面板。
func TestPanelKeyFallsBackToAPIKeyWhenUnset(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{APIKey: "data-key"})
	p := newKeyPanel(Config{APIKey: "data-key", Live: live})
	if got := panelCall(p, "/panel/api/overview", "data-key"); got != http.StatusOK {
		t.Errorf("panel_key 未设时应回落复用 api_key → %d, want 200", got)
	}
	// Live 为 nil 的静态回落路径。
	p2 := newKeyPanel(Config{APIKey: "data-key"})
	if got := panelCall(p2, "/panel/api/overview", "data-key"); got != http.StatusOK {
		t.Errorf("Live=nil 且 panel_key 未设时应回落 api_key → %d, want 200", got)
	}
}

// TestPanelNoAuthWhenBothKeysEmpty 两个 key 都为空 = 不鉴权（与现状一致）。
func TestPanelNoAuthWhenBothKeysEmpty(t *testing.T) {
	p := newKeyPanel(Config{Live: livecfg.New(livecfg.Snapshot{})})
	if got := panelCall(p, "/panel/api/overview", ""); got != http.StatusOK {
		t.Errorf("双 key 皆空（Live）时应不鉴权 → %d, want 200", got)
	}
	p2 := newKeyPanel(Config{})
	if got := panelCall(p2, "/panel/api/overview", ""); got != http.StatusOK {
		t.Errorf("双 key 皆空（Live=nil）时应不鉴权 → %d, want 200", got)
	}
}

// TestPanelKeyHotReload 面板里改 panel_key 后下一个请求立即按新值判定；清空后立即
// 回到"复用 api_key"的回落语义（不是卡在启动值上）。
func TestPanelKeyHotReload(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{APIKey: "data-key", PanelKey: "panel-1"})
	p := newKeyPanel(Config{APIKey: "data-key", PanelKey: "panel-1", Live: live})

	if got := panelCall(p, "/panel/api/overview", "panel-1"); got != http.StatusOK {
		t.Fatalf("改前 panel-1 → %d, want 200", got)
	}
	// 热改：同一个进程、同一个 Panel 对象，不重启。
	live.Store(livecfg.Snapshot{APIKey: "data-key", PanelKey: "panel-2"})
	if got := panelCall(p, "/panel/api/overview", "panel-1"); got != http.StatusUnauthorized {
		t.Errorf("热改后旧 panel_key 应失效 → %d, want 401", got)
	}
	if got := panelCall(p, "/panel/api/overview", "panel-2"); got != http.StatusOK {
		t.Errorf("热改后新 panel_key 应立即生效 → %d, want 200", got)
	}
	// 清空 panel_key → 立即回到复用 api_key（data-key 是当前 Live 的 APIKey）。
	live.Store(livecfg.Snapshot{APIKey: "data-key"})
	if got := panelCall(p, "/panel/api/overview", "panel-2"); got != http.StatusUnauthorized {
		t.Errorf("清空 panel_key 后旧值应失效 → %d, want 401", got)
	}
	if got := panelCall(p, "/panel/api/overview", "data-key"); got != http.StatusOK {
		t.Errorf("清空 panel_key 后应回落复用 api_key → %d, want 200", got)
	}
}

// TestPanelConfigNotLeakedToUnauthenticated panel_key 明文不得回显给未鉴权请求：
// /panel/api/config 在鉴权之后才读配置；用数据面 api_key 打它必须 401，且 401 响应体
// 里不得出现 panel_key 的值。
func TestPanelConfigNotLeakedToUnauthenticated(t *testing.T) {
	const secret = "panel-secret-xyz"
	live := livecfg.New(livecfg.Snapshot{APIKey: "data-key", PanelKey: secret})
	p := newKeyPanel(Config{
		APIKey: "data-key", PanelKey: secret, Live: live,
		LoadConfig: func() (any, error) {
			return map[string]any{"api_key": "data-key", "panel_key": secret}, nil
		},
	})

	req := httptest.NewRequest("GET", "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer data-key") // 数据面 key：不得读配置
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("数据面 key 读 /panel/api/config → %d, want 401", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Errorf("401 响应体泄露了 panel_key 明文: %s", rec.Body.String())
	}

	// 正确的 panel_key 才读得到（配置页需要回填表单）。
	req2 := httptest.NewRequest("GET", "/panel/api/config", nil)
	req2.Header.Set("Authorization", "Bearer "+secret)
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("panel_key 读配置 → %d, want 200 body=%s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), secret) {
		t.Errorf("已鉴权的配置读取应包含 panel_key 供表单回填: %s", rec2.Body.String())
	}
}

// TestPanelKeyFrontendWiring 前端接线闸门：JS 冒烟测试用的是 inert DOM 桩，
// 元素缺失（$('x') 返回 null → .onclick 抛错、整页白屏）它抓不到，故这里直接
// 对真实 index.html / app.js 断言面板密钥字段与按钮齐全。
func TestPanelKeyFrontendWiring(t *testing.T) {
	p := newKeyPanel(Config{})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	html := rec.Body.String()
	for _, must := range []string{`name="panel_key"`, `id="cfgPanelKey"`, `id="btnEyePanel"`} {
		if !strings.Contains(html, must) {
			t.Errorf("index.html 缺少 %s（配置页无法填 panel_key，或 JS 取元素会抛错）", must)
		}
	}
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, httptest.NewRequest("GET", "/panel/app.js", nil))
	js := rec2.Body.String()
	if !strings.Contains(js, "panel_key: ['panel_key']") {
		t.Error("app.js 的 CFG_MAP 未登记 panel_key（读取/保存会漏掉该字段）")
	}
	if !strings.Contains(js, "btnEyePanel") {
		t.Error("app.js 未绑定 btnEyePanel（index.html 上的按钮会失效）")
	}
}

// TestPanelOverviewAuthRequiredReflectsPanelKey overview.auth_required 描述**面板自身**
// 是否需要密钥：panel_key 非空即为 true（即使 api_key 为空），双空才 false。
func TestPanelOverviewAuthRequiredReflectsPanelKey(t *testing.T) {
	// api_key 为空 + panel_key 非空：面板仍需鉴权（管理面独立于数据面）。
	live := livecfg.New(livecfg.Snapshot{PanelKey: "panel-key"})
	p := newKeyPanel(Config{PanelKey: "panel-key", Live: live})
	req := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req.Header.Set("Authorization", "Bearer panel-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"auth_required":true`) {
		t.Errorf("api_key 为空但 panel_key 非空时 auth_required 应为 true: %s", rec.Body.String())
	}
	// 双空 → false。
	p2 := newKeyPanel(Config{})
	rec2 := httptest.NewRecorder()
	p2.ServeHTTP(rec2, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if !strings.Contains(rec2.Body.String(), `"auth_required":false`) {
		t.Errorf("双 key 皆空时 auth_required 应为 false: %s", rec2.Body.String())
	}
}
