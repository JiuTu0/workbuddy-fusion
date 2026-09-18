// dashboard.go 看板页面托管：把 dashboard/index.html 用 embed 打进二进制，
// 由网关本体直接提供页面与数据接口（不需要额外的 nginx 或静态目录）。
//
// 路径设计：页面里的请求全部是相对路径 `api/...`（→ `/api/...`），
// 因此这里把 `/api/` 前缀剥掉后交给网关既有的内部路由处理：
//
//	浏览器                     →  本进程
//	GET  /                      →  看板 HTML（embed）
//	GET  /api/status            →  内部 /status
//	GET  /api/credits/history   →  内部 /credits/history
//
// 鉴权：Go 直接做 HTTP Basic Auth（见 withBasicAuth）。看板账号密码是**独立于**
// 网关 api_key 的另一套凭据——把看板交给运维同事看，不必连带交出 API 密钥。
// 浏览器只在用户输入时携带 Basic 凭据，**网关 api_key 不下发到页面**。
//
// 开关：仅在同时配置了看板账号与密码时启用；未配置时根路径与 /api/ 都不注册，
// 行为与"没有看板"的版本完全一致（零回归，也不会意外开出一个无鉴权的管理页）。
package server

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"net/http"
	"strings"
)

// dashboardHTML 看板单页。用 embed 打进二进制：部署时不需要额外挂载或拷贝静态资源，
// 页面版本随二进制走，不会出现"页面比后端旧"的错配。
//
//go:embed dashboard/index.html
var dashboardHTML []byte

// dashboardEnabled 报告看板是否已启用（账号与密码都配置了才算启用）。
func (h *Handler) dashboardEnabled() bool {
	return h.cfg.DashboardUser != "" && h.cfg.DashboardPass != ""
}

// registerRoot 注册根路径 `GET /{$}` 的**唯一**处理器。
//
// 为什么必须唯一：http.ServeMux 对同一模式重复注册会 panic。根路径在看板启用时是
// 看板入口、未启用时退化成 404（与无此路由时的默认行为一致）。
func (h *Handler) registerRoot() {
	h.mux.HandleFunc("GET /{$}", h.rootHandler)
}

// rootHandler 按看板是否启用分发根路径。
func (h *Handler) rootHandler(w http.ResponseWriter, r *http.Request) {
	if h.dashboardEnabled() {
		h.withBasicAuth(h.dashboardIndex)(w, r)
		return
	}
	// 未启用看板：保持与"未注册该路由"相同的响应（net/http 默认 404 文案）。
	http.Error(w, "404 page not found", http.StatusNotFound)
}

// registerDashboard 注册看板的数据入口 `/api/*`（Basic Auth 保护）。
//
// 根路径 `/` 由 registerRoot 统一处理，见其注释。
func (h *Handler) registerDashboard() {
	if !h.dashboardEnabled() {
		return
	}
	// 前端的 /api/* → 内部路由（剥前缀）。这是唯一给页面用的入口，
	// 同样走 Basic Auth，避免"页面受保护但数据接口裸奔"。
	h.mux.HandleFunc("/api/", h.withBasicAuth(h.dashboardAPI))
}

// dashboardIndex 返回看板 HTML。
func (h *Handler) dashboardIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // 页面随版本变化，别让浏览器留旧副本
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(dashboardHTML)
}

// dashboardAPI 把 /api/<rest> 转发给内部路由处理。
//
// 实现方式：改写 r.URL.Path 后交回 h.mux。这样 /api/status 自然命中已注册的
// /status，不需要为每个端点写一份重复注册（也就少一个"新增端点忘了加 /api 前缀"的漂移点）。
//
// 鉴权衔接：内部路由都包了 withAuth（校验网关 api_key），而浏览器手里只有 Basic 凭据。
// 这里在转发前打上"已通过看板鉴权"的标记，withAuth 见到标记即放行——避免把 api_key
// 硬编码进页面（那会让任何能打开页面的人拿到网关密钥）。安全性由外层的 withBasicAuth
// 保证：能走到这里的一定已通过凭据校验。
func (h *Handler) dashboardAPI(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api")
	if rest == "" || rest == "/" {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown api path")
		return
	}
	r2 := r.Clone(context.WithValue(r.Context(), dashAuthedKey{}, true))
	r2.URL.Path = rest
	h.mux.ServeHTTP(w, r2)
}

// dashAuthedKey 请求上下文里标记"已经过看板 Basic 鉴权"的私有 key 类型。
// 用空结构体而非字符串：避免与其他包的 context key 撞名。
type dashAuthedKey struct{}

// dashAuthed 报告该请求是否已通过看板鉴权。
func dashAuthed(r *http.Request) bool {
	v, _ := r.Context().Value(dashAuthedKey{}).(bool)
	return v
}

// withBasicAuth 对看板相关路径做 HTTP Basic 鉴权。
//
// 为什么不复用 withAuth：withAuth 校验的是网关 api_key（给 API 客户端的），
// 看板登录是**另一套凭据**（dashboard 账号密码）。两者分离的意义：
// 把看板密码给运维同事，不必交出 API 密钥。
func (h *Handler) withBasicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := parseBasicAuth(r.Header.Get("Authorization"))
		if !ok || !h.checkDashboardCreds(user, pass) {
			// WWW-Authenticate 让浏览器弹出原生登录框。
			w.Header().Set("WWW-Authenticate", `Basic realm="workbuddy-fusion dashboard", charset="UTF-8"`)
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// checkDashboardCreds 常量时间比较用户名与密码（防时序侧信道）。
func (h *Handler) checkDashboardCreds(user, pass string) bool {
	uOK := subtle.ConstantTimeCompare([]byte(user), []byte(h.cfg.DashboardUser)) == 1
	pOK := subtle.ConstantTimeCompare([]byte(pass), []byte(h.cfg.DashboardPass)) == 1
	return uOK && pOK
}

// parseBasicAuth 解析 Basic 头；格式非法返回 ok=false。
// 按第一个冒号切分用户与密码（密码本身可含冒号），语义与标准库一致且更直观。
func parseBasicAuth(header string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	s := string(raw)
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}
