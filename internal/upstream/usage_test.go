// usage_test.go 钉住出口侧 usage 缓存命中别名的统一（吸收上游 25016de）。
//
// 真实缺陷：部分上游把命中量放在 prompt_tokens_details.cached_tokens，同时把
// cache_read_input_tokens / cached_tokens / prompt_cache_hit_tokens 这些兼容别名留为 0。
// 严格按 schema 解析的下游优先读别名 → 「命中了」被误判成「没命中」。
package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 嵌套真实命中量必须回填到三个顶层别名与 prompt_tokens_details。
func TestNormalizeUsageCacheAliasesMirrorsNestedHit(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":            21041.0,
		"completion_tokens":        8.0,
		"total_tokens":             21049.0,
		"cache_read_input_tokens":  0.0,
		"cached_tokens":            0.0,
		"prompt_cache_hit_tokens":  0.0,
		"prompt_cache_miss_tokens": 177.0,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 20864.0,
		},
	}

	got := normalizeUsageCacheAliases(usage)

	for _, key := range []string{
		"cache_read_input_tokens",
		"cached_tokens",
		"prompt_cache_hit_tokens",
	} {
		if got[key] != 20864.0 {
			t.Fatalf("%s=%v want 20864", key, got[key])
		}
	}
	details := got["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != 20864.0 {
		t.Fatalf("prompt_tokens_details.cached_tokens=%v want 20864", details["cached_tokens"])
	}
	// 未命中量不该被顺手改写（本修复只统一命中口径）。
	if got["prompt_cache_miss_tokens"] != 177.0 {
		t.Fatalf("prompt_cache_miss_tokens=%v want 177（不应被改写）", got["prompt_cache_miss_tokens"])
	}
}

// 全 0 是「真的没命中」，不是「未知」：必须零改写原样返回。
func TestNormalizeUsageCacheAliasesPreservesZeroResult(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":           35.0,
		"completion_tokens":       2.0,
		"total_tokens":            37.0,
		"cache_read_input_tokens": 0.0,
		"prompt_cache_hit_tokens": 0.0,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 0.0,
		},
	}

	got := normalizeUsageCacheAliases(usage)

	details := got["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != 0.0 {
		t.Fatalf("prompt_tokens_details.cached_tokens=%v want 0", details["cached_tokens"])
	}
	// 无正值 → 原对象返回（同一个 map），零分配零改写。
	if len(got) != len(usage) {
		t.Fatalf("无正值时不应增删字段: got %d want %d", len(got), len(usage))
	}
	if _, ok := got["cache_read_input_tokens"]; !ok {
		t.Fatal("原有字段丢失")
	}
}

// 有正值时必须返回**新 map**：调用方可能还持有入参做统计（不得被就地改写）。
func TestNormalizeUsageCacheAliasesDoesNotMutateInput(t *testing.T) {
	usage := map[string]any{
		"cache_read_input_tokens": 0.0,
		"prompt_cache_hit_tokens": 0.0,
		"prompt_tokens_details":   map[string]any{"cached_tokens": 99.0},
	}
	origDetails := usage["prompt_tokens_details"].(map[string]any)

	got := normalizeUsageCacheAliases(usage)

	if usage["cache_read_input_tokens"] != 0.0 || usage["prompt_cache_hit_tokens"] != 0.0 {
		t.Fatalf("入参顶层被就地改写: %+v", usage)
	}
	if origDetails["cached_tokens"] != 99.0 {
		t.Fatalf("入参嵌套对象被就地改写: %+v", origDetails)
	}
	if got["cache_read_input_tokens"] != 99.0 {
		t.Fatalf("返回值未回填: %+v", got)
	}
}

// input_tokens_details（Responses API 形态）只在响应本来就带该对象时同步，
// 不能给 Chat-only 客户端凭空发明字段。
func TestNormalizeUsageCacheAliasesInputDetailsOnlyWhenPresent(t *testing.T) {
	without := map[string]any{
		"prompt_cache_hit_tokens": 7.0,
		"prompt_tokens_details":   map[string]any{"cached_tokens": 0.0},
	}
	got := normalizeUsageCacheAliases(without)
	if _, ok := got["input_tokens_details"]; ok {
		t.Fatal("上游未下发 input_tokens_details 时不应发明该字段")
	}

	with := map[string]any{
		"prompt_cache_hit_tokens": 7.0,
		"prompt_tokens_details":   map[string]any{"cached_tokens": 0.0},
		"input_tokens_details":    map[string]any{"cached_tokens": 0.0, "audio_tokens": 1.0},
	}
	got = normalizeUsageCacheAliases(with)
	det, ok := got["input_tokens_details"].(map[string]any)
	if !ok {
		t.Fatal("原有 input_tokens_details 丢失")
	}
	if det["cached_tokens"] != 7.0 {
		t.Fatalf("input_tokens_details.cached_tokens=%v want 7", det["cached_tokens"])
	}
	if det["audio_tokens"] != 1.0 {
		t.Fatalf("input_tokens_details 其它键被丢弃: %+v", det)
	}
}

// 顶层别名带正值、嵌套为 0 时也要统一（反向形态：别名有值、明细缺）。
func TestNormalizeUsageCacheAliasesTopLevelWinsWhenDetailsZero(t *testing.T) {
	usage := map[string]any{
		"cache_read_input_tokens": 42.0,
		"prompt_cache_hit_tokens": 0.0,
		"prompt_tokens_details":   map[string]any{"cached_tokens": 0.0},
	}
	got := normalizeUsageCacheAliases(usage)
	if got["prompt_cache_hit_tokens"] != 42.0 || got["cached_tokens"] != 42.0 {
		t.Fatalf("别名未统一: %+v", got)
	}
	if d := got["prompt_tokens_details"].(map[string]any); d["cached_tokens"] != 42.0 {
		t.Fatalf("嵌套未回填: %+v", d)
	}
}

// 非流式聚合路径：Aggregate 出口必须已统一别名。
func TestAggregateNormalizesUsageCacheAliases(t *testing.T) {
	sse := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":21041,\"completion_tokens\":8,\"total_tokens\":21049,\"cache_read_input_tokens\":0,\"cached_tokens\":0,\"prompt_cache_hit_tokens\":0,\"prompt_tokens_details\":{\"cached_tokens\":20864}}}\n\n" +
		"data: [DONE]\n\n"

	resp, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	usage := resp["usage"].(map[string]any)
	if usage["cache_read_input_tokens"] != 20864.0 {
		t.Fatalf("cache_read_input_tokens=%v want 20864", usage["cache_read_input_tokens"])
	}
	if usage["cached_tokens"] != 20864.0 {
		t.Fatalf("cached_tokens=%v want 20864", usage["cached_tokens"])
	}
	if usage["prompt_cache_hit_tokens"] != 20864.0 {
		t.Fatalf("prompt_cache_hit_tokens=%v want 20864", usage["prompt_cache_hit_tokens"])
	}
	// 与既有 ensureUsageTotal 组合不得互相覆盖（total 合成仍然成立）。
	if usage["total_tokens"] != 21049.0 {
		t.Fatalf("total_tokens=%v want 21049", usage["total_tokens"])
	}
}

// 非流式：别名统一与 total 合成叠加（上游末帧缺 total 的形态）。
func TestAggregateNormalizesUsageCacheAliasesAndSynthesizesTotal(t *testing.T) {
	sse := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"prompt_cache_hit_tokens\":0,\"prompt_tokens_details\":{\"cached_tokens\":80}}}\n\n" +
		"data: [DONE]\n\n"

	resp, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	usage := resp["usage"].(map[string]any)
	if usage["total_tokens"] != 105.0 {
		t.Fatalf("total_tokens=%v want 105（合成未生效）", usage["total_tokens"])
	}
	if usage["prompt_cache_hit_tokens"] != 80.0 {
		t.Fatalf("prompt_cache_hit_tokens=%v want 80", usage["prompt_cache_hit_tokens"])
	}
}

// 流式透传路径：normalizeFrame 出口的 usage 也必须已统一别名。
func TestStreamNormalizesUsageCacheAliases(t *testing.T) {
	sse := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":21041,\"completion_tokens\":8,\"total_tokens\":21049,\"cache_read_input_tokens\":0,\"cached_tokens\":0,\"prompt_cache_hit_tokens\":0,\"prompt_tokens_details\":{\"cached_tokens\":20864}}}\n\n" +
		"data: [DONE]\n\n"

	recorder := httptest.NewRecorder()
	if err := Stream(recorder, strings.NewReader(sse)); err != nil {
		t.Fatal(err)
	}

	var usage map[string]any
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
			t.Fatal(err)
		}
		if raw, ok := frame["usage"].(map[string]any); ok && raw != nil {
			usage = raw
		}
	}
	if usage == nil {
		t.Fatal("stream response missing usage")
	}
	if usage["cache_read_input_tokens"] != 20864.0 {
		t.Fatalf("cache_read_input_tokens=%v want 20864", usage["cache_read_input_tokens"])
	}
	if usage["prompt_cache_hit_tokens"] != 20864.0 {
		t.Fatalf("prompt_cache_hit_tokens=%v want 20864", usage["prompt_cache_hit_tokens"])
	}
}

// 流式：usage 为 null / 非对象形态时透传语义不变（不 panic、不发明对象）。
func TestNormalizeFrameUsageShapeUnchanged(t *testing.T) {
	frame := normalizeFrame(map[string]any{
		"id":      "x",
		"choices": []any{},
	})
	if frame["usage"] != nil {
		t.Fatalf("usage 缺失应补 nil，got %#v", frame["usage"])
	}
	frame = normalizeFrame(map[string]any{
		"id":      "x",
		"choices": []any{},
		"usage":   "weird",
	})
	if frame["usage"] != "weird" {
		t.Fatalf("非对象 usage 应原样透传，got %#v", frame["usage"])
	}
}
