// handler_edge_auth_test.go 边缘层 401 的**账号保全**回归（H2，处理侧）。
//
// 缺陷：无业务信封的 401 此前落 ErrClient → applyErrorPolicy 喂连败计数 NoteFailures
// → 连续 5 次（defaultDegradeThreshold）**临时出池**。但边缘层（CDN/网关/WAF）鉴权
// 拦截拦的是**整批请求**，与账号无关：实测一次 5 分钟的边缘故障把 33 个健康账号降权，
// 随后全池 503（同一批 token 在窗口过后立刻 200）。
//
// 闸门（两条对照，缺一不可）：
//   - ErrEdgeAuth 反复命中 → 账号**必须仍可用**（零账号处置）；
//   - ErrClient 反复命中 → 账号**必须被降权**（证明对照有效，也证明 NoteFailures
//     路径本身没被改坏）。
//
// ⚠️ 归属说明（实测得出，勿误解本文件的作用域）：真正**承载**本修复的是
// `upstream.Classify` 的分野——是它把边缘层 401 从 ErrClient 改成 ErrEdgeAuth，
// 从而不再走 NoteFailures。`applyErrorPolicy` 里的 `case ErrEdgeAuth:` **只是把
// 处置意图写明**（与 ErrPromptTooLong / ErrImageInvalid 两处「只为文档完备」分支
// 同理）：实测删掉该 case 后，ErrEdgeAuth 落到 default 分支也**不会**被罚（default
// 只对 `kind == ErrClient` 喂连败），故本文件无法用「删 case」证伪。所以本文件锁定
// 的是**「零处置」这个契约**（防止日后有人把 ErrEdgeAuth 接进某条惩罚路径），
// 而修复本身的证伪闸门在 `internal/upstream/edgeauth_test.go` 的
// TestClassifyEdgeAuth（已实测：去掉 Classify 的分野即红）。
package server

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// available 报告 uid 是否仍在「可用（健康）账号」集合里。
func available(p *pool.Pool, uid string) bool {
	for _, u := range p.AvailableUIDsForRealm("") {
		if u == uid {
			return true
		}
	}
	return false
}

// TestEdgeAuthDoesNotDegradeAccount 边缘层 401 反复命中**不得**让账号出池。
// 反事实：把 ErrEdgeAuth 的处理退回 default（喂 NoteFailures）时本用例必红。
func TestEdgeAuthDoesNotDegradeAccount(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	// 远超降权阈值（默认 5），确保任何累计式惩罚都会触发。
	for i := 0; i < 20; i++ {
		h.applyErrorPolicy("u1", upstream.ErrEdgeAuth, `<html>401 Authorization Required</html>`, "", nil)
	}

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号不在池中")
	}
	if st.Disabled {
		t.Fatalf("边缘层拦截与账号无关，绝不能禁用：%+v", st)
	}
	if st.Cooling {
		t.Fatalf("边缘层拦截不应给账号冷却（该形态由 IP 级 fail-fast 处理）：%+v", st)
	}
	if !available(p, "u1") {
		t.Fatalf("边缘层 401 反复命中后账号被降权出池（基础设施故障记在了账号头上）：%+v", st)
	}
}

// TestErrClientStillDegradesAccount 对照组：语义不明的 4xx 仍**必须**降权——
// 证明上一条不是因为「NoteFailures 路径整体失效」而通过的。
func TestErrClientStillDegradesAccount(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	for i := 0; i < 20; i++ {
		h.applyErrorPolicy("u1", upstream.ErrClient, `{"code":40001,"msg":"unknown"}`, "", nil)
	}
	if available(p, "u1") {
		t.Fatal("ErrClient 连续命中达阈值后必须降权出池（对照组失效：说明 NoteFailures 路径被改坏了）")
	}
}

// TestEdgeAuthEndToEndViaClassify 端到端：真实边缘拦截 body 经 Classify 得到
// ErrEdgeAuth，且处理后账号仍可用。反事实：去掉 Classify 的边缘层判定（退回
// ErrClient）时，这条会把账号降权 → 必红。
func TestEdgeAuthEndToEndViaClassify(t *testing.T) {
	for _, body := range []string{"", "<html>401</html>", "Unauthorized"} {
		p := pool.New("")
		p.Add(&auth.Auth{UID: "u1"})
		h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

		kind := upstream.Classify(401, body)
		if kind != upstream.ErrEdgeAuth {
			t.Fatalf("边缘层 401 应归 ErrEdgeAuth，got %v（body=%.40q）", kind, body)
		}
		for i := 0; i < 20; i++ {
			h.applyErrorPolicy("u1", kind, body, "", nil)
		}
		if !available(p, "u1") {
			t.Fatalf("端到端：边缘层 401 不得让账号出池（body=%.40q）", body)
		}
	}
}

// TestEdgeAuthEnvelopeStillSessionDead 带信封的 401（12153）**必须仍然**禁用：
// 分野不能把真实 session 终态也放过——那种号留在池里会被反复选中、反复失败。
func TestEdgeAuthEnvelopeStillSessionDead(t *testing.T) {
	const body = `{"code":12153,"msg":"Offline user session not found"}`
	if kind := upstream.Classify(401, body); kind != upstream.ErrSessionDead {
		t.Fatalf("带信封的 12153 必须仍归 ErrSessionDead，got %v", kind)
	}

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})
	h.applyErrorPolicy("u1", upstream.ErrSessionDead, body, "", nil)

	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatalf("真实 session 终态必须仍被禁用：%+v", st)
	}
}

// TestEdgeGateTripsOnMultipleAccounts IP 级 fail-fast：短窗内**不同**账号接连命中
// 边缘层拒绝 → 判定出口被拦，终止轮转（否则轮转会把一次请求放大成整池命中）。
func TestEdgeGateTripsOnMultipleAccounts(t *testing.T) {
	oldWin := wafIPWindow
	wafIPWindow = 60 * time.Second
	t.Cleanup(func() { wafIPWindow = oldWin })

	var g wafIPGate
	if g.noteWaf("u1") {
		t.Fatal("单个账号命中不得判定 IP 级拦截（账号级偶发归账号处置）")
	}
	if !g.noteWaf("u2") {
		t.Fatal("两个**不同**账号在窗内接连命中边缘层拒绝 → 必须判定 IP 级并 fail-fast")
	}
	if !g.active() {
		t.Error("判定后 gate 应处于激活态")
	}
}
