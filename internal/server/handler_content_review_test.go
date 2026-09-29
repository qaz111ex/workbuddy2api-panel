// handler_content_review_test.go 11140「内容审核 vs 账号封禁」分野的**账号保全**回归。
//
// 线上事故（2026-09-28，上游两个独立 fork 同日实测）：上游用**同一个**
// code 11140 + msg "request illegal" 同时表达
//   - 账号级授权封禁（真封号，需重登）；
//   - 内容审核拒绝（displayMsg "内容未通过安全审核"/"did not pass the safety review"）。
//
// 本仓库此前 accountFaultMarkers 只认裸 "request illegal" → 审核形态被判成
// ErrAccountFault → 此处 Pool.Disable **永久禁用健康账号**。
//
// 本仓库还**主动触发**该形态：夜猫子任务给每个账号发**一字不差**的同一句话
// （"1+1等于几？直接回答。"），大量账号从同 IP 同文本批量请求，正是审核最容易
// 命中的指纹。故这不是理论风险——它每晚都在发生，且误禁不可逆（需人工重登）。
//
// 闸门：审核形态必须**不** Disable（软冷却或零动作）；纯 request illegal 必须
// **仍然** Disable（分野不能把真封号也放过，否则该禁的号永不出池）。
package server

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 上游真实形态（en/zh displayMsg 双语文案）。
const (
	bodyReviewEN = `{"code":11140,"msg":"request illegal","displayMsg":{"en":"The content did not pass the safety review. Please adjust and retry.","zh":"内容未通过安全审核，请调整后重试"}}`
	bodyReviewZH = `{"code":11140,"msg":"request illegal","displayMsg":{"zh":"内容未通过安全审核"}}`
	bodyPureBan  = `{"code":11140,"msg":"request illegal"}`
)

// TestContentReviewIsNotAccountBan 端到端：审核 body 经 Classify 得到的分野必须是
// 「不罚号」的类别，且 applyErrorPolicy 绝不 Disable。
// 反事实：去掉 Classify 的分野（或去掉 handler 二道闸）时，账号会被永久禁用 → 本用例必红。
func TestContentReviewIsNotAccountBan(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"en 审核文案", bodyReviewEN},
		{"zh 审核文案", bodyReviewZH},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 第一层：分类必须落到「不罚号」的类别（而非 ErrAccountFault）。
			kind := upstream.Classify(403, tc.body)
			if kind == upstream.ErrAccountFault {
				t.Fatalf("审核形态被误判为账号故障（会被 Pool.Disable 永久禁用）：kind=%v", kind)
			}

			p := pool.New("")
			p.Add(&auth.Auth{UID: "u1"})
			h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})
			h.applyErrorPolicy("u1", kind, tc.body, "", nil)

			st, ok := p.Status("u1")
			if !ok {
				t.Fatal("账号不在池中")
			}
			if st.Disabled {
				t.Fatalf("内容审核拒绝是请求级问题，绝不能永久禁用账号：%+v", st)
			}
		})
	}
}

// TestContentReviewHandlerDefenceRegardlessOfKind 第二道闸：**即使**调用方把审核
// body 误标成 ErrAccountFault（模拟上游换措辞/分类被绕过），handler 也必须拒绝 Disable。
// 失效方向必须是「少禁号」，不能是「多禁号」——误禁不可逆，误冷却只是短暂少一个号。
func TestContentReviewHandlerDefenceRegardlessOfKind(t *testing.T) {
	for _, body := range []string{bodyReviewEN, bodyReviewZH} {
		p := pool.New("")
		p.Add(&auth.Auth{UID: "u1"})
		h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

		// 刻意传入 ErrAccountFault（最坏情形）。
		h.applyErrorPolicy("u1", upstream.ErrAccountFault, body, "", nil)

		st, ok := p.Status("u1")
		if !ok {
			t.Fatal("账号不在池中")
		}
		if st.Disabled {
			t.Fatalf("二道闸失效：即使 kind=ErrAccountFault，审核 body 也不得 Disable：body=%.90s st=%+v", body, st)
		}
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Errorf("审核形态应走软冷却（可自愈）：%+v", st)
		}
	}
}

// TestPureRequestIllegalStillBans **防过宽**：分野不能把真封号放过。
// 纯 request illegal（无审核 displayMsg）必须仍然永久禁用——否则该出池的坏号
// 会一直被选中、一直被轮转到，把请求预算耗在死号上。
func TestPureRequestIllegalStillBans(t *testing.T) {
	if kind := upstream.Classify(403, bodyPureBan); kind != upstream.ErrAccountFault {
		t.Fatalf("纯 request illegal 应为 ErrAccountFault（真封号），got %v", kind)
	}

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})
	h.applyErrorPolicy("u1", upstream.ErrAccountFault, bodyPureBan, "", nil)

	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("纯 request illegal 必须仍然 Disable（真封号不能因分野被放过）：%+v", st)
	}
}

// TestIsContentReviewBodySingleSource 判据单一事实源：handler 侧用的
// upstream.IsContentReviewBody 必须与 Classify 的分野口径一致（否则两处 marker 漂移，
// 而漂移的代价是永久误禁）。
func TestIsContentReviewBodySingleSource(t *testing.T) {
	for _, body := range []string{bodyReviewEN, bodyReviewZH} {
		if !upstream.IsContentReviewBody(body) {
			t.Errorf("IsContentReviewBody 未识别审核形态: %.80s", body)
		}
		if got := upstream.Classify(403, body); got != upstream.ErrContentBlocked {
			t.Errorf("Classify 未把审核形态归 ErrContentBlocked: got %v", got)
		}
	}
	for _, body := range []string{bodyPureBan, body14017} {
		if upstream.IsContentReviewBody(body) {
			t.Errorf("IsContentReviewBody 误报非审核 body: %.80s", body)
		}
	}
}
