// checkin_retry_test.go 钉住签到/余额维护类计费调用的瞬时错误有界重试：
// 上游 5xx（实测偶发 code 10000 / http 500）与网络抖动重试，业务错误（已签到/
// 参数错/4xx）不重试——重试只会原样再失败一次（吸收上游 dd4ea34）。
//
// 退避序列走 SetBillingRetryDelaysForTest 压到 0：断言「重试发生了」不必付
// 真实 2s/4s 等待。
package upstream

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// shortBillingRetry 把重试退避压到 0（重试次数不变，只是立即重发）。
func shortBillingRetry(t *testing.T) {
	t.Helper()
	t.Cleanup(SetBillingRetryDelaysForTest(0, 0))
}

// 生产默认退避必须是 2s/4s（行为与上游一致，不为测试牺牲生产语义）。
func TestBillingRetryDelaysProductionDefaults(t *testing.T) {
	want := []time.Duration{2 * time.Second, 4 * time.Second}
	if len(billingRetryDelays) != len(want) {
		t.Fatalf("默认退避档数=%d want %d", len(billingRetryDelays), len(want))
	}
	for i, d := range want {
		if billingRetryDelays[i] != d {
			t.Fatalf("billingRetryDelays[%d]=%v want %v", i, billingRetryDelays[i], d)
		}
	}
}

// 测试钩子必须可还原：退出用例后默认值不受影响（防跨用例污染）。
func TestSetBillingRetryDelaysForTestRestores(t *testing.T) {
	before := append([]time.Duration(nil), billingRetryDelays...)
	restore := SetBillingRetryDelaysForTest(time.Millisecond)
	if len(billingRetryDelays) != 1 || billingRetryDelays[0] != time.Millisecond {
		t.Fatalf("钩子未生效: %+v", billingRetryDelays)
	}
	restore()
	if len(billingRetryDelays) != len(before) {
		t.Fatalf("还原失败: %+v", billingRetryDelays)
	}
	for i := range before {
		if billingRetryDelays[i] != before[i] {
			t.Fatalf("还原后 [%d]=%v want %v", i, billingRetryDelays[i], before[i])
		}
	}
}

// 退避档数即重试次数上限：3 档 → 最多 4 次调用（1 次首发 + 3 次重试）。
func TestRetryBillingTransientCountFollowsDelays(t *testing.T) {
	t.Cleanup(SetBillingRetryDelaysForTest(0, 0, 0))
	var calls int32
	err := (&Client{}).retryBillingTransient(func() error {
		atomic.AddInt32(&calls, 1)
		return &Error{Kind: ErrServer, Msg: "boom"}
	})
	if err == nil {
		t.Fatal("持续 5xx 应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 4 {
		t.Fatalf("calls=%d want 4（1 次 + 3 档退避）", n)
	}
}

// 非瞬时错误（业务 4xx）首次即返回，一次都不重试。
func TestRetryBillingTransientSkipsBusinessError(t *testing.T) {
	t.Cleanup(SetBillingRetryDelaysForTest(0, 0))
	var calls int32
	_ = (&Client{}).retryBillingTransient(func() error {
		atomic.AddInt32(&calls, 1)
		return &Error{Kind: ErrClient, Msg: "bad params"}
	})
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls=%d want 1（业务错误不重试）", n)
	}
}

// 网络层错误（非 *Error）算瞬时：重试。
func TestIsTransientBillingErr(t *testing.T) {
	if isTransientBillingErr(nil) {
		t.Fatal("nil 不是瞬时错误")
	}
	if !isTransientBillingErr(errors.New("dial tcp: connection reset")) {
		t.Fatal("网络层错误应判为瞬时")
	}
	if !isTransientBillingErr(&Error{Kind: ErrServer, Msg: "500"}) {
		t.Fatal("5xx 应判为瞬时")
	}
	for _, k := range []ErrKind{ErrClient, ErrSoftRate, ErrHardCredit, ErrNotFound, ErrSessionDead, ErrBadParams} {
		if isTransientBillingErr(&Error{Kind: k, Msg: "x"}) {
			t.Fatalf("ErrKind=%v 不应判为瞬时（重试无意义）", k)
		}
	}
}

func TestDailyCheckinRetriesTransient500(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
		}
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("首次 500 应重试成功，err=%v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls=%d want 2（1 次失败 + 1 次重试）", n)
	}
}

func TestDailyCheckinRetriesExhausted(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("持续 500 应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("calls=%d want 3（1 次 + 2 次重试封顶）", n)
	}
}

func TestDailyCheckinNoRetryOnBusinessError(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		// 「今天已签到」是业务幂等拒绝（code!=0），重试无意义。
		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !IsAlreadyCheckin(err) {
		t.Fatalf("err=%v want already-checkin business error", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls=%d want 1（业务错误不重试）", n)
	}
}

func TestDailyCheckinNoRetryOn4xx(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(403, `{"code":11140,"msg":"request illegal"}`), nil
	})
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("403 应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls=%d want 1（4xx 非瞬时，不重试）", n)
	}
}

// 余额查询同样重试（本 fork 的入口是 UserResourceDetailed，非上游的
// UserResourceDetailedWithExpiry——本仓库没有后者）。
func TestUserResourceDetailedRetriesTransient500(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CapacitySize":100,"CapacityRemain":60}
		]}}}}`), nil
	})
	remain, _, _, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 0)
	if err != nil || remain != 60 {
		t.Fatalf("remain=%d err=%v, want 60 nil", remain, err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls=%d want 2（1 次失败 + 1 次重试）", n)
	}
}

// 余额查询的业务错误（4xx）不重试。
func TestUserResourceDetailedNoRetryOn4xx(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(403, `{"code":11140,"msg":"request illegal"}`), nil
	})
	if _, _, _, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 0); err == nil {
		t.Fatal("403 应返回错误")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls=%d want 1（业务错误不重试）", n)
	}
}
