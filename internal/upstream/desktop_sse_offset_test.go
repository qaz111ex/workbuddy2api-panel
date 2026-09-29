// desktop_sse_offset_test.go 钉住 DesktopChatWithExpert 抓服务端 requestId 时的
// 搜索偏移推进（吸收上游 5f6c7ca）。
//
// 真实缺陷：SSE 里 `"id":"` 会先出现在每一帧的消息 id（chatcmpl-*）上，而
// requestId（cmb-* / 裸 32hex）在后面。原实现每轮都从 buf[0] 重新 bytes.Index，
// 首个不匹配的 id 会被无限重复命中——读到 1MB 上限（或流结束）后误报「未找到
// 服务端 requestId」，该账号的 expert_actual_use 等 JOIN 事件全部拿不到真实 id。
package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// desktopSSEClient 造一个固定返回 body 的 Client（path 不校验，本函数只关心流解析）。
func desktopSSEClient(body string) *Client {
	return testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
}

// 首个 `"id":"` 是消息 id（不匹配 idRegex），真正的 requestId 在后续帧：
// 必须跳过前者、拿到后者（原实现会在此死循环到流末尾后报错）。
func TestDesktopChatWithExpertAdvancesSSEOffset(t *testing.T) {
	const realID = "cmb-0123456789abcdef0123456789abcdef"
	sse := "data: {\"id\":\"chatcmpl-first\",\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n" +
		"data: {\"id\":\"" + realID + "\",\"choices\":[{\"delta\":{}}]}\n\n" +
		"data: [DONE]\n\n"

	_, gotID, err := desktopSSEClient(sse).DesktopChatWithExpert(&auth.Auth{AccessToken: "at", UID: "u1"}, "")
	if err != nil {
		t.Fatalf("应拿到第二个 id 作为 requestId，err=%v", err)
	}
	if gotID != realID {
		t.Fatalf("requestId=%q want %q", gotID, realID)
	}
}

// 首个不匹配 id 之后还有大段无关内容（< 1MB 上限）：
// 原实现会一直重复命中同一位置、把整段读完再报错。
func TestDesktopChatWithExpertFindsRequestIdAfterLargePayload(t *testing.T) {
	const realID = "0123456789abcdef0123456789abcdef" // 裸 32hex 形态
	pad := strings.Repeat("x", 256*1024)              // 256KB：在 1MB 上限内，但足以暴露死读
	sse := "data: {\"id\":\"chatcmpl-pad\"}\n\n" + pad + "\n\n" +
		"data: {\"id\":\"" + realID + "\"}\n\n" +
		"data: [DONE]\n\n"

	_, gotID, err := desktopSSEClient(sse).DesktopChatWithExpert(&auth.Auth{AccessToken: "at", UID: "u1"}, "")
	if err != nil {
		t.Fatalf("大 payload 后仍应找到 requestId，err=%v", err)
	}
	if gotID != realID {
		t.Fatalf("requestId=%q want %q", gotID, realID)
	}
}

// `"id":""` 空 id 同样永不匹配 idRegex：也必须被跳过
// （上游只处理了「非空但不匹配」；本 fork 顺手把空 id 也堵住）。
func TestDesktopChatWithExpertSkipsEmptyID(t *testing.T) {
	const realID = "cmb-ffffffffffffffffffffffffffffffff"
	sse := "data: {\"id\":\"\",\"x\":1}\n\n" +
		"data: {\"id\":\"" + realID + "\"}\n\n"

	_, gotID, err := desktopSSEClient(sse).DesktopChatWithExpert(&auth.Auth{AccessToken: "at", UID: "u1"}, "")
	if err != nil {
		t.Fatalf("空 id 应被跳过，err=%v", err)
	}
	if gotID != realID {
		t.Fatalf("requestId=%q want %q", gotID, realID)
	}
}

// 值尚未读完（跨 chunk 截断）时必须**原地等待补齐**，不能把半截 id 当不匹配跳过，
// 也不能把 `"id":"` 之后的内容误当 id。这里用 8KB 边界把 id 切成两半。
func TestDesktopChatWithExpertWaitsForSplitID(t *testing.T) {
	const realID = "cmb-00112233445566778899aabbccddeeff"
	// 把 `"id":"` 与 id 值分到不同 read：前缀长度刻意跨过 8192 字节边界。
	prefix := "data: {\"pad\":\"" + strings.Repeat("y", 8192) + "\"}\n\n"
	sse := prefix + "data: {\"id\":\"" + realID + "\"}\n\n"

	_, gotID, err := desktopSSEClient(sse).DesktopChatWithExpert(&auth.Auth{AccessToken: "at", UID: "u1"}, "")
	if err != nil {
		t.Fatalf("跨 chunk 的 id 应被正确拼回，err=%v", err)
	}
	if gotID != realID {
		t.Fatalf("requestId=%q want %q", gotID, realID)
	}
}

// 全流确实没有 requestId 形状的 id 时仍必须报错（不臆造、不返回空串当成功）。
func TestDesktopChatWithExpertNoRequestIDStillErrors(t *testing.T) {
	sse := "data: {\"id\":\"chatcmpl-only\",\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n" +
		"data: [DONE]\n\n"

	_, gotID, err := desktopSSEClient(sse).DesktopChatWithExpert(&auth.Auth{AccessToken: "at", UID: "u1"}, "")
	if err == nil {
		t.Fatalf("无 requestId 形态时应报错，got id=%q", gotID)
	}
	if gotID != "" {
		t.Fatalf("报错时不应返回 id，got %q", gotID)
	}
}
