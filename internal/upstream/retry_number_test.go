// retry_number_test.go 钉住限流响应头数字解析的溢出防护（吸收上游 5f6c7ca）：
// 16 位数字乘 1e9 会溢出 int64 回绕，回绕值若恰好落在 sanity 区间内，就会被
// 当成合法等待时长——必须在**乘法之前**按上限拒绝。
package upstream

import (
	"math/big"
	"net/http"
	"testing"
	"time"
)

// 用例前提自检：这些数字的朴素乘积确实超出 int64 表示范围（否则本文件失去意义）。
func TestParseRetryNumberOverflowFixturePremise(t *testing.T) {
	prod := new(big.Int).Mul(big.NewInt(9999999999999999), big.NewInt(1000000000))
	if prod.IsInt64() {
		t.Fatalf("用例前提失效：%v 未溢出 int64，需换更大的字面量", prod)
	}
}

// 超上限（含会回绕的 16 位数字）必须返回 ok=false——回绕值不得成为合法等待时长。
func TestParseRetryNumberRejectsOverflow(t *testing.T) {
	cases := []struct {
		v      string
		header string
	}{
		{"9999999999999999", "Retry-After"},    // 16 位：len 守卫放行，乘法会回绕
		{"9999999999999999", "Retry-After-Ms"}, // 同上（毫秒口径）
		{"9223372036854775", "Retry-After"},    // 16 位：len 守卫同样放行
		{"7201", "Retry-After"},                // 仅超 sanity 1 秒
		{"7200001", "Retry-After-Ms"},          // 仅超 sanity 1 毫秒
	}
	for _, c := range cases {
		if d, ok := parseRetryNumber(c.v, c.header); ok {
			t.Errorf("parseRetryNumber(%q, %s) = %v, ok=true；超上限值必须被拒（回绕值会骗过 sanity 校验）", c.v, c.header, d)
		}
	}
}

// 边界值必须仍然接受：修溢出不能顺手把合法上限也砍掉。
func TestParseRetryNumberAcceptsAtSanityBoundary(t *testing.T) {
	if d, ok := parseRetryNumber("7200", "Retry-After"); !ok || d != 7200*time.Second {
		t.Fatalf("7200 秒应接受，got %v ok=%v", d, ok)
	}
	if d, ok := parseRetryNumber("7200000", "Retry-After-Ms"); !ok || d != 7200*time.Second {
		t.Fatalf("7200000 毫秒应接受，got %v ok=%v", d, ok)
	}
	if d, ok := parseRetryNumber("30", "Retry-After"); !ok || d != 30*time.Second {
		t.Fatalf("常规值应接受，got %v ok=%v", d, ok)
	}
}

// 端到端：畸形 Retry-After 被丢弃后必须继续尝试下一个头，而不是整体失败。
func TestParseRetryAfterSkipsOverflowHeaderUsesNext(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "9999999999999999") // 超限/回绕：丢弃
	h.Set("Retry-After-Ms", "1500")          // 合法：1.5s
	d, ok := ParseRetryAfter(h)
	if !ok || d != 1500*time.Millisecond {
		t.Fatalf("got %v ok=%v want 1.5s true", d, ok)
	}
}

// 位数守卫（本 fork 既有加固）与乘法前上限守卫叠加后行为不变：
// 17 位以上仍直接丢弃。
func TestParseRetryNumberRejectsOverlongDigits(t *testing.T) {
	if _, ok := parseRetryNumber("12345678901234567", "Retry-After"); ok {
		t.Fatal("17 位数字应被位数守卫拒绝")
	}
}
