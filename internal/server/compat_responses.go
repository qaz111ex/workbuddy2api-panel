// compat_responses.go OpenAI Responses API（`POST /v1/responses`）兼容层。
//
// 与 /v1/messages 同构：入站把 Responses 请求体翻译成 OpenAI chat 请求体，出站把
// chat 聚合响应 / SSE chunk 翻译成 Responses 对象 / Responses 流式事件，中间复用
// 既有 h.chatCompletions（账号轮转、粘性、冷却熔断、applyErrorPolicy、跨域回落、
// 错误透传零绕开，见 compat.go）。
//
// 已知取舍（**明确列出，不静默丢弃**）：
//   - 无状态网关：`previous_response_id`、`store`、`include` 不支持（丢弃）。
//   - `input` 里的 `reasoning` / `item_reference` 条目丢弃（无 chat 等价物；
//     item_reference 依赖 previous_response_id 的对话状态）。
//   - 内置工具（`web_search` / `file_search` / `computer_use` / `code_interpreter` /
//     `mcp`）与 `custom` 工具丢弃——上游 chat 只有 function 工具形态；function 工具
//     的 name/description/parameters 完整映射。
//   - `text.format`（结构化输出 json_schema）不支持：上游是否认 chat 的
//     response_format 未实测，不冒险透传（丢弃，返回正文仍是纯文本）。
//   - `reasoning.effort` / `reasoning.summary` 丢弃（未透传 reasoning_effort，
//     与 /v1/messages 的 thinking 映射不同：Responses 的 effort 档位语义与
//     Anthropic 预算不同源，无法可靠折算）。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 入站：Responses 请求 → chat 请求
// ---------------------------------------------------------------------------

type responsesRequest struct {
	Model           string            `json:"model"`
	Input           json.RawMessage   `json:"input"`
	Instructions    string            `json:"instructions"`
	MaxOutputTokens *int              `json:"max_output_tokens"`
	Temperature     *float64          `json:"temperature"`
	TopP            *float64          `json:"top_p"`
	Stream          bool              `json:"stream"`
	Tools           []json.RawMessage `json:"tools"`
	ToolChoice      json.RawMessage   `json:"tool_choice"`
}

// responsesOutputText function_call_output.output 的文本化（字符串直取，其余原文）。
func responsesOutputText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// responsesContentToChat Responses 消息条目的 content → chat 消息。
// 纯文本 → 字符串；含图片 → OpenAI 多段数组（input_image.image_url 直接透传）。
func responsesContentToChat(role string, raw json.RawMessage) ([]any, *compatReqError) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		return []any{map[string]any{"role": role, "content": s}}, nil
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		return nil, badRequest("input item content must be a string or an array of content parts")
	}
	var texts []string
	chatParts := make([]any, 0, len(parts))
	hasImage := false
	for _, p := range parts {
		switch t, _ := p["type"].(string); t {
		case "input_text", "output_text", "text":
			if txt, _ := p["text"].(string); txt != "" {
				texts = append(texts, txt)
				chatParts = append(chatParts, map[string]any{"type": "text", "text": txt})
			}
		case "refusal":
			if txt, _ := p["refusal"].(string); txt != "" {
				texts = append(texts, txt)
				chatParts = append(chatParts, map[string]any{"type": "text", "text": txt})
			}
		case "input_image":
			url, _ := p["image_url"].(string)
			if url == "" {
				// file_id 形态需要文件上传通道，网关无此能力 → 丢弃（无等价表示）。
				continue
			}
			hasImage = true
			chatParts = append(chatParts, map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": url},
			})
		default:
			// 未知 part 类型丢弃。
		}
	}
	if len(texts) == 0 && !hasImage {
		return nil, nil
	}
	if hasImage {
		return []any{map[string]any{"role": role, "content": chatParts}}, nil
	}
	return []any{map[string]any{"role": role, "content": strings.Join(texts, "\n")}}, nil
}

// responsesInputToChat 把 `input`（字符串或条目数组）翻译成 chat messages 前缀。
func responsesInputToChat(raw json.RawMessage, instructions string) ([]any, *compatReqError) {
	msgs := make([]any, 0, 4)
	if strings.TrimSpace(instructions) != "" {
		// Responses 的 instructions 是独立字段（system 级指令）→ chat 的 system 消息。
		msgs = append(msgs, map[string]any{"role": "system", "content": instructions})
	}
	if len(raw) == 0 || string(raw) == "null" {
		return msgs, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": s})
		}
		return msgs, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil, badRequest("input: must be a string or an array of input items")
	}
	for i, ri := range items {
		var it struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Output    json.RawMessage `json:"output"`
		}
		if json.Unmarshal(ri, &it) != nil {
			return nil, badRequest(fmt.Sprintf("input.%d: invalid input item", i))
		}
		switch it.Type {
		case "function_call":
			// 工具调用历史 → assistant 的 tool_calls。连续的 function_call 条目合并进
			// 同一条 assistant 消息（OpenAI 的多工具调用形态）。
			tc := map[string]any{
				"id": it.CallID, "type": "function",
				"function": map[string]any{"name": it.Name, "arguments": it.Arguments},
			}
			if tc["id"] == "" {
				tc["id"] = newCompatID("call_")
			}
			if n := len(msgs); n > 0 {
				if last, ok := msgs[n-1].(map[string]any); ok {
					if r, _ := last["role"].(string); r == "assistant" {
						if calls, has := last["tool_calls"].([]any); has {
							last["tool_calls"] = append(calls, tc)
							continue
						}
					}
				}
			}
			msgs = append(msgs, map[string]any{
				"role": "assistant", "content": "", "tool_calls": []any{tc},
			})
		case "function_call_output":
			if it.CallID == "" {
				continue // 无 call_id 的结果无法配对，丢弃（配对错乱会被上游整体拒绝）
			}
			msgs = append(msgs, map[string]any{
				"role": "tool", "tool_call_id": it.CallID, "content": responsesOutputText(it.Output),
			})
		case "reasoning", "item_reference":
			// 丢弃：无 chat 等价物（item_reference 依赖 previous_response_id 状态）。
		case "message", "":
			role := strings.TrimSpace(it.Role)
			if role == "" {
				role = "user"
			}
			switch role {
			case "user", "assistant", "system", "developer":
			default:
				return nil, badRequest(fmt.Sprintf("input.%d.role: unknown role %q", i, role))
			}
			out, cerr := responsesContentToChat(role, it.Content)
			if cerr != nil {
				return nil, cerr
			}
			msgs = append(msgs, out...)
		default:
			// 未知条目类型丢弃。
		}
	}
	return msgs, nil
}

// responsesToolsToChat Responses function 工具 → chat tools。非 function 工具
// （内置工具/custom）无 chat 等价物 → 丢弃（分支显式）。
func responsesToolsToChat(raws []json.RawMessage) []any {
	tools := make([]any, 0, len(raws))
	for _, raw := range raws {
		var t struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		if json.Unmarshal(raw, &t) != nil {
			continue
		}
		if t.Type != "function" || t.Name == "" {
			continue
		}
		params := any(map[string]any{"type": "object", "properties": map[string]any{}})
		if len(t.Parameters) > 0 && string(t.Parameters) != "null" {
			var sch any
			if json.Unmarshal(t.Parameters, &sch) == nil {
				params = sch
			}
		}
		fn := map[string]any{"name": t.Name, "parameters": params}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		tools = append(tools, map[string]any{"type": "function", "function": fn})
	}
	return tools
}

// responsesToolChoiceToChat Responses tool_choice → chat tool_choice。
func responsesToolChoiceToChat(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto", "none", "required":
			return s
		}
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
		Mode string `json:"mode"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}
	switch tc.Type {
	case "function":
		if tc.Name == "" {
			return "auto"
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	case "allowed_tools":
		if tc.Mode == "required" {
			return "required"
		}
		return "auto"
	}
	return nil
}

// translateResponsesRequest 入站校验 + 翻译。校验错误按 OpenAI 错误体返回
// （Responses 是 OpenAI 风格）。
func translateResponsesRequest(raw []byte) (*compatChatRequest, *compatReqError) {
	var req responsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, badRequest("invalid JSON body: " + err.Error())
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		return nil, badRequest("model: field required")
	}
	msgs, cerr := responsesInputToChat(req.Input, req.Instructions)
	if cerr != nil {
		return nil, cerr
	}
	if len(msgs) == 0 {
		return nil, badRequest("input: field required")
	}
	out := map[string]any{"model": model, "messages": msgs, "stream": req.Stream}
	if req.MaxOutputTokens != nil {
		out["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.Tools) > 0 {
		if tools := responsesToolsToChat(req.Tools); len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if tc := responsesToolChoiceToChat(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, badRequest("failed to encode translated chat request: " + err.Error())
	}
	return &compatChatRequest{body: body, stream: req.Stream, model: model}, nil
}

// ---------------------------------------------------------------------------
// 出站：chat → Responses 对象 / 事件
// ---------------------------------------------------------------------------

// responsesOutputItems chat 聚合响应 → Responses output 条目 + 正文文本。
func responsesOutputItems(chat map[string]any) (items []any, text string) {
	items = []any{}
	var msg map[string]any
	if c := chatChoices(chat); c != nil {
		msg, _ = c["message"].(map[string]any)
	}
	text = chatMessageContent(msg)
	if text != "" {
		items = append(items, map[string]any{
			"type": "message", "id": newCompatID("msg_"), "status": "completed",
			"role": "assistant",
			"content": []any{map[string]any{
				"type": "output_text", "text": text, "annotations": []any{},
			}},
		})
	}
	for _, tc := range chatToolCalls(msg) {
		m, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := m["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		callID, _ := m["id"].(string)
		if callID == "" {
			callID = newCompatID("call_")
		}
		args, _ := fn["arguments"].(string)
		items = append(items, map[string]any{
			"type": "function_call", "id": newCompatID("fc_"), "call_id": callID,
			"name": name, "arguments": args, "status": "completed",
		})
	}
	return items, text
}

// responsesObject 组装 Responses 响应对象。字段集刻意补齐 SDK 强类型模型里的
// 必填/常用键（缺失会让严格客户端反序列化失败）；有状态类字段（store、
// previous_response_id）恒为空/关——本网关无状态。
func responsesObject(id, model, status string, items []any, text string, usage map[string]any, finish string) map[string]any {
	obj := map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           time.Now().Unix(),
		"status":               status,
		"model":                model,
		"output":               items,
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"metadata":             map[string]any{},
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            map[string]any{},
		"store":                false,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"temperature":          nil,
		"top_p":                nil,
		"truncation":           "disabled",
		"user":                 nil,
		"usage":                usage,
	}
	if text != "" {
		obj["output_text"] = text
	}
	if reason := responsesIncompleteReason(finish); reason != "" {
		obj["incomplete_details"] = map[string]any{"reason": reason}
	}
	return obj
}

// writeResponsesFromChat 非流式出站。
func writeResponsesFromChat(em *compatEmitter, cap *compatCapture, reqModel string) {
	if cap.status >= 400 {
		info := readCompatError(cap.status, cap.body)
		writeResponsesError(em, info)
		return
	}
	var chat map[string]any
	if json.Unmarshal(cap.body, &chat) != nil {
		em.emitJSON(http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": "invalid upstream response", "type": "api_error", "code": "upstream_parse"},
		})
		return
	}
	if _, hasErr := chat["error"]; hasErr {
		writeResponsesError(em, readCompatError(cap.status, cap.body))
		return
	}
	items, text := responsesOutputItems(chat)
	finish := chatFinishReason(chat)
	var usage map[string]any
	if u, ok := chat["usage"].(map[string]any); ok {
		usage = responsesUsage(u)
	}
	em.emitJSON(http.StatusOK, responsesObject(newCompatID("resp_"), reqModel, responsesStatus(finish), items, text, usage, finish))
}

// writeResponsesError 错误体：OpenAI 风格 `{"error":{message,type,code,gateway_hint}}`，
// HTTP 状态码沿用内层判定（401/429/400/503…）。
func writeResponsesError(em *compatEmitter, info compatErrInfo) {
	e := map[string]any{"message": info.message, "type": "api_error"}
	if info.code != "" {
		e["code"] = info.code
	}
	if info.hint != "" {
		e["gateway_hint"] = info.hint
	}
	em.emitJSON(info.status, map[string]any{"error": e})
}

// ---------------------------------------------------------------------------
// 出站流式：chat chunk → Responses 事件
// ---------------------------------------------------------------------------

// responsesStreamWriter 承接 chatCompletions 流式分支，翻译成 Responses 事件：
//
//	response.created → response.in_progress →
//	response.output_item.added → response.content_part.added →
//	response.output_text.delta* → response.output_text.done →
//	response.content_part.done → response.output_item.done → response.completed
//
// 工具调用走 response.output_item.added（function_call）→
// response.function_call_arguments.delta* → ...done → response.output_item.done。
// 每个事件都带严格递增的 sequence_number（SDK 强类型事件模型的必填字段）。
type responsesStreamWriter struct {
	em    *compatEmitter
	model string
	id    string

	hdr     http.Header
	status  int
	body    []byte
	jsonMod bool
	sawAny  bool
	pending []byte

	started   bool
	msgOpen   bool
	msgID     string
	text      strings.Builder
	textDone  bool
	nextOut   int
	stopCause string
	usage     map[string]any
	errored   bool
	sawFrame  bool
	finished  bool

	toolItems map[int]*responsesToolItem
	toolOrder []*responsesToolItem
}

// responsesToolItem 流式工具调用条目状态（累积 arguments 供 done 事件回填）。
type responsesToolItem struct {
	key     int
	itemID  string
	outIdx  int
	callID  string
	name    string
	args    strings.Builder
	started bool
	done    bool
}

func newResponsesStreamWriter(em *compatEmitter, model string) *responsesStreamWriter {
	return &responsesStreamWriter{
		em:        em,
		model:     model,
		id:        newCompatID("resp_"),
		hdr:       http.Header{},
		toolItems: map[int]*responsesToolItem{},
	}
}

func (w *responsesStreamWriter) Header() http.Header { return w.hdr }

func (w *responsesStreamWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

// Flush 吞掉（真实响应的 flush 由 compatEmitter 负责，见 compat.go 头写入纪律）。
func (w *responsesStreamWriter) Flush() {}

func (w *responsesStreamWriter) Write(p []byte) (int, error) {
	if w.status >= 400 {
		w.body = append(w.body, p...)
		return len(p), nil
	}
	if !w.sawAny {
		if t := bytes.TrimLeft(p, " \t\r\n"); len(t) > 0 {
			w.sawAny = true
			if t[0] == '{' || t[0] == '[' {
				w.jsonMod = true
			}
		}
	}
	if w.jsonMod {
		w.body = append(w.body, p...)
		return len(p), nil
	}
	w.pending = append(w.pending, p...)
	start := 0
	for {
		i := bytes.IndexByte(w.pending[start:], '\n')
		if i < 0 {
			break
		}
		line := string(w.pending[start : start+i])
		start += i + 1
		w.handleLine(strings.TrimRight(line, "\r"))
	}
	if start > 0 {
		w.pending = append(w.pending[:0], w.pending[start:]...)
	}
	return len(p), nil
}

func (w *responsesStreamWriter) handleLine(line string) {
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return
	}
	if e, hasErr := obj["error"]; hasErr {
		w.emitErrorFrame(e)
		return
	}
	w.sawFrame = true
	if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
		w.usage = u
	}
	c := chatChoices(obj)
	if c == nil {
		return
	}
	if fr, _ := c["finish_reason"].(string); fr != "" {
		w.stopCause = fr
	}
	delta, _ := c["delta"].(map[string]any)
	if delta == nil {
		delta, _ = c["message"].(map[string]any)
	}
	if delta == nil {
		return
	}
	if txt, _ := delta["content"].(string); txt != "" {
		w.emitText(txt)
	}
	if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
		w.emitToolCalls(tcs)
	}
}

// event 写一个 Responses 事件（自动补 type / sequence_number）。
func (w *responsesStreamWriter) event(name string, extra map[string]any) {
	p := map[string]any{"type": name, "sequence_number": w.em.nextSeq()}
	for k, v := range extra {
		p[k] = v
	}
	w.em.emitEvent(name, p)
}

// responseSnapshot 当前响应快照（进行中/完成通用；usage 未观测到时为 nil）。
func (w *responsesStreamWriter) responseSnapshot(status, text string, items []any) map[string]any {
	var usage map[string]any
	if w.usage != nil {
		usage = responsesUsage(w.usage)
	}
	return responsesObject(w.id, w.model, status, items, text, usage, w.stopCause)
}

func (w *responsesStreamWriter) ensureStart() {
	if w.started {
		return
	}
	w.started = true
	empty := []any{}
	w.event("response.created", map[string]any{
		"response": w.responseSnapshot("in_progress", "", empty),
	})
	w.event("response.in_progress", map[string]any{
		"response": w.responseSnapshot("in_progress", "", empty),
	})
}

// openMessageItem 开启正文 output item（index 0）+ content part。
func (w *responsesStreamWriter) openMessageItem() {
	if w.msgOpen {
		return
	}
	w.ensureStart()
	w.msgOpen = true
	w.msgID = newCompatID("msg_")
	w.event("response.output_item.added", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"type": "message", "id": w.msgID, "status": "in_progress",
			"role": "assistant", "content": []any{},
		},
	})
	w.event("response.content_part.added", map[string]any{
		"item_id": w.msgID, "output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

func (w *responsesStreamWriter) emitText(txt string) {
	w.openMessageItem()
	w.text.WriteString(txt)
	w.event("response.output_text.delta", map[string]any{
		"item_id": w.msgID, "output_index": 0, "content_index": 0,
		"delta": txt, "logprobs": []any{},
	})
}

// closeMessageItem 收口正文 item（幂等）：output_text.done / content_part.done /
// output_item.done。
func (w *responsesStreamWriter) closeMessageItem() {
	if !w.msgOpen || w.textDone {
		return
	}
	w.textDone = true
	text := w.text.String()
	w.event("response.output_text.done", map[string]any{
		"item_id": w.msgID, "output_index": 0, "content_index": 0,
		"text": text, "logprobs": []any{},
	})
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
	w.event("response.content_part.done", map[string]any{
		"item_id": w.msgID, "output_index": 0, "content_index": 0, "part": part,
	})
	w.event("response.output_item.done", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"type": "message", "id": w.msgID, "status": "completed",
			"role": "assistant", "content": []any{part},
		},
	})
}

// emitToolCalls 把 delta.tool_calls 分片翻译成 function_call output item 及其
// arguments 增量事件。
func (w *responsesStreamWriter) emitToolCalls(tcs []any) {
	for _, tci := range tcs {
		tc, ok := tci.(map[string]any)
		if !ok {
			continue
		}
		key := compatIntOf(tc["index"])
		fn, _ := tc["function"].(map[string]any)
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		item, seen := w.toolItems[key]
		if !seen {
			w.closeMessageItem() // 正文 item 先收口，工具条目另起 output_index
			w.ensureStart()
			callID, _ := tc["id"].(string)
			if callID == "" {
				callID = newCompatID("call_")
			}
			item = &responsesToolItem{
				key: key, itemID: newCompatID("fc_"), outIdx: w.nextOut,
				callID: callID, name: name,
			}
			w.nextOut++
			w.toolItems[key] = item
			w.toolOrder = append(w.toolOrder, item)
			item.started = true
			w.event("response.output_item.added", map[string]any{
				"output_index": item.outIdx,
				"item": map[string]any{
					"type": "function_call", "id": item.itemID, "call_id": item.callID,
					"name": item.name, "arguments": "", "status": "in_progress",
				},
			})
		}
		if args != "" {
			item.args.WriteString(args)
			w.event("response.function_call_arguments.delta", map[string]any{
				"item_id": item.itemID, "output_index": item.outIdx, "delta": args,
			})
		}
	}
}

// closeToolItems 收口全部工具条目（幂等）。
func (w *responsesStreamWriter) closeToolItems() {
	for _, item := range w.toolOrder {
		if item.done {
			continue
		}
		item.done = true
		args := item.args.String()
		w.event("response.function_call_arguments.done", map[string]any{
			"item_id": item.itemID, "output_index": item.outIdx, "arguments": args,
		})
		w.event("response.output_item.done", map[string]any{
			"output_index": item.outIdx,
			"item": map[string]any{
				"type": "function_call", "id": item.itemID, "call_id": item.callID,
				"name": item.name, "arguments": args, "status": "completed",
			},
		})
	}
}

func (w *responsesStreamWriter) emitErrorFrame(e any) {
	w.errored = true
	msg, code, hint := "upstream error", "", ""
	if m, ok := e.(map[string]any); ok {
		if s, _ := m["message"].(string); s != "" {
			msg = s
		}
		if s, _ := m["code"].(string); s != "" {
			code = s
		}
		if s, _ := m["gateway_hint"].(string); s != "" {
			hint = s
		}
	}
	extra := map[string]any{"message": msg, "param": nil}
	if code != "" {
		extra["code"] = code
	}
	if hint != "" {
		extra["gateway_hint"] = hint // 额外字段：SDK 忽略未知键，排障可见
	}
	w.event("error", extra)
}

// finish 收尾：错误 → OpenAI 错误体；成功 → 收口条目 + response.completed。
func (w *responsesStreamWriter) finish() {
	if w.finished {
		return
	}
	w.finished = true
	if w.status >= 400 || w.jsonMod {
		status := w.status
		if status < 400 {
			status = http.StatusBadGateway
		}
		writeResponsesError(w.em, readCompatError(status, w.body))
		return
	}
	if w.errored {
		return // 已发 error 事件
	}
	if !w.started && !w.sawFrame {
		w.event("error", map[string]any{
			"message": "upstream returned no content", "code": "upstream_parse", "param": nil,
		})
		return
	}
	w.ensureStart()
	var items []any
	if w.msgOpen {
		w.closeMessageItem()
		text := w.text.String()
		items = append(items, map[string]any{
			"type": "message", "id": w.msgID, "status": "completed", "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		})
	}
	w.closeToolItems()
	for _, item := range w.toolOrder {
		items = append(items, map[string]any{
			"type": "function_call", "id": item.itemID, "call_id": item.callID,
			"name": item.name, "arguments": item.args.String(), "status": "completed",
		})
	}
	text := w.text.String()
	status := responsesStatus(w.stopCause)
	w.event("response.completed", map[string]any{
		"response": w.responseSnapshot(status, text, items),
	})
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// responses POST /v1/responses（OpenAI Responses API）。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	tr, cerr := translateResponsesRequest(raw)
	if cerr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", cerr.message)
		return
	}
	em := newCompatEmitter(w)
	if tr.stream {
		iw := newResponsesStreamWriter(em, tr.model)
		h.runInnerChat(iw, r, tr.body)
		iw.finish()
		return
	}
	cap := newCompatCapture()
	h.runInnerChat(cap, r, tr.body)
	writeResponsesFromChat(em, cap, tr.model)
}
