// family_stagger_test.go 同槽位多任务族的**启动错开**回归。
//
// 缺陷：runBatch 给每个任务族各起一个 goroutine 并发执行，而每个族都是「逐账号 +
// activityAccountDelay(800ms) 限速」的**同一节奏**——同时启动会锁步推进，导致每个
// 账号在同一瞬间被两个族各打一次。默认配置 checkin_hours 与 travel_hours 都是
// [9,21]，即**默认就在 9 点与 21 点各发生一次系统性 2× 瞬时并发**（稳定复现，不是
// 随机抖动），正是本项目 WAF 403 / 边缘层 fail-fast 一直在防的形态。
//
// 闸门：多族批量必须体现出按 idx 递增的错开延迟；单族不得被无谓拖延；
// ctx 取消必须能提前终止错开等待（优雅停机不必等睡醒）。
package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// newEmptyScheduler 空池调度器：各族都不会做实际工作，从而只测「启动错开」。
func newEmptyScheduler() *Scheduler {
	return New(Config{Pool: pool.New("")})
}

// TestRunBatchStaggersTaskFamilies 多族按 idx 递增错开；单族无延迟。
// 反事实：去掉 runBatch 里的错开等待时，第二条断言必红（三族耗时≈0 < 2×stagger）。
func TestRunBatchStaggersTaskFamilies(t *testing.T) {
	old := familyStagger
	t.Cleanup(func() { familyStagger = old })
	familyStagger = 80 * time.Millisecond

	s := newEmptyScheduler()

	// 单族：idx==0 不应引入任何错开延迟（否则每个整点都被无谓拖慢）。
	start := time.Now()
	s.runBatch(context.Background(), []taskKind{taskCheckin})
	if el := time.Since(start); el > familyStagger/2 {
		t.Errorf("单族不应有错开延迟，实际 %v（stagger=%v）", el, familyStagger)
	}

	// 三族：第 2、3 族分别等待 1×、2× stagger（并发执行，故总耗时 ≈ 2×stagger）。
	start = time.Now()
	s.runBatch(context.Background(), []taskKind{taskCheckin, taskTravel, taskKeepalive})
	el := time.Since(start)
	if el < 2*familyStagger {
		t.Errorf("三族总耗时 %v < %v：任务族未被错开——各族的 800ms 节奏一致，会锁步"+
			"让同一账号在同一瞬间被多个族各打一次", el, 2*familyStagger)
	}
}

// TestRunBatchStaggerIsPerIndexNotConstant 错开量随 idx 线性增长（而非固定一次
// 延迟）：用 4 族验证总耗时 ≥ 3×stagger。固定延迟的实现只能满足「≥1×stagger」，
// 这条能把它区分出来。
func TestRunBatchStaggerIsPerIndexNotConstant(t *testing.T) {
	old := familyStagger
	t.Cleanup(func() { familyStagger = old })
	familyStagger = 60 * time.Millisecond

	s := newEmptyScheduler()
	start := time.Now()
	s.runBatch(context.Background(), []taskKind{taskCheckin, taskTravel, taskKeepalive, taskActivity})
	if el := time.Since(start); el < 3*familyStagger {
		t.Errorf("4 族总耗时 %v < %v：错开量未随 idx 增长（固定延迟会让后两个族仍与前者重叠）",
			el, 3*familyStagger)
	}
}

// TestRunBatchStaggerRespectsContextCancel ctx 取消必须能提前结束错开等待：
// 优雅停机时不该为了等错开而卡住数秒。
func TestRunBatchStaggerRespectsContextCancel(t *testing.T) {
	old := familyStagger
	t.Cleanup(func() { familyStagger = old })
	familyStagger = 5 * time.Second // 故意很长：只能靠 ctx 取消提前返回

	s := newEmptyScheduler()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	s.runBatch(ctx, []taskKind{taskCheckin, taskTravel})
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("ctx 取消后不应等满错开延迟（实际 %v，stagger=%v）：优雅停机被错开等待拖住",
			el, familyStagger)
	}
}
