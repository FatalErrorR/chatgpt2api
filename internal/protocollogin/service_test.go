package protocollogin

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

// Ported from test_protocol_login.py (ParseLinesTests, ErrorTextTests, ServiceTests, relogin).

const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

type keepRemote struct{}

func (keepRemote) UserInfo(context.Context, pool.Account) (pool.Account, error) {
	return pool.Account{}, nil
}
func (keepRemote) RefreshAccessToken(context.Context, string, pool.Account) (map[string]string, error) {
	return nil, errors.New("no refresh token")
}

func newPool(t *testing.T, accounts ...pool.Account) *pool.Service {
	t.Helper()
	repo, err := repository.Open("sqlite:///" + filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	pool.Migrate(repo.DB())
	p, err := pool.New(pool.NewDBStore(repo.DB()), settings.NewForTest(map[string]any{"auto_remove_invalid_accounts": false}, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Remote = keepRemote{}
	if len(accounts) > 0 {
		p.AddAccountItems(accounts)
	}
	return p
}

func ok(token string, extra map[string]any) map[string]any {
	out := map[string]any{"ok": true, "access_token": token, "refresh_token": "rt-" + token, "id_token": "id", "email": ""}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestParseLines(t *testing.T) {
	entries, invalid := ParseLines("a@example.com----@pw1----" + rfcSecret + "\n")
	if len(invalid) != 0 || len(entries) != 1 || entries[0].Password != "@pw1" || entries[0].TOTPSecret != rfcSecret || entries[0].Line != 1 {
		t.Fatalf("%+v %v", entries, invalid)
	}
	entries, _ = ParseLines("a@example.com----pw\nb@example.com----p----w----GEZDGNBV")
	if entries[0].TOTPSecret != "" || entries[1].Password != "p----w" || entries[1].TOTPSecret != "GEZDGNBV" {
		t.Fatalf("%+v", entries)
	}
	text := strings.Join([]string{"# comment", "", "no-separator", "not-an-email----pw", "a@example.com----", "c@example.com----pw----!!bad!!", "d@example.com----pw", "D@example.com----pw2"}, "\n")
	entries, invalid = ParseLines(text)
	lines := []any{}
	for _, item := range invalid {
		lines = append(lines, item["line"])
	}
	if len(entries) != 1 || entries[0].Email != "d@example.com" || !reflect.DeepEqual(lines, []any{3, 4, 5, 6, 8}) || invalid[4]["error"] != "邮箱重复" {
		t.Fatalf("%+v %v", entries, invalid)
	}
}

func TestDescribeError(t *testing.T) {
	cases := map[string]map[string]any{
		"密码错误":          {"error": "invalid_password"},
		"二步验证码被拒绝（400）": {"error": "mfa_verify_failed_400"},
		"账号已被停用":        {"error": "password_verify_failed_403", "detail": map[string]any{"error": map[string]any{"code": "account_deactivated"}}},
		"weird":         {"error": "weird"},
	}
	for want, result := range cases {
		if got := DescribeError(result); got != want {
			t.Errorf("%v: %q", result, got)
		}
	}
}

func run(t *testing.T, s *Service, text string) map[string]any {
	t.Helper()
	started, err := s.Start(text, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Wait()
	return s.Progress(started["progress_id"].(string))
}

func TestSuccessAddsAccountWithCredentials(t *testing.T) {
	p := newPool(t)
	var calls [][]string
	var mu sync.Mutex
	s := &Service{Pool: p, Login: func(_ context.Context, email, password, totp, proxy string) (map[string]any, error) {
		mu.Lock()
		calls = append(calls, []string{email, password, totp, proxy})
		mu.Unlock()
		return ok("tok-1", map[string]any{"account_id": "acc-1"}), nil
	}}
	progress := run(t, s, "a@example.com----pw----"+rfcSecret)
	if !reflect.DeepEqual(calls, [][]string{{"a@example.com", "pw", rfcSecret, ""}}) || progress["status"] != "done" || progress["success"] != 1 || progress["failed"] != 0 {
		t.Fatalf("calls %v progress %v", calls, progress)
	}
	saved := p.GetAccount("tok-1")
	if saved["email"] != "a@example.com" || saved["password"] != "pw" || saved["totp_secret"] != rfcSecret || saved["status"] != "正常" ||
		saved["source_type"] != "password" || saved["account_id"] != "acc-1" {
		t.Fatalf("saved %v", saved)
	}
	if text := fmt.Sprint(progress); strings.Contains(text, "pw") || strings.Contains(text, rfcSecret) {
		t.Fatalf("progress leaks credentials: %s", text)
	}
}

func TestFailureAndErrorDoNotStopTheBatch(t *testing.T) {
	p := newPool(t)
	s := &Service{Pool: p, Login: func(_ context.Context, email, _, _, _ string) (map[string]any, error) {
		switch {
		case strings.HasPrefix(email, "bad"):
			return map[string]any{"ok": false, "error": "invalid_password"}, nil
		case strings.HasPrefix(email, "boom"):
			return nil, errors.New("network down")
		}
		return ok("tok-"+email, nil), nil
	}}
	progress := run(t, s, "bad@example.com----x\nboom@example.com----x\ngood@example.com----x")
	statuses := map[string][]any{}
	for _, raw := range progress["items"].([]any) {
		item := raw.(map[string]any)
		statuses[item["email"].(string)] = []any{item["status"], item["error"]}
	}
	if !reflect.DeepEqual(statuses["bad@example.com"], []any{"失败", "密码错误"}) || statuses["boom@example.com"][0] != "失败" ||
		!strings.Contains(statuses["boom@example.com"][1].(string), "network down") || statuses["good@example.com"][0] != "成功" {
		t.Fatalf("statuses %v", statuses)
	}
	if progress["success"] != 1 || progress["failed"] != 2 || progress["completed"] != 3 || p.GetAccount("tok-bad@example.com") != nil {
		t.Fatalf("progress %v", progress)
	}
}

func TestExistingEmailIsUpdatedInPlace(t *testing.T) {
	p := newPool(t, pool.Account{"access_token": "old-token", "email": "a@example.com", "status": "异常", "password": "old"})
	s := &Service{Pool: p, Login: func(context.Context, string, string, string, string) (map[string]any, error) {
		return ok("new-token", nil), nil
	}}
	run(t, s, "A@Example.com----newpw----"+rfcSecret)
	saved := p.GetAccount("new-token")
	if len(p.ListAccounts()) != 1 || saved["status"] != "正常" || saved["password"] != "newpw" || saved["totp_secret"] != rfcSecret {
		t.Fatalf("accounts %v", p.ListAccounts())
	}
}

func TestBatchLimitAndEmptyInput(t *testing.T) {
	s := &Service{Pool: newPool(t), Login: func(context.Context, string, string, string, string) (map[string]any, error) {
		return ok("t", nil), nil
	}}
	lines := []string{}
	for i := 0; i <= MaxBatch; i++ {
		lines = append(lines, fmt.Sprintf("u%d@example.com----pw", i))
	}
	if _, err := s.Start(strings.Join(lines, "\n"), "", nil); err == nil {
		t.Fatal("over the batch limit")
	}
	progress := run(t, s, "garbage")
	if progress["total"] != 0 || progress["status"] != "done" || len(progress["invalid"].([]map[string]any)) != 1 || s.Progress("missing") != nil {
		t.Fatalf("progress %v", progress)
	}
}

type reserver struct {
	mu       sync.Mutex
	released []string
}

func (r *reserver) Reserve(candidates []string, count int) ([]string, error) {
	out := []string{}
	for i := 0; i < count; i++ {
		out = append(out, candidates[i%len(candidates)])
	}
	return out, nil
}
func (r *reserver) Release(urls []string) {
	r.mu.Lock()
	r.released = append(r.released, urls...)
	r.mu.Unlock()
}

func TestDistributionOnlyGivesNewEmailsAProxyAndReleasesOnce(t *testing.T) {
	p := newPool(t, pool.Account{"access_token": "old", "email": "old@example.com", "proxy": "http://kept:1"})
	r := &reserver{}
	seen := map[string]string{}
	var mu sync.Mutex
	s := &Service{Pool: p, Proxies: r, Login: func(_ context.Context, email, _, _, proxy string) (map[string]any, error) {
		mu.Lock()
		seen[email] = proxy
		mu.Unlock()
		if strings.HasPrefix(email, "bad") {
			return map[string]any{"ok": false, "error": "invalid_password"}, nil
		}
		return ok("tok-"+email, nil), nil
	}}
	if _, err := s.Start("old@example.com----x\nnew@example.com----x\nbad@example.com----x", "", []string{"http://p:1"}); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	if seen["old@example.com"] != "http://kept:1" || seen["new@example.com"] != "http://p:1" || seen["bad@example.com"] != "http://p:1" {
		t.Fatalf("seen %v", seen)
	}
	if len(r.released) != 2 || p.GetAccount("tok-new@example.com")["proxy"] != "http://p:1" {
		t.Fatalf("released %v", r.released)
	}
}

func TestReloginPassesTheStoredSecretAndProxyAndDisablesDeactivated(t *testing.T) {
	p := newPool(t, pool.Account{"access_token": "t", "email": "a@example.com", "password": "pw", "totp_secret": rfcSecret, "status": "异常", "proxy": "http://1.2.3.4:8080"},
		pool.Account{"access_token": "gone", "email": "g@example.com", "password": "pw", "status": "正常"})
	var got []string
	relogin := p.PasswordRelogin(func(_ context.Context, email, password, totp, proxy string) (map[string]any, error) {
		got = []string{email, password, totp, proxy}
		if email == "g@example.com" {
			return map[string]any{"ok": false, "error": "password_verify_failed_403", "detail": map[string]any{"error": map[string]any{"code": "account_deactivated"}}}, nil
		}
		return ok("t2", nil), nil
	})
	relogin("t", "a@example.com", "pw", "manual", "", rfcSecret)
	if !reflect.DeepEqual(got, []string{"a@example.com", "pw", rfcSecret, "http://1.2.3.4:8080"}) {
		t.Fatalf("login args %v", got)
	}
	if account := p.GetAccount("t2"); account["status"] != "正常" || account["refresh_token"] != "rt-t2" {
		t.Fatalf("account %v", account)
	}
	relogin("gone", "g@example.com", "pw", "manual", "", "")
	if account := p.GetAccount("gone"); account["status"] != "禁用" {
		t.Fatalf("deactivated %v", account)
	}
}
