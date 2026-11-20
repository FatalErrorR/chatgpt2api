// Package protocollogin is services/protocol_login_service.py: log in a batch of
// "email----password----TOTP" lines without a browser and add (or update) the accounts.
package protocollogin

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/basketikun/chatgpt2api/internal/openailogin"
	"github.com/basketikun/chatgpt2api/internal/pool"
)

const (
	separator   = "----"
	MaxBatch    = 200
	concurrency = 3
	maxProgress = 20
)

var errorText = map[string]string{
	"invalid_password":                     "密码错误",
	"need_totp_secret":                     "账号开启了二步验证，需要提供 TOTP 密钥",
	"invalid_totp_secret":                  "TOTP 密钥格式无效",
	"need_verification_code":               "需要邮箱验证码，协议登录暂不支持",
	"mfa_factor_not_found":                 "没有找到二步验证方式",
	"mfa_no_auth_code":                     "二步验证通过后没有拿到授权码",
	"rate_limit_exceeded":                  "触发频率限制，请稍后再试",
	"unsupported_country_region_territory": "当前出口地区不受支持，请换代理",
	"no_auth_code":                         "登录后没有拿到授权码",
	"token_exchange_failed":                "授权码换取 token 失败",
}

// Entry is one parsed line.
type Entry struct {
	Line       int
	Email      string
	Password   string
	TOTPSecret string
	Proxy      string
	reserved   bool
}

// ParseLines is parse_account_lines: valid entries and invalid lines (first of each email wins).
func ParseLines(text string) ([]*Entry, []map[string]any) {
	entries, invalid := []*Entry{}, []map[string]any{}
	seen := map[string]bool{}
	for index, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		lineNo := index + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, separator)
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		if len(parts) < 2 {
			invalid = append(invalid, map[string]any{"line": lineNo, "error": "格式应为 邮箱----密码----TOTP密钥"})
			continue
		}
		email := parts[0]
		totpRaw, password := "", parts[1]
		if len(parts) >= 3 {
			totpRaw, password = parts[len(parts)-1], strings.Join(parts[1:len(parts)-1], separator)
		}
		if !strings.Contains(email, "@") || password == "" {
			invalid = append(invalid, map[string]any{"line": lineNo, "email": email, "error": "邮箱或密码为空"})
			continue
		}
		secret := ""
		if totpRaw != "" {
			if secret = openailogin.NormalizeTOTPSecret(totpRaw); secret == "" {
				invalid = append(invalid, map[string]any{"line": lineNo, "email": email, "error": "TOTP 密钥格式无效"})
				continue
			}
		}
		key := strings.ToLower(email)
		if seen[key] {
			invalid = append(invalid, map[string]any{"line": lineNo, "email": email, "error": "邮箱重复"})
			continue
		}
		seen[key] = true
		entries = append(entries, &Entry{Line: lineNo, Email: email, Password: password, TOTPSecret: secret})
	}
	return entries, invalid
}

// DescribeError is describe_login_error.
func DescribeError(result map[string]any) string {
	code := str(result["error"])
	if code == "" {
		code = "unknown"
	}
	if detail, ok := result["detail"].(map[string]any); ok && code == "password_verify_failed_403" {
		if inner, ok := detail["error"].(map[string]any); ok && inner["code"] == "account_deactivated" {
			return "账号已被停用"
		}
	}
	if strings.HasPrefix(code, "mfa_verify_failed_") {
		return "二步验证码被拒绝（" + code[strings.LastIndex(code, "_")+1:] + "）"
	}
	if text, ok := errorText[code]; ok {
		return text
	}
	return code
}

func firstNonEmpty(values ...any) any {
	for _, value := range values {
		if value != nil && value != "" {
			return value
		}
	}
	return nil
}

// Reserver hands out proxies from the proxy list (proxy_list.reserve / release).
type Reserver interface {
	Reserve(candidates []string, count int) ([]string, error)
	Release(urls []string)
}

// Service runs the batches.
type Service struct {
	Pool    *pool.Service
	Proxies Reserver
	Login   pool.LoginFunc

	mu       sync.Mutex
	progress map[string]map[string]any
	running  sync.WaitGroup
}

// Wait blocks until running batches finish (tests).
func (s *Service) Wait() { s.running.Wait() }

// Progress is get_progress.
func (s *Service) Progress(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	progress, ok := s.progress[id]
	if !ok {
		return nil
	}
	out := map[string]any{}
	for key, value := range progress {
		out[key] = value
	}
	items := []any{}
	for _, item := range progress["items"].([]map[string]any) {
		copied := map[string]any{}
		for key, value := range item {
			copied[key] = value
		}
		items = append(items, copied)
	}
	out["items"] = items
	return out
}

// Start is start(): parse, assign proxies, then log in in the background.
func (s *Service) Start(text, proxy string, distribute []string) (map[string]any, error) {
	entries, invalid := ParseLines(text)
	if len(entries) > MaxBatch {
		return nil, fmt.Errorf("一次最多 %d 个账号", MaxBatch)
	}
	if err := s.assignProxies(entries, strings.TrimSpace(proxy), distribute); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	items := make([]map[string]any, len(entries))
	for i, entry := range entries {
		items[i] = map[string]any{"email": entry.Email, "status": "等待", "error": ""}
	}
	status := "running"
	if len(entries) == 0 {
		status = "done"
	}
	s.mu.Lock()
	if s.progress == nil {
		s.progress = map[string]map[string]any{}
	}
	s.progress[id] = map[string]any{"id": id, "status": status, "created_at": float64(time.Now().UnixNano()) / 1e9, "total": len(entries),
		"completed": 0, "success": 0, "failed": 0, "invalid": invalid, "items": items}
	s.purgeLocked()
	s.mu.Unlock()
	if len(entries) > 0 {
		s.running.Add(1)
		go func() {
			defer s.running.Done()
			s.run(id, entries)
		}()
	}
	return map[string]any{"progress_id": id, "total": len(entries), "invalid": invalid}, nil
}

func (s *Service) purgeLocked() {
	if len(s.progress) <= maxProgress {
		return
	}
	ids := make([]string, 0, len(s.progress))
	for id := range s.progress {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return s.progress[ids[i]]["created_at"].(float64) < s.progress[ids[j]]["created_at"].(float64)
	})
	for _, id := range ids[:len(s.progress)-maxProgress] {
		delete(s.progress, id)
	}
}

// assignProxies is _assign_proxies: distributing only gives new emails a reserved proxy; emails
// already in the pool keep theirs.
func (s *Service) assignProxies(entries []*Entry, proxy string, distribute []string) error {
	if len(distribute) == 0 {
		for _, entry := range entries {
			entry.Proxy = proxy
		}
		return nil
	}
	existing := map[string]string{}
	for _, account := range s.Pool.ListAccounts() {
		existing[strings.ToLower(str(account["email"]))] = str(account["proxy"])
	}
	fresh := []*Entry{}
	for _, entry := range entries {
		if _, ok := existing[strings.ToLower(strings.TrimSpace(entry.Email))]; !ok {
			fresh = append(fresh, entry)
		}
	}
	if s.Proxies == nil {
		return fmt.Errorf("代理清单还没有接入")
	}
	assigned, err := s.Proxies.Reserve(distribute, len(fresh))
	if err != nil {
		return err
	}
	for i, entry := range fresh {
		if i < len(assigned) {
			entry.Proxy, entry.reserved = assigned[i], true
		}
	}
	for _, entry := range entries {
		if !entry.reserved {
			entry.Proxy = existing[strings.ToLower(strings.TrimSpace(entry.Email))]
		}
	}
	return nil
}

func (s *Service) release(entry *Entry) {
	s.mu.Lock()
	reserved := entry.reserved
	entry.reserved = false
	s.mu.Unlock()
	if reserved && s.Proxies != nil {
		s.Proxies.Release([]string{entry.Proxy})
	}
}

func (s *Service) setItem(id string, index int, fields map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	progress, ok := s.progress[id]
	if !ok {
		return
	}
	items := progress["items"].([]map[string]any)
	for key, value := range fields {
		items[index][key] = value
	}
	completed, success, failed := 0, 0, 0
	for _, item := range items {
		switch item["status"] {
		case "成功":
			completed++
			success++
		case "失败":
			completed++
			failed++
		}
	}
	progress["completed"], progress["success"], progress["failed"] = completed, success, failed
}

func (s *Service) run(id string, entries []*Entry) {
	defer func() {
		for _, entry := range entries {
			s.release(entry)
		}
		s.mu.Lock()
		if progress, ok := s.progress[id]; ok {
			progress["status"] = "done"
		}
		s.mu.Unlock()
	}()
	slots := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for index, entry := range entries {
		wg.Add(1)
		slots <- struct{}{}
		go func(index int, entry *Entry) {
			defer wg.Done()
			defer func() { <-slots }()
			s.loginOne(id, index, entry)
		}(index, entry)
	}
	wg.Wait()
}

func (s *Service) loginOne(id string, index int, entry *Entry) {
	defer s.release(entry)
	defer func() {
		if r := recover(); r != nil {
			s.setItem(id, index, map[string]any{"status": "失败", "error": truncate(fmt.Sprintf("panic: %v", r), 130)})
		}
	}()
	s.setItem(id, index, map[string]any{"status": "登录中"})
	result, err := s.Login(context.Background(), entry.Email, entry.Password, entry.TOTPSecret, entry.Proxy)
	if err != nil {
		s.setItem(id, index, map[string]any{"status": "失败", "error": "RuntimeError: " + truncate(err.Error(), 120)})
		return
	}
	if result["ok"] != true {
		s.setItem(id, index, map[string]any{"status": "失败", "error": DescribeError(result)})
		return
	}
	token := s.save(entry, result)
	s.setItem(id, index, map[string]any{"status": "成功", "error": "", "token": pool.AnonymizeToken(token)})
}

func truncate(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}

func str(value any) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// save is _save_account: update the account with the same email in place, else add it; then
// release the reservation and refresh the quota.
func (s *Service) save(entry *Entry, result map[string]any) string {
	email := str(result["email"])
	if email == "" {
		email = entry.Email
	}
	sourceType := str(result["source_type"])
	if sourceType == "" {
		sourceType = "password"
	}
	extra := pool.Account{"email": email, "password": entry.Password, "totp_secret": entry.TOTPSecret, "source_type": sourceType, "status": pool.StatusNormal}
	if accountID := str(result["account_id"]); accountID != "" {
		extra["account_id"] = accountID
	}
	if entry.Proxy != "" {
		extra["proxy"] = entry.Proxy
	}
	tokens := map[string]string{"access_token": str(result["access_token"]), "refresh_token": str(result["refresh_token"]), "id_token": str(result["id_token"])}
	var existing pool.Account
	for _, account := range s.Pool.ListAccounts() {
		if strings.EqualFold(str(account["email"]), email) {
			existing = account
			break
		}
	}
	token := tokens["access_token"]
	if existing != nil {
		token = s.Pool.ApplyRefreshedTokens(str(existing["access_token"]), tokens, "protocol_login")
		s.Pool.UpdateAccount(token, extra, true)
	} else {
		item := pool.Account{}
		for key, value := range tokens {
			item[key] = value
		}
		for key, value := range extra {
			item[key] = value
		}
		s.Pool.AddAccountItems([]pool.Account{item})
	}
	s.release(entry)
	s.Pool.RefreshAccounts(context.Background(), []string{token}, "", false)
	return token
}
