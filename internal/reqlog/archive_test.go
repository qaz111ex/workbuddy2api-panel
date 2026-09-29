package reqlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSized(t *testing.T, dir, name string, n int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), bytes.Repeat([]byte("x"), n), 0o600); err != nil {
		t.Fatalf("造文件 %s: %v", name, err)
	}
}

func statNames(t *testing.T, dir string) map[string]int64 {
	t.Helper()
	files, err := listArchive(dir)
	if err != nil {
		t.Fatalf("listArchive: %v", err)
	}
	out := make(map[string]int64, len(files))
	for _, f := range files {
		out[f.name] = f.size
	}
	return out
}

// TestArchiveWritesValidJSONL 落盘的最基本契约：逐行合法 JSON，字段原样可读回。
func TestArchiveWritesValidJSONL(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, Enabled: true, QueueSize: 4096})

	// 固定时间 → 文件名与内容都确定，测试不随运行日期漂移。
	ts := time.Date(2026, 3, 4, 5, 6, 7, 0, time.Local)
	const n = 40
	for i := 0; i < n; i++ {
		r.Record(Event{
			Time: ts, RequestID: fmt.Sprintf("req-%d", i), Path: "/v1/chat/completions",
			Account: "猫(a1b2c3d4)", Model: "claude-sonnet-4",
			Status: 200, OK: true, Outcome: OutcomeSuccess,
			DurationMs: int64(i), TTFBMs: 3, Attempts: 2,
			PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30,
			Credit: 1.5, HasCredit: true,
		})
	}
	r.Close()

	s := r.Snapshot(0)
	if s.Archive.DroppedWrites != 0 {
		t.Fatalf("DroppedWrites = %d，期望 0（队列足够大）", s.Archive.DroppedWrites)
	}
	if !s.Archive.Enabled {
		t.Fatal("Archive.Enabled = false")
	}
	if s.Archive.Dir != dir {
		t.Errorf("Archive.Dir = %q，期望 %q", s.Archive.Dir, dir)
	}
	if s.Archive.Files != 1 {
		t.Errorf("Archive.Files = %d，期望 1", s.Archive.Files)
	}
	if s.Archive.Bytes <= 0 {
		t.Errorf("Archive.Bytes = %d，期望 > 0", s.Archive.Bytes)
	}
	if s.Archive.LastError != "" {
		t.Errorf("Archive.LastError = %q，期望空", s.Archive.LastError)
	}

	wantName := "requests-2026-03-04.jsonl"
	if _, err := os.Stat(filepath.Join(dir, wantName)); err != nil {
		t.Fatalf("期望归档文件名 %s: %v", wantName, err)
	}

	lines := readArchiveLines(t, dir)
	if len(lines) != n {
		t.Fatalf("归档 %d 行，期望 %d 行", len(lines), n)
	}
	for i, ln := range lines {
		var ev Event
		if err := json.Unmarshal([]byte(ln), &ev); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %v\n%s", i, err, ln)
		}
		if want := fmt.Sprintf("req-%d", i); ev.RequestID != want {
			t.Errorf("第 %d 行 RequestID = %q，期望 %q（顺序即写入顺序）", i, ev.RequestID, want)
		}
		if !ev.Time.Equal(ts) {
			t.Errorf("第 %d 行 Time = %v，期望 %v", i, ev.Time, ts)
		}
		if ev.Account != "猫(a1b2c3d4)" || ev.Model != "claude-sonnet-4" {
			t.Errorf("第 %d 行账号/模型丢失：%+v", i, ev)
		}
		if ev.TotalTokens != 30 || !ev.HasCredit || ev.Credit != 1.5 {
			t.Errorf("第 %d 行 token/credit 丢失：%+v", i, ev)
		}
	}
}

// TestArchiveRotatesByFileMaxBytes 单文件上限：写满即换序号，跨文件不丢行。
func TestArchiveRotatesByFileMaxBytes(t *testing.T) {
	dir := t.TempDir()
	const fileMax = 400
	const n = 60
	r := New(Config{Dir: dir, Enabled: true, QueueSize: 4096, FileMaxBytes: fileMax})
	for i := 0; i < n; i++ {
		r.Record(Event{RequestID: fmt.Sprintf("req-%d", i), Path: "/v1/chat/completions",
			Model: "claude-sonnet-4", Status: 200, OK: true, Outcome: OutcomeSuccess, DurationMs: 1})
	}
	r.Close()

	files, err := listArchive(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 3 {
		t.Fatalf("只切出 %d 个文件，期望 >= 3（fileMax=%d, n=%d）", len(files), fileMax, n)
	}

	// 单行长度：空文件允许单行超限（否则那行只能丢），所以上界是 max(上限, 单行长)。
	one, err := json.Marshal(Event{RequestID: "req-0", Path: "/v1/chat/completions",
		Model: "claude-sonnet-4", Status: 200, OK: true, Outcome: OutcomeSuccess, DurationMs: 1})
	if err != nil {
		t.Fatal(err)
	}
	lineLen := int64(len(one) + 1)
	for _, f := range files {
		if f.size > fileMax && f.size > lineLen {
			t.Errorf("%s 大小 %d 超过单文件上限 %d", f.name, f.size, fileMax)
		}
	}

	if got := len(readArchiveLines(t, dir)); got != n {
		t.Errorf("跨文件总行数 = %d，期望 %d", got, n)
	}
	// 序号从 0 起连续递增，命名可被 Cleanup 识别。
	for i, f := range files {
		if f.seq != i {
			t.Errorf("第 %d 个文件序号 = %d，期望 %d（%s）", i, f.seq, i, f.name)
		}
	}
}

// TestArchiveStatsReflectOpenFileBytes 回归：归档文件**还开着**的时候，
// Archive.Bytes 也必须反映真实字节数。
//
// 背景（本机实测踩到的坑）：Windows 上 os.ReadDir 返回的 DirEntry.Info() 对
// 「正被打开写入的文件」报的大小是陈旧的（实测写入 155 字节后仍报 0，句柄关闭
// 后才正确），而 os.Stat 是准的。listArchive 因此必须走 os.Stat——否则面板的
// Archive.Bytes 会长期偏低，Cleanup 的容量核算也会少算。这个测试就是钉住它：
// 换回 DirEntry.Info 实现时它会红。
func TestArchiveStatsReflectOpenFileBytes(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, Enabled: true, QueueSize: 64})
	defer r.Close()

	r.Record(Event{Time: time.Now(), RequestID: "a", Status: 200, OK: true, Outcome: OutcomeSuccess})

	deadline := time.Now().Add(5 * time.Second)
	for {
		s := r.Snapshot(0)
		if s.Archive.Bytes > 0 {
			if s.Archive.Files != 1 {
				t.Errorf("Archive.Files = %d，期望 1", s.Archive.Files)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("文件已写入但 Archive.Bytes 仍为 %d：枚举来的大小在 Windows 上是陈旧的（须用 os.Stat）", s.Archive.Bytes)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestArchiveSplitsByDay 跨天切分：不同日期的两条事件落在两个文件里。
func TestArchiveSplitsByDay(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, Enabled: true, QueueSize: 64})
	d1 := time.Date(2026, 3, 4, 23, 59, 0, 0, time.Local)
	d2 := time.Date(2026, 3, 5, 0, 1, 0, 0, time.Local)
	r.Record(Event{Time: d1, RequestID: "a", Status: 200, OK: true, Outcome: OutcomeSuccess})
	r.Record(Event{Time: d2, RequestID: "b", Status: 200, OK: true, Outcome: OutcomeSuccess})
	r.Close()

	for _, want := range []string{"requests-2026-03-04.jsonl", "requests-2026-03-05.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("期望按天切分的文件 %s: %v", want, err)
		}
	}
	if got := len(readArchiveLines(t, dir)); got != 2 {
		t.Errorf("总行数 = %d，期望 2", got)
	}
}

// TestCleanupDeletesByRetention 保留期：窗口外的整日文件删掉，窗口内的留下。
func TestCleanupDeletesByRetention(t *testing.T) {
	dir := t.TempDir()
	day := func(back int) string { return time.Now().AddDate(0, 0, -back).Format(dayLayout) }

	writeSized(t, dir, archiveName(day(10), 0), 100) // 远早于窗口 → 删
	writeSized(t, dir, archiveName(day(9), 0), 100)  // 仍早于窗口（保留 7 天）→ 删
	writeSized(t, dir, archiveName(day(6), 0), 100)  // 窗口边界内 → 留
	writeSized(t, dir, archiveName(day(0), 1), 100)  // 今天（非当前写文件）→ 留

	r := New(Config{Dir: dir, Enabled: true, RetentionDays: 7})
	r.Cleanup()
	r.Close()

	got := statNames(t, dir)
	for _, gone := range []string{archiveName(day(10), 0), archiveName(day(9), 0)} {
		if _, ok := got[gone]; ok {
			t.Errorf("超出保留期的 %s 没被删", gone)
		}
	}
	for _, kept := range []string{archiveName(day(6), 0), archiveName(day(0), 1)} {
		if _, ok := got[kept]; !ok {
			t.Errorf("保留期内的 %s 被误删", kept)
		}
	}
}

// TestCleanupDeletesOldestByMaxBytes 容量：总量超限时从最旧删起，直到不超。
func TestCleanupDeletesOldestByMaxBytes(t *testing.T) {
	dir := t.TempDir()
	day := func(back int) string { return time.Now().AddDate(0, 0, -back).Format(dayLayout) }

	// 保留期设得很大，隔离出「只由容量触发删除」的场景。
	writeSized(t, dir, archiveName(day(4), 0), 1000)
	writeSized(t, dir, archiveName(day(3), 0), 1000)
	writeSized(t, dir, archiveName(day(2), 0), 1000)

	r := New(Config{Dir: dir, Enabled: true, RetentionDays: 365, MaxBytes: 2500})
	r.Cleanup()
	r.Close()

	got := statNames(t, dir)
	var total int64
	for _, sz := range got {
		total += sz
	}
	if total > 2500 {
		t.Errorf("清理后总量 %d 仍超上限 2500", total)
	}
	if _, ok := got[archiveName(day(4), 0)]; ok {
		t.Error("最旧的文件没先被删")
	}
	for _, kept := range []string{archiveName(day(3), 0), archiveName(day(2), 0)} {
		if _, ok := got[kept]; !ok {
			t.Errorf("只需删一个就够，%s 不该被删", kept)
		}
	}
}

// TestCleanupAggressiveMaxBytesKeepsNothingExtra 极端容量（比单文件还小）：
// 只该剩当前正在写的那个，且不报错。
func TestCleanupAggressiveMaxBytesKeepsNothingExtra(t *testing.T) {
	dir := t.TempDir()
	day := func(back int) string { return time.Now().AddDate(0, 0, -back).Format(dayLayout) }
	for i := 1; i <= 5; i++ {
		writeSized(t, dir, archiveName(day(i), 0), 512)
	}

	r := New(Config{Dir: dir, Enabled: true, RetentionDays: 365, MaxBytes: 1})
	r.Cleanup()
	r.Close()

	if got := statNames(t, dir); len(got) != 0 {
		t.Errorf("MaxBytes=1 时应删光非活跃文件，剩 %v", got)
	}
}

// TestCleanupNeverDeletesActiveFile 正在写的文件永不被删——哪怕容量上限比它还小。
// Windows 上删打开中的文件本来就会失败，这条保证「配置过小」不会把归档写挂。
func TestCleanupNeverDeletesActiveFile(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, Enabled: true, QueueSize: 64, RetentionDays: 365, MaxBytes: 1})
	defer r.Close()

	r.Record(Event{RequestID: "live", Status: 200, OK: true, Outcome: OutcomeSuccess})

	// 等到「当前文件」确实存在且有内容——写盘发生在 curName 赋值之后，
	// 所以字节数 > 0 就说明它一定就是 recoder 眼里的活动文件。
	deadline := time.Now().Add(5 * time.Second)
	active := ""
	for active == "" {
		if files, err := listArchive(dir); err == nil {
			var total int64
			for _, f := range files {
				total += f.size
			}
			if total > 0 {
				active = files[len(files)-1].name
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("等不到首次落盘")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 再放一个更旧、更大的文件：容量规则本应先删它。
	old := archiveName(time.Now().AddDate(0, 0, -30).Format(dayLayout), 0)
	writeSized(t, dir, old, 4096)

	r.Cleanup()

	got := statNames(t, dir)
	if _, ok := got[old]; ok {
		t.Error("最旧的超额文件没被删")
	}
	if len(got) != 1 {
		t.Fatalf("清理后应只剩当前活动文件，实际 %v", got)
	}
	if _, ok := got[active]; !ok {
		t.Errorf("活动文件 %s 被误删（剩 %v）", active, got)
	}
}

// TestCleanupRetentionOneKeepsTodayOnly RetentionDays=1 的边界语义。
func TestCleanupRetentionOneKeepsTodayOnly(t *testing.T) {
	dir := t.TempDir()
	today := time.Now().Format(dayLayout)
	yesterday := time.Now().AddDate(0, 0, -1).Format(dayLayout)
	writeSized(t, dir, archiveName(today, 0), 10)
	writeSized(t, dir, archiveName(yesterday, 0), 10)

	r := New(Config{Dir: dir, Enabled: true, RetentionDays: 1})
	r.Cleanup()
	r.Close()

	got := statNames(t, dir)
	if _, ok := got[archiveName(yesterday, 0)]; ok {
		t.Error("RetentionDays=1 时昨天该被删")
	}
	if _, ok := got[archiveName(today, 0)]; !ok {
		t.Error("RetentionDays=1 时今天该保留")
	}
}

// TestCleanupNoDirectoryIsHarmless 目录还不存在时 Cleanup 不该报错/panic。
func TestCleanupNoDirectoryIsHarmless(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created-yet")
	r := New(Config{Dir: dir, Enabled: true})
	// New 会建目录，先删掉模拟「目录被运维清空/还没建」。
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	r.Cleanup()
	if got := r.Snapshot(0).Archive.LastError; got != "" {
		t.Errorf("目录不存在不该记 LastError: %q", got)
	}
	r.Close()
}

// TestArchiveWriteErrorIsVisible 写不进去必须可见（LastError + DroppedWrites），
// 而不是静默丢：静默失败比失败更难查。
func TestArchiveWriteErrorIsVisible(t *testing.T) {
	// 用一个**文件**占住归档目录的路径：MkdirAll/OpenFile 必然失败。
	parent := t.TempDir()
	blocked := filepath.Join(parent, "blocked")
	if err := os.WriteFile(blocked, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := New(Config{Dir: blocked, Enabled: true, QueueSize: 64})
	r.Record(Event{RequestID: "a", Status: 200, OK: true, Outcome: OutcomeSuccess})
	r.Close()

	s := r.Snapshot(0)
	if s.Archive.LastError == "" {
		t.Error("归档写失败却没有 LastError")
	}
	if s.Archive.DroppedWrites != 1 {
		t.Errorf("DroppedWrites = %d，期望 1（写失败也算漏）", s.Archive.DroppedWrites)
	}
	// 内存指标不受归档故障影响。
	if s.Completed != 1 || s.Succeeded != 1 {
		t.Errorf("归档失败拖累了内存指标：%+v", s)
	}
}
