// Package oauth 实现 WorkBuddy 设备授权登录（cn / global 双域）的上游调用。
//
// 由看板「添加账号」（internal/server）与 cmd/login 共用同一份协议实现：
// 上游 token 结构与错误判定只此一处，两条路径行为一致。
//
// 流程（无 PKCE，state 由服务端签发）：
//
//	StartLogin(realm)    → POST /v2/plugin/auth/state?platform=CLI  拿 state + authUrl
//	  用户在浏览器完成登录
//	PollLogin(realm, state) → GET /v2/plugin/auth/token?state=      一次，拿 token bundle
//	                         → GET /v2/plugin/login/account?state=  拿 uid/nickname
//
// 双域：
//
//	cn     → https://copilot.tencent.com（Origin: https://www.codebuddy.cn）
//	global → https://www.workbuddy.ai（Origin: https://www.workbuddy.ai）
//
// 设计约束：
//   - **无状态**：包内不保存 state 与会话，并发由调用方（server 的会话表）负责。
//   - 每次调用独立 cookie jar（多账号登录互不串会话）。
//   - **绝不打印 token**：任何日志/错误信息都不含凭证。
package oauth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"
)

// Realm 常量（与 auth.Realm 的 "cn" / "global" 口径一致）。
const (
	RealmCN     = "cn"
	RealmGlobal = "global"
)

// realmEndpoint 单域的上游配置：base 是 API 根，origin 是浏览器来源头。
type realmEndpoint struct {
	base   string
	origin string
}

// defaultEndpoints 双域缺省上游端点。生产恒为这些值；测试经 SetBaseURLForTest 覆盖。
var defaultEndpoints = map[string]realmEndpoint{
	RealmCN:     {base: "https://copilot.tencent.com", origin: "https://www.codebuddy.cn"},
	RealmGlobal: {base: "https://www.workbuddy.ai", origin: "https://www.workbuddy.ai"},
}

// testOverrides 仅供测试覆盖的 base 覆盖表（key=realm，value=base）。
var testOverrides = struct {
	sync.RWMutex
	m map[string]string
}{m: map[string]string{}}

// SetBaseURLForTest 覆盖某 realm 的上游 base 并返回旧值，仅供测试使用。
// 传空字符串清除覆盖。生产代码不得调用——它是包级可变状态，会串到并发用例上。
func SetBaseURLForTest(realm, u string) string {
	testOverrides.Lock()
	defer testOverrides.Unlock()
	old, _ := testOverrides.m[realm]
	if u == "" {
		delete(testOverrides.m, realm)
		return old
	}
	testOverrides.m[realm] = u
	return old
}

// endpoint 解析某 realm 的上游配置；未知 realm 返回错误。
func endpoint(realm string) (realmEndpoint, error) {
	def, ok := defaultEndpoints[realm]
	if !ok {
		return realmEndpoint{}, fmt.Errorf("unknown realm %q（仅支持 cn / global）", realm)
	}
	testOverrides.RLock()
	override := testOverrides.m[realm]
	testOverrides.RUnlock()
	if override != "" {
		def.base = override
	}
	return def, nil
}

// clientUA 设备授权协议要求的客户端标识（与 CLI 出站口径一致）。
const clientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

// 端点路径（相对 base）。
const (
	pathAuthState = "/v2/plugin/auth/state?platform=CLI"
	pathLoginAcct = "/v2/plugin/login/account?state="
	pathAuthToken = "/v2/plugin/auth/token?state="
)

// timeout 单次上游调用总时长上限（登录是交互流程，给足余量）。
const timeout = 30 * time.Second

// ErrPending 表示用户尚未在浏览器完成授权（上游 code!=0 或 token 为空）。
// 调用方据此区分「还在等」与「真失败」——前者应继续轮询，后者应停止。
var ErrPending = errors.New("login not finished (waiting for user)")

// Bundle 一次成功登录的完整凭证。
type Bundle struct {
	AccessToken  string
	RefreshToken string
	Domain       string
	ExpiresIn    int64
	UID          string
	EnterpriseID string
	Nickname     string
	Realm        string // 本次登录所选域（cn/global）
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// newClient 每个流程独立 cookie jar（多账号登录互不串会话）。
func newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Timeout: timeout, Jar: jar}
}

// commonHeaders 通用请求头（与网关出站口径一致；origin 按域注入）。
func commonHeaders(origin string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", clientUA)
	}
}

// doJSON 发请求并解信封。返回 (data, httpStatus, err)。
// code!=0 时返回错误（错误信息只含上游 code/msg，不含任何凭证）。
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// StartLogin 发起一次设备授权（指定域）：返回 state 与供用户打开的 authUrl。
// state 是后续 PollLogin 的唯一凭据，**由调用方负责保存与过期清理**。
func StartLogin(realm string) (state, authURL string, err error) {
	ep, err := endpoint(realm)
	if err != nil {
		return "", "", err
	}
	client := newClient()
	data, _, err := doJSON(client, http.MethodPost, ep.base+pathAuthState, commonHeaders(ep.origin), bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", "", fmt.Errorf("auth state failed: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", "", fmt.Errorf("auth state: missing state or authUrl")
	}
	return st.State, st.AuthURL, nil
}

// PollLogin 查询一次登录结果（指定域）。
//
// 返回：
//   - (*Bundle, nil)      —— 登录完成
//   - (nil, ErrPending)   —— 尚未完成，调用方应继续轮询
//   - (nil, 其他 error)   —— 真失败（网络/上游 5xx），调用方应停止并报错
//
// 语义依据：上游 auth/token 是权威登录状态端点，pending 时业务 code 非 0
// （"login ing"），完成时 code=0 且带 token bundle。故 4xx 视作 pending
// （用户还没登完），5xx/网络错误视作真失败。
func PollLogin(realm, state string) (*Bundle, error) {
	if strings.TrimSpace(state) == "" {
		return nil, fmt.Errorf("empty state")
	}
	ep, err := endpoint(realm)
	if err != nil {
		return nil, err
	}
	client := newClient()
	tokRaw, status, errTok := doJSON(client, http.MethodGet, ep.base+pathAuthToken+state, commonHeaders(ep.origin), nil)
	if errTok != nil {
		// status==0（传输层失败）或 5xx → 真失败；其余（4xx，含上游 code!=0）→ 仍在等待。
		if status == 0 || status >= 500 {
			return nil, fmt.Errorf("token endpoint error: %w", errTok)
		}
		return nil, ErrPending
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return nil, ErrPending
	}

	// login/account 拿 uid/nickname（带 Bearer）。此步失败不阻断登录：
	// token 已拿到即可用，uid 缺失时由调用方兜底（如用 token 哈希占位）。
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctHeaders := func(r *http.Request) {
		commonHeaders(ep.origin)(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := doJSON(client, http.MethodGet, ep.base+pathLoginAcct+state, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}

	return &Bundle{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Domain:       tok.Domain,
		ExpiresIn:    tok.ExpiresIn,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
		Realm:        realm,
	}, nil
}
