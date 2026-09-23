package oauth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeUpstream 模拟上游设备授权三端点，并记录收到的请求以便断言域/来源头。
type fakeUpstream struct {
	mu       sync.Mutex
	pending  bool // true 时 token 端点返回业务 pending（code!=0）
	requests []fakeReq
}

type fakeReq struct {
	method string
	path   string
	origin string
	authz  string
}

func (f *fakeUpstream) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, fakeReq{
		method: r.Method,
		path:   r.URL.RequestURI(),
		origin: r.Header.Get("Origin"),
		authz:  r.Header.Get("Authorization"),
	})
}

func (f *fakeUpstream) setPending(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending = v
}

func (f *fakeUpstream) requestsSnapshot() []fakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeReq, len(f.requests))
	copy(out, f.requests)
	return out
}

func writeFakeJSON(w http.ResponseWriter, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// newFakeUpstream 起一个模拟上游的 httptest server，并把两个 realm 的 base 都指过去。
func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{pending: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/plugin/auth/state", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeFakeJSON(w, map[string]any{
			"code": 0,
			"data": map[string]any{"state": "st-1", "authUrl": "https://auth.example/do?state=st-1"},
		})
	})
	mux.HandleFunc("/v2/plugin/auth/token", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		f.mu.Lock()
		pending := f.pending
		f.mu.Unlock()
		if pending {
			writeFakeJSON(w, map[string]any{"code": 1, "msg": "login ing", "data": map[string]any{}})
			return
		}
		writeFakeJSON(w, map[string]any{
			"code": 0,
			"data": map[string]any{
				"accessToken":  "at-secret-1",
				"refreshToken": "rt-secret-1",
				"expiresIn":    3600,
				"domain":       "https://www.workbuddy.ai",
			},
		})
	})
	mux.HandleFunc("/v2/plugin/login/account", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		writeFakeJSON(w, map[string]any{
			"code": 0,
			"data": map[string]any{"uid": "u-1", "enterpriseId": "e-1", "nickname": "测试号"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	for _, realm := range []string{RealmCN, RealmGlobal} {
		old := SetBaseURLForTest(realm, srv.URL)
		t.Cleanup(func() { SetBaseURLForTest(realm, old) })
	}
	return f
}

// TestEndpointDefaults 双域缺省 base/origin 与任务口径一致（cn/global）。
func TestEndpointDefaults(t *testing.T) {
	cn := defaultEndpoints[RealmCN]
	if cn.base != "https://copilot.tencent.com" || cn.origin != "https://www.codebuddy.cn" {
		t.Errorf("cn endpoint = %+v, want base=copilot.tencent.com origin=codebuddy.cn", cn)
	}
	gl := defaultEndpoints[RealmGlobal]
	if gl.base != "https://www.workbuddy.ai" || gl.origin != "https://www.workbuddy.ai" {
		t.Errorf("global endpoint = %+v, want base/origin=workbuddy.ai", gl)
	}
}

// TestStartLoginUnknownRealm 非法 realm 直接报错（双域白名单边界）。
func TestStartLoginUnknownRealm(t *testing.T) {
	if _, _, err := StartLogin("eu"); err == nil {
		t.Fatal("unknown realm should fail")
	}
	if _, err := PollLogin("eu", "st-1"); err == nil {
		t.Fatal("unknown realm should fail")
	}
}

// TestStartLoginUsesRealmBaseAndOrigin 双域 start 都命中对应 base，且 Origin/Referer 随域切换。
func TestStartLoginUsesRealmBaseAndOrigin(t *testing.T) {
	f := newFakeUpstream(t)
	wantOrigin := map[string]string{
		RealmCN:     "https://www.codebuddy.cn",
		RealmGlobal: "https://www.workbuddy.ai",
	}
	for _, realm := range []string{RealmCN, RealmGlobal} {
		state, authURL, err := StartLogin(realm)
		if err != nil {
			t.Fatalf("StartLogin(%s): %v", realm, err)
		}
		if state != "st-1" || authURL == "" {
			t.Fatalf("StartLogin(%s) = %q,%q want st-1 + authUrl", realm, state, authURL)
		}
		reqs := f.requestsSnapshot()
		last := reqs[len(reqs)-1]
		if last.method != http.MethodPost || last.path != "/v2/plugin/auth/state?platform=CLI" {
			t.Errorf("start(%s) request = %s %s", realm, last.method, last.path)
		}
		if last.origin != wantOrigin[realm] {
			t.Errorf("start(%s) Origin = %q, want %q", realm, last.origin, wantOrigin[realm])
		}
	}
}

// TestPollLoginPending token 端点业务 pending（code!=0）→ ErrPending，轮询方继续等。
func TestPollLoginPending(t *testing.T) {
	f := newFakeUpstream(t)
	f.setPending(true)
	b, err := PollLogin(RealmCN, "st-1")
	if !errors.Is(err, ErrPending) {
		t.Fatalf("PollLogin pending = %v, want ErrPending", err)
	}
	if b != nil {
		t.Fatalf("pending 时不应返回 bundle")
	}
}

// TestPollLoginSuccess 完整流程：token + account 双端点，返回带 Realm 的 Bundle。
func TestPollLoginSuccess(t *testing.T) {
	f := newFakeUpstream(t)
	f.setPending(false)
	b, err := PollLogin(RealmGlobal, "st-1")
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if b == nil {
		t.Fatal("bundle 不应为空")
	}
	if b.AccessToken != "at-secret-1" || b.RefreshToken != "rt-secret-1" {
		t.Errorf("token 未正确解析: %+v", b)
	}
	if b.ExpiresIn != 3600 || b.Domain != "https://www.workbuddy.ai" {
		t.Errorf("expires/domain 未正确解析: %+v", b)
	}
	if b.UID != "u-1" || b.Nickname != "测试号" || b.EnterpriseID != "e-1" {
		t.Errorf("account 未正确解析: %+v", b)
	}
	if b.Realm != RealmGlobal {
		t.Errorf("Realm = %q, want global", b.Realm)
	}
	reqs := f.requestsSnapshot()
	acct := reqs[len(reqs)-1]
	if !strings.HasPrefix(acct.path, "/v2/plugin/login/account?state=") {
		t.Errorf("account 端点路径 = %q", acct.path)
	}
	if !strings.HasPrefix(acct.authz, "Bearer ") {
		t.Errorf("account 端点应带 Bearer 头, authz=%q", acct.authz)
	}
}

// TestPollLoginServerError 5xx 是真失败（不是 pending），调用方应停止轮询。
func TestPollLoginServerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/plugin/auth/token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	old := SetBaseURLForTest(RealmCN, srv.URL)
	t.Cleanup(func() { SetBaseURLForTest(RealmCN, old) })

	_, err := PollLogin(RealmCN, "st-1")
	if err == nil {
		t.Fatal("5xx 应报真错误")
	}
	if errors.Is(err, ErrPending) {
		t.Fatalf("5xx 不应是 ErrPending: %v", err)
	}
}

// TestPollLoginEmptyState 空 state 直接报错，不发起请求。
func TestPollLoginEmptyState(t *testing.T) {
	f := newFakeUpstream(t)
	if _, err := PollLogin(RealmCN, "  "); err == nil {
		t.Fatal("空 state 应报错")
	}
	if n := len(f.requestsSnapshot()); n != 0 {
		t.Fatalf("空 state 不应发请求，实际 %d 个", n)
	}
}
