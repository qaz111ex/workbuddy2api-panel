// autotask_mp_pace_test.go mp 对话事件「真人节奏」的行为闸门（upstream c675297）。
//
// 缺陷背景：上游对 chat_request_send 有节奏反作弊——数秒级连发的事件**先被计入
// 进度**、随后被整体判无效回滚，claim 返回 400 "task not completed"（进度对但一条
// 不落账）。修复 = 每条上报前等待 mpChatEventDelay()（45s + 0~10s 抖动，首条也等）。
//
// 这里用「上报到达时间戳」而不是「睡了多久」断言，才能锁住**顺序**：连发（无 sleep）
// 时相邻上报间隔是亚毫秒级，用例必红。
package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// paceStub 只服务对话事件补报链路的四类端点，并记录每条 /v2/report 的到达时刻。
type paceStub struct {
	mu       sync.Mutex
	target   int64
	accepted bool
	claimed  bool
	events   int
	reportAt []time.Time
}

func (s *paceStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.URL.Path == "/v2/activity/growth/tasks" && r.Method == http.MethodGet:
			// 上游实测形态：未 accept 时 progress 为 null 且平铺 target=0；
			// accept 之后 progress 对象才是权威值。
			status := "not_accepted"
			var prog any
			tgt := int64(0)
			if s.accepted {
				status = "accepted"
				prog = map[string]any{"current": s.events, "target": s.target}
				tgt = s.target
			}
			if s.claimed {
				status = "claimed"
			}
			writeEnvelope(w, map[string]any{"tasks": []map[string]any{{
				"task_code": "Sequential_Tasks_3", "title": "5 次有效对话",
				"target": tgt, "accept_status": status, "progress": prog,
			}}})
		case r.URL.Path == "/v2/activity/growth/tasks/accept":
			s.accepted = true
			writeEnvelope(w, map[string]any{"results": []any{}})
		case strings.HasSuffix(r.URL.Path, "/claim"):
			s.claimed = true
			writeEnvelope(w, map[string]any{"already_claimed": false, "credit": 300, "energy": 5})
		case r.URL.Path == "/v2/report":
			s.reportAt = append(s.reportAt, time.Now())
			s.events++
			writeEnvelope(w, map[string]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":404,"msg":"not found"}`))
		}
	}
}

// TestMPChatEventGapIsHumanPace 节奏参数本身的下界（上游 2026-09-26 实测结论）：
// 数秒级连发被判无效，45s 间隔逐条上报全存活；且必须有抖动打散等距机器指纹。
func TestMPChatEventGapIsHumanPace(t *testing.T) {
	oldGap, oldJitter := mpChatEventGap, mpChatEventJitter
	t.Cleanup(func() { mpChatEventGap, mpChatEventJitter = oldGap, oldJitter })

	if oldGap < 40*time.Second {
		t.Errorf("mpChatEventGap=%v 太短：上游实测数秒级连发会被反作弊先计入再整体回滚（45s 才稳定）", oldGap)
	}
	if oldJitter <= 0 {
		t.Errorf("mpChatEventJitter=%v：等距连发本身就是机器指纹，抖动不能为 0", oldJitter)
	}

	mpChatEventJitter = 0
	if d := mpChatEventDelay(); d != mpChatEventGap {
		t.Errorf("jitter=0 时 delay=%v, want %v", d, mpChatEventGap)
	}

	mpChatEventJitter = 10 * time.Second
	seen := map[time.Duration]bool{}
	for i := 0; i < 200; i++ {
		d := mpChatEventDelay()
		if d < mpChatEventGap || d >= mpChatEventGap+mpChatEventJitter {
			t.Fatalf("delay=%v 越界 [%v, %v)", d, mpChatEventGap, mpChatEventGap+mpChatEventJitter)
		}
		seen[d] = true
	}
	if len(seen) < 50 {
		t.Errorf("200 次采样只有 %d 个不同取值（抖动未生效）", len(seen))
	}
}

// TestRunMPMiniChatTaskPacesChatEvents 每次上报**之前**都要等：5 条补报的到达时刻
// 必须两两间隔 >= gap（含首条距起点的间隔）。把 sleep 去掉（回到连发）时，相邻间隔
// 落到亚毫秒级，本用例必红——这就是反事实闸门。
func TestRunMPMiniChatTaskPacesChatEvents(t *testing.T) {
	fastPoll(t) // 压掉 claimPollGap / mpActionGap（默认 3s/2s 会让用例白等）
	const gap = 80 * time.Millisecond
	mpChatEventGap, mpChatEventJitter = gap, 0

	s := &paceStub{target: 5}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	c := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	p := &Panel{cfg: Config{Upstream: c}}
	a := &auth.Auth{AccessToken: "at", UID: "u-1", Nickname: "测试"}

	start := time.Now()
	msg, err := p.runMPMiniChatTask(a, "Sequential_Tasks_3", false)
	if err != nil {
		t.Fatalf("runMPMiniChatTask: %v", err)
	}

	s.mu.Lock()
	at := append([]time.Time(nil), s.reportAt...)
	claimed := s.claimed
	s.mu.Unlock()
	if len(at) != 5 {
		t.Fatalf("对话事件上报 %d 条, want 5（msg=%q）", len(at), msg)
	}
	if !claimed {
		t.Errorf("补满 5/5 后应领奖；msg=%q", msg)
	}
	// 首条也等：accept/回读完成到首条上报之间必须有一个完整 gap。
	if d := at[0].Sub(start); d < gap {
		t.Errorf("首条上报距起点 %v < %v（首条未等待：上一轮回滚后立即重报同样无效）", d, gap)
	}
	for i := 1; i < len(at); i++ {
		if d := at[i].Sub(at[i-1]); d < gap {
			t.Errorf("第 %d 条与第 %d 条间隔 %v < %v（连发上报会被反作弊整体回滚）", i, i+1, d, gap)
		}
	}
}
