// reqlogmw.go 请求级可观测性的**埋点中间件**（reqlog 包的接入口）。
//
// 为什么做成中间件、而不是往 chatCompletions 里塞埋点：
//   - chatCompletions 是本项目最复杂的函数（账号轮转 / 粘性 / 熔断 / 冷却 / 跨域
//     回落 / 流式 / 错误透传全在里面），任何新增语句都有踩坏账号保护的风险；
//   - 而「谁、什么模型、什么状态、多久、重试几次」这些事实在**外层**就能拿到完整
//     答案（状态码与耗时来自 ResponseWriter，账号/模型/重试由内层**可选**回填）。
//
// 因此：外层一律记录（零侵入），内层只在信息可得处调用 reqlogNote 回填（nil 安全，
// 不改变任何既有控制流）。这样即使回填全缺，也已有 path/status/duration/ttfb——
// 足以回答「刚才那个 5xx 是谁、多久」。
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// reqlogKey 请求上下文里的**可变**收集器键。
type reqlogKeyType struct{}

var reqlogKey = reqlogKeyType{}

// reqlogNote 一次请求的可回填元数据。只有标量，**不承载任何内容**
// （与 reqlog.Event 同口径：提示词/正文/凭证绝不入内）。
//
// 字段用指针/布尔「是否已知」双写，是为了区分「未知」与「已知为 0」——
// 例如 attempts 未知时不该写成 1，tokens 未知时不该写成 0。
type reqlogNote struct {
	Account   string
	Model     string
	Attempts  int
	HasAttmpt bool

	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	HasTokens        bool

	Credit    float64
	HasCredit bool

	Outcome string // 空 = 由中间件按状态码推断
}

// withReqlog 请求级埋点中间件。rec == nil 时**原样直通**（零开销、零行为变化）：
// 归档未启用时不该为可观测性付出任何代价。
func (h *Handler) withReqlog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := h.reqlogRecorder()
		if rec == nil {
			next.ServeHTTP(w, r)
			return
		}
		rec.Begin()
		note := &reqlogNote{}
		start := time.Now()
		sw := &reqlogWriter{ResponseWriter: w, start: start}

		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), reqlogKey, note)))

		status := sw.status
		if status == 0 {
			// 处理器一个字节都没写（例如 405/404 由 mux 处理，或提前 return）：
			// net/http 会隐式回 200，如实记录 200 而不是编一个 0。
			status = http.StatusOK
		}
		ev := reqlog.Event{
			Time:             start,
			RequestID:        reqlog.NewRequestID(),
			Path:             r.URL.Path,
			Account:          note.Account,
			Model:            note.Model,
			Status:           status,
			OK:               status < 400 && note.Outcome != reqlog.OutcomeStreamError && note.Outcome != reqlog.OutcomeInterrupted,
			Outcome:          firstNonEmpty(note.Outcome, outcomeFromStatus(status)),
			DurationMs:       time.Since(start).Milliseconds(),
			TTFBMs:           sw.ttfbMs(),
			PromptTokens:     note.PromptTokens,
			CompletionTokens: note.CompletionTokens,
			TotalTokens:      note.TotalTokens,
			HasCredit:        note.HasCredit,
			Credit:           note.Credit,
		}
		if note.HasAttmpt {
			ev.Attempts = note.Attempts
		}
		if !note.HasTokens {
			ev.PromptTokens, ev.CompletionTokens, ev.TotalTokens = 0, 0, 0
		}
		rec.Record(ev)
	})
}

// outcomeFromStatus 仅按状态码推断结果（没有更精确信息时的兜底）。
func outcomeFromStatus(status int) string {
	if status >= 400 {
		return reqlog.OutcomeHTTPError
	}
	return reqlog.OutcomeSuccess
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// reqlogWriter 只捕获**状态码**与**首字节时刻**的包装 writer。
//
// ⚠️ 刻意不实现 http.Flusher 之外的任何接口，且**必须实现 Flusher**：流式响应
// （SSE）依赖它。若这里把一个非 Flusher 包在外面，chatCompletions 的
// `w.(http.Flusher)` 断言会失败 → 流式退化成非流式（真实功能回归）。
// 同理不能吞掉 Write 的返回值语义。
type reqlogWriter struct {
	http.ResponseWriter
	start     time.Time
	status    int
	firstByte time.Time
}

func (w *reqlogWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *reqlogWriter) Write(b []byte) (int, error) {
	if w.firstByte.IsZero() {
		w.firstByte = time.Now()
	}
	if w.status == 0 {
		// 未显式写头就写体：net/http 会隐式提交 200，此处同步记账。
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush 透传（**必需**，见类型注释）：否则 SSE 会被缓冲到请求结束才吐出去。
func (w *reqlogWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ttfbMs 首字节延迟（相对请求开始）。未写任何字节 → 0（omitempty 会省略该字段，
// 比编一个假值诚实）。
func (w *reqlogWriter) ttfbMs() int64 {
	if w.firstByte.IsZero() {
		return 0
	}
	return w.firstByte.Sub(w.start).Milliseconds()
}

// reqlogNoteOf 取当前请求的回填收集器（nil = 中间件未启用，调用方直接跳过）。
// 内层埋点一律写成：
//
//	if n := reqlogNoteOf(r); n != nil { n.Account = ...; }
//
// 这样未启用归档时零开销、零分支副作用。
func reqlogNoteOf(r *http.Request) *reqlogNote {
	if r == nil {
		return nil
	}
	n, _ := r.Context().Value(reqlogKey).(*reqlogNote)
	return n
}
