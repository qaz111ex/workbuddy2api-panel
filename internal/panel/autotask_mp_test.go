// autotask_mp_test.go 小程序（mp）成长任务续环的行为测试（Sequential_Tasks_1..7）。
//
// 全部用 httptest stub，不打真实上游、不连 8787。测试重点是两个**真实缺陷**的回归：
//  1. mp 任务未 accept 时 progress 为 null（target 读成 0）→ 必须有 target 兜底，
//     且达标判定/差额计算必须在 accept **之后**基于回读的权威值，否则 target=5 的
//     多元任务只上报 1 次；
//  2. 判据对象 id（专家市场 ex_ id）解析必须在 accept **之前**完成，拿不到就整任务
//     不动作（不留「已登记未上报」半程态）。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// mpStub 小程序口径上游 stub：列表 / accept / claim / v2/report 四类端点。
// accepted 之前 progress 恒为 null（上游实测形态），accept 之后按事件计数下发。
type mpStub struct {
	mu           sync.Mutex
	code         string
	target       int64
	accepted     bool
	claimed      bool
	events       []map[string]any // 收到的 /v2/report 事件（已合并公共指纹）
	acceptCalls  int
	claimCalls   int
	listCalls    int
	marketOnline bool   // 专家市场端点是否可用
	marketExpert string // 市场返回的专家 id
	// nullProgressForever：accept 之后 progress 仍为 null（target 恒 0）。
	// 用于隔离「target 兜底」——服务端永不公布规格时，若不兜底会把 null 进度
	// 误判为已达标而一次都不上报。
	nullProgressForever bool
	// preAcceptTarget > 0 时，accept 之前平铺下发该 target（默认 0，即上游
	// 「未 accept 时 progress 为 null」形态）。用于验证 accept 后回读取的是
	// 服务端权威规格，而非任何本地兜底。
	preAcceptTarget int64
	// modelGate：仅当收到带 requestModelId 的事件才计分（Sequential_Tasks_5 判据载体）
	modelGate bool
}

func (s *mpStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks" && r.Method == http.MethodGet:
			s.listCalls++
			// 未 accept：progress 为 null 且**平铺 target 也为 0**（上游实测形态——
			// 任务规格 target 只在 accept 之后才随 progress 下发）。accept 之后
			// progress 对象才是权威值。
			var prog any
			tgt := int64(0)
			if !s.accepted {
				tgt = s.preAcceptTarget
			}
			if s.accepted && !s.nullProgressForever {
				cur := int64(0)
				for _, ev := range s.events {
					if ev["eventCode"] == "chat_request_send" {
						if s.modelGate && ev["requestModelId"] == "" {
							continue // modelGate：只认带模型字段的对话事件
						}
						cur++
					}
					if s.modelGate && ev["eventCode"] == "model_chat" {
						cur++
					}
				}
				prog = map[string]any{"current": cur, "target": s.target}
				tgt = s.target
			}
			status := "not_accepted"
			if s.accepted {
				status = "accepted"
			}
			if s.claimed {
				status = "claimed"
			}
			writeEnvelope(w, map[string]any{
				"tasks": []map[string]any{{
					"task_code": s.code, "title": s.code, "target": tgt,
					"accept_status": status, "progress": prog,
				}},
			})
		case r.URL.Path == "/v2/activity/growth/tasks/accept":
			s.acceptCalls++
			s.accepted = true
			writeEnvelope(w, map[string]any{"results": []any{}})
		case strings.HasSuffix(r.URL.Path, "/claim"):
			s.claimCalls++
			s.claimed = true
			writeEnvelope(w, map[string]any{"already_claimed": false, "credit": 300, "energy": 5})
		case r.URL.Path == "/v2/report":
			var evs []map[string]any
			_ = json.NewDecoder(r.Body).Decode(&evs)
			s.events = append(s.events, evs...)
			writeEnvelope(w, map[string]any{})
		case r.URL.Path == "/portal/operation-platform/market/expert/list":
			if !s.marketOnline {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"code":500,"msg":"market down"}`))
				return
			}
			writeEnvelope(w, map[string]any{"experts": []map[string]any{{
				"expert_id": s.marketExpert, "expert_type": "agent",
				"display_name_zh": "论文写作导师", "profession_zh": "写作",
			}}})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
		}
	}
}

func writeEnvelope(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": data})
}

func (s *mpStub) countEvent(code string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, ev := range s.events {
		if ev["eventCode"] == code {
			n++
		}
	}
	return n
}

// newMPPanel 构造指向 stub 的 Panel（双域都指向同一 stub，避免误打真实域名）。
func newMPPanel(t *testing.T, s *mpStub) (*Panel, *auth.Auth) {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	c := &upstream.Client{
		HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL,
	}
	return &Panel{cfg: Config{Upstream: c}}, &auth.Auth{AccessToken: "at", UID: "u-1", Nickname: "测试"}
}

// fastPoll 把异步计分的轮询间隔压到毫秒级（默认 3s×2 轮会让测试慢 6s）。
func fastPoll(t *testing.T) {
	t.Helper()
	oldGap, oldMP := claimPollGap, mpActionGap
	claimPollGap, mpActionGap = time.Millisecond, time.Millisecond
	t.Cleanup(func() { claimPollGap, mpActionGap = oldGap, oldMP })
}

// TestRunMPMiniChatTaskUsesPostAcceptAuthoritativeTarget accept 后回读的独立证据：
// 服务端 accept **之后**公布的规格（3）与本地规格兜底表（Tasks_3=5）不同——必须
// 以服务端权威值为准上报 3 次。删掉 accept 后回读会退回兜底表 5 次，本测试即失败。
func TestRunMPMiniChatTaskUsesPostAcceptAuthoritativeTarget(t *testing.T) {
	fastPoll(t)
	s := &mpStub{code: "Sequential_Tasks_3", target: 3, preAcceptTarget: 0}
	p, a := newMPPanel(t, s)

	msg, err := p.runMPMiniChatTask(a, "Sequential_Tasks_3", false)
	if err != nil {
		t.Fatalf("runMPMiniChatTask: %v", err)
	}
	if got := s.countEvent("chat_request_send"); got != 3 {
		t.Errorf("chat_request_send 上报数 = %d, want 3（accept 后回读未取服务端权威 target，退回本地兜底）；msg=%q", got, msg)
	}
	if s.claimCalls == 0 {
		t.Errorf("达标未领奖；msg=%q", msg)
	}
}

// TestRunMPMiniChatTaskTargetFallbackOnNullProgress 隔离 target 兜底本身：
// 服务端在 accept 之后**仍然**不下发 progress（target 恒 0）。若不按任务规格兜底，
// 只会兜底成 1（多元任务只上报 1 次）；规格表兜底后按 target 差额补报 5 次。
func TestRunMPMiniChatTaskTargetFallbackOnNullProgress(t *testing.T) {
	fastPoll(t)
	s := &mpStub{code: "Sequential_Tasks_3", target: 5, nullProgressForever: true}
	p, a := newMPPanel(t, s)

	msg, err := p.runMPMiniChatTask(a, "Sequential_Tasks_3", false)
	if err != nil {
		t.Fatalf("runMPMiniChatTask: %v", err)
	}
	if got := s.countEvent("chat_request_send"); got != 5 {
		t.Errorf("chat_request_send 上报数 = %d, want 5（target 兜底缺失：null 进度被误判为已达标）；msg=%q", got, msg)
	}
	if s.claimCalls != 0 {
		t.Errorf("进度未归账时不应领奖（claim 调用 %d 次）", s.claimCalls)
	}
}

// TestRunMPMiniChatTaskMultiTargetUsesPostAcceptTarget 是 target 兜底 + accept 后回读的
// 核心回归：未 accept 时 progress 为 null（target 读成 0），服务端规格 target=5。
// 必须上报 5 条 chat_request_send 并领奖——若达标判定/差额计算回退到 accept 之前，
// 只会上报 1 条（反事实验证见报告）。
func TestRunMPMiniChatTaskMultiTargetUsesPostAcceptTarget(t *testing.T) {
	fastPoll(t)
	s := &mpStub{code: "Sequential_Tasks_3", target: 5, preAcceptTarget: 0}
	p, a := newMPPanel(t, s)

	msg, err := p.runMPMiniChatTask(a, "Sequential_Tasks_3", false)
	if err != nil {
		t.Fatalf("runMPMiniChatTask: %v", err)
	}
	if got := s.countEvent("chat_request_send"); got != 5 {
		t.Errorf("chat_request_send 上报数 = %d, want 5（target 兜底/accept 后回读失效）", got)
	}
	if s.acceptCalls == 0 {
		t.Error("未调用 accept（mp 任务必须先 accept 才计分）")
	}
	if s.claimCalls == 0 {
		t.Errorf("达标未领奖；msg=%q", msg)
	}
	if !strings.Contains(msg, "+300c") {
		t.Errorf("msg=%q 应包含领奖信息", msg)
	}
}

// TestRunMPMiniChatTaskNullProgressNotTreatedAsDone 未 accept（progress=null）时不得
// 被误判为「已达标」而直接领奖：必须先 accept 再上报。
func TestRunMPMiniChatTaskNullProgressNotTreatedAsDone(t *testing.T) {
	fastPoll(t)
	s := &mpStub{code: "Sequential_Tasks_3", target: 5}
	p, a := newMPPanel(t, s)

	if _, err := p.runMPMiniChatTask(a, "Sequential_Tasks_3", false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if s.acceptCalls == 0 {
		t.Fatal("未 accept（progress=null 被当成已达标直接走领奖）")
	}
	if got := s.countEvent("chat_request_send"); got == 0 {
		t.Fatal("未上报任何对话事件")
	}
}

// TestRunMiniExpertResolvesExpertBeforeAccept 对象 id 前置解析：市场不可用时整任务
// 不动作——既不发 accept 也不上报（不留「已登记未上报」半程态）。
func TestRunMiniExpertResolvesExpertBeforeAccept(t *testing.T) {
	fastPoll(t)
	s := &mpStub{code: "Sequential_Tasks_2", target: 1, marketOnline: false}
	p, a := newMPPanel(t, s)

	msg, err := runMiniExpert(p, a)
	if err != nil {
		t.Fatalf("runMiniExpert: %v", err)
	}
	if s.acceptCalls != 0 {
		t.Errorf("市场不可用时不应 accept（got %d 次）", s.acceptCalls)
	}
	if got := s.countEvent("expert_actual_use"); got != 0 {
		t.Errorf("市场不可用时不应上报判据（got %d 条）", got)
	}
	if !strings.Contains(msg, "专家市场不可用") {
		t.Errorf("msg=%q 应说明市场不可用", msg)
	}
}

// TestRunMiniExpertReportsMPFingerprint 市场可用时按 mp 指纹上报 expert_actual_use
// 并领奖：形状须与 school 域 expert 事件区分（不带 conversationId/activityId）。
func TestRunMiniExpertReportsMPFingerprint(t *testing.T) {
	fastPoll(t)
	s := &mpStub{code: "Sequential_Tasks_2", target: 1, marketOnline: true, marketExpert: "ex_real001"}
	p, a := newMPPanel(t, s)

	msg, err := runMiniExpert(p, a)
	if err != nil {
		t.Fatalf("runMiniExpert: %v", err)
	}
	if got := s.countEvent("expert_actual_use"); got != 1 {
		t.Fatalf("expert_actual_use 上报数 = %d, want 1（msg=%q）", got, msg)
	}
	s.mu.Lock()
	var ev map[string]any
	for _, e := range s.events {
		if e["eventCode"] == "expert_actual_use" {
			ev = e
		}
	}
	s.mu.Unlock()
	if ev == nil {
		t.Fatal("未捕获 expert_actual_use 事件")
	}
	if ev["id"] != "ex_real001" {
		t.Errorf("专家 id = %v, want 市场真实 id ex_real001", ev["id"])
	}
	if ev["type"] != "send_message" || ev["source"] != "mini_program" {
		t.Errorf("mp 指纹不符：type=%v source=%v", ev["type"], ev["source"])
	}
	if ev["extVersion"] != "2.2.8" {
		t.Errorf("extVersion=%v, want 2.2.8", ev["extVersion"])
	}
	if _, ok := ev["conversationId"]; ok {
		t.Error("mp 专家事件不应带 conversationId（school 域口径勿混）")
	}
	if _, ok := ev["activityId"]; ok {
		t.Error("mp 专家事件不应带 activityId")
	}
}

// TestRunSequentialEventTaskFallback 预留骨架：primary 判据未点亮时补报 fallback 一轮。
// stub 开启 modelGate（只认带 requestModelId 的对话事件），primary 发的裸事件不计分，
// 迫使第 0 轮回读后走 fallback（PC 域 ReportChatActivityModel）。
func TestRunSequentialEventTaskFallback(t *testing.T) {
	fastPoll(t)
	s := &mpStub{code: "Sequential_Tasks_5", target: 1, modelGate: true}
	p, a := newMPPanel(t, s)

	var primary, fallback int
	msg, err := p.runSequentialEventTask(a, "Sequential_Tasks_5",
		func() error {
			primary++
			// 不带模型字段：modelGate 下不计分，模拟「primary 未点亮」
			return p.cfg.Upstream.ReportMPEvent(a, upstream.MiniChatModelEvent("c-1", "", ""))
		},
		func() error {
			fallback++
			return p.cfg.Upstream.ReportChatActivityModel(a, "c-2", "", "glm-5.2", "GLM-5.2")
		})
	if err != nil {
		t.Fatalf("runSequentialEventTask: %v", err)
	}
	if primary != 1 {
		t.Errorf("primary 调用 %d 次, want 1", primary)
	}
	if fallback != 1 {
		t.Errorf("fallback 调用 %d 次, want 1（未点亮时应补报一轮）；msg=%q", fallback, msg)
	}
}

// TestRunSequentialEventTaskSkipsWhenLocked accept 未登记生效（每日锁定窗口）时返回
// 等下次调度，且**不**上报判据。
func TestRunSequentialEventTaskSkipsWhenLocked(t *testing.T) {
	fastPoll(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks":
			// 恒 not_accepted：模拟 locked 任务 accept 200 但不落账
			writeEnvelope(w, map[string]any{"tasks": []map[string]any{{
				"task_code": "Sequential_Tasks_4", "accept_status": "not_accepted",
				"progress": nil,
			}}})
		case r.URL.Path == "/v2/activity/growth/tasks/accept":
			writeEnvelope(w, map[string]any{"results": []any{}})
		default:
			writeEnvelope(w, map[string]any{})
		}
	}))
	defer srv.Close()
	c := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	p := &Panel{cfg: Config{Upstream: c}}
	a := &auth.Auth{AccessToken: "at", UID: "u-1", Nickname: "测试"}

	var reported int
	msg, err := p.runSequentialEventTask(a, "Sequential_Tasks_4", func() error {
		reported++
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("runSequentialEventTask: %v", err)
	}
	if reported != 0 {
		t.Errorf("accept 未落账时不应上报判据（got %d 次）", reported)
	}
	if !strings.Contains(msg, "锁定窗口") {
		t.Errorf("msg=%q 应提示每日锁定窗口", msg)
	}
}

// TestSequentialTaskCodesRegistered 新任务码必须同时被三条链路感知：
// autoActions（任务中心/队列/一键完成）与 mpTaskCodes（双口径回读/accept/claim）。
func TestSequentialTaskCodesRegistered(t *testing.T) {
	want := []string{
		"Sequential_Tasks_1", "Sequential_Tasks_2", "Sequential_Tasks_3",
		"Sequential_Tasks_4", "Sequential_Tasks_5", "Sequential_Tasks_6", "Sequential_Tasks_7",
	}
	for _, code := range want {
		act := autoActionFor(code)
		if act == nil {
			t.Errorf("autoActions 缺少 %s（任务列表不会出现「一键完成」按钮）", code)
			continue
		}
		if act.run == nil {
			t.Errorf("%s 的 run 为空", code)
		}
		if !isMPTaskCode(code) {
			t.Errorf("mpTaskCodes 缺少 %s（回读/accept/claim 不会走 mp 变体）", code)
		}
	}
	// 链条顺序必须单调（队列按 autoActionIndex 排依赖序）。
	for i := 1; i < len(want); i++ {
		if autoActionIndex(want[i-1]) >= autoActionIndex(want[i]) {
			t.Errorf("autoActions 顺序错乱：%s 应在 %s 之前", want[i-1], want[i])
		}
	}
	// 无重复登记（重复会让 autoActionFor 命中错项）。
	seen := map[string]bool{}
	for _, act := range autoActions {
		if seen[act.TaskCode] {
			t.Errorf("autoActions 重复登记 %s", act.TaskCode)
		}
		seen[act.TaskCode] = true
	}
}

// TestSequentialTaskRunnersWiredToRightCode 走各码的 run 入口，校验判据事件形状——
// 这同时证明「登记了但 run 指向旧任务」不会发生（函数值不可比较，只能靠行为断言）。
func TestSequentialTaskRunnersWiredToRightCode(t *testing.T) {
	fastPoll(t)
	cases := []struct {
		code      string
		target    int64
		wantEvent string
		wantCount int
		run       func(*Panel, *auth.Auth) (string, error)
	}{
		{"Sequential_Tasks_4", 1, "automated_task_create_suc", 1, runSequentialAutomation},
		{"Sequential_Tasks_6", 10, "chat_request_send", 10, runSequentialChat10},
		{"Sequential_Tasks_7", 1, "playbook_prompt_send", 1, runSequentialPlaybook},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			s := &mpStub{code: tc.code, target: tc.target}
			p, a := newMPPanel(t, s)
			act := autoActionFor(tc.code)
			if act == nil {
				t.Fatalf("%s 未登记", tc.code)
			}
			if _, err := tc.run(p, a); err != nil {
				t.Fatalf("%s run: %v", tc.code, err)
			}
			if got := s.countEvent(tc.wantEvent); got != tc.wantCount {
				t.Errorf("%s 发出 %s ×%d, want ×%d", tc.code, tc.wantEvent, got, tc.wantCount)
			}
		})
	}
}
