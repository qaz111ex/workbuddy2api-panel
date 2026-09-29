// pool_bugfix_test.go 锁定 upstream 5f6c7ca 的 pool 侧修复（手工适配本仓库结构）：
//   - NoteModelCost 快过期桶独立扣减 + 脏数据（负余额）不得反向膨胀桶/余额；
//   - PickByUID/PickByUIDForModel 粘性命中推进 pickSeq/usedSeq（PickByUIDForModel
//     由 weight_optimization_test.go:TestStickyPickAdvancesUsedSeq 锁定，本文件补
//     PickByUID 侧）；
//   - statusOf 冷却剩余取 until/breakerUntil 中仍在未来且更晚者；
//   - modelExempt 排除账号级不可用期（until/degradeUntil）。
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ---------------------------------------------------------------------------
// NoteModelCost：扣减口径（账务）
// ---------------------------------------------------------------------------

// TestNoteModelCostDecrementsBothBucketsIndependently credits 与 creditsExpiring
// 各自按本次消耗独立扣减，且快过期桶钳 0（不越界、不虚高 ×8 权重项）。
// 本仓库只有一个快过期桶（无 upstream 的 creditsEarliest* 最早到期批次），
// 故上游「复用同一 consume 变量逐级取 min → 两个桶都被少扣」的缺陷在本仓库
// 结构上不可达；本用例锁定等价修法后的账务口径。
func TestNoteModelCostDecrementsBothBucketsIndependently(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 100, 0, 80)

	p.NoteModelCost("u1", "m", 30, 1000) // d=30
	p.mu.RLock()
	c, x := p.byUID["u1"].credits, p.byUID["u1"].creditsExpiring
	p.mu.RUnlock()
	if c != 70 || x != 50 {
		t.Fatalf("首次扣费后 credits=%d expiring=%d want 70/50", c, x)
	}

	p.NoteModelCost("u1", "m", 60, 1000) // d=min(60,70)=60；快过期桶独立扣 min(60,50)=50
	p.mu.RLock()
	c, x = p.byUID["u1"].credits, p.byUID["u1"].creditsExpiring
	p.mu.RUnlock()
	if c != 10 || x != 0 {
		t.Fatalf("二次扣费后 credits=%d expiring=%d want 10/0（快过期桶钳 0）", c, x)
	}
}

// TestNoteModelCostNegativeCreditsDoesNotInflateExpiring 脏数据防御（有牙用例）：
// credits 为负（手工编辑/损坏的 state.json；persist 恢复侧只钳 expiring 不钳 credits，
// SetCredits 又有意不动 expiring——两条公共路径即可构造 credits<0 且 expiring>0）
// 时，旧实现 d = min(credit, credits) 为负 → credits 被"扣"成更大值、且
// creditsExpiring -= d 反向**膨胀**快过期桶（权重项虚高）。
// 扣减绝不能增加任何余额桶。
func TestNoteModelCostNegativeCreditsDoesNotInflateExpiring(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 100, 0, 20) // credits=100, expiring=20
	p.SetCredits("u1", -50, 0)             // 脏数据：负余额（SetCredits 有意不动 expiring）

	p.mu.RLock()
	cBefore, xBefore := p.byUID["u1"].credits, p.byUID["u1"].creditsExpiring
	p.mu.RUnlock()
	if cBefore != -50 || xBefore != 20 {
		t.Fatalf("前置构造失败：credits=%d expiring=%d want -50/20", cBefore, xBefore)
	}

	p.NoteModelCost("u1", "m", 10, 1000)

	p.mu.RLock()
	c, x := p.byUID["u1"].credits, p.byUID["u1"].creditsExpiring
	p.mu.RUnlock()
	if c > cBefore {
		t.Errorf("扣费不得增加余额：credits %d → %d", cBefore, c)
	}
	if x > xBefore {
		t.Errorf("扣费不得膨胀快过期桶：expiring %d → %d（旧实现会膨胀到 70）", xBefore, x)
	}
	if x < 0 {
		t.Errorf("creditsExpiring=%d 不得为负", x)
	}
	if x > c && c >= 0 {
		t.Errorf("creditsExpiring=%d 必须 ≤ credits=%d（快过期桶是子集）", x, c)
	}
}

// ---------------------------------------------------------------------------
// statusOf：冷却剩余（until / breakerUntil 只有其一在生效）
// ---------------------------------------------------------------------------

// TestStatusBreakerOnlyReportsRemainingAndKind 仅熔断期（until 为零）时不得误报
// 0 秒 / unknown——旧实现只看 until，time.Until(零值) 是大负数被钳成 0，
// cool_kind 取 coolKind.String()=unknown，运维看到"已到期"却选不到号。
func TestStatusBreakerOnlyReportsRemainingAndKind(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // 触发熔断：breakerUntil ≈ now+1h

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}
	if !st.Cooling {
		t.Fatalf("熔断期应判 Cooling: %+v", st)
	}
	if st.CoolKind != "breaker" {
		t.Errorf("cool_kind=%q want breaker（仅熔断期不得报 unknown）", st.CoolKind)
	}
	if st.CoolRemaining < 3500 || st.CoolRemaining > 3601 {
		t.Errorf("cool_remaining_sec=%d want ~3600（旧实现钳成 0）", st.CoolRemaining)
	}
}

// TestStatusCoolRemainingTakesLaterDeadline 两个截止同时存在时取更晚者，
// 且熔断更晚时 cool_kind 归为 breaker、账号级冷却更晚时保留原类别。
func TestStatusCoolRemainingTakesLaterDeadline(t *testing.T) {
	// 账号级冷却（1h）晚于熔断（5m）→ 取账号级剩余，kind 保留 soft_rate。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, 5*time.Minute, 5*time.Minute)
	p.NoteError("u1")
	p.Cooldown("u1", CoolSoft, time.Hour, "429")
	st, _ := p.Status("u1")
	if st.CoolKind != "soft_rate" {
		t.Errorf("账号级冷却更晚时 cool_kind=%q want soft_rate", st.CoolKind)
	}
	if st.CoolRemaining < 3500 || st.CoolRemaining > 3601 {
		t.Errorf("cool_remaining_sec=%d want ~3600（取更晚者）", st.CoolRemaining)
	}

	// 熔断（1h）晚于账号级冷却（5m）→ 取熔断剩余，kind=breaker。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u2"})
	p2.SetBreaker(1, time.Hour, time.Hour)
	p2.Cooldown("u2", CoolSoft, 5*time.Minute, "429")
	p2.NoteError("u2")
	st2, _ := p2.Status("u2")
	if st2.CoolKind != "breaker" {
		t.Errorf("熔断更晚时 cool_kind=%q want breaker", st2.CoolKind)
	}
	if st2.CoolRemaining < 3500 || st2.CoolRemaining > 3601 {
		t.Errorf("cool_remaining_sec=%d want ~3600（取熔断）", st2.CoolRemaining)
	}
}

// ---------------------------------------------------------------------------
// 粘性/直取命中推进序号（LRU 兜底不把重度号当"最旧"）
// ---------------------------------------------------------------------------

// TestPickByUIDAdvancesUsedSeq PickByUID（会话粘性直取）命中同样推进 pickSeq/usedSeq。
// PickByUIDForModel 侧由 weight_optimization_test.go:TestStickyPickAdvancesUsedSeq 锁定；
// 本用例补 PickByUID 侧——旧实现只更新 lastUsed，Windows 上 time.Now() 精度有限
// （~0.5ms）时多个账号 lastUsed 相等，LRU 兜底会把粘性重度号当"最旧"而集中流量。
func TestPickByUIDAdvancesUsedSeq(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	for i := 0; i < 3; i++ {
		if a := p.PickByUID("u1"); a == nil {
			t.Fatalf("PickByUID 第 %d 次返回 nil", i)
		}
	}
	p.mu.RLock()
	s1, s2 := p.byUID["u1"].usedSeq, p.byUID["u2"].usedSeq
	p.mu.RUnlock()
	if s1 <= s2 {
		t.Errorf("PickByUID 命中应推进 usedSeq: u1=%d 应高于从未选中的 u2=%d", s1, s2)
	}
}

// ---------------------------------------------------------------------------
// modelExempt：账号级不可用期与模型级豁免互斥
// ---------------------------------------------------------------------------

// TestModelExemptExcludesAccountLevelUnavailable 模型级豁免的前提是「账号本身可用，
// 只是某几个模型被限流」。旧实现只看 disabled 与 breakerUntil，账号正处账号级冷却
// （until）或连败降权（degradeUntil）时仍判豁免 → ServableForRealm 的
// healthy||modelExempt 或门把整体不可用的账号报成可服务（/healthz 200 而 chat 全 503）。
func TestModelExemptExcludesAccountLevelUnavailable(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, p *Pool, e *entry)
	}{
		{"account_cooldown_until", func(t *testing.T, p *Pool, e *entry) {
			// 走公共 API 的 Cooldown 会清 modelCooldowns（账号级冷却语义），
			// 这里要构造「台账 + until 并存」的形态，故直接置 until。
			p.mu.Lock()
			e.until = time.Now().Add(time.Hour)
			e.coolKind = CoolSoft
			p.mu.Unlock()
		}},
		{"degrade_until", func(t *testing.T, p *Pool, e *entry) {
			p.SetDegrade(1, time.Hour, time.Hour)
			p.NoteFailures("u1")
			if e.degradeUntil.IsZero() {
				t.Fatal("前置：连败降权应生效")
			}
		}},
		{"breaker_until", func(t *testing.T, p *Pool, e *entry) {
			p.SetBreaker(1, time.Hour, time.Hour)
			p.NoteError("u1")
			if e.breakerUntil.IsZero() {
				t.Fatal("前置：熔断应生效")
			}
		}},
		{"disabled", func(t *testing.T, p *Pool, e *entry) {
			p.mu.Lock()
			e.disabled = true
			p.mu.Unlock()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := New("")
			p.Add(&auth.Auth{UID: "u1"})
			p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.3", "6004")
			p.mu.RLock()
			e := p.byUID["u1"]
			p.mu.RUnlock()
			if !e.modelExempt() {
				t.Fatal("前置：仅模型级台账且账号级健康时应判豁免")
			}
			tc.mutate(t, p, e)
			if e.modelExempt() {
				t.Errorf("%s 生效期内不得判模型豁免（账号整体不可用）", tc.name)
			}
		})
	}
}

// TestServableFalseWhenOnlyAccountIsAccountLevelCooledWithModelLedger 行为面：
// 唯一账号处于账号级冷却、同时带 6004 模型级台账时，/healthz 口径（ServableNow /
// ServableForRealm）必须报不可服务——旧实现会因 modelExempt 误判为可服务。
func TestServableFalseWhenOnlyAccountIsAccountLevelCooledWithModelLedger(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429") // 账号级冷却（先，会清台账）
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.3", "6004")

	if p.ServableNow() {
		t.Error("账号级冷却中的唯一账号不得被报为可服务（模型豁免不成立）")
	}
	st, _ := p.Status("u1")
	if st.Realm == "" {
		t.Skip("无 realm 信息，跳过按域断言")
	}
	if p.ServableForRealm(st.Realm) {
		t.Errorf("ServableForRealm(%q) 同口径应报不可服务", st.Realm)
	}
}
