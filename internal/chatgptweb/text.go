package chatgptweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	http "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

// Text conversation, model list and search of the Python OpenAIBackendAPI (stream_conversation,
// list_models, search). Without an access token the anonymous /backend-anon routes are used.

const (
	SearchModel          = "gpt-5-5"
	searchTimeout        = 300 * time.Second
	searchPollInterval   = 3 * time.Second
	textConversationPath = "/backend-api/conversation"
	anonConversationPath = "/backend-anon/conversation"
)

var (
	searchDoneStatus       = map[string]bool{"finished_successfully": true, "finished_partial_completion": true}
	searchConversationIDRE = regexp.MustCompile(`"conversation_id"\s*:\s*"([^"]+)"`)
	searchURLRE            = regexp.MustCompile(`https?://[^\s"'<>）)\]}]+`)
)

func (b *Backend) anonymous() bool { return b.API.AccessToken() == "" }

// NormalizeThinkingEffort is _normalize_thinking_effort of the backend.
func NormalizeThinkingEffort(value string) string {
	switch normalized := strings.ToLower(strings.TrimSpace(value)); normalized {
	case "low", "medium", "high", "standard", "max":
		return normalized
	case "xhigh", "extended":
		return "extended"
	}
	return ""
}

// conversationMessages is _api_messages_to_conversation_messages. Image parts are
// {"type": "image", "data": []byte, "mime": "..."} and get uploaded first.
func (b *Backend) conversationMessages(ctx context.Context, messages []map[string]any) ([]any, error) {
	out := []any{}
	for _, item := range messages {
		role, _ := item["role"].(string)
		if role == "" {
			role = "user"
		}
		switch content := item["content"].(type) {
		case string:
			out = append(out, map[string]any{"id": uuid.NewString(), "author": map[string]any{"role": role},
				"content": map[string]any{"content_type": "text", "parts": []any{content}}})
			continue
		case []any:
			texts := []string{}
			type imageInput struct {
				data []byte
				mime string
			}
			images := []imageInput{}
			for _, raw := range content {
				part, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				switch part["type"] {
				case "text":
					texts = append(texts, fmt.Sprint(firstNonNil(part["text"], "")))
				case "image":
					if data, ok := part["data"].([]byte); ok {
						mime, _ := part["mime"].(string)
						if mime == "" {
							mime = "image/png"
						}
						images = append(images, imageInput{data, mime})
					}
				}
			}
			if len(images) == 0 {
				out = append(out, map[string]any{"id": uuid.NewString(), "author": map[string]any{"role": role},
					"content": map[string]any{"content_type": "text", "parts": []any{strings.Join(texts, "")}}})
				continue
			}
			if b.anonymous() {
				return nil, errors.New("authenticated upstream account required for image input")
			}
			refs := []uploaded{}
			for i, image := range images {
				ext := "png"
				if _, after, ok := strings.Cut(image.mime, "/"); ok {
					ext = strings.SplitN(after, "+", 2)[0]
				}
				if ext == "jpeg" {
					ext = "jpg"
				}
				if ext == "" {
					ext = "png"
				}
				ref, err := b.upload(ctx, image.data, fmt.Sprintf("image_%d.%s", i+1, ext))
				if err != nil {
					return nil, err
				}
				refs = append(refs, ref)
			}
			parts := []any{}
			attachments := []any{}
			for _, ref := range refs {
				parts = append(parts, map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "file-service://" + ref.FileID,
					"width": ref.Width, "height": ref.Height, "size_bytes": ref.FileSize})
				attachments = append(attachments, map[string]any{"id": ref.FileID, "mimeType": ref.MimeType, "name": ref.FileName,
					"size": ref.FileSize, "width": ref.Width, "height": ref.Height})
			}
			if text := strings.Join(texts, ""); text != "" {
				parts = append(parts, text)
			}
			out = append(out, map[string]any{"id": uuid.NewString(), "author": map[string]any{"role": role},
				"content": map[string]any{"content_type": "multimodal_text", "parts": parts}, "metadata": map[string]any{"attachments": attachments}})
		default:
			return nil, errors.New("only string or list message content is supported")
		}
	}
	return out, nil
}

// StreamText is stream_conversation for text: the SSE data payloads of one web conversation.
func (b *Backend) StreamText(ctx context.Context, messages []map[string]any, model, thinkingEffort string) (<-chan string, func() error, error) {
	if err := convertErr(b.API.Bootstrap(ctx)); err != nil {
		return nil, nil, err
	}
	if err := convertErr(b.API.ChatRequirements(ctx)); err != nil {
		return nil, nil, err
	}
	path, timezone := textConversationPath, "Asia/Shanghai"
	if b.anonymous() {
		path, timezone = anonConversationPath, "America/Los_Angeles"
	}
	converted, err := b.conversationMessages(ctx, messages)
	if err != nil {
		return nil, nil, err
	}
	payload := map[string]any{
		"action": "next", "messages": converted, "model": model, "parent_message_id": uuid.NewString(),
		"conversation_mode": map[string]any{"kind": "primary_assistant"}, "conversation_origin": nil, "force_paragen": false,
		"force_paragen_model_slug": "", "force_rate_limit": false, "force_use_sse": true, "history_and_training_disabled": true,
		"reset_rate_limits": false, "suggestions": []any{}, "supported_encodings": []any{}, "system_hints": []any{},
		"timezone": timezone, "timezone_offset_min": -480, "variant_purpose": "comparison_implicit", "websocket_request_id": uuid.NewString(),
		"client_contextual_info": map[string]any{"is_dark_mode": false, "time_since_loaded": 120, "page_height": 900, "page_width": 1400,
			"pixel_ratio": 2, "screen_height": 1440, "screen_width": 2560},
	}
	if thinkingEffort == "" && b.Settings != nil {
		thinkingEffort = b.Settings.DefaultThinkingEffort()
	}
	if effort := NormalizeThinkingEffort(thinkingEffort); effort != "" {
		payload["thinking_effort"] = effort
	}
	response, err := b.API.Request(ctx, http.MethodPost, path, payload, map[string]string{"Accept": "text/event-stream"})
	if err != nil {
		return nil, nil, convertErr(err)
	}
	return b.capStream(response, 0)
}

// ListModels is list_models: the account's (or the anonymous) models as an OpenAI list.
func (b *Backend) ListModels(ctx context.Context) (map[string]any, error) {
	if err := convertErr(b.API.Bootstrap(ctx)); err != nil {
		return nil, err
	}
	path := "/backend-api/models?history_and_training_disabled=false"
	if b.anonymous() {
		path = "/backend-anon/models?iim=false&is_gizmo=false"
	}
	result, err := b.getJSON(ctx, http.MethodGet, path, nil, nil, false)
	if err != nil {
		return nil, err
	}
	data, seen := []map[string]any{}, map[string]bool{}
	models, _ := result["models"].([]any)
	for _, raw := range models {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		slug := strings.TrimSpace(fmt.Sprint(firstNonNil(item["slug"], "")))
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		created := int64(numberOf(item["created"]))
		owner := strings.TrimSpace(fmt.Sprint(firstNonNil(item["owned_by"], "")))
		if owner == "" {
			owner = "chatgpt"
		}
		data = append(data, map[string]any{"id": slug, "object": "model", "created": created, "owned_by": owner,
			"permission": []any{}, "root": slug, "parent": nil})
	}
	sort.Slice(data, func(i, j int) bool { return data[i]["id"].(string) < data[j]["id"].(string) })
	return map[string]any{"object": "list", "data": data}, nil
}

// Search is search(): a web-search conversation, then poll it until the answer is final.
func (b *Backend) Search(ctx context.Context, prompt string) (map[string]any, error) {
	if b.anonymous() {
		return nil, errors.New("access_token is required for search")
	}
	prepared, err := b.getJSON(ctx, http.MethodPost, "/backend-api/f/conversation/prepare", map[string]any{
		"action": "next", "fork_from_shared_post": false, "parent_message_id": clientCreatedRoot, "model": SearchModel,
		"client_prepare_state": "success", "timezone_offset_min": -480, "timezone": "Asia/Shanghai",
		"conversation_mode": map[string]any{"kind": "primary_assistant"}, "system_hints": []string{"search"},
		"partial_query":      map[string]any{"id": uuid.NewString(), "author": map[string]any{"role": "user"}, "content": map[string]any{"content_type": "text", "parts": []string{prompt}}},
		"supports_buffering": true, "supported_encodings": []string{"v1"}, "client_contextual_info": map[string]any{"app_name": "chatgpt.com"},
	}, map[string]string{"Accept": "*/*", "X-Conduit-Token": "no-token"}, true)
	if err != nil {
		return nil, err
	}
	conduit, _ := prepared["conduit_token"].(string)
	if conduit == "" {
		return nil, errors.New("missing conduit_token")
	}
	if err := convertErr(b.API.Bootstrap(ctx)); err != nil {
		return nil, err
	}
	if err := convertErr(b.API.ChatRequirements(ctx)); err != nil {
		return nil, err
	}
	payload := map[string]any{
		"action": "next",
		"messages": []any{map[string]any{"id": uuid.NewString(), "author": map[string]any{"role": "user"}, "create_time": float64(time.Now().UnixNano()) / 1e9,
			"content": map[string]any{"content_type": "text", "parts": []string{prompt}},
			"metadata": map[string]any{"developer_mode_connector_ids": []any{}, "selected_github_repos": []any{}, "selected_all_github_repos": false,
				"system_hints": []string{"search"}, "serialization_metadata": map[string]any{"custom_symbol_offsets": []any{}}}}},
		"parent_message_id": clientCreatedRoot, "model": SearchModel, "client_prepare_state": "success", "timezone_offset_min": -480,
		"timezone": "Asia/Shanghai", "conversation_mode": map[string]any{"kind": "primary_assistant"}, "enable_message_followups": true,
		"system_hints": []any{}, "supports_buffering": true, "supported_encodings": []string{"v1"}, "force_use_search": true,
		"client_reported_search_source": "conversation_composer_web_icon",
		"client_contextual_info": map[string]any{"is_dark_mode": false, "time_since_loaded": 36, "page_height": 925, "page_width": 886,
			"pixel_ratio": 2, "screen_height": 1440, "screen_width": 2560, "app_name": "chatgpt.com"},
		"paragen_cot_summary_display_override": "allow", "force_parallel_switch": "auto",
	}
	response, err := b.withChallengeRetry(ctx, func() (*http.Response, error) {
		return b.API.Request(ctx, http.MethodPost, "/backend-api/f/conversation", payload,
			map[string]string{"Accept": "text/event-stream", "X-Conduit-Token": conduit})
	})
	if err != nil {
		return nil, err
	}
	payloads, wait, _ := b.capStream(response, 0)
	conversationID := ""
	for item := range payloads {
		if conversationID == "" {
			conversationID = findSearchValue(item, "conversation_id")
		}
		if item == "[DONE]" {
			break
		}
	}
	go func() {
		for range payloads {
		}
	}()
	response.Body.Close()
	wait()
	if conversationID == "" {
		return nil, errors.New("conversation_id not found in stream")
	}
	return b.waitSearchResult(ctx, conversationID)
}

func (b *Backend) waitSearchResult(ctx context.Context, conversationID string) (map[string]any, error) {
	deadline := time.Now().Add(searchTimeout)
	var last map[string]any
	lastAnswer, stable := "", 0
	for time.Now().Before(deadline) {
		conversation, err := b.getJSON(ctx, http.MethodGet, "/backend-api/conversation/"+conversationID, nil,
			map[string]string{"Accept": "*/*", "Referer": b.API.BaseURL() + "/c/" + conversationID,
				"X-OpenAI-Target-Route": "/backend-api/conversation/{conversation_id}"}, false)
		if err != nil {
			var httpErr *imagegen.UpstreamHTTPError
			if !errors.As(err, &httpErr) || !map[int]bool{404: true, 409: true, 423: true, 429: true, 500: true, 502: true, 503: true, 504: true}[httpErr.StatusCode] {
				return nil, err
			}
		} else {
			last = extractSearchResult(conversationID, conversation)
		}
		if last != nil && last["answer"] != "" {
			if searchDoneStatus[last["status"].(string)] {
				return last, nil
			}
			answer := last["answer"].(string)
			if answer == lastAnswer {
				stable++
			} else {
				stable = 0
			}
			lastAnswer = answer
			if stable >= 2 {
				return last, nil
			}
		}
		b.sleep(ctx, searchPollInterval)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if last != nil {
		return last, nil
	}
	return nil, fmt.Errorf("timed out waiting for search result: %s", conversationID)
}

func extractSearchResult(conversationID string, conversation map[string]any) map[string]any {
	var message map[string]any
	best := math.Inf(-1)
	mapping, _ := conversation["mapping"].(map[string]any)
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		node, _ := mapping[key].(map[string]any)
		candidate, _ := node["message"].(map[string]any)
		author, _ := candidate["author"].(map[string]any)
		if author["role"] != "assistant" {
			continue
		}
		if created := numberOf(candidate["create_time"]); created > best {
			best, message = created, candidate
		}
	}
	if message == nil {
		message = map[string]any{}
	}
	metadata, _ := message["metadata"].(map[string]any)
	finish, _ := metadata["finish_details"].(map[string]any)
	answer := searchMessageText(message)
	sources := extractSearchSources(message)
	for _, url := range searchURLRE.FindAllString(answer, -1) {
		url = cleanSearchURL(url)
		if url != "" && !hasSource(sources, url) {
			sources = append(sources, map[string]any{"title": "", "url": url, "snippet": "", "source_type": ""})
		}
	}
	status := strings.TrimSpace(firstString(finish["type"], metadata["status"], findSearchValue(message, "status")))
	return map[string]any{"conversation_id": conversationID, "status": status, "answer": answer, "sources": sources,
		"assistant_message_id": fmt.Sprint(firstNonNil(message["id"], "")), "create_time": numberOf(message["create_time"])}
}

func hasSource(sources []map[string]any, url string) bool {
	for _, item := range sources {
		if item["url"] == url {
			return true
		}
	}
	return false
}

func cleanSearchURL(value any) string {
	text, _ := value.(string)
	return strings.TrimRight(strings.TrimSpace(text), ".,;，。；")
}

func walkDicts(payload any) []map[string]any {
	out := []map[string]any{}
	switch v := payload.(type) {
	case map[string]any:
		out = append(out, v)
		for _, key := range sortedKeys(v) {
			out = append(out, walkDicts(v[key])...)
		}
	case []any:
		for _, item := range v {
			out = append(out, walkDicts(item)...)
		}
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func extractSearchSources(payload any) []map[string]any {
	sources := []map[string]any{}
	for _, obj := range walkDicts(payload) {
		metadata, _ := obj["metadata"].(map[string]any)
		url := cleanSearchURL(firstNonEmptyValue(obj["url"], obj["link"], obj["source_url"], metadata["url"]))
		if url == "" || hasSource(sources, url) {
			continue
		}
		sources = append(sources, map[string]any{
			"title": strings.TrimSpace(firstString(obj["title"], obj["name"], obj["source"])), "url": url,
			"snippet":     strings.TrimSpace(firstString(obj["snippet"], obj["text"], obj["description"])),
			"source_type": strings.TrimSpace(firstString(obj["type"], obj["source_type"])),
		})
	}
	return sources
}

// firstNonEmptyValue is Python's `a or b or c` over strings.
func firstNonEmptyValue(values ...any) any {
	for _, value := range values {
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
}

func searchMessageText(message map[string]any) string {
	parts := []string{}
	switch content := message["content"].(type) {
	case map[string]any:
		if text, ok := content["text"].(string); ok {
			parts = append(parts, text)
		}
		list, _ := content["parts"].([]any)
		for _, part := range list {
			switch p := part.(type) {
			case string:
				parts = append(parts, p)
			case map[string]any:
				for _, key := range []string{"text", "summary", "content"} {
					if value, ok := p[key]; ok && value != nil && value != "" {
						parts = append(parts, fmt.Sprint(value))
					}
				}
			}
		}
	case string:
		parts = append(parts, content)
	}
	kept := []string{}
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			kept = append(kept, strings.TrimSpace(part))
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// findSearchValue is _find_search_value: the first non-empty string under key, anywhere.
func findSearchValue(payload any, key string) string {
	if text, ok := payload.(string); ok {
		if key == "conversation_id" {
			if match := searchConversationIDRE.FindStringSubmatch(text); match != nil {
				return match[1]
			}
		}
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) != nil {
			return ""
		}
		payload = decoded
	}
	switch v := payload.(type) {
	case map[string]any:
		if value, ok := v[key].(string); ok && value != "" {
			return value
		}
		for _, k := range sortedKeys(v) {
			if found := findSearchValue(v[k], key); found != "" {
				return found
			}
		}
	case []any:
		for _, item := range v {
			if found := findSearchValue(item, key); found != "" {
				return found
			}
		}
	}
	return ""
}
