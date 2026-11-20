// Package imagegen is the image pipeline of the Python version (services/protocol/conversation.py):
// one attempt per account (StreamImageOutputs), the account-switching retry loop
// (GenerateSingleImage) and the result formatting. ChatGPT itself is reached through the Backend
// interface, implemented on the upstream Go client.
package imagegen

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	ModelNotFoundCooldown = 10 * time.Minute
	ForbiddenCooldown     = 10 * time.Minute

	maxStreamResetRetries    = 2
	streamResetBreakerWindow = 5 * time.Minute
	streamResetBreakerLimit  = 10
	upstreamBodyLogLimit     = 500
)

// UpstreamHTTPError is utils/helper.py UpstreamHTTPError: a non-2xx answer from one endpoint.
// Body is the decoded JSON when the answer was JSON, otherwise the text.
type UpstreamHTTPError struct {
	Context    string
	StatusCode int
	Body       any
	RetryAfter int
}

func (e *UpstreamHTTPError) Error() string {
	return fmt.Sprintf("%s failed: status=%d, body=%s", e.Context, e.StatusCode, bodyText(e.Body))
}

// bodyText is the body part of the Python message: json.dumps(ensure_ascii=False) for objects and
// lists, str() otherwise, cut at upstreamBodyLogLimit.
func bodyText(body any) string {
	var text string
	switch v := body.(type) {
	case nil:
		text = "None"
	case string:
		text = v
	case map[string]any, []any:
		text = PyJSONDumps(v)
	default:
		text = fmt.Sprint(v)
	}
	if runes := []rune(text); len(runes) > upstreamBodyLogLimit {
		text = string(runes[:upstreamBodyLogLimit]) + "…[truncated]"
	}
	return text
}

// PollTimeoutError is ImagePollTimeoutError: no image within the wait.
type PollTimeoutError struct {
	Message        string
	ConversationID string
	AccountEmail   string
}

func (e *PollTimeoutError) Error() string { return e.Message }

// ContentPolicyError is ImageContentPolicyError: upstream refused the prompt.
type ContentPolicyError struct {
	Message        string
	ConversationID string
}

func (e *ContentPolicyError) Error() string { return e.Message }

// GenerationError is ImageGenerationError: what the API turns into an OpenAI-style error.
type GenerationError struct {
	Message        string
	StatusCode     int
	ErrorType      string
	Code           any // string or nil
	Param          any
	AccountEmail   string
	ConversationID string
}

func (e *GenerationError) Error() string { return e.Message }

// NewGenerationError fills the Python defaults: 502, server_error, upstream_error.
func NewGenerationError(message string) *GenerationError {
	return &GenerationError{Message: message, StatusCode: 502, ErrorType: "server_error", Code: "upstream_error"}
}

// OpenAIError is to_openai_error().
func (e *GenerationError) OpenAIError() map[string]any {
	body := map[string]any{"message": PublicImageErrorMessage(e.Message), "type": e.ErrorType, "param": e.Param, "code": e.Code}
	if e.AccountEmail != "" {
		body["account_email"] = e.AccountEmail
	}
	return map[string]any{"error": body}
}

// PublicImageErrorMessage hides upstream internals from callers.
func PublicImageErrorMessage(message string) string {
	text := strings.TrimSpace(message)
	lower := strings.ToLower(text)
	for _, marker := range []string{"backend-api/", "status=", "body=", "chatgpt.com", "upstreamhttperror"} {
		if strings.Contains(lower, marker) {
			return "The image generation request failed. Please try again later."
		}
	}
	if text == "" {
		return "The image generation request failed. Please try again later."
	}
	return text
}

func containsAny(text string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(text, fragment) {
			return true
		}
	}
	return false
}

// IsStreamResetError: an HTTP/2 stream cut off before it ended (curl 92) or the Go equivalents.
func IsStreamResetError(message string) bool {
	text := strings.ToLower(message)
	return containsAny(text, "curl: (92)", "was not closed cleanly", "stream error: stream id", "http2: response body closed", "unexpected eof")
}

// IsTokenInvalidError matches the 401 texts of ChatGPT.
func IsTokenInvalidError(message string) bool {
	text := strings.ToLower(message)
	return containsAny(text, "token_invalidated", "token_revoked", "authentication token has been invalidated", "invalidated oauth token", "token invalidated")
}

// IsTLSConnectionError matches TLS/connection failures (curl texts plus the Go ones).
func IsTLSConnectionError(message string) bool {
	text := strings.ToLower(message)
	return containsAny(text, "curl: (35)", "tls connect error", "openssl_internal", "ssl: wrong_version_number", "ssl: certificate_verify_failed",
		"connection aborted", "remote disconnected", "connection reset by peer", "tls: handshake failure", "remote error: tls", "tls: first record does not look like")
}

// IsConnectionTimeoutError matches connect/read timeouts.
func IsConnectionTimeoutError(message string) bool {
	text := strings.ToLower(message)
	return containsAny(text, "curl: (28)", "operation timed out", "connection timed out", "read timed out", "connect timeout",
		"i/o timeout", "context deadline exceeded (client.timeout", "timeout awaiting response headers")
}

// ImageStreamErrorMessage is image_stream_error_message: the text shown for a failed attempt.
func ImageStreamErrorMessage(message string) string {
	switch {
	case IsTokenInvalidError(message):
		return "image generation failed"
	case IsTLSConnectionError(message):
		return "upstream image connection failed, please retry later"
	case IsConnectionTimeoutError(message):
		return "upstream connection timed out, please retry later"
	case IsStreamResetError(message):
		return "upstream connection was interrupted, please retry later"
	case message == "":
		return "image generation failed"
	}
	return message
}

// IsUpstreamForbidden is a 403 from upstream, usually Cloudflare's check on the egress IP.
func IsUpstreamForbidden(err error) bool {
	var httpErr *UpstreamHTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == 403
}

// IsImageModelNotFound matches only the exact {"detail":"Model not found"} 404 of /f/conversation.
func IsImageModelNotFound(err error) bool {
	var httpErr *UpstreamHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != 404 || httpErr.Context != "/backend-api/f/conversation" {
		return false
	}
	body, ok := httpErr.Body.(map[string]any)
	return ok && body["detail"] == "Model not found"
}

var (
	referencedImageIDsRE = regexp.MustCompile(`"referenced_image_ids"\s*:\s*\[([^\]]+)\]`)
	toolParamsJSONRE     = regexp.MustCompile(`\{\s*"size"\s*:\s*"\d+x\d+"\s*,\s*"n"\s*:\s*\d+\s*\}`)
)

// IsModelTextReplyInsteadOfImage: the model answered with the tool's JSON arguments as text
// instead of calling the image tool.
func IsModelTextReplyInsteadOfImage(message string) bool {
	return message != "" && (referencedImageIDsRE.MatchString(message) || toolParamsJSONRE.MatchString(message))
}

// streamResetBreaker counts stream resets across the whole process (the Python module globals).
type streamResetBreaker struct {
	mu    sync.Mutex
	times []time.Time
	now   func() time.Time
}

// note records one reset and reports whether the breaker is open (too many resets lately).
func (b *streamResetBreaker) note() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	kept := b.times[:0]
	for _, at := range b.times {
		if now.Sub(at) < streamResetBreakerWindow {
			kept = append(kept, at)
		}
	}
	b.times = append(kept, now)
	return len(b.times) > streamResetBreakerLimit
}

func (b *streamResetBreaker) reset() {
	b.mu.Lock()
	b.times = nil
	b.mu.Unlock()
}
