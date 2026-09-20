package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func globalChatAuth() *auth.Auth {
	return &auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
}

func cnChatAuth() *auth.Auth {
	return &auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999}
}

// TestChatPathsGlobalV2First 锁定 #119：global 主路必须是 /v2/chat/completions。
//
// 上游实测 /console 挂腾讯云 WAF body 内容规则（反引号 printf/whoami 等命令执行特征
// 确定性 403），global 号会随机 403；/v2 同 base 不挂该规则。本 fork 有意与上游
// 4ac68b7 的「整体退役 fallback 链」不同：保留 /console 作为 404/405 的次选，
// 但**主路必须是 /v2**——这条断言就是该分歧的守卫。
func TestChatPathsGlobalV2First(t *testing.T) {
	c := &Client{ChatBaseCN: "https://cn.example", ChatBaseGlobal: "https://global.example", GlobalEnabled: true}

	got := c.chatPaths(globalChatAuth())
	if len(got) == 0 || got[0] != chatCompletionsPath {
		t.Fatalf("global chatPaths=%v want first=%s (v2 must be the primary path, #119)", got, chatCompletionsPath)
	}
	// 次选保留 /console：上游未来下线 /v2 时 404/405 仍可回落（本 fork 有意保留）。
	if len(got) < 2 || got[1] != globalChatConsolePath {
		t.Fatalf("global chatPaths=%v want fallback=%s (fork keeps 404/405 fallback)", got, globalChatConsolePath)
	}

	// cn 恒单路径 /v2（现状逐字，零回归）。
	if got := c.chatPaths(cnChatAuth()); len(got) != 1 || got[0] != chatCompletionsPath {
		t.Fatalf("cn chatPaths=%v want [%s]", got, chatCompletionsPath)
	}
}

// TestChatPathsGlobalDisabledFallsBackToCNPaths 逃生门（global.enabled=false）：
// global 账号按 cn 口径走单路径 /v2（auth.Realm() 同口径回落）。
func TestChatPathsGlobalDisabledFallsBackToCNPaths(t *testing.T) {
	c := &Client{ChatBaseCN: "https://cn.example", ChatBaseGlobal: "https://global.example", GlobalEnabled: false}
	if got := c.chatPaths(globalChatAuth()); len(got) != 1 || got[0] != chatCompletionsPath {
		t.Fatalf("global-disabled chatPaths=%v want single [%s]", got, chatCompletionsPath)
	}
}

// TestChatFallbackHTTPStatus 回落只在 404/405 发生；403 是 WAF 内容规则判定，
// 换路径重试只会重复触发罚号链路，不得回落。
func TestChatFallbackHTTPStatus(t *testing.T) {
	for _, status := range []int{404, 405} {
		if !chatFallbackHTTPStatus(status) {
			t.Errorf("status %d should allow fallback", status)
		}
	}
	for _, status := range []int{403, 429, 500, 400, 200} {
		if chatFallbackHTTPStatus(status) {
			t.Errorf("status %d must NOT allow fallback", status)
		}
	}
}

// TestChatStreamContextGlobalUsesV2Endpoint 端到端：global 账号的出站请求必须打在
// /v2/chat/completions（主路），且不得先打 /console（WAF 规则面）。
func TestChatStreamContextGlobalUsesV2Endpoint(t *testing.T) {
	var paths []string
	c := &Client{
		ChatBaseCN:     "https://cn.example",
		ChatBaseGlobal: "https://global.example",
		GlobalEnabled:  true,
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.URL.Path)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		})},
	}
	rc, status, _, err := c.ChatStreamContext(context.Background(), globalChatAuth(),
		[]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		"", ChatMeta{})
	if err != nil {
		t.Fatalf("ChatStreamContext: %v", err)
	}
	if rc != nil {
		rc.Close()
	}
	if status != 200 {
		t.Fatalf("status=%d want 200", status)
	}
	if len(paths) != 1 || paths[0] != chatCompletionsPath {
		t.Fatalf("upstream paths=%v want exactly [%s] (global must not hit /console first)", paths, chatCompletionsPath)
	}
}

// TestChatStreamContextGlobalFallsBackToConsoleOn404 上游下线 /v2 时（404）仍能
// 回落 /console 继续服务——本 fork 保留 fallback 的收益点。
func TestChatStreamContextGlobalFallsBackToConsoleOn404(t *testing.T) {
	var paths []string
	c := &Client{
		ChatBaseCN:     "https://cn.example",
		ChatBaseGlobal: "https://global.example",
		GlobalEnabled:  true,
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.URL.Path)
			if r.URL.Path == chatCompletionsPath {
				return &http.Response{
					StatusCode: 404,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":1,"msg":"not found"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		})},
	}
	rc, status, _, err := c.ChatStreamContext(context.Background(), globalChatAuth(),
		[]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`),
		"", ChatMeta{})
	if err != nil {
		t.Fatalf("ChatStreamContext: %v", err)
	}
	if rc != nil {
		rc.Close()
	}
	if status != 200 {
		t.Fatalf("status=%d want 200 after /v2 404 → /console fallback", status)
	}
	want := []string{chatCompletionsPath, globalChatConsolePath}
	if len(paths) != 2 || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("upstream paths=%v want %v", paths, want)
	}
}
