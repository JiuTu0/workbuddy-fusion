// info.go 看板「API 接入信息」端点：把第三方接入网关所需的 base_url / api_key /
// 模型使用说明一次性返回给前端展示（看板页面经 /api/info 读取，受看板 Basic Auth
// 保护；直接打 /info 的外部请求仍校验网关 api_key）。
package server

import "net/http"

// info 返回对外接入信息。
//
// base_url 的 host 取自请求 Host 头（含端口）：无论用户从本机、局域网还是反代
// 域名访问看板，展示的都是当前实际可用的地址。协议按 http 推导（网关本体直接
// 对外时即 http；如前面有 TLS 反代，部署方可自行替换为 https，与看板访问方式一致）。
//
// api_key 来自网关 Config.APIKey（config.json 的 api_key 字段），为空表示网关
// 未开启鉴权——此时返回 api_key_set=false 与提示文案，而不是空串误导客户端。
//
// 说明文本（instructions）与 /v1/models 的路由协议保持一致：
//   - CN 模型 ID 带 cn: 前缀、global 带 global: 前缀（见 modelList / resolveModel）；
//   - 裸 model 名默认按 cn 路由（现状零回归）。
func (h *Handler) info(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if host == "" {
		host = "localhost:7863"
	}
	baseURL := "http://" + host + "/v1"

	instructions := []string{
		"1. 网关对外提供 OpenAI 兼容接口：base_url + api_key 可直接填入任何 OpenAI SDK / 客户端（ChatGPT-Next-Web、OpenCat、LobeChat 等）。",
		"2. model 参数填带前缀的账号 ID（cn: 或 global:，如 cn:sliverkiss / global:xxx），前缀决定走 CN 域还是 global 域账号池；不写前缀默认按 CN 路由。",
		"3. 可用模型与完整 ID 列表：GET /v1/models（同样需要 api_key）。",
	}
	if h.cfg.APIKey == "" {
		instructions = append(instructions,
			"4. 当前 api_key 为空：网关未开启鉴权，客户端可不带 Authorization 访问；公网部署强烈建议在 config.json 配置 api_key。")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"base_url":     baseURL,
		"api_key":      h.cfg.APIKey,
		"api_key_set":  h.cfg.APIKey != "",
		"instructions": instructions,
	})
}
