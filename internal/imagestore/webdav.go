package imagestore

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// WebDAV is WebDAVClient.
type WebDAV struct {
	URL, Username, Password, RootPath string
	client                            *http.Client
}

func (s *Service) webdav() *WebDAV {
	settings := s.settings.ImageStorage()
	text := func(key string) string { v, _ := settings[key].(string); return strings.TrimSpace(v) }
	return &WebDAV{URL: strings.TrimRight(text("webdav_url"), "/"), Username: text("webdav_username"), Password: text("webdav_password"),
		RootPath: strings.Trim(text("webdav_root_path"), "/"), client: &http.Client{Timeout: 30 * time.Second}}
}

func (w *WebDAV) do(method, target string, body []byte, contentType string) (*http.Response, error) {
	request, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		return nil, &StorageError{err.Error()}
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if w.Username != "" || w.Password != "" {
		request.SetBasicAuth(w.Username, w.Password)
	}
	response, err := w.client.Do(request)
	if err != nil {
		return nil, &StorageError{err.Error()}
	}
	return response, nil
}

func (w *WebDAV) request(method, target string, body []byte, contentType string) ([]byte, error) {
	response, err := w.do(method, target, body, contentType)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	if response.StatusCode >= 400 && !(method == "MKCOL" && response.StatusCode == 405) {
		return nil, &StorageError{fmt.Sprintf("WebDAV %s failed: HTTP %d", method, response.StatusCode)}
	}
	return payload, nil
}

func encodeSegments(items ...string) string {
	parts := []string{}
	for _, item := range items {
		for _, part := range strings.Split(item, "/") {
			if part != "" {
				parts = append(parts, url.PathEscape(part))
			}
		}
	}
	return strings.Join(parts, "/")
}

// RemoteURL is remote_url.
func (w *WebDAV) RemoteURL(rel string) string {
	safe := ""
	if rel != "" {
		safe, _ = SafeRel(rel)
	}
	if encoded := encodeSegments(w.RootPath, safe); encoded != "" {
		return w.URL + "/" + encoded
	}
	return w.URL
}

func (w *WebDAV) ensureDirs(rel string) error {
	safe, err := SafeRel(rel)
	if err != nil {
		return err
	}
	current := w.URL
	dir := path.Dir(safe)
	if dir == "." {
		dir = ""
	}
	for _, part := range strings.Split(strings.Trim(w.RootPath+"/"+dir, "/"), "/") {
		if part == "" {
			continue
		}
		current += "/" + url.PathEscape(part)
		response, err := w.do("MKCOL", current, nil, "")
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode == 201 || response.StatusCode == 405 {
			continue
		}
		if response.StatusCode >= 400 {
			return &StorageError{fmt.Sprintf("WebDAV MKCOL failed: HTTP %d", response.StatusCode)}
		}
	}
	return nil
}

// Put uploads and returns the remote URL.
func (w *WebDAV) Put(rel string, payload []byte, contentType string) (string, error) {
	if err := w.ensureDirs(rel); err != nil {
		return "", err
	}
	target := w.RemoteURL(rel)
	if _, err := w.request("PUT", target, payload, contentType); err != nil {
		return "", err
	}
	return target, nil
}

func (w *WebDAV) Get(rel string) ([]byte, error) { return w.request("GET", w.RemoteURL(rel), nil, "") }

// Delete reports whether the file existed.
func (w *WebDAV) Delete(rel string) (bool, error) {
	response, err := w.do("DELETE", w.RemoteURL(rel), nil, "")
	if err != nil {
		return false, err
	}
	response.Body.Close()
	switch response.StatusCode {
	case 200, 202, 204:
		return true, nil
	case 404:
		return false, nil
	}
	return false, &StorageError{fmt.Sprintf("WebDAV DELETE failed: HTTP %d", response.StatusCode)}
}

// Test is test(): put and delete a small file.
func (w *WebDAV) Test() map[string]any {
	if w.URL == "" {
		return map[string]any{"ok": false, "status": 0, "error": "WebDAV URL is required"}
	}
	if parsed, err := url.Parse(w.URL); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return map[string]any{"ok": false, "status": 0, "error": "invalid WebDAV URL"}
	}
	const rel = ".chatgpt2api_webdav_test.txt"
	if _, err := w.Put(rel, []byte("chatgpt2api webdav test\n"), "text/plain"); err != nil {
		return map[string]any{"ok": false, "status": 0, "error": err.Error()}
	}
	if _, err := w.Delete(rel); err != nil {
		return map[string]any{"ok": false, "status": 0, "error": err.Error()}
	}
	return map[string]any{"ok": true, "status": 200, "error": nil}
}
