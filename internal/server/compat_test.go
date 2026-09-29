package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// compatUpstream 假上游：记录每次请求的**已翻译 chat 请求体**（出站形态，含
// payload.go 的归一化），供断言入站翻译是否正确落到 chat 形态。
type compatUpstream struct {
	mu     sync.Mutex
	bodies []map[string]any
	auths  []string
}

func (c *compatUpstream) last(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		t.Fatal("upstream 未被调用")
	}
	return c.bodies[len(c.bodies)-1]
}

func (c *compatUpstream) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

// newCompatUpstream 构建记录 body 的假上游。fn 依据 Authorization 与已解析 body
// 决定响应。
func newCompatUpstream(t *testing.T, fn func(auth string, body map[string]any) (int, string, bool)) (*upstream.Client, *compatUpstream) {
	t.Helper()
	cu := &compatUpstream{}
	c := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			var obj map[string]any
			_ = json.Unmarshal(raw, &obj)
			authz := r.Header.Get("Authorization")
			cu.mu.Lock()
			cu.auths = append(cu.auths, authz)
			cu.bodies = append(cu.bodies, obj)
			cu.mu.Unlock()
			status, body, isStream := fn(authz, obj)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	return c, cu
}

// headerCountingWriter 统计真实 ResponseWriter 的 WriteHeader 调用次数，并复刻
// net/http 的提交语义：Write/Flush 在未提交时**隐式** WriteHeader(200)；提交后的
// 重复 WriteHeader 记为 superfluous（= 线上 net/http 的
// "superfluous response.WriteHeader call" 噪音来源，即 misakano7545 435cb8ec 的坑）。
type headerCountingWriter struct {
	hdr         http.Header
	body        bytes.Buffer
	status      int
	committed   bool
	superfluous int
	flushes     int
}

func newHeaderCountingWriter() *headerCountingWriter {
	return &headerCountingWriter{hdr: http.Header{}}
}

func (c *headerCountingWriter) Header() http.Header { return c.hdr }

func (c *headerCountingWriter) WriteHeader(code int) {
	if c.committed {
		c.superfluous++
		return
	}
	c.committed, c.status = true, code
}

func (c *headerCountingWriter) Write(p []byte) (int, error) {
	if !c.committed {
		c.WriteHeader(http.StatusOK)
	}
	return c.body.Write(p)
}

func (c *headerCountingWriter) Flush() {
	c.flushes++
	if !c.committed {
		c.WriteHeader(http.StatusOK)
	}
}

// compatEvent 一帧目标协议 SSE 事件。
type compatEvent struct {
	name string
	data map[string]any
}

// parseCompatSSE 解析 `event: X\ndata: {...}\n\n` 形态（Anthropic / Responses 共用）。
func parseCompatSSE(t *testing.T, raw string) []compatEvent {
	t.Helper()
	out := []compatEvent{}
	for _, frame := range strings.Split(raw, "\n\n") {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		name, data := "", ""
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" {
			t.Fatalf("SSE 帧缺 event: 行: %q", frame)
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			t.Fatalf("SSE data 不是合法 JSON（event=%s）: %q", name, data)
		}
		out = append(out, compatEvent{name: name, data: obj})
	}
	return out
}

func eventNames(evs []compatEvent) []string {
	names := make([]string, 0, len(evs))
	for _, e := range evs {
		names = append(names, e.name)
	}
	return names
}

// matchEventPattern 事件名序列匹配 pattern：`*` 匹配 0+ 个任意事件（贪婪跳到下一个字面量）。
func matchEventPattern(got, pattern []string) bool {
	gi, pi := 0, 0
	for pi < len(pattern) {
		if pattern[pi] == "*" {
			if pi == len(pattern)-1 {
				return true
			}
			next := pattern[pi+1]
			for gi < len(got) && got[gi] != next {
				gi++
			}
			if gi >= len(got) {
				return false
			}
			pi++
			continue
		}
		if gi >= len(got) || got[gi] != pattern[pi] {
			return false
		}
		gi++
		pi++
	}
	return gi == len(got)
}

// requireEventPattern 断言事件序列匹配 pattern，失败时打印实际序列。
func requireEventPattern(t *testing.T, evs []compatEvent, pattern ...string) {
	t.Helper()
	got := eventNames(evs)
	if !matchEventPattern(got, pattern) {
		t.Fatalf("事件序列不匹配\n got=%v\nwant=%v", got, pattern)
	}
}

// findEvent 取首个指定名的事件（不存在返回 nil）。
func findEvent(evs []compatEvent, name string) *compatEvent {
	for i := range evs {
		if evs[i].name == name {
			return &evs[i]
		}
	}
	return nil
}

// countEvents 统计指定名事件出现次数。
func countEvents(evs []compatEvent, name string) int {
	n := 0
	for _, e := range evs {
		if e.name == name {
			n++
		}
	}
	return n
}

// anthropicStreamBody 一段可用的 chat SSE：role 首帧 → 两段正文 → finish+usage → DONE。
const anthropicStreamBody = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"世界\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\n" +
	"data: [DONE]\n\n"

// anthropicToolStreamBody 工具调用流：首帧带 id/name，后续帧只带 arguments 分片。
const anthropicToolStreamBody = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Bash\",\"arguments\":\"\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"cmd\\\":\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"ls\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":9,\"total_tokens\":16}}\n\n" +
	"data: [DONE]\n\n"

// postCompat 发一个兼容层请求并返回 recorder。
func postCompat(t *testing.T, h *Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return rec
}

// ---------------------------------------------------------------------------
// 不泄漏内部形状 + realm 前缀仍走既有路由
// ---------------------------------------------------------------------------

// TestMessagesStreamNoChatShapeLeak Anthropic 客户端只能看到 Anthropic 事件；
// 上游 chat chunk 的字段名（choices/delta/chat.completion.chunk/[DONE]）一律不得泄漏。
func TestMessagesStreamNoChatShapeLeak(t *testing.T) {
	up, _ := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		return 200, anthropicStreamBody, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/messages", `{"model":"m","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"q"}]}`)
	out := rec.Body.String()
	for _, leak := range []string{"chat.completion", "choices", "\"delta\":{\"role\"", "[DONE]"} {
		if strings.Contains(out, leak) {
			t.Errorf("SSE 输出泄漏内部 chat 形状 %q: %s", leak, out)
		}
	}
}

// TestMessagesRealmPrefixStillRouted 模型名的 cn:/global: 前缀是网关路由协议：
// 兼容层必须原样交给既有 chat 链路，由既有 resolveModel/rewriteModel 剥前缀出站，
// 且响应仍回显客户端原始名。
func TestMessagesRealmPrefixStillRouted(t *testing.T) {
	up, cu := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		if b["model"] != "glm-5.2" {
			t.Errorf("出站 model 必须被既有 rewriteModel 剥成裸名: %v", b["model"])
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/messages", `{"model":"cn:glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"q"}]}`)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["model"] != "cn:glm-5.2" {
		t.Errorf("响应应回显客户端原始模型名: %v", resp["model"])
	}
	if cu.last(t)["model"] != "glm-5.2" {
		t.Errorf("出站 model=%v", cu.last(t)["model"])
	}
}

// TestResponsesCountTokensAuthAndErrorShape Responses 校验失败必须是 OpenAI 形状
// （与 /v1/messages 的 Anthropic 形状区分开，否则客户端解析器对不上）。
func TestResponsesValidationUsesOpenAIShape(t *testing.T) {
	up, cu := newCompatUpstream(t, func(auth string, b map[string]any) (int, string, bool) {
		t.Fatal("校验失败不应打上游")
		return 500, "", false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := postCompat(t, h, "/v1/responses", `{"input":"q"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	e, ok := resp["error"].(map[string]any)
	if !ok || e["code"] == nil {
		t.Fatalf("OpenAI 形状错误体应有 error.code: %s", rec.Body)
	}
	if cu.count() != 0 {
		t.Errorf("上游调用数=%d want 0", cu.count())
	}
}

// brokenPipeWriter 头提交后 body 写入立即失败（模拟客户端中途断开）。
type brokenPipeWriter struct {
	hdr   http.Header
	heads int
}

func (b *brokenPipeWriter) Header() http.Header       { return b.hdr }
func (b *brokenPipeWriter) WriteHeader(int)           { b.heads++ }
func (b *brokenPipeWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (b *brokenPipeWriter) Flush()                    {}

// TestCompatClientDisconnectStillOneHeader 客户端断开（body 写失败）时也不得再写头：
// emitter 记录 failed 后丢弃后续事件，且绝不重试 WriteHeader。
func TestCompatClientDisconnectStillOneHeader(t *testing.T) {
	bp := &brokenPipeWriter{hdr: http.Header{}}
	em := newCompatEmitter(bp)
	w := newAnthropicStreamWriter(em, "m")
	if _, err := w.Write([]byte(anthropicStreamBody)); err != nil {
		t.Fatal(err) // 翻译层对内层恒返回 nil；写失败由 emitter 吸收
	}
	w.finish()
	if bp.heads != 1 {
		t.Fatalf("WriteHeader 次数=%d want 1（断开后不得重试写头）", bp.heads)
	}

	bp2 := &brokenPipeWriter{hdr: http.Header{}}
	em2 := newCompatEmitter(bp2)
	em2.emitEvent("message_start", map[string]any{"type": "message_start"})
	em2.emitEvent("message_stop", map[string]any{"type": "message_stop"})
	em2.emitJSON(http.StatusOK, map[string]any{"x": 1})
	if bp2.heads != 1 {
		t.Fatalf("写失败后后续输出必须被丢弃: WriteHeader 次数=%d want 1", bp2.heads)
	}
}
