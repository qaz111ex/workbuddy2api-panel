package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestPanelModelsGlobalRealmAndEfforts 面板「模型与档位」对国际版账号：
//  1. 走 global 目录探测（不再打 CN 端点，即 500 的根因）；
//  2. id 带 global: 前缀（前端显示的就是调用要填的值）；
//  3. 档位与国内版一致（dsv4.1-flash=low/high/max、hy4-preview=high）——
//     上游目录只回固定 reasoning.effort，档位来自网关静态兜底表。
func TestPanelModelsGlobalRealmAndEfforts(t *testing.T) {
	catalog := `{"code":0,"data":{"models":[
		{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":393216,"reasoning":{"effort":"high","summary":"auto"}},
		{"id":"hy4-preview","name":"Hy4-Preview","maxInputTokens":1000000,"maxOutputTokens":128000,"reasoning":{"effort":"high","summary":"auto"}}
	]}}`
	up := &upstream.Client{
		HTTP: &http.Client{Transport: panelRT(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(catalog)),
			}, nil
		})},
		ChatBaseCN:     "https://cn.example",
		ChatBaseGlobal: "https://global.example",
		GlobalEnabled:  true,
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	p.SetCredits("g1", 1000, 0)

	pn := New(Config{Pool: p, Upstream: up, Version: "test", APIKey: "test-key"})
	req := httptest.NewRequest("GET", "/panel/api/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Models []struct {
			ID            string   `json:"id"`
			Efforts       []string `json:"supported_efforts"`
			DefaultEffort string   `json:"default_effort"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byID := map[string]int{}
	for i, m := range got.Models {
		byID[m.ID] = i
	}
	for _, c := range []struct {
		id      string
		efforts string
		def     string
	}{
		{"global:deepseek-v4.1-flash", "low,high,max", "high"},
		{"global:hy4-preview", "high", "high"},
	} {
		i, ok := byID[c.id]
		if !ok {
			t.Fatalf("missing %s in panel models (got %d)", c.id, len(got.Models))
		}
		if strings.Join(got.Models[i].Efforts, ",") != c.efforts {
			t.Errorf("%s efforts=%v want %s", c.id, got.Models[i].Efforts, c.efforts)
		}
		if got.Models[i].DefaultEffort != c.def {
			t.Errorf("%s default=%q want %q", c.id, got.Models[i].DefaultEffort, c.def)
		}
	}
}

// TestPanelModelsDualRealmMixedPool 混合池（同时有 cn 与 global 可用账号）必须
// **一次查询同时输出两域**模型（吸收 upstream c3cc888 的修复项）。
//
// 回归背景：旧实现走 `Pool.Pick()` 只取一个账号，于是面板只显示恰好被选中的那一域
// 的模型，另一域静默消失——模型名带 cn:/global: 前缀的语义本身就是"两域都可调用"，
// 只列一域会让用户看不到另一半可用的模型名（纯 global 池则整页只有 global 条目）。
func TestPanelModelsDualRealmMixedPool(t *testing.T) {
	up := &upstream.Client{
		HTTP: &http.Client{Transport: panelRT(func(r *http.Request) (*http.Response, error) {
			// 按 host 分流：global 域返回 global 专属模型，cn 域返回 cn 专属模型。
			body := `{"code":0,"data":{"models":[{"id":"cn-only-model","name":"CN"}]}}`
			if r.URL.Host == "global.example" {
				body = `{"code":0,"data":{"models":[{"id":"global-only-model","name":"G"}]}}`
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:     "https://cn.example",
		ChatBaseGlobal: "https://global.example",
		GlobalEnabled:  true,
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	p.SetCredits("c1", 1000, 0)
	p.SetCredits("g1", 1000, 0)

	pn := New(Config{Pool: p, Upstream: up, Version: "test", APIKey: "test-key"})
	req := httptest.NewRequest("GET", "/panel/api/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range got.Models {
		ids[m.ID] = true
	}
	// 两域条目必须同时出现（旧实现只会出现其中一域）。
	for _, want := range []string{"cn:cn-only-model", "global:global-only-model"} {
		if !ids[want] {
			t.Errorf("mixed pool must list both realms; missing %s (got %v)", want, got.Models)
		}
	}
}

// TestPanelModelsGlobalDisabledSkipsGlobalRealm 逃生门（global.enabled=false）：
// 即便池里有 global 账号，面板也不查询 global 域（只列 cn），与 /v1/models 同口径。
func TestPanelModelsGlobalDisabledSkipsGlobalRealm(t *testing.T) {
	var globalHits int
	up := &upstream.Client{
		HTTP: &http.Client{Transport: panelRT(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host == "global.example" {
				globalHits++
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":0,"data":{"models":[{"id":"cn-only-model"}]}}`)),
			}, nil
		})},
		ChatBaseCN:     "https://cn.example",
		ChatBaseGlobal: "https://global.example",
		GlobalEnabled:  false,
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"})
	p.SetCredits("c1", 1000, 0)
	p.SetCredits("g1", 1000, 0)

	pn := New(Config{Pool: p, Upstream: up, Version: "test", APIKey: "test-key"})
	req := httptest.NewRequest("GET", "/panel/api/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if globalHits != 0 {
		t.Errorf("global realm must not be probed when global.enabled=false, got %d hits", globalHits)
	}
	if strings.Contains(rec.Body.String(), "global:") {
		t.Errorf("global models must not be listed when global.enabled=false: %s", rec.Body)
	}
}

// TestPanelModelsNoAccounts503 两域都无可用账号 → 503（既有语义保留）。
func TestPanelModelsNoAccounts503(t *testing.T) {
	up := &upstream.Client{ChatBaseCN: "https://cn.example", ChatBaseGlobal: "https://global.example", GlobalEnabled: true}
	p := pool.New("")
	pn := New(Config{Pool: p, Upstream: up, Version: "test", APIKey: "test-key"})
	req := httptest.NewRequest("GET", "/panel/api/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	pn.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 (no available account)", rec.Code)
	}
}

// panelRT 面板测试用 RoundTripper。
type panelRT func(*http.Request) (*http.Response, error)

func (f panelRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
