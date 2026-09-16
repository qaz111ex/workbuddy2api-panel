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

// panelRT 面板测试用 RoundTripper。
type panelRT func(*http.Request) (*http.Response, error)

func (f panelRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
