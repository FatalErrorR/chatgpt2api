package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
	t.Helper()
	gin.SetMode(gin.TestMode)
	fake := fakeupstream.New("", imageDelay, 30*time.Second)
	upstreamServer := httptest.NewServer(fake)
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
	return application, store
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
