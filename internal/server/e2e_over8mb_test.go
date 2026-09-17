package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestE2EOver8MBBodyReachesUpstream 端到端：超过旧默认上限（8MB）的请求体在缺省配置
// （max_body_mb=0 = 不限）下必须完整到达上游。
//
// 请求体按真实形态构造：多轮对话 + 一条内联 base64 图片。这是 agent 客户端
// （Claude Code / Codex / opencode 等）的常见形态——每轮都会把**历史全部图片**
// 重新以 base64 塞进请求体，编码膨胀约 4/3。
//
// 为什么这是缺陷而非"保护"：上游的真实限制在 **token**（超限返回 11115），而字节与
// token 并不对应。一个上游完全接受的请求（纯文本 475k token 会话约 3.6MB，叠加截图
// 后轻易破 8MB）会被网关的字节上限先掐死，客户端看到的是网关自造的 413，而看不到
// 上游本来会给出的答复。
func TestE2EOver8MBBodyReachesUpstream(t *testing.T) {
	// 构造 9MB+ 的请求体：一条真实用户消息 + 一张内联图片（base64 形态 9MB）。
	// base64 内容用固定字符填充即可——本测试只关心字节量能否穿过网关，不关心图片语义。
	blob := strings.Repeat("A", 9<<20)
	body := map[string]any{
		"model":  "deepseek-v4.1-flash",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "看看这张图"},
				map[string]any{"type": "image_url", "image_url": map[string]any{
					"url": "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg" + blob,
				}},
			}},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 8<<20 {
		t.Fatalf("fixture must exceed the old 8MB default, got %d bytes", len(raw))
	}
	t.Logf("request body: %d bytes (%.2f MB)", len(raw), float64(len(raw))/1024/1024)

	var gotLen int
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			gotLen = len(b)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://cn.example",
	}

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 关键：不注入 MaxBodyBytes → 缺省 0 = 不限。
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(raw)))

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (oversized body must pass with default unlimited)", rec.Code, truncateStr(rec.Body.String(), 300))
	}
	if gotLen == 0 {
		t.Fatal("upstream never received the body")
	}
	t.Logf("upstream received: %d bytes (%.2f MB)", gotLen, float64(gotLen)/1024/1024)
	// 出站经改写（thinking 注入 / prompt_cache_key 等）字节数会略增，故只要求不小于入站
	// ——被截断的 body 必然远小于入站，这条断言足以捕获。
	if gotLen < len(raw) {
		t.Errorf("upstream body truncated: got %d want >= %d", gotLen, len(raw))
	}
}

// TestE2EOver8MBBodyRejectedWhenLimitSet 反向锚定：显式设上限时，超限请求仍按
// 旧语义 413（且不打上游、不罚账号）——证明"默认不限"没有把保护开关本身改坏。
func TestE2EOver8MBBodyRejectedWhenLimitSet(t *testing.T) {
	blob := strings.Repeat("A", 2<<20)
	body := map[string]any{
		"model": "deepseek-v4.1-flash",
		"messages": []any{
			map[string]any{"role": "user", "content": "x" + blob},
		},
	}
	raw, _ := json.Marshal(body)
	if len(raw) <= 1<<20 {
		t.Fatalf("fixture too small: %d", len(raw))
	}

	var calls int
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://cn.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 1 << 20}) // 显式 1MB 上限

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(raw)))

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code=%d want 413 when explicit limit set", rec.Code)
	}
	if calls != 0 {
		t.Errorf("upstream must not be called on 413, got %d", calls)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Errorf("413 must not penalize account: %+v", st)
	}
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
