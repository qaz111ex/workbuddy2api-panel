// compat_messages.go Anthropic Messages API（`POST /v1/messages`）兼容层。
//
// 入站翻译：Anthropic 请求体 → OpenAI chat 请求体（system 独立字段落 messages[0]、
// content 块数组展开、tools/tool_choice 尽力映射、thinking 预算 → reasoning_effort）。
// 出站翻译：既有 chat 链路的聚合响应 / SSE chunk → Anthropic message 对象 /
// Anthropic SSE 事件序列。
//
// 全程复用 h.chatCompletions（见 compat.go 的设计说明），账号轮转、粘性、冷却/
// 熔断、applyErrorPolicy、跨域回落、错误透传全部走既有那一套。
//
// 已知取舍（**明确列出，不静默丢弃**，详见 README/交付报告的限制清单）：
//   - `metadata.user_id` 不转发：chat 形态无该字段，且本仓库既有契约规定带
//     user_id 的请求**抑制**派生粘性键（user 维度粘性过粗），转发会让 Claude Code
//     这类只发 user_id 的客户端彻底失去按会话粘号。丢弃后由 system+首条 user 消息
//     派生会话级粘性键，"同一对话钉同一账号"反而成立。
//   - `thinking` / `redacted_thinking` 内容块不回传（Anthropic 的 thinking 块必须带
//     signature，网关无法合成；伪造会被严格客户端拒绝），但请求侧 `thinking` 预算会
//     映射成 OpenAI `reasoning_effort`（启发式分档，出站再由 payload.go 按模型
//     supportedEfforts 降级）。
//   - 上游 delta 里的 `reasoning_content`（思维链）不透出为 thinking 块（同上），
//     也不折进正文。
//   - `document`（PDF）块、服务端工具（`{"type":"web_search_..."}`）无 chat 形态对应
//     → 丢弃（代码内有显式分支与注释，不回传占位文本污染上下文）。
//   - `tool_result` 里的图片无法进入 OpenAI tool 消息 → 转成显式占位文本（可见，
//     非静默丢失）。
//   - `top_k`、`disable_parallel_tool_use` 不透传（上游只认 chat 白名单字段）。
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// 入站：Anthropic Messages 请求 → chat 请求
// ---------------------------------------------------------------------------

type anthropicMessagesRequest struct {
	Model         string            `json:"model"`
	System        json.RawMessage   `json:"system"`
	Messages      []json.RawMessage `json:"messages"`
	MaxTokens     *int              `json:"max_tokens"`
	Temperature   *float64          `json:"temperature"`
	TopP          *float64          `json:"top_p"`
	StopSequences []string          `json:"stop_sequences"`
	Stream        bool              `json:"stream"`
	Tools         []json.RawMessage `json:"tools"`
	ToolChoice    json.RawMessage   `json:"tool_choice"`
	Thinking      json.RawMessage   `json:"thinking"`
}

// compatChatRequest 翻译结果（chat 请求体 + 流式标志 + 客户端原始模型名）。
type compatChatRequest struct {
	body   []byte
	stream bool
	model  string // 客户端原始模型名（响应回显；保留 cn:/global: 前缀）
}

// anthropicSystemText 把 Anthropic 顶层 `system`（字符串或文本块数组）合成一段文本。
// 块数组按 "\n\n" 连接——客户端常把「缓存前缀段 + 变动段」拆成多块，保留段落边界。
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if t, _ := b["type"].(string); t != "text" {
			continue
		}
		if txt, _ := b["text"].(string); txt != "" {
			parts = append(parts, txt)
		}
	}
	return strings.Join(parts, "\n\n")
}

// anthropicImageURL 把 Anthropic image 块转成 image_url 字符串（base64 → data URL）。
func anthropicImageURL(b map[string]any) string {
	src, _ := b["source"].(map[string]any)
	if src == nil {
		u, _ := b["url"].(string)
		return u
	}
	switch t, _ := src["type"].(string); t {
	case "base64":
		data, _ := src["data"].(string)
		if data == "" {
			return ""
		}
		mt, _ := src["media_type"].(string)
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + data
	case "url":
		u, _ := src["url"].(string)
		return u
	}
	return ""
}

// anthropicToolResultText 取 tool_result 的文本内容；数组形态拼接文本块，
// 图片块转**显式占位**（OpenAI tool 消息不能带图片，静默丢弃会让模型以为没这回事）。
func anthropicToolResultText(v any) string {
	switch c := v.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch t, _ := m["type"].(string); t {
			case "text":
				if txt, _ := m["text"].(string); txt != "" {
					sb.WriteString(txt)
				}
			case "image":
				sb.WriteString("\n[image content omitted: the OpenAI tool message format cannot carry images]")
			}
		}
		return strings.TrimSpace(sb.String())
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// anthropicBlocksToChat 把一条 Anthropic 消息的 content 翻译成 0..n 条 chat 消息。
//
// 展开规则（OpenAI 配对约束优先）：
//   - assistant 的 text 块 → 文本；tool_use 块 → 同一 assistant 消息的 tool_calls；
//   - user 的 tool_result 块 → 独立 role=tool 消息，且**排在同消息的文本之前**
//     （OpenAI 要求 tool 消息紧跟带 tool_calls 的 assistant 消息；Anthropic 则把
//     tool_result 放在 user 消息里，两者顺序语义不同，此处 repack）；
//   - 其余块（thinking/document/未知）丢弃，分支内注明原因。
func anthropicBlocksToChat(role string, raw json.RawMessage) ([]any, *compatReqError) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, badRequest("content: field required")
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		return []any{map[string]any{"role": role, "content": s}}, nil
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, badRequest("content: must be a string or an array of content blocks")
	}

	var texts []string
	var parts []any
	hasImage := false
	var toolCalls []any
	var toolResults []any
	for _, b := range blocks {
		switch typ, _ := b["type"].(string); typ {
		case "text":
			if t, _ := b["text"].(string); t != "" {
				texts = append(texts, t)
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		case "image":
			url := anthropicImageURL(b)
			if url == "" {
				continue
			}
			hasImage = true
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		case "tool_use":
			if role != "assistant" {
				continue // tool_use 只出现在 assistant 消息；其他位置属畸形输入
			}
			name, _ := b["name"].(string)
			if name == "" {
				continue // 无名字的 tool_use 无法映射成 function 调用
			}
			id, _ := b["id"].(string)
			if id == "" {
				id = newCompatID("call_")
			}
			args := "{}"
			if in, ok := b["input"]; ok && in != nil {
				if rawIn, err := json.Marshal(in); err == nil {
					args = string(rawIn)
				}
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			})
		case "tool_result":
			if role != "user" {
				continue
			}
			tid, _ := b["tool_use_id"].(string)
			if tid == "" {
				continue
			}
			content := anthropicToolResultText(b["content"])
			if isErr, _ := b["is_error"].(bool); isErr && !strings.HasPrefix(content, "Error") {
				// OpenAI 的 tool 消息没有 is_error 标志位：把错误语义显式写进文本，
				// 否则模型会把失败结果当成成功结果继续推理。
				content = "Error: " + content
			}
			toolResults = append(toolResults, map[string]any{
				"role": "tool", "tool_call_id": tid, "content": content,
			})
		case "thinking", "redacted_thinking":
			// 丢弃：chat 形态无 thinking 块；Anthropic 要求带 signature，网关无法合成。
		case "document":
			// 丢弃：上游 chat 形态不支持文档块（PDF），无等价表示。
		default:
			// 未知块类型：丢弃（不编造内容、也不注入占位文本污染上下文）。
		}
	}

	var out []any
	switch role {
	case "assistant":
		if len(toolCalls) > 0 {
			out = append(out, map[string]any{
				"role": "assistant", "content": strings.Join(texts, "\n"), "tool_calls": toolCalls,
			})
		} else if len(texts) > 0 {
			out = append(out, map[string]any{"role": "assistant", "content": strings.Join(texts, "\n")})
		}
	case "user":
		out = append(out, toolResults...) // 配对约束：tool 消息必须在 user 文本之前
		if hasImage {
			if len(parts) > 0 {
				out = append(out, map[string]any{"role": "user", "content": parts})
			}
		} else if len(texts) > 0 {
			out = append(out, map[string]any{"role": "user", "content": strings.Join(texts, "\n")})
		}
	default:
		if len(texts) > 0 {
			out = append(out, map[string]any{"role": role, "content": strings.Join(texts, "\n")})
		}
	}
	return out, nil
}

// anthropicToolsToChat 客户端工具 → OpenAI chat tools。
// 客户端工具带 input_schema；Anthropic 的**服务端工具**（如 web_search_20250305）
// 带 type 且无 input_schema——chat 形态无对应实现，丢弃（分支显式，不静默）。
func anthropicToolsToChat(raws []json.RawMessage) []any {
	tools := make([]any, 0, len(raws))
	for _, raw := range raws {
		var t struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if json.Unmarshal(raw, &t) != nil {
			continue
		}
		if t.Name == "" {
			continue
		}
		if t.Type != "" && t.Type != "custom" {
			continue // 服务端工具：无 chat 等价物
		}
		params := any(map[string]any{"type": "object", "properties": map[string]any{}})
		if len(t.InputSchema) > 0 && string(t.InputSchema) != "null" {
			var sch any
			if json.Unmarshal(t.InputSchema, &sch) == nil {
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

// anthropicToolChoiceToChat Anthropic tool_choice → OpenAI tool_choice。
// auto → auto；any → required；tool → 指定 function；none → none（payload.go 会
// 顺带清掉 tools）。上游该字段是 string，出站归一化由 payload.normalizeToolChoice 完成。
func anthropicToolChoiceToChat(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return map[string]any{"type": "auto"}
	case "any":
		return map[string]any{"type": "required"}
	case "none":
		return map[string]any{"type": "none"}
	case "tool":
		if tc.Name == "" {
			return map[string]any{"type": "auto"}
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	}
	return nil
}

// anthropicThinkingToEffort Anthropic thinking 预算 → OpenAI reasoning_effort。
// Anthropic 用 token 预算、OpenAI 用离散档位，映射只能是启发式；出站时
// payload.normalizeReasoningEffort 会按模型 supportedEfforts 再降级，不会发出
// 模型不支持的档位（安全）。
func anthropicThinkingToEffort(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var t struct {
		Type   string `json:"type"`
		Budget int    `json:"budget_tokens"`
	}
	if json.Unmarshal(raw, &t) != nil || t.Type != "enabled" {
		return ""
	}
	switch {
	case t.Budget <= 0:
		return "medium"
	case t.Budget < 4096:
		return "low"
	case t.Budget < 16384:
		return "medium"
	default:
		return "high"
	}
}

// translateAnthropicRequest 入站校验 + 翻译。校验错误按 Anthropic 400 形态返回。
func translateAnthropicRequest(raw []byte) (*compatChatRequest, *compatReqError) {
	var req anthropicMessagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, badRequest("invalid JSON body: " + err.Error())
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		return nil, badRequest("model: field required")
	}
	if len(req.Messages) == 0 {
		return nil, badRequest("messages: field required")
	}
	msgs := make([]any, 0, len(req.Messages)+1)
	if sys := anthropicSystemText(req.System); strings.TrimSpace(sys) != "" {
		// Anthropic 的 system 是独立顶层字段，OpenAI 落在 messages[0]（顺序语义等价）。
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for i, rm := range req.Messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(rm, &m) != nil {
			return nil, badRequest(fmt.Sprintf("messages.%d: invalid message object", i))
		}
		role := strings.TrimSpace(m.Role)
		switch role {
		case "user", "assistant", "system", "developer":
			// system/developer 在 Anthropic 规范里只出现在顶层；此处宽松接受。
		default:
			return nil, badRequest(fmt.Sprintf("messages.%d.role: Input should be 'user' or 'assistant'", i))
		}
		out, cerr := anthropicBlocksToChat(role, m.Content)
		if cerr != nil {
			return nil, badRequest(fmt.Sprintf("messages.%d.%s", i, cerr.message))
		}
		msgs = append(msgs, out...)
	}
	if len(msgs) == 0 {
		return nil, badRequest("messages: field required")
	}

	out := map[string]any{"model": model, "messages": msgs, "stream": req.Stream}
	if req.MaxTokens != nil {
		out["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	if len(req.Tools) > 0 {
		if tools := anthropicToolsToChat(req.Tools); len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if tc := anthropicToolChoiceToChat(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if effort := anthropicThinkingToEffort(req.Thinking); effort != "" {
		out["reasoning_effort"] = effort
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, badRequest("failed to encode translated chat request: " + err.Error())
	}
	return &compatChatRequest{body: body, stream: req.Stream, model: model}, nil
}

// ---------------------------------------------------------------------------
// 入站：anthropic.default_model 兜底替换（**仅** /v1/messages）
// ---------------------------------------------------------------------------

// anthropicModelSubstitution 一次模型兜底替换的事实（响应标注 + INFO 日志用）。
// 之所以必须显式透出，是因为静默把 claude-sonnet-4-* 换成目录里的模型会让用户
// 困惑「我明明要 sonnet，怎么答的是别的模型」。
type anthropicModelSubstitution struct {
	requested string // 客户端原始请求名（响应 model 仍回显它）
	effective string // 实际出站模型名（可能带 cn:/global: 前缀）
}

// 替换标注字段名（非标准扩展字段；Anthropic SDK 忽略未知键，排障与客户端自检可读）。
const (
	anthropicSubstitutedField = "gateway_model_substituted" // bool：是否发生过替换
	anthropicRequestedField   = "gateway_model_requested"   // string：客户端原名
	anthropicEffectiveField   = "gateway_model_effective"   // string：实际出站名
)

// markAnthropicModelSubstitution 把替换事实标注到 Anthropic message 对象上。
// sub == nil（未替换）时不加任何字段——未配置 anthropic.default_model 的响应形状
// 与改造前逐字节一致。
func markAnthropicModelSubstitution(obj map[string]any, sub *anthropicModelSubstitution) {
	if sub == nil {
		return
	}
	obj[anthropicSubstitutedField] = true
	obj[anthropicRequestedField] = sub.requested
	obj[anthropicEffectiveField] = sub.effective
}

// normalizeAnthropicRealm 归一 anthropic.default_realm：只认 cn / global（大小写不
// 敏感、容忍首尾空白），其余（含空串）→ 无前缀。非法值退化为裸名，而不是拼出网关
// 路由协议不认的前缀——"weird:glm-5.2" 会被 resolveModel 当成裸名（冒号留在里面），
// 那比不带前缀更糟。
func normalizeAnthropicRealm(realm string) string {
	switch strings.ToLower(strings.TrimSpace(realm)) {
	case "cn":
		return "cn"
	case "global":
		return "global"
	}
	return ""
}

// anthropicModelInCatalog 报告裸模型名是否已命中网关只读目录快照（与 hintContext /
// realmModelState("cn") 同一数据源：cachedModelsSnapshot）。缓存冷 / 过期 / 未拉取
// → false（宁缺勿滥，不编造目录事实）。**零上游调用**。
func anthropicModelInCatalog(model string) bool {
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == model {
			return true
		}
	}
	return false
}

// substituteAnthropicModel 纯函数：决定 /v1/messages 的最终出站模型名，返回
// (最终模型名, 是否发生替换)。
//
// 规则（严格按接口契约）：
//  1. 请求名带 cn: / global: 前缀 → 原样不动（显式前缀是网关路由协议的表达，
//     用户显式指定的模型一律不篡改）；
//  2. 请求名已命中网关只读目录 → 原样不动（同上：不能改掉用户显式指定的可用模型）；
//  3. 未命中且 defModel 非空 → 替换为 [defRealm:]defModel（realm 非法/为空则裸名）；
//  4. defModel 为空 → 原样返回 + false（未配置时行为与现状逐字一致）。
//
// 目录判定只读 cachedModelsSnapshot，**绝不触发上游拉取**：请求路径上加一次
// FetchModels 网络调用既拖慢首字延迟，又与本仓库「错误/hint 路径不拉上游」的既有
// 设计相悖。
func substituteAnthropicModel(reqModel, defModel, defRealm string) (string, bool) {
	req := strings.TrimSpace(reqModel)
	def := strings.TrimSpace(defModel)
	if req == "" || def == "" {
		return reqModel, false // 规则 4：未配置 → 现状语义
	}
	if HasRealmPrefix(req) {
		return reqModel, false // 规则 1：显式前缀原样
	}
	if anthropicModelInCatalog(req) {
		return reqModel, false // 规则 2：命中目录原样
	}
	if realm := normalizeAnthropicRealm(defRealm); realm != "" {
		return realm + ":" + def, true // 规则 3
	}
	return def, true
}

// rewriteChatBodyModel 把翻译后的 chat 请求体里的 model 换成 final，其余字段逐字
// 保留（body 本就是 map → json.Marshal 的产物，键序稳定，重写无额外语义变化）。
func rewriteChatBodyModel(body []byte, final string) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	obj["model"] = final
	raw, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return raw
}

// applyAnthropicDefaultModel /v1/messages 入站路径上的兜底替换入口。未替换 → nil
// 且 tr 逐字不变（未配置字段时零行为变化）；发生替换 → 重写 chat body 的 model
// 字段 + INFO 日志 + 返回替换事实供响应标注。
//
// tr.model（响应回显用）**刻意不改**：Anthropic 语义里响应 model = 请求 model，
// 替换事实由 gateway_model_* 字段显式透出（见 anthropicModelSubstitution）。
func (h *Handler) applyAnthropicDefaultModel(tr *compatChatRequest) *anthropicModelSubstitution {
	final, replaced := substituteAnthropicModel(tr.model, h.cfg.AnthropicDefaultModel, h.cfg.AnthropicDefaultRealm)
	if !replaced {
		return nil
	}
	tr.body = rewriteChatBodyModel(tr.body, final)
	log.Printf("INFO: [anthropic] 模型名未命中网关目录，按 anthropic.default_model 替换: %q -> %q", tr.model, final)
	return &anthropicModelSubstitution{requested: tr.model, effective: final}
}

// ---------------------------------------------------------------------------
// 出站：chat → Anthropic message / SSE
// ---------------------------------------------------------------------------

// anthropicToolUseBlock chat tool_call → Anthropic tool_use 块。
// arguments 必须是合法 JSON 对象；上游截断产生的残缺 JSON 不塞进 input（会让
// 客户端解析崩溃），也不静默丢弃——装进显式标记字段，排障可见。
func anthropicToolUseBlock(tc any) (map[string]any, bool) {
	m, ok := tc.(map[string]any)
	if !ok {
		return nil, false
	}
	fn, _ := m["function"].(map[string]any)
	name, _ := fn["name"].(string)
	if name == "" {
		return nil, false
	}
	id, _ := m["id"].(string)
	if id == "" {
		id = newCompatID("toolu_")
	}
	args, _ := fn["arguments"].(string)
	input := any(map[string]any{})
	if s := strings.TrimSpace(args); s != "" {
		var parsed any
		if json.Unmarshal([]byte(s), &parsed) == nil {
			input = parsed
		} else {
			input = map[string]any{"_gateway_unparsed_arguments": args}
		}
	}
	if input == nil {
		input = map[string]any{}
	}
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}, true
}

// anthropicMessageObject chat 聚合响应 → Anthropic message 对象（非流式）。
// model 回显**客户端请求的模型名**（Anthropic 语义：响应 model 即请求 model），
// 而不是上游裸名——带 cn:/global: 前缀的客户端请求才不会因模型名变化而误判。
// 发生 anthropic.default_model 兜底替换时，替换事实经 sub 标注为 gateway_model_*
// 扩展字段（见 markAnthropicModelSubstitution），model 字段本身仍回显原始请求名。
func anthropicMessageObject(chat map[string]any, reqModel string, sub *anthropicModelSubstitution) map[string]any {
	content := make([]any, 0, 2)
	var msg map[string]any
	if c := chatChoices(chat); c != nil {
		msg, _ = c["message"].(map[string]any)
	}
	if txt := chatMessageContent(msg); txt != "" {
		content = append(content, map[string]any{"type": "text", "text": txt})
	}
	toolUse := 0
	for _, tc := range chatToolCalls(msg) {
		if blk, ok := anthropicToolUseBlock(tc); ok {
			content = append(content, blk)
			toolUse++
		}
	}
	if len(content) == 0 {
		// Anthropic 恒返回至少一个内容块；空完成补空文本块（保持形状，不编造正文）。
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	stop := anthropicStopReason(chatFinishReason(chat))
	if toolUse > 0 {
		stop = "tool_use" // 有 tool_use 块时 stop_reason 必须是 tool_use
	}
	obj := map[string]any{
		"id":            newCompatID("msg_"),
		"type":          "message",
		"role":          "assistant",
		"model":         reqModel,
		"content":       content,
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage":         anthropicUsageOf(chat),
	}
	markAnthropicModelSubstitution(obj, sub)
	return obj
}

// writeAnthropicFromChat 非流式出站：内层聚合响应（或错误体）→ Anthropic 形态。
func writeAnthropicFromChat(em *compatEmitter, cap *compatCapture, reqModel string, sub *anthropicModelSubstitution) {
	if cap.status >= 400 {
		info := readCompatError(cap.status, cap.body)
		em.emitJSON(info.status, anthropicErrorBody(anthropicErrType(info.status), info.message, info.hint, info.code))
		return
	}
	var chat map[string]any
	if json.Unmarshal(cap.body, &chat) != nil {
		em.emitJSON(http.StatusBadGateway, anthropicErrorBody("api_error", "invalid upstream response", "", "upstream_parse"))
		return
	}
	if _, hasErr := chat["error"]; hasErr {
		info := readCompatError(cap.status, cap.body)
		em.emitJSON(info.status, anthropicErrorBody(anthropicErrType(info.status), info.message, info.hint, info.code))
		return
	}
	em.emitJSON(http.StatusOK, anthropicMessageObject(chat, reqModel, sub))
}

// ---------------------------------------------------------------------------
// Handler
// ---------------------------------------------------------------------------

// messages POST /v1/messages（Anthropic Messages API）。
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error(), "")
		return
	}
	tr, cerr := translateAnthropicRequest(raw)
	if cerr != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", cerr.message, "")
		return
	}
	// anthropic.default_model 兜底替换：**仅本端点**。Claude Code 默认发
	// claude-sonnet-4-* 这类原生 Anthropic 模型名，而网关目录里没有这些 id，
	// 不兜底就直接失败、用户被迫手工设 ANTHROPIC_MODEL——这是接入摩擦最大的一处。
	// 未配置（AnthropicDefaultModel 为空）时 sub == nil，行为与改造前逐字一致。
	sub := h.applyAnthropicDefaultModel(tr)
	em := newCompatEmitter(w)
	if tr.stream {
		iw := newAnthropicStreamWriter(em, tr.model)
		iw.sub = sub
		h.runInnerChat(iw, r, tr.body)
		iw.finish()
		return
	}
	cap := newCompatCapture()
	h.runInnerChat(cap, r, tr.body)
	writeAnthropicFromChat(em, cap, tr.model, sub)
}

// messagesCountTokens POST /v1/messages/count_tokens。
//
// 上游不提供 tokenizer 接口，本地也不引入分词库，故这里给出**估算值**（口径见
// estimateChatTokens）。Claude Code 用它做上下文管理，估算足以判断数量级；
// 但它不代表上游计费口径——文档与报告都明确标注为估算。
func (h *Handler) messagesCountTokens(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error(), "")
		return
	}
	tr, cerr := translateAnthropicRequest(raw)
	if cerr != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", cerr.message, "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateChatTokens(tr.body)})
}

// estimateChatTokens 对翻译后的 chat 请求体估算输入 token：
// ASCII 4 字符 ≈ 1 token、非 ASCII（CJK 等）1 字符 ≈ 1 token，每条消息另加
// 结构开销 4。刻意保持简单可解释——这是估算端点，不是计费口径。
func estimateChatTokens(body []byte) int {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return estimateTokensOfText(string(body))
	}
	total := 0
	if msgs, ok := obj["messages"].([]any); ok {
		for _, m := range msgs {
			total += 4
			mm, _ := m.(map[string]any)
			if mm == nil {
				continue
			}
			if raw, err := json.Marshal(mm["content"]); err == nil {
				total += estimateTokensOfText(string(raw))
			}
			if raw, err := json.Marshal(mm["tool_calls"]); err == nil {
				total += estimateTokensOfText(string(raw))
			}
		}
	}
	if raw, err := json.Marshal(obj["tools"]); err == nil && len(raw) > 4 {
		total += estimateTokensOfText(string(raw))
	}
	if total < 1 {
		total = 1
	}
	return total
}

// estimateTokensOfText 文本 token 估算（ASCII/4 + 非 ASCII/1）。
func estimateTokensOfText(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < 0x80 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

// ---------------------------------------------------------------------------
// 出站流式：chat chunk → Anthropic SSE 事件
// ---------------------------------------------------------------------------

// anthropicStreamWriter 承接 chatCompletions 流式分支写出的 OpenAI chat chunk，
// 翻译成 Anthropic 事件序列：
//
//	message_start → content_block_start → content_block_delta* →
//	content_block_stop → message_delta → message_stop
//
// 事件的上游形态：StreamHint 逐帧写 `data: {chunk}\n\n`；chunk 里 delta.content
// 是文本、delta.tool_calls 是工具调用分片（同一 index 的 arguments 拼接）。
// 一律不直接写真实响应——全部经 compatEmitter（头恰好写一次）。
type anthropicStreamWriter struct {
	em    *compatEmitter
	model string
	// sub anthropic.default_model 兜底替换事实（nil = 未替换）。在 message_start
	// 的 message 对象上标注 gateway_model_* 字段，与**非流式**响应同口径。
	sub *anthropicModelSubstitution

	hdr     http.Header
	status  int    // 内层 WriteHeader 声明的状态（0 = 未声明 → 流式成功）
	body    []byte // status>=400 或非 SSE 形态时缓冲的 JSON
	jsonMod bool
	sawAny  bool
	pending []byte

	msgStarted bool
	blocks     []int // 已开启的 content block 序号（升序）
	openBlocks map[int]bool
	nextIndex  int
	textIndex  int

	toolIndex map[int]int // openai tool_call index → anthropic content block index

	stopReason string
	usage      map[string]any
	errored    bool
	sawFrame   bool
	finished   bool
}

func newAnthropicStreamWriter(em *compatEmitter, model string) *anthropicStreamWriter {
	return &anthropicStreamWriter{
		em:         em,
		model:      model,
		hdr:        http.Header{},
		openBlocks: map[int]bool{},
		toolIndex:  map[int]int{},
		textIndex:  -1,
	}
}

// Header 独立 header 视图：内层设置的 header 不落真实响应（真实响应 header 由
// compatEmitter 独占；若这里返回真实 header，StreamHint 设的 text/event-stream 会
// 在 emitter 写头前被 Flush 隐式提交，就是那个 superfluous 双头坑）。
func (w *anthropicStreamWriter) Header() http.Header { return w.hdr }

// WriteHeader 只记录内层状态，不落真实响应。
func (w *anthropicStreamWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}

// Flush 吞掉：真实响应的 flush 由 emitter 每帧负责。
func (w *anthropicStreamWriter) Flush() {}

// Write 接收内层输出：SSE 逐行解析翻译；错误体/JSON 形态缓冲到 finish 统一处理。
// 恒返回 (len(p), nil)：内层 StreamHint 不应因翻译层而中断（客户端写失败由
// emitter.failed 记录，emitter 会在真实写失败后静默丢弃后续事件）。
func (w *anthropicStreamWriter) Write(p []byte) (int, error) {
	if w.status >= 400 {
		w.body = append(w.body, p...)
		return len(p), nil
	}
	if !w.sawAny {
		if t := bytes.TrimLeft(p, " \t\r\n"); len(t) > 0 {
			w.sawAny = true
			if t[0] == '{' || t[0] == '[' {
				// 流式成功路径只写 SSE（"data: "）；JSON 只可能来自防御分支。
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

// handleLine 处理 SSE 行：只认 "data:"（event:/注释/空行一律忽略）。
func (w *anthropicStreamWriter) handleLine(line string) {
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return // 非 JSON 帧无法翻译：丢弃（不把 chat 形状泄漏给 Anthropic 客户端）
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
		w.stopReason = fr
	}
	delta, _ := c["delta"].(map[string]any)
	if delta == nil {
		delta, _ = c["message"].(map[string]any) // 防御：非 delta 的整条 message 形态
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
	if fc, ok := delta["function_call"].(map[string]any); ok && len(fc) > 0 {
		w.emitLegacyFunctionCall(fc)
	}
	// delta.reasoning_content 丢弃：Anthropic thinking 块必须带 signature，无法合成。
}

// ensureStart 发 message_start（只发一次）。input_tokens 置 0 是**协议约束下的
// 诚实值**：上游 usage 只在末帧到达，无法在起点前知；真实 input_tokens 在
// message_delta.usage 里透出（Anthropic SDK 的 usage 合并语义会取到该字段）。
func (w *anthropicStreamWriter) ensureStart() {
	if w.msgStarted {
		return
	}
	w.msgStarted = true
	msg := map[string]any{
		"id":            newCompatID("msg_"),
		"type":          "message",
		"role":          "assistant",
		"model":         w.model,
		"content":       []any{},
		"stop_reason":   nil,
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
	}
	markAnthropicModelSubstitution(msg, w.sub)
	w.em.emitEvent("message_start", map[string]any{
		"type":    "message_start",
		"message": msg,
	})
}

// openBlock 开启一个 content block 并发 content_block_start。
func (w *anthropicStreamWriter) openBlock(kind string, extra map[string]any) int {
	w.ensureStart()
	idx := w.nextIndex
	w.nextIndex++
	w.blocks = append(w.blocks, idx)
	w.openBlocks[idx] = true
	cb := map[string]any{"type": kind}
	if kind == "text" {
		cb["text"] = ""
	}
	for k, v := range extra {
		cb[k] = v
	}
	w.em.emitEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": idx, "content_block": cb,
	})
	return idx
}

// closeBlock 关闭一个尚未关闭的 content block（幂等）。
func (w *anthropicStreamWriter) closeBlock(idx int) {
	if !w.openBlocks[idx] {
		return
	}
	delete(w.openBlocks, idx)
	w.em.emitEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
}

// closeAllBlocks 按开启顺序关闭全部剩余块（Anthropic 的收尾要求）。
func (w *anthropicStreamWriter) closeAllBlocks() {
	for _, idx := range w.blocks {
		w.closeBlock(idx)
	}
}

// closeText 收口当前文本块（首个工具块开始前调用：Anthropic 惯例是文本在前）。
func (w *anthropicStreamWriter) closeText() {
	if w.textIndex >= 0 {
		w.closeBlock(w.textIndex)
		w.textIndex = -1
	}
}

func (w *anthropicStreamWriter) emitText(txt string) {
	if w.textIndex < 0 {
		w.textIndex = w.openBlock("text", nil)
	}
	w.em.emitEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": w.textIndex,
		"delta": map[string]any{"type": "text_delta", "text": txt},
	})
}

// emitToolCalls 把一帧 delta.tool_calls 翻译成 tool_use 块 + input_json_delta。
// OpenAI 的首片带 id/name、后续片只带 arguments 分片（上游 StreamHint 的
// stripToolCallNames 把 name 收敛为「每 index 只出现一次」），故块在首片开启、
// 参数分片原样以 partial_json 转发（Anthropic 的 input_json_delta 语义就是
// 「JSON 片段的流式拼接」）。
func (w *anthropicStreamWriter) emitToolCalls(tcs []any) {
	for _, tci := range tcs {
		tc, ok := tci.(map[string]any)
		if !ok {
			continue
		}
		key := compatIntOf(tc["index"])
		fn, _ := tc["function"].(map[string]any)
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		idx, seen := w.toolIndex[key]
		if !seen {
			w.closeText()
			id, _ := tc["id"].(string)
			if id == "" {
				id = newCompatID("toolu_")
			}
			idx = w.openBlock("tool_use", map[string]any{
				"id": id, "name": name, "input": map[string]any{},
			})
			w.toolIndex[key] = idx
		}
		if args != "" {
			w.emitInputJSON(idx, args)
		}
	}
}

// emitLegacyFunctionCall 支持旧式 delta.function_call（单工具、无 index）。
func (w *anthropicStreamWriter) emitLegacyFunctionCall(fc map[string]any) {
	const key = -1
	name, _ := fc["name"].(string)
	args, _ := fc["arguments"].(string)
	idx, seen := w.toolIndex[key]
	if !seen {
		w.closeText()
		id := newCompatID("toolu_")
		idx = w.openBlock("tool_use", map[string]any{
			"id": id, "name": name, "input": map[string]any{},
		})
		w.toolIndex[key] = idx
	}
	if args != "" {
		w.emitInputJSON(idx, args)
	}
}

func (w *anthropicStreamWriter) emitInputJSON(idx int, partial string) {
	w.em.emitEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": idx,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
	})
}

// emitErrorFrame 上游中途 error 帧 → Anthropic `error` 事件（原文 + gateway_hint
// 一并带出；附带 error 事件的流不再补 message_stop，避免把失败流收尾成"成功完成"）。
func (w *anthropicStreamWriter) emitErrorFrame(e any) {
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
	w.em.emitEvent("error", anthropicErrorBody("api_error", msg, hint, code))
}

// finish 收尾（幂等）：错误 → Anthropic 错误体（状态码保留）；成功 → 关闭所有
// content block + message_delta（stop_reason/usage）+ message_stop。
func (w *anthropicStreamWriter) finish() {
	if w.finished {
		return
	}
	w.finished = true

	// 内层以 >=400 写出 JSON 错误体（或无可用账号 / 上游 400 等），或非 SSE 兜底：
	// 此时尚未写头，能给出正确的 HTTP 状态码 + Anthropic 错误体。
	if w.status >= 400 || w.jsonMod {
		status := w.status
		if status < 400 {
			status = http.StatusBadGateway
		}
		info := readCompatError(status, w.body)
		w.em.emitJSON(info.status, anthropicErrorBody(anthropicErrType(info.status), info.message, info.hint, info.code))
		return
	}
	if w.errored {
		return // 已发 error 事件（含上游空流兜底帧）
	}
	if !w.msgStarted && !w.sawFrame {
		w.em.emitEvent("error", anthropicErrorBody("api_error",
			"upstream returned no content", "", "upstream_parse"))
		return
	}
	w.ensureStart()
	if len(w.blocks) == 0 {
		w.openBlock("text", nil) // 空完成：补一个空文本块保持 Anthropic 形状
	}
	w.closeAllBlocks()
	stop := w.stopReason
	if len(w.toolIndex) > 0 {
		stop = "tool_calls" // 有 tool_use 块 → stop_reason 必须是 tool_use
	}
	w.em.emitEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": anthropicStopReason(stop), "stop_sequence": nil},
		"usage": anthropicUsage(w.usage),
	})
	w.em.emitEvent("message_stop", map[string]any{"type": "message_stop"})
}
