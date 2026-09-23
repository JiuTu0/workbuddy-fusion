package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 导入相关：网页看板「粘贴 JSON / 选文件」批量添加账号的能力。
//
// 设计约束（与 SaveAtomic 同族）：
//   - 文件名永远由 FileNameFor(uid) 推导，不接受外部路径（防路径穿越）；
//   - 写入走 tmp + rename + 0600 原子写（与 SaveAtomic 一致的落盘语义）；
//   - 多形态 JSON（单个对象 / 数组 / {"accounts":[...]} 包装）统一归一化；
//   - realm 语义与现有 Auth 对齐：显式 realm 优先，否则按 domain 后缀推断（cn/global）。

// SetRealmForImport 显式设置账号域（cn/global），供跨包写入未导出 realm 字段
// （网页 OAuth 添加场景：登录所选域优先于 domain 后缀推断，与 auth.Parse 的
// realm 键语义一致）。空值/非法值不改变现状（保持 domain 推断口径）。
func (a *Auth) SetRealmForImport(realm string) {
	r := ResolveRealm(realm, a.Domain)
	if r != "" {
		a.realm = r
	}
}

// ImportResult 单条导入的结果（供看板逐条渲染 成功绿/失败红）。
type ImportResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	File     string `json:"file,omitempty"`
}

// FileNameFor 由 uid 推导受管账号文件名（形如 workbuddy-<uid>.json）。
//
// 安全边界：
//   - 输出恒匹配 AuthFileGlob（workbuddy*.json），这是删除/软删白名单的判据；
//   - uid 只保留安全字符（字母数字 . _ -），路径分隔符/控制符一律剔除，
//     杜绝用 uid 做路径穿越或覆盖其他文件；
//   - uid 为空或清洗后为空 → 退化为 workbuddy-unknown.json（仍在白名单内，
//     不会产生 .deleted 之外的副作用）。
func FileNameFor(uid string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '_', r == '-':
			return r
		default:
			return -1
		}
	}, strings.TrimSpace(uid))
	if clean == "" {
		return "workbuddy-unknown.json"
	}
	if len(clean) > 96 {
		clean = clean[:96]
	}
	return "workbuddy-" + clean + ".json"
}

// ParseImport 解析导入 JSON 的多种形态：
//
//	单个对象             {"accessToken":"...","uid":"..."}
//	数组                 [ {...}, {...} ]
//	包装形               {"accounts":[...]}
//
// 返回解析出的账号列表（每个条目都通过了 accessToken 校验），以及一一对应的
// 结果表（results 与 accounts 同序，调用方按索引回填写盘成败）。
// 整份 JSON 无法解析时返回错误；单条字段非法（缺 token 等）不阻断其他条目，
// 该条会以失败结果出现在 results 里（调用方据此逐条回报）。
func ParseImport(raw []byte) ([]*Auth, []ImportResult, error) {
	if len(raw) == 0 {
		return nil, nil, fmt.Errorf("empty import body")
	}
	var items []json.RawMessage
	var probe json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, nil, fmt.Errorf("import_parse_error: %w", err)
	}
	trimmed := strings.TrimSpace(string(probe))
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(probe, &items); err != nil {
			return nil, nil, fmt.Errorf("import_parse_error: %w", err)
		}
	} else if strings.HasPrefix(trimmed, "{") {
		// 包装形 {"accounts":[...]} 优先；否则视为单个账号对象。
		var wrap struct {
			Accounts []json.RawMessage `json:"accounts"`
		}
		if err := json.Unmarshal(probe, &wrap); err == nil && wrap.Accounts != nil {
			items = wrap.Accounts
		} else {
			items = []json.RawMessage{probe}
		}
	} else {
		return nil, nil, fmt.Errorf("import_parse_error: 不支持的非 JSON 顶层形态")
	}
	if len(items) == 0 {
		return nil, nil, fmt.Errorf("import empty accounts")
	}

	accounts := make([]*Auth, 0, len(items))
	results := make([]ImportResult, 0, len(items))
	for _, it := range items {
		a, err := Parse(it)
		if err != nil {
			results = append(results, ImportResult{OK: false, Error: err.Error()})
			continue
		}
		// realm 语义：显式 realm 或 domain 推断，未标识的按既有 BackfillRealm 规则补。
		if a.RealmStored() == "" {
			a.realm = ResolveRealm("", a.Domain)
		}
		// uid 缺失时用 token 哈希占位：保证文件名可推导且同一 token 幂等。
		if strings.TrimSpace(a.UID) == "" {
			sum := sha256.Sum256([]byte(a.AccessToken))
			a.UID = "tok-" + hex.EncodeToString(sum[:])[:16]
		}
		accounts = append(accounts, a)
		results = append(results, ImportResult{
			UID:      a.UID,
			Nickname: a.Nickname,
			OK:       true,
		})
	}
	return accounts, results, nil
}

// Save 把账号以嵌套形原子写入 dir/FileNameFor(uid)（tmp + rename + 0600），
// 与 SaveAtomic 同一落盘语义，但文件名由调用方目录 + FileNameFor 推导
// （网页添加/导入场景：目标目录在请求里给出，文件名必须白名单可验）。
//
// 返回写盘后的完整路径。accessToken 为空时拒绝写入（防误用空凭证覆盖有效文件）。
func (a *Auth) Save(dir string) (string, error) {
	if strings.TrimSpace(a.AccessToken) == "" {
		return "", fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	name := FileNameFor(a.UID)
	if !strings.HasPrefix(name, "workbuddy") || !strings.HasSuffix(name, ".json") {
		return "", fmt.Errorf("save refused: filename %q 不在白名单内", name)
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir auth dir: %w", err)
	}
	dst := filepath.Join(dir, name)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	a.FilePath = dst
	return dst, nil
}
