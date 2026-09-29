// credit_package_endtime_test.go 钉住逐包接口的到期字段补读（吸收上游 4466a3e）。
//
// 真实缺陷：CreditPackages 的解析结构只读 ExpiredTime / PackageEndTime，而 CN/global
// 实测字段全集里这两个恒 miss（真实下发的是 CycleEndTime）→ end_time 恒空，
// 面板积分构成页到期列全部显示 "—"。同文件 UserResourceDetailed 的聚合口径早已
// 改用 CycleEndTime，逐包接口漏同步了这一修复。
package upstream

import (
	"net/http"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// creditPackagesClient 造一个按 Accounts 数组回包的假上游。
// doJSON 已解过外层信封，故 data 之下从 Response 开始（与 CreditPackages 的解析层级一致）。
func creditPackagesClient(t *testing.T, accounts string) *Client {
	t.Helper()
	return testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[`+accounts+`]}}}}`), nil
	})
}

// CycleEndTime 是实测唯一下发的到期字段：必须被读进 EndTime。
func TestCreditPackagesReadsCycleEndTime(t *testing.T) {
	c := creditPackagesClient(t, `{"PackageName":"拉新权益包","CycleCapacitySize":100,"CycleCapacityRemain":40,"CycleEndTime":"2026-10-25 12:00:00"}`)

	packs, sumRemain, sumSize, err := c.CreditPackages(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 1 {
		t.Fatalf("packs=%d want 1", len(packs))
	}
	if packs[0].EndTime != "2026-10-25 12:00:00" {
		t.Fatalf("EndTime=%q want CycleEndTime（此前恒空 → 面板到期列显示 —）", packs[0].EndTime)
	}
	if sumRemain != 40 || sumSize != 100 {
		t.Fatalf("sumRemain=%d sumSize=%d want 40/100", sumRemain, sumSize)
	}
}

// 三口径优先级：ExpiredTime > PackageEndTime > CycleEndTime（上游若恢复下发前两者，
// 优先级不变）。
func TestCreditPackagesEndTimePriority(t *testing.T) {
	cases := []struct {
		name    string
		account string
		want    string
	}{
		{
			name:    "三者在场取 ExpiredTime",
			account: `{"PackageName":"p","CapacitySize":10,"CapacityRemain":5,"ExpiredTime":"2026-01-01 00:00:00","PackageEndTime":"2026-02-02 00:00:00","CycleEndTime":"2026-03-03 00:00:00"}`,
			want:    "2026-01-01 00:00:00",
		},
		{
			name:    "无 ExpiredTime 取 PackageEndTime",
			account: `{"PackageName":"p","CapacitySize":10,"CapacityRemain":5,"PackageEndTime":"2026-02-02 00:00:00","CycleEndTime":"2026-03-03 00:00:00"}`,
			want:    "2026-02-02 00:00:00",
		},
		{
			name:    "前两者空串回落 CycleEndTime（实测形态）",
			account: `{"PackageName":"p","CapacitySize":10,"CapacityRemain":5,"ExpiredTime":"","PackageEndTime":"","CycleEndTime":"2026-03-03 00:00:00"}`,
			want:    "2026-03-03 00:00:00",
		},
		{
			name:    "三者全空 → 留空（不伪造 1970）",
			account: `{"PackageName":"p","CapacitySize":10,"CapacityRemain":5}`,
			want:    "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			packs, _, _, err := creditPackagesClient(t, c.account).CreditPackages(&auth.Auth{AccessToken: "at"})
			if err != nil {
				t.Fatal(err)
			}
			if len(packs) != 1 {
				t.Fatalf("packs=%d want 1", len(packs))
			}
			if packs[0].EndTime != c.want {
				t.Fatalf("EndTime=%q want %q", packs[0].EndTime, c.want)
			}
		})
	}
}

// 逐包 CycleEndTime 与聚合口径同源：同一条 Accounts 数据在 CreditPackages 与
// UserResourceDetailed 上都应解出到期时间（防两处再次分叉）。
func TestCreditPackagesCycleEndTimeAgreesWithAggregate(t *testing.T) {
	const account = `{"PackageName":"拉新权益包","CycleCapacitySize":100,"CycleCapacityRemain":40,"CycleEndTime":"2026-10-25 12:00:00"}`
	c := creditPackagesClient(t, account)

	packs, _, _, err := c.CreditPackages(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatal(err)
	}
	if packs[0].EndTime == "" {
		t.Fatal("逐包 EndTime 为空：与聚合口径（读 CycleEndTime）分叉")
	}

	// 聚合侧同数据：soon 足够大时该包必须计入 expiring（说明它确实解析到了到期时刻）。
	remain, _, expiring, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 365*24*100*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if remain != 40 || expiring != 40 {
		t.Fatalf("remain=%d expiring=%d want 40/40（聚合侧未解析到 CycleEndTime）", remain, expiring)
	}
}
