package compat

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// The frontend: api/app.py serve_web and resolve_web_asset, plus FastAPI's GZipMiddleware.

// resolveWebAsset is resolve_web_asset: the file itself, its index.html, or <path>.html.
func resolveWebAsset(root, requested string) string {
	base, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	if info, err := os.Stat(base); err != nil || !info.IsDir() {
		return ""
	}
	clean := strings.Trim(requested, "/")
	candidates := []string{filepath.Join(base, "index.html")}
	if clean != "" {
		path := filepath.Join(base, filepath.FromSlash(clean))
		candidates = []string{path, filepath.Join(path, "index.html"), path + ".html"}
	}
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if candidate != base && !strings.HasPrefix(candidate, base+string(filepath.Separator)) {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate
		}
	}
	return ""
}

var (
	gzipCacheMu sync.Mutex
	gzipCache   = map[string][]byte{}
)

// cachedGzip is _cached_gzip: compressed js/css kept per file and modification time.
func cachedGzip(path string, info os.FileInfo) ([]byte, error) {
	key := path + "|" + info.ModTime().String()
	gzipCacheMu.Lock()
	cached, ok := gzipCache[key]
	gzipCacheMu.Unlock()
	if ok {
		return cached, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&buf, 6)
	writer.Write(raw)
	writer.Close()
	gzipCacheMu.Lock()
	if len(gzipCache) > 80 {
		gzipCache = map[string][]byte{}
	}
	gzipCache[key] = buf.Bytes()
	gzipCacheMu.Unlock()
	return buf.Bytes(), nil
}

// serveAsset is _asset_response.
func serveAsset(c *gin.Context, path string) {
	if strings.Contains(filepath.ToSlash(path), "_next/static/") {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
	}
	ext := filepath.Ext(path)
	if c.Request.Method == http.MethodGet && strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") && (ext == ".js" || ext == ".css") {
		if info, err := os.Stat(path); err == nil {
			if body, err := cachedGzip(path, info); err == nil {
				c.Header("Content-Encoding", "gzip")
				c.Header("Vary", "Accept-Encoding")
				contentType := mime.TypeByExtension(ext)
				if contentType == "" {
					contentType = "application/octet-stream"
				}
				c.Set(skipGzipKey, true)
				c.Data(http.StatusOK, contentType, body)
				return
			}
		}
	}
	c.Set(skipGzipKey, true)
	c.File(path)
}

// ServeWeb handles every request no route matched. GET and HEAD get the frontend (a missing
// _next/ asset is a 404); other methods get 405, as FastAPI answers when only the GET catch-all
// route matches.
func ServeWeb(root string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			c.AbortWithStatusJSON(http.StatusMethodNotAllowed, gin.H{"detail": "Method Not Allowed"})
			return
		}
		path := c.Request.URL.Path
		if asset := resolveWebAsset(root, path); asset != "" {
			serveAsset(c, asset)
			return
		}
		if strings.HasPrefix(strings.Trim(path, "/"), "_next/") {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"detail": "Not Found"})
			return
		}
		if index := resolveWebAsset(root, ""); index != "" {
			serveAsset(c, index)
			return
		}
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"detail": "Not Found"})
	}
}

// ---- GZipMiddleware(minimum_size=800, compresslevel=6) ----

const (
	gzipMinimumSize = 800
	skipGzipKey     = "compat.skip_gzip"
)

type gzipWriter struct {
	gin.ResponseWriter
	c        *gin.Context
	buf      bytes.Buffer
	gz       *gzip.Writer
	decided  bool
	compress bool
}

func (w *gzipWriter) decide(final bool) {
	if w.decided {
		return
	}
	header := w.ResponseWriter.Header()
	contentType := header.Get("Content-Type")
	skip, _ := w.c.Get(skipGzipKey)
	if skip == true || header.Get("Content-Encoding") != "" || strings.HasPrefix(contentType, "text/event-stream") {
		w.decided = true
		return
	}
	if final && w.buf.Len() < gzipMinimumSize {
		w.decided = true
		return
	}
	if !final && w.buf.Len() < gzipMinimumSize {
		return
	}
	w.decided, w.compress = true, true
	header.Set("Content-Encoding", "gzip")
	header.Add("Vary", "Accept-Encoding")
	header.Del("Content-Length")
	w.gz, _ = gzip.NewWriterLevel(w.ResponseWriter, 6)
}

func (w *gzipWriter) flushBuffer() {
	if w.buf.Len() == 0 {
		return
	}
	if w.compress {
		w.gz.Write(w.buf.Bytes())
	} else {
		w.ResponseWriter.Write(w.buf.Bytes())
	}
	w.buf.Reset()
}

func (w *gzipWriter) Write(data []byte) (int, error) {
	if w.decided {
		if w.compress {
			return w.gz.Write(data)
		}
		return w.ResponseWriter.Write(data)
	}
	w.buf.Write(data)
	w.decide(false)
	if w.decided {
		w.flushBuffer()
	}
	return len(data), nil
}

func (w *gzipWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

// Flush sends what is buffered: a stream decides now (an SSE stream is never compressed).
func (w *gzipWriter) Flush() {
	w.decide(true)
	w.flushBuffer()
	if w.compress {
		w.gz.Flush()
	}
	w.ResponseWriter.Flush()
}

func (w *gzipWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.ResponseWriter.Hijack()
}

func (w *gzipWriter) finish() {
	w.decide(true)
	w.flushBuffer()
	if w.compress {
		w.gz.Close()
	}
}

// Gzip compresses responses of 800 bytes or more for clients that accept gzip.
func Gzip(c *gin.Context) {
	if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") || c.Request.Method == http.MethodHead {
		c.Next()
		return
	}
	writer := &gzipWriter{ResponseWriter: c.Writer, c: c}
	c.Writer = writer
	defer writer.finish()
	c.Next()
}
