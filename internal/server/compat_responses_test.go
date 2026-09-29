package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ---------------------------------------------------------------------------
// 入站翻译
// ---------------------------------------------------------------------------

func TestTranslateResponsesRequestStringInput(t *testing.T) {
	tr, cerr := translateResponsesRequest([]byte(
		`{"model":"gpt-5","instructions":"SYS","input":"hello","max_output_tokens":256,"temperature":0.2,"stream":true}`))
	if cerr != nil {
		t.Fatalf("unexpected error: %s", cerr.message)
	}
	if !tr.stream || tr.model != "gpt-5" {
		t.Errorf("stream/model: %v %q", tr.stream, tr.model)
	}
	var body map[string]any
	_ = json.Unmarshal(tr.body, &body)
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("instructions → system 消息 + input → user: %v", msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != "SYS" {
		t.Errorf("instructions 未落到 messages[0]: %v", msgs[0])
	}
	if msgs[1].(map[string]any)["content"] != "hello" {
		t.Errorf("input 字符串未落到 user: %v", msgs[1])
	}
	if body["max_tokens"] != float64(256) {
		t.Errorf("max_output_tokens → max_tokens 未映射: %v", body["max_tokens"])
	}
}

func TestTranslateResponsesRequestItemArray(t *testing.T) {
	raw := []byte(`{"model":"m","input":[
		{"role":"user","content":[{"type":"input_text","text":"look at this"},{"type":"input_image","image_url":"data:image/png;base64,QQ=="}]},
		{"type":"function_call","call_id":"call_1","name":"Bash","arguments":"{\"cmd\":\"ls\"}"},
		{"type":"function_call_output","call_id":"call_1","output":"file1"},
		{"role":"user","content":"and then?"}
	]}`)
	tr, cerr := translateResponsesRequest(raw)
	if cerr != nil {
		t.Fatalf("unexpected error: %s", cerr.message)
	}
	var body map[string]any
	_ = json.Unmarshal(tr.body, &body)
	msgs := body["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages=%d want 4: %v", len(msgs), msgs)
	}
	u := msgs[0].(map[string]any)
	parts, ok := u["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("多模态 user 消息形态: %v", u)
	}
	if parts[1].(map[string]any)["type"] != "image_url" {
		t.Errorf("input_image → image_url: %v", parts[1])
	}
	fc := msgs[1].(map[string]any)
	if fc["role"] != "assistant" {
		t.Fatalf("function_call → assistant.tool_calls: %v", fc)
	}
	tcs := fc["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if tc["id"] != "call_1" || tc["function"].(map[string]any)["name"] != "Bash" {
		t.Errorf("function_call 字段: %v", tc)
	}
	out := msgs[2].(map[string]any)
	if out["role"] != "tool" || out["tool_call_id"] != "call_1" || out["content"] != "file1" {
		t.Errorf("function_call_output → role=tool: %v", out)
	}
	if msgs[3].(map[string]any)["content"] != "and then?" {
		t.Errorf("末尾 user 消息: %v", msgs[3])
	}
}

func TestTranslateResponsesRequestConsecutiveFunctionCallsMerged(t *testing.T) {
	raw := []byte(`{"model":"m","input":[
		{"type":"function_call","call_id":"c1","name":"A","arguments":"{}"},
		{"type":"function_call","call_id":"c2","name":"B","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"r1"},
		{"type":"function_call_output","call_id":"c2","output":"r2"}
	]}`)
	tr, cerr := translateResponsesRequest(raw)
	if cerr != nil {
		t.Fatalf("unexpected error: %s", cerr.message)
	}
	var body map[string]any
	_ = json.Unmarshal(tr.body, &body)
	msgs := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("连续 function_call 应合并进同一 assistant 消息: %v", msgs)
	}
	if got := len(msgs[0].(map[string]any)["tool_calls"].([]any)); got != 2 {
		t.Errorf("合并后 tool_calls=%d want 2", got)
	}
}

func TestTranslateResponsesToolsAndToolChoice(t *testing.T) {
	raw := []byte(`{"model":"m","input":"q",
		"tools":[{"type":"function","name":"Bash","description":"d","parameters":{"type":"object"}},
		         {"type":"web_search"}],
		"tool_choice":{"type":"function","name":"Bash"}}`)
	tr, cerr := translateResponsesRequest(raw)
	if cerr != nil {
		t.Fatalf("unexpected error: %s", cerr.message)
	}
	var body map[string]any
	_ = json.Unmarshal(tr.body, &body)
	tools := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("内置工具应丢弃、function 保留: %v", tools)
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "Bash" || fn["parameters"].(map[string]any)["type"] != "object" {
		t.Errorf("function 工具映射: %v", fn)
	}
	tc := body["tool_choice"].(map[string]any)
	if tc["type"] != "function" {
		t.Errorf("tool_choice: %v", tc)
	}
}

func TestTranslateResponsesAllowedToolsChoice(t *testing.T) {
	tr, _ := translateResponsesRequest([]byte(`{"model":"m","input":"q","tool_choice":{"type":"allowed_tools","mode":"required"}}`))
	var body map[string]any
	_ = json.Unmarshal(tr.body, &body)
	if body["tool_choice"] != "required" {
		t.Errorf("allowed_tools required → required: %v", body["tool_choice"])
	}
}

func TestTranslateResponsesValidation(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"bad json", `{`, "invalid JSON body"},
		{"no model", `{"input":"q"}`, "model: field required"},
		{"no input", `{"model":"m"}`, "input: field required"},
		{"bad input type", `{"model":"m","input":42}`, "input: must be a string or an array"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, cerr := translateResponsesRequest([]byte(c.raw))
			if cerr == nil || !strings.Contains(cerr.message, c.want) {
				t.Fatalf("err=%v want contains %q", cerr, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 端到端：非流式 / 流式
// ---------------------------------------------------------------------------

func TestResponsesNonStreamObject(t *testing.T) {
	up, cu := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi","instructions":"SYS"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Errorf("响应对象形态: %v", resp)
	}
	if !strings.HasPrefix(resp["id"].(string), "resp_") {
		t.Errorf("id=%v 应以 resp_ 开头", resp["id"])
	}
	if resp["model"] != "gpt-5" {
		t.Errorf("model 应回显请求名: %v", resp["model"])
	}
	if resp["output_text"] != "你好" {
		t.Errorf("output_text=%v", resp["output_text"])
	}
	out := resp["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output=%v", out)
	}
	item := out[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" || item["status"] != "completed" {
		t.Errorf("output item 形态: %v", item)
	}
	parts := item["content"].([]any)
	if parts[0].(map[string]any)["type"] != "output_text" || parts[0].(map[string]any)["text"] != "你好" {
		t.Errorf("output content part: %v", parts)
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(1) || usage["output_tokens"] != float64(1) || usage["total_tokens"] != float64(2) {
		t.Errorf("usage 映射: %v", usage)
	}
	// SDK 强类型模型的必填嵌套对象。
	if _, has := usage["input_tokens_details"]; !has {
		t.Error("usage.input_tokens_details 是 SDK 必填键")
	}
	if _, has := usage["output_tokens_details"]; !has {
		t.Error("usage.output_tokens_details 是 SDK 必填键")
	}
	// instructions 落到上游 system。
	b := cu.last(t)
	if b["messages"].([]any)[0].(map[string]any)["role"] != "system" {
		t.Errorf("instructions 未落到 system: %v", b["messages"])
	}
}

func TestResponsesNonStreamToolCalls(t *testing.T) {
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 200, anthropicToolStreamBody, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/responses", `{"model":"m","input":"q","tools":[{"type":"function","name":"Bash","parameters":{"type":"object"}}]}`)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	out := resp["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output=%v", out)
	}
	item := out[0].(map[string]any)
	if item["type"] != "function_call" || item["call_id"] != "call_1" || item["name"] != "Bash" {
		t.Errorf("function_call item: %v", item)
	}
	if item["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("arguments 必须原样透传: %v", item["arguments"])
	}
	if !strings.HasPrefix(item["id"].(string), "fc_") {
		t.Errorf("item id=%v", item["id"])
	}
}

func TestResponsesStreamEventSequence(t *testing.T) {
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 200, anthropicStreamBody, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/responses", `{"model":"gpt-5","input":"hi","stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type=%q", ct)
	}
	evs := parseCompatSSE(t, rec.Body.String())
	requireEventPattern(t, evs,
		"response.created", "response.in_progress", "response.output_item.added",
		"response.content_part.added", "response.output_text.delta", "*",
		"response.output_text.done", "response.content_part.done",
		"response.output_item.done", "response.completed")
	// sequence_number 严格递增且从 0 开始。
	for i, e := range evs {
		sn, ok := e.data["sequence_number"].(float64)
		if !ok {
			t.Fatalf("事件 %s 缺 sequence_number", e.name)
		}
		if int(sn) != i {
			t.Errorf("事件 %d (%s) sequence_number=%v want %d", i, e.name, sn, i)
		}
		if e.data["type"] != e.name {
			t.Errorf("事件 data.type=%v want %s", e.data["type"], e.name)
		}
	}
	var sb strings.Builder
	for _, e := range evs {
		if e.name == "response.output_text.delta" {
			if e.data["output_index"] != float64(0) || e.data["content_index"] != float64(0) {
				t.Errorf("delta 索引: %v", e.data)
			}
			if _, has := e.data["logprobs"]; !has {
				t.Error("response.output_text.delta 必须带 logprobs 键（SDK 必填）")
			}
			sb.WriteString(e.data["delta"].(string))
		}
	}
	if sb.String() != "你好世界" {
		t.Errorf("正文拼接=%q", sb.String())
	}
	done := findEvent(evs, "response.output_text.done").data
	if done["text"] != "你好世界" {
		t.Errorf("output_text.done.text=%v", done["text"])
	}
	completed := findEvent(evs, "response.completed").data
	robj := completed["response"].(map[string]any)
	if robj["status"] != "completed" {
		t.Errorf("completed.status=%v", robj["status"])
	}
	if robj["output_text"] != "你好世界" {
		t.Errorf("completed.output_text=%v", robj["output_text"])
	}
	usage := robj["usage"].(map[string]any)
	if usage["output_tokens"] != float64(4) {
		t.Errorf("completed.usage=%v", usage)
	}
}

func TestResponsesStreamToolCallEvents(t *testing.T) {
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 200, anthropicToolStreamBody, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/responses", `{"model":"m","input":"q","stream":true,"tools":[{"type":"function","name":"Bash"}]}`)
	evs := parseCompatSSE(t, rec.Body.String())
	added := findEvent(evs, "response.output_item.added")
	if added == nil {
		t.Fatalf("缺 output_item.added: %v", eventNames(evs))
	}
	item := added.data["item"].(map[string]any)
	if item["type"] != "function_call" || item["name"] != "Bash" || item["call_id"] != "call_1" {
		t.Fatalf("function_call item: %v", item)
	}
	var args strings.Builder
	for _, e := range evs {
		if e.name == "response.function_call_arguments.delta" {
			args.WriteString(e.data["delta"].(string))
		}
	}
	if args.String() != `{"cmd":"ls"}` {
		t.Errorf("arguments delta 拼接=%q", args.String())
	}
	argDone := findEvent(evs, "response.function_call_arguments.done")
	if argDone == nil || argDone.data["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("arguments.done: %v", argDone)
	}
	itemDone := findEvent(evs, "response.output_item.done")
	if itemDone == nil {
		t.Fatal("缺 output_item.done")
	}
	di := itemDone.data["item"].(map[string]any)
	if di["status"] != "completed" || di["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("output_item.done item: %v", di)
	}
	comp := findEvent(evs, "response.completed").data["response"].(map[string]any)
	out := comp["output"].([]any)
	if len(out) != 1 || out[0].(map[string]any)["type"] != "function_call" {
		t.Errorf("completed.output: %v", out)
	}
	// 纯工具调用 → 不产出空 message item。
	if _, has := comp["output_text"]; has {
		t.Errorf("无正文时不应带 output_text: %v", comp)
	}
}

func TestResponsesStreamLengthBecomesIncomplete(t *testing.T) {
	const body = "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"trunc\"}}]}\n\n" +
		"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 200, body, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/responses", `{"model":"m","input":"q","stream":true}`)
	evs := parseCompatSSE(t, rec.Body.String())
	comp := findEvent(evs, "response.completed").data["response"].(map[string]any)
	if comp["status"] != "incomplete" {
		t.Errorf("finish_reason=length → status incomplete: %v", comp["status"])
	}
	det := comp["incomplete_details"].(map[string]any)
	if det["reason"] != "max_output_tokens" {
		t.Errorf("incomplete_details.reason=%v", det["reason"])
	}
}

func TestResponsesErrorShapeAndStatus(t *testing.T) {
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 400, `{"code":11115,"msg":"prompt is too long: 300000 tokens > 200000 maximum"}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/responses", `{"model":"m","input":"q"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	e, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("Responses 错误体必须是 OpenAI 风格 {\"error\":{...}}: %s", rec.Body)
	}
	if !strings.Contains(e["message"].(string), "prompt is too long") {
		t.Errorf("message 必须是上游原文: %v", e["message"])
	}
	if e["gateway_hint"] == nil || !strings.Contains(e["gateway_hint"].(string), "context") {
		t.Errorf("gateway_hint 未带出: %v", e)
	}
}

func TestResponsesStreamMidStreamErrorEvent(t *testing.T) {
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 200, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n" +
			"data: {\"error\":{\"message\":\"6004 rate limit\",\"code\":\"6004\"}}\n\n" +
			"data: [DONE]\n\n", true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/responses", `{"model":"m","input":"q","stream":true}`)
	evs := parseCompatSSE(t, rec.Body.String())
	if countEvents(evs, "error") != 1 {
		t.Fatalf("应产出 1 个 error 事件: %v", eventNames(evs))
	}
	if countEvents(evs, "response.completed") != 0 {
		t.Errorf("error 之后不得再发 response.completed: %v", eventNames(evs))
	}
	e := findEvent(evs, "error").data
	if e["message"] != "6004 rate limit" {
		t.Errorf("error.message=%v", e["message"])
	}
}

func TestResponsesAllAccountsDownReturns503(t *testing.T) {
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		t.Fatal("无可用账号不应调用上游")
		return 500, "", false
	})
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up})
	rec := postCompat(t, h, "/v1/responses", `{"model":"m","input":"q"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	e := resp["error"].(map[string]any)
	if e["code"] != "no_healthy_account" || e["gateway_hint"] == nil {
		t.Errorf("本地调度错误形态: %v", e)
	}
}
