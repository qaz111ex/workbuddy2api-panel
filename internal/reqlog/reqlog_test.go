package reqlog

import (
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readArchiveLines 按文件名序（即时间序）读回归档目录里的全部 JSONL 行。
func readArchiveLines(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读归档目录: %v", err)
	}
	var lines []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), archiveExt) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("读归档文件 %s: %v", e.Name(), err)
		}
		for _, ln := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			if ln != "" {
				lines = append(lines, ln)
			}
		}
	}
	return lines
}

func TestNewRequestIDShape(t *testing.T) {
	seen := make(map[string]bool, 4096)
	for i := 0; i < 4096; i++ {
		id := NewRequestID()
		if !strings.HasPrefix(id, "req-") {
			t.Fatalf("ID %q 缺 req- 前缀", id)
		}
		hexPart := strings.TrimPrefix(id, "req-")
		if len(hexPart) != 16 {
			t.Fatalf("ID %q 的随机段长度 = %d，期望 16 hex", id, len(hexPart))
		}
		if _, err := hex.DecodeString(hexPart); err != nil {
			t.Fatalf("ID %q 不是合法 hex: %v", id, err)
		}
		if seen[id] {
			t.Fatalf("ID 重复: %q（4096 次里不该撞）", id)
		}
		seen[id] = true
	}
}

func TestCountersRatesAndInFlight(t *testing.T) {
	r := New(Config{}) // 纯内存
	defer r.Close()

	if s := r.Snapshot(0); s.Completed != 0 || s.InFlight != 0 || s.SuccessRate != 0 {
		t.Fatalf("空记录器不该有数字：%+v", s)
	}

	for i := 0; i < 3; i++ {
		r.Begin()
	}
	if s := r.Snapshot(0); s.InFlight != 3 {
		t.Fatalf("Begin x3 后 InFlight = %d，期望 3", s.InFlight)
	}

	r.Record(Event{RequestID: "a", Status: 200, OK: true, Outcome: OutcomeSuccess, DurationMs: 100})
	r.Record(Event{RequestID: "b", Status: 502, OK: false, Outcome: OutcomeHTTPError, DurationMs: 200})
	// c 是「HTTP 头 200、流中途断线」：HTTP 层成功、请求层失败——两个口径的差就在这。
	r.Record(Event{RequestID: "c", Status: 200, OK: false, Outcome: OutcomeStreamError, DurationMs: 300})

	s := r.Snapshot(0)
	if s.Completed != 3 {
		t.Errorf("Completed = %d，期望 3", s.Completed)
	}
	if s.InFlight != 0 {
		t.Errorf("InFlight = %d，期望 0（3 次 Begin 已被 3 次 Record 抵掉）", s.InFlight)
	}
	if s.Succeeded != 1 {
		t.Errorf("Succeeded = %d，期望 1", s.Succeeded)
	}
	if s.Failed != 2 {
		t.Errorf("Failed = %d，期望 2", s.Failed)
	}
	if math.Abs(s.SuccessRate-1.0/3.0) > 1e-9 {
		t.Errorf("SuccessRate = %v，期望 1/3", s.SuccessRate)
	}
	// a(200) 与 c(200) 算 HTTP 成功；b(502) 不算 → 2/3。
	if math.Abs(s.HTTPSuccessRate-2.0/3.0) > 1e-9 {
		t.Errorf("HTTPSuccessRate = %v，期望 2/3（流失败的 200 算 HTTP 成功）", s.HTTPSuccessRate)
	}
	if math.Abs(s.AvgDurationMs-200) > 1e-9 {
		t.Errorf("AvgDurationMs = %v，期望 200", s.AvgDurationMs)
	}
	if len(s.Recent) != 3 {
		t.Fatalf("Recent 条数 = %d，期望 3", len(s.Recent))
	}
	if s.Recent[0].RequestID != "c" || s.Recent[2].RequestID != "a" {
		t.Errorf("Recent 未按倒序（最新在前）：%s,%s,%s",
			s.Recent[0].RequestID, s.Recent[1].RequestID, s.Recent[2].RequestID)
	}
	if !s.StartedAt.Equal(r.started) {
		t.Errorf("StartedAt = %v，期望 %v", s.StartedAt, r.started)
	}
}

func TestRecentBoundedTo100(t *testing.T) {
	r := New(Config{})
	defer r.Close()

	const total = 250
	for i := 0; i < total; i++ {
		r.Record(Event{
			RequestID: fmt.Sprintf("r%03d", i),
			Status:    200, OK: true, Outcome: OutcomeSuccess,
		})
	}

	s := r.Snapshot(0)
	if s.Completed != total {
		t.Errorf("Completed = %d，期望 %d（计数不受 Recent 上限影响）", s.Completed, total)
	}
	if len(s.Recent) != recentCap {
		t.Fatalf("Recent 条数 = %d，期望 %d", len(s.Recent), recentCap)
	}
	if s.Recent[0].RequestID != "r249" {
		t.Errorf("Recent[0] = %s，期望 r249（最新）", s.Recent[0].RequestID)
	}
	if s.Recent[recentCap-1].RequestID != "r150" {
		t.Errorf("Recent[99] = %s，期望 r150", s.Recent[recentCap-1].RequestID)
	}

	// 内存有界：缓冲长度恒为 recentCap，不会随请求数增长。
	if len(r.ring) != recentCap || r.ringN != recentCap {
		t.Errorf("环形缓冲 len=%d n=%d，恒应为 %d", len(r.ring), r.ringN, recentCap)
	}

	if got := len(r.Snapshot(7).Recent); got != 7 {
		t.Errorf("Snapshot(7).Recent = %d，期望 7", got)
	}
	if got := len(r.Snapshot(10_000).Recent); got != recentCap {
		t.Errorf("Snapshot(10000).Recent = %d，期望夹到 %d", got, recentCap)
	}
}

// TestRecordNeverBlocksWhenArchiveStalled 是本包最关键的一条不变量：
// **归档停摆（goroutine 已退出、没人消费队列）时，Record 必须立刻返回并丢弃。**
//
// 造法：New 出带队列的记录器后先 Close——归档 goroutine 确定已退出且队列已被排空，
// 此后入队的事件必然无人消费。若 Record 用的是阻塞发送（r.queue <- ev），这个测试
// 会卡死在这里、并在超时后变红；用 select+default 则立刻走丢弃分支。
func TestRecordNeverBlocksWhenArchiveStalled(t *testing.T) {
	dir := t.TempDir()
	const queueSize = 8
	r := New(Config{Dir: dir, Enabled: true, QueueSize: queueSize})
	r.Close() // 归档 goroutine 已退出；队列空且无人消费

	const n = 500
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			r.Record(Event{
				RequestID: fmt.Sprintf("x%d", i),
				Status:    200, OK: true, Outcome: OutcomeSuccess,
			})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Record 被阻塞了：归档停摆时请求路径被拖住（必须丢弃而不是等待）")
	}

	s := r.Snapshot(0)
	if s.Completed != n {
		t.Errorf("Completed = %d，期望 %d（丢弃归档不能丢指标）", s.Completed, n)
	}
	if !s.Archive.Enabled {
		t.Error("Archive.Enabled = false，期望 true")
	}
	// 队列容量 8：恰好 8 条入队、其余全部丢弃。
	if want := uint64(n - queueSize); s.Archive.DroppedWrites != want {
		t.Errorf("DroppedWrites = %d，期望 %d（队列满即丢，一条不吞）", s.Archive.DroppedWrites, want)
	}
}

// TestRecordIsFastUnderBurst 是上一条的「活体」版本：归档在跑、请求在猛灌，
// 单次 Record 也不该出现等待磁盘的毛刺。
func TestRecordIsFastUnderBurst(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, Enabled: true, QueueSize: 16, FileMaxBytes: 4096})
	defer r.Close()

	const n = 2000
	worst := time.Duration(0)
	start := time.Now()
	for i := 0; i < n; i++ {
		t0 := time.Now()
		r.Record(Event{RequestID: fmt.Sprintf("b%d", i), Path: "/v1/chat/completions",
			Model: "m", Status: 200, OK: true, Outcome: OutcomeSuccess, DurationMs: 1})
		if d := time.Since(t0); d > worst {
			worst = d
		}
	}
	total := time.Since(start)
	// 阈值刻意宽松（CI/慢盘）：要抓的是「等待磁盘」级别的阻塞（几十毫秒起），
	// 不是几微秒的调度抖动。
	if worst > 200*time.Millisecond {
		t.Errorf("单次 Record 最坏耗时 %v：请求路径上出现了等待", worst)
	}
	if total > 10*time.Second {
		t.Errorf("%d 次 Record 总耗时 %v，远超预期", n, total)
	}
}

// TestDroppedPlusWrittenEqualsTotal 端到端守恒：落盘行数 + 丢弃数 == 提交总数。
// 这条同时证明两件事：丢弃是真丢弃（不会重复写），以及计数没漏账。
func TestDroppedPlusWrittenEqualsTotal(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, Enabled: true, QueueSize: 16, FileMaxBytes: 4096})

	const n = 3000
	for i := 0; i < n; i++ {
		r.Begin()
		r.Record(Event{
			RequestID: fmt.Sprintf("req-%d", i),
			Path:      "/v1/chat/completions",
			Account:   "猫(a1b2c3d4)",
			Model:     "claude-sonnet-4",
			Status:    200, OK: true, Outcome: OutcomeSuccess, DurationMs: 12,
		})
	}
	r.Close()

	lines := readArchiveLines(t, dir)
	dropped := r.Snapshot(0).Archive.DroppedWrites
	if int64(len(lines))+int64(dropped) != n {
		t.Fatalf("落盘 %d 行 + 丢弃 %d ≠ 提交 %d：归档丢了事件却没记账", len(lines), dropped, n)
	}
	if len(lines) == 0 {
		t.Fatal("一条都没落盘：归档 goroutine 没在干活")
	}
}

func TestDisabledArchiveStillRecordsMetrics(t *testing.T) {
	cases := []struct {
		name string
		cfg  func(t *testing.T) Config
	}{
		{"enabled-false", func(t *testing.T) Config { return Config{Dir: t.TempDir()} }},
		{"dir-empty", func(t *testing.T) Config { return Config{Enabled: true} }},
		{"dir-blank", func(t *testing.T) Config { return Config{Enabled: true, Dir: "   "} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg(t)
			r := New(cfg)
			defer r.Close()

			r.Begin()
			r.Record(Event{RequestID: "a", Path: "/v1/chat/completions", Status: 200, OK: true,
				Outcome: OutcomeSuccess, DurationMs: 5})

			s := r.Snapshot(0)
			if s.Completed != 1 || s.Succeeded != 1 || s.Failed != 0 || s.InFlight != 0 {
				t.Errorf("未落盘时内存指标不完整：%+v", s)
			}
			if len(s.Recent) != 1 {
				t.Errorf("Recent 条数 = %d，期望 1", len(s.Recent))
			}
			if s.Archive.Enabled {
				t.Error("Archive.Enabled = true，期望 false")
			}
			if s.Archive.Dir != "" {
				t.Errorf("Archive.Dir = %q，未启用时不该报目录", s.Archive.Dir)
			}
			if s.Archive.DroppedWrites != 0 {
				t.Errorf("DroppedWrites = %d，未启用归档不该记丢弃", s.Archive.DroppedWrites)
			}
			if cfg.Dir != "" {
				entries, err := os.ReadDir(cfg.Dir)
				if err != nil && !os.IsNotExist(err) {
					t.Fatalf("读目录: %v", err)
				}
				if len(entries) != 0 {
					t.Errorf("未启用归档却写了 %d 个文件", len(entries))
				}
			}
		})
	}
}

func TestNilRecorderIsSafe(t *testing.T) {
	var r *Recorder
	r.Begin()
	r.Record(Event{RequestID: "x"})
	r.Cleanup()
	r.Close() // 幂等 + nil 安全

	s := r.Snapshot(0)
	if s.Completed != 0 {
		t.Errorf("nil 记录器返回了数字：%+v", s)
	}
	if s.Recent == nil {
		t.Error("Recent 应是空切片而不是 nil（面板直接 JSON 化，nil 会变 null）")
	}
}

func TestBeginRecordUnpairedDoesNotGoNegative(t *testing.T) {
	r := New(Config{})
	defer r.Close()

	// 没 Begin 就 Record（调用方 bug）：InFlight 夹在 0，不该漂成负数。
	for i := 0; i < 5; i++ {
		r.Record(Event{RequestID: "x", Status: 200, OK: true, Outcome: OutcomeSuccess})
	}
	if got := r.Snapshot(0).InFlight; got != 0 {
		t.Errorf("InFlight = %d，期望夹在 0", got)
	}

	// 反向：Begin 多于 Record → 如实反映在飞请求数。
	for i := 0; i < 4; i++ {
		r.Begin()
	}
	if got := r.Snapshot(0).InFlight; got != 4 {
		t.Errorf("InFlight = %d，期望 4", got)
	}
}

func TestCloseIsIdempotentAndRecordAfterCloseIsSafe(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, Enabled: true, QueueSize: 8})
	r.Record(Event{RequestID: "before", Status: 200, OK: true, Outcome: OutcomeSuccess})

	r.Close()
	r.Close()
	r.Close()

	// Close 之后继续 Record：不 panic、不阻塞（没人消费，队列满即丢）。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			r.Record(Event{RequestID: "after", Status: 200, OK: true, Outcome: OutcomeSuccess})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close 之后的 Record 被阻塞")
	}
	if got := r.Snapshot(0).Completed; got != 101 {
		t.Errorf("Completed = %d，期望 101", got)
	}
}

// TestCloseDrainsQueue 停机不丢已入队事件：Close 排空队列后才返回。
func TestCloseDrainsQueue(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, Enabled: true, QueueSize: 1024})
	const n = 200
	for i := 0; i < n; i++ {
		r.Record(Event{Time: time.Now(), RequestID: fmt.Sprintf("d%d", i),
			Status: 200, OK: true, Outcome: OutcomeSuccess})
	}
	r.Close() // 排空后才返回

	lines := readArchiveLines(t, dir)
	if len(lines) != n {
		t.Errorf("落盘 %d 行，期望 %d（Close 应先排空队列）", len(lines), n)
	}
	if dropped := r.Snapshot(0).Archive.DroppedWrites; dropped != 0 {
		t.Errorf("DroppedWrites = %d，期望 0", dropped)
	}
}
