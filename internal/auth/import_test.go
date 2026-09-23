package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileNameForAlwaysWhitelisted FileNameFor 产物恒匹配 AuthFileGlob（workbuddy*.json）。
func TestFileNameForAlwaysWhitelisted(t *testing.T) {
	for _, uid := range []string{"u-1", "a.b_c", "", "../evil", "..\\..\\etc\\passwd", "带中文 uid", "a/b/c", " x "} {
		name := FileNameFor(uid)
		if !strings.HasPrefix(name, "workbuddy") || !strings.HasSuffix(name, ".json") {
			t.Errorf("FileNameFor(%q) = %q 不在白名单内", uid, name)
		}
		if strings.ContainsAny(name, `/\`) {
			t.Errorf("FileNameFor(%q) = %q 含路径分隔符（路径穿越风险）", uid, name)
		}
	}
}

// TestFileNameForSanitizes 危险 uid 被净化；空 uid 退化为 unknown。
func TestFileNameForSanitizes(t *testing.T) {
	if got := FileNameFor("../evil"); got != "workbuddy-..evil.json" && got != "workbuddy-evil.json" {
		// 具体净化结果取决于清洗规则：点号在安全字符内，但绝不允许出现斜杠/跳目录。
		t.Errorf("FileNameFor(../evil) = %q", got)
	}
	if got := FileNameFor("  "); got != "workbuddy-unknown.json" {
		t.Errorf("FileNameFor(空) = %q, want workbuddy-unknown.json", got)
	}
	// 超长 uid 截断到 96 字符安全段：workbuddy- + 96 + .json = 111。
	if got := FileNameFor(strings.Repeat("x", 200)); len(got) != len("workbuddy-")+96+len(".json") {
		t.Errorf("超长 uid 应截断到固定上限，got len=%d", len(got))
	}
}

// TestParseImportSingleObject 单个对象形态。
func TestParseImportSingleObject(t *testing.T) {
	accounts, results, err := ParseImport([]byte(`{"accessToken":"at-1","uid":"u-1","nickname":"一号"}`))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	if len(accounts) != 1 || len(results) != 1 {
		t.Fatalf("got %d accounts / %d results, want 1/1", len(accounts), len(results))
	}
	if accounts[0].UID != "u-1" || accounts[0].AccessToken != "at-1" {
		t.Errorf("account = %+v", accounts[0])
	}
	if !results[0].OK {
		t.Errorf("results[0] = %+v, want ok", results[0])
	}
}

// TestParseImportArray 数组形态：缺 accessToken 的条目在解析层即被拒（Parse 校验），
// 但整批不失败——坏条目以失败结果逐条回报，合法条目照常解析。
func TestParseImportArray(t *testing.T) {
	raw := `[
		{"accessToken":"at-1","uid":"u-1","nickname":"一号"},
		{"uid":"u-2"},
		{"accessToken":"at-3","uid":"u-3"}
	]`
	accounts, results, err := ParseImport([]byte(raw))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d, want 2（缺 token 的条目被拒）", len(accounts))
	}
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3（逐条回报）", len(results))
	}
	if results[1].OK || results[1].Error == "" {
		t.Errorf("results[1] = %+v, want failed with error", results[1])
	}
	if !results[0].OK || !results[2].OK {
		t.Errorf("合法条目应 ok: %+v", results)
	}
}

// TestParseImportWrapper {"accounts":[...]} 包装形态。
func TestParseImportWrapper(t *testing.T) {
	raw := `{"accounts":[{"accessToken":"at-1","uid":"u-1"},{"accessToken":"at-2","uid":"u-2"}]}`
	accounts, _, err := ParseImport([]byte(raw))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(accounts))
	}
}

// TestParseImportUIDFallback uid 缺失时用 token 哈希占位（文件名可推导 + 同 token 幂等）。
func TestParseImportUIDFallback(t *testing.T) {
	accounts, _, err := ParseImport([]byte(`[{"accessToken":"at-x"},{"accessToken":"at-x"}]`))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(accounts))
	}
	if accounts[0].UID == "" || accounts[1].UID == "" {
		t.Fatalf("uid 应被占位填充")
	}
	if accounts[0].UID != accounts[1].UID {
		t.Errorf("同 token 应得到同 uid: %q vs %q", accounts[0].UID, accounts[1].UID)
	}
	if !strings.HasPrefix(accounts[0].UID, "tok-") {
		t.Errorf("占位 uid 应以 tok- 开头: %q", accounts[0].UID)
	}
}

// TestParseImportInvalid 整份非法 JSON / 空 body / 空列表都报错。
func TestParseImportInvalid(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(""), []byte("{not json"), []byte(`"str"`), []byte(`{"accounts":[]}`)} {
		if _, _, err := ParseImport(raw); err == nil {
			t.Errorf("ParseImport(%q) 应报错", string(raw))
		}
	}
}

// TestParseImportRealmInference 未显式 realm 时按 domain 推断（cn/global），显式 realm 优先。
func TestParseImportRealmInference(t *testing.T) {
	accounts, _, err := ParseImport([]byte(`[
		{"accessToken":"at-cn","uid":"u-cn","domain":"https://copilot.tencent.com"},
		{"accessToken":"at-gl","uid":"u-gl","domain":"https://www.workbuddy.ai"},
		{"accessToken":"at-x","uid":"u-x","realm":"global","domain":"https://copilot.tencent.com"}
	]`))
	if err != nil {
		t.Fatalf("ParseImport: %v", err)
	}
	want := map[string]string{"u-cn": "cn", "u-gl": "global", "u-x": "global"}
	for _, a := range accounts {
		if got := a.Realm(); got != want[a.UID] {
			t.Errorf("uid=%s realm=%q, want %q", a.UID, got, want[a.UID])
		}
	}
}

// TestSaveAtomicWriteRoundtrip Save(dir) 落盘后可被 Parse/LoadDir 读回，realm 持久化。
func TestSaveAtomicWriteRoundtrip(t *testing.T) {
	dir := t.TempDir()
	a := &Auth{
		AccessToken:  "at-1",
		RefreshToken: "rt-1",
		Domain:       "https://www.workbuddy.ai",
		UID:          "u-1",
		EnterpriseID: "e-1",
		Nickname:     "一号",
	}
	a.SetRealmForImport("global")
	path, err := a.Save(dir)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	want := filepath.Join(dir, "workbuddy-u-1.json")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// 写入时以 0600 打开（POSIX 语义）；Windows 忽略 owner/group 位、只报 0666，
	// 因此两种都允许——重点是文件可读且没有放开 group/other 可写位之外的问题。
	if perm := fi.Mode().Perm(); perm != 0o600 && perm != 0o666 {
		t.Errorf("perm = %o, want 600（Windows 下允许 666）", perm)
	}
	raw, _ := os.ReadFile(path)
	back, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse 读回: %v", err)
	}
	if back.UID != "u-1" || back.AccessToken != "at-1" || back.Realm() != "global" {
		t.Errorf("读回不一致: %+v", back)
	}
	loaded, err := LoadDir(dir)
	if err != nil || len(loaded) != 1 || loaded[0].UID != "u-1" {
		t.Fatalf("LoadDir 应能加载新文件: n=%d err=%v", len(loaded), err)
	}
	if loaded[0].Realm() != "global" {
		t.Errorf("LoadDir realm = %q, want global", loaded[0].Realm())
	}
}

// TestSaveRefusesEmptyToken 空 accessToken 拒绝写入（防空凭证覆盖有效文件）。
func TestSaveRefusesEmptyToken(t *testing.T) {
	a := &Auth{UID: "u-1"}
	if _, err := a.Save(t.TempDir()); err == nil {
		t.Fatal("空 token 应拒绝写入")
	}
}

// TestSetRealmForImport 显式 realm 优先；空值按 domain 推断；非法值不当作 global。
// 注意 fusion 语义：Realm() 由 realm 字段（仅 global 标记）或 domain 兜底——在
// global domain 上显式 cn 不会改变 Realm() 结果（路由仍归 global），这不影响
// OAuth 添加场景：cn 域的 domain 本就非 global，global 域则 domain 与 realm 一致。
func TestSetRealmForImport(t *testing.T) {
	a := &Auth{} // 无 domain → 空值推断 cn
	a.SetRealmForImport("")
	if a.Realm() != "cn" {
		t.Errorf("空值+无 domain 应推断 cn，got %q", a.Realm())
	}
	a.SetRealmForImport("global")
	if a.Realm() != "global" {
		t.Errorf("显式 global 应生效，got %q", a.Realm())
	}
	a.SetRealmForImport("bogus")
	if a.Realm() != "cn" {
		t.Errorf("非法 realm 不应当作 global，got %q", a.Realm())
	}
	b := &Auth{Domain: "https://www.workbuddy.ai"}
	b.SetRealmForImport("")
	if b.Realm() != "global" {
		t.Errorf("global domain 空值应推断 global，got %q", b.Realm())
	}
	// global domain 上显式 cn：Realm() 由 domain 兜底仍为 global（语义见函数注释）。
	b.SetRealmForImport("cn")
	if b.Realm() != "global" {
		t.Errorf("global domain 上显式 cn 由 domain 兜底为 global，got %q", b.Realm())
	}
	c := &Auth{Domain: "https://copilot.tencent.com"}
	c.SetRealmForImport("cn")
	if c.Realm() != "cn" {
		t.Errorf("cn domain 显式 cn 应生效，got %q", c.Realm())
	}
}
