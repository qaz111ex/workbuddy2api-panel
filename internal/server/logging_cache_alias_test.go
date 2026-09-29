// logging_cache_alias_test.go 流式请求的缓存命中别名兼容回归。
//
// 缺陷：部分上游把**真实**缓存命中量放在 prompt_tokens_details.cached_tokens，
// 同时把顶层 prompt_cache_hit_tokens 留为 0。流式路径的 parseSSELine 只认顶层 →
// 面板 /v1/stats 的命中率对**所有流式请求**恒为 0（而非流式走 Aggregate 已被
// upstream 的别名统一修好）——同一网关两条路径算出两套命中率。
//
// 闸门：嵌套明细有真实值时必须以它为准；顶层显式 0 不得盖掉它；顶层有正值时
// 优先级仍按 upstream.bestUsageCacheHitTokens 的顺序（嵌套优先，其次三别名）。
package server

import (
	"strings"
	"testing"
	"time"
)

// parseCacheHit 把一行 SSE usage 喂给真实解析器，返回命中的 cache_* 三段。
func parseCacheHit(t *testing.T, usageJSON string) (int, int, int) {
	t.Helper()
	s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	// data: 前缀 + 含 usage 的完整帧（解析器按行处理）。
	s.parseSSELine(`data: {"choices":[],"usage":` + usageJSON + `}`)
	return s.CacheTokens()
}

func TestStreamingCacheHitUsesNestedDetails(t *testing.T) {
	hit, _, _ := parseCacheHit(t, `{"prompt_cache_hit_tokens":0,"prompt_tokens_details":{"cached_tokens":20864}}`)
	if hit != 20864 {
		t.Errorf("嵌套明细的真实命中量必须被采信：hit=%d want 20864（顶层显式 0 不得盖掉它）", hit)
	}
}

func TestStreamingCacheHitNestedBeatsTopLevel(t *testing.T) {
	// 嵌套优先（与 upstream.bestUsageCacheHitTokens 同序）：两者都有值时取嵌套。
	hit, _, _ := parseCacheHit(t, `{"prompt_cache_hit_tokens":111,"prompt_tokens_details":{"cached_tokens":222}}`)
	if hit != 222 {
		t.Errorf("嵌套明细优先级最高：hit=%d want 222", hit)
	}
}

func TestStreamingCacheHitAliasFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage string
		want  int
	}{
		{"顶层 prompt_cache_hit_tokens", `{"prompt_cache_hit_tokens":333}`, 333},
		{"cache_read_input_tokens 别名", `{"cache_read_input_tokens":444}`, 444},
		{"顶层 cached_tokens 别名", `{"cached_tokens":555}`, 555},
		{"Responses 形态 input_tokens_details", `{"input_tokens_details":{"cached_tokens":666}}`, 666},
		{"全 0（真未命中）", `{"prompt_cache_hit_tokens":0}`, 0},
		{"字段全缺", `{}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit, _, _ := parseCacheHit(t, tc.usage)
			if hit != tc.want {
				t.Errorf("hit=%d want %d（usage=%s）", hit, tc.want, tc.usage)
			}
		})
	}
}

func TestStreamingCacheMissAndWriteUnchanged(t *testing.T) {
	// 本次修复只动命中量：miss/write 仍只认原有顶层字段（不擅自扩口径）。
	_, miss, wr := parseCacheHit(t, `{"prompt_cache_miss_tokens":7,"prompt_cache_write_tokens":9,"prompt_tokens_details":{"cached_tokens":100}}`)
	if miss != 7 || wr != 9 {
		t.Errorf("miss/write 不应受命中别名修复影响：miss=%d wr=%d want 7/9", miss, wr)
	}
}

func TestFirstPositiveSemantics(t *testing.T) {
	if got := firstPositive(0, 0, 5, 7); got != 5 {
		t.Errorf("firstPositive 应跳过 0 取首个正值：got %d", got)
	}
	if got := firstPositive(0, 0, 0); got != 0 {
		t.Errorf("全 0 应返回 0（真未命中，不是未知）：got %d", got)
	}
}
