// growth_test.go 成长任务队列「每日自动执行」的排程回归。
//
// 缺陷/需求：Sequential 小程序任务族每日零点解锁一环，此前只能人工点面板的
// 「执行全部待办」；第二天不会自动继续 → 用户必须每天手动点一次，否则任务链停在
// 原地（少拿积分）。本族的排程必须接进既有 nextWake/nextFire/runBatch 体系（不另起
// 定时器），到点回调面板的 RunGrowthQueueOnce（SetGrowthHook 接线）。
//
// 闸门：GrowthHours 命中产生候选、GrowthDisabled 不产生候选；到点 hook 恰好被调用
// 一次；ctx 取消能停止（优雅停机不等宽限/不重复触发）；未接线（nil hook）不 panic。
package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// growthOnlyScheduler 只留成长任务族的调度器（其余各族时点与开关都关掉），
// 让 nextWake 的断言只反映 growth 的行为。
func growthOnlyScheduler(hours []int, disabled bool) *Scheduler {
	return New(Config{
		GrowthHours:       hours,
		GrowthDisabled:    disabled,
		CheckinDisabled:   true,
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		BlackcatDisabled:  true,
	})
}

// TestNextWakeGrowthDefaultSlot 默认 GrowthHours=[1]：到点产生 growth 候选。
// 反事实：把 nextWake 里的 growth 分支去掉时本用例必红（最近时点会落回其他族）。
func TestNextWakeGrowthDefaultSlot(t *testing.T) {
	// 只留 growth，其余禁用 —— 默认 hours 在 New 内回落。
	s := growthOnlyScheduler(nil, false)
	at, kinds := s.nextWake(time.Date(2026, 9, 30, 0, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 30, 1, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Fatalf("next=%v want %v（默认 01:00，刻意避开零点解锁竞态）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskGrowth {
		t.Fatalf("kinds=%v want [growth]", kinds)
	}

	// 已过 1 点：下一次是次日 01:00（每日一环）。
	at, kinds = s.nextWake(time.Date(2026, 9, 30, 2, 0, 0, 0, time.Local))
	if want := time.Date(2026, 10, 1, 1, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Fatalf("next=%v want %v（每日一次）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskGrowth {
		t.Fatalf("kinds=%v want [growth]", kinds)
	}
}

// TestNextWakeGrowthHoursCustom 自定义多时点：取最近的一个。
func TestNextWakeGrowthHoursCustom(t *testing.T) {
	s := growthOnlyScheduler([]int{2, 14}, false)
	at, kinds := s.nextWake(time.Date(2026, 9, 30, 0, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 30, 2, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Fatalf("next=%v want %v（02:00 早于 14:00）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskGrowth {
		t.Fatalf("kinds=%v want [growth]", kinds)
	}

	at, _ = s.nextWake(time.Date(2026, 9, 30, 15, 0, 0, 0, time.Local))
	if want := time.Date(2026, 10, 1, 2, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Fatalf("next=%v want %v（15:00 后最近的是次日 02:00）", at, want)
	}
}

// TestNextWakeGrowthDisabledNoCandidate 禁用后不产生任何候选（nextWake 零值）。
// 反事实：去掉 nextWake 里 `if !growthOff` 的判断时本用例必红。
func TestNextWakeGrowthDisabledNoCandidate(t *testing.T) {
	s := growthOnlyScheduler([]int{1, 2, 3}, true)
	at, kinds := s.nextWake(time.Date(2026, 9, 30, 0, 30, 0, 0, time.Local))
	if !at.IsZero() || len(kinds) != 0 {
		t.Fatalf("at=%v kinds=%v want zero/nil（growth 已禁用，不应产生候选）", at, kinds)
	}
}

// TestNextWakeGrowthSharesSlotIsStaggered 与签到配到同一小时时两族都要执行，
// 且 growth 排在末位（runBatch 的 idx：错开 1×familyStagger）——新族自动受益于
// 既有的按族错开，不与签到锁步打同一账号。
func TestNextWakeGrowthSharesSlotIsStaggered(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		GrowthHours:       []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		BlackcatDisabled:  true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 30, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 30, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Fatalf("next=%v want %v", at, want)
	}
	if len(kinds) != 2 || kinds[0] != taskCheckin || kinds[1] != taskGrowth {
		t.Fatalf("kinds=%v want [checkin growth]（growth 在末位 → 错开量最大）", kinds)
	}
}

// TestSetGrowthScheduleHotUpdate 面板保存配置的热更新路径：改时点/开关后
// 立即重算排程，并 poke 一次 rearmSchedule（Run 在等待中被唤醒重算）。
func TestSetGrowthScheduleHotUpdate(t *testing.T) {
	s := growthOnlyScheduler(nil, true) // 先禁用
	now := time.Date(2026, 9, 30, 0, 30, 0, 0, time.Local)
	if at, _ := s.nextWake(now); !at.IsZero() {
		t.Fatalf("禁用时不应有候选，got %v", at)
	}

	s.SetGrowthSchedule([]int{3}, false)
	at, kinds := s.nextWake(now)
	if want := time.Date(2026, 9, 30, 3, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Fatalf("热改后 next=%v want %v", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskGrowth {
		t.Fatalf("kinds=%v want [growth]", kinds)
	}
	// 空 hours = 未配置：保留原值（与 Reconfigure/config.normalize 的回落语义一致）。
	s.SetGrowthSchedule(nil, false)
	at, _ = s.nextWake(now)
	if want := time.Date(2026, 9, 30, 3, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Fatalf("空 hours 应保留原值，next=%v want %v", at, want)
	}
	select {
	case <-s.rearmSchedule:
	default:
		t.Error("SetGrowthSchedule 必须 poke rearmSchedule，否则运行中的 Run 不会重算时点")
	}

	s.SetGrowthSchedule([]int{4}, true)
	if at, _ := s.nextWake(now); !at.IsZero() {
		t.Fatalf("再次禁用后不应有候选，got %v", at)
	}
}

// TestRunBatchGrowthInvokesHookOnce 到点派发：runBatch 的 growth 分支恰好回调
// hook 一次（与 Run 到点走的是同一条派发路径）。
func TestRunBatchGrowthInvokesHookOnce(t *testing.T) {
	s := New(Config{Pool: pool.New("")})
	var calls atomic.Int32
	s.SetGrowthHook(func(context.Context) { calls.Add(1) })

	s.runBatch(context.Background(), []taskKind{taskGrowth})
	if got := calls.Load(); got != 1 {
		t.Fatalf("hook 调用次数=%d want 1", got)
	}
}

// TestRunBatchGrowthNilHookNoPanic 未接线（cmd/server 忘了 SetGrowthHook）时到点
// 不能 panic，只落一行日志跳过。
func TestRunBatchGrowthNilHookNoPanic(t *testing.T) {
	s := New(Config{Pool: pool.New("")})
	s.runBatch(context.Background(), []taskKind{taskGrowth})
}

// TestRunBatchGrowthHookStopsOnContextCancel ctx 取消能停止：hook 内等待 ctx 时，
// 取消后 runBatch 必须返回，且 hook 不会被第二次调用。
func TestRunBatchGrowthHookStopsOnContextCancel(t *testing.T) {
	s := New(Config{Pool: pool.New("")})
	entered := make(chan struct{})
	var calls atomic.Int32
	s.SetGrowthHook(func(ctx context.Context) {
		calls.Add(1)
		close(entered)
		<-ctx.Done() // 队列在项间收尾的形态
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.runBatch(ctx, []taskKind{taskGrowth})
		close(done)
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("hook 未被调用")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 runBatch 未停止")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("hook 调用次数=%d want 1", got)
	}
}

// TestRunFiresGrowthHookOnceThenStopsOnCancel 端到端（Run → nextWake → waitSlot →
// runBatch → hook）：到点调用 hook 一次；hook 内取消 ctx 后 Run 立即退出，不会
// 再派发第二轮。
//
// schedNow 钉在槽位前一小时、wallclockNow 越过槽位：nextFire 是整点粒度，真实
// 时钟下验证「凌晨 1 点真的动了」最坏要等 59 分钟。日期取远未来使 awaitWakeupGrace
// 的迟到判据为负（机器没在睡眠），避开真实 5s 宽限等待。
func TestRunFiresGrowthHookOnceThenStopsOnCancel(t *testing.T) {
	oldNow, oldWall := schedNow, wallclockNow
	t.Cleanup(func() { schedNow, wallclockNow = oldNow, oldWall })

	base := time.Date(2099, 9, 30, 0, 30, 0, 0, time.Local) // 默认 growth [1] 的下一槽位 = 当天 01:00
	slot := time.Date(2099, 9, 30, 1, 0, 0, 0, time.Local)
	schedNow = func() time.Time { return base }
	wallclockNow = func() time.Time { return slot.Add(time.Second) } // 墙钟已越过槽位 → 立即 slotFired

	s := growthOnlyScheduler(nil, false)
	var calls atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.SetGrowthHook(func(hookCtx context.Context) {
		calls.Add(1)
		cancel() // 本次自动执行完成 → 请求停机
	})

	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run 未在 hook 取消 ctx 后返回（ctx 取消不能停止自动执行）")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("growth hook 调用次数=%d want 1（到点只派发一次；重复启动由面板占位兜底）", got)
	}
}

// TestRunGrowthDisabledNeverCallsHook 禁用时即使墙钟/排程都指向槽位也不调用 hook：
// 无可唤醒时点 → Run 只等退出信号（不空转）。
func TestRunGrowthDisabledNeverCallsHook(t *testing.T) {
	oldNow := schedNow
	t.Cleanup(func() { schedNow = oldNow })
	schedNow = func() time.Time { return time.Date(2099, 9, 30, 0, 30, 0, 0, time.Local) }

	s := growthOnlyScheduler([]int{1}, true)
	var calls atomic.Int32
	s.SetGrowthHook(func(context.Context) { calls.Add(1) })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s.Run(ctx) // 无可唤醒时点：阻塞到 ctx 取消
	if got := calls.Load(); got != 0 {
		t.Errorf("growth 已禁用，hook 调用次数=%d want 0", got)
	}
}
