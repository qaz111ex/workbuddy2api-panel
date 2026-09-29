// taskcenter_growth_test.go 成长任务队列「每日自动执行」入口（RunGrowthQueueOnce）的回归。
//
// 需求：Sequential 小程序任务族每日零点解锁一环，此前只能人工点「执行全部待办」；
// scheduler 到点调用的 RunGrowthQueueOnce 必须与手动入口**完全同管线**（复用先占位 /
// 陈旧占位兜底 / 无待办回滚这套队列状态机），并且在「已在跑」/「无待办」时安全跳过。
//
// 闸门：重复触发不得重复启动（不得二次扫描/覆盖队列）；无待办不得留下「已启动」痕迹
// 且占位必须回滚；启动必须体现同一管线（并发 1、item 真被执行）；ctx 取消不启动。
package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	// growthQueuePendingTasks 一个可自动化的待办（chat_5 在 autoActions 内）。
	growthQueuePendingTasks = `{"code":0,"msg":"OK","data":{"tasks":[` +
		`{"task_code":"chat_5","title":"对话 5 次","target":5,"current":0,"accept_status":"accepted"}]}}`
	// growthQueueNoTasks 无待办的账号。
	growthQueueNoTasks = `{"code":0,"msg":"OK","data":{"tasks":[]}}`
)

// growthQueueFixture 任务中心测试面板：假上游只应答成长任务列表（其余 404），
// listCalls 统计**扫描次数**（默认口径请求数；mp 口径同路径同一次扫描，不重复计），
// 是「重复启动」的直接证据。
type growthQueueFixture struct {
	pn        *Panel
	srv       *httptest.Server
	listCalls atomic.Int32
}

// newGrowthQueueFixture tasksJSON 为任务列表响应体；gate 非 nil 时在应答前调用
// （用于把扫描卡在飞行中）。
func newGrowthQueueFixture(t *testing.T, tasksJSON string, gate func()) *growthQueueFixture {
	t.Helper()
	f := &growthQueueFixture{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tasksListPathForTest && r.Method == http.MethodGet {
			// 默认口径与小程序口径共用同一路径（X-Client-Platform: miniprogram 区分），
			// 一次扫描打两发；这里只数默认口径那发，让 listCalls 等于扫描次数。
			if r.Header.Get("X-Client-Platform") == "" {
				f.listCalls.Add(1)
				if gate != nil {
					gate()
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(tasksJSON))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
	}))
	t.Cleanup(f.srv.Close)

	up := &upstream.Client{HTTP: f.srv.Client(), ChatBaseCN: f.srv.URL, BillingBaseCN: f.srv.URL, WebBaseCN: f.srv.URL}
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	f.pn = New(Config{Pool: pl, Upstream: up, Version: "test"})

	// 节流/轮询压到毫秒级，异步队列才能快速收尾（与既有队列用例同口径）。
	oldGap, oldPoll := reportGap, claimPollGap
	reportGap, claimPollGap = time.Millisecond, time.Millisecond
	t.Cleanup(func() { reportGap, claimPollGap = oldGap, oldPoll })
	// 假上游全 404，退避不该进用例（双保险）。
	t.Cleanup(upstream.SetBillingRetryDelaysForTest(0))
	return f
}

// queueSnapshot 锁内快照队列状态（避免锁外读字段）。
type queueSnapshot struct {
	running bool
	conc    int
	seq     int
	items   []queueItem
}

func snapshotQueue(pn *Panel) queueSnapshot {
	q := pn.queue()
	q.mu.Lock()
	defer q.mu.Unlock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	return queueSnapshot{running: q.running, conc: q.conc, seq: q.seq, items: items}
}

// waitQueueIdle 等异步队列收尾（超时即失败）。
func waitQueueIdle(t *testing.T, pn *Panel) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for pn.queue().running && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if pn.queue().running {
		t.Fatal("队列未在 10s 内收尾")
	}
}

// TestRunGrowthQueueOnceSecondTriggerSkips 已在跑时二次触发必须安全跳过：不得重复
// 扫描、不得覆盖队列、不得起第二个执行者。
//
// 反事实：去掉 startGrowthQueue 里的新鲜占位判拒（`if q.running { ... return busy }`）
// 时，第二次触发会再扫一遍（listCalls 变 2）并二次启动队列——本用例第一条断言必红。
func TestRunGrowthQueueOnceSecondTriggerSkips(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	f := newGrowthQueueFixture(t, growthQueuePendingTasks, func() {
		enterOnce.Do(func() { close(entered) })
		<-release // 把第一次扫描卡在飞行中，占位保持新鲜
	})
	defer unblock() // 反事实分支下也要放行，否则 srv.Close() 会等未完成请求

	first := make(chan struct{})
	go func() {
		f.pn.RunGrowthQueueOnce(context.Background())
		close(first)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("首次自动执行未进入扫描")
	}
	if !f.pn.queue().running {
		t.Fatal("扫描期队列未占位：并发第二次触发无法被挡")
	}

	// 第二次触发（scheduler 重排 / 手动点同时发生）：必须立刻跳过。
	// 用 goroutine + 超时而不是同步调用：反事实实现（未判拒）下第二次触发会再进一次
	// 扫描并卡在 gate 上，同步调用会把用例挂死——失败要好过挂死（与既有 stale 用例的
	// unblock 注释同口径）。
	second := make(chan struct{})
	go func() {
		f.pn.RunGrowthQueueOnce(context.Background())
		close(second)
	}()
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("第二次触发未立刻跳过（疑似重复启动：又进了一次扫描）")
	}
	if n := f.listCalls.Load(); n != 1 {
		t.Fatalf("第二次触发不应再扫描（上游任务列表调用=%d want 1）——重复启动会互相覆盖 q.items/q.seq", n)
	}
	if snap := snapshotQueue(f.pn); snap.seq != 0 || len(snap.items) != 0 {
		t.Fatalf("第二次触发不得记一轮启动：seq=%d items=%d", snap.seq, len(snap.items))
	}

	unblock()
	select {
	case <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("首次自动执行未在放行后返回")
	}
	waitQueueIdle(t, f.pn)

	snap := snapshotQueue(f.pn)
	if snap.seq != 1 || len(snap.items) != 1 {
		t.Fatalf("最终只应有第一轮的一次启动：seq=%d items=%d", snap.seq, len(snap.items))
	}
	if n := f.listCalls.Load(); n < 1 {
		t.Fatalf("首次执行必须扫描（listCalls=%d）", n)
	}
}

// TestRunGrowthQueueOnceSkipsWhenManualQueueRunning 自动触发与手动入口共用同一个
// 占位状态机：用户手动点的队列还在扫描时，scheduler 到点触发必须跳过（不重复启动）。
func TestRunGrowthQueueOnceSkipsWhenManualQueueRunning(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	f := newGrowthQueueFixture(t, growthQueuePendingTasks, func() {
		enterOnce.Do(func() { close(entered) })
		<-release
	})
	defer unblock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/panel/api/tasks/run_queue", strings.NewReader(`{"concurrency":1}`))
	manualDone := make(chan struct{})
	go func() {
		f.pn.ServeHTTP(rec, req)
		close(manualDone)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("手动 run_queue 未进入扫描")
	}

	// 自动触发：必须立刻跳过（不扫描）。
	second := make(chan struct{})
	go func() {
		f.pn.RunGrowthQueueOnce(context.Background())
		close(second)
	}()
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("手动队列占位期间自动触发未立刻跳过（重复启动）")
	}
	if n := f.listCalls.Load(); n != 1 {
		t.Fatalf("手动队列占位期间自动触发不应再扫描（上游调用=%d want 1）", n)
	}

	unblock()
	select {
	case <-manualDone:
	case <-time.After(10 * time.Second):
		t.Fatal("手动 run_queue 未在放行后返回")
	}
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"started":true`) {
		t.Fatalf("手动入口应照常启动：code=%d body=%s", rec.Code, rec.Body.String())
	}
	waitQueueIdle(t, f.pn)
}

// TestRunGrowthQueueOnceNoPendingDoesNotStart 无待办时不误报启动：seq 不动、items
// 为空、占位回滚（后续触发能正常再扫，不会被自己的占位锁死）。
func TestRunGrowthQueueOnceNoPendingDoesNotStart(t *testing.T) {
	f := newGrowthQueueFixture(t, growthQueueNoTasks, nil)

	f.pn.RunGrowthQueueOnce(context.Background())

	snap := snapshotQueue(f.pn)
	if snap.running {
		t.Error("无待办路径必须回滚占位（running=false），否则队列会被自己的占位永久挡住")
	}
	if snap.seq != 0 || len(snap.items) != 0 {
		t.Errorf("无待办不得记一轮启动（误报）：seq=%d items=%d", snap.seq, len(snap.items))
	}
	if n := f.listCalls.Load(); n != 1 {
		t.Errorf("首次触发应扫描一次，got %d", n)
	}

	// 占位已回滚：下一次触发能重新扫描（不是被上一次的占位挡掉）。
	f.pn.RunGrowthQueueOnce(context.Background())
	if n := f.listCalls.Load(); n != 2 {
		t.Errorf("占位未回滚：第二次触发未扫描（上游调用=%d want 2）", n)
	}
}

// TestRunGrowthQueueOnceUsesSamePipeline 自动执行与手动「执行全部待办」同管线：
// 并发固定 1、队列条目按账号/任务组装、任务真的被执行（状态不再是 pending）。
func TestRunGrowthQueueOnceUsesSamePipeline(t *testing.T) {
	f := newGrowthQueueFixture(t, growthQueuePendingTasks, nil)

	f.pn.RunGrowthQueueOnce(context.Background())

	snap := snapshotQueue(f.pn)
	if snap.seq != 1 {
		t.Fatalf("应记一轮启动：seq=%d want 1", snap.seq)
	}
	if snap.conc != 1 {
		t.Errorf("自动执行并发应固定 1（串行），got %d", snap.conc)
	}
	if len(snap.items) != 1 {
		t.Fatalf("应入队 1 项：%+v", snap.items)
	}
	it := snap.items[0]
	if it.UID != "u1" || it.Kind != "growth" || it.Code != "chat_5" {
		t.Errorf("队列条目与手动入口同构，got %+v", it)
	}

	waitQueueIdle(t, f.pn)
	after := snapshotQueue(f.pn)
	switch st := after.items[0].Status; st {
	case "error", "done":
	default:
		t.Errorf("任务应已真被执行（终态 error/done），got status=%q", st)
	}
}

// TestRunGrowthQueueOnceCanceledContextDoesNotStart ctx 已取消（scheduler 停机）时
// 不发起扫描、不启动队列，占位必须回滚——否则会留一个永久 running 的假状态。
func TestRunGrowthQueueOnceCanceledContextDoesNotStart(t *testing.T) {
	f := newGrowthQueueFixture(t, growthQueuePendingTasks, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.pn.RunGrowthQueueOnce(ctx)

	snap := snapshotQueue(f.pn)
	if snap.running || snap.seq != 0 || len(snap.items) != 0 {
		t.Errorf("停机路径不得启动：running=%v seq=%d items=%d", snap.running, snap.seq, len(snap.items))
	}
	if n := f.listCalls.Load(); n != 0 {
		t.Errorf("ctx 已取消不应发起扫描，got %d 次", n)
	}
}

// TestRunGrowthQueueOnceReleasesStalePlaceholder 陈旧占位（上次运行疑似挂死）必须
// 放行本次自动执行——复用既有 growthQueueStaleAfter 兜底，否则一次上游挂死会让
// 每日自动执行永久失效。
func TestRunGrowthQueueOnceReleasesStalePlaceholder(t *testing.T) {
	f := newGrowthQueueFixture(t, growthQueuePendingTasks, nil)

	q := f.pn.queue()
	q.mu.Lock()
	q.running = true
	q.startedAt = time.Now().Add(-growthQueueStaleAfter - time.Minute)
	q.mu.Unlock()

	f.pn.RunGrowthQueueOnce(context.Background())

	if snap := snapshotQueue(f.pn); snap.seq != 1 || len(snap.items) != 1 {
		t.Fatalf("陈旧占位应放行本次启动：seq=%d items=%d", snap.seq, len(snap.items))
	}
	waitQueueIdle(t, f.pn)
}
