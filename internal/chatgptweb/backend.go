// Package chatgptweb implements imagegen.Backend: the ChatGPT web image flow of the Python
// OpenAIBackendAPI (services/openai_backend_api.py), with the Python request bodies sent through the
// upstream Go client's transport.
package chatgptweb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/basketikun/chatgpt2api/internal/settings"
	"github.com/basketikun/chatgpt2api/internal/upstream"
	http "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
	_ "golang.org/x/image/webp"
)

const (
	imageCreationMention = "@创建图像"
	clientCreatedRoot    = "client-created-root"
	downloadAttempts     = 4
)

var downloadRetryStatuses = map[int]bool{429: true, 500: true, 502: true, 503: true, 504: true}

// Health records how a proxy did (proxy_list.record_success / record_failure).
type Health interface {
	RecordSuccess(proxy string)
	RecordFailure(proxy, cause string)
}

// Backend is one account's ChatGPT client for the image pipeline.
type Backend struct {
	API      *upstream.OpenAIBackendAPI
	Settings *settings.Store
	Health   Health
	// Fit shrinks reference images to the upload budget (image_ref_fit); nil keeps them as they are.
	Fit   func(images [][]byte, prompt string) [][]byte
	Sleep func(ctx context.Context, d time.Duration)
}

var _ imagegen.Backend = (*Backend)(nil)

func (b *Backend) sleep(ctx context.Context, d time.Duration) {
	if b.Sleep != nil {
		b.Sleep(ctx, d)
		return
	}
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// pythonContext names the failing call like the Python ensure_ok contexts.
func pythonContext(path string) string {
	switch {
	case path == "/":
		return "bootstrap"
	case strings.HasSuffix(path, "/sentinel/chat-requirements/prepare"):
		return "chat_requirements_prepare"
	case strings.HasSuffix(path, "/sentinel/chat-requirements/finalize"):
		return "chat_requirements_finalize"
	}
	return strings.SplitN(path, "?", 2)[0]
}

// convertErr turns upstream client errors into the Python error types the pipeline classifies.
func convertErr(err error) error {
	if err == nil {
		return nil
	}
	var httpErr *upstream.UpstreamHTTPError
	if errors.As(err, &httpErr) {
		var body any = httpErr.Body
		var decoded any
		if json.Unmarshal([]byte(httpErr.Body), &decoded) == nil {
			if _, isMap := decoded.(map[string]any); isMap {
				body = decoded
			}
			if _, isList := decoded.([]any); isList {
				body = decoded
			}
		}
		return &imagegen.UpstreamHTTPError{Context: pythonContext(httpErr.Context), StatusCode: httpErr.StatusCode, Body: body}
	}
	return err
}

func isChallenge(err error) bool {
	var httpErr *imagegen.UpstreamHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != 403 {
		return false
	}
	_, isJSON := httpErr.Body.(map[string]any)
	return !isJSON
}

// withChallengeRetry is _retry_cloudflare_challenge: up to 3 tries on a Cloudflare challenge page,
// and the result goes into the proxy's health.
func (b *Backend) withChallengeRetry(ctx context.Context, call func() (*http.Response, error)) (*http.Response, error) {
	proxy := b.API.AccountProxy()
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		response, err := call()
		err = convertErr(err)
		if err == nil {
			b.recordSuccess(proxy)
			return response, nil
		}
		if !isChallenge(err) {
			var httpErr *imagegen.UpstreamHTTPError
			if errors.As(err, &httpErr) || errors.Is(err, upstream.ErrTokenInvalidated) {
				b.recordSuccess(proxy) // a JSON answer means the proxy got through
			} else {
				b.recordFailure(proxy, err.Error())
			}
			return nil, err
		}
		lastErr = err
		if attempt < 3 {
			b.sleep(ctx, time.Duration(1.5*float64(attempt)*float64(time.Second)))
		}
	}
	b.recordFailure(proxy, "Cloudflare challenge (HTTP 403)")
	return nil, lastErr
}

func (b *Backend) recordSuccess(proxy string) {
	if b.Health != nil && proxy != "" {
		b.Health.RecordSuccess(proxy)
	}
}

func (b *Backend) recordFailure(proxy, cause string) {
	if b.Health != nil && proxy != "" {
		b.Health.RecordFailure(proxy, cause)
	}
}

func (b *Backend) getJSON(ctx context.Context, method, path string, payload any, extra map[string]string, retry bool) (map[string]any, error) {
	call := func() (*http.Response, error) { return b.API.Request(ctx, method, path, payload, extra) }
	var response *http.Response
	var err error
	if retry {
		response, err = b.withChallengeRetry(ctx, call)
	} else {
		response, err = call()
		err = convertErr(err)
	}
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var data map[string]any
	if err := decoder.Decode(&data); err != nil {
		return nil, err
	}
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

// decodeImage is _decode_image_base64 (data URLs or bare base64).
func decodeImage(value string) ([]byte, error) {
	payload := value
	if strings.HasPrefix(value, "data:") {
		if _, after, ok := strings.Cut(value, ","); ok {
			payload = after
		}
	}
	return base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
}

var mimeByFormat = map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}

type uploaded struct {
	FileID   string
	FileName string
	FileSize int
	MimeType string
	Width    int
	Height   int
}

// upload is _upload_image: register the file, PUT it to the signed URL, mark it uploaded.
func (b *Backend) upload(ctx context.Context, data []byte, fileName string) (uploaded, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return uploaded{}, fmt.Errorf("cannot identify image file: %w", err)
	}
	mime := mimeByFormat[format]
	if mime == "" {
		mime = "image/png"
	}
	meta, err := b.getJSON(ctx, http.MethodPost, "/backend-api/files", map[string]any{
		"file_name": fileName, "file_size": len(data), "use_case": "multimodal", "width": config.Width, "height": config.Height,
	}, map[string]string{"Accept": "application/json"}, true)
	if err != nil {
		return uploaded{}, err
	}
	uploadURL, _ := meta["upload_url"].(string)
	fileID, _ := meta["file_id"].(string)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return uploaded{}, err
	}
	for key, value := range map[string]string{
		"Content-Type": mime, "x-ms-blob-type": "BlockBlob", "x-ms-version": "2020-04-08", "Origin": b.API.BaseURL(),
		"Referer": b.API.BaseURL() + "/", "User-Agent": b.API.UserAgent(), "Accept": "application/json, text/plain, */*", "Accept-Language": "en-US,en;q=0.8",
	} {
		req.Header.Set(key, value)
	}
	response, err := b.API.Do(req)
	if err != nil {
		return uploaded{}, err
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return uploaded{}, &imagegen.UpstreamHTTPError{Context: "image_upload", StatusCode: response.StatusCode, Body: string(body)}
	}
	path := "/backend-api/files/" + fileID + "/uploaded"
	if _, err := b.withChallengeRetry(ctx, func() (*http.Response, error) {
		return b.API.RequestBody(ctx, http.MethodPost, path, strings.NewReader("{}"), "application/json", map[string]string{"Accept": "application/json"})
	}); err != nil {
		return uploaded{}, err
	}
	return uploaded{FileID: fileID, FileName: fileName, FileSize: len(data), MimeType: mime, Width: config.Width, Height: config.Height}, nil
}

// upstreamModel is _image_model_settings' model part.
func (b *Backend) upstreamModel(model string) string {
	_, base := imagegen.SplitImageModel(model)
	switch {
	case base == "":
		return "auto"
	case base == "gpt-image-2":
		if value := strings.TrimSpace(b.Settings.DefaultUpstreamModelName()); value != "" {
			return value
		}
		return "gpt-5-5"
	}
	if name, suffix, ok := cutLast(base, "-"); ok && (suffix == "standard" || suffix == "extended" || suffix == "max") {
		return name
	}
	return base
}

func cutLast(text, sep string) (string, string, bool) {
	i := strings.LastIndex(text, sep)
	if i < 0 {
		return "", "", false
	}
	return text[:i], strings.ToLower(text[i+len(sep):]), true
}

func clientContext() map[string]any {
	return map[string]any{"app_name": "chatgpt.com", "app_surface": "codex_browser", "has_web_push_capabilities": true, "web_push_notification_permission": "default"}
}

// StreamPicture is _stream_picture_conversation: upload, sentinel, prepare, start, then the SSE
// data payloads with a hard cap of image_poll_timeout_secs on the whole stream.
func (b *Backend) StreamPicture(ctx context.Context, prompt, model string, images []string, size string) (<-chan string, func() error, error) {
	decoded := make([][]byte, 0, len(images))
	for _, value := range images {
		data, err := decodeImage(value)
		if err != nil {
			return nil, nil, err
		}
		decoded = append(decoded, data)
	}
	if len(decoded) > 0 && b.Fit != nil && (b.Settings.ImageRefFitEnabled() || b.Settings.ImageReqFitEnabled()) {
		decoded = b.Fit(decoded, prompt)
	}
	refs := make([]uploaded, 0, len(decoded))
	for i, data := range decoded {
		ref, err := b.upload(ctx, data, fmt.Sprintf("image_%d.png", i+1))
		if err != nil {
			return nil, nil, err
		}
		refs = append(refs, ref)
	}
	if err := convertErr(b.API.Bootstrap(ctx)); err != nil {
		return nil, nil, err
	}
	if err := convertErr(b.API.ChatRequirements(ctx)); err != nil {
		return nil, nil, err
	}
	upstreamModel := b.upstreamModel(model)
	traceID := uuid.NewString()
	prepared, err := b.getJSON(ctx, http.MethodPost, "/backend-api/f/conversation/prepare", map[string]any{
		"action": "next", "is_do_not_remember": false, "model": upstreamModel, "parent_message_id": clientCreatedRoot,
		"system_hints": []string{"picture_v2"}, "timezone": "Asia/Shanghai", "timezone_offset_min": -480,
		"client_contextual_info": clientContext(), "local_function_names": []string{"local.continue_in_work"},
		"supported_encodings": []string{"v1"}, "client_prepare_state": "sent",
		"partial_query":           map[string]any{"id": uuid.NewString(), "author": map[string]any{"role": "user"}, "content": map[string]any{"content_type": "text", "parts": []string{prompt}}},
		"client_prepare_dispatch": "debounced", "client_prepare_source": "composer_editor_state",
	}, map[string]string{"X-Oai-Turn-Trace-Id": traceID}, true)
	if err != nil {
		return nil, nil, err
	}
	conduit, _ := prepared["conduit_token"].(string)

	mention := imageCreationMention + " " + prompt
	parts := []any{}
	for _, ref := range refs {
		parts = append(parts, map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + ref.FileID, "width": ref.Width, "height": ref.Height, "size_bytes": ref.FileSize})
	}
	parts = append(parts, mention)
	content := map[string]any{"content_type": "text", "parts": []any{mention}}
	if len(refs) > 0 {
		content = map[string]any{"content_type": "multimodal_text", "parts": parts}
	}
	metadata := map[string]any{
		"developer_mode_connector_ids": []any{}, "selected_github_repos": []any{}, "selected_all_github_repos": false, "system_hints": []string{"picture_v2"},
		"serialization_metadata": map[string]any{"render_format": "markdown", "custom_symbol_offsets": []any{map[string]any{
			"id": "picture_v2", "symbol": "ecosystemMention", "startIndex": 0, "endIndex": len([]rune(imageCreationMention)),
		}}},
	}
	if len(refs) > 0 {
		attachments := []any{}
		for _, ref := range refs {
			attachments = append(attachments, map[string]any{"id": ref.FileID, "mimeType": ref.MimeType, "name": ref.FileName, "size": ref.FileSize, "width": ref.Width, "height": ref.Height})
		}
		metadata["attachments"] = attachments
	}
	if settingsForSize := ImageGenerationSettings(size); settingsForSize != nil {
		metadata["image_generation_settings"] = settingsForSize
	}
	payload := map[string]any{
		"turn_attribution": map[string]any{"turn_trigger": "composer"}, "action": "next", "is_do_not_remember": false,
		"messages": []any{map[string]any{
			"id": uuid.NewString(), "author": map[string]any{"metadata": map[string]any{}, "name": nil, "role": "user"}, "channel": nil,
			"create_time": float64(time.Now().UnixNano()) / 1e9, "end_turn": nil, "content": content, "metadata": metadata,
			"recipient": "all", "status": "finished_successfully", "update_time": nil, "weight": 1,
		}},
		"parent_message_id": clientCreatedRoot, "model": upstreamModel, "system_hints": []string{"picture_v2"},
		"timezone_offset_min": -480, "timezone": "Asia/Shanghai", "client_contextual_info": clientContext(),
		"local_function_names": []string{"local.continue_in_work"}, "supported_encodings": []string{"v1"},
		"genui_state_snapshots": []any{}, "client_prepare_state": "success",
	}
	headers := map[string]string{"Accept": "text/event-stream", "X-Oai-Turn-Trace-Id": traceID}
	if conduit != "" {
		headers["X-Conduit-Token"] = conduit
	}
	response, err := b.withChallengeRetry(ctx, func() (*http.Response, error) {
		return b.API.Request(ctx, http.MethodPost, "/backend-api/f/conversation", payload, headers)
	})
	if err != nil {
		return nil, nil, err
	}
	return b.capStream(response, float64(b.Settings.ImagePollTimeoutSecs()))
}

// capStream is _iter_sse_payloads_capped: data payloads, and the connection is closed once the
// hard cap passes so a hung stream cannot block forever. capSecs <= 0 means no cap (iter_sse_payloads).
func (b *Backend) capStream(response *http.Response, capSecs float64) (<-chan string, func() error, error) {
	out := make(chan string, 16)
	var streamErr error
	done := make(chan struct{})
	if capSecs <= 0 {
		capSecs = 100 * 365 * 24 * 3600
	}
	deadline := time.Now().Add(time.Duration(capSecs * float64(time.Second)))
	var once sync.Once
	closeBody := func() { once.Do(func() { response.Body.Close() }) }
	watchdog := time.AfterFunc(time.Until(deadline), closeBody)
	message := fmt.Sprintf("图片生成流已超过硬上限 %d 秒，已强制中断（上游可能未生成图片）", int(capSecs))
	go func() {
		defer close(done)
		defer close(out)
		defer watchdog.Stop()
		defer closeBody()
		reader := bufio.NewReaderSize(response.Body, 64*1024)
		for {
			line, err := reader.ReadString('\n')
			if trimmed := strings.TrimRight(line, "\r\n"); strings.HasPrefix(trimmed, "data:") {
				if payload := strings.TrimSpace(trimmed[5:]); payload != "" {
					out <- payload
					if !time.Now().Before(deadline) {
						streamErr = errors.New(message)
						return
					}
				}
			}
			if err != nil {
				if err != io.EOF {
					if !time.Now().Before(deadline) {
						streamErr = errors.New(message)
					} else {
						streamErr = err
					}
				}
				return
			}
		}
	}()
	return out, func() error { <-done; return streamErr }, nil
}

// QueryTasks is _query_backend_tasks filtered by conversation.
func (b *Backend) QueryTasks(ctx context.Context, conversationID string, timeout time.Duration) ([]map[string]any, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err := b.getJSON(callCtx, http.MethodGet, "/backend-api/tasks", nil, map[string]string{"Accept": "application/json"}, false)
	if err != nil {
		return nil, err
	}
	list, _ := data["tasks"].([]any)
	out := []map[string]any{}
	for _, item := range list {
		task, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if conversationID != "" && task["conversation_id"] != conversationID && task["original_conversation_id"] != conversationID {
			continue
		}
		out = append(out, task)
	}
	return out, nil
}

// CheckTaskError is check_task_error.
func (b *Backend) CheckTaskError(task map[string]any) (bool, string) {
	message, _ := task["image_gen_message"].(map[string]any)
	if len(message) == 0 {
		return false, ""
	}
	metadata, _ := message["metadata"].(map[string]any)
	content, _ := message["content"].(map[string]any)
	isError, _ := metadata["is_error"].(bool)
	if !isError || content["content_type"] != "text" {
		return isError, ""
	}
	parts, _ := content["parts"].([]any)
	var text strings.Builder
	for _, part := range parts {
		if s, ok := part.(string); ok {
			text.WriteString(s)
		}
	}
	return true, text.String()
}

// DeleteConversation hides the conversation (PATCH is_visible=false).
func (b *Backend) DeleteConversation(ctx context.Context, conversationID string) error {
	_, err := b.getJSON(ctx, http.MethodPatch, "/backend-api/conversation/"+conversationID, map[string]any{"is_visible": false}, map[string]string{
		"Accept": "*/*", "Referer": b.API.BaseURL() + "/c/" + conversationID, "X-OpenAI-Target-Route": "/backend-api/conversation/{conversation_id}",
	}, false)
	return err
}

func (b *Backend) getConversation(ctx context.Context, conversationID string) (map[string]any, error) {
	return b.getJSON(ctx, http.MethodGet, "/backend-api/conversation/"+conversationID, nil, map[string]string{"Accept": "application/json"}, false)
}

func numberOf(value any) float64 {
	switch v := value.(type) {
	case json.Number:
		f, _ := v.Float64()
		return f
	case float64:
		return v
	case string:
		var f float64
		fmt.Sscan(v, &f)
		return f
	}
	return 0
}

// FindConversationByPrompt is find_conversation_by_prompt: the recent conversation whose title
// shares the most words with the prompt (image titles get a bonus), else the newest one.
func (b *Backend) FindConversationByPrompt(ctx context.Context, prompt string, since time.Time, timeout time.Duration) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, err := b.getJSON(callCtx, http.MethodGet, "/backend-api/conversations?offset=0&limit=10&order=updated&conversation_filter=all", nil, map[string]string{"Accept": "application/json"}, false)
	if err != nil {
		return "", nil
	}
	items, _ := data["items"].([]any)
	if len(items) == 0 {
		items, _ = data["conversations"].([]any)
	}
	started := float64(since.UnixNano()) / 1e9
	words := strings.Fields(strings.ToLower(strings.TrimSpace(prompt)))
	promptWords := map[string]bool{}
	for _, word := range words {
		promptWords[word] = true
	}
	best, bestScore := "", 0.0
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		id := firstString(item["id"], item["conversation_id"])
		if id == "" {
			continue
		}
		updated := numberOf(firstNonNil(item["update_time"], item["updated_at"]))
		if updated != 0 && started != 0 && (updated < started-30 || updated > started+600) {
			continue
		}
		title := strings.ToLower(firstString(item["title"]))
		score := 0.0
		if len(promptWords) > 0 && title != "" {
			common := 0
			seen := map[string]bool{}
			for _, word := range strings.Fields(title) {
				if promptWords[word] && !seen[word] {
					seen[word] = true
					common++
				}
			}
			if common > 0 {
				score = float64(common) / float64(max(len(promptWords), 1))
			}
		}
		if strings.HasPrefix(title, "image") {
			score += 0.3
		}
		if score > bestScore {
			best, bestScore = id, score
		}
	}
	if best != "" && bestScore > 0.1 {
		return best, nil
	}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		id := firstString(item["id"], item["conversation_id"])
		updated := numberOf(firstNonNil(item["update_time"], item["updated_at"]))
		if id != "" && updated != 0 && started != 0 && updated >= started-30 {
			return id, nil
		}
	}
	return "", nil
}

func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil && value != "" && value != false {
			return value
		}
	}
	return nil
}

// jitter is random.uniform(0, n).
func jitter(n float64) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration(rand.Float64() * n * float64(time.Second))
}
