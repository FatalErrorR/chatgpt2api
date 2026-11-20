package pool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/settings"
)

// memoryStore is the MemoryStorage of the Python tests, plus a write counter.
type memoryStore struct {
	mu       sync.Mutex
	docs     map[string]Account
	order    []string
	ints     map[string]int
	writes   int
	lastSave map[string]Account
}

func newMemoryStore(accounts ...Account) *memoryStore {
	s := &memoryStore{docs: map[string]Account{}, ints: map[string]int{}}
	for _, account := range accounts {
		token := accountToken(account)
		s.docs[token] = account
		s.order = append(s.order, token)
	}
	return s
}

func (m *memoryStore) Load() ([]Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Account{}
	for _, token := range m.order {
		out = append(out, copyAccount(m.docs[token]))
	}
	return out, nil
}

func (m *memoryStore) Save(order []string, changed map[string]Account, removed []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes++
	m.lastSave = changed
	for _, token := range removed {
		delete(m.docs, token)
	}
	for token, account := range changed {
		raw, _ := json.Marshal(account)
		var copied Account
		json.Unmarshal(raw, &copied)
		m.docs[token] = copied
	}
	m.order = append([]string(nil), order...)
	return nil
}

func (m *memoryStore) LoadInt(key string) (int, bool) { n, ok := m.ints[key]; return n, ok }
func (m *memoryStore) SaveInt(key string, value int) error {
	m.ints[key] = value
	return nil
}

// echoRemote stands in for fetch_remote_info = lambda token: service.get_account(token).
type echoRemote struct {
	userInfo func(token string) (Account, error)
	refresh  func(refreshToken string) (map[string]string, error)
}

func (r *echoRemote) UserInfo(_ context.Context, account Account) (Account, error) {
	if r.userInfo != nil {
		return r.userInfo(str(account["access_token"]))
	}
	return Account{}, nil
}

func (r *echoRemote) RefreshAccessToken(_ context.Context, refreshToken string, _ Account) (map[string]string, error) {
	if r.refresh != nil {
		return r.refresh(refreshToken)
	}
	return nil, errors.New("no refresh")
}

type recordingLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *recordingLog) Add(kind, summary string, detail map[string]any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, summary)
	return nil
}

func newService(t *testing.T, cfg map[string]any, accounts ...Account) (*Service, *memoryStore) {
	t.Helper()
	store := newMemoryStore(accounts...)
	s, err := New(store, settings.NewForTest(cfg, ""), &recordingLog{})
	if err != nil {
		t.Fatal(err)
	}
	s.Remote = &echoRemote{}
	return s, store
}

func makeJWT(payload map[string]any) string {
	encode := func(value map[string]any) string {
		raw, _ := json.Marshal(value)
		return strings.TrimRight(base64.URLEncoding.EncodeToString(raw), "=")
	}
	return encode(map[string]any{"alg": "none", "typ": "JWT"}) + "." + encode(payload) + ".sig"
}

// --- test_account_export.py ---

func TestExportUsesCodexShapeAndJWTClaims(t *testing.T) {
	access := makeJWT(map[string]any{"exp": 0, "iat": 3600,
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct_123"},
		"https://api.openai.com/profile": map[string]any{"email": "test@example.com"}})
	id := makeJWT(map[string]any{"email": "fallback@example.com"})
	s, _ := newService(t, nil, Account{"access_token": access, "id_token": id, "refresh_token": "rt_test"})
	items := s.BuildExportItems([]string{access})
	if len(items) != 1 {
		t.Fatalf("items: %v", items)
	}
	item := items[0]
	want := map[string]any{"type": "codex", "email": "test@example.com", "expired": "1970-01-01T08:00:00+08:00", "account_id": "acct_123",
		"access_token": access, "last_refresh": "1970-01-01T09:00:00+08:00", "id_token": id, "refresh_token": "rt_test"}
	for key, value := range want {
		if item[key] != value {
			t.Errorf("%s: got %v want %v", key, item[key], value)
		}
	}
}

func TestExportSkipsIncompleteAccounts(t *testing.T) {
	access := makeJWT(map[string]any{"exp": 0})
	id := makeJWT(map[string]any{"email": "complete@example.com"})
	s, _ := newService(t, nil, Account{"access_token": "only_access"}, Account{"access_token": "missing_id", "refresh_token": "rt"},
		Account{"access_token": access, "id_token": id, "refresh_token": "rt_complete"})
	items := s.BuildExportItems(nil)
	if len(items) != 1 || items[0]["access_token"] != access || items[0]["refresh_token"] != "rt_complete" {
		t.Fatalf("items: %v", items)
	}
}

func TestAddItemsKeepsExportFieldsWithoutTakingPlanType(t *testing.T) {
	s, _ := newService(t, nil)
	result := s.AddAccountItems([]Account{{"type": "codex", "access_token": "access_token_test", "refresh_token": "rt_test", "account_id": "acct_123"}})
	account := s.GetAccount("access_token_test")
	if result.Added != 1 || account["type"] != "free" || account["export_type"] != "codex" || account["refresh_token"] != "rt_test" || account["account_id"] != "acct_123" || account["source_type"] != "codex" {
		t.Fatalf("result %+v account %v", result, account)
	}
}

// --- test_account_image_capabilities.py ---

func TestImageAccountsNeedPositiveQuota(t *testing.T) {
	if isImageAccountAvailable(Account{"status": StatusLimited, "quota": 1}) || isImageAccountAvailable(Account{"status": StatusNormal, "quota": 0}) || !isImageAccountAvailable(Account{"status": StatusNormal, "quota": 1}) {
		t.Fatal("availability rules")
	}
}

func TestAccountTypeAliases(t *testing.T) {
	for in, want := range map[string]string{"prolite": "ProLite", "pro_lite": "ProLite", "PRO-LITE": "ProLite", "business": "Team", "plus": "Plus", "Free": "free", "weird": "weird", "": ""} {
		if got := NormalizeAccountType(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestMarkImageResultConsumesQuota(t *testing.T) {
	s, _ := newService(t, nil)
	s.AddAccounts([]string{"token-1"}, "web")
	s.UpdateAccount("token-1", Account{"status": StatusNormal, "quota": 1}, false)
	updated := s.MarkImageResult("token-1", true)
	if updated["quota"] != 0 || updated["status"] != StatusLimited || updated["success"] != 1 {
		t.Fatalf("updated: %v", updated)
	}
}

func TestAvailableTokenFiltersByPlanType(t *testing.T) {
	s, _ := newService(t, nil, Account{"access_token": "token-plus", "type": "Plus", "status": StatusNormal, "quota": 3},
		Account{"access_token": "token-pro", "type": "Pro", "status": StatusNormal, "quota": 3})
	ctx := context.Background()
	plus, err := s.GetAvailableAccessToken(ctx, Filter{PlanType: "plus"}, false)
	if err != nil {
		t.Fatal(err)
	}
	pro, err := s.GetAvailableAccessToken(ctx, Filter{PlanType: "pro"}, false)
	if err != nil {
		t.Fatal(err)
	}
	s.ReleaseImageSlot(plus)
	s.ReleaseImageSlot(pro)
	if plus != "token-plus" || pro != "token-pro" {
		t.Fatalf("plus %s pro %s", plus, pro)
	}
}

func TestAbnormalAccountKeepsErrorText(t *testing.T) {
	s, _ := newService(t, map[string]any{"auto_remove_invalid_accounts": false}, Account{"access_token": "bad-token", "status": StatusNormal, "quota": 1})
	s.RemoveInvalidToken("bad-token", "image_stream", false, "401 invalid access token")
	account := s.GetAccount("bad-token")
	if account["status"] != StatusAbnormal || account["last_refresh_error"] != "401 invalid access token" || account["quota"] != 0 {
		t.Fatalf("account: %v", account)
	}
}

func invalidRemote() *echoRemote {
	return &echoRemote{userInfo: func(string) (Account, error) {
		return nil, fmt.Errorf("token invalidated (/backend-api/me): %w", ErrInvalidToken)
	}}
}

func TestRefreshCanRemoveInvalidTokenWithoutDelay(t *testing.T) {
	s, _ := newService(t, map[string]any{"auto_remove_invalid_accounts": true}, Account{"access_token": "invalid-token", "status": StatusNormal})
	s.Remote = invalidRemote()
	result := s.RefreshAccounts(context.Background(), []string{"invalid-token"}, "", false)
	if result.Refreshed != 0 || len(result.Errors) != 1 || len(result.Items) != 0 || s.GetAccount("invalid-token") != nil {
		t.Fatalf("result %+v", result)
	}
}

func TestRefreshDefersInvalidRemovalByDefault(t *testing.T) {
	s, _ := newService(t, map[string]any{"auto_remove_invalid_accounts": true}, Account{"access_token": "invalid-token", "status": StatusNormal})
	s.Remote = invalidRemote()
	result := s.RefreshAccounts(context.Background(), []string{"invalid-token"}, "", true)
	account := s.GetAccount("invalid-token")
	if result.Refreshed != 0 || len(result.Errors) != 1 || account == nil || account["invalid_count"] != 1 {
		t.Fatalf("result %+v account %v", result, account)
	}
}

func TestAnonymizeTokenHidesValue(t *testing.T) {
	ref := AnonymizeToken("super-secret-token")
	if !strings.HasPrefix(ref, "token:") || strings.Contains(ref, "super-secret") || len(ref) != len("token:")+10 {
		t.Fatal(ref)
	}
	if AnonymizeToken("") != "token:empty" {
		t.Fatal("empty")
	}
}

// --- test_edit_upload_cooldown.py ---

const sample429 = `/backend-api/files failed: status=429, body={"detail": "已达到文件上传上限，请 3 小时内重试"}`

func TestParseEditUploadCooldown(t *testing.T) {
	cases := map[string]time.Duration{
		"请 3 小时内重试": 3 * time.Hour, "请16分钟内重试": 16 * time.Minute, sample429: 3 * time.Hour,
		"请 48 小时内重试": maxEditUploadCooldown, "no retry hint": defaultEditUploadCooldown, "请 0 小时内重试": defaultEditUploadCooldown,
	}
	for in, want := range cases {
		if got := ParseEditUploadCooldown(in); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
	if !IsEditUploadThrottled(sample429) || IsEditUploadThrottled("/backend-api/f/conversation failed: status=413") || IsEditUploadThrottled("status=429, body=rate limited") || !IsEditUploadThrottled("You've hit the File upload limit") {
		t.Fatal("throttle detection")
	}
}

func TestEditsSkipCooledAccountGenerationsDoNot(t *testing.T) {
	until := isoUTC(time.Now().Add(3 * time.Hour))
	s, _ := newService(t, map[string]any{"image_edit_upload_cooldown_enabled": true},
		Account{"access_token": "token-cool", "type": "free", "status": StatusNormal, "quota": 11, "edit_upload_blocked_until": until},
		Account{"access_token": "token-ok", "type": "Plus", "status": StatusNormal, "quota": 11})
	ctx := context.Background()
	edits, err := s.GetAvailableAccessToken(ctx, Filter{}, true)
	if err != nil || edits != "token-ok" {
		t.Fatalf("edits: %s %v", edits, err)
	}
	s.ReleaseImageSlot(edits)
	gen, err := s.GetAvailableAccessToken(ctx, Filter{Excluded: map[string]bool{"token-ok": true}}, false)
	if err != nil || gen != "token-cool" {
		t.Fatalf("generations: %s %v", gen, err)
	}
	s.ReleaseImageSlot(gen)
	if _, err := s.GetAvailableAccessToken(ctx, Filter{Excluded: map[string]bool{"token-ok": true}}, true); err == nil {
		t.Fatal("only a cooled account left: edits must fail")
	}
	assertNoSlots(t, s)
}

func TestMarkCooldownSurvivesRefresh(t *testing.T) {
	s, _ := newService(t, nil, Account{"access_token": "token-plus", "type": "Plus", "status": StatusNormal, "quota": 5})
	before := time.Now().UTC()
	until := s.MarkEditUploadCooldown("token-plus", sample429)
	parsed, ok := parseTime(until)
	if !ok || parsed.Before(before.Add(2*time.Hour+50*time.Minute)) || parsed.After(before.Add(3*time.Hour+10*time.Minute)) {
		t.Fatalf("until %s", until)
	}
	s.UpdateAccount("token-plus", Account{"email": "plus@example.com", "quota": 5, "status": StatusNormal, "type": "Plus"}, false)
	account := s.GetAccount("token-plus")
	if account["edit_upload_blocked_until"] != until || account["email"] != "plus@example.com" {
		t.Fatalf("account %v", account)
	}
}

// --- Go-side checks ---

func assertNoSlots(t *testing.T, s *Service) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.inflight) != 0 {
		t.Fatalf("slots still held: %v", s.inflight)
	}
}

func TestRoundRobinAndConcurrencyLimit(t *testing.T) {
	s, _ := newService(t, map[string]any{"image_account_concurrency": 1},
		Account{"access_token": "a", "status": StatusNormal, "quota": 5}, Account{"access_token": "b", "status": StatusNormal, "quota": 5})
	ctx := context.Background()
	first, _ := s.GetAvailableAccessToken(ctx, Filter{}, false)
	second, _ := s.GetAvailableAccessToken(ctx, Filter{}, false)
	if first == second {
		t.Fatalf("round robin gave %s twice", first)
	}
	// both are at the limit of 1: the next request waits until a slot frees up
	got := make(chan string, 1)
	go func() {
		token, _ := s.GetAvailableAccessToken(ctx, Filter{}, false)
		got <- token
	}()
	select {
	case token := <-got:
		t.Fatalf("got %s while both accounts were busy", token)
	case <-time.After(200 * time.Millisecond):
	}
	s.MarkImageResult(first, true)
	select {
	case token := <-got:
		if token != first {
			t.Fatalf("expected the freed account %s, got %s", first, token)
		}
		s.ReleaseImageSlot(token)
	case <-time.After(3 * time.Second):
		t.Fatal("waiter never woke up")
	}
	s.ReleaseImageSlot(second)
	assertNoSlots(t, s)
}

func TestNoQuotaMessages(t *testing.T) {
	s, _ := newService(t, nil, Account{"access_token": "a", "status": StatusLimited, "quota": 0})
	_, err := s.GetAvailableAccessToken(context.Background(), Filter{}, false)
	if err == nil || err.Error() != "no available image quota" {
		t.Fatalf("err %v", err)
	}
	_, err = s.GetAvailableAccessToken(context.Background(), Filter{PlanType: "plus"}, false)
	if err == nil || err.Error() != "no available plus image quota" {
		t.Fatalf("err %v", err)
	}
}

func TestSidelinedAccountIsSkipped(t *testing.T) {
	s, _ := newService(t, nil, Account{"access_token": "a", "status": StatusNormal, "quota": 5}, Account{"access_token": "b", "status": StatusNormal, "quota": 5})
	s.SidelineImageAccount("a", time.Minute)
	for i := 0; i < 3; i++ {
		token, err := s.GetAvailableAccessToken(context.Background(), Filter{}, false)
		if err != nil || token != "b" {
			t.Fatalf("got %s %v", token, err)
		}
		s.ReleaseImageSlot(token)
	}
}

func TestReloadKeepsOrderAndFields(t *testing.T) {
	s, store := newService(t, nil)
	s.AddAccountItems([]Account{{"access_token": "z", "custom_field": "kept", "quota": 2}, {"access_token": "a"}})
	again, err := New(store, settings.NewForTest(nil, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	tokens := again.ListTokens()
	if strings.Join(tokens, ",") != "z,a" || again.GetAccount("z")["custom_field"] != "kept" || intOf(again.GetAccount("z")["quota"]) != 2 {
		t.Fatalf("tokens %v account %v", tokens, again.GetAccount("z"))
	}
}

func TestOnlyChangedAccountsAreWritten(t *testing.T) {
	s, store := newService(t, nil)
	s.AddAccounts([]string{"a", "b", "c"}, "web")
	s.UpdateAccount("b", Account{"quota": 9}, true)
	if len(store.lastSave) != 1 || store.lastSave["b"] == nil {
		t.Fatalf("last save wrote %d accounts", len(store.lastSave))
	}
	writes := store.writes
	s.UpdateAccount("b", Account{"quota": 9}, true)
	if store.writes != writes {
		t.Fatal("an unchanged account was written again")
	}
}

func TestTokenRotationMovesSlotsAndAlias(t *testing.T) {
	old := makeJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix()}) // expires within 24h
	s, _ := newService(t, nil, Account{"access_token": old, "refresh_token": "rt", "status": StatusNormal, "quota": 3}, Account{"access_token": "other"})
	s.Remote = &echoRemote{refresh: func(string) (map[string]string, error) {
		return map[string]string{"access_token": "new-token", "refresh_token": "rt2"}, nil
	}}
	s.mu.Lock()
	s.inflight[old] = 1
	s.mu.Unlock()
	got := s.RefreshAccessToken(context.Background(), old, false, "test")
	if got != "new-token" || s.ResolveAccessToken(old) != "new-token" || s.ImageInflight(old) != 1 {
		t.Fatalf("got %s resolve %s inflight %d", got, s.ResolveAccessToken(old), s.ImageInflight(old))
	}
	if order := s.ListTokens(); strings.Join(order, ",") != "other,new-token" {
		t.Fatalf("order %v: the rotated token goes to the end, as with the Python dict", order)
	}
	if account := s.GetAccount(old); account["refresh_token"] != "rt2" || account["last_token_refresh_at"] == nil {
		t.Fatalf("account %v", account)
	}
}

func TestStatsCountsStatuses(t *testing.T) {
	s, _ := newService(t, nil, Account{"access_token": "a", "status": StatusNormal, "quota": 4, "success": 2},
		Account{"access_token": "b", "status": StatusLimited, "quota": 9, "fail": 1}, Account{"access_token": "c", "status": StatusAbnormal, "type": "Plus"})
	stats := s.Stats()
	if stats["total"] != 3 || stats["active"] != 1 || stats["limited"] != 1 || stats["abnormal"] != 1 || stats["total_quota"] != 4 || stats["total_success"] != 2 || stats["total_fail"] != 1 {
		t.Fatalf("stats %v", stats)
	}
	if byType := stats["by_type"].(map[string]any); byType["free"] != 2 || byType["Plus"] != 1 {
		t.Fatalf("by_type %v", byType)
	}
}

// test_image_stream_retry.py ImageForbiddenTests.test_same_proxy_tokens_only_groups_accounts_with_that_proxy
func TestSameProxyTokensOnlyGroupsThatProxy(t *testing.T) {
	s, _ := newService(t, nil, Account{"access_token": "a", "proxy": "http://1.1.1.1:80"}, Account{"access_token": "b", "proxy": "http://1.1.1.1:80"},
		Account{"access_token": "c", "proxy": "http://2.2.2.2:80"}, Account{"access_token": "d"}, Account{"access_token": "e"})
	for token, want := range map[string][]string{"a": {"a", "b"}, "c": {"c"}, "d": {}} {
		got := s.SameProxyTokens(token)
		if len(got) != len(want) {
			t.Fatalf("%s: %v", token, got)
		}
		for _, other := range want {
			if !got[other] {
				t.Fatalf("%s: %v", token, got)
			}
		}
	}
}
