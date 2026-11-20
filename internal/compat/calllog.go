package compat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

// ---- services/content_filter.py ----

const defaultReviewPrompt = "判断用户请求是否允许。只回答 ALLOW 或 REJECT。"
const maxReviewTextLen = 100_000
const truncationMarker = "\n…[truncated]…\n"

var base64DataURI = regexp.MustCompile(`data:[\w/.+;-]+;base64,[A-Za-z0-9+/=]+`)

func reviewText(text string) string {
	sanitized := base64DataURI.ReplaceAllString(text, "[image]")
	runes := []rune(sanitized)
	if len(runes) > maxReviewTextLen {
		half := (maxReviewTextLen - len([]rune(truncationMarker))) / 2
		sanitized = string(runes[:half]) + truncationMarker + string(runes[len(runes)-half:])
	}
	return sanitized
}

func hasPrefixAny(text string, prefixes ...string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// CheckRequest is check_request: sensitive words, then the optional AI review.
func CheckRequest(store *settings.Store, text string) *httpError {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	for _, word := range store.SensitiveWords() {
		if strings.Contains(text, word) {
			return badRequest("检测到敏感词，拒绝本次任务")
		}
	}
	review := store.AIReview()
	if !truthyValue(review["enabled"]) {
		return nil
	}
	baseURL := strings.TrimRight(strings.TrimSpace(fmt.Sprint(firstOf(review["base_url"], ""))), "/")
	apiKey := strings.TrimSpace(fmt.Sprint(firstOf(review["api_key"], "")))
	model := strings.TrimSpace(fmt.Sprint(firstOf(review["model"], "")))
	if baseURL == "" || apiKey == "" || model == "" {
		return badRequest("ai review config is incomplete")
	}
	failOpen := true
	switch v := review["fail_open"].(type) {
	case bool:
		failOpen = v
	case string:
		failOpen = map[string]bool{"1": true, "true": true, "yes": true, "on": true}[strings.ToLower(strings.TrimSpace(v))]
	case nil:
	default:
		failOpen = truthyValue(v)
	}
	onFailure := func() *httpError {
		if !failOpen {
			return &httpError{status: 503, message: "AI 审核服务暂时不可用，请稍后重试"}
		}
		return nil
	}
	prompt := strings.TrimSpace(fmt.Sprint(firstOf(review["prompt"], defaultReviewPrompt)))
	content := prompt + "\n\n用户请求:\n" + reviewText(text) + "\n\n只回答 ALLOW 或 REJECT。"
	body, _ := json.Marshal(map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": content}}, "temperature": 0})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy := store.Proxy(); proxy != "" {
		if proxyURL, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 60 * time.Second, Transport: transport}).Do(req)
	if err != nil {
		return onFailure()
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return onFailure()
	}
	choices, _ := data["choices"].([]any)
	if len(choices) == 0 {
		return onFailure()
	}
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message == nil || message["content"] == nil {
		return onFailure()
	}
	decision := strings.ToLower(strings.TrimSpace(fmt.Sprint(message["content"])))
	if hasPrefixAny(decision, "allow", "pass", "true", "yes", "通过", "允许", "安全") {
		return nil
	}
	if hasPrefixAny(decision, "reject", "deny", "block", "false", "no", "拒绝", "不允许", "违规", "禁止") {
		return badRequest("AI 审核未通过，拒绝本次任务")
	}
	return onFailure()
}

// ---- LoggedCall (services/log_service.py) ----

// CallLog records one API call like LoggedCall.
type CallLog struct {
	logs        *logs.Service
	identity    Identity
	endpoint    string
	model       string
	summary     string
	started     time.Time
	requestText string
	shape       map[string]int
	size        string
}

func newCallLog(l *logs.Service, identity Identity, endpoint, model, summary, requestText, size string) *CallLog {
	return &CallLog{logs: l, identity: identity, endpoint: endpoint, model: model, summary: summary, started: time.Now(), requestText: requestText, size: size}
}

func requestExcerpt(text string) string {
	value := strings.TrimSpace(text)
	if value == "" {
		return ""
	}
	normalized := strings.Join(strings.Fields(value), " ")
	runes := []rune(normalized)
	if len(runes) <= 1000 {
		return normalized
	}
	return strings.TrimRight(string(runes[:999]), " \t\n") + "…"
}

func collect(value any, match func(key string, item any) ([]string, bool)) []string {
	out := []string{}
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			if found, ok := match(key, item); ok {
				out = append(out, found...)
			} else {
				out = append(out, collect(item, match)...)
			}
		}
	case []any:
		for _, item := range v {
			out = append(out, collect(item, match)...)
		}
	case []map[string]any:
		for _, item := range v {
			out = append(out, collect(item, match)...)
		}
	}
	return out
}

func collectURLs(value any) []string {
	return collect(value, func(key string, item any) ([]string, bool) {
		if text, ok := item.(string); ok && key == "url" {
			return []string{text}, true
		}
		if list, ok := item.([]any); ok && key == "urls" {
			out := []string{}
			for _, entry := range list {
				if text, ok := entry.(string); ok {
					out = append(out, text)
				}
			}
			return out, true
		}
		return nil, false
	})
}

func collectNamed(value any, keys ...string) []string {
	wanted := map[string]bool{}
	for _, key := range keys {
		wanted[key] = true
	}
	return collect(value, func(key string, item any) ([]string, bool) {
		if text, ok := item.(string); ok && wanted[key] && strings.TrimSpace(text) != "" {
			return []string{strings.TrimSpace(text)}, true
		}
		return nil, false
	})
}

// Log is LoggedCall.log.
func (c *CallLog) Log(suffix string, result any, status, errText string, urls []string, email, conversationID string) {
	if c.logs == nil {
		return
	}
	if status == "" {
		status = "success"
	}
	detail := map[string]any{
		"key_id": c.identity["id"], "key_name": c.identity["name"], "role": c.identity["role"], "endpoint": c.endpoint, "model": c.model,
		"started_at": c.started.Format("2006-01-02 15:04:05"), "ended_at": time.Now().Format("2006-01-02 15:04:05"),
		"duration_ms": time.Since(c.started).Milliseconds(), "status": status,
	}
	if excerpt := requestExcerpt(c.requestText); excerpt != "" {
		detail["request_text"] = excerpt
	}
	if len(c.shape) > 0 {
		detail["request_shape"] = c.shape
	}
	if size := strings.TrimSpace(c.size); size != "" {
		detail["size"] = size
	}
	if errText != "" {
		detail["error"] = errText
	}
	if email = strings.TrimSpace(email); email == "" {
		if emails := collectNamed(result, "_account_email", "account_email"); len(emails) > 0 {
			email = emails[0]
		}
	}
	if email != "" {
		detail["account_email"] = email
	}
	if conversationID = strings.TrimSpace(conversationID); conversationID == "" {
		if ids := collectNamed(result, "_conversation_id"); len(ids) > 0 {
			conversationID = ids[0]
		}
	}
	if conversationID != "" {
		detail["conversation_id"] = conversationID
	}
	all := append(append([]string{}, urls...), collectURLs(result)...)
	if len(all) > 0 && !strings.HasPrefix(c.endpoint, "/v1/search") {
		seen := map[string]bool{}
		unique := []any{}
		for _, item := range all {
			if !seen[item] {
				seen[item] = true
				unique = append(unique, item)
			}
		}
		detail["urls"] = unique
	}
	c.logs.Add(logs.TypeCall, c.summary+suffix, detail)
}

// stripInternal removes _account_email and _conversation_id before responding.
func stripInternal(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if key == "_account_email" || key == "_conversation_id" {
				continue
			}
			out[key] = stripInternal(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = stripInternal(item)
		}
		return out
	case []map[string]any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = stripInternal(item)
		}
		return out
	}
	return value
}

// ---- services/protocol/error_response.py ----

func defaultErrorType(status int) string {
	switch {
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 429:
		return "rate_limit_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	}
	return "server_error"
}

func defaultErrorCode(status int) string {
	switch {
	case status == 401:
		return "invalid_api_key"
	case status == 403:
		return "permission_denied"
	case status == 429:
		return "rate_limit_exceeded"
	case status >= 400 && status < 500:
		return "bad_request"
	}
	return "upstream_error"
}

// openAIError is openai_error_payload for a plain message.
func openAIError(message string, status int) map[string]any {
	if message == "" {
		message = "request failed"
	}
	return map[string]any{"error": map[string]any{"message": message, "type": defaultErrorType(status), "param": nil, "code": defaultErrorCode(status)}}
}
