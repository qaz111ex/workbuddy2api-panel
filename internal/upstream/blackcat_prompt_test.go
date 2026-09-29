// blackcat_prompt_test.go 夜猫子对话文案多样化的回归（11140 内容审核误禁的**根因**侧）。
//
// 缺陷形态：此前每号每晚发一字不差的同一句话（"1+1等于几？直接回答。"），大量账号
// 在凌晨从同一出口 IP 发出完全相同的裸文本 → 机器指纹最容易被内容审核命中 → 一夜
// 219 次审核拒绝，且被错误分类成账号封禁 → 误禁 32 个健康账号。
//
// 本文件锁定「不再有全池一字不差的文案」这一性质。分类侧的闸门在
// client_test.go 的 TestClassify 与 handler_content_review_test.go。
package upstream

import (
	"fmt"
	"strings"
	"testing"
)

// TestNightChatPromptDeterministic 同 (uid, 日期, 次数) 必须稳定复现——排查依赖它。
func TestNightChatPromptDeterministic(t *testing.T) {
	a := nightChatPrompt("uid-1", "2026-09-30", 0)
	b := nightChatPrompt("uid-1", "2026-09-30", 0)
	if a != b {
		t.Errorf("同一 (uid,日期,次数) 文案必须稳定：%q vs %q", a, b)
	}
	if a == "" {
		t.Error("文案不得为空")
	}
	if !bcHas(nightChatPrompts, a) {
		t.Errorf("文案必须取自池内，got %q", a)
	}
}

// TestNightChatPromptSpreadsAcrossAccountsAndDays 不同账号 / 不同日期 / 不同次数
// 必须散开——这正是消除「全池一字不差」指纹的机制本身。
func TestNightChatPromptSpreadsAcrossAccountsAndDays(t *testing.T) {
	// 不同账号（同一天、同一次）应散开到多个文案，而不是全部相同。
	seen := map[string]int{}
	for i := 0; i < 64; i++ {
		seen[nightChatPrompt(bcUID(i), "2026-09-30", 0)]++
	}
	if len(seen) < 8 {
		t.Errorf("64 个不同账号只散出 %d 种文案，指纹未被消除（池大小 %d）：%v",
			len(seen), len(nightChatPrompts), seen)
	}

	// 同一账号跨日期也应散开（否则每晚同一句话仍会被识别）。
	seenDay := map[string]int{}
	for d := 1; d <= 28; d++ {
		seenDay[nightChatPrompt("uid-fixed", fmt.Sprintf("2026-09-%02d", d), 0)]++
	}
	if len(seenDay) < 5 {
		t.Errorf("同一账号跨 28 天只散出 %d 种文案，仍存在跨天重复指纹：%v", len(seenDay), seenDay)
	}

	// 同一账号同一天的不同次数（need>1）必须互不相同——否则一轮内就是重复文本。
	seenNth := map[string]bool{}
	for n := 0; n < 3; n++ {
		seenNth[nightChatPrompt("uid-fixed", "2026-09-30", n)] = true
	}
	if len(seenNth) < 3 {
		t.Errorf("同账号同日 3 次对话只有 %d 种文案（应互不相同）：%v", len(seenNth), seenNth)
	}
}

// TestNightChatPromptPoolHasNoOldFixedText 防回归：旧的一字不差文案不得再出现在池里，
// 也不得被任何 (uid,日期,次数) 选中。
func TestNightChatPromptPoolHasNoOldFixedText(t *testing.T) {
	const oldFixed = "1+1等于几？直接回答。"
	for _, p := range nightChatPrompts {
		if strings.Contains(p, "1+1") {
			t.Errorf("池内仍有旧的固定文案（会按账号重复命中同一指纹）：%q", p)
		}
	}
	for i := 0; i < 200; i++ {
		if p := nightChatPrompt(bcUID(i), "2026-09-30", i%3); p == oldFixed {
			t.Fatalf("仍会选中旧的固定文案: %q", p)
		}
	}
}

// TestNightChatPromptPoolIsSubstantial 池容量下限：池被削小到近似固定文本时，
// 上面的散开断言会失去意义——这里显式钉住下限。
func TestNightChatPromptPoolIsSubstantial(t *testing.T) {
	if len(nightChatPrompts) < 12 {
		t.Errorf("文案池过小（%d 条），不足以消除全池指纹；下限 12", len(nightChatPrompts))
	}
	dup := map[string]bool{}
	for _, p := range nightChatPrompts {
		if strings.TrimSpace(p) == "" {
			t.Error("池内存在空文案")
		}
		if dup[p] {
			t.Errorf("池内文案重复：%q", p)
		}
		dup[p] = true
	}
}

func bcUID(i int) string { return fmt.Sprintf("acct-%03d", i) }

func bcHas(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
