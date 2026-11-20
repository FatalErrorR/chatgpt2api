// Package compat serves the API of the Python version (main) on top of the Go services, so the
// Next.js frontend and existing clients keep working unchanged. Response bodies, status codes and
// error texts follow the Python handlers in api/*.py.
package compat

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/gin-gonic/gin"
)

// abortDetail answers like FastAPI's HTTPException(detail={"error": message}). Under /v1 the
// Python exception handlers (api/errors.py) turn it into an OpenAI error, or an Anthropic one for
// /v1/messages.
func abortDetail(c *gin.Context, status int, message string) {
	path := c.Request.URL.Path
	switch {
	case path == "/v1/messages":
		c.AbortWithStatusJSON(status, anthropicError(message, status))
	case path == "/v1" || strings.HasPrefix(path, "/v1/"):
		c.AbortWithStatusJSON(status, openAIError(message, status))
	default:
		c.AbortWithStatusJSON(status, gin.H{"detail": gin.H{"error": message}})
	}
}

func anthropicError(message string, status int) map[string]any {
	errorType := defaultErrorType(status)
	if status >= 500 {
		errorType = "api_error"
	}
	if message == "" {
		message = "request failed"
	}
	return map[string]any{"type": "error", "error": map[string]any{"type": errorType, "message": message}}
}

// bindJSON reads a JSON object body. Numbers stay json.Number so 5 and 5.0 stay apart,
// as with Python's json module. An empty body is an empty object.
func bindJSON(c *gin.Context) (map[string]any, bool) {
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		abortValidation(c, "body", "There was an error parsing the body")
		return nil, false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil || body == nil {
		abortValidation(c, "body", "Input should be a valid dictionary or object to extract fields from")
		return nil, false
	}
	return body, true
}

// abortValidation answers like FastAPI's request validation error (HTTP 422).
func abortValidation(c *gin.Context, field string, message string) {
	path := c.Request.URL.Path
	if path == "/v1" || strings.HasPrefix(path, "/v1/") {
		// error_message_from_detail: "<loc without body>: <msg>"
		text := message
		if field != "" {
			text = field + ": " + message
		}
		if path == "/v1/messages" {
			c.AbortWithStatusJSON(422, anthropicError(text, 422))
		} else {
			c.AbortWithStatusJSON(422, openAIError(text, 422))
		}
		return
	}
	c.AbortWithStatusJSON(422, gin.H{"detail": []gin.H{{"type": "value_error", "loc": []string{"body", field}, "msg": message, "input": nil}}})
}
