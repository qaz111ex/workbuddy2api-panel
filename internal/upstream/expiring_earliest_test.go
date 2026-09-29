// 最早未来到期批次（UserResourceDetailedWithExpiry）的取数口径锚点。
//
// 背景：pool 的「最早到期优先路由」需要知道每个账号**最早过期的那个批次**及其剩余量。
// 该信息上游响应里本来就有（每包的 CycleEndTime），旧实现算完窗口内求和后把时间戳丢了。
// 本改动的判据：
//   - earliestAt / earliestRemaining 是「全部未来批次」中最早的一批（与 soon 窗口无关）；
//   - 已过期、零余额、缺 CycleEndTime、解析失败的包都不构成最早批次；
//   - expiring 的既有口径**逐字不变**（已过期但仍有余量的包在旧实现里同样计入 expiring）；
//   - 复用同一份响应体，**零新增上游请求**。
package upstream

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// parseEnd 按上游墙钟格式解析到期串（与实现同一 loc，便于等值断言）。
func parseEnd(t *testing.T, raw string) time.Time {
	t.Helper()
	got, err := time.ParseInLocation(packageEndLayout, raw, softRateResetLoc)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return got
}

// TestUserResourceDetailedWithExpiryEarliestFutureBatch 最早批次 = 全部未来批次中
// 到期时刻最小者；earliestRemaining 只算该批次。响应里最早包排在最后，锚定「取 min」
// 而非「取首个」。
func TestUserResourceDetailedWithExpiryEarliestFutureBatch(t *testing.T) {
	now := time.Now()
	in3d := now.Add(3 * 24 * time.Hour).Format(packageEndLayout)
	in10d := now.Add(10 * 24 * time.Hour).Format(packageEndLayout)
	in30d := now.Add(30 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"奖励包","CycleEndTime":"` + in10d + `","CycleCapacitySize":500,"CycleCapacityRemain":50,"CycleCapacityUsed":450},` +
				`{"PackageName":"周期包","CycleEndTime":"` + in30d + `","CycleCapacitySize":500,"CycleCapacityRemain":300,"CycleCapacityUsed":200},` +
				`{"PackageName":"最早包","CycleEndTime":"` + in3d + `","CycleCapacitySize":1500,"CycleCapacityRemain":1200,"CycleCapacityUsed":300}`), nil
	})
	remain, _, expiring, earliestAt, earliestRemaining, err := c.UserResourceDetailedWithExpiry(
		&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 1550 {
		t.Errorf("remain=%d want 1550 (50+300+1200)", remain)
	}
	if expiring != 1200 {
		t.Errorf("expiring=%d want 1200（7 天窗内只有最早包）", expiring)
	}
	if want := parseEnd(t, in3d); !earliestAt.Equal(want) {
		t.Errorf("earliestAt=%v want %v（全部未来批次中最早者，与窗口无关）", earliestAt, want)
	}
	if earliestRemaining != 1200 {
		t.Errorf("earliestRemaining=%d want 1200（只算最早批次的剩余量）", earliestRemaining)
	}
}

// TestUserResourceDetailedWithExpiryMergesTieSkipsExpiredAndEmpty 同到期时刻的多个包
// 合并为一批（剩余量求和）；已过期与零余额的包都不参与最早批次。
func TestUserResourceDetailedWithExpiryMergesTieSkipsExpiredAndEmpty(t *testing.T) {
	now := time.Now()
	same := now.Add(5 * 24 * time.Hour).Format(packageEndLayout)
	past := now.Add(-24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"A","CycleEndTime":"` + same + `","CycleCapacitySize":100,"CycleCapacityRemain":100,"CycleCapacityUsed":0},` +
				`{"PackageName":"B","CycleEndTime":"` + same + `","CycleCapacitySize":80,"CycleCapacityRemain":80,"CycleCapacityUsed":0},` +
				`{"PackageName":"已过期","CycleEndTime":"` + past + `","CycleCapacitySize":900,"CycleCapacityRemain":900,"CycleCapacityUsed":0},` +
				`{"PackageName":"零剩余","CycleEndTime":"` + same + `","CycleCapacitySize":10,"CycleCapacityRemain":0,"CycleCapacityUsed":10}`), nil
	})
	_, _, _, earliestAt, earliestRemaining, err := c.UserResourceDetailedWithExpiry(
		&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if want := parseEnd(t, same); !earliestAt.Equal(want) {
		t.Errorf("earliestAt=%v want %v（同到期时刻合并，已过期包被跳过）", earliestAt, want)
	}
	if earliestRemaining != 180 {
		t.Errorf("earliestRemaining=%d want 180 (A100+B80，已过期/零余额不计)", earliestRemaining)
	}
}

// TestUserResourceDetailedWithExpiryNoValidBatch 无有效批次（缺字段/格式坏/已过期）
// 返回零值——pool 据此把该账号排除在最早到期优先集之外。
func TestUserResourceDetailedWithExpiryNoValidBatch(t *testing.T) {
	now := time.Now()
	past := now.Add(-24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"无到期","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20},` +
				`{"PackageName":"格式坏","CycleEndTime":"not-a-time","CycleCapacitySize":100,"CycleCapacityRemain":30,"CycleCapacityUsed":70},` +
				`{"PackageName":"已过期","CycleEndTime":"` + past + `","CycleCapacitySize":100,"CycleCapacityRemain":40,"CycleCapacityUsed":60}`), nil
	})
	remain, _, expiring, earliestAt, earliestRemaining, err := c.UserResourceDetailedWithExpiry(
		&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 150 {
		t.Errorf("remain=%d want 150（无有效到期不影响余额聚合）", remain)
	}
	if !earliestAt.IsZero() || earliestRemaining != 0 {
		t.Errorf("无有效批次应返回零值，got at=%v remaining=%d", earliestAt, earliestRemaining)
	}
	if expiring != 40 {
		t.Errorf("expiring=%d want 40（既有口径：已过期包仍计入窗口分桶）", expiring)
	}
}

// TestUserResourceDetailedWithExpiryKeepsLegacyExpiringBucket 加最早批次跟踪**不得**
// 改变 expiring 的既有口径：已过期但仍有余额的包在旧实现里也满足
// !end.After(now+soon) 而被计入。这是零回归锚点——顺手「修」语义会造成静默行为变更。
func TestUserResourceDetailedWithExpiryKeepsLegacyExpiringBucket(t *testing.T) {
	now := time.Now()
	past := now.Add(-24 * time.Hour).Format(packageEndLayout)
	in3d := now.Add(3 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"已过期","CycleEndTime":"` + past + `","CycleCapacitySize":100,"CycleCapacityRemain":100,"CycleCapacityUsed":0},` +
				`{"PackageName":"窗内","CycleEndTime":"` + in3d + `","CycleCapacitySize":200,"CycleCapacityRemain":200,"CycleCapacityUsed":0}`), nil
	})
	remain, _, expiring, earliestAt, earliestRemaining, err := c.UserResourceDetailedWithExpiry(
		&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 300 || expiring != 300 {
		t.Errorf("remain=%d expiring=%d want 300/300（已过期包在旧口径下同样计入 expiring）", remain, expiring)
	}
	if want := parseEnd(t, in3d); !earliestAt.Equal(want) || earliestRemaining != 200 {
		t.Errorf("earliestAt/earliestRemaining=%v/%d want %v/200（已过期包不参与最早批次）",
			earliestAt, earliestRemaining, want)
	}
}

// TestUserResourceDetailedDelegatesToWithExpiry 旧签名（4 值）与带到期版本逐值一致：
// scheduler 等既有调用方零改动，行为不变。
func TestUserResourceDetailedDelegatesToWithExpiry(t *testing.T) {
	now := time.Now()
	in3d := now.Add(3 * 24 * time.Hour).Format(packageEndLayout)
	mk := func() *Client {
		return testClient(func(r *http.Request) (*http.Response, error) {
			return mkDetailedResp(
				`{"PackageName":"p","CycleEndTime":"` + in3d + `","CycleCapacitySize":500,"CycleCapacityRemain":320,"CycleCapacityUsed":180}`), nil
		})
	}
	remain, total, expiring, err := mk().UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("legacy: %v", err)
	}
	remain2, total2, expiring2, _, _, err := mk().UserResourceDetailedWithExpiry(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("with expiry: %v", err)
	}
	if remain != remain2 || total != total2 || expiring != expiring2 {
		t.Errorf("四值版本 (%d,%d,%d) != 带到期版本 (%d,%d,%d)", remain, total, expiring, remain2, total2, expiring2)
	}
}

// TestUserResourceDetailedWithExpiryNoExtraRequest 复用同一份响应体：带到期版本
// 只发一次余额请求（本改动是纯管道，不得引入额外上游调用）。
func TestUserResourceDetailedWithExpiryNoExtraRequest(t *testing.T) {
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		in3d := time.Now().Add(3 * 24 * time.Hour).Format(packageEndLayout)
		return mkDetailedResp(
			`{"PackageName":"p","CycleEndTime":"` + in3d + `","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	if _, _, _, _, _, err := c.UserResourceDetailedWithExpiry(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour); err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("上游请求数=%d want 1（最早批次复用同一响应体，零新增请求）", got)
	}
}
