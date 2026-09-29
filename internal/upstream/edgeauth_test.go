// edgeauth_test.go 边缘层 401 的分类回归（H2）。
//
// 缺陷：无业务信封的 401 此前落通用 4xx 兜底（ErrClient）→ applyErrorPolicy 对每个
// 命中账号喂连败计数 NoteFailures → 连续 N 次**临时出池**。但边缘层（CDN/网关/WAF）
// 鉴权拦截拦的是**整批请求**、与账号无关（实测：同一批 token 在拦截窗口内全 401，
// 窗口过后立刻 200）。一次 5 分钟的边缘故障就能把整池健康账号降权一遍（实测 33 个），
// 随后 503——**把基础设施故障记在账号头上**。
//
// 本文件锁定分类侧的分野。处理侧（零账号处置 + IP 级 fail-fast）见
// internal/server/handler_edge_auth_test.go。
//
// 顺带补上 ErrWafBlock 的分类用例——此前**没有任何**测试覆盖 WAF 403 形态，
// 而它是本判据的对称参照物。
package upstream

import (
	"net/http"
	"strings"
	"testing"
)

func TestClassifyEdgeAuth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		// —— 边缘层形态（无业务信封）→ ErrEdgeAuth ——
		{"空体 401", http.StatusUnauthorized, "", ErrEdgeAuth},
		{"HTML 拦截页", http.StatusUnauthorized, `<html><body>401 Unauthorized</body></html>`, ErrEdgeAuth},
		{"纯文本", http.StatusUnauthorized, "Unauthorized", ErrEdgeAuth},
		{"nginx 风格", http.StatusUnauthorized, "<html>\r\n<head><title>401 Authorization Required</title></head>", ErrEdgeAuth},
		{"JSON 但无 code/msg 字段", http.StatusUnauthorized, `{"error":"unauthorized"}`, ErrEdgeAuth},

		// —— 账号/session 真实状态（带业务信封）→ 必须**不**被判成边缘层 ——
		// 12153 是权威的 session 终态，短冷却救不活，必须 Disable。
		{"带信封 + 12153", http.StatusUnauthorized, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		// 带信封但无已知 marker：语义不明，保持既有 ErrClient（不放过、也不冒充边缘层）。
		{"带信封但无 marker", http.StatusUnauthorized, `{"code":40001,"msg":"some unknown auth error"}`, ErrClient},

		// —— 对称参照：403 形态不受本次改动影响 ——
		{"空体 403 仍是 WAF", http.StatusForbidden, "", ErrWafBlock},
		{"HTML 403 仍是 WAF", http.StatusForbidden, `<html>403 Forbidden</html>`, ErrWafBlock},
		{"带信封 403 仍走分类链", http.StatusForbidden, `{"code":11140,"msg":"request illegal"}`, ErrAccountFault},

		// —— 状态码边界：只有 401/403 命中各自形态 ——
		{"402 不受影响", http.StatusPaymentRequired, "", ErrHardCredit},
		{"空体 500 仍是服务端错误", http.StatusInternalServerError, "", ErrServer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.status, tc.body); got != tc.want {
				t.Errorf("Classify(%d, %.60q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

func TestIsEdgeAuthFailureSymmetry(t *testing.T) {
	// 判据与 IsWafBlocked 完全对称，只是状态码不同——这条对称性本身就是设计意图，
	// 用测试钉住，避免后人只改一边。
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		for _, body := range []string{"", "<html>x</html>", "plain"} {
			waf := IsWafBlocked(status, body)
			edge := IsEdgeAuthFailure(status, body)
			if status == http.StatusForbidden && (!waf || edge) {
				t.Errorf("403 无信封：waf=%v edge=%v（应 waf=true edge=false）body=%q", waf, edge, body)
			}
			if status == http.StatusUnauthorized && (!edge || waf) {
				t.Errorf("401 无信封：waf=%v edge=%v（应 edge=true waf=false）body=%q", waf, edge, body)
			}
		}
	}
	// 带信封一律不判边缘层（两侧同口径）。
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		body := `{"code":1,"msg":"x"}`
		if IsEdgeAuthFailure(status, body) || IsWafBlocked(status, body) {
			t.Errorf("带业务信封不得判为边缘/WAF 形态：status=%d body=%s", status, body)
		}
	}
}

func TestErrKindStringEdgeAuth(t *testing.T) {
	// String() 是日志/面板/客户端可见 code 的稳定契约，必须有测试锁定。
	if got := ErrEdgeAuth.String(); got != "edge_auth" {
		t.Errorf("ErrEdgeAuth.String() = %q, want %q", got, "edge_auth")
	}
	if got := ErrWafBlock.String(); got != "waf_block" {
		t.Errorf("ErrWafBlock.String() = %q, want %q", got, "waf_block")
	}
}

func TestGatewayHintEdgeAuth(t *testing.T) {
	hint := GatewayHint(ErrEdgeAuth, "", HintContext{})
	if hint == "" {
		t.Fatal("边缘层 401 必须有客户端可见 hint（否则客户端只知道 401，会去改本来正确的 key）")
	}
	// 关键：不得把基础设施拦截误导成凭证问题。
	low := strings.ToLower(hint)
	for _, bad := range []string{"api key", "api_key", "credential is invalid", "check your key"} {
		if strings.Contains(low, bad) {
			t.Errorf("hint 不得把边缘拦截说成凭证问题：%q 含 %q", hint, bad)
		}
	}
	// WAF 与边缘层对客户端的动作一致（等窗口过去再试），hint 都应给出该指引。
	if !strings.Contains(strings.ToLower(GatewayHint(ErrWafBlock, "", HintContext{})), "retry") {
		t.Error("WAF hint 应包含重试指引")
	}
	if !strings.Contains(low, "retry") {
		t.Error("边缘层 hint 应包含重试指引")
	}
}
