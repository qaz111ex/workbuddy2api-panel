package upstream

import (
	"strings"
	"testing"
)

// TestGlobalEffortInheritsCN 国际版共享模型必须与 CN 同档位（同名模型两域同一后端）：
// 历史版本把国际版 deepseek-v4.1-flash 写成仅 ['high']、且漏了 hy4-preview，导致
// 面板「模型与档位」显示错误（issue #84 实测 dsv4.1-flash 支持 low/high/max）。
func TestGlobalEffortInheritsCN(t *testing.T) {
	cases := []struct {
		model       string
		wantEfforts string
		wantDefault string
	}{
		{"deepseek-v4.1-flash", "low,high,max", "high"},
		{"hy4-preview", "high", "high"},
		{"deepseek-v4-pro", "low,high,xhigh", "high"},
		{"deepseek-v4-flash", "low,high,max", ""},
		{"hy3", "low,high", "high"},
		{"glm-5.3-flash", "low,high,max", "high"},
		{"glm-5.2", "high,xhigh", "high"},
		// 国际版专有模型仍按国际表。
		{"fast-model", "medium", ""},
		{"primary-model", "high", ""},
		{"gpt-5.6-sol", "low,medium,high,xhigh,max", "high"},
		{"hy4-preview-f", "high", "high"},
	}
	for _, c := range cases {
		efforts, def := EffortListing("global", c.model, nil, "")
		if got := strings.Join(efforts, ","); got != c.wantEfforts {
			t.Errorf("EffortListing(global,%s) efforts=%q want %q", c.model, got, c.wantEfforts)
		}
		if def != c.wantDefault {
			t.Errorf("EffortListing(global,%s) default=%q want %q", c.model, def, c.wantDefault)
		}
	}
	// CN 面行为零回归。
	if efforts, def := EffortListing("cn", "deepseek-v4.1-flash", nil, ""); strings.Join(efforts, ",") != "low,high,max" || def != "high" {
		t.Errorf("CN dsv4.1f efforts=%v default=%q", efforts, def)
	}
	// 远端桶仍为权威：探测下发 ['high'] 时不被静态表覆盖。
	if efforts, def := EffortListing("global", "deepseek-v4.1-flash", []string{"high"}, "high"); strings.Join(efforts, ",") != "high" || def != "high" {
		t.Errorf("remote must win: efforts=%v default=%q", efforts, def)
	}
}

// TestGlobalEffortMapInheritsCN globalEffortMap（出站请求体 effort 归一化）同样继承
// CN 档位：客户端传 low/max 对国际版共享模型不再被误降到 high；专有模型仍按国际表。
func TestGlobalEffortMapInheritsCN(t *testing.T) {
	efforts, defs := globalEffortMap(nil, nil)
	if got := strings.Join(efforts["deepseek-v4.1-flash"], ","); got != "low,high,max" {
		t.Errorf("globalEffortMap dsv4.1f=%q want low,high,max", got)
	}
	if defs["deepseek-v4.1-flash"] != "high" {
		t.Errorf("globalEffortMap dsv4.1f default=%q want high", defs["deepseek-v4.1-flash"])
	}
	if got := strings.Join(efforts["hy4-preview"], ","); got != "high" {
		t.Errorf("globalEffortMap hy4-preview=%q want high", got)
	}
	if got := strings.Join(efforts["fast-model"], ","); got != "medium" {
		t.Errorf("globalEffortMap fast-model=%q want medium", got)
	}
	// 远端桶覆盖静态（权威优先）。
	efforts2, _ := globalEffortMap(map[string][]string{"deepseek-v4.1-flash": {"high"}}, nil)
	if got := strings.Join(efforts2["deepseek-v4.1-flash"], ","); got != "high" {
		t.Errorf("remote override: %q want high", got)
	}
}
