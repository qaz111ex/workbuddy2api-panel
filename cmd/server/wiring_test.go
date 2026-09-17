package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// realmPool 构造含 cn/global 账号的池并确保 global 开关开启（缺省）。
func realmPool(t *testing.T) *pool.Pool {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	return p
}

// TestRealmAwareAvailableForModel 断言会话粘性的 realm 感知闭包：
// 带前缀的模型名按 realm 过滤可用账号；裸名在跨域回落开启（本 fork 默认）时
// **不限域**（粘性可绑定任一域的同模型账号）。
func TestRealmAwareAvailableForModel(t *testing.T) {
	p := realmPool(t)
	fn := realmAwareAvailableForModel(p, true)

	cases := []struct {
		model string
		want  []string
	}{
		{"glm-5.2", []string{"cn1", "g1"}}, // 裸名 + fallback 开启 → 不限域
		{"cn:glm-5.2", []string{"cn1"}},    // 显式 cn 前缀 → cn 集合
		{"global:gpt-5.4", []string{"g1"}}, // global 前缀 → global 集合
	}
	for _, c := range cases {
		if got := fn(c.model); !reflect.DeepEqual(got, c.want) {
			t.Errorf("AvailableForModel(%q)=%v want %v", c.model, got, c.want)
		}
	}
}

// TestRealmAwareAvailableForModelFallbackOff 跨域回落关闭时，裸名收敛为 cn
// （严格域路由语义，与上游旧行为一致）。
func TestRealmAwareAvailableForModelFallbackOff(t *testing.T) {
	p := realmPool(t)
	fn := realmAwareAvailableForModel(p, false)
	if got := fn("glm-5.2"); !reflect.DeepEqual(got, []string{"cn1"}) {
		t.Errorf("fallback off: AvailableForModel(glm-5.2)=%v want [cn1]", got)
	}
}

// TestRealmAwareAvailableForModelGlobalDisabled 逃生门：全局开关显式关闭（config
// "enabled": false）时，即便 auth 写了 realm=global 也不路由 global——闭包对 global:
// 前缀返回空集（纯 CN 锁定语义与旧缺省等价）。
func TestRealmAwareAvailableForModelGlobalDisabled(t *testing.T) {
	auth.SetGlobalEnabled(false)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	fn := realmAwareAvailableForModel(p, true)

	// 开关关闭 → 该 global 账号 Realm()=="cn"（逃生门），对 global: 前缀不可见。
	if got := fn("global:gpt-5.4"); len(got) != 0 {
		t.Errorf("global disabled: AvailableForModel(global:gpt-5.4)=%v want empty", got)
	}
	// 裸名 → cn：该账号被当作 cn 可见（现状等价，纯 CN 锁定）。
	if got := fn("glm-5.2"); !reflect.DeepEqual(got, []string{"g1"}) {
		t.Errorf("global disabled: AvailableForModel(glm-5.2)=%v want [g1]", got)
	}
}

// TestRealmAwareAvailableForModelDefaultOnCNZeroRegression 开关缺省开启（生产语义）时
// 纯 CN 部署零回归：cn 账号（cn domain / 空 domain）对裸名与 cn: 前缀都可见，
// global: 前缀对纯 CN 池不可见（池里无 global 账号）。
func TestRealmAwareAvailableForModelDefaultOnCNZeroRegression(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "cn2", Domain: ""}) // 空 domain → cn（老 CN 凭证）
	fn := realmAwareAvailableForModel(p, true)

	cases := []struct {
		model string
		want  []string
	}{
		{"glm-5.2", []string{"cn1", "cn2"}},    // 裸名 → cn 集合（现状零回归）
		{"cn:glm-5.2", []string{"cn1", "cn2"}}, // cn 前缀 → cn 集合
		{"global:gpt-5.4", nil},                // global 前缀 → 纯 CN 池无可用（不对 spread）
	}
	for _, c := range cases {
		want := c.want
		if want == nil {
			want = []string{}
		}
		if got := fn(c.model); !reflect.DeepEqual(got, want) {
			t.Errorf("AvailableForModel(%q)=%v want %v", c.model, got, want)
		}
	}
}

// TestModelJSONPath model.json 路径推导（本 fork 为 stateSibling 的第二参数）（context_length 四级查找链第 3 级接线）：
// 与 state.json 同目录同名换缀（Docker ./data volume 持久化）；空 state 路径 →
// 空串（禁用落盘，内存 + 种子仍可用）。
func TestModelJSONPath(t *testing.T) {
	cases := []struct {
		state, want string
	}{
		{"./data/state.json", "data/model.json"},
		{"/app/data/state.json", "/app/data/model.json"},
		{"state.json", "model.json"},
		// 本 fork 的 stateSibling 对空 state 返回裸文件名（上游 modelJSONPath 返回空串）；
		// 空路径实际由 SetModelCatalogPath 的调用方（cfg.StateFile 非空）保证不出现。
		{"", "model.json"},
	}
	for _, c := range cases {
		// 归一化 got 侧：modelJSONPath 走 filepath.Join，Windows 产出反斜杠；
		// want 本就是正斜杠字面量（跨平台规范形式），对 want 做 ToSlash 是无操作，
		// 反斜杠会原样留在 got 里导致断言在 Windows 必然失败。
		if got := filepath.ToSlash(stateSibling(c.state, "model.json")); got != c.want {
			t.Errorf("modelJSONPath(%q)=%q want %q", c.state, got, c.want)
		}
	}
}
