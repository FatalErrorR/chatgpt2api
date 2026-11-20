package app

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/service"
)

// A proxy that accepts the connection and never answers fails after the test timeout (15 s in
// production, like Python's test_proxy), instead of after the client's 300 s.
func TestProxyTestGivesUpOnAHangingProxy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close() // hold it open, say nothing
		}
	}()
	t.Setenv("CHATGPT2API_AUTH_KEY", "test-admin-key")
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	test := boundedProxyTest(service.NewProxyService(cfg).Test, 500*time.Millisecond)
	started := time.Now()
	result := test(context.Background(), "http://"+listener.Addr().String())
	if elapsed := time.Since(started); elapsed > 5*time.Second || result["ok"] == true {
		t.Fatalf("took %s: %v", elapsed, result)
	}
	if proxyTestTimeout != 15*time.Second {
		t.Fatalf("production timeout %s, Python uses 15 s", proxyTestTimeout)
	}
}
