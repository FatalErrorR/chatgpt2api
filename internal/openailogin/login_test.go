package openailogin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
)

// Ported from test_protocol_login.py (TOTP, MFA helpers, login flow) plus the OAuth bridge.

const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

func TestTOTP(t *testing.T) {
	if code, _ := TOTPCode(rfcSecret, time.Unix(59, 0)); code != "287082" {
		t.Fatalf("code %s", code)
	}
	if code, _ := TOTPCode(rfcSecret, time.Unix(1111111109, 0)); code != "081804" {
		t.Fatalf("code %s", code)
	}
	if NormalizeTOTPSecret("gezd gnbv-gy3t qojq") != "GEZDGNBVGY3TQOJQ" || NormalizeTOTPSecret("not base32 !!") != "" || NormalizeTOTPSecret("") != "" {
		t.Fatal("normalize")
	}
	if _, err := TOTPCode("!!!", time.Now()); err == nil {
		t.Fatal("bad secret accepted")
	}
	if secondsLeft(time.Unix(60, 0)) != 30 || secondsLeft(time.Unix(89, 0)) != 1 {
		t.Fatal("seconds left")
	}
}

func TestMFAHelpers(t *testing.T) {
	cases := []struct {
		data map[string]any
		want string
	}{
		{map[string]any{"page": map[string]any{"payload": map[string]any{"factor_id": "f1"}}}, "f1"},
		{map[string]any{"page": map[string]any{"payload": map[string]any{"factor": map[string]any{"id": "f2"}}}}, "f2"},
		{map[string]any{"page": map[string]any{"payload": map[string]any{"factors": []any{map[string]any{"id": "sms", "factor_type": "sms"}, map[string]any{"id": "totp1", "factor_type": "totp"}}}}}, "totp1"},
		{map[string]any{"continue_url": "https://auth.openai.com/mfa-challenge/f3?x=1"}, "f3"},
		{map[string]any{}, ""},
	}
	for _, c := range cases {
		if got := mfaFactorID(c.data); got != c.want {
			t.Errorf("%v: %q", c.data, got)
		}
	}
	if !isMFAPage(map[string]any{"page": map[string]any{"type": "mfa_challenge"}}) || !isMFAPage(map[string]any{"continue_url": "https://x/mfa-challenge/x"}) ||
		isMFAPage(map[string]any{"page": map[string]any{"type": "email_otp_verification"}}) || isMFAPage(map[string]any{}) {
		t.Fatal("isMFAPage")
	}
}

func TestSentinelProofMeetsTheDifficulty(t *testing.T) {
	g := sentinelGenerator{deviceID: "d", userAgent: UserAgent, sid: "s"}
	token := g.proofToken("seed", "0fff")
	payload := strings.TrimSuffix(strings.TrimPrefix(token, "gAAAAAB"), "~S")
	if !strings.HasSuffix(token, "~S") || fnv1a32("seed" + payload)[:4] > "0fff" {
		t.Fatalf("token %s", token)
	}
	raw, _ := base64.StdEncoding.DecodeString(payload)
	var data []any
	if json.Unmarshal(raw, &data) != nil || len(data) != 18 {
		t.Fatalf("config %s", raw)
	}
}

// fakeAuth is auth.openai.com, sentinel and chatgpt.com in one server.
type fakeAuth struct {
	t        *testing.T
	mu       sync.Mutex
	verify   func() (int, map[string]any)
	mfa      func(body map[string]any) (int, map[string]any)
	calls    []string
	bodies   map[string]map[string]any
	base     string
	jwtEmail string
}

func jwt(claims map[string]any) string {
	raw, _ := json.Marshal(claims)
	return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func (f *fakeAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.bodies[r.URL.Path] = body
	f.mu.Unlock()
	reply := func(status int, data map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(data)
	}
	switch r.URL.Path {
	case "/api/accounts/authorize":
		if r.URL.Query().Get("login_hint") == "" || r.URL.Query().Get("code_challenge_method") != "S256" {
			f.t.Errorf("authorize query %v", r.URL.Query())
		}
		http.Redirect(w, r, "/log-in/password", http.StatusFound)
	case "/log-in/password":
		w.Write([]byte("<html>"))
	case "/backend-api/sentinel/req":
		reply(200, map[string]any{"token": "sentinel-c", "proofofwork": map[string]any{"required": true, "seed": "s", "difficulty": "0fffff"}})
	case "/api/accounts/password/verify":
		if r.Header.Get("openai-sentinel-token") == "" || r.Header.Get("oai-device-id") == "" {
			f.t.Errorf("verify headers %v", r.Header)
		}
		reply(f.verify())
	case "/api/accounts/mfa/verify":
		reply(f.mfa(body))
	case "/hop":
		http.Redirect(w, r, f.base+"/auth/callback?code=final-code&state=s", http.StatusFound)
	case "/api/accounts/oauth/token":
		reply(200, map[string]any{"access_token": jwt(map[string]any{"https://api.openai.com/profile": map[string]any{"email": f.jwtEmail}, "exp": 9999999999,
			"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct-jwt"}}), "refresh_token": "rt", "id_token": "idt"})
	case "/backend-api/me":
		reply(200, map[string]any{"account": map[string]any{"account_id": "acc"}})
	default:
		reply(404, map[string]any{})
	}
}

func newFake(t *testing.T) (*fakeAuth, Login) {
	f := &fakeAuth{t: t, bodies: map[string]map[string]any{}, jwtEmail: "a@example.com"}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	f.base = server.URL
	ep := Endpoints{Auth: server.URL, Platform: server.URL, Sentinel: server.URL, ChatGPT: server.URL}
	return f, Login{Endpoints: ep, Now: func() time.Time { return time.Unix(100, 0) }, Sleep: func(time.Duration) {}}
}

func TestPasswordOnlyLogin(t *testing.T) {
	f, login := newFake(t)
	f.verify = func() (int, map[string]any) {
		return 200, map[string]any{"continue_url": f.base + "/auth/callback?code=abc"}
	}
	result, err := login.PasswordLogin(context.Background(), "a@example.com", "pw", "", "")
	if err != nil || result["ok"] != true || result["account_id"] != "acct-jwt" || result["email"] != "a@example.com" || result["refresh_token"] != "rt" {
		t.Fatalf("result %v err %v", result, err)
	}
	if f.bodies["/api/accounts/password/verify"]["password"] != "pw" || f.bodies["/api/accounts/oauth/token"]["code"] != "abc" ||
		f.bodies["/api/accounts/oauth/token"]["code_verifier"] == "" {
		t.Fatalf("bodies %v", f.bodies)
	}
}

func TestMFALoginSubmitsTOTPAndFollowsRedirect(t *testing.T) {
	f, login := newFake(t)
	f.verify = func() (int, map[string]any) {
		return 200, map[string]any{"continue_url": f.base + "/mfa-challenge/factor-9", "page": map[string]any{"type": "mfa_challenge", "payload": map[string]any{"factor": map[string]any{"id": "factor-9"}}}}
	}
	f.mfa = func(body map[string]any) (int, map[string]any) {
		return 200, map[string]any{"continue_url": f.base + "/hop"}
	}
	result, err := login.PasswordLogin(context.Background(), "a@example.com", "pw", rfcSecret, "")
	if err != nil || result["ok"] != true {
		t.Fatalf("result %v err %v", result, err)
	}
	want, _ := TOTPCode(rfcSecret, time.Unix(100, 0))
	if body := f.bodies["/api/accounts/mfa/verify"]; body["id"] != "factor-9" || body["type"] != "totp" || body["code"] != want {
		t.Fatalf("mfa body %v", body)
	}
	if f.bodies["/api/accounts/oauth/token"]["code"] != "final-code" {
		t.Fatalf("exchange %v", f.bodies["/api/accounts/oauth/token"])
	}
}

func TestLoginFailures(t *testing.T) {
	mfaPage := func(f *fakeAuth) func() (int, map[string]any) {
		return func() (int, map[string]any) {
			return 200, map[string]any{"page": map[string]any{"type": "mfa_challenge"}, "continue_url": f.base + "/mfa-challenge/f"}
		}
	}
	cases := []struct {
		name   string
		setup  func(f *fakeAuth)
		secret string
		want   string
	}{
		{"no secret", func(f *fakeAuth) { f.verify = mfaPage(f) }, "", "need_totp_secret"},
		{"rejected code", func(f *fakeAuth) {
			f.verify = mfaPage(f)
			f.mfa = func(map[string]any) (int, map[string]any) {
				return 400, map[string]any{"error": map[string]any{"code": "invalid_code"}}
			}
		}, rfcSecret, "mfa_verify_failed_400"},
		{"bad secret", func(f *fakeAuth) { f.verify = mfaPage(f) }, "!!!", "invalid_totp_secret"},
		{"email otp", func(f *fakeAuth) {
			f.verify = func() (int, map[string]any) {
				return 200, map[string]any{"page": map[string]any{"type": "email_otp_verification"}}
			}
		}, "", "need_verification_code"},
		{"wrong password", func(f *fakeAuth) {
			f.verify = func() (int, map[string]any) {
				return 401, map[string]any{"error": map[string]any{"message": "Invalid credentials"}}
			}
		}, "", "invalid_password"},
		{"deactivated", func(f *fakeAuth) {
			f.verify = func() (int, map[string]any) {
				return 403, map[string]any{"error": map[string]any{"code": "account_deactivated"}}
			}
		}, "", "password_verify_failed_403"},
	}
	for _, c := range cases {
		f, login := newFake(t)
		c.setup(f)
		result, err := login.PasswordLogin(context.Background(), "a@example.com", "pw", c.secret, "")
		if err != nil || result["ok"] != false || result["error"] != c.want {
			t.Errorf("%s: %v %v", c.name, result, err)
		}
	}
}

func TestLoginUsesTheAccountProxyElseTheGlobalOne(t *testing.T) {
	var got []string
	login := Login{Endpoints: Endpoints{Auth: "http://127.0.0.1:1", Platform: "http://x", Sentinel: "http://x", ChatGPT: "http://x"},
		GlobalProxy: func() string { return "http://global:8080" },
		NewClient:   func(proxy string) (Client, error) { got = append(got, proxy); return failingClient{}, nil }}
	login.PasswordLogin(context.Background(), "a@example.com", "pw", "", "socks5://user:pass@1.2.3.4:1080")
	login.PasswordLogin(context.Background(), "a@example.com", "pw", "", "")
	if len(got) != 2 || got[0] != "socks5h://user:pass@1.2.3.4:1080" || got[1] != "http://global:8080" {
		t.Fatalf("proxies %v", got)
	}
}

type failingClient struct{}

func (failingClient) Do(*fhttp.Request) (*fhttp.Response, error) { return nil, context.Canceled }
func (failingClient) SetFollowRedirect(bool)                     {}
func (failingClient) SetCookies(*url.URL, []*fhttp.Cookie)       {}

func TestOAuthBridge(t *testing.T) {
	f, login := newFake(t)
	_ = login
	o := &OAuth{Endpoints: Endpoints{Auth: f.base, Platform: f.base, Sentinel: f.base, ChatGPT: f.base}}
	started := o.Start("a@example.com")
	authorize, _ := url.Parse(started["authorize_url"])
	state := authorize.Query().Get("state")
	if !strings.HasPrefix(state, started["session_id"]+".") || authorize.Query().Get("login_hint") != "a@example.com" || started["expires_in"] != "600" {
		t.Fatalf("start %v", started)
	}
	if _, err := o.Finish(context.Background(), "", f.base+"/auth/callback?code=c&state=other.x"); err == nil || !strings.Contains(err.Error(), "过期或不存在") {
		t.Fatalf("unknown session: %v", err)
	}
	if _, err := o.Finish(context.Background(), started["session_id"], f.base+"/auth/callback?error=access_denied"); err == nil || err.Error() != "access_denied" {
		t.Fatalf("error callback: %v", err)
	}
	if _, err := o.Finish(context.Background(), started["session_id"], f.base+"/auth/callback?code=c&state="+started["session_id"]+".wrong"); err == nil || !strings.Contains(err.Error(), "state 不匹配") {
		t.Fatalf("state mismatch: %v", err)
	}
	tokens, err := o.Finish(context.Background(), "", f.base+"/auth/callback?code=the-code&state="+url.QueryEscape(state))
	if err != nil || tokens["refresh_token"] != "rt" || f.bodies["/api/accounts/oauth/token"]["code"] != "the-code" {
		t.Fatalf("tokens %v err %v", tokens, err)
	}
	if _, err := o.Finish(context.Background(), started["session_id"], "the-code"); err == nil {
		t.Fatal("a session is used once")
	}
}
