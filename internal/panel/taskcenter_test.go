// taskcenter_test.go 任务中心的两处真实缺陷修复 + 开学季下线的回归闸门。
//
//   - 达标未领的任务（target 已满但未领奖）此前被 growthPending 判为「非待办」，
//     队列从不入队 → 奖励永远领不到（upstream 5f6c7ca）。
//   - 队列启动的「先占位」：原实现先扫描（数秒级网络耗时）再置 running=true，
//     并发第二次触发会同时通过 running 判拒、两个 goroutine 互相覆盖 q.items/q.seq
//     （upstream 5f6c7ca）。
//   - 开学季任务侧下线（upstream 729247b）：/school/status 与 /school/run_all 移除，
//     /school/vouchers 券码查询保留。
package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestGrowthPendingIncludesMetUnclaimed 达标未领的任务必须入队（入队后由
// runGrowthQueued 走「回读 → claimable → 自动领奖」）。反事实：回退成
// `return false` 时第一条断言必红——奖励永远领不到。
func TestGrowthPendingIncludesMetUnclaimed(t *testing.T) {
	met := upstream.Task{TaskCode: "chat_5", Target: 5, Current: 5}
	if !growthPending(met) {
		t.Error("达标未领（5/5 未 claim）且可自动化的任务必须入队，否则奖励永远领不到")
	}
	// 达标未领但**无**自动化动作：不入队——队列执行时 autoActionFor 为 nil 只会报错。
	noAction := upstream.Task{TaskCode: "task_student_verify", Target: 1, Current: 1}
	if growthPending(noAction) {
		t.Error("无自动动作的任务不应入队（队列执行时因 autoActionFor 为 nil 直接报错）")
	}
	// 未达标 + 可自动化：照旧入队。
	if !growthPending(upstream.Task{TaskCode: "chat_5", Target: 5, Current: 2}) {
		t.Error("进行中且可自动化的任务应在待办中")
	}
	// 已领 / 上游锁定：不入队。
	if growthPending(upstream.Task{TaskCode: "chat_5", Target: 5, Current: 5, Claimed: true}) {
		t.Error("已领取的任务不应入队")
	}
	if growthPending(upstream.Task{TaskCode: "Sequential_Tasks_4", Target: 1, Current: 0, Locked: true}) {
		t.Error("上游锁定的任务（每日解锁环）不应入队")
	}
	if growthPending(upstream.Task{TaskCode: "task_student_verify", Target: 1, Current: 0}) {
		t.Error("无自动动作的任务不应入队")
	}
}

// TestRunQueuePlaceholderBlocksConcurrentStart 扫描期的队列占位：
// A 请求进入扫描（上游列表挂起）时，B 请求必须立刻拿到 409，而不是也进入扫描
// 并在扫描结束后覆盖 A 的 q.items/q.seq。
//
// 反事实：把占位挪回扫描之后（原实现），B 会通过 running 判拒并进入扫描 → 本用例
// 在 3s 超时上失败（两个 run_queue 同时在跑）。
func TestRunQueuePlaceholderBlocksConcurrentStart(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	// unblock 幂等放行上游列表（还挂带着 srv.Close() 不阻塞）：反事实（未占位）分支
	// 下 B 请求会同样进入扫描并挂在这里，若不在用例退出前放行，srv.Close() 会等
	// 未完成的请求而把整个测试套件挂死——失败要好过挂死。
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tasksListPathForTest && r.Method == http.MethodGet {
			once.Do(func() { close(entered) })
			<-release
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[
				{"task_code":"chat_5","title":"对话 5 次","target":5,"current":0,"accept_status":"accepted"}]}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
	}))
	defer srv.Close()
	defer unblock()

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	// APIKey 空 = 不鉴权，请求直达 handler。
	pn := New(Config{Pool: pl, Upstream: up, Version: "test"})

	// 队列执行是异步的，把节流/异步回读轮询压到毫秒级（用例结束前等 running 归位）。
	oldGap, oldPoll := reportGap, claimPollGap
	reportGap, claimPollGap = time.Millisecond, time.Millisecond
	t.Cleanup(func() { reportGap, claimPollGap = oldGap, oldPoll })

	recA := httptest.NewRecorder()
	reqA := httptest.NewRequest("POST", "/panel/api/tasks/run_queue", strings.NewReader(`{"concurrency":1}`))
	doneA := make(chan struct{})
	go func() { pn.ServeHTTP(recA, reqA); close(doneA) }()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("A 请求未进入扫描（上游列表未被调用）")
	}
	// A 在扫描中：队列必须已占位。
	if !pn.queue().running {
		t.Fatal("扫描期队列未占位（running=false）：并发第二次触发无法被挡")
	}

	recB := httptest.NewRecorder()
	reqB := httptest.NewRequest("POST", "/panel/api/tasks/run_queue", strings.NewReader(`{"concurrency":1}`))
	doneB := make(chan struct{})
	go func() { pn.ServeHTTP(recB, reqB); close(doneB) }()

	select {
	case <-doneB:
	case <-time.After(3 * time.Second):
		t.Fatal("并发第二次 run_queue 未被挡（扫描期未占位：两个 goroutine 会互相覆盖 q.items/q.seq）")
	}
	if recB.Code != http.StatusConflict {
		t.Errorf("并发第二次 run_queue 应 409，got %d body=%s", recB.Code, recB.Body.String())
	}
	unblock() // 放行 A（幂等；用例异常退出时由 defer unblock 兜底）

	select {
	case <-doneA:
	case <-time.After(10 * time.Second):
		t.Fatal("A 请求未在放行后完成")
	}
	if recA.Code != http.StatusOK {
		t.Fatalf("A 请求应 200，got %d body=%s", recA.Code, recA.Body.String())
	}
	if !strings.Contains(recA.Body.String(), `"started":true`) {
		t.Errorf("A 应真正启动队列：%s", recA.Body.String())
	}

	// 等异步队列执行收尾（running 归位），避免后台 goroutine 越过用例边界。
	deadline := time.Now().Add(10 * time.Second)
	for pn.queue().running && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if pn.queue().running {
		t.Error("队列执行未在 10s 内收尾")
	}
}

// tasksListPathForTest 与 upstream 内部常量同值（growth 域任务列表路径）。
const tasksListPathForTest = "/v2/activity/growth/tasks"

// TestSchoolTaskRoutesRemoved 开学季任务侧已下线：两个任务路由 404，券码查询保留
// （历史券码仍可查）。反事实：路由还在时前两条断言必红。
func TestSchoolTaskRoutesRemoved(t *testing.T) {
	pn := New(Config{Pool: pool.New(""), Version: "test", APIKey: "k"})
	call := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer k")
		rec := httptest.NewRecorder()
		pn.ServeHTTP(rec, req)
		return rec
	}
	for _, tc := range []struct{ method, path string }{
		{"GET", "/panel/api/school/status"},
		{"POST", "/panel/api/school/run_all"},
	} {
		if rec := call(tc.method, tc.path); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s 应已下线（404），got %d", tc.method, tc.path, rec.Code)
		}
	}
	if rec := call("GET", "/panel/api/school/vouchers"); rec.Code != http.StatusOK {
		t.Errorf("券码查询必须保留（历史券码仍可查），got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestRunQueueStalePlaceholderDoesNotWedgeForever 锁定「先占位」引入的**新失效模式**：
// 扫描阶段的 ListTasks/ListTasksMP 是无 ctx 超时的上游调用，网络黑洞会让 wg.Wait()
// 永不返回——此时 running 恒为 true，队列从此永久 409 直到进程重启。
//
// 反事实：去掉 growthQueueStaleAfter 仓龄判据（`if q.running` 直接 409）时，本用例
// 第二条断言必红（陈旧占位把队列永久锁死）。
func TestRunQueueStalePlaceholderDoesNotWedgeForever(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == tasksListPathForTest && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			// 空待办：本次启动会扫描成功但无待办 → 200 + started:false 并回滚占位。
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"tasks":[]}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
	}))
	defer srv.Close()

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	pn := New(Config{Pool: pl, Upstream: up, Version: "test"})

	run := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/panel/api/tasks/run_queue", strings.NewReader(`{"concurrency":1}`))
		rec := httptest.NewRecorder()
		pn.ServeHTTP(rec, req)
		return rec
	}

	// 新鲜占位：必须仍然 409（占位的防并发语义不能被削弱）。
	q := pn.queue()
	q.mu.Lock()
	q.running = true
	q.startedAt = time.Now()
	q.mu.Unlock()
	if rec := run(); rec.Code != http.StatusConflict {
		t.Fatalf("新鲜占位应 409（防并发重复启动语义被削弱），got %d body=%s", rec.Code, rec.Body.String())
	}

	// 陈旧占位：判定上次运行已死，必须放行——否则一次上游挂死 = 队列永久失效。
	q.mu.Lock()
	q.running = true
	q.startedAt = time.Now().Add(-growthQueueStaleAfter - time.Minute)
	q.mu.Unlock()
	rec := run()
	if rec.Code == http.StatusConflict {
		t.Fatalf("陈旧占位（超过 %s）必须放行本次启动，否则队列被永久卡死：body=%s",
			growthQueueStaleAfter, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("放行后应正常扫描并返回 200，got %d body=%s", rec.Code, rec.Body.String())
	}
	// 放行后占位必须已被回滚（空待办路径），否则下轮又会被自己的占位挡住。
	if q.running {
		t.Error("无待办路径应回滚 running=false")
	}
}
