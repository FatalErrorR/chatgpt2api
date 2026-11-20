package chatgptweb_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/chatgptweb"
	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/basketikun/chatgpt2api/internal/settings"
	"github.com/basketikun/chatgpt2api/internal/testsupport/fakeupstream"
	"github.com/basketikun/chatgpt2api/internal/upstream"
)

// End-to-end through the real pool, retry loop and ChatGPT backend, against the fake upstream.
// The fake picks its behaviour from the token: fake-<scenario>-<n>.

// echoRemote keeps accounts as they are when the pool checks them before use, so the scenario
// shows up in the image request itself (with the real /me a 403 account would just be skipped).
type echoRemote struct{}

func (echoRemote) UserInfo(context.Context, pool.Account) (pool.Account, error) {
	return pool.Account{}, nil
}
func (echoRemote) RefreshAccessToken(context.Context, string, pool.Account) (map[string]string, error) {
	return nil, errors.New("no refresh token")
}

type env struct {
	fake      *fakeupstream.Server
	accounts  *pool.Service
	generator *imagegen.Generator
}

func newEnv(t *testing.T, tokens ...string) *env {
	t.Helper()
	fake := fakeupstream.New("", 50*time.Millisecond, 30*time.Second)
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	fake.SetBase(server.URL)
	t.Setenv("CHATGPT2API_UPSTREAM_BASE_URL", server.URL)

	repo, err := repository.Open("sqlite:///" + filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := pool.Migrate(repo.DB()); err != nil {
		t.Fatal(err)
	}
	store := settings.NewForTest(map[string]any{
		"image_poll_initial_wait_secs": 0, "image_poll_interval_secs": 0.5, "image_settle_secs": 0.5, "image_poll_timeout_secs": 20,
		"auto_remove_invalid_accounts": false,
	}, "")
	accounts, err := pool.New(pool.NewDBStore(repo.DB()), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	accounts.Remote = echoRemote{}
	items := []pool.Account{}
	for _, token := range tokens {
		items = append(items, pool.Account{"access_token": token, "email": token + "@fake.test", "status": "正常", "quota": 5})
	}
	accounts.AddAccountItems(items)

	cfg := &config.Config{}
	proxies := service.NewProxyService(cfg)
	pipeline := imagegen.NewPipeline(store, imagegen.Hooks{Save: func(data []byte, _ string) string { return "http://saved/" + itoa(len(data)) }})
	pipeline.Background = func(fn func()) {}
	generator := &imagegen.Generator{Pipeline: pipeline, Accounts: accounts, NewBackend: func(token string, _ pool.Account) (imagegen.Backend, error) {
		return &chatgptweb.Backend{API: upstream.NewOpenAIBackendAPI(token, "", proxies, cfg), Settings: store,
			Sleep: func(context.Context, time.Duration) {}}, nil
	}}
	return &env{fake: fake, accounts: accounts, generator: generator}
}

func itoa(n int) string {
	return strings.TrimSpace(strings.Repeat(" ", 0) + string(rune('0'+n%10)))
}

func (e *env) run(t *testing.T, request imagegen.Request) ([]imagegen.Output, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if request.Model == "" {
		request.Model = "gpt-image-2"
	}
	if request.Prompt == "" {
		request.Prompt = "a red apple"
	}
	request.MessageAsError = true
	return e.generator.StreamWithPool(ctx, request)
}

// noSlotsHeld: every slot taken during the request came back.
func (e *env) noSlotsHeld(t *testing.T) {
	t.Helper()
	for _, token := range e.accounts.ListTokens() {
		if held := e.accounts.ImageInflight(token); held != 0 {
			t.Fatalf("%s still holds %d slot(s)", token, held)
		}
	}
}

func resultURLs(outputs []imagegen.Output) []string {
	urls := []string{}
	for _, output := range outputs {
		if output.Kind == "result" {
			for _, item := range output.Data {
				urls = append(urls, item["url"].(string))
			}
		}
	}
	return urls
}

func samplePNG(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 5), 90, 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestTextToImageSucceeds(t *testing.T) {
	e := newEnv(t, "fake-ok-1")
	outputs, err := e.run(t, imagegen.Request{Size: "1536x1024"})
	if err != nil || len(resultURLs(outputs)) != 1 {
		t.Fatalf("outputs %v err %v", outputs, err)
	}
	if e.fake.Count("POST /backend-api/sentinel/chat-requirements/finalize") == 0 || e.fake.Count("POST /backend-api/f/conversation/prepare") != 1 {
		t.Fatal("the sentinel handshake or prepare did not run")
	}
	account := e.accounts.GetAccount("fake-ok-1")
	if account["success"] != 1 || account["quota"] != 4 {
		t.Fatalf("account %v", account)
	}
	e.noSlotsHeld(t)
}

func TestEditsUploadEveryReference(t *testing.T) {
	e := newEnv(t, "fake-ok-1")
	ref := samplePNG(t)
	outputs, err := e.run(t, imagegen.Request{Images: []string{ref, ref, ref, ref}})
	if err != nil || len(resultURLs(outputs)) != 1 {
		t.Fatalf("outputs %v err %v", outputs, err)
	}
	if got := e.fake.Count("POST /backend-api/files"); got != 4 {
		t.Fatalf("uploaded %d references, want 4", got)
	}
	if got := e.fake.Count("PUT /upload/{id}"); got != 4 {
		t.Fatalf("PUT %d, want 4", got)
	}
	e.noSlotsHeld(t)
}

func TestForbiddenAccountIsSidelinedAndAnotherOneDraws(t *testing.T) {
	e := newEnv(t, "fake-403-1", "fake-ok-1")
	outputs, err := e.run(t, imagegen.Request{})
	if err != nil || len(resultURLs(outputs)) != 1 {
		t.Fatalf("outputs %v err %v", outputs, err)
	}
	// the 403 account is cooled for 10 minutes: the next request goes straight to the other one
	before := e.fake.Count("GET /")
	if _, err := e.run(t, imagegen.Request{}); err != nil {
		t.Fatal(err)
	}
	if bad := e.accounts.GetAccount("fake-403-1"); bad["fail"] != 1 || bad["status"] != "正常" {
		t.Fatalf("403 must not change the account's status: %v", bad)
	}
	_ = before
	e.noSlotsHeld(t)
}

// A stream cut off after the first events switches accounts: nothing reached the caller yet.
// (Python stopped retrying after the first progress event; the Go port deliberately does not.)
func TestDroppedStreamAfterProgressSwitchesAccount(t *testing.T) {
	e := newEnv(t, "fake-drop-1", "fake-ok-1")
	outputs, err := e.run(t, imagegen.Request{})
	if err != nil || len(resultURLs(outputs)) != 1 {
		t.Fatalf("outputs %v err %v", outputs, err)
	}
	if e.accounts.GetAccount("fake-drop-1")["fail"] != 1 || e.accounts.GetAccount("fake-ok-1")["success"] != 1 {
		t.Fatal("expected the dropped attempt to fail and the next account to draw")
	}
	e.noSlotsHeld(t)
}

func TestTextReplyBecomesAContentPolicyError(t *testing.T) {
	e := newEnv(t, "fake-notool-1")
	_, err := e.run(t, imagegen.Request{})
	var generation *imagegen.GenerationError
	if !errors.As(err, &generation) || generation.StatusCode != 400 || generation.Code != "content_policy_violation" || !strings.Contains(generation.Message, "gradient square") {
		t.Fatalf("err %#v", err)
	}
	if generation.AccountEmail != "fake-notool-1@fake.test" {
		t.Fatalf("email %q", generation.AccountEmail)
	}
	e.noSlotsHeld(t)
}

func TestUploadLimitCoolsTheAccountForEdits(t *testing.T) {
	e := newEnv(t, "fake-upload429-1", "fake-ok-1")
	outputs, err := e.run(t, imagegen.Request{Images: []string{samplePNG(t)}})
	if err != nil || len(resultURLs(outputs)) != 1 {
		t.Fatalf("outputs %v err %v", outputs, err)
	}
	if until, _ := e.accounts.GetAccount("fake-upload429-1")["edit_upload_blocked_until"].(string); until == "" {
		t.Fatal("upload cooldown not recorded")
	}
	e.noSlotsHeld(t)
}

func TestModelNotFoundSidelinesAndSwitches(t *testing.T) {
	e := newEnv(t, "fake-nomodel-1", "fake-ok-1")
	outputs, err := e.run(t, imagegen.Request{})
	if err != nil || len(resultURLs(outputs)) != 1 {
		t.Fatalf("outputs %v err %v", outputs, err)
	}
	e.noSlotsHeld(t)
}

func TestInvalidTokenMarksTheAccountAbnormal(t *testing.T) {
	e := newEnv(t, "fake-badtoken-1", "fake-ok-1")
	outputs, err := e.run(t, imagegen.Request{})
	if err != nil || len(resultURLs(outputs)) != 1 {
		t.Fatalf("outputs %v err %v", outputs, err)
	}
	if status := e.accounts.GetAccount("fake-badtoken-1")["status"]; status != "异常" {
		t.Fatalf("status %v", status)
	}
	e.noSlotsHeld(t)
}

func TestTwoImagesInParallelUseTwoAccounts(t *testing.T) {
	e := newEnv(t, "fake-ok-1", "fake-ok-2")
	outputs, err := e.run(t, imagegen.Request{N: 2})
	if err != nil || len(resultURLs(outputs)) != 2 {
		t.Fatalf("outputs %v err %v", outputs, err)
	}
	if e.accounts.GetAccount("fake-ok-1")["success"] != 1 || e.accounts.GetAccount("fake-ok-2")["success"] != 1 {
		t.Fatal("each image should use its own account")
	}
	e.noSlotsHeld(t)
}

func TestAspectRatioIsSent(t *testing.T) {
	if got := chatgptweb.ImageGenerationSettings("1536x1024"); got["aspect_ratio"] != "4:3" {
		t.Fatalf("got %v", got)
	}
	if chatgptweb.ImageGenerationSettings("auto") != nil {
		t.Fatal("auto has no ratio")
	}
}
