// Package openailogin is the OpenAI account login of the Python version: the password login
// (account_service._login_with_password with TOTP two-step verification), the sentinel
// proof-of-work it needs (utils/sentinel.py), PKCE and TOTP helpers, and the code exchange the
// manual OAuth bridge uses (oauth_login_service).
package openailogin

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/google/uuid"

	"github.com/basketikun/chatgpt2api/internal/proxylist"
)

const (
	ClientID       = "app_2SKx67EdpoN0G6j64rFvigXD"
	audience       = "https://api.openai.com/v1"
	auth0Client    = "eyJuYW1lIjoiYXV0aDAtc3BhLWpzIiwidmVyc2lvbiI6IjEuMjEuMCJ9"
	UserAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"
	loginSecChUA   = `"Chromium";v="145", "Google Chrome";v="145", "Not/A)Brand";v="99"`
	oauthSecChUA   = `"Google Chrome";v="145", "Not?A_Brand";v="8", "Chromium";v="145"`
	totpPeriod     = 30
	defaultAuth    = "https://auth.openai.com"
	defaultPlat    = "https://platform.openai.com"
	defaultSentry  = "https://sentinel.openai.com"
	defaultChatGPT = "https://chatgpt.com"
)

// Endpoints are the hosts the login talks to (tests point them at a fake server).
type Endpoints struct {
	Auth, Platform, Sentinel, ChatGPT string
}

// Default are the real hosts.
var Default = Endpoints{Auth: defaultAuth, Platform: defaultPlat, Sentinel: defaultSentry, ChatGPT: defaultChatGPT}

func (e Endpoints) redirectURI() string { return e.Platform + "/auth/callback" }

// ---- TOTP (utils/totp.py) ----

// NormalizeTOTPSecret strips spaces and dashes, upper-cases; "" when it is not base32.
func NormalizeTOTPSecret(secret string) string {
	cleaned := strings.ToUpper(strings.ReplaceAll(strings.Join(strings.Fields(secret), ""), "-", ""))
	if cleaned == "" {
		return ""
	}
	if _, err := base32.StdEncoding.DecodeString(cleaned + strings.Repeat("=", (8-len(cleaned)%8)%8)); err != nil {
		return ""
	}
	return cleaned
}

// TOTPCode is totp_code (RFC 6238, SHA-1, 6 digits, 30 s).
func TOTPCode(secret string, now time.Time) (string, error) {
	cleaned := NormalizeTOTPSecret(secret)
	if cleaned == "" {
		return "", fmt.Errorf("invalid totp secret")
	}
	key, _ := base32.StdEncoding.DecodeString(cleaned + strings.Repeat("=", (8-len(cleaned)%8)%8))
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(now.Unix()/totpPeriod))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	digest := mac.Sum(nil)
	offset := digest[len(digest)-1] & 0x0F
	value := (binary.BigEndian.Uint32(digest[offset:offset+4]) & 0x7FFFFFFF) % 1000000
	return fmt.Sprintf("%06d", value), nil
}

func secondsLeft(now time.Time) float64 {
	t := float64(now.UnixNano()) / 1e9
	return totpPeriod - mod(t, totpPeriod)
}

func mod(a, b float64) float64 { return a - b*float64(int64(a/b)) }

// ---- PKCE ----

func randomURLSafe(n int) string {
	buf := make([]byte, n)
	rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// PKCE is generate_pkce: a 64-byte verifier and its S256 challenge.
func PKCE() (string, string) {
	verifier := randomURLSafe(64)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// ---- HTTP ----

// Client is the part of tls_client.HttpClient the login needs.
type Client interface {
	Do(req *http.Request) (*http.Response, error)
	SetFollowRedirect(follow bool)
	SetCookies(u *url.URL, cookies []*http.Cookie)
}

// NewClient is a Chrome-impersonating client with its own cookie jar (curl_cffi Session with
// impersonate="chrome110", verify=False).
func NewClient(proxy string) (Client, error) {
	options := []tls_client.HttpClientOption{tls_client.WithTimeoutSeconds(60), tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithCookieJar(tls_client.NewCookieJar()), tls_client.WithInsecureSkipVerify()}
	if proxy = strings.TrimSpace(proxy); proxy != "" {
		options = append(options, tls_client.WithProxyUrl(proxy))
	}
	return tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
}

type response struct {
	status   int
	body     []byte
	finalURL string
	location string
}

func (r response) json() map[string]any {
	var out map[string]any
	if len(r.body) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(r.body))
		decoder.UseNumber()
		if decoder.Decode(&out) != nil {
			return map[string]any{}
		}
	}
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func send(ctx context.Context, client Client, method, target string, headers map[string]string, body any) (response, error) {
	var reader io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(v)
	default:
		raw, _ := json.Marshal(v)
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return response{}, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	resp, err := client.Do(request)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	final := target
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL.String()
	}
	return response{status: resp.StatusCode, body: raw, finalURL: final, location: resp.Header.Get("Location")}, nil
}

// ---- sentinel (utils/sentinel.py) ----

func fnv1a32(text string) string {
	h := uint32(2166136261)
	for _, r := range text {
		h ^= uint32(r)
		h *= 16777619
	}
	h ^= h >> 16
	h *= 2246822507
	h ^= h >> 13
	h *= 3266489909
	h ^= h >> 16
	return fmt.Sprintf("%08x", h)
}

func randFloat() float64 {
	var b [8]byte
	rand.Read(b[:])
	return float64(binary.BigEndian.Uint64(b[:])>>11) / float64(1<<53)
}

func pick(items []any) any {
	var b [1]byte
	rand.Read(b[:])
	return items[int(b[0])%len(items)]
}

type sentinelGenerator struct {
	deviceID, userAgent, sid string
}

func (g sentinelGenerator) config() []any {
	perfNow := 1000 + randFloat()*49000
	return []any{"1920x1080", time.Now().UTC().Format("Mon Jan 02 2006 15:04:05") + " GMT+0000 (Coordinated Universal Time)", 4294705152, randFloat(),
		g.userAgent, "https://sentinel.openai.com/sentinel/20260124ceb8/sdk.js", nil, nil, "en-US", randFloat(),
		pick([]any{"vendorSub-undefined", "plugins-undefined", "mimeTypes-undefined", "hardwareConcurrency-undefined"}),
		pick([]any{"location", "implementation", "URL", "documentURI", "compatMode"}),
		pick([]any{"Object", "Function", "Array", "Number", "parseFloat", "undefined"}),
		perfNow, g.sid, "", pick([]any{4, 8, 12, 16}), float64(time.Now().UnixNano())/1e6 - perfNow}
}

func b64JSON(value any) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.Encode(value)
	return base64.StdEncoding.EncodeToString(bytes.TrimRight(buf.Bytes(), "\n"))
}

func (g sentinelGenerator) requirementsToken() string {
	data := g.config()
	data[3] = 1
	data[9] = int(5 + randFloat()*45 + 0.5)
	return "gAAAAAC" + b64JSON(data)
}

func (g sentinelGenerator) proofToken(seed, difficulty string) string {
	start := time.Now()
	data := g.config()
	if difficulty == "" {
		difficulty = "0"
	}
	for i := 0; i < 500000; i++ {
		data[3] = i
		data[9] = time.Since(start).Milliseconds()
		payload := b64JSON(data)
		if hash := fnv1a32(seed + payload); hash[:min(len(difficulty), len(hash))] <= difficulty {
			return "gAAAAAB" + payload + "~S"
		}
	}
	return "gAAAAAB" + "wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D" + b64JSON("None")
}

// sentinelToken is build_sentinel_token: the openai-sentinel-token header and the oai-sc cookie.
func sentinelToken(ctx context.Context, client Client, ep Endpoints, deviceID, flow string) (string, string, error) {
	g := sentinelGenerator{deviceID: deviceID, userAgent: UserAgent, sid: uuid.NewString()}
	body, _ := json.Marshal(map[string]any{"p": g.requirementsToken(), "id": deviceID, "flow": flow})
	resp, err := send(ctx, client, http.MethodPost, ep.Sentinel+"/backend-api/sentinel/req", map[string]string{
		"Content-Type": "text/plain;charset=UTF-8", "Referer": "https://sentinel.openai.com/backend-api/sentinel/frame.html",
		"Origin": "https://sentinel.openai.com", "User-Agent": UserAgent, "sec-ch-ua": loginSecChUA, "sec-ch-ua-mobile": "?0", "sec-ch-ua-platform": `"Windows"`,
	}, string(body))
	if err != nil {
		return "", "", err
	}
	var data map[string]any
	if json.Unmarshal(resp.body, &data) != nil {
		fallback, _ := json.Marshal(map[string]any{"p": g.requirementsToken(), "t": "", "c": "", "id": deviceID, "flow": flow})
		return string(fallback), "", nil
	}
	token, _ := data["token"].(string)
	if resp.status != 200 || strings.TrimSpace(token) == "" {
		return "", "", fmt.Errorf("sentinel_req_failed_%d", resp.status)
	}
	p := g.requirementsToken()
	if pow, _ := data["proofofwork"].(map[string]any); pow != nil && pow["required"] == true && fmt.Sprint(pow["seed"]) != "" && pow["seed"] != nil {
		p = g.proofToken(fmt.Sprint(pow["seed"]), fmt.Sprint(firstNonNil(pow["difficulty"], "0")))
	}
	value, _ := json.Marshal(map[string]any{"p": p, "t": "", "c": token, "id": deviceID, "flow": flow})
	return string(value), "0" + token, nil
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil && value != "" {
			return value
		}
	}
	return nil
}

// ---- password login ----

// Result is the dict _login_with_password returns: ok, error, detail, tokens and identity.
type Result map[string]any

func fail(code string, detail any) Result {
	return Result{"ok": false, "error": code, "detail": detail}
}

func extractAuthCode(link string) string {
	if link == "" {
		return ""
	}
	parsed, err := url.Parse(link)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.Query().Get("code"))
}

func str(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func isMFAPage(data map[string]any) bool {
	page, _ := data["page"].(map[string]any)
	pageType := strings.ToLower(str(page["type"]))
	continueURL := strings.ToLower(str(data["continue_url"]))
	return strings.Contains(pageType, "mfa") || strings.Contains(pageType, "totp") || strings.Contains(continueURL, "/mfa-challenge")
}

var mfaChallengeRE = regexp.MustCompile(`/mfa-challenge/([^/?#]+)`)

func mfaFactorID(data map[string]any) string {
	page, _ := data["page"].(map[string]any)
	payload, _ := page["payload"].(map[string]any)
	candidates := []any{payload["factor_id"]}
	if factor, ok := payload["factor"].(map[string]any); ok {
		candidates = append(candidates, factor["id"])
	}
	if factors, ok := payload["factors"].([]any); ok {
		totp, others := []any{}, []any{}
		for _, raw := range factors {
			f, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			kind := strings.ToLower(str(firstNonNil(f["factor_type"], f["type"])))
			if kind == "totp" {
				totp = append(totp, f["id"])
			} else {
				others = append(others, f["id"])
			}
		}
		candidates = append(append(candidates, totp...), others...)
	}
	candidates = append(candidates, payload["id"])
	for _, value := range candidates {
		if text := str(value); text != "" {
			return text
		}
	}
	if match := mfaChallengeRE.FindStringSubmatch(str(data["continue_url"])); match != nil {
		return match[1]
	}
	return ""
}

// DecodeJWTPayload reads a JWT's claims without verifying it.
func DecodeJWTPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&claims) != nil || claims == nil {
		return map[string]any{}
	}
	return claims
}

// Login performs the password login; totpSecret answers a two-step challenge. Sleep lets tests
// skip the wait before a TOTP window boundary. GlobalProxy is used when the account has none.
type Login struct {
	Endpoints   Endpoints
	GlobalProxy func() string
	NewClient   func(proxy string) (Client, error)
	Now         func() time.Time
	Sleep       func(time.Duration)
}

// PasswordLogin is _login_with_password.
func (l Login) PasswordLogin(ctx context.Context, email, password, totpSecret, proxy string) (Result, error) {
	ep := l.Endpoints
	if ep.Auth == "" {
		ep = Default
	}
	newClient := l.NewClient
	if newClient == nil {
		newClient = NewClient
	}
	// the account's proxy first, so the login and later image calls share one exit; else the global one
	proxy = proxylist.NormalizeProxyURL(proxy)
	if proxy == "" && l.GlobalProxy != nil {
		proxy = strings.TrimSpace(l.GlobalProxy())
	}
	client, err := newClient(proxy)
	if err != nil {
		return nil, err
	}
	deviceID := uuid.NewString()
	verifier, challenge := PKCE()
	if authURL, err := url.Parse(ep.Auth); err == nil {
		client.SetCookies(authURL, []*http.Cookie{{Name: "oai-did", Value: deviceID, Domain: authURL.Hostname(), Path: "/"}})
	}
	params := url.Values{"issuer": {ep.Auth}, "client_id": {ClientID}, "audience": {audience}, "redirect_uri": {ep.redirectURI()},
		"device_id": {deviceID}, "screen_hint": {"login_or_signup"}, "max_age": {"0"}, "login_hint": {email},
		"scope": {"openid profile email offline_access"}, "response_type": {"code"}, "response_mode": {"query"},
		"state": {randomURLSafe(32)}, "nonce": {randomURLSafe(32)}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"auth0Client": {auth0Client}}
	client.SetFollowRedirect(true)
	resp, err := send(ctx, client, http.MethodGet, ep.Auth+"/api/accounts/authorize?"+params.Encode(), map[string]string{
		"accept": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8", "accept-language": "zh-CN,zh;q=0.9,en;q=0.8",
		"user-agent": UserAgent, "sec-ch-ua": `"Chromium";v="145", "Google Chrome";v="145", "Not/A)Brand";v="99"`, "sec-ch-ua-mobile": "?0",
		"sec-ch-ua-platform": `"Windows"`, "sec-fetch-dest": "document", "sec-fetch-mode": "navigate", "sec-fetch-site": "cross-site",
		"sec-fetch-user": "?1", "upgrade-insecure-requests": "1", "referer": "https://platform.openai.com/",
	}, nil)
	if err != nil {
		return nil, err
	}
	if resp.status != 200 && resp.status != 302 {
		text := string(resp.body)
		return fail(fmt.Sprintf("authorize_failed_%d", resp.status), map[string]any{"url": resp.finalURL, "text": text[:min(len(text), 500)]}), nil
	}
	if strings.Contains(resp.finalURL, "/error") && strings.Contains(resp.finalURL, "payload=") {
		parsed, _ := url.Parse(resp.finalURL)
		encoded := parsed.Query().Get("payload")
		encoded += strings.Repeat("=", (4-len(encoded)%4)%4)
		raw, err := base64.StdEncoding.DecodeString(encoded)
		var payload map[string]any
		if err == nil {
			err = json.Unmarshal(raw, &payload)
		}
		if err != nil {
			return fail("authorize_redirect_error", map[string]any{"url": resp.finalURL, "parse_error": err.Error()}), nil
		}
		if code := str(payload["errorCode"]); code == "rate_limit_exceeded" {
			return fail("rate_limit_exceeded", payload), nil
		} else {
			return fail("authorize_error_"+code, payload), nil
		}
	}
	headers := map[string]string{"accept": "application/json", "accept-language": "zh-CN,zh;q=0.9", "content-type": "application/json",
		"origin": ep.Auth, "priority": "u=1, i", "user-agent": UserAgent, "sec-ch-ua": loginSecChUA, "sec-ch-ua-mobile": "?0",
		"sec-ch-ua-platform": `"Windows"`, "sec-fetch-dest": "empty", "sec-fetch-mode": "cors", "sec-fetch-site": "same-origin",
		"referer": ep.Auth + "/email-verification", "oai-device-id": deviceID}
	if token, cookie, err := sentinelToken(ctx, client, ep, deviceID, "password_verify"); err == nil {
		headers["openai-sentinel-token"] = token
		if cookie != "" {
			if openai, err := url.Parse(ep.Auth); err == nil {
				client.SetCookies(openai, []*http.Cookie{{Name: "oai-sc", Value: cookie, Path: "/"}})
			}
		}
	}
	login, err := send(ctx, client, http.MethodPost, ep.Auth+"/api/accounts/password/verify", headers, map[string]any{"password": password})
	if err != nil {
		return nil, err
	}
	data := login.json()
	if login.status != 200 {
		inner, _ := data["error"].(map[string]any)
		code, message := str(inner["code"]), str(inner["message"])
		switch {
		case code == "unsupported_country_region_territory":
			return fail(code, data), nil
		case code == "invalid_state":
			return fail(code, data), nil
		case strings.Contains(message, "Invalid credentials") || strings.Contains(strings.ToLower(message), "wrong password"):
			return fail("invalid_password", data), nil
		}
		return fail(fmt.Sprintf("password_verify_failed_%d", login.status), data), nil
	}
	authCode := extractAuthCode(str(data["continue_url"]))
	if authCode == "" {
		page, _ := data["page"].(map[string]any)
		if isMFAPage(data) {
			if strings.TrimSpace(totpSecret) == "" {
				return fail("need_totp_secret", data), nil
			}
			mfa, err := l.submitTOTP(ctx, client, ep, headers, data, totpSecret)
			if err != nil {
				return nil, err
			}
			if mfa["ok"] != true {
				return mfa, nil
			}
			authCode = str(mfa["auth_code"])
		} else if str(page["type"]) == "email_otp_verification" {
			return fail("need_verification_code", data), nil
		}
		if authCode == "" {
			return fail("no_auth_code", data), nil
		}
	}
	tokens, err := exchange(ctx, client, ep, authCode, verifier, ep.redirectURI(), map[string]string{
		"accept": "*/*", "accept-language": "zh-CN,zh;q=0.9", "auth0-client": auth0Client, "cache-control": "no-cache",
		"content-type": "application/json", "origin": ep.Platform, "pragma": "no-cache", "priority": "u=1, i", "referer": ep.Platform + "/",
		"sec-ch-ua": loginSecChUA, "sec-ch-ua-mobile": "?0", "sec-ch-ua-platform": `"Windows"`, "sec-fetch-dest": "empty",
		"sec-fetch-mode": "cors", "sec-fetch-site": "same-site", "user-agent": UserAgent})
	if err != nil {
		return nil, err
	}
	if tokens.status != 200 || str(tokens.data["access_token"]) == "" {
		return fail("token_exchange_failed", tokens.data), nil
	}
	accessToken := str(tokens.data["access_token"])
	userInfo := map[string]any{}
	if me, err := send(ctx, client, http.MethodGet, ep.ChatGPT+"/backend-api/me", map[string]string{"accept": "application/json",
		"authorization": "Bearer " + accessToken, "user-agent": UserAgent}, nil); err == nil && me.status == 200 {
		userInfo = me.json()
	}
	claims := DecodeJWTPayload(accessToken)
	profile, _ := claims["https://api.openai.com/profile"].(map[string]any)
	authClaims, _ := claims["https://api.openai.com/auth"].(map[string]any)
	account, _ := userInfo["account"].(map[string]any)
	resultEmail := str(profile["email"])
	if resultEmail == "" {
		resultEmail = email
	}
	accountID := str(authClaims["chatgpt_account_id"])
	if accountID == "" {
		accountID = str(account["account_id"])
	}
	return Result{"ok": true, "email": resultEmail, "account_id": accountID, "access_token": accessToken,
		"refresh_token": str(tokens.data["refresh_token"]), "id_token": str(tokens.data["id_token"]), "expires_at": claims["exp"],
		"source_type": "password"}, nil
}

func (l Login) submitTOTP(ctx context.Context, client Client, ep Endpoints, loginHeaders map[string]string, data map[string]any, secret string) (Result, error) {
	secret = NormalizeTOTPSecret(secret)
	if secret == "" {
		return Result{"ok": false, "error": "invalid_totp_secret"}, nil
	}
	factorID := mfaFactorID(data)
	if factorID == "" {
		return fail("mfa_factor_not_found", data), nil
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	if remaining := secondsLeft(now()); remaining < 3 {
		wait := time.Duration((remaining + 0.5) * float64(time.Second))
		if l.Sleep != nil {
			l.Sleep(wait)
		} else {
			time.Sleep(wait)
		}
	}
	code, err := TOTPCode(secret, now())
	if err != nil {
		return Result{"ok": false, "error": "invalid_totp_secret"}, nil
	}
	headers := map[string]string{}
	for key, value := range loginHeaders {
		headers[key] = value
	}
	headers["referer"] = ep.Auth + "/mfa-challenge/" + factorID
	resp, err := send(ctx, client, http.MethodPost, ep.Auth+"/api/accounts/mfa/verify", headers, map[string]any{"id": factorID, "type": "totp", "code": code})
	if err != nil {
		return nil, err
	}
	result := resp.json()
	if resp.status != 200 {
		return fail(fmt.Sprintf("mfa_verify_failed_%d", resp.status), result), nil
	}
	continueURL := str(result["continue_url"])
	authCode := extractAuthCode(continueURL)
	if authCode == "" && continueURL != "" {
		authCode = followForAuthCode(ctx, client, continueURL, ep.redirectURI(), loginHeaders)
	}
	if authCode == "" {
		return fail("mfa_no_auth_code", result), nil
	}
	return Result{"ok": true, "auth_code": authCode}, nil
}

// followForAuthCode is _follow_for_auth_code: follow redirects by hand until the callback.
func followForAuthCode(ctx context.Context, client Client, current, redirectURI string, headers map[string]string) string {
	client.SetFollowRedirect(false)
	defer client.SetFollowRedirect(true)
	for hop := 0; hop < 8; hop++ {
		if strings.HasPrefix(current, redirectURI) {
			return extractAuthCode(current)
		}
		resp, err := send(ctx, client, http.MethodGet, current, headers, nil)
		if err != nil || strings.TrimSpace(resp.location) == "" {
			return extractAuthCode(current)
		}
		base, _ := url.Parse(current)
		next, err := base.Parse(strings.TrimSpace(resp.location))
		if err != nil {
			return extractAuthCode(current)
		}
		current = next.String()
	}
	return extractAuthCode(current)
}

type tokenResponse struct {
	status int
	data   map[string]any
	text   string
}

func exchange(ctx context.Context, client Client, ep Endpoints, code, verifier, redirectURI string, headers map[string]string) (tokenResponse, error) {
	resp, err := send(ctx, client, http.MethodPost, ep.Auth+"/api/accounts/oauth/token", headers, map[string]any{"client_id": ClientID,
		"code_verifier": verifier, "grant_type": "authorization_code", "code": code, "redirect_uri": redirectURI})
	if err != nil {
		return tokenResponse{}, err
	}
	return tokenResponse{status: resp.status, data: resp.json(), text: string(resp.body)}, nil
}
