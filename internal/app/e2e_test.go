package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/settings"
	"github.com/basketikun/chatgpt2api/internal/testsupport/fakeupstream"
)

// e2eApp is the whole application against an in-process fake chatgpt.com.
func e2eApp(t *testing.T, imageDelay time.Duration) (*App, *settings.Store) {
	application, store, _ := e2eAppCounting(t, imageDelay)
	return application, store
}

// e2eAppCounting also counts the requests that reach the fake chatgpt.com.
func e2eAppCounting(t *testing.T, imageDelay time.Duration) (*App, *settings.Store, *atomic.Int64) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	fake := fakeupstream.New("", imageDelay, 30*time.Second)
	requests := &atomic.Int64{}
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(upstreamServer.Close)
	fake.SetBase(upstreamServer.URL)
	t.Setenv("CHATGPT2API_UPSTREAM_BASE_URL", upstreamServer.URL)
	t.Setenv("CHATGPT2API_AUTH_KEY", "test-admin-key")
	dir := t.TempDir()
	t.Setenv("DATABASE_URL", "sqlite:///"+filepath.Join(dir, "e2e.db"))
	cfg, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := repository.Open(cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	store := settings.NewForTest(map[string]any{"auth-key": "test-admin-key", "image_poll_initial_wait_secs": 0, "image_poll_interval_secs": 0.2,
		"image_settle_secs": 0.2, "image_poll_timeout_secs": 30}, "")
	store.DataDir = filepath.Join(dir, "data")
	application, err := New(cfg, repo, store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // finish the asynchronous writes before the database closes
		for _, wait := range application.waiters {
			wait()
		}
	})
	return application, store, requests
}

// CHATGPT2API_BACKGROUND_JOBS=off (the rehearsal instance on a copy of production data) starts no
// account watcher, so nothing refreshes or rotates the accounts' tokens; by default it runs at once.
func TestBackgroundJobsCanBeTurnedOff(t *testing.T) {
	for _, mode := range []string{"off", ""} {
		t.Setenv("CHATGPT2API_BACKGROUND_JOBS", mode)
		application, _, requests := e2eAppCounting(t, 0)
		if response := call(t, application.Engine, "POST", "/api/accounts", map[string]any{"tokens": []string{"fake-ok-1"}}); response.Code != 200 {
			t.Fatalf("add account %d", response.Code)
		}
		before := requests.Load()
		ctx, stop := context.WithCancel(context.Background())
		application.Start(ctx)
		time.Sleep(700 * time.Millisecond)
		stop()
		if called := requests.Load() - before; (mode == "off") != (called == 0) {
			t.Fatalf("mode %q: %d upstream requests after start", mode, called)
		}
	}
}

func call(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	request := httptest.NewRequest(method, path, bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer test-admin-key")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// The image SSE stream ends with "data: [DONE]" like sse_json_stream (OpenAI clients wait for it).
func TestImageStreamEndsWithDone(t *testing.T) {
	application, _ := e2eApp(t, 50*time.Millisecond)
	if response := call(t, application.Engine, "POST", "/api/accounts", map[string]any{"tokens": []string{"fake-ok-1"}}); response.Code != 200 {
		t.Fatalf("add account %d %s", response.Code, response.Body.String())
	}
	response := call(t, application.Engine, "POST", "/v1/images/generations", map[string]any{"prompt": "a red apple", "stream": true, "response_format": "url"})
	body := response.Body.String()
	if response.Code != 200 || !strings.HasPrefix(body, ": stream-open\n\n") || !strings.HasSuffix(body, "data: [DONE]\n\n") || !strings.Contains(body, `"url"`) {
		t.Fatalf("%d %q", response.Code, body[max(0, len(body)-300):])
	}
}

func freeAddr(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

// SIGTERM (ctx ending) lets a generation in flight finish instead of cutting it off.
func TestStopWaitsForAGenerationInFlight(t *testing.T) {
	application, _ := e2eApp(t, 1500*time.Millisecond)
	call(t, application.Engine, "POST", "/api/accounts", map[string]any{"tokens": []string{"fake-ok-1"}})
	addr := freeAddr(t)
	server := &http.Server{Addr: addr, Handler: application.Engine}
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- application.Serve(ctx, server, 30*time.Second) }()
	for i := 0; i < 50; i++ {
		if conn, err := net.Dial("tcp", addr); err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	type result struct {
		status int
		body   string
		err    error
	}
	results := make(chan result, 1)
	go func() {
		request, _ := http.NewRequest("POST", "http://"+addr+"/v1/images/generations", strings.NewReader(`{"prompt":"a red apple","response_format":"url"}`))
		request.Header.Set("Authorization", "Bearer test-admin-key")
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			results <- result{err: err}
			return
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		results <- result{status: response.StatusCode, body: string(raw)}
	}()
	time.Sleep(400 * time.Millisecond) // the image is still being generated
	started := time.Now()
	stop()
	got := <-results
	if got.err != nil || got.status != 200 || !strings.Contains(got.body, `"url"`) {
		t.Fatalf("request cut off: %+v", got)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the request finished")
	}
	if waited := time.Since(started); waited < 500*time.Millisecond {
		t.Fatalf("returned after %s, before the image could be ready", waited)
	}
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Fatal("still accepting connections after the stop")
	}
}
