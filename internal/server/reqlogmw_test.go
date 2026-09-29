// reqlogmw_test.go 请求级埋点中间件的回归。
//
// 最高风险的一条：包装 writer 若**丢掉 http.Flusher**，chatCompletions 里的
// `w.(http.Flusher)` 断言会失败 → 流式退化成非流式（真实功能回归，且只在高负载/
// 长流时才明显）。故本文件把「Flusher 被透传」当作头号闸门。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// flushSpy 既能被 Flush，也记录调用次数（复刻 net/http 的真实能力集）。
type flushSpy struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushSpy) Flush() { f.flushes++ }

func newReqlogRecorder(t *testing.T) *reqlog.Recorder {
	t.Helper()
	rec := reqlog.New(reqlog.Config{Enabled: false}) // 只测内存指标，不落盘
	t.Cleanup(rec.Close)
	return rec
}

// TestReqlogMiddlewarePreservesFlusher 头号闸门：包装后仍可 Flush（SSE 不退化为缓冲）。
// 反事实：把 reqlogWriter 的 Flush 方法删掉 → 本用例必红（断言失败）。
func TestReqlogMiddlewarePreservesFlusher(t *testing.T) {
	h := &Handler{cfg: Config{Requests: newReqlogRecorder(t)}}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("包装后的 writer 丢失了 http.Flusher：流式会被缓冲到请求结束才吐出")
			return
		}
		if _, err := w.Write([]byte("data: x\n\n")); err != nil {
			t.Errorf("write: %v", err)
		}
		f.Flush()
	})
	spy := &flushSpy{ResponseRecorder: httptest.NewRecorder()}
	h.withReqlog(inner).ServeHTTP(spy, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if spy.flushes != 1 {
		t.Errorf("Flush 透传次数=%d want 1（0 = 流式被吞掉）", spy.flushes)
	}
}

// TestReqlogMiddlewareRecordsBasics 中间件记录路径/状态/耗时/首字节。
func TestReqlogMiddlewareRecordsBasics(t *testing.T) {
	rec := newReqlogRecorder(t)
	h := &Handler{cfg: Config{Requests: rec}}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 内层按约定回填元数据（nil 安全）。
		if n := reqlogNoteOf(r); n != nil {
			n.Account = "nick(12345678)"
			n.Model = "glm-5.2"
			n.Attempts = 2
			n.HasAttmpt = true
		}
		time.Sleep(3 * time.Millisecond)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("boom"))
	})
	spy := httptest.NewRecorder()
	h.withReqlog(inner).ServeHTTP(spy, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	snap := rec.Snapshot(10)
	if snap.Completed != 1 {
		t.Fatalf("Completed=%d want 1", snap.Completed)
	}
	if len(snap.Recent) != 1 {
		t.Fatalf("Recent 长度=%d want 1", len(snap.Recent))
	}
	ev := snap.Recent[0]
	if ev.Path != "/v1/messages" {
		t.Errorf("Path=%q", ev.Path)
	}
	if ev.Status != http.StatusBadGateway || ev.OK {
		t.Errorf("Status=%d OK=%v want 502/false", ev.Status, ev.OK)
	}
	if ev.Outcome != reqlog.OutcomeHTTPError {
		t.Errorf("Outcome=%q want %q", ev.Outcome, reqlog.OutcomeHTTPError)
	}
	if ev.Account != "nick(12345678)" || ev.Model != "glm-5.2" || ev.Attempts != 2 {
		t.Errorf("回填字段丢失：%+v", ev)
	}
	if ev.RequestID == "" {
		t.Error("RequestID 为空")
	}
}

// TestReqlogMiddlewareNilRecorderIsPassthrough 未启用时**逐字直通**：
// 连 Flusher 都不该被包一层（零开销、零行为变化）。
func TestReqlogMiddlewareNilRecorderIsPassthrough(t *testing.T) {
	h := &Handler{cfg: Config{}} // Requests 为 nil
	var got http.ResponseWriter
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = w
		if reqlogNoteOf(r) != nil {
			t.Error("未启用记录器时不应注入 note（会带来无谓的 context 分配）")
		}
	})
	spy := &flushSpy{ResponseRecorder: httptest.NewRecorder()}
	h.withReqlog(inner).ServeHTTP(spy, httptest.NewRequest(http.MethodPost, "/v1/messages", nil))
	if got != http.ResponseWriter(spy) {
		t.Errorf("nil 记录器时必须原样透传 writer，实际被包装了：%T", got)
	}
}

// TestReqlogMiddlewareImplicitStatus200 未显式写头就写体 → net/http 隐式 200，
// 记账必须同步为 200（否则会记成 0 这种不存在的状态码）。
func TestReqlogMiddlewareImplicitStatus200(t *testing.T) {
	rec := newReqlogRecorder(t)
	h := &Handler{cfg: Config{Requests: rec}}
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok")) // 不调 WriteHeader
	})
	h.withReqlog(inner).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	ev := rec.Snapshot(1).Recent[0]
	if ev.Status != http.StatusOK || !ev.OK {
		t.Errorf("隐式 200 记账错误：Status=%d OK=%v", ev.Status, ev.OK)
	}
	if ev.Outcome != reqlog.OutcomeSuccess {
		t.Errorf("Outcome=%q", ev.Outcome)
	}
}

// TestIsModelEndpointOnlyModelCalls 只埋点模型调用端点（运维面不计入「谁在打上游」）。
func TestIsModelEndpointOnlyModelCalls(t *testing.T) {
	for _, p := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		if !isModelEndpoint(p) {
			t.Errorf("%s 应埋点", p)
		}
	}
	for _, p := range []string{"/v1/models", "/status", "/v1/stats", "/healthz", "/panel/", "/v1/messages/count_tokens"} {
		if isModelEndpoint(p) {
			t.Errorf("%s 不应埋点（运维面/纯本地估算）", p)
		}
	}
}

// TestReqlogWriterDoesNotSwallowInterfaces 反向保护：包装 writer 不应破坏
// Hijacker/ReaderFrom 之外**必需**的能力。这里只锁 Flusher（唯一被本仓库内层断言的），
// 并确认 Write 返回值语义未被改写。
func TestReqlogWriterWriteReturnSemantics(t *testing.T) {
	spy := &flushSpy{ResponseRecorder: httptest.NewRecorder()}
	w := &reqlogWriter{ResponseWriter: spy, start: time.Now()}
	n, err := w.Write([]byte("hello"))
	if n != 5 || err != nil {
		t.Errorf("Write 返回值被改写：n=%d err=%v", n, err)
	}
	if !strings.Contains(spy.Body.String(), "hello") {
		t.Error("body 未落到真实 writer")
	}
	if w.status != http.StatusOK {
		t.Errorf("未显式写头应记账 200，实际 %d", w.status)
	}
	// TTFB 必须相对**请求开始**（此前一版误写成 firstByte 自减恒 0）。
	w.firstByte = w.start.Add(7 * time.Millisecond)
	if got := w.ttfbMs(); got != 7 {
		t.Errorf("ttfbMs=%d want 7（必须相对请求开始，不能恒 0）", got)
	}
}

// 编译期确认：*reqlogWriter 仍然满足 http.Flusher（防止有人"精简"掉 Flush 方法）。
var _ http.Flusher = (*reqlogWriter)(nil)
