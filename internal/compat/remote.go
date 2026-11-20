package compat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/settings"
	"github.com/basketikun/chatgpt2api/internal/upstream"
	http "github.com/bogdanfinn/fhttp"
)

const (
	oauthTokenURL  = "https://auth.openai.com/oauth/token"
	oauthClientID  = "app_2SKx67EdpoN0G6j64rFvigXD"
	oauthUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
)

// ChatGPTRemote implements pool.Remote with the upstream ChatGPT client.
type ChatGPTRemote struct {
	Proxy  upstream.ClientProvider
	Config *config.Config
	// Health records how the account's proxy did, as _retry_cloudflare_challenge does in Python.
	Health interface {
		RecordSuccess(proxy string)
		RecordFailure(proxy, cause string)
	}
	Sleep func(time.Duration)
}

// challengePage is a 403 whose body is not JSON: Cloudflare's challenge, not an API answer.
func challengePage(err error) bool {
	var httpErr *upstream.UpstreamHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != 403 {
		return false
	}
	var decoded any
	return json.Unmarshal([]byte(httpErr.Body), &decoded) != nil
}

func accountProxy(account pool.Account) string {
	text, _ := account["proxy"].(string)
	return strings.TrimSpace(text)
}

// UserInfo is OpenAIBackendAPI.get_user_info for one account, through its own proxy.
func (r *ChatGPTRemote) UserInfo(ctx context.Context, account pool.Account) (pool.Account, error) {
	token, _ := account["access_token"].(string)
	api := upstream.NewOpenAIBackendAPI(token, accountProxy(account), r.Proxy, r.Config).SingleAttempt()
	if accountID, _ := account["account_id"].(string); accountID != "" {
		api.WithAccountID(accountID)
	}
	proxy := accountProxy(account)
	var info pool.Account
	var err error
	var outcomes []error // the last attempt's sub-request results
	api.ObserveUserInfo(func(result error) { outcomes = append(outcomes, result) })
	for attempt := 1; attempt <= 3; attempt++ {
		outcomes = nil
		info, err = api.UserInfo(ctx)
		if !challengePage(err) {
			break
		}
		if attempt < 3 {
			if r.Sleep != nil {
				r.Sleep(time.Duration(1.5 * float64(attempt) * float64(time.Second)))
			} else {
				time.Sleep(time.Duration(1.5 * float64(attempt) * float64(time.Second)))
			}
		}
	}
	if r.Health != nil && proxy != "" {
		for _, result := range outcomes {
			var httpErr *upstream.UpstreamHTTPError
			switch {
			case challengePage(result):
				r.Health.RecordFailure(proxy, "Cloudflare challenge (HTTP 403)")
			case result == nil || errors.As(result, &httpErr) || errors.Is(result, upstream.ErrTokenInvalidated):
				r.Health.RecordSuccess(proxy) // an API answer: the proxy got through
			default:
				r.Health.RecordFailure(proxy, result.Error())
			}
		}
	}
	if err != nil {
		if errors.Is(err, upstream.ErrTokenInvalidated) {
			return nil, fmt.Errorf("%s: %w", strings.TrimSuffix(err.Error(), ": token invalidated"), pool.ErrInvalidToken)
		}
		return nil, err
	}
	return info, nil
}

// RefreshAccessToken is AccountService._request_access_token_refresh.
func (r *ChatGPTRemote) RefreshAccessToken(ctx context.Context, refreshToken string, account pool.Account) (map[string]string, error) {
	client, err := r.Proxy.TLSClient(accountProxy(account), false, 60)
	if err != nil {
		return nil, err
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {oauthClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", oauthUserAgent)
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var data map[string]any
	json.Unmarshal(raw, &data)
	accessToken, _ := data["access_token"].(string)
	if response.StatusCode != 200 || accessToken == "" {
		detail := ""
		for _, key := range []string{"error_description", "error", "message"} {
			if text, ok := data[key].(string); ok && text != "" {
				detail = text
				break
			}
		}
		if detail == "" {
			detail = string(raw)
			if len(detail) > 300 {
				detail = detail[:300]
			}
		}
		message := fmt.Sprintf("oauth_refresh_http_%d", response.StatusCode)
		if detail != "" {
			message += ": " + detail
		}
		return nil, errors.New(message)
	}
	next, _ := data["refresh_token"].(string)
	if next == "" {
		next = refreshToken
	}
	id, _ := data["id_token"].(string)
	return map[string]string{"access_token": strings.TrimSpace(accessToken), "refresh_token": strings.TrimSpace(next), "id_token": strings.TrimSpace(id)}, nil
}

// SyncUpstreamConfig copies the settings the upstream services read from config.json.
func SyncUpstreamConfig(cfg *config.Config, store *settings.Store) {
	runtime := store.ProxyRuntime()
	clearance, _ := runtime["clearance"].(map[string]any)
	stringOf := func(m map[string]any, key string) string { text, _ := m[key].(string); return text }
	boolOf := func(m map[string]any, key string) bool { value, _ := m[key].(bool); return value }
	intOf := func(m map[string]any, key string) int { value, _ := m[key].(int); return value }
	cfg.Sync(func(c *config.Config) {
		c.Proxy = store.Proxy()
		c.BaseURL = store.BaseURL()
		c.SystemPrompt = store.GlobalSystemPrompt()
		c.DefaultModel = store.DefaultUpstreamModelName()
		c.ThinkingEffort = store.DefaultThinkingEffort()
		c.Account.RefreshIntervalMinute = store.RefreshAccountIntervalMinute()
		c.Account.AutoRemoveInvalid = store.AutoRemoveInvalidAccounts()
		c.Account.RemoveConversation = store.ImageRemoveConversationAfterResult()
		c.Image.RetentionDays = store.ImageRetentionDays()
		c.Image.PollTimeoutSeconds = store.ImagePollTimeoutSecs()
		c.Image.PollIntervalSeconds = store.ImagePollIntervalSecs()
		c.Image.PollInitialWaitSeconds = store.ImagePollInitialWaitSecs()
		c.Image.AccountConcurrency = store.ImageAccountConcurrency()
		c.ProxyRuntime = config.ProxyRuntime{
			Enabled: boolOf(runtime, "enabled"), EgressMode: stringOf(runtime, "egress_mode"),
			ProxyURL: stringOf(runtime, "proxy_url"), ResourceProxyURL: stringOf(runtime, "resource_proxy_url"),
			Clearance: config.ClearanceSettings{
				Mode: stringOf(clearance, "mode"), CFCookies: stringOf(clearance, "cf_cookies"), CFClearance: stringOf(clearance, "cf_clearance"),
				UserAgent: stringOf(clearance, "user_agent"), FlareSolverrURL: stringOf(clearance, "flaresolverr_url"),
				TimeoutSeconds: intOf(clearance, "timeout_sec"), RefreshInterval: intOf(clearance, "refresh_interval"),
			},
		}
	})
}
