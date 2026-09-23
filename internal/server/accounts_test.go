package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"workbuddy-fusion/internal/auth"
	"workbuddy-fusion/internal/oauth"
	"workbuddy-fusion/internal/pool"
	"workbuddy-fusion/internal/upstream"
)

// newAccountsHandler 构造一个启用了账号管理（AuthDir 非空）的 Handler。
func newAccountsHandler(t *testing.T, apiKey, authDir string, p *pool.Pool) *Handler {
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
	if p == nil {
		p = testPoolWith()
	}
	h := NewHandler(Config{
		Pool:          p,
		Upstream:      upstream.New(),
		APIKey:        apiKey,
		AuthDir:       authDir,
		Stats:         stats,
		CallTrack:     calls,
		CreditTrack:   credits,
		DashboardUser: "dash",
		DashboardPass: "pass",
	})
	if h.logins != nil {
		t.Cleanup(h.logins.Close)
	}
	return h
}

// doReqBody 带 JSON body 的请求辅助。
func doReqBody(t *testing.T, h *Handler, method, target, authz string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeJSON 解响应体为 map（不关心具体字段类型）。
func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rec.Body.String())
	}
	return m
}

// ---------------------------------------------------------------------------
// 鉴权边界
// ---------------------------------------------------------------------------

// TestAccountsEndpointsAuthBoundary 未配 auth_dir 时不注册路由；配了但缺 api_key 时 401。
func TestAccountsEndpointsAuthBoundary(t *testing.T) {
	// 未配 AuthDir：/accounts/* 路由不注册 → 404（不会 panic）。
	hNoDir := newAccountsHandler(t, "k", "", nil)
	for _, tc := range []struct{ method, target string }{
		{"POST", "/accounts/login/start"},
		{"GET", "/accounts/login/poll?id=x"},
		{"POST", "/accounts/import"},
		{"POST", "/accounts/u-1/revive"},
		{"DELETE", "/accounts/u-1"},
	} {
		rec := doReqBody(t, hNoDir, tc.method, tc.target, "Bearer k", []byte(`{}`))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s (无 auth_dir) = %d, want 404", tc.method, tc.target, rec.Code)
		}
	}

	// 配了 AuthDir：无凭据 / 错误凭据 → 401；正确凭据 → 放行到业务逻辑。
	h := newAccountsHandler(t, "k", t.TempDir(), nil)
	rec := doReqBody(t, h, "POST", "/accounts/login/start", "", []byte(`{}`))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("无凭据 = %d, want 401", rec.Code)
	}
	rec = doReqBody(t, h, "POST", "/accounts/login/start", "Bearer wrong", []byte(`{}`))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("错误凭据 = %d, want 401", rec.Code)
	}
	rec = doReqBody(t, h, "POST", "/accounts/login/start", "Bearer k", []byte(`{}`))
	if rec.Code == http.StatusUnauthorized {
		t.Errorf("正确凭据不应 401")
	}
}

// ---------------------------------------------------------------------------
// 批量导入
// ---------------------------------------------------------------------------

// TestAccountsImportShapesAndPerRow 三种 JSON 形态 + 逐条回报 + 响应无 token。
func TestAccountsImportShapesAndPerRow(t *testing.T) {
	authDir := t.TempDir()
	p := testPoolWith()
	h := newAccountsHandler(t, "k", authDir, p)

	// 单个对象
	rec := doReqBody(t, h, "POST", "/accounts/import", "Bearer k",
		[]byte(`{"accessToken":"at-1","uid":"u-1","nickname":"一号"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("single = %d body=%s", rec.Code, rec.Body.String())
	}
	m := decodeJSON(t, rec)
	if m["imported"] != float64(1) || m["failed"] != float64(0) || m["total"] != float64(1) {
		t.Errorf("single 统计 = %+v", m)
	}
	if p.AuthByUID("u-1") == nil {
		t.Error("导入后池中应存在 u-1")
	}

	// 数组：中间一条缺 token 失败，其余成功（部分成功）
	rec = doReqBody(t, h, "POST", "/accounts/import", "Bearer k",
		[]byte(`[
			{"accessToken":"at-2","uid":"u-2"},
			{"uid":"u-bad"},
			{"accessToken":"at-3","uid":"u-3"}
		]`))
	if rec.Code != http.StatusOK {
		t.Fatalf("array = %d body=%s", rec.Code, rec.Body.String())
	}
	m = decodeJSON(t, rec)
	if m["imported"] != float64(2) || m["failed"] != float64(1) || m["total"] != float64(3) {
		t.Errorf("array 统计 = %+v", m)
	}
	if p.AuthByUID("u-2") == nil || p.AuthByUID("u-3") == nil {
		t.Error("合法条目应入池")
	}
	if p.AuthByUID("u-bad") != nil {
		t.Error("坏条目不应入池")
	}

	// 包装形
	rec = doReqBody(t, h, "POST", "/accounts/import", "Bearer k",
		[]byte(`{"accounts":[{"accessToken":"at-4","uid":"u-4"},{"accessToken":"at-5","uid":"u-5"}]}`))
	m = decodeJSON(t, rec)
	if m["imported"] != float64(2) || m["total"] != float64(2) {
		t.Errorf("wrapper 统计 = %+v", m)
	}

	// 响应体绝不含 token
	all := rec.Body.String()
	for _, tok := range []string{"at-1", "at-2", "at-3", "at-4", "at-5"} {
		if strings.Contains(all, tok) {
			t.Errorf("响应泄露 token %q: %s", tok, all)
		}
	}
}

// TestAccountsImportMalformed 非法 JSON → 400 import_parse_failed。
func TestAccountsImportMalformed(t *testing.T) {
	h := newAccountsHandler(t, "k", t.TempDir(), nil)
	rec := doReqBody(t, h, "POST", "/accounts/import", "Bearer k", []byte(`{not json`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed = %d, want 400", rec.Code)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "import_parse_failed") {
		t.Errorf("error code 不匹配: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// OAuth 双域添加（start + poll）
// ---------------------------------------------------------------------------

// fakeOAuth 模拟上游三端点；token 端点是否 pending 可切换。
type fakeOAuth struct {
	mu      sync.Mutex
	pending bool
	reqs    []string
}

func (f *fakeOAuth) setPending(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = v
}

func (f *fakeOAuth) record(line string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, line)
}

func writeOAuthJSON(w http.ResponseWriter, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func newFakeOAuthForServer(t *testing.T) *fakeOAuth {
	t.Helper()
	f := &fakeOAuth{pending: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/plugin/auth/state", func(w http.ResponseWriter, r *http.Request) {
		f.record("state " + r.URL.RequestURI() + " origin=" + r.Header.Get("Origin"))
		writeOAuthJSON(w, map[string]any{
			"code": 0,
			"data": map[string]any{"state": "st-oauth-1", "authUrl": "https://auth.example/do"},
		})
	})
	mux.HandleFunc("/v2/plugin/auth/token", func(w http.ResponseWriter, r *http.Request) {
		f.record("token " + r.URL.RequestURI())
		f.mu.Lock()
		pending := f.pending
		f.mu.Unlock()
		if pending {
			writeOAuthJSON(w, map[string]any{"code": 1, "msg": "login ing", "data": map[string]any{}})
			return
		}
		writeOAuthJSON(w, map[string]any{
			"code": 0,
			"data": map[string]any{
				"accessToken":  "at-oauth-secret",
				"refreshToken": "rt-oauth-secret",
				"expiresIn":    3600,
				"domain":       "https://www.workbuddy.ai",
			},
		})
	})
	mux.HandleFunc("/v2/plugin/login/account", func(w http.ResponseWriter, r *http.Request) {
		f.record("acct " + r.URL.RequestURI())
		writeOAuthJSON(w, map[string]any{
			"code": 0,
			"data": map[string]any{"uid": "u-oauth-1", "enterpriseId": "e-1", "nickname": "授权号"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	for _, realm := range []string{oauth.RealmCN, oauth.RealmGlobal} {
		old := oauth.SetBaseURLForTest(realm, srv.URL)
		t.Cleanup(func() { oauth.SetBaseURLForTest(realm, old) })
	}
	return f
}

// TestAccountsOAuthStartRealm 双域 start：global 命中 global base；未知 realm 400；
// 响应不含 state。
func TestAccountsOAuthStartRealm(t *testing.T) {
	fake := newFakeOAuthForServer(t)
	authDir := t.TempDir()
	h := newAccountsHandler(t, "k", authDir, testPoolWith())

	rec := doReqBody(t, h, "POST", "/accounts/login/start", "Bearer k", []byte(`{"realm":"global"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("start global = %d body=%s", rec.Code, rec.Body.String())
	}
	m := decodeJSON(t, rec)
	if m["realm"] != "global" || m["id"] == "" || m["auth_url"] == "" {
		t.Errorf("start 响应 = %+v", m)
	}
	if strings.Contains(rec.Body.String(), "st-oauth-1") {
		t.Error("state 不应出现在响应中")
	}
	joined := strings.Join(fake.reqs, "\n")
	if !strings.Contains(joined, "state /v2/plugin/auth/state?platform=CLI origin=https://www.workbuddy.ai") {
		t.Errorf("global start 应命中 global base/origin，实际请求:\n%s", joined)
	}

	// 未知 realm
	rec = doReqBody(t, h, "POST", "/accounts/login/start", "Bearer k", []byte(`{"realm":"eu"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown realm = %d, want 400", rec.Code)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "bad_realm") {
		t.Errorf("error code 不匹配: %s", rec.Body.String())
	}
}

// TestAccountsOAuthPollFlow pending → done：先落盘后入池，响应与日志无 token。
func TestAccountsOAuthPollFlow(t *testing.T) {
	fake := newFakeOAuthForServer(t)
	authDir := t.TempDir()
	p := testPoolWith()
	h := newAccountsHandler(t, "k", authDir, p)

	rec := doReqBody(t, h, "POST", "/accounts/login/start", "Bearer k", []byte(`{"realm":"global"}`))
	m := decodeJSON(t, rec)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("start 未返回 id: %+v", m)
	}

	// 第一次轮询：pending
	rec = doReqBody(t, h, "GET", "/accounts/login/poll?id="+id, "Bearer k", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll = %d body=%s", rec.Code, rec.Body.String())
	}
	m = decodeJSON(t, rec)
	if m["status"] != "pending" {
		t.Errorf("首次轮询 status = %v, want pending", m["status"])
	}

	// 上游完成 → 第二次轮询：done，先落盘后入池
	fake.setPending(false)
	rec = doReqBody(t, h, "GET", "/accounts/login/poll?id="+id, "Bearer k", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll done = %d body=%s", rec.Code, rec.Body.String())
	}
	m = decodeJSON(t, rec)
	if m["status"] != "done" {
		t.Fatalf("poll status = %v, want done (body=%s)", m["status"], rec.Body.String())
	}
	acct, _ := m["account"].(map[string]any)
	if acct == nil || acct["uid"] != "u-oauth-1" || acct["realm"] != "global" {
		t.Errorf("account = %+v", acct)
	}
	if strings.Contains(rec.Body.String(), "at-oauth-secret") || strings.Contains(rec.Body.String(), "rt-oauth-secret") {
		t.Error("轮询响应泄露 token")
	}
	// 先落盘：auths/ 下存在 workbuddy-u-oauth-1.json
	if _, err := os.Stat(filepath.Join(authDir, "workbuddy-u-oauth-1.json")); err != nil {
		t.Errorf("凭证文件未落盘: %v", err)
	}
	// 后入池
	if p.AuthByUID("u-oauth-1") == nil {
		t.Error("账号应已入池")
	}

	// 会话终结后轮询幂等：仍是 done，且不再调用上游
	rec = doReqBody(t, h, "GET", "/accounts/login/poll?id="+id, "Bearer k", nil)
	m = decodeJSON(t, rec)
	if m["status"] != "done" {
		t.Errorf("终结会话 status = %v, want done", m["status"])
	}

	// 无效会话 id
	rec = doReqBody(t, h, "GET", "/accounts/login/poll?id=no-such", "Bearer k", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("无效会话 = %d, want 404", rec.Code)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "session_expired") {
		t.Errorf("error code 不匹配: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 软删除
// ---------------------------------------------------------------------------

// TestAccountsDeleteSoftDelete 删除 = 移入 .deleted/（时间戳前缀）+ 池内移除 + 非白名单文件不动。
func TestAccountsDeleteSoftDelete(t *testing.T) {
	authDir := t.TempDir()
	// 预置一个受管账号文件
	a := &auth.Auth{AccessToken: "at-1", UID: "u-1", Nickname: "一号"}
	if _, err := a.Save(authDir); err != nil {
		t.Fatalf("预置账号: %v", err)
	}
	// 预置一个不受管文件（模拟用户放进 auths/ 的其他文件）
	otherPath := filepath.Join(authDir, "notworkbuddy.json")
	if err := os.WriteFile(otherPath, []byte(`{"x":1}`), 0o600); err != nil {
		t.Fatalf("预置不受管文件: %v", err)
	}

	p := testPoolWith(&auth.Auth{AccessToken: "at-1", UID: "u-1"})
	h := newAccountsHandler(t, "k", authDir, p)

	rec := doReqBody(t, h, "DELETE", "/accounts/u-1", "Bearer k", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d body=%s", rec.Code, rec.Body.String())
	}
	m := decodeJSON(t, rec)
	if m["deleted"] != true || m["uid"] != "u-1" {
		t.Errorf("delete 响应 = %+v", m)
	}
	dst, _ := m["file"].(string)
	if dst == "" || !strings.Contains(dst, ".deleted") {
		t.Errorf("回收站路径异常: %v", m["file"])
	}
	if !strings.Contains(filepath.Base(dst), "workbuddy-u-1.json") {
		t.Errorf("回收站文件名应保留原名: %s", dst)
	}
	// 原文件已移走
	if _, err := os.Stat(filepath.Join(authDir, "workbuddy-u-1.json")); !os.IsNotExist(err) {
		t.Errorf("原文件应已移走: %v", err)
	}
	// 不受管文件不受影响
	if _, err := os.Stat(otherPath); err != nil {
		t.Errorf("非白名单文件不应被动: %v", err)
	}
	// 池内已移除
	if _, ok := p.Status("u-1"); ok {
		t.Error("账号应已从池中移除")
	}

	// 删除不存在的账号（无文件且不在池）→ 404
	rec = doReqBody(t, h, "DELETE", "/accounts/ghost", "Bearer k", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("delete ghost = %d, want 404", rec.Code)
	}
}

// TestAccountsDeleteSanitizesUID uid 中的非法字符被净化，不接受外部路径。
func TestAccountsDeleteSanitizesUID(t *testing.T) {
	authDir := t.TempDir()
	h := newAccountsHandler(t, "k", authDir, testPoolWith())
	// 恶意 uid 经 FileNameFor 净化后落为 workbuddy-..evil.json 之类的白名单名；
	// 该文件不存在 → 404，绝不触碰 authDir 之外。
	rec := doReqBody(t, h, "DELETE", "/accounts/..%2F..%2Fevil", "Bearer k", nil)
	if rec.Code == http.StatusOK {
		t.Errorf("恶意 uid 不应删除成功: %d %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// 复活
// ---------------------------------------------------------------------------

// TestAccountsRevive 禁用账号可复活；未禁用幂等；未知 404。
func TestAccountsRevive(t *testing.T) {
	authDir := t.TempDir()
	p := testPoolWith(&auth.Auth{AccessToken: "at-1", UID: "u-1"})
	h := newAccountsHandler(t, "k", authDir, p)

	// 未禁用：changed=false
	rec := doReqBody(t, h, "POST", "/accounts/u-1/revive", "Bearer k", nil)
	m := decodeJSON(t, rec)
	if m["changed"] != false || m["disabled"] != false {
		t.Errorf("未禁用 revive = %+v", m)
	}

	// 禁用后复活：changed=true
	p.Disable("u-1", "测试禁用")
	rec = doReqBody(t, h, "POST", "/accounts/u-1/revive", "Bearer k", nil)
	m = decodeJSON(t, rec)
	if m["changed"] != true || m["disabled"] != false {
		t.Errorf("禁用 revive = %+v", m)
	}
	if st, ok := p.Status("u-1"); !ok || st.Disabled {
		t.Errorf("复活后应可用: %+v ok=%v", st, ok)
	}

	// 未知账号 → 404
	rec = doReqBody(t, h, "POST", "/accounts/ghost/revive", "Bearer k", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown revive = %d, want 404", rec.Code)
	}
}
