package compat

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func webRoot(t *testing.T) string {
	root := t.TempDir()
	files := map[string]string{
		"index.html":             "<html>home</html>",
		"settings/index.html":    "<html>settings</html>",
		"login.html":             "<html>login</html>",
		"_next/static/app.js":    strings.Repeat("console.log(1);", 200),
		"_next/static/small.css": "a{}",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(content), 0o644)
	}
	return root
}

func webEngine(root string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(Gzip)
	engine.GET("/api/big", func(c *gin.Context) { c.JSON(200, gin.H{"items": strings.Repeat("x", 2000)}) })
	engine.GET("/api/small", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	engine.GET("/api/stream", func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.String(200, strings.Repeat("data: x\n\n", 200))
	})
	engine.NoRoute(ServeWeb(root))
	return engine
}

func do(engine *gin.Engine, method, path string, gzipOK bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	if gzipOK {
		request.Header.Set("Accept-Encoding", "gzip, br")
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)
	return recorder
}

func TestServeWebFollowsPythonResolution(t *testing.T) {
	engine := webEngine(webRoot(t))
	cases := map[string]string{
		"/":                   "<html>home</html>",
		"/settings/":          "<html>settings</html>",
		"/settings":           "<html>settings</html>",
		"/login":              "<html>login</html>",
		"/accounts/anything/": "<html>home</html>", // unknown pages fall back to the app
		"/api/missing":        "<html>home</html>", // like Python: any unmatched GET is the frontend
	}
	for path, want := range cases {
		if response := do(engine, "GET", path, false); response.Code != 200 || response.Body.String() != want {
			t.Errorf("%s: %d %q", path, response.Code, response.Body.String())
		}
	}
	if response := do(engine, "GET", "/_next/static/missing.js", false); response.Code != 404 || response.Body.String() != `{"detail":"Not Found"}` {
		t.Fatalf("missing asset: %d %s", response.Code, response.Body.String())
	}
	if response := do(engine, "POST", "/whatever", false); response.Code != 405 || response.Body.String() != `{"detail":"Method Not Allowed"}` {
		t.Fatalf("post: %d %s", response.Code, response.Body.String())
	}
	if response := do(engine, "GET", "/../../etc/passwd", false); strings.Contains(response.Body.String(), "root:") {
		t.Fatal("path traversal")
	}
	if response := do(webEngine(t.TempDir()+"/none"), "GET", "/", false); response.Code != 404 {
		t.Fatalf("no build: %d", response.Code)
	}
}

func gunzip(t *testing.T, body []byte) string {
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(reader)
	return string(raw)
}

func TestGzipLikeStarlette(t *testing.T) {
	engine := webEngine(webRoot(t))
	big := do(engine, "GET", "/api/big", true)
	if big.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(gunzip(t, big.Body.Bytes()), `"items"`) {
		t.Fatalf("big: %v", big.Header())
	}
	if small := do(engine, "GET", "/api/small", true); small.Header().Get("Content-Encoding") != "" || small.Body.String() != `{"ok":true}` {
		t.Fatalf("small: %v %s", small.Header(), small.Body.String())
	}
	if stream := do(engine, "GET", "/api/stream", true); stream.Header().Get("Content-Encoding") != "" {
		t.Fatal("event streams are not compressed")
	}
	if plain := do(engine, "GET", "/api/big", false); plain.Header().Get("Content-Encoding") != "" {
		t.Fatal("compressed without Accept-Encoding")
	}
	js := do(engine, "GET", "/_next/static/app.js", true)
	if js.Header().Get("Content-Encoding") != "gzip" || js.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" ||
		!strings.HasPrefix(gunzip(t, js.Body.Bytes()), "console.log") {
		t.Fatalf("js: %v", js.Header())
	}
	if html := do(engine, "GET", "/", true); html.Header().Get("Content-Encoding") != "" || html.Body.String() != "<html>home</html>" {
		t.Fatalf("html: %v", html.Header())
	}
}

var _ = http.MethodGet
