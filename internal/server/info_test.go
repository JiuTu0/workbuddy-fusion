// info_test.go 「API 接入信息」端点测试：锁住三条主线——未启用看板时不注册
// （外部探测不到 base_url/api_key）、经 /api/info 走看板 Basic 鉴权、直接打
// /info 仍校验网关 api_key；以及 api_key 为空时的提示语义。
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestInfoNotRegisteredWhenDashboardDisabled 未配置看板凭据时 /info 完全不注册：
// 无论带不带 api_key / Basic 凭据都 404（对外探测不到 base_url 与 api_key 信息）。
func TestInfoNotRegisteredWhenDashboardDisabled(t *testing.T) {
	h := NewHandler(Config{
		Pool:   testPoolWith(),
		APIKey: "k",
	})
	for _, tc := range []struct{ authz string }{
		{authz: ""},
		{authz: "Bearer k"},
		{authz: basicHeader("ops", "s3cret")},
	} {
		if rec := doReq(t, h, http.MethodGet, "/info", tc.authz); rec.Code != http.StatusNotFound {
			t.Fatalf("未启用看板 GET /info(authz=%q) = %d，期望 404", tc.authz, rec.Code)
		}
		if rec := doReq(t, h, http.MethodGet, "/api/info", tc.authz); rec.Code != http.StatusNotFound {
			t.Fatalf("未启用看板 GET /api/info(authz=%q) = %d，期望 404", tc.authz, rec.Code)
		}
	}
}

// TestInfoViaDashboardBasicAuth 看板启用后：
//   - /api/info 受看板 Basic Auth 保护，无凭据 401；
//   - 带看板凭据返回 base_url / api_key / 使用说明；
//   - 直接打 /info 的外部请求仍须网关 api_key（dashAuthed 豁免只给 /api/ 转发）。
func TestInfoViaDashboardBasicAuth(t *testing.T) {
	h := newDashboardHandler(t, "ops", "s3cret", "gw-key")

	if rec := doReq(t, h, http.MethodGet, "/api/info", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据 GET /api/info = %d，期望 401", rec.Code)
	}
	if rec := doReq(t, h, http.MethodGet, "/info", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 api_key GET /info = %d，期望 401", rec.Code)
	}

	rec := doReq(t, h, http.MethodGet, "/api/info", basicHeader("ops", "s3cret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/info = %d，期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	var info struct {
		BaseURL      string   `json:"base_url"`
		APIKey       string   `json:"api_key"`
		APIKeySet    bool     `json:"api_key_set"`
		Instructions []string `json:"instructions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("GET /api/info 响应不是 JSON: %v", err)
	}
	if !strings.HasPrefix(info.BaseURL, "http://") || !strings.HasSuffix(info.BaseURL, "/v1") {
		t.Fatalf("base_url = %q，期望形如 http://<host>/v1", info.BaseURL)
	}
	if info.APIKey != "gw-key" || !info.APIKeySet {
		t.Fatalf("api_key = %q set=%v，期望 gw-key / true", info.APIKey, info.APIKeySet)
	}
	if len(info.Instructions) == 0 {
		t.Fatal("instructions 为空，期望包含模型使用说明")
	}
	joined := strings.Join(info.Instructions, "\n")
	for _, want := range []string{"cn:", "global:", "/v1/models", "OpenAI"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("instructions 未包含 %q：%s", want, joined)
		}
	}

	// 直接打内部 /info 走 api_key 鉴权同样可用。
	rec = doReq(t, h, http.MethodGet, "/info", "Bearer gw-key")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /info(api_key) = %d，期望 200", rec.Code)
	}

	// 看板页面本身应已包含「API 接入信息」区块与复制按钮。
	rec = doReq(t, h, http.MethodGet, "/", basicHeader("ops", "s3cret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d，期望 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"API 接入信息", "id=\"apiInfo\"", "data-copy", "api/info"} {
		if !strings.Contains(body, want) {
			t.Fatalf("看板页面未包含 %q（新增区块缺失）", want)
		}
	}
}

// TestInfoEmptyAPIKey 网关未配置 api_key 时：返回 api_key_set=false、api_key 为空，
// 说明文本追加"未开启鉴权"提示，前端据此隐藏复制按钮并展示提示。
func TestInfoEmptyAPIKey(t *testing.T) {
	h := newDashboardHandler(t, "ops", "s3cret", "")

	rec := doReq(t, h, http.MethodGet, "/api/info", basicHeader("ops", "s3cret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/info = %d，期望 200", rec.Code)
	}
	var info struct {
		APIKey       string   `json:"api_key"`
		APIKeySet    bool     `json:"api_key_set"`
		Instructions []string `json:"instructions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if info.APIKeySet || info.APIKey != "" {
		t.Fatalf("api_key = %q set=%v，期望空 / false", info.APIKey, info.APIKeySet)
	}
	joined := strings.Join(info.Instructions, "\n")
	if !strings.Contains(joined, "未开启鉴权") {
		t.Fatalf("api_key 为空时应提示未开启鉴权：%s", joined)
	}
}
