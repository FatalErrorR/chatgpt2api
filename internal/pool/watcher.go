package pool

import (
	"context"
	"log"
	"strings"
	"time"
)

// KeepaliveRefreshTokens is keepalive_refresh_tokens: force a refresh-token exchange for each account.
func (s *Service) KeepaliveRefreshTokens(ctx context.Context, tokens []string) map[string]any {
	refreshed, errors := 0, []map[string]any{}
	for _, token := range uniqueNonEmpty(tokens) {
		before := s.ResolveAccessToken(token)
		after := s.RefreshAccessToken(ctx, before, true, "refresh_token_keepalive")
		account := s.GetAccount(after)
		if account == nil {
			continue
		}
		if message := strings.TrimSpace(str(account["last_token_refresh_error"])); message != "" {
			errors = append(errors, map[string]any{"token": AnonymizeToken(before), "error": message})
			continue
		}
		refreshed++
	}
	return map[string]any{"refreshed": refreshed, "errors": errors, "items": s.ListAccounts(), "relogined": 0}
}

// WatchOnce is one round of start_limited_account_watcher: refresh limited, normal and expiring
// accounts, then keep the remaining refresh tokens alive.
func (s *Service) WatchOnce(ctx context.Context) {
	limited, normal, expiring := s.ListLimitedTokens(), s.ListNormalTokens(), s.ListExpiringAccessTokens()
	tokens := uniqueNonEmpty(append(append(append([]string{}, limited...), normal...), expiring...))
	expiringSet := map[string]bool{}
	for _, token := range expiring {
		expiringSet[token] = true
	}
	keepalive := []string{}
	for _, token := range s.ListRefreshTokenKeepaliveTokens() {
		if !expiringSet[token] {
			keepalive = append(keepalive, token)
		}
	}
	if len(tokens) > 0 {
		log.Printf("[account-watcher] checking %d limited accounts, %d normal accounts, %d expiring access tokens", len(limited), len(normal), len(expiring))
		s.RefreshAccounts(ctx, tokens, "", false)
	}
	if len(keepalive) > 0 {
		log.Printf("[account-watcher] keepalive %d refresh tokens", len(keepalive))
		if result := s.KeepaliveRefreshTokens(ctx, keepalive); len(result["errors"].([]map[string]any)) > 0 {
			log.Printf("[account-watcher] keepalive errors: %v", result["errors"])
		}
	}
}

// Watch runs WatchOnce right away and then every refresh_account_interval_minute.
func (s *Service) Watch(ctx context.Context) {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[account-watcher] fail %v", r)
				}
			}()
			s.WatchOnce(ctx)
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(s.settings.RefreshAccountIntervalMinute()) * time.Minute):
		}
	}
}

// uniqueNonEmpty is list(dict.fromkeys(t for t in tokens if t)).
func uniqueNonEmpty(tokens []string) []string {
	seen, out := map[string]bool{}, []string{}
	for _, token := range tokens {
		if token != "" && !seen[token] {
			seen[token] = true
			out = append(out, token)
		}
	}
	return out
}
