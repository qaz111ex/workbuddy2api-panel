package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// ---------------------------------------------------------------------------
// 入站翻译：Anthropic 请求体 → chat 请求体
// ---------------------------------------------------------------------------

func TestTranslateAnthropicRequestSystemModelAndParams(t *testing.T) {
	raw := []byte(`{
		"model":"cn:glm-5.2","max_tokens":128,"temperature":0.3,"top_p":0.9,
		"stop_sequences":["STOP","END"],"stream":true,
		"system":"SYS-A",
		"messages":[{"role":"user","content":"hi"}]
	}`)
	tr, cerr := translateAnthropicRequest(raw)
	if cerr != nil {
		t.Fatalf("unexpected error: %s", cerr.message)
	}
	if !tr.stream {
		t.Error("stream 应透传 true")
	}
	if tr.model != "cn:glm-5.2" {
		t.Errorf("model=%q（应保留原始名含 realm 前缀，供响应回显）", tr.model)
	}
	var body map[string]any
	if err := json.Unmarshal(tr.body, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "cn:glm-5.2" {
		t.Errorf("outbound model=%v", body["model"])
	}
	if body["max_tokens"] != float64(128) || body["temperature"] != 0.3 || body["top_p"] != 0.9 {
		t.Errorf("数值参数未透传: %v", body)
	}
	stop, _ := body["stop"].([]any)
	if len(stop) != 2 || stop[0] != "STOP" || stop[1] != "END" {
		t.Errorf("stop_sequences → stop 映射错误: %v", body["stop"])
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len=%d want 2（system 落到 messages[0]）", len(msgs))
	}
	m0 := msgs[0].(map[string]any)
	m1 := msgs[1].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "SYS-A" {
		t.Errorf("system 未落到 messages[0]: %v", m0)
	}
	if m1["role"] != "user" || m1["content"] != "hi" {
		t.Errorf("user 消息错误: %v", m1)
	}
}

func TestTranslateAnthropicRequestSystemBlocksJoined(t *testing.T) {
	raw := []byte(`{"model":"m","system":[{"type":"text","text":"A"},{"type":"text","text":"B"}],
		"messages":[{"role":"user","content":"q"}]}`)
	tr, cerr := translateAnthropicRequest(raw)
	if cerr != nil {
		t.Fatalf("unexpected error: %s", cerr.message)
	}
	var body map[string]any
	_ = json.Unmarshal(tr.body, &body)
	m0 := body["messages"].([]any)[0].(map[string]any)
	if m0["content"] != "A\n\nB" {
		t.Errorf("system 块数组应合成文本: %v", m0["content"])
	}
}

func TestTranslateAnthropicImageBlockToDataURL(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":[
		{"type":"text","text":"look"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}]}`)
	tr, cerr := translateAnthropicRequest(raw)
	if cerr != nil {
		t.Fatalf("unexpected error: %s", cerr.message)
	}
	var body map[string]any
	_ = json.Unmarshal(tr.body, &body)
	content, ok := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if !ok {
		t.Fatalf("含图消息应翻译成 OpenAI 多段 content 数组: %v", body["messages"])
	}
	if len(content) != 2 {
		t.Fatalf("content parts=%d want 2: %v", len(content), content)
	}
	img := content[1].(map[string]any)
	iu := img["image_url"].(map[string]any)
	if img["type"] != "image_url" || iu["url"] != "data:image/png;base64,QUJD" {
		t.Errorf("图片未翻译成 image_url data URL: %v", img)
	}
	// hasImagePart（gateway_hint 的 11133 指向前提）能识别翻译后的形态。
	if !hasImagePart(tr.body) {
		t.Error("翻译后的 body 必须能被 hasImagePart 识别（否则 11133 hint 退化）")
	}
}

func TestTranslateAnthropicToolsAndToolChoice(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"q"}],
		"tools":[
			{"name":"Bash","description":"run a command","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}},
			{"name":"web_search","type":"web_search_20250305"}
		],
		"tool_choice":{"type":"any"}}`)
	tr, cerr := translateAnthropicRequest(raw)
	if cerr != nil {
		t.Fatalf("unexpected error: %s", cerr.message)
	}
	var body map[string]any
	_ = json.Unmarshal(tr.body, &body)
	tools := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("客户端工具 1 个、服务端工具 1 个（丢弃）: got %v", tools)
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "Bash" || fn["description"] != "run a command" {
		t.Errorf("工具字段未映射: %v", fn)
	}
	params, _ := fn["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Errorf("input_schema → parameters 未映射: %v", fn["parameters"])
	}
	if body["tool_choice"].(map[string]any)["type"] != "required" {
		t.Errorf("tool_choice any → required: %v", body["tool_choice"])
	}
}

func TestTranslateAnthropicToolChoiceNamed(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[{"role":"user","content":"q"}],
		"tools":[{"name":"Bash","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"tool","name":"Bash"}}`)
	tr, _ := translateAnthropicRequest(raw)
	var body map[string]any
	_ = json.Unmarshal(tr.body, &body)
	tc := body["tool_choice"].(map[string]any)
	fn := tc["function"].(map[string]any)
	if tc["type"] != "function" || fn["name"] != "Bash" {
		t.Errorf("tool_choice tool → function: %v", tc)
	}
}

// TestTranslateAnthropicToolUseAndToolResultPairing assistant 的 tool_use 块 →
// chat tool_calls；user 的 tool_result 块 → role=tool 消息，且**排在同消息文本之前**
// （OpenAI 配对约束：tool 消息必须紧跟 assistant 的 tool_calls）。
func TestTranslateAnthropicToolUseAndToolResultPairing(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"ls"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"file1"},{"type":"text","text":"continue"}]}
	]}`)
	tr, cerr := translateAnthropicRequest(raw)
	if cerr != nil {
		t.Fatalf("unexpected error: %s", cerr.message)
	}
	var body map[string]any
	_ = json.Unmarshal(tr.body, &body)
	msgs := body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages=%d want 3 (assistant / tool / user): %v", len(msgs), msgs)
	}
	a := msgs[0].(map[string]any)
	if a["role"] != "assistant" || a["content"] != "ok" {
		t.Errorf("assistant 文本丢失: %v", a)
	}
	tcs := a["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_use → tool_calls 失败: %v", a)
	}
	tc := tcs[0].(map[string]any)
	if tc["id"] != "toolu_1" || tc["type"] != "function" {
		t.Errorf("tool_call 身份字段: %v", tc)
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "Bash" || fn["arguments"] != `{"cmd":"ls"}` {
		t.Errorf("tool_call function: %v", fn)
	}
	tool := msgs[1].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "toolu_1" || tool["content"] != "file1" {
		t.Errorf("tool_result → role=tool 失败: %v", tool)
	}
	u := msgs[2].(map[string]any)
	if u["role"] != "user" || u["content"] != "continue" {
		t.Errorf("同消息文本应排到 tool 之后: %v", u)
	}
}

func TestTranslateAnthropicToolResultImagePlaceholder(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"shot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[
			{"type":"text","text":"result"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QQ=="}}]}]}
	]}`)
	tr, _ := translateAnthropicRequest(raw)
	var body map[string]any
	_ = json.Unmarshal(tr.body, &body)
	tool := body["messages"].([]any)[1].(map[string]any)
	txt, _ := tool["content"].(string)
	if !strings.Contains(txt, "result") || !strings.Contains(txt, "image content omitted") {
		t.Errorf("tool_result 图片必须显式占位而非静默丢弃: %q", txt)
	}
}

func TestTranslateAnthropicToolResultIsErrorPrefixed(t *testing.T) {
	raw := []byte(`{"model":"m","messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"boom","is_error":true}]}
	]}`)
	tr, _ := translateAnthropicRequest(raw)
	var body map[string]any
	_ = json.Unmarshal(tr.body, &body)
	tool := body["messages"].([]any)[1].(map[string]any)
	if tool["content"] != "Error: boom" {
		t.Errorf("is_error 语义应显式带出: %v", tool["content"])
	}
}

func TestTranslateAnthropicThinkingToEffort(t *testing.T) {
	cases := []struct {
		budget string
		want   string
	}{
		{`{"type":"enabled","budget_tokens":1024}`, "low"},
		{`{"type":"enabled","budget_tokens":8000}`, "medium"},
		{`{"type":"enabled","budget_tokens":32000}`, "high"},
		{`{"type":"disabled"}`, ""},
	}
	for _, c := range cases {
		got := anthropicThinkingToEffort([]byte(c.budget))
		if got != c.want {
			t.Errorf("thinking %s → effort %q want %q", c.budget, got, c.want)
		}
	}
}

func TestTranslateAnthropicMetadataNotForwarded(t *testing.T) {
	raw := []byte(`{"model":"m","metadata":{"user_id":"u-123"},"messages":[{"role":"user","content":"q"}]}`)
	tr, _ := translateAnthropicRequest(raw)
	var body map[string]any
	_ = json.Unmarshal(tr.body, &body)
	if _, has := body["metadata"]; has {
		t.Error("metadata.user_id 不转发（本仓库契约：带 user_id 会抑制派生粘性键）")
	}
}

func TestTranslateAnthropicValidationErrors(t *testing.T) {
	cases := []struct{ name, raw, want string }{
		{"bad json", `{`, "invalid JSON body"},
		{"no model", `{"messages":[{"role":"user","content":"q"}]}`, "model: field required"},
		{"no messages", `{"model":"m","messages":[]}`, "messages: field required"},
		{"bad role", `{"model":"m","messages":[{"role":"robot","content":"q"}]}`, "messages.0.role"},
		{"bad content shape", `{"model":"m","messages":[{"role":"user","content":42}]}`, "messages.0.content"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, cerr := translateAnthropicRequest([]byte(c.raw))
			if cerr == nil {
				t.Fatalf("want error containing %q", c.want)
			}
			if !strings.Contains(cerr.message, c.want) {
				t.Fatalf("err=%q want contains %q", cerr.message, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 端到端：非流式
// ---------------------------------------------------------------------------

func TestMessagesNonStreamTextEndToEnd(t *testing.T) {
	up, cu := newCompatUpstream(t, func(auth string, body map[string]any) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/messages",
		`{"model":"glm-5.2","max_tokens":64,"system":"SYS","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%s", err, rec.Body)
	}
	if resp["type"] != "message" || resp["role"] != "assistant" {
		t.Errorf("响应形态错误: %v", resp)
	}
	if resp["model"] != "glm-5.2" {
		t.Errorf("model 应回显请求模型名: %v", resp["model"])
	}
	content := resp["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%v", content)
	}
	blk := content[0].(map[string]any)
	if blk["type"] != "text" || blk["text"] != "你好" {
		t.Errorf("正文块错误: %v", blk)
	}
	if resp["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v", resp["stop_reason"])
	}
	if _, has := resp["stop_sequence"]; !has {
		t.Error("stop_sequence 键必须存在（Anthropic 形态）")
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(1) || usage["output_tokens"] != float64(1) {
		t.Errorf("usage 未透出: %v", usage)
	}
	// 上游收到的是翻译后的 chat 请求体。
	b := cu.last(t)
	if b["stream"] != true {
		t.Errorf("上游必须强制 stream=true（payload.go 归一化）: %v", b["stream"])
	}
	msgs := b["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" {
		t.Errorf("上游 messages 形态: %v", msgs)
	}
	if b["max_tokens"] != float64(64) {
		t.Errorf("max_tokens 未透传: %v", b["max_tokens"])
	}
}

func TestMessagesNonStreamToolUseResponse(t *testing.T) {
	up, _ := newCompatUpstream(t, func(auth string, body map[string]any) (int, string, bool) {
		return 200, anthropicToolStreamBody, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/messages",
		`{"model":"glm-5.2","max_tokens":64,"messages":[{"role":"user","content":"run ls"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	content := resp["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%v", content)
	}
	blk := content[0].(map[string]any)
	if blk["type"] != "tool_use" || blk["name"] != "Bash" || blk["id"] != "call_1" {
		t.Errorf("tool_use 块错误: %v", blk)
	}
	in, _ := blk["input"].(map[string]any)
	if in["cmd"] != "ls" {
		t.Errorf("tool_use.input 未从 arguments 解析: %v", blk["input"])
	}
	if resp["stop_reason"] != "tool_use" {
		t.Errorf("有 tool_use 块时 stop_reason 必须是 tool_use: %v", resp["stop_reason"])
	}
}

func TestMessagesUsageCacheReadSubtracted(t *testing.T) {
	const body = "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n" +
		"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]," +
		"\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":5,\"total_tokens\":105,\"cache_read_input_tokens\":80}}\n\n" +
		"data: [DONE]\n\n"
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 200, body, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/messages", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"q"}]}`)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	usage := resp["usage"].(map[string]any)
	// Anthropic 语义：input_tokens 不含缓存命中；命中量单独计数（避免双重计入上下文）。
	if usage["input_tokens"] != float64(20) {
		t.Errorf("input_tokens=%v want 20（prompt 100 - cache_read 80）", usage["input_tokens"])
	}
	if usage["cache_read_input_tokens"] != float64(80) {
		t.Errorf("cache_read_input_tokens 未透出: %v", usage)
	}
	if usage["output_tokens"] != float64(5) {
		t.Errorf("output_tokens=%v", usage["output_tokens"])
	}
}

// ---------------------------------------------------------------------------
// 端到端：流式事件序列
// ---------------------------------------------------------------------------

func TestMessagesStreamEventSequence(t *testing.T) {
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 200, anthropicStreamBody, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/messages",
		`{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type=%q", ct)
	}
	evs := parseCompatSSE(t, rec.Body.String())
	requireEventPattern(t, evs,
		"message_start", "content_block_start", "content_block_delta", "*",
		"content_block_stop", "message_delta", "message_stop")
	if n := countEvents(evs, "message_start"); n != 1 {
		t.Errorf("message_start x%d", n)
	}
	start := findEvent(evs, "message_start").data
	msg := start["message"].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "assistant" || msg["model"] != "glm-5.2" {
		t.Errorf("message_start.message 形态: %v", msg)
	}
	cb := findEvent(evs, "content_block_start").data
	if cb["index"] != float64(0) {
		t.Errorf("content_block_start index=%v", cb["index"])
	}
	if cbm := cb["content_block"].(map[string]any); cbm["type"] != "text" {
		t.Errorf("content_block 形态: %v", cbm)
	}
	// 两段正文 delta 按序拼接。
	var sb strings.Builder
	for _, e := range evs {
		if e.name != "content_block_delta" {
			continue
		}
		d := e.data["delta"].(map[string]any)
		if d["type"] != "text_delta" {
			t.Fatalf("delta 形态: %v", d)
		}
		if e.data["index"] != float64(0) {
			t.Fatalf("delta index=%v", e.data["index"])
		}
		sb.WriteString(d["text"].(string))
	}
	if sb.String() != "你好世界" {
		t.Errorf("正文拼接=%q want 你好世界", sb.String())
	}
	md := findEvent(evs, "message_delta").data
	delta := md["delta"].(map[string]any)
	if delta["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v", delta["stop_reason"])
	}
	usage := md["usage"].(map[string]any)
	if usage["output_tokens"] != float64(4) {
		t.Errorf("message_delta.usage 未透出 output_tokens: %v", usage)
	}
	if usage["input_tokens"] != float64(10) {
		t.Errorf("message_delta.usage 应带真实 input_tokens: %v", usage)
	}
	if n := countEvents(evs, "message_stop"); n != 1 {
		t.Errorf("message_stop x%d want 1", n)
	}
	// 事件行 + data 行成对，且帧间以空行分隔（parseCompatSSE 已强制校验）。
	if !strings.HasSuffix(rec.Body.String(), "\n\n") {
		t.Error("SSE 流应以空行收尾")
	}
}

func TestMessagesStreamToolUseEvents(t *testing.T) {
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 200, anthropicToolStreamBody, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/messages",
		`{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"q"}],
		  "tools":[{"name":"Bash","input_schema":{"type":"object"}}]}`)
	evs := parseCompatSSE(t, rec.Body.String())
	requireEventPattern(t, evs,
		"message_start", "content_block_start", "content_block_delta", "*",
		"content_block_stop", "message_delta", "message_stop")
	cb := findEvent(evs, "content_block_start").data
	blk := cb["content_block"].(map[string]any)
	if blk["type"] != "tool_use" || blk["name"] != "Bash" || blk["id"] != "call_1" {
		t.Fatalf("tool_use 块的 content_block_start 形态: %v", blk)
	}
	if _, has := blk["input"]; !has {
		t.Error("tool_use 块必须带 input 键（Anthropic 形态）")
	}
	var partial strings.Builder
	for _, e := range evs {
		if e.name != "content_block_delta" {
			continue
		}
		d := e.data["delta"].(map[string]any)
		if d["type"] != "input_json_delta" {
			t.Fatalf("工具参数 delta 形态: %v", d)
		}
		partial.WriteString(d["partial_json"].(string))
	}
	if partial.String() != `{"cmd":"ls"}` {
		t.Errorf("partial_json 拼接=%q want {\"cmd\":\"ls\"}", partial.String())
	}
	md := findEvent(evs, "message_delta").data
	if md["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v", md["delta"])
	}
}

func TestMessagesStreamEmptyCompletionKeepsShape(t *testing.T) {
	// 上游只发 role + finish_reason（无正文）：仍必须产出完整事件序列（空文本块），
	// 而不是报错或挂起。
	const body = "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 200, body, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/messages", `{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"q"}]}`)
	evs := parseCompatSSE(t, rec.Body.String())
	requireEventPattern(t, evs,
		"message_start", "content_block_start", "content_block_stop", "message_delta", "message_stop")
	if findEvent(evs, "message_delta").data["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Errorf("空完成 stop_reason 应为 end_turn")
	}
}

func TestMessagesStreamEmptyUpstreamEmitsErrorEvent(t *testing.T) {
	// 上游 200 但 0 有效帧：StreamHint 写兜底 error 帧 → 翻译成 Anthropic error 事件，
	// 且**不补 message_stop**（避免把失败流收尾成成功）。
	const body = "data: [DONE]\n\n"
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 200, body, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/messages", `{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"q"}]}`)
	evs := parseCompatSSE(t, rec.Body.String())
	if countEvents(evs, "error") != 1 {
		t.Fatalf("空流应产出 1 个 error 事件: %v", eventNames(evs))
	}
	if countEvents(evs, "message_stop") != 0 {
		t.Errorf("error 之后不得再发 message_stop: %v", eventNames(evs))
	}
	errObj := findEvent(evs, "error").data["error"].(map[string]any)
	if errObj["message"] != "empty upstream stream" {
		t.Errorf("error.message 应为上游兜底原文: %v", errObj)
	}
	if errObj["code"] != "upstream_parse" {
		t.Errorf("error.code 应透传: %v", errObj)
	}
}

// ---------------------------------------------------------------------------
// 错误语义
// ---------------------------------------------------------------------------

func TestMessagesErrorShapeAndStatus(t *testing.T) {
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 429, `{"code":11140,"msg":"rate limiting, will reset at 12:00"}`, false
	})
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:     up,
		SoftCooldown: time.Minute,
	})
	rec := postCompat(t, h, "/v1/messages", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["type"] != "error" {
		t.Errorf("Anthropic 错误体必须有顶层 type=error: %v", resp)
	}
	e := resp["error"].(map[string]any)
	if e["type"] != "rate_limit_error" {
		t.Errorf("error.type=%v want rate_limit_error", e["type"])
	}
	if !strings.Contains(e["message"].(string), "rate limiting") {
		t.Errorf("message 必须是上游原文透传: %v", e["message"])
	}
	if e["gateway_hint"] == nil || e["gateway_hint"] == "" {
		t.Errorf("gateway_hint 必须一并带出: %v", e)
	}
}

func TestMessagesAllAccountsDownReturns503(t *testing.T) {
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		t.Fatal("无可用账号时不应调用上游")
		return 500, "", false
	})
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up})
	rec := postCompat(t, h, "/v1/messages", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	e := resp["error"].(map[string]any)
	if e["type"] != "api_error" {
		t.Errorf("error.type=%v", e["type"])
	}
	if e["code"] != "no_healthy_account" {
		t.Errorf("error.code=%v want no_healthy_account", e["code"])
	}
	if e["gateway_hint"] == nil {
		t.Error("本地调度错误也要带 gateway_hint（no healthy account）")
	}
}

func TestMessagesStreamErrorBeforeFirstTokenKeepsHTTPStatus(t *testing.T) {
	// 上游 400 + 11115（请求级确定性错误，既有链路 fail-fast 不轮转）：流式请求也必须
	// 拿到 HTTP 400 + Anthropic 错误体（首个内容帧之前不写 SSE 头，这正是「延迟写头」
	// 设计的收益：错误还能带上正确状态码）。
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 400, `{"code":11115,"msg":"prompt is too long: 300000 > 200000"}`, false
	})
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:     up,
		SoftCooldown: time.Minute,
	})
	rec := postCompat(t, h, "/v1/messages", `{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("错误路径必须是 JSON 而非 SSE: %q", ct)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["type"] != "error" {
		t.Errorf("流式请求的错误体仍是 Anthropic JSON 错误形态: %v", resp)
	}
	e := resp["error"].(map[string]any)
	if !strings.Contains(e["message"].(string), "prompt is too long") {
		t.Errorf("message 必须是上游原文: %v", e["message"])
	}
}

// TestMessagesStreamErrorBeforeFirstTokenHardCredit 402 余额耗尽 + 单账号池：账号被
// 既有 applyErrorPolicy 冷却后本轮无号可用，末端收敛为既有 503 no_healthy_account
// （hint 仍指出是账号积分耗尽）——**状态码语义沿用既有 chat 链路**，兼容层不另起一套
// 映射（见交付报告的状态码映射表）。
func TestMessagesStreamErrorBeforeFirstTokenHardCredit(t *testing.T) {
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 402, `{"code":14018,"msg":"credits exhausted"}`, false
	})
	h := NewHandler(Config{
		Pool:         testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:     up,
		SoftCooldown: time.Minute,
	})
	rec := postCompat(t, h, "/v1/messages", `{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503（单账号被冷却后无号可用）body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("错误路径必须是 JSON 而非 SSE: %q", ct)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	e := resp["error"].(map[string]any)
	if !strings.Contains(e["message"].(string), "credits exhausted") {
		t.Errorf("上游原文必须透传: %v", e["message"])
	}
	if e["gateway_hint"] == nil {
		t.Error("gateway_hint 必须带出（账号积分耗尽）")
	}
}

func TestMessagesInvalidRequestRejectedWithoutUpstream(t *testing.T) {
	up, cu := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		t.Fatal("校验失败不应打上游")
		return 500, "", false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/messages", `{"messages":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	e := resp["error"].(map[string]any)
	if resp["type"] != "error" || e["type"] != "invalid_request_error" {
		t.Errorf("Anthropic 400 错误体形态: %v", resp)
	}
	if cu.count() != 0 {
		t.Errorf("上游调用数=%d want 0", cu.count())
	}
}

func TestMessagesAuthFailureUsesAnthropicShape(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: newFakeUpstream(t,
		func(string) (int, string, bool) { return 200, sseOK, true }), APIKey: "sk-secret"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"q"}]}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["type"] != "error" {
		t.Fatalf("Anthropic 客户端需要 Anthropic 形状的 401: %s", rec.Body)
	}
	e := resp["error"].(map[string]any)
	if e["type"] != "authentication_error" {
		t.Errorf("error.type=%v", e["type"])
	}
	// 正确 key 放行。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"q"}]}`))
	req2.Header.Set("Authorization", "Bearer sk-secret")
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("count_tokens 带正确 key 应 200: code=%d body=%s", rec2.Code, rec2.Body)
	}
}

// ---------------------------------------------------------------------------
// 复用既有账号管线（轮转 / 冷却 / 粘性）
// ---------------------------------------------------------------------------

func TestMessagesReusesAccountRotation(t *testing.T) {
	calls := map[string]int{}
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		calls[auth]++
		if auth == "Bearer at-bad" {
			return 500, `{"code":500,"msg":"boom"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // 确定性随机源 r=0 → 先选 bad
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := postCompat(t, h, "/v1/messages", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s（应轮转后成功）", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("轮转调用数=%v want bad/good 各 1", calls)
	}
	// 既有 applyErrorPolicy 生效：bad 被 5xx 喂熔断计数。
	st, _ := p.Status("bad")
	if st.ErrTotal == 0 && st.BreakerFails == 0 {
		t.Errorf("上游 5xx 必须经既有 applyErrorPolicy 记账: %+v", st)
	}
}

func TestMessagesReusesSoftCooldownPolicy(t *testing.T) {
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 429, `{"code":6004,"msg":"rate limit, will reset at 23:59"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	postCompat(t, h, "/v1/messages", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"q"}]}`)
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatalf("429 必须经既有 applyErrorPolicy 进入冷却: %+v", st)
	}
}

func TestMessagesReusesStickySession(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		if auth == "Bearer at-bad" {
			return 500, `{"code":500}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"sticky-q"}]}`)))
	// 粘性键来自「system + 首条 user」派生（Anthropic 无 conversation id 字段），
	// 成功号的绑定必须落在 store 里。
	binds := st.LoadBinds()
	if len(binds) == 0 {
		t.Fatalf("Anthropic 请求必须经既有粘性路由落绑定: %v", binds)
	}
	for _, uid := range binds {
		if uid != "good" {
			t.Fatalf("绑定应收敛到最终成功号 good: %v", binds)
		}
	}
	// 粘性键必须在两轮之间稳定（同会话钉同号）。
	sess2 := newBindStore()
	sessR := session.New(session.Config{TTL: time.Minute, Store: sess2, Available: func() []string { return []string{"good"} }})
	p2 := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	h2 := NewHandler(Config{Pool: p2, Upstream: up, Session: sessR, SoftCooldown: time.Minute})
	for i := 0; i < 2; i++ {
		h2.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages",
			strings.NewReader(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"same-q"}]}`)))
	}
	if len(sess2.LoadBinds()) != 1 {
		t.Fatalf("同会话两轮必须复用同一粘性键: %v", sess2.LoadBinds())
	}
}

// ---------------------------------------------------------------------------
// count_tokens
// ---------------------------------------------------------------------------

func TestMessagesCountTokensEstimate(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	short := postCompat(t, h, "/v1/messages/count_tokens",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	long := postCompat(t, h, "/v1/messages/count_tokens",
		`{"model":"m","messages":[{"role":"user","content":"`+strings.Repeat("a", 400)+`"}]}`)
	if short.Code != 200 || long.Code != 200 {
		t.Fatalf("code=%d/%d", short.Code, long.Code)
	}
	var s, l map[string]any
	_ = json.Unmarshal(short.Body.Bytes(), &s)
	_ = json.Unmarshal(long.Body.Bytes(), &l)
	si := int(s["input_tokens"].(float64))
	li := int(l["input_tokens"].(float64))
	if si <= 0 || li <= si {
		t.Errorf("估算值应随文本增长: short=%d long=%d", si, li)
	}
	if li < 90 || li > 130 {
		t.Errorf("400 字符 ASCII 估算应约 100: got %d", li)
	}
	// CJK 按 1 字符 1 token。
	cjk := postCompat(t, h, "/v1/messages/count_tokens",
		`{"model":"m","messages":[{"role":"user","content":"你好世界你好世界"}]}`)
	var c map[string]any
	_ = json.Unmarshal(cjk.Body.Bytes(), &c)
	if got := int(c["input_tokens"].(float64)); got < 8 {
		t.Errorf("CJK 8 字估算应 >= 8: got %d", got)
	}
}

// ---------------------------------------------------------------------------
// 双 WriteHeader 防护（最高优先）
// ---------------------------------------------------------------------------

// TestCompatHeaderWrittenOnce 锁定「任何情况下真实 ResponseWriter 只写一次头」。
// 覆盖：非流式成功/失败、流式成功、流式中途 error 帧、空流、无可用账号、
// 校验失败、鉴权失败。superfluous 必须是 0（= 不会有 net/http 的
// "superfluous response.WriteHeader call"）。
func TestCompatHeaderWrittenOnce(t *testing.T) {
	okAuth := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	cases := []struct {
		name       string
		path       string
		body       string
		upstream   func(auth string, b map[string]any) (int, string, bool)
		pool       func() *pool.Pool
		apiKey     string
		authHeader string
		wantStatus int
	}{
		{
			name: "non-stream success", path: "/v1/messages",
			body:       `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"q"}]}`,
			upstream:   func(string, map[string]any) (int, string, bool) { return 200, sseOK, true },
			wantStatus: 200,
		},
		{
			name: "non-stream error", path: "/v1/messages",
			body:       `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"q"}]}`,
			upstream:   func(string, map[string]any) (int, string, bool) { return 429, `{"code":6004,"msg":"slow down"}`, false },
			wantStatus: 429,
		},
		{
			name: "stream success", path: "/v1/messages",
			body:       `{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"q"}]}`,
			upstream:   func(string, map[string]any) (int, string, bool) { return 200, anthropicStreamBody, true },
			wantStatus: 200,
		},
		{
			name: "stream mid-stream error frame", path: "/v1/messages",
			body: `{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"q"}]}`,
			upstream: func(string, map[string]any) (int, string, bool) {
				return 200, "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n" +
					"data: {\"error\":{\"message\":\"6004 rate limit\",\"code\":\"6004\"}}\n\n" +
					"data: [DONE]\n\n", true
			},
			wantStatus: 200,
		},
		{
			name: "stream empty upstream", path: "/v1/messages",
			body:       `{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"q"}]}`,
			upstream:   func(string, map[string]any) (int, string, bool) { return 200, "data: [DONE]\n\n", true },
			wantStatus: 200,
		},
		{
			name: "stream error before first token", path: "/v1/messages",
			body:       `{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"q"}]}`,
			upstream:   func(string, map[string]any) (int, string, bool) { return 400, `{"code":11115,"msg":"too long"}`, false },
			wantStatus: 400,
		},
		{
			name: "no healthy account", path: "/v1/messages",
			body:       `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"q"}]}`,
			upstream:   func(string, map[string]any) (int, string, bool) { return 200, sseOK, true },
			pool:       func() *pool.Pool { return testPoolWith() },
			wantStatus: 503,
		},
		{
			name: "validation failure", path: "/v1/messages",
			body:       `{"messages":[]}`,
			upstream:   func(string, map[string]any) (int, string, bool) { return 200, sseOK, true },
			wantStatus: 400,
		},
		{
			name: "auth failure", path: "/v1/messages",
			body:       `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"q"}]}`,
			upstream:   func(string, map[string]any) (int, string, bool) { return 200, sseOK, true },
			apiKey:     "sk-secret",
			wantStatus: 401,
		},
		{
			name: "count_tokens", path: "/v1/messages/count_tokens",
			body:       `{"model":"m","messages":[{"role":"user","content":"q"}]}`,
			upstream:   func(string, map[string]any) (int, string, bool) { return 200, sseOK, true },
			wantStatus: 200,
		},
		{
			name: "responses non-stream success", path: "/v1/responses",
			body:       `{"model":"m","input":"q"}`,
			upstream:   func(string, map[string]any) (int, string, bool) { return 200, sseOK, true },
			wantStatus: 200,
		},
		{
			name: "responses stream success", path: "/v1/responses",
			body:       `{"model":"m","input":"q","stream":true}`,
			upstream:   func(string, map[string]any) (int, string, bool) { return 200, anthropicStreamBody, true },
			wantStatus: 200,
		},
		{
			name: "responses stream error", path: "/v1/responses",
			body:       `{"model":"m","input":"q","stream":true}`,
			upstream:   func(string, map[string]any) (int, string, bool) { return 503, `{"code":500}`, false },
			wantStatus: 503,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up, _ := newCompatUpstream(t, c.upstream)
			cfg := Config{Upstream: up, APIKey: c.apiKey, SoftCooldown: time.Minute}
			if c.pool != nil {
				cfg.Pool = c.pool()
			} else {
				cfg.Pool = testPoolWith(okAuth)
			}
			h := NewHandler(cfg)
			cw := newHeaderCountingWriter()
			req := httptest.NewRequest("POST", c.path, strings.NewReader(c.body))
			if c.authHeader != "" {
				req.Header.Set("Authorization", c.authHeader)
			}
			h.ServeHTTP(cw, req)
			if !cw.committed {
				t.Fatalf("响应头从未写出（零输出）")
			}
			if cw.superfluous != 0 {
				t.Errorf("二次 WriteHeader 次数=%d want 0（net/http 会打 superfluous 警告）", cw.superfluous)
			}
			if cw.status != c.wantStatus {
				t.Errorf("status=%d want %d body=%s", cw.status, c.wantStatus, cw.body.String())
			}
		})
	}
}

// TestCompatStreamWriterInnerFlushDoesNotEarlyCommit 直击 misakano7545 435cb8ec
// 的坑：内层（StreamHint 的逐帧 flush）在 emitter 首个事件之前 Flush。若内层
// Flush 透传到真实 writer，net/http 会隐式提交 200，随后 emitter 的显式
// WriteHeader(200) 就是 superfluous 二次写头。本测试锁死内层 Flush 被吞掉。
func TestCompatStreamWriterInnerFlushDoesNotEarlyCommit(t *testing.T) {
	cw := newHeaderCountingWriter()
	em := newCompatEmitter(cw)
	w := newAnthropicStreamWriter(em, "glm-5.2")
	w.Flush() // 内层空流 flush（提交语义最脆弱的一刻）
	if cw.committed {
		t.Fatal("内层 Flush 不得提交真实响应（否则 emitter 写头即 superfluous）")
	}
	if _, err := w.Write([]byte(anthropicStreamBody)); err != nil {
		t.Fatal(err)
	}
	w.finish()
	if cw.superfluous != 0 {
		t.Fatalf("superfluous=%d want 0", cw.superfluous)
	}
	if cw.status != 200 {
		t.Fatalf("status=%d want 200", cw.status)
	}
	if !strings.Contains(cw.body.String(), "event: message_stop") {
		t.Fatalf("流未收尾: %q", cw.body.String())
	}
	// finish 幂等：重复收尾不得再写头/再写事件。
	before := cw.body.Len()
	w.finish()
	if cw.superfluous != 0 || cw.body.Len() != before {
		t.Errorf("finish 必须幂等: superfluous=%d bodyGrow=%d", cw.superfluous, cw.body.Len()-before)
	}
}

// TestCompatResponsesStreamWriterInnerFlushDoesNotEarlyCommit 同上，Responses 侧。
func TestCompatResponsesStreamWriterInnerFlushDoesNotEarlyCommit(t *testing.T) {
	cw := newHeaderCountingWriter()
	em := newCompatEmitter(cw)
	w := newResponsesStreamWriter(em, "glm-5.2")
	w.Flush()
	w.Flush()
	if cw.committed {
		t.Fatal("内层 Flush 不得提交真实响应")
	}
	if _, err := w.Write([]byte(anthropicStreamBody)); err != nil {
		t.Fatal(err)
	}
	w.finish()
	if cw.superfluous != 0 {
		t.Fatalf("superfluous=%d want 0", cw.superfluous)
	}
	if !strings.Contains(cw.body.String(), "event: response.completed") {
		t.Fatalf("流未收尾: %q", cw.body.String())
	}
}

// TestCompatEmitterBeginsOnce 直接对 emitter 闩锁做白盒断言：任何后续 begin/事件
// 都不得触发第二次 WriteHeader。
func TestCompatEmitterBeginsOnce(t *testing.T) {
	cw := newHeaderCountingWriter()
	em := newCompatEmitter(cw)
	em.emitJSON(200, map[string]any{"a": 1})
	em.emitJSON(500, map[string]any{"b": 2})
	em.emitEvent("message_stop", map[string]any{"type": "message_stop"})
	if cw.superfluous != 0 {
		t.Fatalf("superfluous=%d want 0", cw.superfluous)
	}
	if cw.status != 200 {
		t.Fatalf("status=%d want 200（第二次 begin 必须被闩锁拒绝）", cw.status)
	}
	if strings.Contains(cw.body.String(), "\"b\"") {
		t.Error("闩锁后的写入必须被拒绝")
	}
}

// TestMessagesStreamParallelToolCalls 并行工具调用（Claude Code 高频形态）：两个
// tool_call index 必须各开一个 tool_use 块（content 索引升序），各自的 arguments
// 分片进各自的块，不能串到同一块里。
func TestMessagesStreamParallelToolCalls(t *testing.T) {
	const body = "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[" +
		"{\"index\":0,\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"Read\",\"arguments\":\"\"}}," +
		"{\"index\":1,\"id\":\"call_b\",\"type\":\"function\",\"function\":{\"name\":\"Grep\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"f\\\":1}\"}},{\"index\":1,\"function\":{\"arguments\":\"{\\\"g\\\":2}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 200, body, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/messages", `{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"q"}]}`)
	evs := parseCompatSSE(t, rec.Body.String())
	starts := []compatEvent{}
	for _, e := range evs {
		if e.name == "content_block_start" {
			starts = append(starts, e)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("并行工具应开 2 个 content block: %v", eventNames(evs))
	}
	wantNames := []string{"Read", "Grep"}
	wantIDs := []string{"call_a", "call_b"}
	gotArgs := map[float64]string{}
	for i, st := range starts {
		if st.data["index"] != float64(i) {
			t.Errorf("块索引应升序: %v", st.data["index"])
		}
		cb := st.data["content_block"].(map[string]any)
		if cb["type"] != "tool_use" || cb["name"] != wantNames[i] || cb["id"] != wantIDs[i] {
			t.Errorf("块 %d 形态: %v", i, cb)
		}
	}
	for _, e := range evs {
		if e.name != "content_block_delta" {
			continue
		}
		if e.data["delta"].(map[string]any)["type"] != "input_json_delta" {
			t.Fatalf("工具流里不得出现 text_delta: %v", e.data)
		}
		idx := e.data["index"].(float64)
		gotArgs[idx] += e.data["delta"].(map[string]any)["partial_json"].(string)
	}
	if gotArgs[0] != `{"f":1}` || gotArgs[1] != `{"g":2}` {
		t.Errorf("并行工具参数串块: %v", gotArgs)
	}
	if md := findEvent(evs, "message_delta").data; md["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v", md["delta"])
	}
}

// TestMessagesNonStreamToolCallsWithoutFinishReason stop_reason 缺失但有 tool_calls
// 时仍必须报 tool_use（否则客户端会当成正常收尾、丢掉工具调用语义）。
func TestMessagesNonStreamToolCallsWithoutFinishReason(t *testing.T) {
	const body = "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[" +
		"{\"index\":0,\"id\":\"call_x\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 200, body, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/messages", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"q"}]}`)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason=%v want tool_use", resp["stop_reason"])
	}
	if len(resp["content"].([]any)) != 1 {
		t.Fatalf("content=%v", resp["content"])
	}
}
