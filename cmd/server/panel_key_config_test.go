// panel_key_config_test.go panel_key 的配置层接线：解析 / 环境变量 / 落盘热生效 /
// 样例文件与首启生成的一致性。
package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
)

// TestConfigPanelKeyParseAndEnv panel_key 是顶层键（与 api_key 同层），支持
// WB2A_PANEL_KEY 覆盖；缺省为空（= 回落复用 api_key）。
func TestConfigPanelKeyParseAndEnv(t *testing.T) {
	c, err := ParseConfig([]byte(`{"api_key":"a","panel_key":"p"}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if c.APIKey != "a" || c.PanelKey != "p" {
		t.Errorf("解析结果 api_key=%q panel_key=%q, want a/p", c.APIKey, c.PanelKey)
	}
	// 缺省：空 = 回落（绝不因为新增键而改变老配置行为）。
	if d := Default(); d.PanelKey != "" {
		t.Errorf("panel_key 缺省应为空（回落复用 api_key），got %q", d.PanelKey)
	}
	// 老配置（无 panel_key 键）解析后仍为空。
	old, err := ParseConfig([]byte(`{"listen":":7863","api_key":"legacy"}`))
	if err != nil {
		t.Fatal(err)
	}
	if old.PanelKey != "" {
		t.Errorf("老配置应保持 panel_key 为空，got %q", old.PanelKey)
	}

	t.Setenv("WB2A_PANEL_KEY", "env-panel")
	env, err := Load("") // 不读文件，只跑 Default + env + normalize
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if env.PanelKey != "env-panel" {
		t.Errorf("WB2A_PANEL_KEY 未生效: %q", env.PanelKey)
	}
}

// TestSaveConfigHotAppliesPanelKey 面板保存 panel_key 后：livecfg 快照立即更新，
// 且**已装配的 Panel 对象**下一个请求就按新值判定（无需重启）。
func TestSaveConfigHotAppliesPanelKey(t *testing.T) {
	path, live, p, up, sch := newSaveConfigFixture(t)
	// 与 main 同款装配：live 快照同时承载 api_key 与 panel_key。
	live.Store(livecfg.Snapshot{APIKey: "orig-key"})
	pn := panel.New(panel.Config{Pool: p, Version: "test", APIKey: "orig-key", Live: live})

	call := func(key string) int {
		req := httptest.NewRequest("GET", "/panel/api/overview", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		pn.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := call("orig-key"); got != http.StatusOK {
		t.Fatalf("保存前面板应认 api_key（回落）→ %d, want 200", got)
	}

	if _, err := saveConfig([]byte(`{"panel_key":"hot-panel"}`), path, live, p, up, sch); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	if got := live.Load().PanelKey; got != "hot-panel" {
		t.Fatalf("saveConfig 后 livecfg.PanelKey=%q, want hot-panel（热改未接线）", got)
	}
	// 下一个请求立即生效：panel_key 通过，数据面 api_key 被拒。
	if got := call("hot-panel"); got != http.StatusOK {
		t.Errorf("热改后 panel_key → %d, want 200", got)
	}
	if got := call("orig-key"); got != http.StatusUnauthorized {
		t.Errorf("热改后数据面 api_key 必须被面板拒绝 → %d, want 401", got)
	}
	// 落盘也要带上（重启后仍分离）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"panel_key": "hot-panel"`) {
		t.Errorf("panel_key 未落盘: %s", raw)
	}
}

// TestConfigExampleHasPanelKey 样例配置必须带上 panel_key（用户抄样例即见该键）。
func TestConfigExampleHasPanelKey(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatalf("read config.example.json: %v", err)
	}
	c, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("config.example.json 必须可被 ParseConfig 解析: %v", err)
	}
	if !strings.Contains(string(raw), `"panel_key"`) {
		t.Errorf("config.example.json 缺少 panel_key 键")
	}
	if c.PanelKey != "" {
		t.Errorf("样例里的 panel_key 应为空占位（不能是真实密钥），got %q", c.PanelKey)
	}
}

// TestWriteDefaultDoesNotGeneratePanelKey 首启自动生成的配置**只**生成 api_key。
//
// 刻意的取舍：首启流程是「双击运行 → 从日志抄 api_key → 进面板」。若同时随机生成
// panel_key 而不把它打到日志里，用户就再也进不了面板；打到日志里又要让用户理解
// 两个 key 各填哪里。故首启不生成，改为启动日志告警建议自行设置（面板里可热改）。
// 这条断言把该取舍钉住：将来若改成生成，必须同时更新首启日志指引。
func TestWriteDefaultDoesNotGeneratePanelKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	key, err := WriteDefault(path)
	if err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}
	if key == "" {
		t.Fatal("WriteDefault 必须返回随机 api_key")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("生成的配置必须可解析: %v", err)
	}
	if c.APIKey == "" {
		t.Error("生成的配置必须有 api_key")
	}
	if c.PanelKey != "" {
		t.Errorf("首启不应自动生成 panel_key（got %q）——若改此行为，须同步打印到启动日志并更新指引", c.PanelKey)
	}
}
