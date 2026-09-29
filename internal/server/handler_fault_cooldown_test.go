// handler_fault_cooldown_test.go 14017（trial not activated）账号故障冷却的
// **热改生效**回归（upstream 5f6c7ca 的 server 部分）。
//
// 缺陷：该分支读的是 h.cfg.SoftCooldown（进程启动时快照），而 429/6004 路径读
// h.softCooldown()（Live 热改优先）→ 面板里把 soft_rate 从 600s 改成 90s 后，
// 429 立即按 90s 冷却，14017 却仍按启动值 600s 罚号，同一条「软冷却」两套口径。
//
// 修复：统一走 h.softCooldown()。下面第一条用例即反事实闸门——回退为
// h.cfg.SoftCooldown 时冷却剩余会是 600s 而非 90s，用例必红。
package server

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 14017 真实响应形态（上游账号故障信封；无 "request illegal" 文案 → 走软冷却）。
const body14017 = `{"code":14017,"msg":"trial not activated, please complete register"}`

// TestApplyErrorPolicy14017UsesHotSoftCooldown 热改值必须立即生效：
// 启动值 600s + Live 快照 90s → 冷却剩余约 90s（而非 600s）。
func TestApplyErrorPolicy14017UsesHotSoftCooldown(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	live := livecfg.New(livecfg.Snapshot{SoftCooldown: 90 * time.Second})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second, Live: live})

	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body14017, "", nil)

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号不在池中")
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("14017 应为 soft_rate 软冷却: %+v", st)
	}
	// 90s 热改值（留 3s 调度余量）。启动值 600s 会使此断言失败——这就是闸门。
	if st.CoolRemaining < 87 || st.CoolRemaining > 90 {
		t.Errorf("14017 冷却剩余 %ds, want ~90s（热改 soft_rate 未走 h.softCooldown()，回退到了启动值 %s）",
			st.CoolRemaining, 600*time.Second)
	}
}

// TestApplyErrorPolicy14017HotChangeBetweenCalls 面板在线改写后，下一次 14017
// 立即按新值冷却（同进程两次调用上界不同）。
func TestApplyErrorPolicy14017HotChangeBetweenCalls(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	live := livecfg.New(livecfg.Snapshot{SoftCooldown: 600 * time.Second})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second, Live: live})

	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body14017, "", nil)
	if st, _ := p.Status("u1"); st.CoolRemaining < 597 {
		t.Fatalf("首次（600s）冷却剩余 %ds, want ~600s", st.CoolRemaining)
	}
	// 面板改成 30s；冷却中的重复触发不缩短，故先解冻再触发。
	p.Revive("u1") // 只清冷却，模拟冷却自然到期
	live.Store(livecfg.Snapshot{SoftCooldown: 30 * time.Second})

	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body14017, "", nil)
	st, _ := p.Status("u1")
	if st.CoolRemaining < 27 || st.CoolRemaining > 30 {
		t.Errorf("热改后 14017 冷却剩余 %ds, want ~30s", st.CoolRemaining)
	}
}

// TestApplyErrorPolicy14017FallsBackToStaticWithoutLive Live 缺失（测试/裸用场景）
// 时回退静态 SoftCooldown；Live 里 soft_rate <= 0 时同样回退（softCooldown 语义）。
func TestApplyErrorPolicy14017FallsBackToStaticWithoutLive(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 45 * time.Second}) // Live 为 nil

	h.applyErrorPolicy("u1", upstream.ErrAccountFault, body14017, "", nil)
	st, _ := p.Status("u1")
	if st.CoolRemaining < 42 || st.CoolRemaining > 45 {
		t.Errorf("Live 缺失时应回退静态 soft_rate 45s, got %ds", st.CoolRemaining)
	}

	p2 := pool.New("")
	p2.Add(&auth.Auth{UID: "u2"})
	h2 := NewHandler(Config{
		Pool: p2, SoftCooldown: 50 * time.Second,
		Live: livecfg.New(livecfg.Snapshot{SoftCooldown: 0}), // <=0 → 回退静态
	})
	h2.applyErrorPolicy("u2", upstream.ErrAccountFault, body14017, "", nil)
	st2, _ := p2.Status("u2")
	if st2.CoolRemaining < 47 || st2.CoolRemaining > 50 {
		t.Errorf("Live soft_rate<=0 时应回退静态 50s, got %ds", st2.CoolRemaining)
	}
}

// TestApplyErrorPolicy11140StillHardDisables 同一分支的 11140 硬禁用语义未被
// 本次改动波及（"request illegal" 优先判、直接 Disable 不给软冷却）。
func TestApplyErrorPolicy11140StillHardDisables(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	h.applyErrorPolicy("u1", upstream.ErrAccountFault,
		`{"code":11140,"msg":"request illegal"}`, "", nil)

	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("11140 request illegal 应硬禁用: %+v", st)
	}
	if st.Cooling {
		t.Errorf("硬禁用不应同时挂软冷却: %+v", st)
	}
}
