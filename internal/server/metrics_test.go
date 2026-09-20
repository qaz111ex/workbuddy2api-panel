package server

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// resetMetricsForTest 隔离用例间的全局聚合状态。
func resetMetricsForTest(t *testing.T) {
	t.Helper()
	ResetMetrics()
	t.Cleanup(ResetMetrics)
}

// TestMetricsAggregatesByModel 同一模型的多次请求累加，派生字段按口径折算。
func TestMetricsAggregatesByModel(t *testing.T) {
	resetMetricsForTest(t)

	// 两次成功 + 一次失败，同一模型。
	mk := func(status int, ttfbMS, toks, prompt, hit, miss, wr int, credit float64, mode string) *chatStat {
		return &chatStat{
			model: "global:deepseek-v4.1-flash", mode: mode, status: status,
			ttfb: time.Duration(ttfbMS) * time.Millisecond,
			toks: toks, hasUsage: true, prompt: prompt,
			cacheHit: hit, cacheMiss: miss, cacheWr: wr,
			credit: credit, hasCredit: true,
		}
	}
	recordChatMetric(mk(200, 1000, 100, 50, 800, 200, 0, 0.02, "stream"), 5*time.Second)
	recordChatMetric(mk(200, 2000, 200, 60, 900, 100, 0, 0.04, "stream"), 7*time.Second)
	recordChatMetric(mk(429, 0, -1, 0, 0, 0, 0, 0, "sync"), 1*time.Second)

	snap := MetricsSnapshotOf()
	if len(snap.Models) != 1 {
		t.Fatalf("models=%d want 1", len(snap.Models))
	}
	m := snap.Models[0]
	if m.Requests != 3 || m.Success != 2 || m.Failed != 1 {
		t.Errorf("req/succ/fail = %d/%d/%d want 3/2/1", m.Requests, m.Success, m.Failed)
	}
	if m.Streaming != 2 {
		t.Errorf("streaming=%d want 2", m.Streaming)
	}
	// 端到端均值 = (5000+7000+1000)/3 = 4333.33ms
	if got := m.AvgLatencyMS; got < 4333 || got > 4334 {
		t.Errorf("avg_latency=%.2f want ~4333.33", got)
	}
	// TTFB 只统计有观测的两次：(1000+2000)/2 = 1500ms
	if got := m.AvgTTFBMS; got != 1500 {
		t.Errorf("avg_ttfb=%.2f want 1500", got)
	}
	// token 只累加 hasUsage 的两次
	if m.PromptTokens != 110 || m.CompletionTokens != 300 {
		t.Errorf("prompt/comp = %d/%d want 110/300", m.PromptTokens, m.CompletionTokens)
	}
	// 命中率 = 1700/(1700+300) = 0.85
	if got := m.CacheHitRate; got < 0.8499 || got > 0.8501 {
		t.Errorf("cache_hit_rate=%.4f want 0.85", got)
	}
	if m.Credit != 0.06 {
		t.Errorf("credit=%.4f want 0.06", m.Credit)
	}
}

// TestMetricsMissingUsageNotCountedAsZero usage 缺失时不得把 0 计进 token/缓存。
func TestMetricsMissingUsageNotCountedAsZero(t *testing.T) {
	resetMetricsForTest(t)

	// hasUsage=false（上游没回 usage）：toks=-1 是哨兵，不该被当成 token 累加。
	recordChatMetric(&chatStat{
		model: "m1", mode: "sync", status: 200, toks: -1,
	}, time.Second)
	// hasUsage=true 且显式全 0：合法观测，参与累加（分母不为零才有意义）。
	recordChatMetric(&chatStat{
		model: "m1", mode: "sync", status: 200, toks: 0, hasUsage: true,
	}, time.Second)

	snap := MetricsSnapshotOf()
	m := snap.Models[0]
	if m.Requests != 2 {
		t.Fatalf("requests=%d want 2", m.Requests)
	}
	if m.CompletionTokens != 0 {
		t.Errorf("completion=%d want 0（-1 哨兵不得计入）", m.CompletionTokens)
	}
	if m.CacheHitRate != 0 {
		t.Errorf("cache_hit_rate=%f want 0（无观测时不做除法）", m.CacheHitRate)
	}
}

// TestMetricsTotalIsSumOfModels total 必须等于各模型累加，不另算一份。
func TestMetricsTotalIsSumOfModels(t *testing.T) {
	resetMetricsForTest(t)

	recordChatMetric(&chatStat{model: "a", mode: "sync", status: 200, toks: 10, hasUsage: true, prompt: 5}, time.Second)
	recordChatMetric(&chatStat{model: "b", mode: "sync", status: 500, toks: 20, hasUsage: true, prompt: 7}, time.Second)

	snap := MetricsSnapshotOf()
	if snap.Total.Requests != 2 || snap.Total.Success != 1 || snap.Total.Failed != 1 {
		t.Errorf("total req/succ/fail = %d/%d/%d want 2/1/1",
			snap.Total.Requests, snap.Total.Success, snap.Total.Failed)
	}
	if snap.Total.PromptTokens != 12 || snap.Total.CompletionTokens != 30 {
		t.Errorf("total tokens = %d/%d want 12/30", snap.Total.PromptTokens, snap.Total.CompletionTokens)
	}
}

// TestMetricsResetClears 重置后归零。
func TestMetricsResetClears(t *testing.T) {
	resetMetricsForTest(t)

	recordChatMetric(&chatStat{model: "a", mode: "sync", status: 200, toks: 10, hasUsage: true}, time.Second)
	if MetricsSnapshotOf().Total.Requests != 1 {
		t.Fatal("前置：应有 1 条")
	}
	ResetMetrics()
	snap := MetricsSnapshotOf()
	if snap.Total.Requests != 0 || len(snap.Models) != 0 {
		t.Errorf("重置后 requests=%d models=%d want 0/0", snap.Total.Requests, len(snap.Models))
	}
}

// TestMetricsEmptyModelFallsBack 空模型名归入 "-"，不丢弃观测。
func TestMetricsEmptyModelFallsBack(t *testing.T) {
	resetMetricsForTest(t)

	recordChatMetric(&chatStat{model: "", mode: "sync", status: 200}, time.Second)
	snap := MetricsSnapshotOf()
	if len(snap.Models) != 1 || snap.Models[0].Model != "-" {
		t.Fatalf("空模型名应归入 \"-\"，得到 %+v", snap.Models)
	}
	if snap.Total.Requests != 1 {
		t.Errorf("观测不得因模型名为空而丢弃")
	}
}

// TestMetricsCapBounded 超容量上限时丢弃新键且不 panic。
func TestMetricsCapBounded(t *testing.T) {
	resetMetricsForTest(t)

	for i := 0; i < metricsCap+50; i++ {
		recordChatMetric(&chatStat{model: string(rune('a'+i%26)) + string(rune('0'+i%10)) + string(rune('A'+i/260)), mode: "sync", status: 200}, time.Second)
	}
	snap := MetricsSnapshotOf()
	if len(snap.Models) > metricsCap {
		t.Errorf("models=%d 超过上限 %d", len(snap.Models), metricsCap)
	}
}

// ─── fork 适配专项：本 fork 的 chatStatsReader 与 sk 上游形态不同 ──────────────
//
// 上游只有 hasUsage 单闸；本 fork 是 promptTokens/completionTokens/totalTokens
// 三件套 + 三个 hasXxx 闸。metrics 的 usage 闸门必须用 UsageSeen()（任一字段出现
// 即算观测），否则「末帧只带 prompt_tokens」的部分观测会被整段漏计——这是本 fork
// 手工适配时必须锁住的行为。

// TestChatStatsReaderUsageSeenPartialUsage 末帧只带 prompt_tokens（无 completion）
// 时：UsageSeen 为真、prompt 与缓存三段可读、completion 仍报缺失。
func TestChatStatsReaderUsageSeenPartialUsage(t *testing.T) {
	const line = `data: {"choices":[{"delta":{"content":"x"}}],"usage":{"prompt_tokens":7,"prompt_cache_hit_tokens":5,"prompt_cache_miss_tokens":2,"prompt_cache_write_tokens":1}}` + "\n\n"
	r := newChatStatsReaderSince(strings.NewReader(line), time.Now())
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}

	if !r.UsageSeen() {
		t.Fatal("UsageSeen()=false want true（只带 prompt_tokens 也算有观测）")
	}
	if got := r.PromptTokens(); got != 7 {
		t.Errorf("PromptTokens()=%d want 7", got)
	}
	hit, miss, wr := r.CacheTokens()
	if hit != 5 || miss != 2 || wr != 1 {
		t.Errorf("CacheTokens()=%d/%d/%d want 5/2/1", hit, miss, wr)
	}
	if _, ok := r.Tokens(); ok {
		t.Error("Tokens() ok=true want false（末帧无 completion_tokens）")
	}
}

// TestMetricsPartialUsageKeepsPromptTokens 部分观测（usage 有 prompt、缺
// completion）时 prompt tokens 与缓存三段必须入账，completion 的 -1 哨兵不得计入。
func TestMetricsPartialUsageKeepsPromptTokens(t *testing.T) {
	resetMetricsForTest(t)

	// 模拟流式路径：hasUsage=UsageSeen()=true，toks 保持 -1 哨兵。
	recordChatMetric(&chatStat{
		model: "cn:hy3", mode: "stream", status: 200,
		toks: -1, hasUsage: true, prompt: 7, cacheHit: 5, cacheMiss: 2, cacheWr: 1,
	}, time.Second)

	snap := MetricsSnapshotOf()
	m := snap.Models[0]
	if m.PromptTokens != 7 {
		t.Errorf("prompt_tokens=%d want 7（部分观测不得整段漏计）", m.PromptTokens)
	}
	if m.CompletionTokens != 0 {
		t.Errorf("completion_tokens=%d want 0（-1 哨兵不得计入）", m.CompletionTokens)
	}
	if m.CacheHitTokens != 5 || m.CacheMissTokens != 2 || m.CacheWriteTokens != 1 {
		t.Errorf("cache = %d/%d/%d want 5/2/1", m.CacheHitTokens, m.CacheMissTokens, m.CacheWriteTokens)
	}
}

// TestChatStatsReaderUsageStillServesUsagePanel fork 独有 Usage()（服务
// /panel/api/usage 用量时间线）在三件套字段下的返回必须保持不变——本任务只允许
// 「追加」字段，绝不能顺手改成上游的 hasUsage/tokens 形态。
func TestChatStatsReaderUsageStillServesUsagePanel(t *testing.T) {
	const line = `data: {"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}` + "\n\n"
	r := newChatStatsReaderSince(strings.NewReader(line), time.Now())
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}

	got := r.Usage()
	want := pool.TokenUsageDelta{
		HasPromptTokens: true, PromptTokens: 11,
		HasCompletionTokens: true, CompletionTokens: 3,
		HasTotalTokens: true, TotalTokens: 14,
	}
	if got != want {
		t.Errorf("Usage()=%+v want %+v", got, want)
	}
	if n, ok := r.TotalTokens(); !ok || n != 14 {
		t.Errorf("TotalTokens()=%d,%v want 14,true", n, ok)
	}
	if !r.UsageSeen() {
		t.Error("UsageSeen()=false want true")
	}
}

// TestChatStatDoneRecordsMetricsAndLogs done() 是唯一埋点：既落表格日志又记账。
func TestChatStatDoneRecordsMetricsAndLogs(t *testing.T) {
	resetMetricsForTest(t)

	st := newChatStat(time.Now(), []byte(`{"model":"cn:hy3","stream":true}`), true)
	st.status = 200
	st.ttfb = 500 * time.Millisecond
	st.toks = 9
	st.hasUsage = true
	st.prompt = 4
	st.done()
	st.done() // 幂等：第二次不得双计

	snap := MetricsSnapshotOf()
	if snap.Total.Requests != 1 {
		t.Fatalf("requests=%d want 1（done 幂等，不得双计）", snap.Total.Requests)
	}
	if snap.Total.PromptTokens != 4 || snap.Total.CompletionTokens != 9 {
		t.Errorf("tokens = %d/%d want 4/9", snap.Total.PromptTokens, snap.Total.CompletionTokens)
	}
	if snap.Total.Streaming != 1 {
		t.Errorf("streaming=%d want 1", snap.Total.Streaming)
	}
}
