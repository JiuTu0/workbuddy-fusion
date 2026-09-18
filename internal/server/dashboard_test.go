// dashboard_test.go 看板行为测试：锁住"未启用零回归 / 启用后鉴权边界 / /api 转发"三条主线，
// 以及三个数据模块（用量统计 / 积分快照 / 调用流水）在 realm 维度上的记录语义。
package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy-fusion/internal/auth"
	"workbuddy-fusion/internal/upstream"
)

// newDashboardHandler 构建启用看板的 handler（数据模块落在临时目录，随测试回收）。
func newDashboardHandler(t *testing.T, user, pass, apiKey string) *Handler {
	t.Helper()
	dir := t.TempDir()
	stats := NewStats(filepath.Join(dir, "stats.json"))
	calls := NewCallTrack(filepath.Join(dir, "call_log.json"))
	credits := NewCreditTrack(filepath.Join(dir, "credits_snapshots.json"), nil)
	t.Cleanup(func() {
		stats.Close()
		calls.Close()
		credits.Close()
	})
	return NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u-1", Nickname: "甲"}),
		Upstream:      upstream.New(),
		APIKey:        apiKey,
		DashboardUser: user,
		DashboardPass: pass,
		Stats:         stats,
		CallTrack:     calls,
		CreditTrack:   credits,
	})
}

// basicHeader 构造 Basic 鉴权头（不用 r.SetBasicAuth：这里直接构造请求）。
func basicHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// doReq 发起一次请求并返回响应。
func doReq(t *testing.T, h *Handler, method, target, authz string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestDashboardDisabledByDefault 未配置看板凭据时：根路径与 /api/ 都不存在，
// 看板数据接口仍可经 api_key 正常访问（行为与无看板版本一致）。
func TestDashboardDisabledByDefault(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u-1"}),
		Upstream: upstream.New(),
		APIKey:   "k",
	})
	if rec := doReq(t, h, http.MethodGet, "/", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("未启用看板时 GET / = %d，期望 404", rec.Code)
	}
	if rec := doReq(t, h, http.MethodGet, "/api/status", basicHeader("a", "b")); rec.Code != http.StatusNotFound {
		t.Fatalf("未启用看板时 GET /api/status = %d，期望 404", rec.Code)
	}
	// 看板数据接口在未启用看板时也注册（与 /status 同级），走 api_key 鉴权。
	rec := doReq(t, h, http.MethodGet, "/stats", "Bearer k")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /stats(api_key) = %d，期望 200", rec.Code)
	}
	var snap StatsSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("GET /stats 响应不是 JSON: %v", err)
	}
	if snap.Total.Requests != 0 {
		t.Fatalf("空统计 Requests = %d，期望 0", snap.Total.Requests)
	}
}

// TestDashboardBasicAuthGate 看板页面与 /api/ 都必须过 Basic 鉴权；
// 页面里的 api_key 不下发：没有 Basic 凭据时一律 401 且带 WWW-Authenticate。
func TestDashboardBasicAuthGate(t *testing.T) {
	h := newDashboardHandler(t, "ops", "s3cret", "gw-key")

	rec := doReq(t, h, http.MethodGet, "/", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据 GET / = %d，期望 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "workbuddy-fusion dashboard") {
		t.Fatalf("WWW-Authenticate = %q，期望含 realm 标识", got)
	}
	if rec := doReq(t, h, http.MethodGet, "/", basicHeader("ops", "wrong")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误密码 GET / = %d，期望 401", rec.Code)
	}
	if rec := doReq(t, h, http.MethodGet, "/api/status", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据 GET /api/status = %d，期望 401", rec.Code)
	}

	rec = doReq(t, h, http.MethodGet, "/", basicHeader("ops", "s3cret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("带凭据 GET / = %d，期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("看板页面 Content-Type = %q", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "workbuddy-fusion") {
		head := body
		if len(head) > 120 {
			head = head[:120]
		}
		t.Fatalf("看板页面未包含项目标识，前 120 字节: %q", head)
	}
}

// TestDashboardAPIForwards 走 /api/ 的请求在剥离前缀后由内部路由处理，
// 且凭据是看板账号（不需要、也不应泄露 api_key）。
func TestDashboardAPIForwards(t *testing.T) {
	h := newDashboardHandler(t, "ops", "s3cret", "gw-key")

	// 直接打内部 /status：仍须 api_key。
	if rec := doReq(t, h, http.MethodGet, "/status", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无 api_key GET /status = %d，期望 401", rec.Code)
	}

	rec := doReq(t, h, http.MethodGet, "/api/status", basicHeader("ops", "s3cret"))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/status = %d，期望 200（body=%s）", rec.Code, rec.Body.String())
	}
	var payload struct {
		Total       int                       `json:"total"`
		RealmTotals map[string]map[string]int `json:"realm_totals"`
		Accounts    []map[string]any          `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("GET /api/status 响应不是 JSON: %v", err)
	}
	for _, realm := range []string{"cn", "global"} {
		if _, ok := payload.RealmTotals[realm]; !ok {
			t.Fatalf("/status 缺 realm_totals[%s]（双域维度必须可见）", realm)
		}
	}

	// /api/ 下的未知路径：明确 404，而不是转发到别的处理器。
	if rec := doReq(t, h, http.MethodGet, "/api/unknown", basicHeader("ops", "s3cret")); rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/unknown = %d，期望 404", rec.Code)
	}
	// 其余看板数据接口经 /api/ 也应可用（均为空数据但结构完整）。
	for _, path := range []string{"/api/stats", "/api/credits/history", "/api/calls?limit=10"} {
		if rec := doReq(t, h, http.MethodGet, path, basicHeader("ops", "s3cret")); rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d，期望 200", path, rec.Code)
		}
	}
}

// TestStatsRecordRealmDimension 用量统计按 模型/账号/域/小时 四维累加；失败请求不计入。
func TestStatsRecordRealmDimension(t *testing.T) {
	s := NewStats("")
	defer s.Close()

	s.Record("m-1", "u-1", "cn", 10, 20, 30, http.StatusOK)
	s.Record("m-1", "u-1", "cn", 1, 2, 0, http.StatusOK) // total 缺失时按 prompt+completion 回落
	s.Record("m-2", "u-2", "global", 5, 5, 10, http.StatusOK)
	s.Record("m-2", "u-2", "global", 100, 100, 200, http.StatusInternalServerError) // 失败路径不计入

	snap := s.Snapshot()
	if snap.Total.Requests != 3 || snap.Total.Total != 43 {
		t.Fatalf("Total = %+v，期望 requests=3 total=43", snap.Total)
	}
	byRealm := map[string]UsageTotals{}
	for _, e := range snap.ByRealm {
		byRealm[e.Name] = e.UsageTotals
	}
	if byRealm["cn"].Total != 33 || byRealm["global"].Total != 10 {
		t.Fatalf("分域统计 = %+v，期望 cn=33 global=10", byRealm)
	}
	if len(snap.ByModel) != 2 || len(snap.ByUID) != 2 || len(snap.Hourly) != 1 {
		t.Fatalf("维度条目数异常：model=%d uid=%d hourly=%d",
			len(snap.ByModel), len(snap.ByUID), len(snap.Hourly))
	}
}

// TestCallTrackRecordAndRecent 调用流水按时间追加，Recent 只回最近 limit 条。
func TestCallTrackRecordAndRecent(t *testing.T) {
	c := NewCallTrack("")
	defer c.Close()

	c.Record("u-1", "甲", "cn", "m-1", "stream", http.StatusOK)
	c.Record("u-2", "乙", "global", "m-2", "sync", http.StatusOK)
	c.Record("", "", "", "", "sync", http.StatusServiceUnavailable) // 无可用账号的失败路径

	if got := c.Count(); got != 3 {
		t.Fatalf("Count = %d，期望 3", got)
	}
	recent := c.Recent(2)
	if len(recent) != 2 || recent[1].Status != http.StatusServiceUnavailable {
		t.Fatalf("Recent(2) = %+v", recent)
	}
	if recent[1].Model != "-" {
		t.Fatalf("模型为空时应落占位符 -，实际 %q", recent[1].Model)
	}
	if all := c.Recent(0); len(all) != 3 || all[0].UID != "u-1" || all[0].Realm != "cn" {
		t.Fatalf("Recent(0) = %+v", all)
	}
	if got := parseCallLimit("", callLogDefaultLimit); got != callLogDefaultLimit {
		t.Fatalf("parseCallLimit(\"\") = %d", got)
	}
	if got := parseCallLimit("-3", callLogDefaultLimit); got != callLogDefaultLimit {
		t.Fatalf("parseCallLimit(-3) = %d", got)
	}
	if got := parseCallLimit("99999999", callLogDefaultLimit); got != callLogRetention {
		t.Fatalf("parseCallLimit 超大值 = %d，期望被钳到 %d", got, callLogRetention)
	}
}

// TestCreditTrackSample 采样合计 = 各账号之和；空明细不写快照（避免假断崖）。
func TestCreditTrackSample(t *testing.T) {
	c := NewCreditTrack("", nil)
	defer c.Close()

	c.Sample() // 采样函数未注入：不写
	if got := len(c.History()); got != 0 {
		t.Fatalf("未注入采样函数时快照数 = %d，期望 0", got)
	}

	c.SetSampler(func() []CreditAccountSnapshot { return nil })
	c.Sample() // 空明细：不写
	if got := len(c.History()); got != 0 {
		t.Fatalf("空采样时快照数 = %d，期望 0", got)
	}

	c.SetSampler(func() []CreditAccountSnapshot {
		return []CreditAccountSnapshot{
			{UID: "u-1", Name: "甲", Realm: "cn", Remain: 700},
			{UID: "u-2", Name: "乙", Realm: "global", Remain: 300},
		}
	})
	c.Sample()
	hist := c.History()
	if len(hist) != 1 {
		t.Fatalf("快照数 = %d，期望 1", len(hist))
	}
	if hist[0].Remain != 1000 || hist[0].Accounts != 2 || !hist[0].OK {
		t.Fatalf("快照 = %+v，期望 remain=1000 accounts=2", hist[0])
	}
	if hist[0].Detail[1].Realm != "global" {
		t.Fatalf("明细未携带 realm：%+v", hist[0].Detail)
	}
}
