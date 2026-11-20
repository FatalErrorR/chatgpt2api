package compat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/service"
)

type healthLog struct {
	mu      sync.Mutex
	entries []string
}

func (h *healthLog) RecordSuccess(proxy string) {
	h.mu.Lock()
	h.entries = append(h.entries, "ok "+proxy)
	h.mu.Unlock()
}
func (h *healthLog) RecordFailure(proxy, cause string) {
	h.mu.Lock()
	h.entries = append(h.entries, "fail "+proxy)
	h.mu.Unlock()
}

// The account refresh counts towards proxy health and retries Cloudflare challenge pages, like
// every Python call through _retry_cloudflare_challenge.
func TestUserInfoRecordsProxyHealthAndRetriesChallenges(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(403)
			w.Write([]byte("<html>Just a moment...</html>"))
			return
		}
		w.WriteHeader(401)
		w.Write([]byte(`{"detail":"token expired"}`))
	}))
	defer server.Close()
	t.Setenv("CHATGPT2API_UPSTREAM_BASE_URL", server.URL)
	cfg := &config.Config{}
	health := &healthLog{}
	remote := &ChatGPTRemote{Proxy: service.NewProxyService(cfg), Config: cfg, Health: health, Sleep: func(time.Duration) {}}
	account := pool.Account{"access_token": "tok", "proxy": ""}
	// no proxy: nothing to record
	remote.UserInfo(context.Background(), account)
	if len(health.entries) != 0 {
		t.Fatalf("recorded without a proxy: %v", health.entries)
	}
	if hits < 2 {
		t.Fatalf("challenge not retried: %d requests", hits)
	}

	// an API answer (401 JSON) after a challenge: the proxy is fine
	health.entries = nil
	atomic.StoreInt32(&hits, 0)
	remote.Proxy = service.NewProxyService(cfg)
	account["proxy"] = "http://127.0.0.1:1" // unreachable proxy
	if _, err := remote.UserInfo(context.Background(), account); err == nil {
		t.Fatal("expected a connection error")
	}
	// one record per sub-request (/me, conversation init, accounts check), as in Python
	if len(health.entries) != 3 || health.entries[0] != "fail http://127.0.0.1:1" {
		t.Fatalf("connection error: %v", health.entries)
	}
}
