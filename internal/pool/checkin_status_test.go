// checkin_status_test.go 钉住「今日已签到」观测（upstream dd4ea34 的 pool 侧）：
// NoteCheckinDone 标记当日，statusOf 输出 CheckinDone（跨零点自然过期），
// 持久化往返不丢。
package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestNoteCheckinDoneMarksToday(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	if st, _ := p.Status("u1"); st.CheckinDone {
		t.Fatal("未签到账号不应显示已签")
	}
	p.NoteCheckinDone("u1")
	st, ok := p.Status("u1")
	if !ok || !st.CheckinDone {
		t.Fatalf("u1 标记后应显示已签: %+v ok=%v", st, ok)
	}
	if st, _ := p.Status("u2"); st.CheckinDone {
		t.Fatal("u2 未标记，不应显示已签")
	}
	// 未知 uid 不 panic、不影响其他账号。
	p.NoteCheckinDone("no-such-uid")
	if st, _ := p.Status("u1"); !st.CheckinDone {
		t.Fatal("未知 uid 调用不得影响已标记账号")
	}
}

// TestNoteCheckinDoneDoesNotTouchCooling 签到标记与冷却域正交：已签到状态不得
// 解冻冷却、不得清熔断（与 reviveCoolingLocked 的边界一致）。
func TestNoteCheckinDoneDoesNotTouchCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.3", "6004")
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1")

	p.mu.RLock()
	e := p.byUID["u1"]
	untilBefore, breakerBefore, ledgerBefore := e.until, e.breakerUntil, len(e.modelCooldowns)
	p.mu.RUnlock()

	p.NoteCheckinDone("u1")

	p.mu.RLock()
	e = p.byUID["u1"]
	untilAfter, breakerAfter, ledgerAfter := e.until, e.breakerUntil, len(e.modelCooldowns)
	p.mu.RUnlock()
	if !untilAfter.Equal(untilBefore) || !breakerAfter.Equal(breakerBefore) || ledgerAfter != ledgerBefore {
		t.Errorf("NoteCheckinDone 不得触碰冷却/熔断域: until %v→%v breaker %v→%v modelCooldowns %d→%d",
			untilBefore, untilAfter, breakerBefore, breakerAfter, ledgerBefore, ledgerAfter)
	}
	if ledgerBefore != 1 || breakerBefore.IsZero() {
		t.Fatalf("前置构造失败：modelCooldowns=%d breakerUntil=%v", ledgerBefore, breakerBefore)
	}
}

func TestCheckinDonePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteCheckinDone("u1")
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"last_checkin_day"`) {
		t.Fatalf("state.json 缺少 last_checkin_day:\n%s", raw)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st, _ := p2.Status("u1"); !st.CheckinDone {
		t.Fatal("重启后当日已签状态丢失")
	}
}

func TestCheckinDoneExpiredYesterday(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteCheckinDone("u1")
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	raw = []byte(strings.Replace(string(raw), time.Now().Format("2006-01-02"), yesterday, 1))
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st, _ := p2.Status("u1"); st.CheckinDone {
		t.Fatal("昨日签到记录不应显示为今日已签")
	}
}

// TestCheckinDoneAbsentInLegacyStateFile 旧 state.json（无 last_checkin_day 字段）
// 正常加载且不误报已签（零值语义）。
func TestCheckinDoneAbsentInLegacyStateFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	legacy := `{"accounts":{"u1":{"credits":100}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("旧文件账号应被加载")
	}
	if st.CheckinDone {
		t.Errorf("旧文件无签到记录，不应显示已签: %+v", st)
	}
}
