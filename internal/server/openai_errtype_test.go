// openai_errtype_test.go OpenAI 错误体 error.type 的状态码映射回归。
//
// 缺陷：writeOpenAIError 此前把 `type` **恒**写成 "api_error"。按 OpenAI 惯例，
// `api_error` 表示**服务端**故障，4xx 属客户端错误应为 `invalid_request_error`。
// 恒用 api_error 有实际后果：按 `type` 决定「是否重试」的客户端会把不该重试的 4xx
// 当成服务端故障反复重试。
//
// 本文件同时锁定「5xx 仍是 api_error」——只把 4xx 改对，不能把「该重试」也改坏。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIErrTypeMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		// 客户端错误 → invalid_request_error（含认证；OpenAI 用 code 区分认证类）。
		{"400 参数错误", http.StatusBadRequest, "invalid_request_error"},
		{"401 认证失败", http.StatusUnauthorized, "invalid_request_error"},
		{"403 无权限", http.StatusForbidden, "invalid_request_error"},
		{"404 不存在", http.StatusNotFound, "invalid_request_error"},
		{"413 过大", http.StatusRequestEntityTooLarge, "invalid_request_error"},
		{"422 语义错误", http.StatusUnprocessableEntity, "invalid_request_error"},
		// 限流单独一类（客户端应退避重试）。
		{"429 限流", http.StatusTooManyRequests, "rate_limit_error"},
		// 服务端错误 → api_error（客户端**应当**重试）。
		{"500", http.StatusInternalServerError, "api_error"},
		{"502", http.StatusBadGateway, "api_error"},
		{"503", http.StatusServiceUnavailable, "api_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := openAIErrType(tc.status); got != tc.want {
				t.Errorf("openAIErrType(%d) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

// TestWriteOpenAIErrorUsesMappedType 端到端：写出的 JSON 里 error.type 必须是映射值，
// 而不是恒定的 api_error。反事实：把 writeOpenAIError 改回硬编码 api_error 时必红。
func TestWriteOpenAIErrorUsesMappedType(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusBadRequest, "invalid_request_error"},
		{http.StatusUnauthorized, "invalid_request_error"},
		{http.StatusTooManyRequests, "rate_limit_error"},
		{http.StatusBadGateway, "api_error"},
	} {
		rec := httptest.NewRecorder()
		writeOpenAIError(rec, tc.status, "some_code", "boom")
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("bad json: %v", err)
		}
		em, _ := body["error"].(map[string]any)
		if em == nil {
			t.Fatalf("status %d: 缺 error 对象：%s", tc.status, rec.Body.String())
		}
		if em["type"] != tc.want {
			t.Errorf("status %d: error.type=%v want %v", tc.status, em["type"], tc.want)
		}
		// code / message 不得被这次修复改动。
		if em["code"] != "some_code" || em["message"] != "boom" {
			t.Errorf("status %d: code/message 被改动：%v", tc.status, em)
		}
	}
}

// TestWriteOpenAIErrorHintUsesMappedType gateway_hint 变体同样走映射
// （否则带 hint 的错误体会退回恒 api_error，形成两套口径）。
func TestWriteOpenAIErrorHintUsesMappedType(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOpenAIErrorHint(rec, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key", "check the key")
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	em, _ := body["error"].(map[string]any)
	if em == nil {
		t.Fatalf("缺 error 对象：%s", rec.Body.String())
	}
	if em["type"] != "invalid_request_error" {
		t.Errorf("带 hint 的 401 也应为 invalid_request_error，got %v", em["type"])
	}
	if em["gateway_hint"] != "check the key" {
		t.Errorf("gateway_hint 必须保留，got %v", em["gateway_hint"])
	}
}
