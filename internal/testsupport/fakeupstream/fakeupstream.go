// Package fakeupstream imitates the chatgpt.com endpoints that chatgpt2api calls, so the Python
// and Go versions can be compared, end-to-end tested and load tested without real accounts.
//
// The scenario comes from the access token: "fake-<scenario>-<anything>".
//
//	ok        image (or text) after -delay
//	slow      like ok, but waits -slow-delay before the image appears
//	notool    replies with text and never calls the image tool
//	403       every call returns the Cloudflare challenge page
//	upload429 uploads fail with upload_protection_limit_exceeded
//	badtoken  every call returns 401 token_invalidated
//	nomodel   the conversation call returns 404 {"detail":"Model not found"}
//	drop      the conversation stream is cut off mid-way
package fakeupstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Server is the fake chatgpt.com.
type Server struct {
	delay     time.Duration
	slowDelay time.Duration
	base      string
	seq       atomic.Int64
	mu        sync.Mutex
	convs     map[string]*conversation
	pngBytes  []byte
	stats     sync.Map // path -> *atomic.Int64
}

type conversation struct {
	id       string
	scenario string
	readyAt  time.Time
	fileID   string
	text     bool
}

const challengePage = `<!DOCTYPE html><html><head><title>Just a moment...</title></head><body>Enable JavaScript and cookies to continue<script>window._cf_chl_opt={}</script></body></html>`

// New builds the fake upstream. base is its own address (used in upload and download links);
// call SetBase when the address is only known after listening (httptest).
func New(base string, delay, slowDelay time.Duration) *Server {
	return &Server{delay: delay, slowDelay: slowDelay, base: base, convs: map[string]*conversation{}, pngBytes: samplePNG()}
}

// SetBase sets the address used in upload and download links.
func (s *Server) SetBase(base string) { s.base = base }

// Count is how many times a route was called ("POST /backend-api/files").
func (s *Server) Count(route string) int64 {
	if counter, ok := s.stats.Load(route); ok {
		return counter.(*atomic.Int64).Load()
	}
	return 0
}

func samplePNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for y := 0; y < 256; y++ {
		for x := 0; x < 256; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 160, 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func scenarioOf(r *http.Request) string {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.SplitN(token, "-", 3)
	if len(parts) >= 2 && parts[0] == "fake" {
		return parts[1]
	}
	return "ok"
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func (s *Server) count(path string) {
	counter, _ := s.stats.LoadOrStore(path, new(atomic.Int64))
	counter.(*atomic.Int64).Add(1)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	s.count(r.Method + " " + routeKey(path))
	scenario := scenarioOf(r)
	switch {
	case path == "/__stats":
		out := map[string]int64{}
		s.stats.Range(func(key, value any) bool { out[key.(string)] = value.(*atomic.Int64).Load(); return true })
		writeJSON(w, 200, out)
		return
	case strings.HasPrefix(path, "/blob/"):
		w.Header().Set("Content-Type", "image/png")
		w.Write(s.pngBytes)
		return
	case strings.HasPrefix(path, "/upload/"):
		w.WriteHeader(http.StatusCreated)
		return
	}
	if scenario == "403" {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(challengePage))
		return
	}
	if scenario == "badtoken" && strings.HasPrefix(path, "/backend-api/") && !strings.HasPrefix(path, "/backend-api/sentinel/sdk") {
		writeJSON(w, 401, map[string]any{"detail": map[string]any{"code": "token_invalidated", "message": "Your authentication token has been invalidated. Please try signing in again."}})
		return
	}
	switch {
	case path == "/":
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<html><head><script src="%s/backend-api/sentinel/sdk.js?build=fake"></script></head><body data-build="fake"></body></html>`, s.base)
	case strings.HasPrefix(path, "/backend-api/sentinel/sdk"):
		w.Header().Set("Content-Type", "application/javascript")
		w.Write([]byte("// fake sentinel sdk\n"))
	case strings.HasSuffix(path, "/sentinel/chat-requirements/prepare"):
		writeJSON(w, 200, map[string]any{"prepare_token": "fake-prepare", "persona": "chatgpt-paid",
			"proofofwork": map[string]any{"required": false}, "turnstile": map[string]any{"required": false}, "arkose": map[string]any{"required": false}})
	case strings.HasSuffix(path, "/sentinel/chat-requirements/finalize"), strings.HasSuffix(path, "/sentinel/chat-requirements"):
		writeJSON(w, 200, map[string]any{"token": "fake-requirements", "so_token": "", "persona": "chatgpt-paid",
			"proofofwork": map[string]any{"required": false}, "turnstile": map[string]any{"required": false}})
	case path == "/backend-api/me":
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		writeJSON(w, 200, map[string]any{"id": "user-" + token, "email": token + "@fake.test", "name": token,
			"orgs": map[string]any{"data": []any{}}})
	case strings.HasPrefix(path, "/backend-api/accounts/check"):
		writeJSON(w, 200, map[string]any{"accounts": map[string]any{"default": map[string]any{
			"account":     map[string]any{"account_id": "acct-fake", "plan_type": "free", "account_user_role": "account-owner", "is_deactivated": false},
			"entitlement": map[string]any{"has_active_subscription": false, "subscription_plan": "chatgptfreeplan"}}}})
	case path == "/backend-api/conversation/init":
		writeJSON(w, 200, map[string]any{"default_model_slug": "auto", "limits_progress": []any{
			map[string]any{"feature_name": "image_gen", "remaining": 25, "reset_after": time.Now().Add(20 * time.Hour).UTC().Format(time.RFC3339)}}})
	case strings.HasPrefix(path, "/backend-api/models"), strings.HasPrefix(path, "/backend-api/codex/models"):
		writeJSON(w, 200, map[string]any{"models": []any{
			map[string]any{"slug": "auto", "title": "Auto"}, map[string]any{"slug": "gpt-5-6", "title": "GPT-5.6"}}})
	case path == "/backend-api/f/conversation/prepare":
		writeJSON(w, 200, map[string]any{"status": "ok", "conduit_token": "fake-conduit"})
	case path == "/backend-api/files" && r.Method == http.MethodPost:
		if scenario == "upload429" {
			// the text ChatGPT returns (the pipeline matches "文件上传上限" and the retry hint)
			writeJSON(w, 429, map[string]any{"detail": "已达到文件上传上限，请 3 小时内重试"})
			return
		}
		id := fmt.Sprintf("file-up%06d", s.seq.Add(1))
		writeJSON(w, 200, map[string]any{"status": "success", "file_id": id, "upload_url": s.base + "/upload/" + id})
	case strings.HasPrefix(path, "/backend-api/files/") && strings.HasSuffix(path, "/uploaded"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/backend-api/files/"), "/uploaded")
		writeJSON(w, 200, map[string]any{"status": "success", "download_url": s.base + "/blob/" + id + ".png"})
	case strings.HasPrefix(path, "/backend-api/files/download/"), strings.HasPrefix(path, "/backend-api/files/") && strings.HasSuffix(path, "/download"):
		id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(path, "/backend-api/files/download/"), "/backend-api/files/"), "/download")
		writeJSON(w, 200, map[string]any{"status": "success", "download_url": s.base + "/blob/" + id + ".png", "file_name": id + ".png"})
	case strings.Contains(path, "/attachment/") && strings.HasSuffix(path, "/download"):
		writeJSON(w, 200, map[string]any{"status": "success", "download_url": s.base + "/blob/attachment.png"})
	case path == "/backend-api/f/conversation" || path == "/backend-api/conversation":
		s.conversationStream(w, r, scenario)
	case strings.HasPrefix(path, "/backend-api/conversation/") && r.Method == http.MethodGet:
		s.conversationDocument(w, strings.TrimPrefix(path, "/backend-api/conversation/"))
	case strings.HasPrefix(path, "/backend-api/conversation/"):
		writeJSON(w, 200, map[string]any{"success": true})
	case strings.HasPrefix(path, "/backend-api/conversations"):
		writeJSON(w, 200, map[string]any{"items": []any{}, "total": 0})
	case strings.HasPrefix(path, "/backend-api/tasks"):
		writeJSON(w, 200, map[string]any{"tasks": []any{}})
	default:
		writeJSON(w, 404, map[string]any{"detail": "fake upstream: no handler for " + r.Method + " " + path})
	}
}

// routeKey collapses ids so /__stats stays readable.
func routeKey(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, "file-") || strings.HasPrefix(part, "conv-") || len(part) > 24 {
			parts[i] = "{id}"
		}
	}
	return strings.Join(parts, "/")
}

func (s *Server) conversationStream(w http.ResponseWriter, r *http.Request, scenario string) {
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	if scenario == "nomodel" {
		writeJSON(w, 404, map[string]any{"detail": "Model not found"})
		return
	}
	hints, _ := json.Marshal(body["system_hints"])
	isImage := strings.Contains(string(hints), "picture_v2")
	id := fmt.Sprintf("conv-%08d", s.seq.Add(1))
	delay := s.delay
	if scenario == "slow" {
		delay = s.slowDelay
	}
	conv := &conversation{id: id, scenario: scenario, readyAt: time.Now().Add(delay), fileID: fmt.Sprintf("file-gen%06d", s.seq.Add(1)), text: !isImage || scenario == "notool"}
	s.mu.Lock()
	s.convs[id] = conv
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	send := func(value any) {
		data, _ := json.Marshal(value)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send(map[string]any{"type": "resume_conversation_token", "conversation_id": id, "token": "fake-resume"})
	send(map[string]any{"type": "message_marker", "conversation_id": id, "message_id": "user-msg", "marker": "user_visible_token", "event": "first"})
	if scenario == "drop" {
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				conn.Close()
			}
		}
		return
	}
	if conv.text {
		text := "Hello from the fake upstream."
		if isImage {
			text = "I can describe the picture instead: a gradient square."
		}
		send(map[string]any{"conversation_id": id, "message": assistantText(text), "error": nil})
		send(map[string]any{"type": "message_stream_complete", "conversation_id": id})
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	send(map[string]any{"conversation_id": id, "message": toolStarted(), "error": nil})
	select {
	case <-time.After(time.Until(conv.readyAt)):
	case <-r.Context().Done():
		return
	}
	send(map[string]any{"conversation_id": id, "message": toolResult(conv.fileID), "error": nil})
	send(map[string]any{"type": "message_stream_complete", "conversation_id": id})
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (s *Server) conversationDocument(w http.ResponseWriter, id string) {
	id = strings.SplitN(id, "/", 2)[0]
	s.mu.Lock()
	conv := s.convs[id]
	s.mu.Unlock()
	if conv == nil {
		writeJSON(w, 404, map[string]any{"detail": "conversation not found"})
		return
	}
	mapping := map[string]any{"user": map[string]any{"message": map[string]any{"author": map[string]any{"role": "user"}, "content": map[string]any{"content_type": "text", "parts": []any{"prompt"}}}}}
	switch {
	case conv.text:
		mapping["assistant"] = map[string]any{"message": assistantText("I can describe the picture instead: a gradient square.")}
	case time.Now().After(conv.readyAt):
		mapping["tool"] = map[string]any{"message": toolResult(conv.fileID)}
	default:
		mapping["tool"] = map[string]any{"message": toolStarted()}
	}
	writeJSON(w, 200, map[string]any{"conversation_id": id, "title": "fake", "mapping": mapping})
}

func assistantText(text string) map[string]any {
	return map[string]any{"id": "assistant-msg", "author": map[string]any{"role": "assistant"}, "create_time": float64(time.Now().Unix()),
		"content": map[string]any{"content_type": "text", "parts": []any{text}}, "status": "finished_successfully", "end_turn": true,
		"metadata": map[string]any{}, "recipient": "all"}
}

func toolStarted() map[string]any {
	return map[string]any{"id": "tool-msg", "author": map[string]any{"role": "tool", "name": "t2uay3k.sj1i4kz"}, "create_time": float64(time.Now().Unix()),
		"content": map[string]any{"content_type": "multimodal_text", "parts": []any{}}, "status": "in_progress",
		"metadata": map[string]any{"async_task_type": "image_gen", "image_gen_async": true, "tool_invoked": true, "turn_use_case": "image gen"}, "recipient": "all"}
}

func toolResult(fileID string) map[string]any {
	return map[string]any{"id": "tool-msg", "author": map[string]any{"role": "tool", "name": "t2uay3k.sj1i4kz"}, "create_time": float64(time.Now().Unix()),
		"content": map[string]any{"content_type": "multimodal_text", "parts": []any{map[string]any{
			"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + fileID, "width": 256, "height": 256, "size_bytes": 1024,
			"metadata": map[string]any{"dalle": map[string]any{"prompt": "fake"}}}}},
		"status": "finished_successfully", "end_turn": true,
		"metadata": map[string]any{"async_task_type": "image_gen", "image_gen_async": true, "tool_invoked": true, "turn_use_case": "image gen"}, "recipient": "all"}
}
