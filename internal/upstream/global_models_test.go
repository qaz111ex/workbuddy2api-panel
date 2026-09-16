package upstream

import (
	"net/http"
	"strings"
	"testing"

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
