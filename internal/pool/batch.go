package pool

import (
	"context"
	"strings"
	"sync"
)

// IsTLSConnectionError is services/protocol/conversation.py is_tls_connection_error, plus the Go
// spellings of the same failures (the Python texts come from curl).
func IsTLSConnectionError(message string) bool {
	text := strings.ToLower(message)
	for _, fragment := range []string{
		"curl: (35)", "tls connect error", "openssl_internal", "ssl: wrong_version_number", "ssl: certificate_verify_failed",
		"connection aborted", "remote disconnected", "connection reset by peer",
		"tls: handshake failure", "remote error: tls", "tls: first record does not look like",
	} {
		if strings.Contains(text, fragment) {
			return true
		}
	}
	return false
}

func uniqueTokens(tokens []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, token := range tokens {
		if token != "" && !seen[token] {
			seen[token] = true
			out = append(out, token)
		}
	}
	return out
}

// ---- progress (in memory, like the Python class attributes) ----

// InitRefreshProgress starts a refresh progress record.
func (s *Service) InitRefreshProgress(id string, total int) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	s.refreshes[id] = map[string]any{
		"total": total, "processed": 0, "done": false, "error": nil,
		"status_counts": map[string]any{StatusNormal: 0, StatusLimited: 0, StatusAbnormal: 0, StatusDisabled: 0}, "total_quota": 0,
	}
}

func (s *Service) updateRefreshProgress(id, token string) {
	account := s.GetAccount(token)
	status, quota := StatusNormal, 0
	if account != nil {
		if text := str(account["status"]); text != "" {
			status = text
		}
		quota = max(0, intOf(account["quota"]))
	}
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	progress := s.refreshes[id]
	if progress == nil {
		return
	}
	progress["processed"] = progress["processed"].(int) + 1
	counts := progress["status_counts"].(map[string]any)
	current, _ := counts[status].(int)
	counts[status] = current + 1
	progress["total_quota"] = progress["total_quota"].(int) + quota
}

func (s *Service) finishProgress(store map[string]map[string]any, id string, result any, errText string) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	progress := store[id]
	if progress == nil {
		return
	}
	progress["done"] = true
	progress["result"] = result
	if errText != "" {
		progress["error"] = errText
	}
}

func cloneProgress(progress map[string]any) map[string]any {
	if progress == nil {
		return nil
	}
	out := make(map[string]any, len(progress))
	for key, value := range progress {
		out[key] = value
	}
	return out
}

// RefreshProgress is get_refresh_progress (nil when unknown).
func (s *Service) RefreshProgress(id string) map[string]any {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	return cloneProgress(s.refreshes[id])
}

// ReloginProgress is get_relogin_progress, also used by the image test.
func (s *Service) ReloginProgress(id string) map[string]any {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	return cloneProgress(s.relogins[id])
}

func (s *Service) InitReloginProgress(id string, total int) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	s.relogins[id] = map[string]any{"total": total, "processed": 0, "done": false, "error": nil, "results": []any{}}
}

// UpdateReloginProgress adds one result; the batch is done when every account reported.
func (s *Service) UpdateReloginProgress(id, token, status, errText string) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	progress := s.relogins[id]
	if progress == nil {
		return
	}
	processed := progress["processed"].(int) + 1
	progress["processed"] = processed
	var errValue any
	if errText != "" {
		errValue = errText
	}
	progress["results"] = append(progress["results"].([]any), map[string]any{"token": AnonymizeToken(token), "status": status, "error": errValue})
	if processed >= progress["total"].(int) {
		progress["done"] = true
	}
}

func (s *Service) FinishReloginProgress(id string, result any, errText string) {
	s.finishProgress(s.relogins, id, result, errText)
}

// RefreshResult is refresh_accounts' result.
type RefreshResult struct {
	Refreshed int
	Errors    []map[string]any
	Items     []Account
	Relogined int
}

func (r RefreshResult) Map() map[string]any {
	errs := make([]any, len(r.Errors))
	for i, item := range r.Errors {
		errs[i] = item
	}
	return map[string]any{"refreshed": r.Refreshed, "errors": errs, "items": r.Items, "relogined": r.Relogined}
}

// RefreshAccounts reads each account from ChatGPT, 10 at a time.
func (s *Service) RefreshAccounts(ctx context.Context, tokens []string, progressID string, deferRemoval bool) RefreshResult {
	tokens = uniqueTokens(tokens)
	if len(tokens) == 0 {
		result := RefreshResult{Errors: []map[string]any{}, Items: s.ListAccounts()}
		if progressID != "" {
			s.finishProgress(s.refreshes, progressID, result.Map(), "")
		}
		return result
	}
	if progressID != "" {
		s.InitRefreshProgress(progressID, len(tokens))
	}
	var (
		mu        sync.Mutex
		refreshed int
		errs      = []map[string]any{}
		wg        sync.WaitGroup
		jobs      = make(chan string)
	)
	for i := 0; i < min(10, len(tokens)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for token := range jobs {
				account, err := s.FetchRemoteInfo(ctx, token, "refresh_accounts", deferRemoval)
				mu.Lock()
				if err != nil {
					// TLS/proxy failures are network problems, not the account's fault
					if !IsTLSConnectionError(err.Error()) {
						errs = append(errs, map[string]any{"token": AnonymizeToken(token), "error": err.Error()})
					}
				} else if account != nil {
					refreshed++
				}
				mu.Unlock()
				if progressID != "" {
					s.updateRefreshProgress(progressID, token)
				}
			}
		}()
	}
	for _, token := range tokens {
		jobs <- token
	}
	close(jobs)
	wg.Wait()

	relogined := 0
	if s.settings.AutoReloginAfterRefresh() && s.Relogin != nil {
		for _, token := range tokens {
			account := s.GetAccount(token)
			if account == nil || str(account["status"]) != StatusAbnormal {
				continue
			}
			email, password := str(account["email"]), str(account["password"])
			if email == "" || password == "" {
				continue
			}
			go s.Relogin(token, email, password, "auto_relogin_after_refresh", "", str(account["totp_secret"]))
			relogined++
		}
	}
	result := RefreshResult{Refreshed: refreshed, Errors: errs, Items: s.ListAccounts(), Relogined: relogined}
	if progressID != "" {
		s.finishProgress(s.refreshes, progressID, result.Map(), "")
	}
	return result
}

// ReloginAccounts is re_login_accounts: start the password login for each account that has one.
func (s *Service) ReloginAccounts(tokens []string, progressID string) map[string]any {
	tokens = uniqueTokens(tokens)
	if len(tokens) == 0 {
		result := map[string]any{"relogined": 0, "skipped": 0, "errors": []any{}, "items": s.ListAccounts()}
		if progressID != "" {
			s.FinishReloginProgress(progressID, result, "")
		}
		return result
	}
	if progressID != "" {
		s.InitReloginProgress(progressID, len(tokens))
	}
	relogined, skipped := 0, 0
	errs := []any{}
	for _, token := range tokens {
		account := s.GetAccount(token)
		if account == nil {
			errs = append(errs, map[string]any{"token": AnonymizeToken(token), "error": "账号不存在"})
			if progressID != "" {
				s.UpdateReloginProgress(progressID, token, "跳过", "账号不存在")
			}
			continue
		}
		email, password := str(account["email"]), str(account["password"])
		if email == "" || password == "" || s.Relogin == nil {
			skipped++
			if progressID != "" {
				s.UpdateReloginProgress(progressID, token, "跳过", "无邮箱密码")
			}
			continue
		}
		go s.Relogin(token, email, password, "manual_relogin", progressID, str(account["totp_secret"]))
		relogined++
	}
	result := map[string]any{"relogined": relogined, "skipped": skipped, "errors": errs, "items": s.ListAccounts()}
	if progressID != "" && relogined == 0 {
		s.FinishReloginProgress(progressID, result, "")
	}
	return result
}

// ImageProbe makes one small test image with the account (test_image_accounts). It returns
// "成功" or "失败" and, on failure, the reason.
type ImageProbe func(ctx context.Context, token string) (status string, reason string)

// ImageProbeReason is _image_probe_reason: a short label for the failure.
func ImageProbeReason(cause string) string {
	raw := strings.Join(strings.Fields(cause), " ")
	low := strings.ToLower(raw)
	switch {
	case strings.Contains(raw, "status=403") || strings.Contains(low, "enlarge-appear") || strings.Contains(low, "bootstrap failed"):
		return "Cloudflare 403"
	case strings.Contains(raw, "内容政策") || strings.Contains(low, "content policy"):
		return "内容政策"
	case strings.Contains(raw, "超时") || strings.Contains(low, "timeout"):
		return "生图超时"
	case strings.Contains(low, "quota") || strings.Contains(raw, "上限") || strings.Contains(low, "no available image quota"):
		return "额度用完"
	case strings.Contains(low, "model not found"):
		return "模型不存在"
	}
	if text := truncate(raw, 120); text != "" {
		return text
	}
	return "没有返回图片"
}

// TestImageAccounts is test_image_accounts: 3 at a time, results in the relogin progress.
func (s *Service) TestImageAccounts(ctx context.Context, tokens []string, progressID string, probe ImageProbe) int {
	tokens = uniqueTokens(tokens)
	if progressID != "" {
		s.InitReloginProgress(progressID, len(tokens))
	}
	if len(tokens) == 0 {
		if progressID != "" {
			s.FinishReloginProgress(progressID, nil, "")
		}
		return 0
	}
	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < min(3, len(tokens)); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for token := range jobs {
				account := s.GetAccount(token)
				label := AnonymizeToken(token)
				if email := str(account["email"]); email != "" {
					label = email
				}
				status, reason := "跳过", label+": 账号不存在"
				if account != nil {
					if probe == nil {
						status, reason = "失败", label+": 没有返回图片"
					} else if status, reason = probe(ctx, token); status != "成功" {
						reason = label + ": " + reason
					} else {
						reason = ""
					}
				}
				if progressID != "" {
					s.UpdateReloginProgress(progressID, token, status, reason)
				}
			}
		}()
	}
	for _, token := range tokens {
		jobs <- token
	}
	close(jobs)
	wg.Wait()
	return len(tokens)
}

// BuildExportItems is build_export_items: CPA/Codex export entries for accounts that have all three tokens.
func (s *Service) BuildExportItems(tokens []string) []map[string]any {
	targets := map[string]bool{}
	for _, token := range tokens {
		if token != "" {
			targets[token] = true
		}
	}
	s.mu.Lock()
	accounts := []Account{}
	for _, token := range s.order {
		if len(targets) == 0 || targets[str(s.items[token]["access_token"])] {
			accounts = append(accounts, copyAccount(s.items[token]))
		}
	}
	s.mu.Unlock()
	items := []map[string]any{}
	for _, account := range accounts {
		access, refresh, id := str(account["access_token"]), str(account["refresh_token"]), str(account["id_token"])
		if access == "" || refresh == "" || id == "" {
			continue
		}
		accessPayload, idPayload := DecodeJWTPayload(access), DecodeJWTPayload(id)
		auth, _ := accessPayload["https://api.openai.com/auth"].(map[string]any)
		profile, _ := accessPayload["https://api.openai.com/profile"].(map[string]any)
		exportType := str(account["export_type"])
		if exportType == "" {
			exportType = "codex"
		}
		item := map[string]any{
			"type":          exportType,
			"email":         firstNonEmpty(str(account["email"]), str(profile["email"]), str(idPayload["email"])),
			"account_id":    firstNonEmpty(str(account["account_id"]), str(auth["chatgpt_account_id"]), str(account["user_id"])),
			"access_token":  access,
			"refresh_token": refresh,
			"id_token":      id,
			"expired":       timestampToISO(accessPayload["exp"]),
			"last_refresh":  timestampToISO(accessPayload["iat"]),
		}
		if password := str(account["password"]); password != "" {
			item["password"] = password
		}
		if secret := str(account["totp_secret"]); secret != "" {
			item["totp_secret"] = secret
		}
		items = append(items, item)
	}
	return items
}

// FinishRefreshProgress marks a refresh batch done (with an error text when it failed).
func (s *Service) FinishRefreshProgress(id string, result any, errText string) {
	s.finishProgress(s.refreshes, id, result, errText)
}
