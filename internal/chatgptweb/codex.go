package chatgptweb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/upstream"
)

// The Codex route of the Python version (stream_codex_image_outputs, iter_codex_image_response_events):
// a plain HTTPS call to /backend-api/codex/responses with the image_generation tool forced. Like
// the Python urllib call it sends only Authorization and Content-Type, without the account proxy.

const (
	codexResponsesModel        = "gpt-5.5"
	codexResponsesInstructions = "Use the image_generation tool to create exactly one image for the user's request. Return the generated image result."
)

var codexClient = &http.Client{Timeout: 1200 * time.Second}

func codexImageInput(prompt string, images []string) []any {
	content := []any{map[string]any{"type": "input_text", "text": prompt}}
	for _, image := range images {
		payload := image
		if !strings.HasPrefix(image, "data:image/") {
			payload = "data:image/png;base64," + image
		}
		content = append(content, map[string]any{"type": "input_image", "image_url": payload})
	}
	return []any{map[string]any{"role": "user", "content": content}}
}

// codexEvents is _iter_codex_response_events: a JSON body, or SSE events separated by blank lines.
func codexEvents(contentType string, body []byte) []map[string]any {
	events := []map[string]any{}
	if strings.Contains(strings.ToLower(contentType), "application/json") {
		var event map[string]any
		if json.Unmarshal(body, &event) == nil && event != nil {
			events = append(events, event)
		}
		return events
	}
	lines := []string{}
	flush := func() {
		if len(lines) == 0 {
			return
		}
		payload := strings.TrimSpace(strings.Join(lines, "\n"))
		lines = lines[:0]
		if payload == "" || payload == "[DONE]" {
			return
		}
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) == nil && event != nil {
			events = append(events, event)
		}
	}
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 1024*1024), 256*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimSpace(line[5:]))
		}
	}
	flush()
	return events
}

// codexResponseImages is _codex_response_images: every image_generation_call result.
func codexResponseImages(value any) []string {
	switch v := value.(type) {
	case map[string]any:
		if v["type"] == "image_generation_call" {
			if result, ok := v["result"].(string); ok {
				if result = strings.TrimSpace(result); result != "" {
					if strings.HasPrefix(result, "data:image/") {
						if _, after, ok := strings.Cut(result, ","); ok {
							result = after
						}
					}
					return []string{result}
				}
			}
		}
		out := []string{}
		for _, item := range v {
			out = append(out, codexResponseImages(item)...)
		}
		return out
	case []any:
		out := []string{}
		for _, item := range v {
			out = append(out, codexResponseImages(item)...)
		}
		return out
	case []map[string]any:
		out := []string{}
		for _, item := range v {
			out = append(out, codexResponseImages(item)...)
		}
		return out
	}
	return nil
}

// CodexAttempt is stream_codex_image_outputs for one account (imagegen.Generator.Codex).
func CodexAttempt(hooks imagegen.Hooks) func(ctx context.Context, token string, account pool.Account, req imagegen.Request, index, total int, emit func(imagegen.Output)) error {
	return func(ctx context.Context, token string, account pool.Account, req imagegen.Request, index, total int, emit func(imagegen.Output)) error {
		if token == "" {
			return errors.New("access_token is required for codex image endpoints")
		}
		if source, _ := account["source_type"].(string); strings.ToLower(strings.TrimSpace(source)) != "codex" {
			return errors.New("codex responses endpoint requires a codex source account")
		}
		size := req.Size
		if size == "" {
			size = "1024x1024"
		}
		quality := req.Quality
		if quality == "" {
			quality = "auto"
		}
		action := "generate"
		if len(req.Images) > 0 {
			action = "edit"
		}
		_, base := imagegen.SplitImageModel(req.Model)
		toolModel := imagegen.CodexToolModels[base]
		if toolModel == "" {
			toolModel = "gpt-image-2"
		}
		tool := map[string]any{"type": "image_generation", "model": toolModel, "action": action, "size": size, "quality": quality, "output_format": "png"}
		if strings.ToLower(req.OutputFormat) == "gif" {
			if len(req.Images) > 0 {
				return errors.New("GIF output is not supported for image edits")
			}
			tool["output_format"] = "gif"
			tool["frame_count"] = imagegen.GIFFrameCount(req.FrameCount)
			tool["frame_duration_ms"] = imagegen.GIFFrameDurationMS(req.FrameDurationMS)
		}
		payload, _ := json.Marshal(map[string]any{
			"model": codexResponsesModel, "instructions": codexResponsesInstructions, "store": false,
			"input": codexImageInput(req.Prompt, req.Images), "tools": []any{tool}, "tool_choice": map[string]any{"type": "image_generation"}, "stream": true,
		})
		path := "/backend-api/codex/responses"
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.UpstreamBaseURL()+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response, err := codexClient.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			var body any = string(raw)
			var decoded any
			if json.Unmarshal(raw, &decoded) == nil {
				body = decoded
			}
			retryAfter := 0
			if value, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil {
				retryAfter = value
			}
			return &imagegen.UpstreamHTTPError{Context: path, StatusCode: response.StatusCode, Body: body, RetryAfter: retryAfter}
		}
		images := codexResponseImages(codexEvents(response.Header.Get("Content-Type"), raw))
		if len(images) == 0 {
			return imagegen.NewGenerationError("No image result found in response")
		}
		items := make([]map[string]any, 0, len(images))
		for _, image := range images {
			if _, err := base64.StdEncoding.DecodeString(image); err == nil {
				items = append(items, map[string]any{"b64_json": image, "revised_prompt": req.Prompt})
			}
		}
		formatted, err := hooks.FormatImageResult(items, req.Prompt, req.ResponseFormat, req.BaseURL, time.Now().Unix(), "", req.Size)
		if err != nil {
			return err
		}
		data, _ := formatted["data"].([]map[string]any)
		if len(data) == 0 {
			return imagegen.NewGenerationError("No image result found in response")
		}
		out := imagegen.Output{Kind: "result", Model: req.Model, Index: index, Total: total, Created: time.Now().Unix(), Data: data}
		emit(out)
		return nil
	}
}
