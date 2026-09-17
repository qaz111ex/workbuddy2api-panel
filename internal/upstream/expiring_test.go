package upstream

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// mkDetailedResp 构造带 CycleEndTime 的 get-user-resource 响应（R-A/R-B 实测上游
// 不下发 PackageEndTime，到期时间真实字段是 CycleEndTime）。
func mkDetailedResp(accounts string) *http.Response {
	body := `{"code":0,"data":{"Response":{"Data":{"TotalCount":1,"Accounts":[` + accounts + `]}}}}`
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
		Header:     make(http.Header),
	}
}

// 本 fork 的 UserResourceDetailed 返回 (remain, total, expiring, err)：
//
//	remain   = Σ 单套餐 remain（与 UserResource 同口径）
//	total    = Σ 单套餐 size（额度总量）
//	expiring = remain 中到期时刻落在窗口内的部分（remain 的子集）
//
// 上游 CreditBuckets 的 Stable 等价于 remain-expiring、Total() 等价于 remain。

// TestUserResourceDetailedSplitsExpiring 7 天窗内的奖励包计入 expiring，
// 窗外周期包留在 Stable（= remain-expiring）。
func TestUserResourceDetailedSplitsExpiring(t *testing.T) {
	now := time.Now()
	in3d := now.Add(3 * 24 * time.Hour).Format(packageEndLayout)
	in30d := now.Add(30 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"奖励包","CycleEndTime":"` + in3d + `","CycleCapacitySize":1500,"CycleCapacityRemain":1200,"CycleCapacityUsed":300},` +
				`{"PackageName":"周期包","CycleEndTime":"` + in30d + `","CycleCapacitySize":500,"CycleCapacityRemain":300,"CycleCapacityUsed":200}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	remain, _, expiring, err := c.UserResourceDetailed(a, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
	if expiring != 1200 {
		t.Errorf("expiring=%d want 1200 (3天内过期的奖励包)", expiring)
	}
	if stable := remain - expiring; stable != 300 {
		t.Errorf("stable=%d want 300 (30天后才过期的周期包)", stable)
	}
}

// TestUserResourceDetailedNoWindowAllStable soon<=0：禁用分桶，expiring 恒 0。
func TestUserResourceDetailedNoWindowAllStable(t *testing.T) {
	in3d := time.Now().Add(3 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"p","CycleEndTime":"` + in3d + `","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	remain, _, expiring, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 0)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if expiring != 0 || remain != 80 {
		t.Errorf("expiring=%d remain=%d want {0, 80} when soon=0", expiring, remain)
	}
}

// TestUserResourceDetailedMissingEndTimeStable 无 CycleEndTime：保守归 Stable，
// 不误标快过期插队。
func TestUserResourceDetailedMissingEndTimeStable(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	remain, _, expiring, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if expiring != 0 || remain != 80 {
		t.Errorf("expiring=%d remain=%d want {0, 80} for missing end time", expiring, remain)
	}
}

// TestUserResourceBackwardCompat 旧 UserResource 签名与总量口径不变。
func TestUserResourceBackwardCompat(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	remain, _, err := c.UserResource(&auth.Auth{AccessToken: "at"})
	if err != nil || remain != 80 {
		t.Errorf("UserResource remain=%d err=%v, want 80/nil", remain, err)
	}
}

// TestUserResourceDetailedExpiringBucketFromCycleEndTime 锁定 Expiring 分桶判据是
// CycleEndTime（R-A/R-B 实测：CN/global 两域 42 字段均无 PackageEndTime，判 PackageEndTime
// 恒 miss → Expiring 恒 0，快过期积分因子从未生效）。上游只下发 CycleEndTime
// （global Bonus Pack 实测 14 天赠送积分到期时刻即此字段）。
func TestUserResourceDetailedExpiringBucketFromCycleEndTime(t *testing.T) {
	now := time.Now()
	in3d := now.Add(3 * 24 * time.Hour).Format(packageEndLayout)
	in30d := now.Add(30 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"奖励包","CycleEndTime":"` + in3d + `","CycleCapacitySize":1500,"CycleCapacityRemain":1200,"CycleCapacityUsed":300},` +
				`{"PackageName":"周期包","CycleEndTime":"` + in30d + `","CycleCapacitySize":500,"CycleCapacityRemain":300,"CycleCapacityUsed":200}`), nil
	})
	remain, _, expiring, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
	if expiring != 1200 {
		t.Errorf("expiring=%d want 1200 (CycleEndTime 在 7 天窗内的奖励包)", expiring)
	}
	if stable := remain - expiring; stable != 300 {
		t.Errorf("stable=%d want 300 (CycleEndTime 在窗外的周期包)", stable)
	}
}

// TestUserResourceDetailedSharesPackageRemainUsed 锁定单套餐取数统一到
// packageRemainUsed 口径：脏数据 CycleCapacityRemain=600 > Size=500 必须钳到 500
// （旧中间 switch 只钳负值不钳超限，会高估 remain）。
func TestUserResourceDetailedSharesPackageRemainUsed(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"脏数据","CycleCapacitySize":500,"CycleCapacityRemain":600,"CycleCapacityUsed":0}`), nil
	})
	remain, _, _, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 500 {
		t.Errorf("remain=%d want 500 (CycleRemain>Size 脏数据钳到 size)", remain)
	}
}

// TestUserResourceDetailedTotalMatchesLegacy 零回归锚：三种套餐形态的 remain 与
// 旧口径（中间 switch + 负值钳 0）逐一相等——口径统一到 packageRemainUsed 不得
// 改变 remain 聚合结果。
func TestUserResourceDetailedTotalMatchesLegacy(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			// 形态 1：纯 Cycle 活跃（当天有消耗，R-C 实测只扣 Cycle 字段）。
			`{"PackageName":"体验版","CycleCapacitySize":500,"CycleCapacityRemain":17,"CycleCapacityUsed":482,"CapacitySize":500,"CapacityRemain":500,"CapacityUsed":0},` +
				// 形态 2：Cycle 三零（从未使用）→ 回退 Capacity。
				`{"PackageName":"未使用","CapacitySize":300,"CapacityRemain":300,"CapacityUsed":0},` +
				// 形态 3：status=3 已结束裂变包（Cycle/Capacity 全套 0/100/100）。
				`{"PackageName":"已结束","CycleCapacitySize":100,"CycleCapacityRemain":0,"CycleCapacityUsed":100,"CapacitySize":100,"CapacityRemain":0,"CapacityUsed":100}`), nil
	})
	remain, _, _, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 0)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	// 旧口径：形态1 → CycleRemain=17；形态2 → CapacityRemain=300；形态3 → ②分支 CycleRemain=0。
	if remain != 317 {
		t.Errorf("remain=%d want 317 (17+300+0, 与旧口径逐一相等)", remain)
	}
}
