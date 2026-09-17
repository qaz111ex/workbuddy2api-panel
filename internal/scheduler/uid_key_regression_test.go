package scheduler

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// realUID 生产形态的 uid（上游真实 uid 为 54 位 hex；此处取 40 位 hex 等效验证）。
// 回归锚：scheduler 曾经把 logfmt.Label(uid, nick)（"昵称(uid8)"）当作 Pool 的
// map 查询键，而 byUID 以**原始 uid** 建键 —— 生产账号全部查不到，签到/保活/余额/
// 旅行/活动/开学季/连登/黑猫静默 no-op。既有测试全用 "u1"/"u2"（≤8 且无昵称，
// Label 恰好等于原 uid）把该缺陷完全掩盖。
const realUID = "5f9692923c93033111c51ad7b003eb80204a9b75"

// TestSchedulerUsesRawUIDAsPoolKey 用「长 uid + 非空昵称」锚定调度器与池的键口径：
// 所有池查询/状态方法必须收到原始 uid，否则本测试会在每个任务上失败。
func TestSchedulerUsesRawUIDAsPoolKey(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 777}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{
		UID:          realUID,
		AccessToken:  "at",
		RefreshToken: "rt",
		ExpiresAt:    9999999999,
		Nickname:     "示例昵称甲",
	}
	p.Add(a)
	p.Cooldown(realUID, pool.CoolHard, time.Hour, "余额不足")

	up := &upstream.Client{
		HTTP:          srv.Client(),
		ChatBaseCN:    srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   []int{9, 21},
		KeepaliveHours: []int{22},
	})

	// 1) 签到：必须真正走到上游（fake 的 checkinCalls 计数）+ 余额写回池。
	s.RunCheckinNow()
	if got := f.checkinCalls.Load(); got != 1 {
		t.Fatalf("checkin calls=%d want 1（调度器未用原始 uid 取号 → 账号被跳过）", got)
	}
	st, ok := p.Status(realUID)
	if !ok {
		t.Fatal("account missing from pool")
	}
	if st.Credits != 777 {
		t.Fatalf("credits=%d want 777（SetCreditsDetailed 收到了错误的 uid）", st.Credits)
	}
	if st.Cooling {
		t.Fatalf("account should be reenabled after checkin: %+v", st)
	}

	// 2) 保活：token 过期 → 必须真的刷新（fake refreshCalls 计数）。
	//    池内 Auth 由 Pool.List() 返回的是同一指针，直接改其过期时间即可触发刷新路径。
	a.ExpiresAt = 1
	s.RunKeepaliveNow()
	if got := f.refreshCalls.Load(); got == 0 {
		t.Fatalf("refresh calls=0（keepalive 未用原始 uid 取号 → 账号被跳过）")
	}

	// 3) 池查询口径自检：Label 形态**不得**能查到账号（防止回归再次引入）。
	if got := p.AuthByUID(realUID); got == nil {
		t.Fatal("AuthByUID(raw uid) must resolve")
	}
}

// TestSchedulerAuthByUIDRejectsLabelKey 反向锚定：把 logfmt.Label 形态喂给池查询
// 必须查不到 —— 明确固定「日志标签 ≠ 池键」这一契约（任何人再把 Label 传给
// AuthByUID 都会让上面的正向测试失败，本测试则固定其语义）。
func TestSchedulerAuthByUIDRejectsLabelKey(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: realUID, AccessToken: "at", ExpiresAt: 9999999999, Nickname: "示例昵称甲"})

	if got := p.AuthByUID(realUID); got == nil {
		t.Fatal("AuthByUID(raw uid) must resolve")
	}
	label := "示例昵称甲(" + realUID[:8] + ")"
	if got := p.AuthByUID(label); got != nil {
		t.Fatalf("AuthByUID(label=%q) unexpectedly resolved — 池键必须是原始 uid", label)
	}
}

// 保证 httptest 引用被使用（部分构建配置下避免 unused import）。
var _ = httptest.NewServer
var _ = http.MethodGet
