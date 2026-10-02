package prism

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// 凭据：账号 = 一份浏览器 Cookie + 身份。
// 认证不是 Bearer，而是 cookie 集合（cf_clearance 与出口 IP + UA 绑定）。
// ============================================================================

// Account 是一个上游账号（一份浏览器会话）。
type Account struct {
	ID        string `json:"id"`
	Cookie    string `json:"cookie"`     // 原始 Cookie header 串
	UserAgent string `json:"user_agent"` // 必须与 cf_clearance 绑定的一致
	ProjectID string `json:"project_id"` // 可选：浏览器会话里的项目 id
	UserID    string `json:"user_id"`    // 可选：浏览器会话里的 userId
	Label     string `json:"label"`      // 可读标签（邮箱/备注）

	// 以下为运行时状态，不参与 JSON。
	mu          sync.RWMutex
	sessionTok  string    // prism_session_token（由 prism_oai_access_token 换取）
	sessionAt   time.Time // 上次刷新时间
	sessionOK   bool
	cooldownTil time.Time // 401/403 后的冷却截止
	errCount    int
	lastErr     string
}

// CookieKV 是从 Cookie 串里解析出的键值对。
type CookieKV struct {
	Name  string
	Value string
}

// ParseCookies 解析 Cookie header 串（容忍 "a=1; b=2" 与换行分隔两种形态）。
func ParseCookies(raw string) []CookieKV {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	raw = strings.ReplaceAll(raw, "\n", ";")
	var out []CookieKV
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		i := strings.Index(part, "=")
		if i <= 0 {
			continue
		}
		name := strings.TrimSpace(part[:i])
		val := strings.TrimSpace(part[i+1:])
		if name == "" {
			continue
		}
		out = append(out, CookieKV{Name: name, Value: val})
	}
	return out
}

// CookieHeader 归一化 Cookie 串（去重 + 统一 "; " 分隔）。
func CookieHeader(raw string) string {
	seen := map[string]bool{}
	var parts []string
	for _, kv := range ParseCookies(raw) {
		if seen[kv.Name] {
			continue
		}
		seen[kv.Name] = true
		parts = append(parts, kv.Name+"="+kv.Value)
	}
	return strings.Join(parts, "; ")
}

// MissingCookies 返回缺失的关键 cookie 名（缺 oai-sc 会 401）。
func MissingCookies(raw string) []string {
	have := map[string]bool{}
	for _, kv := range ParseCookies(raw) {
		have[strings.ToLower(kv.Name)] = true
	}
	var miss []string
	for _, n := range CookieNames {
		if !have[strings.ToLower(n)] {
			miss = append(miss, n)
		}
	}
	return miss
}

// CookieValue 取某个 cookie 的值。
func CookieValue(raw, name string) string {
	for _, kv := range ParseCookies(raw) {
		if strings.EqualFold(kv.Name, name) {
			return kv.Value
		}
	}
	return ""
}

// ---------------------------------------------------------------- 账号运行时状态

// UserIDOr 返回已知的 userId，回退到账号 id。
func (a *Account) UserIDOr() string {
	if strings.TrimSpace(a.UserID) != "" {
		return a.UserID
	}
	return a.ID
}

// InCooldown 报告账号是否处于错误冷却窗口。
func (a *Account) InCooldown(now time.Time) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return now.Before(a.cooldownTil)
}

// NoteErr 记录一次失败；认证类错误触发冷却，避免把坏账号打爆。
func (a *Account) NoteErr(msg string, cooldown time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errCount++
	a.lastErr = msg
	if cooldown > 0 {
		a.cooldownTil = time.Now().Add(cooldown)
	}
}

// NoteOK 清空失败计数。
func (a *Account) NoteOK() {
	a.mu.Lock()
	a.errCount = 0
	a.lastErr = ""
	a.cooldownTil = time.Time{}
	a.mu.Unlock()
}

// Health 返回健康快照（观测用）。
func (a *Account) Health() (errCount int, lastErr string, cooling bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.errCount, a.lastErr, time.Now().Before(a.cooldownTil)
}

// SessionToken 取缓存的 session token 与新鲜度。
func (a *Account) SessionToken(ttl time.Duration, now time.Time) (string, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.sessionOK || a.sessionTok == "" {
		return "", false
	}
	if ttl > 0 && now.Sub(a.sessionAt) > ttl {
		return "", false
	}
	return a.sessionTok, true
}

// SetSessionToken 写入刷新后的 session token。
func (a *Account) SetSessionToken(tok string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessionTok = tok
	a.sessionAt = time.Now()
	a.sessionOK = tok != ""
}

// ---------------------------------------------------------------- 凭据装载

// CredentialsFile 是 accounts.json 的结构。
type CredentialsFile struct {
	Accounts []Account `json:"accounts"`
}

// LoadAccounts 从文件或环境变量装载账号池。
// 优先顺序：显式文件 → PG_ACCOUNTS_FILE → PG_COOKIES（裸 cookie 串，逗号分隔）。
func LoadAccounts(file string, inlineCookies []string) ([]*Account, error) {
	if strings.TrimSpace(file) == "" {
		file = os.Getenv("PG_ACCOUNTS_FILE")
	}
	if strings.TrimSpace(file) != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("credentials: read %s: %w", file, err)
		}
		// 兼容两种形态：{"accounts":[...]} 或裸数组 [...]
		var cf CredentialsFile
		if err := json.Unmarshal(b, &cf); err != nil || len(cf.Accounts) == 0 {
			var arr []Account
			if err2 := json.Unmarshal(b, &arr); err2 != nil {
				return nil, fmt.Errorf("credentials: parse %s: %w", file, err)
			}
			cf.Accounts = arr
		}
		return buildAccounts(cf.Accounts)
	}
	if len(inlineCookies) > 0 {
		accts := make([]Account, 0, len(inlineCookies))
		for i, ck := range inlineCookies {
			accts = append(accts, Account{ID: fmt.Sprintf("acct-%d", i+1), Cookie: ck})
		}
		return buildAccounts(accts)
	}
	return nil, fmt.Errorf("credentials: 未提供账号（设置 PG_ACCOUNTS_FILE 或 PG_COOKIES）")
}

// buildAccounts 归一化并校验账号。
func buildAccounts(in []Account) ([]*Account, error) {
	out := make([]*Account, 0, len(in))
	for i := range in {
		a := &in[i]
		a.Cookie = CookieHeader(a.Cookie)
		if strings.TrimSpace(a.Cookie) == "" {
			return nil, fmt.Errorf("credentials: 账号 %d 的 cookie 为空", i+1)
		}
		if strings.TrimSpace(a.ID) == "" {
			a.ID = fmt.Sprintf("acct-%d", i+1)
		}
		if strings.TrimSpace(a.UserAgent) == "" {
			a.UserAgent = defaultUserAgent
		}
		out = append(out, a)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("credentials: 账号列表为空")
	}
	return out, nil
}

// defaultUserAgent 与 config 默认值保持一致（cf_clearance 绑定 UA）。
const defaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36 Edg/152.0.0.0"

// ValidateAccount 静态检查一个账号是否具备关键 cookie，返回缺失清单。
func ValidateAccount(a *Account) []string { return MissingCookies(a.Cookie) }
