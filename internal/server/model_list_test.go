package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// globalCatalogUpstream 假上游：模型目录只回固定 reasoning.effort、**无 supportedEfforts**
// （issue #84 实测的真实形态：档位完全靠网关静态兜底表）。
func globalCatalogUpstream() *upstream.Client {
	body := `{"code":0,"data":{"models":[
		{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":393216,"reasoning":{"effort":"high","summary":"auto"}},
		{"id":"hy4-preview","name":"Hy4-Preview","maxInputTokens":1000000,"maxOutputTokens":128000,"reasoning":{"effort":"high","summary":"auto"}}
	]}}`
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
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
}

// TestModelListGlobalEffortsMatchCN /v1/models 的国际版条目档位必须与国内版一致：
// deepseek-v4.1-flash = low/high/max、hy4-preview = high（支持思考）。
// 上游不下发 supportedEfforts，档位只能来自网关静态表——历史版本国际表缺 hy4-preview、
// 且把 dsv4.1-flash 写成仅 high，前端因此显示错误。
func TestModelListGlobalEffortsMatchCN(t *testing.T) {
	h := NewHandler(Config{
		Pool:          testPoolWith(globalAuth("g1")),
		Upstream:      globalCatalogUpstream(),
		GlobalEnabled: true,
		RealmFallback: true,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byID := map[string]map[string]any{}
	for _, m := range resp.Data {
		if id, _ := m["id"].(string); id != "" {
			byID[id] = m
		}
	}
	cases := []struct {
		id      string
		efforts string
		def     string
	}{
		{"global:deepseek-v4.1-flash", "low,high,max", "high"},
		{"global:hy4-preview", "high", "high"},
	}
	for _, c := range cases {
		entry := byID[c.id]
		if entry == nil {
			t.Fatalf("missing %s in /v1/models (got %d models)", c.id, len(resp.Data))
		}
		raw, _ := entry["reasoning_supported_efforts"].([]any)
		got := make([]string, 0, len(raw))
		for _, v := range raw {
			if s, ok := v.(string); ok {
				got = append(got, s)
			}
		}
		if strings.Join(got, ",") != c.efforts {
			t.Errorf("%s efforts=%v want %s", c.id, got, c.efforts)
		}
		if def, _ := entry["reasoning_default_effort"].(string); def != c.def {
			t.Errorf("%s default=%q want %q", c.id, def, c.def)
		}
	}
}
