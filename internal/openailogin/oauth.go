package openailogin

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// OAuthError is OAuthLoginError: an expected failure the API answers with 400.
type OAuthError struct{ Message string }

func (e *OAuthError) Error() string { return e.Message }

const (
	oauthSessionTTL = 10 * time.Minute
	oauthMaxSession = 64
)

type oauthSession struct {
	verifier, state, redirectURI string
	created                      time.Time
}

// OAuth is OAuthLoginService: PKCE sessions for the manual browser login bridge.
type OAuth struct {
	Endpoints Endpoints
	NewClient func(proxy string) (Client, error)
	Proxy     func() string // the global proxy

	mu       sync.Mutex
	sessions map[string]oauthSession
}

func (o *OAuth) endpoints() Endpoints {
	if o.Endpoints.Auth == "" {
		return Default
	}
	return o.Endpoints
}

func (o *OAuth) purgeLocked() {
	for id, s := range o.sessions {
		if time.Since(s.created) > oauthSessionTTL {
			delete(o.sessions, id)
		}
	}
	if len(o.sessions) > oauthMaxSession {
		ids := make([]string, 0, len(o.sessions))
		for id := range o.sessions {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return o.sessions[ids[i]].created.Before(o.sessions[ids[j]].created) })
		for _, id := range ids[:len(o.sessions)-oauthMaxSession] {
			delete(o.sessions, id)
		}
	}
}

// Start is start(): a new PKCE session and the authorize URL to open.
func (o *OAuth) Start(emailHint string) map[string]string {
	ep := o.endpoints()
	verifier, challenge := PKCE()
	sessionID := strings.ReplaceAll(uuid.NewString(), "-", "")
	state := sessionID + "." + randomURLSafe(16)
	params := url.Values{"issuer": {ep.Auth}, "client_id": {ClientID}, "audience": {audience}, "redirect_uri": {ep.redirectURI()},
		"device_id": {uuid.NewString()}, "screen_hint": {"login_or_signup"}, "max_age": {"0"}, "scope": {"openid profile email offline_access"},
		"response_type": {"code"}, "response_mode": {"query"}, "state": {state}, "nonce": {randomURLSafe(32)},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "auth0Client": {auth0Client}}
	if hint := strings.TrimSpace(emailHint); hint != "" {
		params.Set("login_hint", hint)
	}
	o.mu.Lock()
	if o.sessions == nil {
		o.sessions = map[string]oauthSession{}
	}
	o.purgeLocked()
	o.sessions[sessionID] = oauthSession{verifier: verifier, state: state, redirectURI: ep.redirectURI(), created: time.Now()}
	o.mu.Unlock()
	return map[string]string{"session_id": sessionID, "authorize_url": ep.Auth + "/api/accounts/authorize?" + params.Encode(),
		"expires_in": strconv.Itoa(int(oauthSessionTTL.Seconds())), "redirect_uri_prefix": ep.redirectURI()}
}

func codeFromCallback(value string) (string, string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", "", nil
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", "", &OAuthError{"无法解析 callback URL: " + err.Error()}
		}
		query := parsed.Query()
		code, state := strings.TrimSpace(query.Get("code")), strings.TrimSpace(query.Get("state"))
		if code == "" {
			message := strings.TrimSpace(query.Get("error_description"))
			if message == "" {
				message = strings.TrimSpace(query.Get("error"))
			}
			if message == "" {
				message = "callback URL 中没有 code 参数"
			}
			return "", "", &OAuthError{message}
		}
		return code, state, nil
	}
	return raw, "", nil
}

// Finish is finish(): exchange the callback's code with the session's verifier.
func (o *OAuth) Finish(ctx context.Context, sessionID, callback string) (map[string]string, error) {
	code, state, err := codeFromCallback(callback)
	if err != nil {
		return nil, err
	}
	if code == "" {
		return nil, &OAuthError{"缺少 code 或 callback URL"}
	}
	candidates := []string{}
	if state != "" {
		candidates = append(candidates, strings.SplitN(state, ".", 2)[0])
	}
	if id := strings.TrimSpace(sessionID); id != "" {
		candidates = append(candidates, id)
	}
	if len(candidates) == 0 {
		return nil, &OAuthError{"既未提供 session_id，callback URL 中也未携带 state"}
	}
	o.mu.Lock()
	if o.sessions == nil {
		o.sessions = map[string]oauthSession{}
	}
	o.purgeLocked()
	var session oauthSession
	picked := ""
	for _, id := range candidates {
		if s, ok := o.sessions[id]; ok {
			session, picked = s, id
			break
		}
	}
	o.mu.Unlock()
	if picked == "" {
		return nil, &OAuthError{`OAuth 会话已过期或不存在，请回到导入对话框点"重新生成"再走一次`}
	}
	if state != "" && session.state != "" && state != session.state {
		return nil, &OAuthError{`state 不匹配。常见原因：你点过两次"打开授权页面"，但浏览器里登录的还是前一次的窗口。请点"重新生成"重来。`}
	}
	newClient := o.NewClient
	if newClient == nil {
		newClient = NewClient
	}
	proxy := ""
	if o.Proxy != nil {
		proxy = o.Proxy()
	}
	client, err := newClient(proxy)
	if err != nil {
		return nil, &OAuthError{"换 token 网络异常: " + err.Error()}
	}
	ep := o.endpoints()
	resp, err := exchange(ctx, client, ep, code, session.verifier, session.redirectURI, map[string]string{
		"accept": "application/json", "accept-language": "en-US,en;q=0.9", "cache-control": "no-cache", "content-type": "application/json",
		"dnt": "1", "priority": "u=1, i", "sec-gpc": "1", "sec-ch-ua": oauthSecChUA, "sec-ch-ua-mobile": "?0", "sec-ch-ua-platform": `"Windows"`,
		"sec-fetch-dest": "empty", "sec-fetch-mode": "cors", "sec-fetch-site": "same-origin", "user-agent": UserAgent,
		"referer": ep.Platform + "/", "origin": ep.Platform, "auth0-client": auth0Client})
	if err != nil {
		return nil, &OAuthError{"换 token 网络异常: " + err.Error()}
	}
	if resp.status != 200 || str(resp.data["access_token"]) == "" {
		detail := str(firstNonNil(resp.data["error_description"], resp.data["error"], resp.data["message"]))
		if detail == "" {
			detail = resp.text[:min(len(resp.text), 300)]
		}
		log.Printf("[oauth-login] /api/accounts/oauth/token rejected: status=%d detail=%q", resp.status, detail)
		message := fmt.Sprintf("OpenAI 拒绝换 token (HTTP %d)", resp.status)
		if detail != "" {
			message += ": " + detail
		}
		return nil, &OAuthError{message}
	}
	if str(resp.data["refresh_token"]) == "" {
		return nil, &OAuthError{"OpenAI 没有返回 refresh_token（可能 scope 未包含 offline_access 或 code 已使用过）"}
	}
	o.mu.Lock()
	delete(o.sessions, picked)
	o.mu.Unlock()
	return map[string]string{"access_token": str(resp.data["access_token"]), "refresh_token": str(resp.data["refresh_token"]),
		"id_token": str(resp.data["id_token"])}, nil
}
