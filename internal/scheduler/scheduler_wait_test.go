// scheduler_wait_test.go 钉住 waitSlot 的三态与墙钟口径：timer 的等待基于单调
// 时钟，机器睡眠会冻结它——分段等待 + 每段墙钟重判把冻结的影响限制在一段之内
// （「睡眠不足整个等待周期时 fire 被顺延、墙钟时点被错过且不补跑」的正面修复）。
package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func newTestScheduler() *Scheduler {
	s := New(Config{})
	return s
}

// TestWaitSlotFiresAtWallclock 到点返回 slotFired：
//   - next 已过（墙钟已越过时点、timer 尚未设置的形态）→ 立即返回，不等下一段；
//   - next 未到且段长小于剩余（多段等待）→ 到点才返回，且不早于计划时刻；
//   - next 未到且段长远大于剩余（单段）→ 仍按剩余时长到点，不睡满段长。
func TestWaitSlotFiresAtWallclock(t *testing.T) {
	s := newTestScheduler()

	// 墙钟已越过的形态：修复目标——睡眠冻结 timer 后醒来第一件事就是发现已到点。
	past := time.Now().Add(-time.Second)
	if got := s.waitSlot(context.Background(), past, time.Hour); got != slotFired {
		t.Fatalf("已越过时点应立即 slotFired，got %v", got)
	}

	// 未来时点：多段等待到点。
	next := time.Now().Add(45 * time.Millisecond)
	start := time.Now()
	if got := s.waitSlot(context.Background(), next, 10*time.Millisecond); got != slotFired {
		t.Fatalf("到点应 slotFired，got %v", got)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Errorf("提前返回：等待 %v，未到计划时刻", time.Since(start))
	}

	// 段长远大于剩余：段长只是上限，不能把等待拉长到段长。
	next = time.Now().Add(30 * time.Millisecond)
	start = time.Now()
	if got := s.waitSlot(context.Background(), next, time.Hour); got != slotFired {
		t.Fatalf("到点应 slotFired，got %v", got)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("段长被当成等待时长：等待 %v，应约 30ms", elapsed)
	}
}

// TestWaitSlotRearm 等待中收到 rearmSchedule（在线改配置重排）→ 立即 slotRearm，
// 不等满剩余时长（段长不影响 rearm 响应性）。
func TestWaitSlotRearm(t *testing.T) {
	s := newTestScheduler()
	next := time.Now().Add(2 * time.Second)

	go func() {
		time.Sleep(30 * time.Millisecond)
		s.rearmSchedule <- struct{}{}
	}()
	start := time.Now()
	if got := s.waitSlot(context.Background(), next, 10*time.Millisecond); got != slotRearm {
		t.Fatalf("rearm 应返回 slotRearm，got %v", got)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("rearm 响应过慢：%v", time.Since(start))
	}
}

// TestWaitSlotCancel ctx 取消 → slotCancel，优雅退出。
func TestWaitSlotCancel(t *testing.T) {
	s := newTestScheduler()
	ctx, cancel := context.WithCancel(context.Background())
	next := time.Now().Add(2 * time.Second)

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if got := s.waitSlot(ctx, next, 10*time.Millisecond); got != slotCancel {
		t.Fatalf("取消应返回 slotCancel，got %v", got)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("取消响应过慢：%v", time.Since(start))
	}
}

// TestWaitSlotWallclockRecheckAfterFrozenMonotonic 钉住修复的核心机理：段计时器
// 被系统睡眠冻结（单调时钟不推进、墙钟照常前进）时，段末的**墙钟重判**能发现
// 剩余时间已变短并继续分段，最终仍在原计划墙钟时点补跑——不会像一次性 timer
// 那样把「睡眠时长」整段顺延到时点之后（越跑越晚）。
//
// 反事实验证：把 waitSlot 退化为「一次性等满 wallRemain 再 slotFired」时，本用例
// 的墙钟落点会变成 base+target+suspend（90s ≠ 60s）而失败。
func TestWaitSlotWallclockRecheckAfterFrozenMonotonic(t *testing.T) {
	s := newTestScheduler()

	base := time.Date(2026, 9, 28, 8, 0, 0, 0, time.Local)
	const (
		step    = 10 * time.Second
		target  = 60 * time.Second
		suspend = 30 * time.Second // 第 1 段内系统睡眠 30s：墙钟前进、单调冻结
	)
	next := base.Add(target)

	oldNow, oldSeg := wallclockNow, slotSegment
	defer func() { wallclockNow, slotSegment = oldNow, oldSeg }()

	fakeWall := base
	var segments int32
	wallclockNow = func() time.Time { return fakeWall }
	slotSegment = func(d time.Duration) (<-chan time.Time, func()) {
		n := atomic.AddInt32(&segments, 1)
		// 段计时器按**单调**时钟等满 d；睡眠只推进墙钟、不推进单调时钟，
		// 故这一段结束时墙钟已额外前进 suspend（d 之外的那部分被冻结吞掉）。
		fakeWall = fakeWall.Add(d)
		if n == 1 {
			fakeWall = fakeWall.Add(suspend)
		}
		ch := make(chan time.Time, 1)
		ch <- fakeWall
		return ch, func() {}
	}

	if got := s.waitSlot(context.Background(), next, step); got != slotFired {
		t.Fatalf("want slotFired, got %v", got)
	}
	if elapsed := fakeWall.Sub(base); elapsed != target {
		t.Errorf("墙钟落点 %v，want %v（一次性 timer 会把睡眠时长 %v 顺延进落点）",
			elapsed, target, suspend)
	}
	if n := atomic.LoadInt32(&segments); n < 2 {
		t.Errorf("应分段等待（segments=%d），单段无法在睡眠后重判墙钟", n)
	}
}

// TestWaitSlotNonPositiveStepDoesNotSpin step<=0 时若原样 clamp 会让每段 timer
// 立即到期而墙钟仍未到点 → 忙等空转；防御后回落 wallclockCheckStep，只等一段。
func TestWaitSlotNonPositiveStepDoesNotSpin(t *testing.T) {
	s := newTestScheduler()

	oldSeg := slotSegment
	defer func() { slotSegment = oldSeg }()

	var segments int32
	block := make(chan time.Time) // 永不发送：只有 ctx 取消能唤醒
	slotSegment = func(d time.Duration) (<-chan time.Time, func()) {
		atomic.AddInt32(&segments, 1)
		if d <= 0 {
			// 未防御的形态：d=0 → 立即到期 → 循环顶再判仍未到点 → 忙等。
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch, func() {}
		}
		return block, func() {}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if got := s.waitSlot(ctx, time.Now().Add(time.Hour), 0); got != slotCancel {
		t.Fatalf("want slotCancel, got %v", got)
	}
	if n := atomic.LoadInt32(&segments); n != 1 {
		t.Errorf("段数=%d want 1（step<=0 未防御会忙等空转）", n)
	}
}
