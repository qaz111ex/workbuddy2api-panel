// 最早到期优先路由（先花掉快过期的积分）的行为锚点。
//
// 口径（与 task 契约一致）：
//   - 只在**成本最优层内**挑（候选 = pick 里已按模型成本分层后的 candsAll）；
//   - 优先集 = 「窗口内仍有积分 + 有有效未来到期批次」的账号，按最早到期升序
//     （同到期时刻 → 该批次剩余积分多者优先；再同 → UID 字典序）；
//   - 已过期 / 零余额 / 无有效到期时间的账号不进优先集；
//   - 优先集为空 → **完全退回**既有加权随机；
//   - SetPreferExpiring(false) 或 window<=0 → 完全退回既有行为（兼容性闸门）；
//   - 热改立即生效。
//
// 抽签确定性：本文件多数用例注入 randInt64N ≡ 0（→ pickWeightedPrecomputed 取
// eligible[0]，即权重最高者），把「退回加权随机」变成可断言的确定结果。
package pool

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// expRead 在白盒读断言里持读锁读取 entry（与既有 pool 测试同风格，避免与落盘
// goroutine 争用）。
func expRead(t *testing.T, p *Pool, uid string, fn func(e *entry)) {
	t.Helper()
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		t.Fatalf("entry %q not found", uid)
	}
	fn(e)
}

// expZeroRand 注入常量随机源：抽签恒取 eligible[0]（权重最高者），消除分布抖动。
func expZeroRand(p *Pool) { p.SetRandomSource(func(int64) int64 { return 0 }) }

// TestPickPrefersEarliestExpiryWithinWindow 窗口内最早到期的账号被稳定优先选中。
func TestPickPrefersEarliestExpiryWithinWindow(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "late"})
	p.Add(&auth.Auth{UID: "early"})
	p.Add(&auth.Auth{UID: "outside"})
	now := time.Now()
	// 三者余额/信用完全相同（隔离 credits 与 ×8 快过期占比权重），只有到期时刻不同。
	p.SetCreditsDetailedWithExpiry("early", 100, 0, 100, now.Add(time.Hour), 100)
	p.SetCreditsDetailedWithExpiry("late", 100, 0, 100, now.Add(3*time.Hour), 100)
	p.SetCreditsDetailedWithExpiry("outside", 100, 0, 0, now.Add(100*time.Hour), 100)
	p.SetPreferExpiring(true, 24*time.Hour)

	for i := 0; i < 5; i++ {
		got := p.Pick()
		if got == nil || got.UID != "early" {
			t.Fatalf("第 %d 次 pick=%v want early（窗口内最早到期优先，且严格升序）", i, got)
		}
	}
}

// TestPickPrefersLargerBatchOnExpiryTie 同到期时刻 → 该批次剩余积分多者优先，
// 且该判据压过 credits 总量差异（避免退化成「谁余额大选谁」）。
func TestPickPrefersLargerBatchOnExpiryTie(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "small"})
	p.Add(&auth.Auth{UID: "big"})
	at := time.Now().Add(2 * time.Hour)
	// small 余额总量更大（1000 > 100），但最早批次剩余更少（40 < 100）。
	p.SetCreditsDetailedWithExpiry("small", 1000, 0, 40, at, 40)
	p.SetCreditsDetailedWithExpiry("big", 100, 0, 100, at, 100)
	p.SetPreferExpiring(true, 24*time.Hour)

	for i := 0; i < 3; i++ {
		got := p.Pick()
		if got == nil || got.UID != "big" {
			t.Fatalf("第 %d 次 pick=%v want big（同到期按批次剩余降序，压过 credits 总量）", i, got)
		}
	}
}

// TestSetPreferExpiringFalseRestoresLegacyWeightedPick 兼容性闸门：
// SetPreferExpiring(false) 与 window=0 都必须**完全退回**既有加权随机行为
// （此处旧行为按 credits 主导 → 巨大余额的 huge 胜出），只有真正开启才按到期排序。
func TestSetPreferExpiringFalseRestoresLegacyWeightedPick(t *testing.T) {
	withNoPickGap(t)
	setup := func() *Pool {
		p := New("")
		p.Add(&auth.Auth{UID: "small"})
		p.Add(&auth.Auth{UID: "huge"})
		now := time.Now()
		// small：窗内最早到期，但余额很小；huge：到期远在窗外，余额巨大。
		p.SetCreditsDetailedWithExpiry("small", 100, 0, 100, now.Add(time.Hour), 100)
		p.SetCreditsDetailedWithExpiry("huge", 100000, 0, 0, now.Add(1000*time.Hour), 100)
		expZeroRand(p)
		return p
	}

	off := setup()
	off.SetPreferExpiring(false, 24*time.Hour)
	if got := off.Pick(); got == nil || got.UID != "huge" {
		t.Fatalf("prefer=false pick=%v want huge（完全退回旧加权随机：credits 主导）", got)
	}

	zeroWin := setup()
	zeroWin.SetPreferExpiring(true, 0) // window<=0 = 关
	if got := zeroWin.Pick(); got == nil || got.UID != "huge" {
		t.Fatalf("window=0 pick=%v want huge（窗口 0 = 关，完全退回）", got)
	}

	negWin := setup()
	negWin.SetPreferExpiring(true, -time.Hour) // 负窗口按 0 处理
	if got := negWin.Pick(); got == nil || got.UID != "huge" {
		t.Fatalf("window<0 pick=%v want huge（负窗口 = 关）", got)
	}

	on := setup()
	on.SetPreferExpiring(true, 24*time.Hour)
	if got := on.Pick(); got == nil || got.UID != "small" {
		t.Fatalf("prefer=true pick=%v want small（最早到期优先，压过 credits 差距）", got)
	}
}

// TestPickExpiringPriorityEmptyFallsBackToWeighted 优先集为空 → 退回加权随机。
// far 带 stale 的窗内分桶（creditsExpiring=5）但到期在 72h 外：窗口二次门槛必须把它
// 排除（否则窗口改小后旧快照会继续插队）。
func TestPickExpiringPriorityEmptyFallsBackToWeighted(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "far"})
	p.Add(&auth.Auth{UID: "noexp"})
	p.Add(&auth.Auth{UID: "zeroBal"})
	now := time.Now()
	p.SetCreditsDetailedWithExpiry("far", 50, 0, 5, now.Add(72*time.Hour), 50) // 窗外 + stale 分桶
	p.SetCreditsDetailed("noexp", 100, 0, 0)                                   // 无到期时间
	p.SetCreditsDetailedWithExpiry("zeroBal", 0, 0, 0, now.Add(time.Hour), 0)  // 零余额
	expZeroRand(p)
	p.SetPreferExpiring(true, 24*time.Hour)

	// 优先集为空 → 加权随机取权重最高者 = noexp（credits 100）。
	if got := p.Pick(); got == nil || got.UID != "noexp" {
		t.Fatalf("pick=%v want noexp（优先集空 → 退回加权随机）", got)
	}
}

// TestPickExpiringExcludesExpiredZeroRemainingZeroCreditsNoExpiring 优先集四道闸门：
// 已过期 / 批次剩余为 0 / 零余额 / 窗口内无快过期积分（creditsExpiring==0）——
// 四类脏快照（余额都远大于 ok）一律不得进优先集。绕过 setter 的清洗直改 entry，
// 才能真正压到 pick 侧判据。
func TestPickExpiringExcludesExpiredZeroRemainingZeroCreditsNoExpiring(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	for _, uid := range []string{"expired", "emptyBatch", "noCredits", "noExpiring", "ok"} {
		p.Add(&auth.Auth{UID: uid})
	}
	now := time.Now()
	p.SetCreditsDetailedWithExpiry("ok", 10, 0, 10, now.Add(time.Hour), 10)
	expZeroRand(p)

	p.mu.Lock()
	e := p.byUID["expired"]
	e.credits, e.creditsExpiring, e.creditsEarliestRemaining = 9999, 9999, 9999
	e.creditsEarliestExpiry = now.Add(-time.Hour) // 已过期
	e = p.byUID["emptyBatch"]
	e.credits, e.creditsExpiring = 9999, 9999
	e.creditsEarliestExpiry, e.creditsEarliestRemaining = now.Add(time.Hour), 0 // 批次打空
	e = p.byUID["noCredits"]
	e.credits, e.creditsExpiring, e.creditsEarliestRemaining = 0, 5, 5
	e.creditsEarliestExpiry = now.Add(time.Hour) // 零余额
	e = p.byUID["noExpiring"]
	e.credits, e.creditsExpiring, e.creditsEarliestRemaining = 9999, 0, 5
	e.creditsEarliestExpiry = now.Add(time.Hour) // 窗口内无快过期积分
	p.mu.Unlock()

	p.SetPreferExpiring(true, 24*time.Hour)
	for i := 0; i < 3; i++ {
		got := p.Pick()
		if got == nil || got.UID != "ok" {
			t.Fatalf("第 %d 次 pick=%v want ok（四类无效快照都不得进优先集）", i, got)
		}
	}
}

// TestPickExpiringRotatesWithinPrioritySet 优先集内也要防并发撞号：第一个刚被用过时
// 顺延到第二个；全部刚用过才取排序首位（不再把所有并发请求硬撞到同一个号）。
func TestPickExpiringRotatesWithinPrioritySet(t *testing.T) {
	old := minPickGap
	minPickGap = time.Hour // 超大窗口：任何 lastUsed 都算「刚用过」
	t.Cleanup(func() { minPickGap = old })

	p := New("")
	p.Add(&auth.Auth{UID: "first"})
	p.Add(&auth.Auth{UID: "second"})
	now := time.Now()
	p.SetCreditsDetailedWithExpiry("first", 100, 0, 100, now.Add(time.Hour), 100)
	p.SetCreditsDetailedWithExpiry("second", 100, 0, 100, now.Add(2*time.Hour), 100)
	p.SetPreferExpiring(true, 24*time.Hour)

	if got := p.Pick(); got == nil || got.UID != "first" {
		t.Fatalf("第一次 pick=%v want first", got)
	}
	if got := p.Pick(); got == nil || got.UID != "second" {
		t.Fatalf("紧接第二次 pick=%v want second（minPickGap 内轮换，不硬撞同号）", got)
	}
	if got := p.Pick(); got == nil || got.UID != "first" {
		t.Fatalf("第三次 pick=%v want first（优先集全在 gap 内 → 取排序首位）", got)
	}
}

// TestSetPreferExpiringHotChangeTakesEffectImmediately 热改即时生效（无需重建池）。
func TestSetPreferExpiringHotChangeTakesEffectImmediately(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "small"})
	p.Add(&auth.Auth{UID: "huge"})
	now := time.Now()
	p.SetCreditsDetailedWithExpiry("small", 100, 0, 100, now.Add(time.Hour), 100)
	p.SetCreditsDetailedWithExpiry("huge", 100000, 0, 0, now.Add(1000*time.Hour), 100)
	expZeroRand(p)

	if got := p.Pick(); got == nil || got.UID != "huge" {
		t.Fatalf("默认（窗口 0）pick=%v want huge", got)
	}
	p.SetPreferExpiring(true, 24*time.Hour) // 热开
	if got := p.Pick(); got == nil || got.UID != "small" {
		t.Fatalf("热开后 pick=%v want small", got)
	}
	p.SetPreferExpiring(false, 24*time.Hour) // 热关
	if got := p.Pick(); got == nil || got.UID != "huge" {
		t.Fatalf("热关后 pick=%v want huge", got)
	}
}

// TestSetCreditsDetailedWithExpiryClampsAndClears 写入口的钳制/清洗口径。
func TestSetCreditsDetailedWithExpiryClampsAndClears(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()

	// 超总余额的脏数据：expiring / earliestRemaining 都钳到 credits。
	at := now.Add(time.Hour)
	p.SetCreditsDetailedWithExpiry("u1", 100, 0, 9999, at, 9999)
	expRead(t, p, "u1", func(e *entry) {
		if e.creditsExpiring != 100 || e.creditsEarliestRemaining != 100 {
			t.Errorf("越界钳制失败: expiring=%d earliest=%d want 100/100", e.creditsExpiring, e.creditsEarliestRemaining)
		}
		if !e.creditsEarliestExpiry.Equal(at) {
			t.Errorf("有效未来到期时刻应原样保留: %v want %v", e.creditsEarliestExpiry, at)
		}
	})

	// 已过期 → 整对清空（不清 expiry 就会被路由当成有效批次）。
	p.SetCreditsDetailedWithExpiry("u1", 100, 0, 10, now.Add(-time.Hour), 10)
	expRead(t, p, "u1", func(e *entry) {
		if !e.creditsEarliestExpiry.IsZero() || e.creditsEarliestRemaining != 0 {
			t.Errorf("已过期批次应清空: %v/%d", e.creditsEarliestExpiry, e.creditsEarliestRemaining)
		}
	})

	// 零剩余批次 → 清空。
	p.SetCreditsDetailedWithExpiry("u1", 100, 0, 10, now.Add(time.Hour), 0)
	expRead(t, p, "u1", func(e *entry) {
		if !e.creditsEarliestExpiry.IsZero() || e.creditsEarliestRemaining != 0 {
			t.Errorf("零剩余批次应清空: %v/%d", e.creditsEarliestExpiry, e.creditsEarliestRemaining)
		}
	})

	// 负值脏数据：credits / expiring / earliestRemaining 全部钳 0。
	p.SetCreditsDetailedWithExpiry("u1", -50, 0, -5, now.Add(time.Hour), -7)
	expRead(t, p, "u1", func(e *entry) {
		if e.credits != 0 || e.creditsExpiring != 0 || e.creditsEarliestRemaining != 0 {
			t.Errorf("负值钳制失败: credits=%d expiring=%d earliest=%d want 0/0/0",
				e.credits, e.creditsExpiring, e.creditsEarliestRemaining)
		}
	})
}

// TestSetCreditsDetailedLegacyEntryClearsEarliestKeepsExpiring 兼容入口（4 参）：
// 不带到期明细 → 清空最早批次快照（不得沿用旧批次路由），但按本 fork 既有口径
// **不动** creditsExpiring（见 transition.go 的不对称根因注释与 ×8 权重项）。
func TestSetCreditsDetailedLegacyEntryClearsEarliestKeepsExpiring(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailedWithExpiry("u1", 100, 0, 60, time.Now().Add(time.Hour), 60)
	expRead(t, p, "u1", func(e *entry) {
		if e.creditsEarliestExpiry.IsZero() {
			t.Fatal("前置构造失败：最早批次快照未写入")
		}
	})

	p.SetCreditsDetailed("u1", 100, 0, 60)
	expRead(t, p, "u1", func(e *entry) {
		if !e.creditsEarliestExpiry.IsZero() || e.creditsEarliestRemaining != 0 {
			t.Errorf("兼容入口应清空最早批次快照: %v/%d", e.creditsEarliestExpiry, e.creditsEarliestRemaining)
		}
		if e.creditsExpiring != 60 {
			t.Errorf("creditsExpiring=%d want 60（既有口径：兼容入口不动快过期桶）", e.creditsExpiring)
		}
	})
}

// TestReenableIfCreditsClearsEarliestSnapshot 该入口只有聚合余额上下文 → 清空最早
// 批次快照（防「权威余额已变、明细未更新」的旧快照参与路由）；同样不动 creditsExpiring。
func TestReenableIfCreditsClearsEarliestSnapshot(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailedWithExpiry("u1", 100, 0, 60, time.Now().Add(time.Hour), 60)

	p.ReenableIfCredits("u1", 200, 300)
	expRead(t, p, "u1", func(e *entry) {
		if !e.creditsEarliestExpiry.IsZero() || e.creditsEarliestRemaining != 0 {
			t.Errorf("应清空最早批次快照: %v/%d", e.creditsEarliestExpiry, e.creditsEarliestRemaining)
		}
		if e.creditsExpiring != 60 {
			t.Errorf("creditsExpiring=%d want 60（既有口径：本入口不动快过期桶）", e.creditsExpiring)
		}
		if e.credits != 200 || e.creditsTotal != 300 {
			t.Errorf("credits/total=%d/%d want 200/300", e.credits, e.creditsTotal)
		}
	})
}

// TestNoteModelCostDeductsEarliestRemaining 扣费同步收敛最早批次剩余量；批次打空即清
// 到期时刻（零剩余批次不再是有效批次）。独立扣减口径与 creditsExpiring 一致。
func TestNoteModelCostDeductsEarliestRemaining(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	at := time.Now().Add(time.Hour).Truncate(time.Second)
	p.SetCreditsDetailedWithExpiry("u1", 100, 0, 50, at, 50)

	p.NoteModelCost("u1", "m", 20, 1000) // 消耗 20
	expRead(t, p, "u1", func(e *entry) {
		if e.credits != 80 || e.creditsExpiring != 30 || e.creditsEarliestRemaining != 30 {
			t.Errorf("扣费未同步收敛: credits=%d expiring=%d earliest=%d want 80/30/30",
				e.credits, e.creditsExpiring, e.creditsEarliestRemaining)
		}
		if !e.creditsEarliestExpiry.Equal(at) {
			t.Errorf("批次未打空时到期时刻应保留: %v want %v", e.creditsEarliestExpiry, at)
		}
	})

	p.NoteModelCost("u1", "m", 100, 1000) // 打空
	expRead(t, p, "u1", func(e *entry) {
		if e.credits != 0 || e.creditsExpiring != 0 || e.creditsEarliestRemaining != 0 {
			t.Errorf("打空后应为 0/0/0: %d/%d/%d", e.credits, e.creditsExpiring, e.creditsEarliestRemaining)
		}
		if !e.creditsEarliestExpiry.IsZero() {
			t.Errorf("批次打空应清到期时刻: %v", e.creditsEarliestExpiry)
		}
	})
}

// TestExpiringSnapshotPersistRoundTrip 落盘/恢复往返无损（重启后首个请求仍可路由）。
func TestExpiringSnapshotPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := stateFilePath(t, dir)
	at := time.Now().Add(2 * time.Hour).Truncate(time.Second)

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailedWithExpiry("u1", 100, 0, 60, at, 60)
	p.Flush()
	p.Close()

	p2 := New(fp)
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "u1"})
	expRead(t, p2, "u1", func(e *entry) {
		if !e.creditsEarliestExpiry.Equal(at) {
			t.Errorf("恢复的 creditsEarliestExpiry=%v want %v", e.creditsEarliestExpiry, at)
		}
		if e.creditsEarliestRemaining != 60 {
			t.Errorf("恢复的 creditsEarliestRemaining=%d want 60", e.creditsEarliestRemaining)
		}
		if e.creditsExpiring != 60 {
			t.Errorf("恢复的 creditsExpiring=%d want 60", e.creditsExpiring)
		}
	})
}

// TestRestoreDropsExpiredExpiringSnapshot 恢复侧惰性清洗：已过期快照不复活
// （否则重启后会拿一个早就过期的批次参与最早到期路由）。
func TestRestoreDropsExpiredExpiringSnapshot(t *testing.T) {
	dir := t.TempDir()
	fp := stateFilePath(t, dir)
	past := time.Now().Add(-time.Hour).Format(time.RFC3339Nano)
	writeState(t, fp, `{"accounts":{"u1":{"credits":100,"credits_expiring":50,`+
		`"credits_earliest_expiry":"`+past+`","credits_earliest_remaining":50}}}`)

	p := New(fp)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	expRead(t, p, "u1", func(e *entry) {
		if !e.creditsEarliestExpiry.IsZero() || e.creditsEarliestRemaining != 0 {
			t.Errorf("已过期快照不得恢复: %v/%d", e.creditsEarliestExpiry, e.creditsEarliestRemaining)
		}
		if e.creditsExpiring != 50 {
			t.Errorf("creditsExpiring=%d want 50（既有恢复口径不受影响）", e.creditsExpiring)
		}
	})
}

// TestRestoreDropsDirtyExpiringSnapshot 恢复侧脏数据防御：剩余量超总余额（手工编辑
// state.json）不得恢复，否则路由会看到虚高的批次剩余量。
func TestRestoreDropsDirtyExpiringSnapshot(t *testing.T) {
	dir := t.TempDir()
	fp := stateFilePath(t, dir)
	future := time.Now().Add(2 * time.Hour).Format(time.RFC3339Nano)
	writeState(t, fp, `{"accounts":{"u1":{"credits":100,`+
		`"credits_earliest_expiry":"`+future+`","credits_earliest_remaining":9999}}}`)

	p := New(fp)
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1"})
	expRead(t, p, "u1", func(e *entry) {
		if !e.creditsEarliestExpiry.IsZero() || e.creditsEarliestRemaining != 0 {
			t.Errorf("超总余额的脏快照不得恢复: %v/%d", e.creditsEarliestExpiry, e.creditsEarliestRemaining)
		}
	})
}

// TestStatusExposesExpiringSnapshot 到期压力台账透出（运维可自查「为什么选了它」）。
func TestStatusExposesExpiringSnapshot(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	at := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	p.SetCreditsDetailedWithExpiry("u1", 100, 0, 60, at, 60)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status not found")
	}
	if st.CreditsExpiring != 60 || st.CreditsEarliestRemaining != 60 {
		t.Errorf("status 到期台账=%d/%d want 60/60", st.CreditsExpiring, st.CreditsEarliestRemaining)
	}
	if st.CreditsEarliestExpiry == nil || !st.CreditsEarliestExpiry.Equal(at) {
		t.Errorf("status credits_earliest_expiry=%v want %v（指针非 nil）", st.CreditsEarliestExpiry, at)
	}
}

// TestStatusOmitsZeroExpiringSnapshot 零值省略：未采集到快照时 /status JSON 形状不变
// （零回归——面板/监控的既有解析不受新字段影响）。
func TestStatusOmitsZeroExpiringSnapshot(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100, 0)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status not found")
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	for _, key := range []string{"credits_expiring", "credits_earliest_expiry", "credits_earliest_remaining"} {
		if strings.Contains(string(b), key) {
			t.Errorf("零值应被 omitempty 省略，但 JSON 含 %q: %s", key, b)
		}
	}
}
