// Package compat serves the API of the Python version (main) on top of the Go services, so the
// Next.js frontend and existing clients keep working unchanged. Response bodies, status codes and
// error texts follow the Python handlers in api/*.py.
package compat

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/gin-gonic/gin"
)

// abortDetail answers like FastAPI's HTTPException(detail={"error": message}).
func abortDetail(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, gin.H{"detail": gin.H{"error": message}})
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
	c.AbortWithStatusJSON(422, gin.H{"detail": []gin.H{{"type": "value_error", "loc": []string{"body", field}, "msg": message, "input": nil}}})
}
