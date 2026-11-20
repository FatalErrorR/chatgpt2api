package app

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/settings"
	"github.com/gin-gonic/gin"
)

var fastAPIParam = regexp.MustCompile(`\{([a-zA-Z_]+)(:path)?\}`)

// normalizeRoute turns "GET /api/x/{id}" and "GET /api/x/:id" into the same key, "GET /api/x/:".
func normalizeRoute(method, path string) string {
	path = fastAPIParam.ReplaceAllStringFunc(path, func(m string) string {
		if strings.HasSuffix(m, ":path}") {
			return "*"
		}
		return ":"
	})
	parts := strings.Split(path, "/")
	for i, part := range parts {
		switch {
		case strings.HasPrefix(part, ":"):
			parts[i] = ":"
		case strings.HasPrefix(part, "*"):
			parts[i] = "*"
		}
	}
	return method + " " + strings.Join(parts, "/")
}

func readRouteFile(t *testing.T, name string) []string {
	t.Helper()
	file, err := os.Open(filepath.Join("..", "..", "docs", "contract", name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	defer file.Close()
	var routes []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		methods, path, _ := strings.Cut(line, " ")
		if path == "/{full_path:path}" { // SPA fallback: engine.NoRoute serves the web build
			continue
		}
		for _, method := range strings.Split(methods, ",") {
			if method != "HEAD" {
				routes = append(routes, normalizeRoute(method, path))
			}
		}
	}
	return routes
}

func testApp(t *testing.T) *App {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("CHATGPT2API_AUTH_KEY", "test-admin-key")
	t.Setenv("DATABASE_URL", "sqlite:///"+filepath.Join(t.TempDir(), "test.db"))
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := repository.Open(cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	application, err := New(cfg, repo, settings.NewForTest(map[string]any{"auth-key": "test-admin-key"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	return application
}

// TestPythonRoutesAreServed is acceptance criterion A1 as a ratchet: every route of the Python
// version must be registered, except those still listed in docs/contract/pending-routes.txt.
// Implementing a route without removing it from the pending list also fails, so the list only shrinks.
func TestPythonRoutesAreServed(t *testing.T) {
	// Only the compat layer counts: upstream's handlers of the same path use upstream's tables
	// (another account store, another API key store), so delegating to them is not a port.
	registered := map[string]bool{}
	for _, route := range testApp(t).Engine.Routes() {
		registered[normalizeRoute(route.Method, route.Path)] = true
	}
	pending := map[string]bool{}
	for _, route := range readRouteFile(t, "pending-routes.txt") {
		pending[route] = true
	}
	var missing, done []string
	expected := readRouteFile(t, "python-routes.txt")
	for _, route := range expected {
		switch {
		case !registered[route] && !pending[route]:
			missing = append(missing, route)
		case registered[route] && pending[route]:
			done = append(done, route)
		}
	}
	sort.Strings(missing)
	sort.Strings(done)
	if len(missing) > 0 {
		t.Errorf("routes of the Python version not served and not pending (%d):\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
	if len(done) > 0 {
		t.Errorf("served now, remove from docs/contract/pending-routes.txt (%d):\n  %s", len(done), strings.Join(done, "\n  "))
	}
	t.Logf("python routes: %d, pending: %d", len(expected), len(pending))
}
