package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/basketikun/chatgpt2api/internal/textapi"
	"github.com/gin-gonic/gin"
)

// The text routes of api/ai.py: /v1/models, /v1/chat/completions, /v1/responses, /v1/messages,
// /v1/search.

// requestTextOf is content_filter.request_text.
func requestTextOf(values ...any) string {
	parts := []string{}
	for _, value := range values {
		if part := strings.TrimSpace(filterText(value)); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n")
}

func filterText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = filterText(item)
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		parts := []string{}
		for _, key := range []string{"text", "input_text", "content", "input", "instructions", "system", "prompt"} {
			parts = append(parts, filterText(v[key]))
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// requestShapeOf is content_filter.request_shape.
func requestShapeOf(values ...any) map[string]int {
	stats := map[string]int{}
	var walk func(value any, key string)
	walk = func(value any, key string) {
		switch v := value.(type) {
		case string:
			lower := strings.ToLower(strings.TrimSpace(v))
			if strings.Contains(lower, "<image>") {
				stats["literal_image_placeholders"]++
			}
			if strings.HasPrefix(lower, "data:image/") {
				stats["data_url_images"]++
			} else if (key == "image_url" || key == "url") && (strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")) {
				stats["remote_image_urls"]++
			}
		case []any:
			for _, item := range v {
				walk(item, key)
			}
		case map[string]any:
			switch strings.TrimSpace(fmt.Sprint(firstNonNilValue(v["type"], ""))) {
			case "message":
				stats["response_message_items"]++
			case "input_image":
				stats["input_image_parts"]++
			case "image_url":
				stats["image_url_parts"]++
			case "image":
				stats["image_parts"]++
			}
			for childKey, child := range v {
				walk(child, childKey)
			}
		}
	}
	for _, value := range values {
		walk(value, "")
	}
	return stats
}

func firstNonNilValue(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

// checkOptional validates the typed fields of the pydantic request models.
func checkOptional(c *gin.Context, body map[string]any, strings_, bools, lists, dictLists, ints []string) bool {
	for _, key := range strings_ {
		if value, present := body[key]; present && value != nil {
			if _, ok := value.(string); !ok {
				abortValidation(c, key, "Input should be a valid string")
				return false
			}
		}
	}
	for _, key := range bools {
		if value, present := body[key]; present && value != nil {
			flag, ok := pydanticBool(value)
			if !ok {
				abortValidation(c, key, "Input should be a valid boolean")
				return false
			}
			body[key] = flag
		}
	}
	for _, key := range ints {
		if value, present := body[key]; present && value != nil {
			n, ok := pydanticInt(value)
			if !ok {
				abortValidation(c, key, "Input should be a valid integer")
				return false
			}
			body[key] = n
		}
	}
	for _, key := range lists {
		if value, present := body[key]; present && value != nil {
			list, ok := value.([]any)
			if !ok {
				abortValidation(c, key, "Input should be a valid list")
				return false
			}
			for _, item := range list {
				if _, isString := item.(string); !isString && key == "modalities" {
					abortValidation(c, key, "Input should be a valid string")
					return false
				}
			}
		}
	}
	for _, key := range dictLists {
		if value, present := body[key]; present && value != nil {
			list, ok := value.([]any)
			if !ok {
				abortValidation(c, key, "Input should be a valid list")
				return false
			}
			for _, item := range list {
				if _, isMap := item.(map[string]any); !isMap {
					abortValidation(c, key, "Input should be a valid dictionary")
					return false
				}
			}
		}
	}
	return true
}

// errorTypeName stands in for exc.__class__.__name__ in stream error chunks.
func errorTypeName(err error) string {
	var httpErr *textapi.HTTPError
	if errors.As(err, &httpErr) {
		return "HTTPException"
	}
	var generation *imagegen.GenerationError
	if errors.As(err, &generation) {
		return "ImageGenerationError"
	}
	return "RuntimeError"
}

// failCall answers an error raised before the response started, like LoggedCall.run.
func failCall(c *gin.Context, call *CallLog, err error, anthropic bool) {
	var httpErr *textapi.HTTPError
	var generation *imagegen.GenerationError
	switch {
	case errors.As(err, &httpErr):
		call.Log("调用失败", nil, "failed", detailRepr(httpErr.Message), nil, "", "")
		abortDetail(c, httpErr.Status, httpErr.Message)
	case errors.As(err, &generation):
		email, conversation := errorAccount(err)
		call.Log("调用失败", nil, "failed", err.Error(), nil, email, conversation)
		imageErrorResponse(c, err)
	default:
		email, _ := errorAccount(err)
		call.Log("调用失败", nil, "failed", err.Error(), nil, email, "")
		if anthropic {
			c.AbortWithStatusJSON(http.StatusBadGateway, anthropicError(err.Error(), http.StatusBadGateway))
		} else {
			c.AbortWithStatusJSON(http.StatusBadGateway, openAIError(err.Error(), http.StatusBadGateway))
		}
	}
}

func encodeChunk(item map[string]any) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.Encode(stripInternal(toJSONValue(item)))
	return strings.TrimRight(buf.String(), "\n")
}

// runCall is LoggedCall.run: a JSON result is logged and returned; a stream starts only once
// its first chunk is there (an error before that is an ordinary error response), then every chunk
// is written as it comes.
func runCall(c *gin.Context, call *CallLog, result textapi.Result, err error, anthropic bool) {
	if err != nil {
		failCall(c, call, err, anthropic)
		return
	}
	if result.Stream == nil {
		call.Log("调用完成", result.JSON, "success", "", nil, "", "")
		c.JSON(http.StatusOK, stripInternal(result.JSON))
		return
	}
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	chunks := make(chan map[string]any)
	done := make(chan error, 1)
	go func() {
		done <- result.Stream(ctx, func(chunk map[string]any) error {
			select {
			case chunks <- chunk:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		close(chunks)
	}()
	first, ok := <-chunks
	if !ok {
		if streamErr := <-done; streamErr != nil {
			failCall(c, call, streamErr, anthropic)
			return
		}
		call.Log("流式调用结束", nil, "success", "", nil, "", "")
		c.Header("Content-Type", "text/event-stream; charset=utf-8")
		c.Status(http.StatusOK)
		if !anthropic {
			io.WriteString(c.Writer, ": stream-open\n\ndata: [DONE]\n\n")
		}
		return
	}
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	if !anthropic {
		io.WriteString(c.Writer, ": stream-open\n\n")
	}
	urls, emails, conversations := []string{}, []string{}, []string{}
	write := func(chunk map[string]any) {
		urls = append(urls, collectURLs(chunk)...)
		emails = append(emails, collectNamed(chunk, "_account_email", "account_email")...)
		conversations = append(conversations, collectNamed(chunk, "_conversation_id")...)
		if anthropic {
			event := "message_delta"
			if kind, ok := chunk["type"].(string); ok && kind != "" {
				event = kind
			}
			fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, encodeChunk(chunk))
		} else {
			fmt.Fprintf(c.Writer, "data: %s\n\n", encodeChunk(chunk))
		}
		c.Writer.Flush()
	}
	write(first)
	for chunk := range chunks {
		write(chunk)
	}
	streamErr := <-done
	firstOf := func(list []string) string {
		if len(list) > 0 {
			return list[0]
		}
		return ""
	}
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		email, conversation := firstOf(emails), firstOf(conversations)
		if email == "" {
			email, conversation = errorAccount(streamErr)
		}
		call.Log("流式调用失败", nil, "failed", streamErr.Error(), urls, email, conversation)
		if anthropic {
			fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", encodeChunk(map[string]any{"type": "error",
				"error": map[string]any{"type": errorTypeName(streamErr), "message": streamErr.Error()}}))
		} else {
			var generation *imagegen.GenerationError
			if errors.As(streamErr, &generation) {
				fmt.Fprintf(c.Writer, "data: %s\n\n", encodeChunk(generation.OpenAIError()))
			} else {
				fmt.Fprintf(c.Writer, "data: %s\n\n", encodeChunk(map[string]any{"error": map[string]any{"message": streamErr.Error(), "type": errorTypeName(streamErr)}}))
			}
		}
	} else {
		call.Log("流式调用结束", nil, "success", "", urls, firstOf(emails), firstOf(conversations))
	}
	if !anthropic {
		io.WriteString(c.Writer, "data: [DONE]\n\n")
	}
	c.Writer.Flush()
}

// modelFields are the declared fields of each pydantic request model.
var modelFields = map[string][]string{
	"/v1/chat/completions": {"model", "prompt", "n", "stream", "modalities", "messages"},
	"/v1/responses":        {"model", "input", "tools", "tool_choice", "stream"},
	"/v1/messages":         {"model", "messages", "system", "stream"},
}

func registerText(engine *gin.Engine, d *Deps) {
	text := d.Text
	engine.GET("/v1/models", d.Auth.RequireIdentity, func(c *gin.Context) {
		c.JSON(http.StatusOK, text.Catalog.ListModels(c.Request.Context()))
	})
	handle := func(c *gin.Context, endpoint, summary string, validate func(map[string]any) bool, preview func(map[string]any) (string, map[string]int),
		run func(ctx context.Context, body map[string]any) (textapi.Result, error), anthropic bool, modelFallback func(map[string]any) string) {
		body, ok := bindJSON(c)
		if !ok || !validate(body) {
			return
		}
		for _, field := range modelFields[endpoint] { // model_dump() includes the declared fields' None defaults
			if _, present := body[field]; !present {
				body[field] = nil
			}
		}
		authorization := c.GetHeader("Authorization")
		if anthropic && authorization == "" {
			if key := c.GetHeader("x-api-key"); key != "" {
				authorization = "Bearer " + key
			}
		}
		identity := d.Auth.Identify(authorization)
		if identity == nil {
			abortDetail(c, 401, "密钥无效或已失效，请重新登录")
			return
		}
		requestText, shape := preview(body)
		call := newCallLog(d.Logs, identity, endpoint, modelFallback(body), summary, requestText, "")
		call.shape = shape
		if rejected := CheckRequest(d.Settings, requestText); rejected != nil {
			call.Log("调用失败", nil, "failed", detailRepr(rejected.message), nil, "", "")
			abortHTTPError(c, rejected)
			return
		}
		result, err := run(c.Request.Context(), body)
		runCall(c, call, result, err, anthropic)
	}
	modelOrAuto := func(body map[string]any) string {
		if model, _ := body["model"].(string); model != "" {
			return model
		}
		return "auto"
	}
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		handle(c, "/v1/chat/completions", "文本生成", func(body map[string]any) bool {
			return checkOptional(c, body, []string{"model", "prompt"}, []string{"stream"}, []string{"modalities"}, []string{"messages"}, []string{"n"})
		}, func(body map[string]any) (string, map[string]int) {
			return requestTextOf(body["prompt"], body["messages"]), requestShapeOf(body["messages"])
		}, text.ChatCompletions, false, modelOrAuto)
	})
	engine.POST("/v1/responses", func(c *gin.Context) {
		handle(c, "/v1/responses", "Responses", func(body map[string]any) bool {
			return checkOptional(c, body, []string{"model"}, []string{"stream"}, nil, []string{"tools"}, nil)
		}, func(body map[string]any) (string, map[string]int) {
			return requestTextOf(body["input"], body["instructions"]), requestShapeOf(body["input"])
		}, text.Responses, false, modelOrAuto)
	})
	engine.POST("/v1/messages", func(c *gin.Context) {
		handle(c, "/v1/messages", "Messages", func(body map[string]any) bool {
			return checkOptional(c, body, []string{"model"}, []string{"stream"}, nil, []string{"messages"}, nil)
		}, func(body map[string]any) (string, map[string]int) {
			return requestTextOf(body["system"], body["messages"], body["tools"]), nil
		}, text.Messages, true, modelOrAuto)
	})
	engine.POST("/v1/search", func(c *gin.Context) {
		handle(c, "/v1/search", "搜索", func(body map[string]any) bool {
			value, present := body["prompt"]
			if !present {
				abortValidation(c, "prompt", "Field required")
				return false
			}
			prompt, isString := value.(string)
			if !isString {
				abortValidation(c, "prompt", "Input should be a valid string")
				return false
			}
			if prompt == "" {
				abortValidation(c, "prompt", "String should have at least 1 character")
				return false
			}
			return true
		}, func(body map[string]any) (string, map[string]int) {
			return body["prompt"].(string), nil
		}, func(ctx context.Context, body map[string]any) (textapi.Result, error) {
			result, err := text.Search(ctx, body["prompt"].(string))
			return textapi.Result{JSON: result}, err
		}, false, func(map[string]any) string { return textapi.SearchModel })
	})
}
