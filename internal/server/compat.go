// compat.go 协议兼容层基础设施：`POST /v1/messages`（Anthropic Messages API）与
// `POST /v1/responses`（OpenAI Responses API）共用。
//
// 设计（对齐任务书）：**不复制调度逻辑**。账号轮转、粘性会话、冷却/熔断、
// applyErrorPolicy、跨域回落、错误透传全部长在既有 `chatCompletions` 那一条链上；
// 本层只做两件事：
//
//  1. 入站：把外部协议请求体**翻译**成 OpenAI chat 请求体；
//  2. 出站：把既有 chat 链路的响应 / SSE 帧**翻译**成目标协议形态。
//
// 中间直接调用 `h.chatCompletions`（同一个方法，不是另起一套选号/重试），因此
// 新增协议对既有账号保护链（11140 分野、边缘 401、WAF IP fail-fast、14017 热改
// 冷却…）零绕开、零回归。
//
// 头写入纪律（本层最容易踩、且会让客户端直接报协议错误的坑，见调研指出的
// misakano7545 435cb8ec）：真实 `http.ResponseWriter` 只允许写一次头，并且
// **只能由 compatEmitter 写**。内层处理器拿到的 writer 全部是喂给 emitter 的
// 「哑」writer：
//   - `WriteHeader` 只记录状态，不落真实响应；
//   - `Write` 只被解析/缓冲，真实输出由 emitter 按目标协议产出；
//   - `Flush` **一律吞掉**——否则内层 StreamHint 的逐帧 flush 会在 emitter 显式
//     WriteHeader 之前隐式提交 200，emitter 再显式写头就是 net/http 的
//     "superfluous response.WriteHeader call"（协议噪音 + 客户端可能收到半截头）。
//
// 该纪律由 compatEmitter 的 `started` 闩锁在结构上保证：`begin` 是唯一触碰真实
// ResponseWriter 的地方，且幂等。测试 TestCompat*WritesHeaderOnce 用计数 writer
// 锁死「任何情况下恰好一次」。
package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
)

// ---------------------------------------------------------------------------
// 出口：恰好一次头写入 + 目标协议事件输出
// ---------------------------------------------------------------------------

// compatEmitter 目标协议输出端。所有对真实 ResponseWriter 的写入都经此，
// `begin` 由 `started` 闩锁保证**恰好一次** WriteHeader（这正是本层最脆弱的点）。
type compatEmitter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	started bool // 真实响应头已写出（恰好一次）
	failed  bool // 真实写失败（客户端断开）：后续一律丢弃
	seq     int  // Responses API 的 sequence_number
}

func newCompatEmitter(w http.ResponseWriter) *compatEmitter {
	fl, _ := w.(http.Flusher)
	return &compatEmitter{w: w, flusher: fl}
}

// begin 唯一写头点：幂等 + 只写一次。返回 false 表示「已写过或已失败」，
// 调用方必须直接返回（绝不二次写头）。
func (e *compatEmitter) begin(contentType string, status int) bool {
	if e.started || e.failed {
		return false
	}
	e.started = true
	e.w.Header().Set("Content-Type", contentType)
	e.w.WriteHeader(status)
	return true
}

// emitJSON 单 JSON 响应（非流式全量 / 错误体）。marshal 失败则**不写头**返回，
// 让调用方保持「零输出」而不是发出坏帧。
func (e *compatEmitter) emitJSON(status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	if !e.begin("application/json", status) {
		return
	}
	if _, werr := e.w.Write(raw); werr != nil {
		e.failed = true
	}
}

// emitEvent 写一帧 SSE（`event:` 行 + `data:` 行），flush 保证流式可见性。
// 首个事件到达时才写头（TTFB 与首个内容帧对齐，且此前一切错误都能走正常状态码）。
func (e *compatEmitter) emitEvent(name string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if e.started {
		if e.failed {
			return
		}
	} else {
		h := e.w.Header()
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		if !e.begin("text/event-stream", http.StatusOK) {
			return
		}
	}
	var b strings.Builder
	b.Grow(len(raw) + len(name) + 16)
	b.WriteString("event: ")
	b.WriteString(name)
	b.WriteString("\ndata: ")
	b.Write(raw)
	b.WriteString("\n\n")
	if _, werr := io.WriteString(e.w, b.String()); werr != nil {
		e.failed = true
		return
	}
	if e.flusher != nil {
		e.flusher.Flush()
	}
}

// nextSeq Responses API 事件的 sequence_number（严格递增，SDK 的强类型事件模型
// 把该字段列为必填）。
func (e *compatEmitter) nextSeq() int {
	e.seq++
	return e.seq - 1
}

// ---------------------------------------------------------------------------
// 内层 writer：只喂 emitter，绝不触碰真实响应
// ---------------------------------------------------------------------------

// compatCapture 承接内层 chat 处理器的**非流式**输出（成功 JSON 或错误 JSON）。
// Header() 返回独立 header 视图：内层设置的 Content-Type 不落到真实响应——真实
// 响应的 header 由 compatEmitter 独占。
type compatCapture struct {
	header http.Header
	status int
	body   []byte
}

func newCompatCapture() *compatCapture { return &compatCapture{header: http.Header{}} }

func (c *compatCapture) Header() http.Header { return c.header }

func (c *compatCapture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

func (c *compatCapture) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.body = append(c.body, p...)
	return len(p), nil
}

// Flush 吞掉：内层的 flush 若落到真实 ResponseWriter 会提前隐式提交 200，之后
// emitter 的 WriteHeader 就变成 superfluous 二次写头。真实响应的提交权只属于
// compatEmitter（见文件头）。
func (c *compatCapture) Flush() {}

// runInnerChat 把翻译后的 chat 请求体交回**既有** chatCompletions 链路：同一套
// 选号/粘性/冷却/熔断/跨域回落/错误处置，输出经 inner 转写。
//
// 注意：直接调用方法而不是 `h.mux.ServeHTTP`——外层路由已做过 Bearer 鉴权，
// 走 mux 会二次鉴权、并在鉴权失败时把 OpenAI 形状的错误体泄漏给 Anthropic 客户端。
func (h *Handler) runInnerChat(inner http.ResponseWriter, r *http.Request, chatBody []byte) {
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chatBody))
	r2.ContentLength = int64(len(chatBody))
	r2.GetBody = nil
	h.chatCompletions(inner, r2)
}

// withCompatAuth 与 withAuth 同口径的 Bearer 校验（同一 httpauth.VerifyBearer +
// 同一热改 key 快照），仅鉴权失败的**错误体形状**按入口协议定制：
// /v1/messages 必须是 Anthropic 形状，否则 Anthropic SDK 拿不到可读的 401。
func (h *Handler) withCompatAuth(next http.HandlerFunc, onFail func(http.ResponseWriter)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, h.loadLive().APIKey) {
			onFail(w)
			return
		}
		next(w, r)
	}
}

// newCompatID 生成协议风格随机 ID（msg_/resp_/toolu_/fc_/item_）。crypto/rand
// 不可用时回落时间戳（不阻塞请求，也不因随机源故障把请求打挂）。
func newCompatID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return prefix + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return prefix + hex.EncodeToString(b)
}

// compatReqError 入站请求的翻译失败（校验错误）：按目标协议回 400 invalid_request_error。
type compatReqError struct {
	message string
}

func badRequest(msg string) *compatReqError { return &compatReqError{message: msg} }

// ---------------------------------------------------------------------------
// 错误体读取与形状映射
// ---------------------------------------------------------------------------

// compatErrInfo 内层 OpenAI 形状错误体的解析结果（状态码 + 原文 + 既有 gateway_hint）。
type compatErrInfo struct {
	status  int
	code    string
	message string
	hint    string
}

// readCompatError 解析内层错误 JSON `{"error":{message,type,code,gateway_hint}}`。
// message 是上游 body 原文透传（既有 error-passthrough 语义，本层不改写、不包装）；
// gateway_hint 是本项目特色，按目标协议一并带出（让客户端知道是账号问题、限流
// 还是请求问题）。解析失败 → 原样 body 作为 message（不编造）。
func readCompatError(status int, body []byte) compatErrInfo {
	info := compatErrInfo{status: status, message: strings.TrimSpace(string(body))}
	var obj struct {
		Error *struct {
			Message string          `json:"message"`
			Code    json.RawMessage `json:"code"`
			Hint    string          `json:"gateway_hint"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &obj) == nil && obj.Error != nil {
		if s := strings.TrimSpace(obj.Error.Message); s != "" {
			info.message = s
		}
		if s := compatScalarString(obj.Error.Code); s != "" {
			info.code = s
		}
		info.hint = obj.Error.Hint
	}
	if info.status < 400 {
		info.status = http.StatusBadGateway
	}
	if strings.TrimSpace(info.message) == "" {
		info.message = http.StatusText(info.status)
	}
	return info
}

// anthropicErrType HTTP 状态码 → Anthropic 错误 type 枚举。
func anthropicErrType(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// anthropicErrorBody Anthropic 错误体：`{"type":"error","error":{type,message,...}}`。
// gateway_hint / code 作为 error 对象上的并列补充字段（Anthropic SDK 忽略未知键，
// 但排障时能一眼看出是账号、限流还是请求问题）。
func anthropicErrorBody(errType, message, hint, code string) map[string]any {
	e := map[string]any{"type": errType, "message": message}
	if hint != "" {
		e["gateway_hint"] = hint
	}
	if code != "" {
		e["code"] = code
	}
	return map[string]any{"type": "error", "error": e}
}

// writeAnthropicError 直接写 Anthropic 错误（仅在翻译前的校验阶段使用——此时
// emitter 还不存在，WriteHeader 天然只有一次）。
func writeAnthropicError(w http.ResponseWriter, status int, errType, message, hint string) {
	writeJSON(w, status, anthropicErrorBody(errType, message, hint, ""))
}

func writeAnthropicAuthError(w http.ResponseWriter) {
	writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "missing or invalid API key", "")
}

func writeOpenAIAuthError(w http.ResponseWriter) {
	writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
}

// anthropicStopReason OpenAI finish_reason → Anthropic stop_reason。
func anthropicStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// ---------------------------------------------------------------------------
// chat 响应字段读取（Aggregate / 流式帧同构）
// ---------------------------------------------------------------------------

// compatIntOf 取整数（JSON number 经 Unmarshal 是 float64；防御手构造的 int 家族）。
func compatIntOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case float32:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case int32:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

// compatScalarString 取标量字符串（string / number 均可，其他返回空）。
func compatScalarString(v json.RawMessage) string {
	if len(v) == 0 || string(v) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(v, &n) == nil {
		return n.String()
	}
	return ""
}

// chatChoices 取 chat 响应的首个 choice（缺失/形态异常 → nil）。
func chatChoices(resp map[string]any) map[string]any {
	chs, _ := resp["choices"].([]any)
	if len(chs) == 0 {
		return nil
	}
	c, _ := chs[0].(map[string]any)
	return c
}

// chatFinishReason 取 finish_reason（缺失 → ""）。
func chatFinishReason(resp map[string]any) string {
	c := chatChoices(resp)
	if c == nil {
		return ""
	}
	fr, _ := c["finish_reason"].(string)
	return fr
}

// chatMessageContent 取 assistant message/delta 的文本内容（字符串或文本块数组）。
func chatMessageContent(msg map[string]any) string {
	if msg == nil {
		return ""
	}
	switch v := msg["content"].(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				if t, _ := m["text"].(string); t != "" {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// chatToolCalls 取 message/delta 的 tool_calls（保持数组顺序）。
func chatToolCalls(msg map[string]any) []any {
	if msg == nil {
		return nil
	}
	tcs, _ := msg["tool_calls"].([]any)
	return tcs
}

// ---------------------------------------------------------------------------
// usage 映射
// ---------------------------------------------------------------------------

// anthropicUsage chat usage → Anthropic usage。
//
// input_tokens 语义（Anthropic）：**不含**缓存命中部分——命中量单独以
// cache_read_input_tokens 计数。chat 的 prompt_tokens 是含缓存的总量，直接把
// prompt 当 input_tokens 会让 input + cache_read 双重计入上下文长度（客户端据此
// 提前触发压缩/截断，属真实功能损失），故减去命中量。
func anthropicUsage(u map[string]any) map[string]any {
	prompt := compatIntOf(u["prompt_tokens"])
	out := compatIntOf(u["completion_tokens"])
	read := compatIntOf(u["cache_read_input_tokens"])
	if read < 0 {
		read = 0
	}
	in := prompt - read
	if in < 0 {
		in = prompt // 上游数据不自洽时不臆造负数
	}
	res := map[string]any{"input_tokens": in, "output_tokens": out}
	if read > 0 {
		res["cache_read_input_tokens"] = read
	}
	if cw := compatIntOf(u["cache_creation_input_tokens"]); cw > 0 {
		res["cache_creation_input_tokens"] = cw
	}
	return res
}

func anthropicUsageOf(resp map[string]any) map[string]any {
	u, _ := resp["usage"].(map[string]any)
	return anthropicUsage(u)
}

// responsesUsage chat usage → Responses API usage。input_tokens_details /
// output_tokens_details 是 OpenAI SDK 强类型模型里的**必填**嵌套对象，缺失会让
// 严格客户端反序列化失败，故显式给出。
func responsesUsage(u map[string]any) map[string]any {
	if u == nil {
		u = map[string]any{}
	}
	read := compatIntOf(u["cache_read_input_tokens"])
	if read < 0 {
		read = 0
	}
	return map[string]any{
		"input_tokens":          compatIntOf(u["prompt_tokens"]),
		"input_tokens_details":  map[string]any{"cached_tokens": read},
		"output_tokens":         compatIntOf(u["completion_tokens"]),
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		"total_tokens":          compatIntOf(u["total_tokens"]),
	}
}

// responsesModel 的 finish_reason → Responses status。
func responsesStatus(finish string) string {
	if finish == "length" {
		return "incomplete"
	}
	if finish == "content_filter" {
		return "incomplete"
	}
	return "completed"
}

// responsesIncompleteReason finish_reason → incomplete_details.reason。
func responsesIncompleteReason(finish string) string {
	if finish == "length" {
		return "max_output_tokens"
	}
	if finish == "content_filter" {
		return "content_filter"
	}
	return ""
}
