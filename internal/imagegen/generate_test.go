package imagegen

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

// Ported from test/test_image_stream_retry.py. fakeAccounts plays the mocked account_service:
// get_available_access_token hands out the listed tokens in order (an error entry raises).

type call struct {
	token   string
	success bool
}

type fakeAccounts struct {
	mu        sync.Mutex
	tokens    []any // string or error
	calls     int
	excluded  []map[string]bool
	marks     []call
	releases  []string
	sidelined []string
	sideFor   []time.Duration
	sameProxy map[string]bool
	refreshed string
	removed   []string
	emails    bool
}

func (f *fakeAccounts) GetAvailableAccessToken(_ context.Context, filter pool.Filter, _ bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	excluded := map[string]bool{}
	for token := range filter.Excluded {
		excluded[token] = true
	}
	f.excluded = append(f.excluded, excluded)
	if f.calls >= len(f.tokens) {
		f.calls++
		return "", ErrNoMoreTokens
	}
	next := f.tokens[f.calls]
	f.calls++
	if err, ok := next.(error); ok {
		return "", err
	}
	return next.(string), nil
}

var ErrNoMoreTokens = errors.New("no available image quota")

func (f *fakeAccounts) GetAccount(token string) pool.Account {
	if f.emails {
		return pool.Account{"email": token + "@example.com"}
	}
	return pool.Account{}
}
func (f *fakeAccounts) MarkImageResult(token string, success bool) pool.Account {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.marks = append(f.marks, call{token, success})
	return nil
}
func (f *fakeAccounts) ReleaseImageSlot(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases = append(f.releases, token)
}
func (f *fakeAccounts) SidelineImageAccount(token string, d time.Duration) {
	f.sidelined = append(f.sidelined, token)
	f.sideFor = append(f.sideFor, d)
}
func (f *fakeAccounts) SameProxyTokens(string) map[string]bool { return f.sameProxy }
func (f *fakeAccounts) RefreshAccessToken(_ context.Context, token string, _ bool, _ string) string {
	if f.refreshed != "" {
		return f.refreshed
	}
	return token
}
func (f *fakeAccounts) RemoveInvalidToken(token, _ string, _ bool, _ string) bool {
	f.removed = append(f.removed, token)
	return false
}
func (f *fakeAccounts) MarkEditUploadCooldown(string, any) string { return "" }

type nopBackend struct{ token string }

func (nopBackend) StreamPicture(context.Context, string, string, []string, string) (<-chan string, func() error, error) {
	return nil, nil, nil
}
func (nopBackend) QueryTasks(context.Context, string, time.Duration) ([]map[string]any, error) {
	return nil, nil
}
func (nopBackend) CheckTaskError(map[string]any) (bool, string) { return false, "" }
func (nopBackend) FindConversationByPrompt(context.Context, string, time.Time, time.Duration) (string, error) {
	return "", nil
}
func (nopBackend) ResolveImageURLs(context.Context, string, []string, []string, float64, bool) ([]string, error) {
	return nil, nil
}
func (nopBackend) PollImageResults(context.Context, string, float64, []string, []string) ([]string, []string, error) {
	return nil, nil, nil
}
func (nopBackend) DownloadImages(context.Context, []string) ([][]byte, error) { return nil, nil }
func (nopBackend) DeleteConversation(context.Context, string) error           { return nil }
func (nopBackend) Close()                                                     {}

func newGenerator(cfg map[string]any, accounts *fakeAccounts) *Generator {
	p := NewPipeline(settings.NewForTest(cfg, ""), Hooks{})
	p.Sleep = func(context.Context, time.Duration) {}
	p.Background = func(func()) {}
	return &Generator{Pipeline: p, Accounts: accounts, NewBackend: func(token string, _ pool.Account) (Backend, error) {
		return nopBackend{token: token}, nil
	}}
}

// generate is ImageRetryTestCase._generate: each attempt raises the next error, then succeeds.
func generate(t *testing.T, errs []error, tokens []any, sameProxy ...string) ([]Output, *fakeAccounts) {
	t.Helper()
	if tokens == nil {
		tokens = []any{"one", "two", "three", "four", "five", "six"}
	}
	accounts := &fakeAccounts{tokens: tokens, sameProxy: map[string]bool{}}
	for _, token := range sameProxy {
		accounts.sameProxy[token] = true
	}
	g := newGenerator(nil, accounts)
	g.Stream = func(_ context.Context, _ Backend, req Request, index, total int, emit func(Output)) error {
		if len(errs) > 0 {
			err := errs[0]
			errs = errs[1:]
			return err
		}
		out := newOutput("result", req, index, total)
		emit(out)
		return nil
	}
	result, err := g.GenerateSingleImage(context.Background(), Request{Model: "gpt-image-2", Prompt: "cat"}, 1, 1)
	if err != nil {
		return nil, accounts
	}
	return result, accounts
}

func modelNotFound() error {
	return &UpstreamHTTPError{Context: "/backend-api/f/conversation", StatusCode: 404, Body: map[string]any{"detail": "Model not found"}}
}

// --- ImageModelNotFoundTests ---

func TestOnlyExactImageConversationErrorMatches(t *testing.T) {
	cases := []struct {
		endpoint string
		status   int
		body     any
		want     bool
	}{
		{"/backend-api/f/conversation", 404, map[string]any{"detail": "Model not found"}, true},
		{"/backend-api/conversation", 404, map[string]any{"detail": "Model not found"}, false},
		{"/backend-api/f/conversation/prepare", 404, map[string]any{"detail": "Model not found"}, false},
		{"/backend-api/f/conversation", 403, map[string]any{"detail": "Model not found"}, false},
		{"/backend-api/f/conversation", 404, map[string]any{"detail": "Not Found"}, false},
		{"/backend-api/f/conversation", 404, map[string]any{"detail": " Model not found "}, false},
		{"/backend-api/f/conversation", 404, "", false},
	}
	for _, c := range cases {
		if got := IsImageModelNotFound(&UpstreamHTTPError{Context: c.endpoint, StatusCode: c.status, Body: c.body}); got != c.want {
			t.Errorf("%s %d %v: got %v", c.endpoint, c.status, c.body, got)
		}
	}
	if IsImageModelNotFound(errors.New("Model not found")) {
		t.Fatal("a plain error must not match")
	}
}

func TestDisguised404MovesToNextAccount(t *testing.T) {
	result, accounts := generate(t, []error{modelNotFound()}, nil)
	if len(result) != 1 || accounts.calls != 2 || !reflect.DeepEqual(accounts.sidelined, []string{"one"}) || accounts.sideFor[0] != 10*time.Minute {
		t.Fatalf("result %v calls %d sidelined %v", result, accounts.calls, accounts.sidelined)
	}
	if len(accounts.removed) != 0 {
		t.Fatal("a 404 must not remove the account")
	}
}

func TestOther404IsNotRetried(t *testing.T) {
	result, accounts := generate(t, []error{&UpstreamHTTPError{Context: "/backend-api/f/conversation", StatusCode: 404, Body: map[string]any{"detail": "Not Found"}}}, nil)
	if result != nil || accounts.calls != 1 || len(accounts.sidelined) != 0 {
		t.Fatalf("result %v calls %d", result, accounts.calls)
	}
}

func TestModelNotFoundFailoverIsBounded(t *testing.T) {
	errs := []error{}
	for i := 0; i < 6; i++ {
		errs = append(errs, modelNotFound())
	}
	result, accounts := generate(t, errs, nil)
	if result != nil || accounts.calls != 5 || len(accounts.sidelined) != 5 {
		t.Fatalf("calls %d sidelined %d", accounts.calls, len(accounts.sidelined))
	}
}

// --- ImageForbiddenTests ---

const challenge = "<html><body>Enable JavaScript and cookies to continue</body></html>"

func TestForbiddenCoolsTheAccountAndSkipsItsEgress(t *testing.T) {
	result, accounts := generate(t, []error{&UpstreamHTTPError{Context: "bootstrap", StatusCode: 403, Body: challenge}}, nil, "one", "sibling")
	if len(result) != 1 || !reflect.DeepEqual(accounts.sidelined, []string{"one"}) || accounts.sideFor[0] != 10*time.Minute {
		t.Fatalf("sidelined %v", accounts.sidelined)
	}
	if len(accounts.excluded[0]) != 0 || !reflect.DeepEqual(accounts.excluded[1], map[string]bool{"one": true, "sibling": true}) {
		t.Fatalf("excluded %v", accounts.excluded)
	}
	if len(accounts.removed) != 0 {
		t.Fatal("403 must not remove the account")
	}
}

func TestForbiddenFallsBackToTheSameEgressWhenNothingElseIsFree(t *testing.T) {
	result, accounts := generate(t, []error{&UpstreamHTTPError{Context: "chat_requirements_prepare", StatusCode: 403, Body: challenge}},
		[]any{"one", errors.New("no available image quota"), "sibling"}, "one", "sibling")
	if len(result) != 1 || !reflect.DeepEqual(accounts.excluded[1], map[string]bool{"one": true, "sibling": true}) || len(accounts.excluded[2]) != 0 {
		t.Fatalf("excluded %v", accounts.excluded)
	}
}

func TestForbiddenRetriesAreBounded(t *testing.T) {
	errs := []error{}
	for i := 0; i < 6; i++ {
		errs = append(errs, &UpstreamHTTPError{Context: "bootstrap", StatusCode: 403, Body: challenge})
	}
	result, accounts := generate(t, errs, nil)
	if result != nil || accounts.calls != 4 || len(accounts.sidelined) != 4 {
		t.Fatalf("calls %d sidelined %d", accounts.calls, len(accounts.sidelined))
	}
}

// --- ImageStreamResetTests ---

var resetErr = errors.New("Failed to perform, curl: (92) HTTP/2 stream 1 was not closed cleanly: PROTOCOL_ERROR (err 1).")

func TestDetectsOnlyHTTP2StreamResets(t *testing.T) {
	if !IsStreamResetError(resetErr.Error()) || IsStreamResetError("curl: (28) Operation timed out") {
		t.Fatal("detection")
	}
	if ImageStreamErrorMessage(resetErr.Error()) != "upstream connection was interrupted, please retry later" {
		t.Fatal("message")
	}
}

func TestResetSwitchesToAnotherAccount(t *testing.T) {
	result, accounts := generate(t, []error{resetErr}, nil)
	if len(result) != 1 || accounts.calls != 2 || !reflect.DeepEqual(accounts.excluded[1], map[string]bool{"one": true}) {
		t.Fatalf("calls %d excluded %v", accounts.calls, accounts.excluded)
	}
	if accounts.marks[0] != (call{"one", false}) {
		t.Fatalf("marks %v", accounts.marks)
	}
}

func TestResetRetriesAtMostTwice(t *testing.T) {
	result, accounts := generate(t, []error{resetErr, resetErr, resetErr, resetErr, resetErr}, nil)
	if result != nil || accounts.calls != 3 {
		t.Fatalf("calls %d", accounts.calls)
	}
}

func TestBreakerStopsRetryingDuringAWideOutage(t *testing.T) {
	accounts := &fakeAccounts{tokens: []any{"one", "two"}, sameProxy: map[string]bool{}}
	g := newGenerator(nil, accounts)
	for i := 0; i < streamResetBreakerLimit; i++ {
		g.breaker.times = append(g.breaker.times, time.Now())
	}
	g.Stream = func(context.Context, Backend, Request, int, int, func(Output)) error { return resetErr }
	if _, err := g.GenerateSingleImage(context.Background(), Request{Model: "gpt-image-2", Prompt: "cat"}, 1, 1); err == nil || accounts.calls != 1 {
		t.Fatalf("err %v calls %d", err, accounts.calls)
	}
}

func TestOldResetsFallOutOfTheBreakerWindow(t *testing.T) {
	accounts := &fakeAccounts{tokens: []any{"one", "two"}, sameProxy: map[string]bool{}}
	g := newGenerator(nil, accounts)
	for i := 0; i < 50; i++ {
		g.breaker.times = append(g.breaker.times, time.Now().Add(-streamResetBreakerWindow-time.Second))
	}
	first := true
	g.Stream = func(_ context.Context, _ Backend, req Request, index, total int, emit func(Output)) error {
		if first {
			first = false
			return resetErr
		}
		emit(newOutput("result", req, index, total))
		return nil
	}
	if result, err := g.GenerateSingleImage(context.Background(), Request{Model: "gpt-image-2", Prompt: "cat"}, 1, 1); err != nil || len(result) != 1 {
		t.Fatalf("err %v", err)
	}
}

// --- ImageFailoverTests ---

type seenAttempt struct {
	token   string
	timeout int
}

func runFailover(t *testing.T, stuck map[string]bool, enabled bool, failoverSecs, totalSecs, pollTimeout int) ([]Output, error, []seenAttempt, *fakeAccounts) {
	t.Helper()
	accounts := &fakeAccounts{tokens: []any{"one", "two", "three", "four", "five", "six", "seven"}, emails: true, sameProxy: map[string]bool{}}
	g := newGenerator(map[string]any{"image_failover_enabled": enabled, "image_failover_secs": failoverSecs, "image_poll_timeout_secs": totalSecs}, accounts)
	seen := []seenAttempt{}
	g.Stream = func(_ context.Context, backend Backend, req Request, index, total int, emit func(Output)) error {
		token := backend.(nopBackend).token
		seen = append(seen, seenAttempt{token, req.FailoverTimeoutSecs})
		// as in production: progress first, then the wait for the image times out
		progress := newOutput("progress", req, index, total)
		progress.ConversationID = "conv"
		emit(progress)
		if stuck[token] {
			return &PollTimeoutError{Message: "ChatGPT 生图超时（已等待 60 秒）。", ConversationID: "conv-stuck"}
		}
		result := newOutput("result", req, index, total)
		result.Data = []map[string]any{{"url": "x"}}
		emit(result)
		return nil
	}
	result, err := g.GenerateSingleImage(context.Background(), Request{Model: "gpt-image-2", Prompt: "cat", PollTimeoutSecs: pollTimeout}, 1, 1)
	return result, err, seen, accounts
}

func tokens(names ...string) map[string]bool {
	out := map[string]bool{}
	for _, name := range names {
		out[name] = true
	}
	return out
}

func TestFailoverSwitchesAccountWhenOneIsStuckAfterProgress(t *testing.T) {
	result, err, seen, accounts := runFailover(t, tokens("one"), true, 60, 600, 0)
	if err != nil || result[len(result)-1].Kind != "result" || !reflect.DeepEqual(seen, []seenAttempt{{"one", 60}, {"two", 60}}) {
		t.Fatalf("err %v seen %v", err, seen)
	}
	if !accounts.excluded[1]["one"] {
		t.Fatal("picked the stuck account again")
	}
	for _, mark := range accounts.marks {
		if mark == (call{"one", false}) {
			t.Fatal("being stuck counted as the account's failure")
		}
	}
}

func TestFailoverOffKeepsTheSingleLongWait(t *testing.T) {
	_, err, seen, _ := runFailover(t, tokens("one"), false, 60, 600, 0)
	if err == nil || !reflect.DeepEqual(seen, []seenAttempt{{"one", 0}}) {
		t.Fatalf("err %v seen %v", err, seen)
	}
}

func TestFailoverTotalWaitIsCappedByThePollTimeout(t *testing.T) {
	_, err, seen, _ := runFailover(t, tokens("one", "two", "three"), true, 60, 100, 0)
	if !reflect.DeepEqual(seen, []seenAttempt{{"one", 60}, {"two", 40}}) || err == nil || !strings.Contains(err.Error(), "换了 2 个账号") || strings.Contains(err.Error(), "image_poll_timeout_secs") {
		t.Fatalf("err %v seen %v", err, seen)
	}
}

func TestFailoverSwitchesAtMostFourTimes(t *testing.T) {
	_, err, seen, accounts := runFailover(t, tokens("one", "two", "three", "four", "five", "six", "seven"), true, 30, 600, 0)
	names := []string{}
	for _, s := range seen {
		names = append(names, s.token)
	}
	if !reflect.DeepEqual(names, []string{"one", "two", "three", "four", "five"}) || !strings.Contains(err.Error(), "换了 5 个账号") {
		t.Fatalf("seen %v err %v", names, err)
	}
	if accounts.marks[len(accounts.marks)-1] != (call{"five", false}) {
		t.Fatalf("marks %v", accounts.marks)
	}
}

func TestExplicitPollTimeoutDisablesFailover(t *testing.T) {
	_, err, seen, _ := runFailover(t, tokens("one"), true, 60, 600, 90)
	if err == nil || !reflect.DeepEqual(seen, []seenAttempt{{"one", 0}}) {
		t.Fatalf("err %v seen %v", err, seen)
	}
}

// --- ImageSlotReleaseTests: every acquired slot comes back exactly once ---

func assertEverySlotReleased(t *testing.T, accounts *fakeAccounts, seen []seenAttempt) {
	t.Helper()
	returned := []string{}
	for _, mark := range accounts.marks {
		returned = append(returned, mark.token)
	}
	returned = append(returned, accounts.releases...)
	acquired := []string{}
	for _, s := range seen {
		acquired = append(acquired, s.token)
	}
	sort.Strings(returned)
	sort.Strings(acquired)
	if !reflect.DeepEqual(returned, acquired) {
		t.Fatalf("took %v, gave back %v", acquired, returned)
	}
}

func TestFailoverSwitchReleasesTheStuckAccountsSlot(t *testing.T) {
	_, err, seen, accounts := runFailover(t, tokens("one", "two"), true, 60, 600, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertEverySlotReleased(t, accounts, seen)
	if !reflect.DeepEqual(accounts.releases, []string{"one", "two"}) {
		t.Fatalf("releases %v", accounts.releases)
	}
}

func TestFailoverExhaustedReleasesEverySlot(t *testing.T) {
	_, err, seen, accounts := runFailover(t, tokens("one", "two", "three", "four", "five", "six", "seven"), true, 30, 600, 0)
	if err == nil {
		t.Fatal("expected an error")
	}
	assertEverySlotReleased(t, accounts, seen)
}

func TestPlainTimeoutAndSuccessReleaseEverySlot(t *testing.T) {
	for _, c := range []struct {
		stuck   map[string]bool
		enabled bool
	}{{tokens("one"), false}, {tokens(), true}, {tokens(), false}} {
		_, _, seen, accounts := runFailover(t, c.stuck, c.enabled, 60, 600, 0)
		assertEverySlotReleased(t, accounts, seen)
	}
}

// --- ImageFailoverConfigTests ---

func TestFailoverSettingDefaultsAndBounds(t *testing.T) {
	for _, c := range []struct {
		raw     map[string]any
		enabled bool
		secs    int
	}{
		{map[string]any{}, false, 60},
		{map[string]any{"image_failover_enabled": "true", "image_failover_secs": "45"}, true, 45},
		{map[string]any{"image_failover_enabled": true, "image_failover_secs": 3}, true, 15},
		{map[string]any{"image_failover_secs": 9999}, false, 600},
		{map[string]any{"image_failover_secs": "abc"}, false, 60},
	} {
		store := settings.NewForTest(c.raw, "")
		if store.ImageFailoverEnabled() != c.enabled || store.ImageFailoverSecs() != c.secs {
			t.Errorf("%v: %v %d", c.raw, store.ImageFailoverEnabled(), store.ImageFailoverSecs())
		}
	}
}
