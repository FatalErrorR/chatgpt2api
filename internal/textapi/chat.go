package textapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/google/uuid"
)

// Result is what a handler returns: a JSON body, or a stream of chunks.
type Result struct {
	JSON   map[string]any
	Stream func(ctx context.Context, emit func(map[string]any) error) error
}

const toolUnavailableSystemMessage = "This compatibility backend cannot execute local tools, shell commands, non-search tools, " +
	"or file operations. Do not claim to have run tools or inspected external resources. " +
	"If a user asks you to use a tool, say that tool execution is unavailable through this backend."

func normalizeEffort(value any) string {
	switch normalized := strings.ToLower(strings.TrimSpace(pyStr(value))); normalized {
	case "low", "medium", "high":
		return normalized
	case "xhigh", "extended":
		return "extended"
	}
	return ""
}

// chatThinkingEffort is openai_v1_chat_complete.thinking_effort_from_body.
func chatThinkingEffort(body map[string]any) string {
	if value, ok := body["thinking_effort"]; ok {
		return normalizeEffort(value)
	}
	if value, ok := body["reasoning_effort"]; ok {
		return normalizeEffort(value)
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		return normalizeEffort(reasoning["effort"])
	}
	return ""
}

func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return fmt.Sprint(value) != "0"
}

func completionChunk(model string, delta map[string]any, finish any, id string, created int64) map[string]any {
	return map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
}

func newCompletionID() string { return "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", "") }

// completionResponse is completion_response.
func completionResponse(model, content string, created int64, messages []map[string]any, annotations []any) map[string]any {
	promptText, promptImage, completion := 0, 0, 0
	if len(messages) > 0 {
		promptText = CountMessageTextTokens(messages, model)
		promptImage = CountMessageImageTokens(messages)
		completion = imagegen.CountTextTokens(content, model)
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	message := map[string]any{"role": "assistant", "content": content}
	if len(annotations) > 0 {
		message["annotations"] = annotations
	}
	prompt := promptText + promptImage
	return map[string]any{
		"id": newCompletionID(), "object": "chat.completion", "created": created, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}},
		"usage": map[string]any{
			"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion,
			"prompt_tokens_details":     map[string]any{"text_tokens": promptText, "image_tokens": promptImage, "cached_tokens": 0},
			"completion_tokens_details": map[string]any{"text_tokens": completion, "image_tokens": 0, "reasoning_tokens": 0},
		},
	}
}

// streamTextChatCompletion is stream_text_chat_completion.
func (d *Deps) streamTextChatCompletion(ctx context.Context, token string, messages []map[string]any, model, effort string, emit func(map[string]any) error) error {
	id, created := newCompletionID(), time.Now().Unix()
	sentRole := false
	err := d.streamTextDeltas(ctx, token, textRequest{Model: model, Messages: messages, ThinkingEffort: effort}, func(delta string) error {
		if !sentRole {
			sentRole = true
			return emit(completionChunk(model, map[string]any{"role": "assistant", "content": delta}, nil, id, created))
		}
		return emit(completionChunk(model, map[string]any{"content": delta}, nil, id, created))
	})
	if err != nil {
		return err
	}
	if !sentRole {
		if err := emit(completionChunk(model, map[string]any{"role": "assistant", "content": ""}, nil, id, created)); err != nil {
			return err
		}
	}
	return emit(completionChunk(model, map[string]any{}, "stop", id, created))
}

// collectChatContent is collect_chat_content over the stream above.
func (d *Deps) collectChat(ctx context.Context, token string, messages []map[string]any, model, effort string) (string, error) {
	var builder strings.Builder
	err := d.streamTextChatCompletion(ctx, token, messages, model, effort, func(chunk map[string]any) error {
		choices, _ := chunk["choices"].([]any)
		if len(choices) > 0 {
			delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
			builder.WriteString(pyStr(delta["content"]))
		}
		return nil
	})
	return builder.String(), err
}

// chatMessagesFromBody is chat_messages_from_body.
func chatMessagesFromBody(body map[string]any) ([]any, error) {
	if messages, ok := body["messages"].([]any); ok && len(messages) > 0 {
		out := []any{}
		for _, message := range messages {
			if _, ok := message.(map[string]any); ok {
				out = append(out, message)
			}
		}
		return out, nil
	}
	if prompt := strings.TrimSpace(pyStr(body["prompt"])); prompt != "" {
		return []any{map[string]any{"role": "user", "content": prompt}}, nil
	}
	return nil, badRequest("messages or prompt is required")
}

func modelOr(body map[string]any, fallback string) string {
	if model := strings.TrimSpace(pyStr(body["model"])); model != "" {
		return model
	}
	return fallback
}

// textChatParts is text_chat_parts.
func (d *Deps) textChatParts(body map[string]any) (string, []map[string]any, error) {
	model := modelOr(body, "auto")
	raw, err := chatMessagesFromBody(body)
	if err != nil {
		return "", nil, err
	}
	normalized, err := d.NormalizeMessages(raw, nil)
	if err != nil {
		return "", nil, err
	}
	messages := NormalizeTextMessages(d.Settings, normalized)
	if hasUnsupportedTools(body, webSearchToolTypes) {
		messages = append([]map[string]any{{"role": "system", "content": toolUnavailableSystemMessage}}, messages...)
	}
	return model, messages, nil
}

// IsImageChatRequest is is_image_chat_request.
func IsImageChatRequest(body map[string]any) bool {
	if imagegen.IsSupportedImageModel(strings.TrimSpace(pyStr(body["model"]))) {
		return true
	}
	modalities, ok := body["modalities"].([]any)
	if !ok {
		return false
	}
	for _, item := range modalities {
		if strings.ToLower(strings.TrimSpace(pyStr(item))) == "image" {
			return true
		}
	}
	return false
}

// extractPromptFromMessageContent is extract_prompt_from_message_content.
func extractPromptFromMessageContent(content any) string {
	if text, ok := content.(string); ok {
		return strings.TrimSpace(text)
	}
	list, ok := content.([]any)
	if !ok {
		return ""
	}
	parts := []string{}
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch strings.TrimSpace(pyStr(item["type"])) {
		case "text":
			if text := strings.TrimSpace(pyStr(item["text"])); text != "" {
				parts = append(parts, text)
			}
		case "input_text":
			if text := strings.TrimSpace(pyStr(firstTruthy(item["text"], item["input_text"]))); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// extractChatPrompt is extract_chat_prompt.
func extractChatPrompt(body map[string]any) string {
	if prompt := strings.TrimSpace(pyStr(body["prompt"])); prompt != "" {
		return prompt
	}
	messages, _ := body["messages"].([]any)
	parts := []string{}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || strings.ToLower(strings.TrimSpace(pyStr(message["role"]))) != "user" {
			continue
		}
		if prompt := extractPromptFromMessageContent(message["content"]); prompt != "" {
			parts = append(parts, prompt)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// extractChatImages is extract_chat_image: the images of the last user message that has any.
func (d *Deps) extractChatImages(body map[string]any) ([]imageBytes, error) {
	messages, _ := body["messages"].([]any)
	for i := len(messages) - 1; i >= 0; i-- {
		message, ok := messages[i].(map[string]any)
		if !ok || strings.ToLower(strings.TrimSpace(pyStr(message["role"]))) != "user" {
			continue
		}
		images, err := d.Images.ExtractImages(message["content"])
		if err != nil {
			return nil, err
		}
		if len(images) > 0 {
			return images, nil
		}
	}
	return nil, nil
}

// parseImageCount is parse_image_count: int(n or 1), then 1..4.
func parseImageCount(raw any) (int, error) {
	if !truthy(raw) {
		return 1, nil
	}
	value := 0
	switch v := raw.(type) {
	case bool:
		value = 1
	case json.Number:
		if n, err := v.Int64(); err == nil {
			value = int(n)
		} else if f, err := v.Float64(); err == nil {
			value = int(f)
		} else {
			return 0, badRequest("n must be an integer")
		}
	case float64:
		value = int(v)
	case int:
		value = v
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, badRequest("n must be an integer")
		}
		value = n
	default:
		return 0, badRequest("n must be an integer")
	}
	if value < 1 || value > 4 {
		return 0, badRequest("n must be between 1 and 4")
	}
	return value, nil
}

// chatImageMarkdown is build_chat_image_markdown_content.
func chatImageMarkdown(data []map[string]any) string {
	images := []string{}
	for i, item := range data {
		if b64 := strings.TrimSpace(pyStr(item["b64_json"])); b64 != "" {
			images = append(images, fmt.Sprintf("![image_%d](data:image/png;base64,%s)", i+1, b64))
		}
	}
	if len(images) == 0 {
		return "Image generation completed."
	}
	return strings.Join(images, "\n\n")
}

type chatImageArgs struct {
	model, prompt string
	n             int
	images        []imageBytes
}

func (d *Deps) chatImageArgs(body map[string]any) (chatImageArgs, error) {
	model := modelOr(body, "gpt-image-2")
	prompt := extractChatPrompt(body)
	if prompt == "" {
		return chatImageArgs{}, badRequest("prompt is required")
	}
	images, err := d.extractChatImages(body)
	if err != nil {
		return chatImageArgs{}, err
	}
	n, err := parseImageCount(body["n"])
	if err != nil {
		return chatImageArgs{}, err
	}
	return chatImageArgs{model, prompt, n, images}, nil
}

func encodeImages(images []imageBytes) []string {
	out := []string{}
	for _, image := range images {
		if len(image.data) > 0 {
			out = append(out, base64.StdEncoding.EncodeToString(image.data))
		}
	}
	return out
}

func (d *Deps) generateImages(ctx context.Context, args chatImageArgs, extra imagegen.Request) ([]imagegen.Output, error) {
	request := extra
	request.Prompt, request.Model, request.N, request.ResponseFormat = args.prompt, args.model, max(args.n, 1), "b64_json"
	if encoded := encodeImages(args.images); len(encoded) > 0 {
		request.Images = encoded
	}
	return d.Generator.StreamWithPool(ctx, request)
}

func imageInputTokens(images []imageBytes) int {
	raw := [][]byte{}
	for _, image := range images {
		raw = append(raw, image.data)
	}
	return imagegen.CountImageInputsTokens(raw)
}

// imageChatResponse is image_chat_response.
func (d *Deps) imageChatResponse(ctx context.Context, body map[string]any) (map[string]any, error) {
	args, err := d.chatImageArgs(body)
	if err != nil {
		return nil, err
	}
	outputs, err := d.generateImages(ctx, args, imagegen.Request{})
	if err != nil {
		return nil, err
	}
	result := imagegen.CollectImageOutputs(outputs)
	data, _ := result["data"].([]map[string]any)
	content := chatImageMarkdown(data)
	if len(data) == 0 {
		content = pyStr(result["message"])
		if content == "" {
			content = "Image generation completed."
		}
	}
	created, _ := result["created"].(int64)
	response := completionResponse(args.model, content, created, nil, nil)
	usage := imagegen.ImageUsage(imagegen.CountTextTokens(args.prompt, args.model), imageInputTokens(args.images),
		imagegen.CountImageOutputItemsTokens(data, "", "auto"))
	response["usage"] = imagegen.ChatUsageFromImageUsage(usage)
	return response, nil
}

// imageChatEvents is image_chat_events / stream_image_chat_completion.
func (d *Deps) imageChatEvents(ctx context.Context, body map[string]any, emit func(map[string]any) error) error {
	args, err := d.chatImageArgs(body)
	if err != nil {
		return err
	}
	outputs, err := d.generateImages(ctx, args, imagegen.Request{})
	if err != nil {
		return err
	}
	id, created := newCompletionID(), time.Now().Unix()
	sentRole, sentText := false, ""
	for _, output := range outputs {
		content := ""
		switch output.Kind {
		case "progress":
			content = output.Text
			sentText += content
		case "result":
			content = chatImageMarkdown(output.Data)
		case "message":
			content = output.Text
			if strings.HasPrefix(output.Text, sentText) {
				content = output.Text[len(sentText):]
			}
		}
		if content == "" {
			continue
		}
		delta := map[string]any{"content": content}
		if !sentRole {
			sentRole = true
			delta = map[string]any{"role": "assistant", "content": content}
		}
		chunk := completionChunk(args.model, delta, nil, id, created)
		if output.AccountEmail != "" {
			chunk["_account_email"] = output.AccountEmail
		}
		if err := emit(chunk); err != nil {
			return err
		}
	}
	if !sentRole {
		if err := emit(completionChunk(args.model, map[string]any{"role": "assistant", "content": ""}, nil, id, created)); err != nil {
			return err
		}
	}
	return emit(completionChunk(args.model, map[string]any{}, "stop", id, created))
}

func chatCompletionAnnotations(annotations []map[string]any) []any {
	out := []any{}
	for _, item := range annotations {
		if item["type"] != "url_citation" {
			continue
		}
		out = append(out, map[string]any{"type": "url_citation", "url_citation": map[string]any{
			"start_index": item["start_index"], "end_index": item["end_index"], "url": item["url"], "title": item["title"],
		}})
	}
	return out
}

// ChatCompletions is openai_v1_chat_complete.handle.
func (d *Deps) ChatCompletions(ctx context.Context, body map[string]any) (Result, error) {
	if truthy(body["stream"]) {
		if IsImageChatRequest(body) {
			return Result{Stream: func(ctx context.Context, emit func(map[string]any) error) error {
				return d.imageChatEvents(ctx, body, emit)
			}}, nil
		}
		model, messages, err := d.textChatParts(body)
		if err != nil {
			return Result{}, err
		}
		if isWebSearchChatRequest(body) && !hasUnsupportedTools(body, webSearchToolTypes) {
			return Result{Stream: func(ctx context.Context, emit func(map[string]any) error) error {
				query := searchQueryFromMessages(messages)
				if query == "" {
					return badRequest("messages or prompt is required for web search")
				}
				result, err := d.runWebSearch(ctx, query)
				if err != nil {
					return err
				}
				text, _ := textWithURLCitations(result)
				id, created := newCompletionID(), time.Now().Unix()
				if err := emit(completionChunk(model, map[string]any{"role": "assistant", "content": text}, nil, id, created)); err != nil {
					return err
				}
				return emit(completionChunk(model, map[string]any{}, "stop", id, created))
			}}, nil
		}
		effort := chatThinkingEffort(body)
		key := CacheKey(body, messages, true)
		return Result{Stream: func(ctx context.Context, emit func(map[string]any) error) error {
			return d.Cache.Stream(key, func(emit func(map[string]any) error) error {
				token, err := d.textToken(ctx, model, nil)
				if err != nil {
					return err
				}
				return d.streamTextChatCompletion(ctx, token, messages, model, effort, emit)
			}, emit)
		}}, nil
	}
	if IsImageChatRequest(body) {
		response, err := d.imageChatResponse(ctx, body)
		return Result{JSON: response}, err
	}
	model, messages, err := d.textChatParts(body)
	if err != nil {
		return Result{}, err
	}
	if isWebSearchChatRequest(body) && !hasUnsupportedTools(body, webSearchToolTypes) {
		query := searchQueryFromMessages(messages)
		if query == "" {
			return Result{}, badRequest("messages or prompt is required for web search")
		}
		result, err := d.runWebSearch(ctx, query)
		if err != nil {
			return Result{}, err
		}
		text, annotations := textWithURLCitations(result)
		return Result{JSON: completionResponse(model, text, 0, messages, chatCompletionAnnotations(annotations))}, nil
	}
	effort := chatThinkingEffort(body)
	response, err := d.Cache.Response(CacheKey(body, messages, false), func() (map[string]any, error) {
		token, err := d.textToken(ctx, model, nil)
		if err != nil {
			return nil, err
		}
		text, err := d.collectText(ctx, token, textRequest{Model: model, Messages: messages, ThinkingEffort: effort})
		if err != nil {
			return nil, err
		}
		return completionResponse(model, text, 0, messages, nil), nil
	})
	return Result{JSON: response}, err
}
