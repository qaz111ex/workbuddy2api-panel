package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{402, ``, ErrHardCredit},
		{400, `{"code":1,"msg":"余额不足"}`, ErrHardCredit},
		{403, `insufficient credits`, ErrHardCredit},
		{200, `{"code":10001,"msg":"积分不足，请充值"}`, ErrHardCredit},
		{400, `{"code":1,"msg":"额度用尽"}`, ErrHardCredit},
		{429, ``, ErrSoftRate},
		// 限流文案（issue #28）：状态码不是 429 时也必须识别为软限流，
		// 否则账号不会被冷却，下次请求仍会被选中。
		{200, `{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, ErrSoftRate},
		{400, `rate limit`, ErrSoftRate},
		{403, `usage limit reached`, ErrSoftRate},
		// "model usage limit exceeded" 不是余额语义（无 credit/quota/积分/额度 等计费词），
		// 属于模型侧用量节流 → 短冷却（误判为硬冷却会把有余量的号停到次日 04:00）。
		{200, `{"code":1,"msg":"model usage limit exceeded"}`, ErrSoftRate},
		{200, `{"code":1,"msg":"too many requests"}`, ErrSoftRate},
		{500, `rate-limited upstream`, ErrSoftRate}, // 限流文案优先于 5xx 分类
		// 内容策略拦截（HTTP 400 + 审核文案）：误报信号，不罚账号，走降级重试。
		{400, `Illegal API invocation from an unapproved channel`, ErrContentBlocked},
		{400, `{"code":11128,"msg":"blocked by security policy"}`, ErrContentBlocked},
		{400, `unapproved channel`, ErrContentBlocked},
		// 通用 4xx（非审核文案）：仍判 ErrClient，只换号不罚。
		{400, `bad request`, ErrClient},
		// ErrBadParams：请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）。
		// 这是"发给上游的 body 有问题"（网关截断已由 413 消灭，剩余为客户端畸形 JSON），
		// 换了账号也一样 400，不罚号。具体词优先于通用 4xx。
		{400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, ErrBadParams},
		{400, `Unmarshal chat params failed`, ErrBadParams},
		{400, `{"code":11101,"msg":"x"}`, ErrBadParams},
		// 图片格式/数据错误（400 + 11135 图片错误族）是确定性请求级错误：分类后
		// 不轮转、不罚号，handler 直接透传上游原文。code 判定走 codeMarker，
		// 天然容忍 JSON 空白（`"code": 11135` / `"code": "11135"`）。
		{400, `{"code":11135,"msg":"invalid_image_data"}`, ErrImageInvalid},
		// 图片族判在 ErrBadParams 之前：同为 11101 信封，图片形态优先归 image_invalid。
		{400, `{"code":11101,"msg":"invalid_image_data"}`, ErrImageInvalid},
		{400, `invalid_image_data`, ErrImageInvalid},
		{400, `{"code":11101,"msg":"please replace the image and retry"}`, ErrImageInvalid},
		{400, `{"code": 11135, "msg": "image rejected"}`, ErrImageInvalid},
		{400, `{"code": "11135", "msg": "image rejected"}`, ErrImageInvalid},
		{400, `{"error": {"code": 11135, "message": "image rejected"}}`, ErrImageInvalid},
		// `invalid image_url content` 是上游最常见的图片报错文案，常与 code 11101
		// 同行（sk c5cdb46 的 invalidImageRule 把它列在首位）。它属**分类**口径的
		// 图片族（isImageInvalidBody），但不属 hint 口径的 11135 族
		// （isInvalidImageData）——故 gateway_hint 走 Kind 表的 ErrImageInvalid 文案。
		{400, `{"code":11101,"msg":"Parse message failed: invalid image_url content"}`, ErrImageInvalid},
		{400, `Parse message failed: invalid image_url content`, ErrImageInvalid},
		{400, `{"code": 11133, "msg": "other business error"}`, ErrClient},
		// 429 + code 14018 = 明确的账号积分耗尽（issue #175）：必须先于通用 429
		// 兜底，否则会被误判为可自愈的软限流并反复兜底选中。仅按结构化 code 判定。
		{429, `{"code":14018,"msg":"Credits exhausted"}`, ErrHardCredit},
		{429, `{"code": 14018, "msg":"Credits exhausted"}`, ErrHardCredit},
		{429, `{"error":{"data":{"code":"14018","msg":"Credits exhausted"}}}`, ErrHardCredit},
		// 防过宽反例：无 14018 业务码的 429 仍是软限流（文案不得参与判定）。
		{429, `{"requestId":"14018","msg":"Credits exhausted"}`, ErrSoftRate},
		{429, `{"code":1,"msg":"Credits exhausted"}`, ErrSoftRate},
		{200, `quota exceeded`, ErrHardCredit},
		// session 死亡优先于限流文案（401+12153 需人工重登，短冷却无意义）。
		{401, `{"code":12153,"msg":"Offline user session not found, rate limit"}`, ErrSessionDead},
		{401, `Offline user session not found`, ErrSessionDead},
		{401, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		{401, `{"code":9999,"msg":"bad token"}`, ErrClient},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{200, ``, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestErrKindString 错误类别字符串是日志/面板/客户端 code 的稳定契约：
// 新增类别必须给出稳定标识（image_invalid），不得回落 default 的 "none"。
func TestErrKindString(t *testing.T) {
	cases := []struct {
		kind ErrKind
		want string
	}{
		{ErrImageInvalid, "image_invalid"},
		{ErrPromptTooLong, "prompt_too_long"},
		{ErrHardCredit, "hard_credit"},
		{ErrSoftRate, "soft_rate"},
		{ErrClient, "client"},
	}
	for _, c := range cases {
		if got := c.kind.String(); got != c.want {
			t.Errorf("ErrKind(%d).String()=%q want %q", c.kind, got, c.want)
		}
	}
}

// TestGatewayHintImageInvalid 图片无效的 gateway_hint：Kind 表覆盖该类别；
// 带 11135 业务码的 body 由既有 11135 形态判定优先给出更具体的图片提示
// （hint.go 的判定次序：业务码形态先于 Kind 表），两者都必须非空且指向图片。
func TestGatewayHintImageInvalid(t *testing.T) {
	plain := GatewayHint(ErrImageInvalid, `{"code":1,"msg":"Parse message failed: invalid image_url content"}`, HintContext{})
	if want := "image request was rejected by upstream; check image_url format and image data"; plain != want {
		t.Errorf("GatewayHint(ErrImageInvalid, plain)=%q want %q", plain, want)
	}
	code := GatewayHint(ErrImageInvalid, `{"code": 11135, "msg":"image rejected"}`, HintContext{})
	if code == "" {
		t.Fatal("GatewayHint(ErrImageInvalid, 11135 body) empty; want non-empty image hint")
	}
	if !strings.Contains(code, "image") {
		t.Errorf("GatewayHint(ErrImageInvalid, 11135 body)=%q want image-related hint", code)
	}
}

// TestIsModelRateLimit 判断 429 body 是否明确指向模型级限流（code 6004）。
func TestIsModelRateLimit(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		// 6004：模型级限流（issue #31 的核心场景）。
		{`{"code":6004,"msg":"将在 2026-09-11 18:33:27 UTC+8 重置"}`, true},
		{`{"code": 6004,"msg":"x"}`, true},
		// 其他 code（非模型级限流）→ 不算。
		{`{"code":11140,"msg":"The model provider is rate-limiting requests."}`, false},
		{`{"code":1,"msg":"429 rate limit"}`, false},
	}
	for _, c := range cases {
		if got := IsModelRateLimit(c.body); got != c.want {
			t.Errorf("IsModelRateLimit(%q)=%v want %v", c.body, got, c.want)
		}
	}
}

// TestParseSoftRateReset 解析上游 429 6004 msg 里的「将在 … 重置」时间（## UTC+8）。
func TestParseSoftRateReset(t *testing.T) {
	future := time.Now().Add(35 * time.Minute)
	ts := future.In(softRateResetLoc).Format("2006-01-02 15:04:05")
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"6004 带时间+UTC+8 后缀", `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, true},
		{"6004 带时间无后缀", `{"code":6004,"msg":"将在 ` + ts + ` 重置"}`, true},
		{"6004 无时间文案", `{"code":6004,"msg":"model usage limit exceeded"}`, false},
		{"非 6004 但带时间（ParseRateReset 统一解析；模型级豁免由调用侧按 6004 判定）", `{"code":11140,"msg":"将在 ` + ts + ` UTC+8 重置"}`, true},
		{"非法时间格式", `{"code":6004,"msg":"将在 明天 重置"}`, false},
		{"空 body", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseRateReset(c.body)
			if ok != c.ok {
				t.Fatalf("ok=%v want %v (body=%s)", ok, c.ok, c.body)
			}
			if ok {
				// 解析结果 = ts 在 UTC+8 解释下的墙钟（截断到分钟），应与 future 相差 ±2 分钟。
				if d := got.Sub(future); d < -2*time.Minute || d > 2*time.Minute {
					t.Errorf("parsed=%v want ~%v (diff %v)", got, future, d)
				}
				if got.Location() != time.UTC {
					// 不同指针的 FixedZone 实例相等性按 offset 判，这里只断言 offset。
					if _, off := got.Zone(); off != 8*60*60 {
						t.Errorf("zone offset=%d want +08:00", off)
					}
				}
			}
		})
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP:          &http.Client{Transport: fn},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
}

func TestRefreshSuccess(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("X-Refresh-Token") != "oldrt" {
			return nil, errors.New("missing X-Refresh-Token")
		}
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
	if a.ExpiresAt <= 1 {
		t.Errorf("expiresAt not advanced: %d", a.ExpiresAt)
	}
}

func TestRefreshPreservesExpiryWhenOmitted(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != 1753600000 {
		t.Errorf("expiresAt should be preserved, got %d", a.ExpiresAt)
	}
	if a.RefreshToken != "rt" {
		t.Errorf("refreshToken should be preserved, got %s", a.RefreshToken)
	}
}

func TestRefreshSessionDead(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 401,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":12153,"msg":"Offline user session not found"}`)),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("want error")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if ue.Kind != ErrSessionDead {
		t.Errorf("kind=%v want ErrSessionDead", ue.Kind)
	}
}

func TestChatStreamSendsHeadersAndStreamTrue(t *testing.T) {
	var gotAuth, gotUID, gotProduct string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		gotProduct = r.Header.Get("X-Product")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1"}
	rc, status, respBody, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}
	rc.Close()
	if gotAuth != "Bearer at" || gotUID != "u1" || gotProduct != "WorkBuddy" {
		t.Errorf("headers: auth=%q uid=%q product=%q", gotAuth, gotUID, gotProduct)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":true`)) {
		t.Errorf("stream not forced: %s", gotBody)
	}
}

func TestFetchModelsEffortsDriveBodyDowngrade(t *testing.T) {
	var outbound []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":["low","high"]}}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[]}}`), nil
		default:
			outbound, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	// ModelInfo.Efforts 应携带 supportedEfforts，DefaultEffort 应携带 reasoning.effort
	if len(infos[0].Efforts) != 2 || infos[0].Efforts[0] != "low" {
		t.Errorf("infos[0].Efforts=%v", infos[0].Efforts)
	}
	if infos[0].DefaultEffort != "high" {
		t.Errorf("infos[0].DefaultEffort=%q want high", infos[0].DefaultEffort)
	}
	// glm-5.2 只支持 low/high，请求 max → 降级为 high
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","reasoning_effort":"max","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	var m map[string]any
	if err := json.Unmarshal(outbound, &m); err != nil {
		t.Fatalf("outbound unmarshal: %v (%s)", err, outbound)
	}
	if got, _ := m["reasoning_effort"].(string); got != "high" {
		t.Errorf("reasoning_effort=%v want high (outbound=%s)", m["reasoning_effort"], outbound)
	}
}

func TestChatStreamHardCreditError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(402, `{"code":1,"msg":"余额不足"}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if status != 402 {
		t.Errorf("status=%d", status)
	}
	// 错误信封一次成型：≥400 返回已分类的 *Error（Kind + body 全量仍经 respBody 透出）
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrHardCredit {
		t.Fatalf("hard credit should return classified *Error envelope, got %v", err)
	}
	if len(respBody) == 0 {
		t.Errorf("body should still be returned for passthrough")
	}
}

// TestChatStreamReadsMultipleChunksOverRealTransport 走真实 net/http 传输层，
// 回归 defer cancel() 导致第二块起 body Read 返回 context canceled 的断流 bug。
func TestChatStreamReadsMultipleChunksOverRealTransport(t *testing.T) {
	const frames = 6
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("http.ResponseWriter does not implement http.Flusher")
			return
		}
		for i := 1; i <= frames; i++ {
			if _, err := fmt.Fprintf(w, "data: chunk-%d\n\n", i); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.IdleTimeout = 5 * time.Second

	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	defer rc.Close()

	buf := make([]byte, 1)
	var got string
	for i := 0; i < frames; i++ {
		if _, err := io.ReadFull(rc, buf); err != nil {
			t.Fatalf("read %d: %v (real transport body must not be cut)", i, err)
		}
		got += string(buf)
	}
	if strings.Contains(got, "context canceled") {
		t.Fatalf("body read hit context canceled, got %q", got)
	}
}

func TestUserResourceAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Method != http.MethodPost {
			return nil, errors.New("want POST")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ProductCode":"p_tcaca"`)) {
			return nil, errors.New("missing ProductCode: " + string(body))
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"TotalDosage":3000,"Accounts":[
			{"PackageName":"签到包","CapacitySize":2000,"CapacityRemain":1200,"CapacityUsed":800,"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800},
			{"PackageName":"体验包","CapacitySize":1000,"CapacityRemain":300,"CapacityUsed":700,"CycleCapacitySize":1000,"CycleCapacityRemain":300,"CycleCapacityUsed":700}
		]}}}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	remain, total, err := c.UserResource(a)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
	if total != 3000 {
		t.Errorf("total=%d want 3000", total)
	}
}

func TestUserResourceNegativeClamped(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":-50,"CycleCapacityUsed":150}
		]}}}}`), nil
	})
	remain, total, err := c.UserResource(&auth.Auth{AccessToken: "at"})
	if err != nil || remain != 0 {
		t.Errorf("remain=%d err=%v, want 0 (clamped)", remain, err)
	}
	if total != 100 {
		t.Errorf("total=%d want 100", total)
	}
}

func TestDailyCheckinAlready(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errors.New("wrong path")
		}
		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !strings.Contains(err.Error(), "已签到") {
		t.Errorf("err=%v", err)
	}
}

func TestBasesAlwaysCN(t *testing.T) {
	c := testClient(nil)
	cn := &auth.Auth{Domain: ""}
	other := &auth.Auth{Domain: "example.com"}
	if c.chatBase(cn) != "https://chat.example" || c.billingBase(cn) != "https://billing.example" {
		t.Error("cn bases wrong")
	}
	// 恒 CN：domain 不同不改变上游 host。
	if c.chatBase(other) != c.chatBase(cn) || c.billingBase(other) != c.billingBase(cn) {
		t.Error("bases must be CN regardless of domain")
	}
}

func TestNewChatClientNoTotalTimeoutAndSharedTransport(t *testing.T) {
	c := New()
	if c.ChatHTTP == nil {
		t.Fatal("ChatHTTP should be initialized")
	}
	if c.ChatHTTP.Timeout != 0 {
		t.Errorf("ChatHTTP.Timeout=%v want 0 (no total cap)", c.ChatHTTP.Timeout)
	}
	// 共享同一个 Transport 实例，连接池不重复。
	if c.ChatHTTP.Transport != c.HTTP.Transport {
		t.Errorf("ChatHTTP and HTTP must share the same *http.Transport")
	}
	htr, ok := c.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", c.ChatHTTP.Transport)
	}
	if htr.ResponseHeaderTimeout != 60*time.Second { // 连接层加固：响应头上限从 120s 收到 60s（慢冷启动留 3.75× 余量）
		t.Errorf("ResponseHeaderTimeout=%v want 60s", htr.ResponseHeaderTimeout)
	}
}

func TestChatStreamRoutesToChatHTTP(t *testing.T) {
	// 显式注入 ChatHTTP（可辨识标记），验证 ChatStream 走它而非 HTTP。
	chatHit, httpHit := false, false
	c := testClient(func(*http.Request) (*http.Response, error) {
		httpHit = true
		return jsonResp(200, `{}`), nil
	})
	c.ChatHTTP = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		chatHit = true
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if !chatHit {
		t.Error("ChatStream should use ChatHTTP")
	}
	if httpHit {
		t.Error("ChatStream must not use HTTP")
	}
}

func TestChatHTTPNilFallsBackToHTTP(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	if c.chatHTTP() != c.HTTP {
		t.Error("chatHTTP() should fall back to HTTP when ChatHTTP is nil")
	}
}

func TestFetchModelsDefaultEffortDualKeyAndSizes(t *testing.T) {
	// 上游双键：老模型 reasoning.effort（auto），新模型（glm-5.3 系）只有 reasoning.defaultEffort；
	// credits/maxAllowedSize/canDisableThinking/supportsReasoning 等尺寸与能力字段应一并透出。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"glm-5.3","maxInputTokens":1000000,"maxOutputTokens":48000,"maxAllowedSize":1000000,"credits":"x0.79","supportsReasoning":true,"reasoning":{"defaultEffort":"high","canDisableThinking":true,"supportedEfforts":["low","high","max"]}},
			{"id":"auto","maxInputTokens":168000,"maxOutputTokens":32000,"supportsReasoning":true,"reasoning":{"effort":"high"}}
		],"agents":[{"name":"cli","models":["glm-5.3","auto"]}]}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	g := byID["glm-5.3"]
	if g.DefaultEffort != "high" {
		t.Errorf("glm-5.3 DefaultEffort=%q want high (from defaultEffort key)", g.DefaultEffort)
	}
	if !g.CanDisableThinking || !g.SupportsReasoning {
		t.Errorf("glm-5.3 capability flags: canDisable=%v supportsReasoning=%v want true/true", g.CanDisableThinking, g.SupportsReasoning)
	}
	if g.MaxAllowedSize != 1000000 || g.MaxTokens != 48000 || g.Credits != "x0.79" {
		t.Errorf("glm-5.3 sizes: maxAllowed=%d maxOut=%d credits=%q", g.MaxAllowedSize, g.MaxTokens, g.Credits)
	}
	if au := byID["auto"]; au.DefaultEffort != "high" {
		t.Errorf("auto DefaultEffort=%q want high (from legacy effort key)", au.DefaultEffort)
	}
}

func TestFetchModelsOverlaysV3ConfigCapabilities(t *testing.T) {
	// CLI 目录给 flash 精简字段（128K / 固定 high）；IDE /v3/config 给完整能力。
	var sawIDE bool
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.03 credits","supportsReasoning":true,"onlyReasoning":true,"reasoning":{"effort":"high","summary":"auto"}}
			],"agents":[{"name":"cli","models":["deepseek-v4.1-flash"]}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			sawIDE = true
			if r.Header.Get("User-Agent") != codeBuddyIDEUA {
				t.Errorf("v3/config UA=%q want %s", r.Header.Get("User-Agent"), codeBuddyIDEUA)
			}
			if r.Header.Get("X-Product") != "SaaS" {
				t.Errorf("X-Product=%q want SaaS", r.Header.Get("X-Product"))
			}
			if r.Header.Get("X-User-Id") != "u1" {
				t.Errorf("X-User-Id=%q want u1", r.Header.Get("X-User-Id"))
			}
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":393216,"credits":"x0.03","supportsReasoning":true,"onlyReasoning":true,"reasoning":{"canDisableThinking":true,"defaultEffort":"high","summary":"auto","supportedEfforts":["low","high","max"]}}
			]}}`), nil
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			return jsonResp(404, `{}`), nil
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "copilot.tencent.com"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if !sawIDE {
		t.Fatal("expected /v3/config request")
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	mi := infos[0]
	if mi.MaxTokens != 393216 {
		t.Errorf("MaxTokens=%d want 393216", mi.MaxTokens)
	}
	if mi.ContextWindow != 1000000 {
		t.Errorf("ContextWindow=%d want 1000000", mi.ContextWindow)
	}
	if !mi.CanDisableThinking || !mi.SupportsReasoning {
		t.Errorf("flags canDisable=%v supportsReasoning=%v", mi.CanDisableThinking, mi.SupportsReasoning)
	}
	if mi.DefaultEffort != "high" {
		t.Errorf("DefaultEffort=%q want high", mi.DefaultEffort)
	}
	if got := strings.Join(mi.Efforts, ","); got != "low,high,max" {
		t.Errorf("Efforts=%v want low,high,max", mi.Efforts)
	}
}

// --- 试用横幅（ModelTrialBanner，upstream b498416 手工适配）---

// TestFetchV3ConfigTrialBannerModels 试用横幅模型：上游把「N 天免费试用」的模型
// 只放在 data.productFeaturesConfig.ModelTrialBanner.banners[].modelId，不在
// data.models 里——纯目录解析会漏（实测 global 侧 hy4-preview-f 即如此，但该模型
// 实际可调用）。元数据口径：能力字段从 targetModelId 的既有条目继承，Credits/Tags
// 显式清空（那是「转正后」的计费与营销信息，用在免费试用版上会误导下游）。
func TestFetchV3ConfigTrialBannerModels(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{
			"models":[{"id":"hy4-preview","name":"Hy4-Preview","maxInputTokens":1000000,"maxOutputTokens":393216,"credits":"x0.29","tags":["badge:限时免费"],"supportsReasoning":true,"reasoning":{"defaultEffort":"high","supportedEfforts":["low","high","max"]}}],
			"productFeaturesConfig":{"ModelTrialBanner":{"banners":[
				{"firstUseTimeKey":"hy4.first_user_time","modelId":"hy4-preview-f","targetModelId":"hy4-preview","trialDays":14},
				{"modelId":"hy4-preview","targetModelId":"hy4-preview"},
				{"modelId":"   "},
				{"modelId":"orphan-trial","targetModelId":"not-in-catalog"}
			]}}
		}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	out, err := c.fetchV3ConfigModelMap(a, codeBuddyIDEUA)
	if err != nil {
		t.Fatalf("fetchV3ConfigModelMap() err = %v", err)
	}
	// 试用版：能力字段继承转正目标，计费/营销字段清空。
	f, ok := out["hy4-preview-f"]
	if !ok {
		t.Fatalf("试用横幅模型未补入目录: %+v", out)
	}
	if f.ContextWindow != 1000000 || f.MaxTokens != 393216 || f.DefaultEffort != "high" || len(f.Efforts) != 3 {
		t.Errorf("试用模型能力字段应从 targetModelId 继承: %+v", f)
	}
	if f.Credits != "" || f.Tags != nil {
		t.Errorf("试用模型 Credits/Tags 必须清空（那是转正后的计费与营销信息）: credits=%q tags=%v", f.Credits, f.Tags)
	}
	// 转正目标自身不得被试用横幅污染（同 id 已存在 → 跳过）。
	if base := out["hy4-preview"]; base.Credits != "x0.29" || len(base.Tags) != 1 {
		t.Errorf("转正目标条目被试用横幅污染: %+v", base)
	}
	// targetModelId 不在目录 → 只带 ID 的裸条目（不编造字段）。
	orphan, ok := out["orphan-trial"]
	if !ok {
		t.Fatalf("targetModelId 缺失时仍应补入裸条目: %+v", out)
	}
	if orphan.ContextWindow != 0 || orphan.MaxTokens != 0 || orphan.Credits != "" || orphan.Tags != nil {
		t.Errorf("未知 targetModelId 不得编造字段: %+v", orphan)
	}
	// 空/空白 modelId 跳过。
	if len(out) != 3 {
		t.Errorf("目录条目数 = %d, want 3（hy4-preview / hy4-preview-f / orphan-trial）: %+v", len(out), out)
	}
}

// --- 优惠生效价（modelPromotions，upstream 2b0eedd 的 client.go 侧手工适配）---

// promoAt 构造 promoZone（Asia/Shanghai，UTC+8）墙钟时刻。
func promoAt(t *testing.T, mo time.Month, d, h, mi int) time.Time {
	t.Helper()
	return time.Date(2026, mo, d, h, mi, 0, 0, promoZone)
}

// TestPromoClock "HH:MM" 解析：合法值转当日分钟数，坏值必须被拒（坏值窗口
// 若被当成 0 点会让优惠全天生效）。
func TestPromoClock(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"00:00", 0, true},
		{"07:50", 470, true},
		{"23:00", 1380, true},
		{" 9:05 ", 545, true},
		{"07", 0, false},
		{"07:60", 0, false},
		{"25:00", 0, false},
		{"-1:00", 0, false},
		{"ab:cd", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := promoClock(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("promoClock(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestPromoActiveCrossMidnightDaily 跨午夜 daily 时段（23:00→7:50）：必须按
// 「start > end 即跨午夜」判定，否则整段夜间优惠失效（实测 glm-5.2 夜间折扣）。
// 边界左闭右开：起点含、终点不含。
func TestPromoActiveCrossMidnightDaily(t *testing.T) {
	p := &v3ModelPromotion{
		Enabled:  true,
		Schedule: &v3PromoSchedule{Daily: []v3PromoWindow{{Start: "23:00", End: "7:50"}}},
	}
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"窗口前一分钟 22:59", promoAt(t, time.September, 23, 22, 59), false},
		{"窗口起点 23:00", promoAt(t, time.September, 23, 23, 0), true},
		{"午夜中段 03:00", promoAt(t, time.September, 24, 3, 0), true},
		{"窗口末端前一分钟 07:49", promoAt(t, time.September, 24, 7, 49), true},
		{"窗口末端 07:50（右开）", promoAt(t, time.September, 24, 7, 50), false},
		{"白天 12:00", promoAt(t, time.September, 24, 12, 0), false},
	}
	for _, tc := range cases {
		if got := promoActive(p, tc.at); got != tc.want {
			t.Errorf("%s: promoActive = %v, want %v", tc.name, got, tc.want)
		}
	}
	// 非跨午夜窗口（09:00-18:00）同样左闭右开。
	day := &v3ModelPromotion{
		Enabled:  true,
		Schedule: &v3PromoSchedule{Daily: []v3PromoWindow{{Start: "09:00", End: "18:00"}}},
	}
	if !promoActive(day, promoAt(t, time.September, 23, 9, 0)) {
		t.Error("09:00-18:00 窗口起点应生效")
	}
	if promoActive(day, promoAt(t, time.September, 23, 18, 0)) {
		t.Error("09:00-18:00 窗口终点应失效（右开）")
	}
}

// TestPromoActiveValidFromUntilBoundary validFrom/validUntil 边界：左闭右开
// （now == validFrom 生效；now == validUntil 失效），坏值忽略不误伤。
func TestPromoActiveValidFromUntilBoundary(t *testing.T) {
	p := &v3ModelPromotion{
		Enabled: true,
		Schedule: &v3PromoSchedule{
			ValidFrom:  "2026-09-23T10:00:00+08:00",
			ValidUntil: "2026-09-24T10:00:00+08:00",
		},
	}
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"起点前一秒 09:59:59", time.Date(2026, 9, 23, 9, 59, 59, 0, promoZone), false},
		{"起点当刻 10:00:00", time.Date(2026, 9, 23, 10, 0, 0, 0, promoZone), true},
		{"区间中段", time.Date(2026, 9, 24, 2, 0, 0, 0, promoZone), true},
		{"终点前一秒 09:59:59", time.Date(2026, 9, 24, 9, 59, 59, 0, promoZone), true},
		{"终点当刻 10:00:00", time.Date(2026, 9, 24, 10, 0, 0, 0, promoZone), false},
	}
	for _, tc := range cases {
		if got := promoActive(p, tc.at); got != tc.want {
			t.Errorf("%s: promoActive = %v, want %v", tc.name, got, tc.want)
		}
	}
	// 坏值（非 RFC3339）不得把整条优惠判死——解析失败即忽略该边界。
	bad := &v3ModelPromotion{Enabled: true, Schedule: &v3PromoSchedule{ValidFrom: "not-a-time", ValidUntil: "also-bad"}}
	if !promoActive(bad, promoAt(t, time.September, 23, 12, 0)) {
		t.Error("坏 validFrom/validUntil 应被忽略，而非让优惠永久失效")
	}
}

// TestPromoActiveFixedShanghaiZone 时段判定固定按 Asia/Shanghai（UTC+8）墙钟，
// 与传入 time 的 Location 表达无关（容器 UTC 部署不得整体错档）。
func TestPromoActiveFixedShanghaiZone(t *testing.T) {
	if _, off := time.Date(2026, 1, 1, 0, 0, 0, 0, promoZone).Zone(); off != 8*3600 {
		t.Fatalf("promoZone offset = %d, want 28800（Asia/Shanghai UTC+8）", off)
	}
	p := &v3ModelPromotion{
		Enabled:  true,
		Schedule: &v3PromoSchedule{Daily: []v3PromoWindow{{Start: "00:00", End: "08:00"}}},
	}
	// 同一绝对时刻：UTC 20:00 == 上海次日 04:00 → 落在窗口内。
	utc := time.Date(2026, 9, 23, 20, 0, 0, 0, time.UTC)
	if !promoActive(p, utc) {
		t.Error("UTC 20:00（上海 04:00）应落在 00:00-08:00 窗口内——判定未按 Asia/Shanghai 墙钟")
	}
	if !promoActive(p, utc.In(promoZone)) {
		t.Error("同一时刻换 Location 表达不得改变结论")
	}
	// 反例：上海本地 20:00（= UTC 12:00）不在窗口内。
	if promoActive(p, promoAt(t, time.September, 23, 20, 0)) {
		t.Error("上海 20:00 不应落在 00:00-08:00 窗口内")
	}
}

// TestApplyModelPromotionsPriority 同模型多促销取 priority 最高，且与切片顺序
// 无关；无 discount 的（错峰类）只挂标签/说明，牌价 Credits 不得被覆盖。
func TestApplyModelPromotionsPriority(t *testing.T) {
	badgeOnly := v3ModelPromotion{
		Enabled: true, Priority: 50, ModelIDs: []string{"glm-5.2"},
		Badge: &v3PromoBadge{Label: "错峰使用"}, Hover: &v3PromoHover{TextZh: "白天说明"},
	}
	night := v3ModelPromotion{
		Enabled: true, Priority: 100, ModelIDs: []string{"glm-5.2"},
		Badge: &v3PromoBadge{Label: "夜间折扣"}, Hover: &v3PromoHover{TextZh: "夜间说明"},
		Discount: &v3PromoDiscount{DiscountedCredits: "0.50x", Factor: 0.5},
	}
	for _, tc := range []struct {
		name   string
		promos []v3ModelPromotion
	}{
		{"低优先级在前", []v3ModelPromotion{badgeOnly, night}},
		{"低优先级在后", []v3ModelPromotion{night, badgeOnly}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := map[string]ModelInfo{"glm-5.2": {ID: "glm-5.2", Credits: "x1.00"}}
			applyModelPromotions(out, tc.promos)
			mi := out["glm-5.2"]
			if mi.PromoLabel != "夜间折扣" || mi.PromoNote != "夜间说明" {
				t.Errorf("priority 最高者应胜出: label=%q note=%q", mi.PromoLabel, mi.PromoNote)
			}
			if mi.PromoFactor == nil || *mi.PromoFactor != 0.5 || mi.PromoCredits != "0.50x" {
				t.Errorf("生效价未挂上: factor=%v credits=%q", mi.PromoFactor, mi.PromoCredits)
			}
			if mi.Credits != "x1.00" {
				t.Errorf("牌价 Credits 不得被生效价覆盖: %q", mi.Credits)
			}
		})
	}
}

// TestApplyModelPromotionsBadgeOnly 无 discount 对象的条目（错峰类）只挂标签+
// 说明，PromoFactor 必须留 nil（不得编造 machine-readable 折扣）。
func TestApplyModelPromotionsBadgeOnly(t *testing.T) {
	out := map[string]ModelInfo{"deepseek-v4.1-flash": {ID: "deepseek-v4.1-flash", Credits: "x0.03"}}
	applyModelPromotions(out, []v3ModelPromotion{{
		Enabled: true, Priority: 10, ModelIDs: []string{"deepseek-v4.1-flash"},
		Badge: &v3PromoBadge{Label: "错峰使用"}, Hover: &v3PromoHover{TextZh: "每日 23:00-07:50 五折"},
	}})
	mi := out["deepseek-v4.1-flash"]
	if mi.PromoLabel != "错峰使用" || mi.PromoNote != "每日 23:00-07:50 五折" {
		t.Errorf("badge-only 促销应挂标签+说明: %+v", mi)
	}
	if mi.PromoFactor != nil || mi.PromoCredits != "" {
		t.Errorf("无 discount 对象不得编造生效价: factor=%v credits=%q", mi.PromoFactor, mi.PromoCredits)
	}
}

// TestApplyModelPromotionsSkipsInactiveAndUnknown 未启用（enabled=false）的促销
// 不生效；modelIds 里的目录外模型既不挂标签、也不得被带进目录。
func TestApplyModelPromotionsSkipsInactiveAndUnknown(t *testing.T) {
	out := map[string]ModelInfo{"glm-5.2": {ID: "glm-5.2"}}
	applyModelPromotions(out, []v3ModelPromotion{
		{Enabled: false, Priority: 100, ModelIDs: []string{"glm-5.2"}, Badge: &v3PromoBadge{Label: "已停用"}},
		{Enabled: true, Priority: 100, ModelIDs: []string{"off-catalog-model"}, Badge: &v3PromoBadge{Label: "不该出现"}},
	})
	if got := out["glm-5.2"].PromoLabel; got != "" {
		t.Errorf("enabled=false 的促销不得生效: label=%q", got)
	}
	if _, ok := out["off-catalog-model"]; ok {
		t.Error("促销不得把目录外模型带进目录")
	}
	if len(out) != 1 {
		t.Errorf("目录条目数被改变: %d", len(out))
	}
}

// TestApplyModelPromotionsAtDailySwitch priority+daily 双轨切换（实测 glm-5.2
// 白天 badge-only(50) / 夜间五折(100)）：注入时钟证明「哪条生效」确实随时段变，
// 且同一时刻只有 priority 最高者挂上。没有这个时钟注入，这类断言只能靠真实墙钟。
func TestApplyModelPromotionsAtDailySwitch(t *testing.T) {
	promos := []v3ModelPromotion{
		{Enabled: true, Priority: 50, ModelIDs: []string{"glm-5.2"},
			Badge: &v3PromoBadge{Label: "错峰使用"}, Hover: &v3PromoHover{TextZh: "白天说明"}},
		{Enabled: true, Priority: 100, ModelIDs: []string{"glm-5.2"},
			Badge: &v3PromoBadge{Label: "夜间折扣"}, Discount: &v3PromoDiscount{DiscountedCredits: "0.50x", Factor: 0.5},
			Schedule: &v3PromoSchedule{Daily: []v3PromoWindow{{Start: "23:00", End: "7:50"}}}},
	}
	// 白天：夜间五折不命中，只剩 badge-only。
	day := map[string]ModelInfo{"glm-5.2": {ID: "glm-5.2", Credits: "x1.00"}}
	applyModelPromotionsAt(day, promos, promoAt(t, time.September, 23, 12, 0))
	if day["glm-5.2"].PromoLabel != "错峰使用" || day["glm-5.2"].PromoFactor != nil {
		t.Errorf("白天应为 badge-only: %+v", day["glm-5.2"])
	}
	// 夜间：五折（priority 100）胜出并带上生效价。
	night := map[string]ModelInfo{"glm-5.2": {ID: "glm-5.2", Credits: "x1.00"}}
	applyModelPromotionsAt(night, promos, promoAt(t, time.September, 23, 23, 30))
	mi := night["glm-5.2"]
	if mi.PromoLabel != "夜间折扣" || mi.PromoFactor == nil || *mi.PromoFactor != 0.5 || mi.PromoCredits != "0.50x" {
		t.Errorf("夜间应为五折生效价: %+v", mi)
	}
}

// TestFetchV3ConfigModelMapAppliesPromotions 端到端（JSON → 目录 → 生效价）：
// 目录 credits 是牌价，modelPromotions 才是客户端显示的生效价；两处合起来
// 复现实测形态——hy4-preview-f 牌价 x0.29（转正价）但试用期生效价 0x「限时免费」。
func TestFetchV3ConfigModelMapAppliesPromotions(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{
			"models":[
				{"id":"hy4-preview","maxInputTokens":1000000,"maxOutputTokens":393216,"credits":"x0.29","tags":["badge:限时免费"]},
				{"id":"glm-5.2","maxInputTokens":1000000,"maxOutputTokens":48000,"credits":"x1.00"}
			],
			"productFeaturesConfig":{"ModelTrialBanner":{"banners":[
				{"modelId":"hy4-preview-f","targetModelId":"hy4-preview","trialDays":14}
			]}},
			"modelPromotions":[
				{"enabled":true,"priority":100,"modelIds":["hy4-preview-f"],"badge":{"label":"限时免费"},"hover":{"textZh":"试用期内 0 积分"},"discount":{"factor":0,"discountedCredits":"0x"},"schedule":{"timezone":"Asia/Shanghai"}},
				{"enabled":false,"priority":999,"modelIds":["hy4-preview-f"],"badge":{"label":"已停用"},"discount":{"factor":9,"discountedCredits":"9x"}}
			]
		}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	out, err := c.fetchV3ConfigModelMap(a, codeBuddyIDEUA)
	if err != nil {
		t.Fatalf("fetchV3ConfigModelMap() err = %v", err)
	}
	f, ok := out["hy4-preview-f"]
	if !ok {
		t.Fatalf("试用横幅模型缺失: %+v", out)
	}
	// 牌价：试用版本身 Credits 清空（banner 口径），转正目标仍是 x0.29。
	if f.Credits != "" {
		t.Errorf("试用版牌价应为空（banner 清空）: %q", f.Credits)
	}
	if base := out["hy4-preview"]; base.Credits != "x0.29" {
		t.Errorf("转正目标牌价 x0.29 应保留: %q", base.Credits)
	}
	// 生效价：限时免费 0x（enabled=false 的高 priority 条目不得胜出）。
	if f.PromoFactor == nil || *f.PromoFactor != 0 {
		t.Fatalf("生效价应为 factor=0（限时免费）: %+v", f)
	}
	if f.PromoCredits != "0x" || f.PromoLabel != "限时免费" || f.PromoNote != "试用期内 0 积分" {
		t.Errorf("生效价展示字段错误: %+v", f)
	}
	// 无促销的模型不得被挂上任何 Promo 字段。
	if g := out["glm-5.2"]; g.PromoFactor != nil || g.PromoLabel != "" || g.PromoCredits != "" || g.PromoNote != "" {
		t.Errorf("无促销模型不应有 Promo 字段: %+v", g)
	}
}

// TestRefreshTokenExpiresInSanityCap expiresIn 量级上限：上游脏值（如
// 99999999999 秒 ≈ 3170 年）不得把 ExpiresAt 推到荒谬未来（NeedsRefresh 永假
// → token 永不刷新反而真过期失效）。依据 pr134-watchlist-analysis.md #4 可选加固：
// 上限 10 年（实测 R-D 响应恒 expiresIn=5184000=60d，10 年是纯防御量级）。
// 超限按脏值处理：保留旧 ExpiresAt（与缺省分支同语义）。
func TestRefreshTokenExpiresInSanityCap(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":99999999999}}`), nil
	})
	oldExpiry := time.Now().Add(time.Hour).Unix()
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: oldExpiry}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != oldExpiry {
		t.Errorf("脏 expiresIn 应保留旧 ExpiresAt=%d, got %d（被推到荒谬未来）", oldExpiry, a.ExpiresAt)
	}
	// token 本身仍应写回（脏 expiresIn 只否决过期时间，不否决凭证）。
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
}

// TestRefreshTokenExpiresInWithinCapApplied 正常量级（60d，实测 R-D 恒 5184000）
// 不受上限影响：ExpiresAt 照常推进。
func TestRefreshTokenExpiresInWithinCapApplied(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":5184000}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	before := time.Now().Unix()
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	want := before + 5184000
	if a.ExpiresAt < want-2 || a.ExpiresAt > want+2 {
		t.Errorf("ExpiresAt=%d want ~%d (60d 正常推进)", a.ExpiresAt, want)
	}
}
