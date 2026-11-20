package textapi

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/google/uuid"
)

// openai_v1_response.py.

var responseContentPartTypes = map[string]bool{"text": true, "input_text": true, "output_text": true, "image_url": true, "input_image": true, "image": true}

func hexID(prefix string) string { return prefix + strings.ReplaceAll(uuid.NewString(), "-", "") }

func responseThinkingEffort(body map[string]any) string {
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		return normalizeEffort(reasoning["effort"])
	}
	if value, ok := body["thinking_effort"]; ok {
		return normalizeEffort(value)
	}
	if value, ok := body["reasoning_effort"]; ok {
		return normalizeEffort(value)
	}
	return ""
}

// hasResponseImageGenerationTool is has_response_image_generation_tool.
func hasResponseImageGenerationTool(body map[string]any) bool {
	if tools, ok := body["tools"].([]any); ok {
		for _, tool := range tools {
			if toolType(tool) == "image_generation" {
				return true
			}
		}
	}
	choice, ok := body["tool_choice"].(map[string]any)
	return ok && strings.TrimSpace(pyStr(choice["type"])) == "image_generation"
}

func hasUnsupportedResponseTools(body map[string]any) bool {
	allowed := map[string]bool{"image_generation": true}
	for kind := range webSearchToolTypes {
		allowed[kind] = true
	}
	return hasUnsupportedTools(body, allowed)
}

func responseImageTool(body map[string]any) map[string]any {
	tools, _ := body["tools"].([]any)
	for _, tool := range tools {
		if m, ok := tool.(map[string]any); ok && m["type"] == "image_generation" {
			return m
		}
	}
	return map[string]any{}
}

// extractResponsePrompt is extract_response_prompt.
func extractResponsePrompt(input any) string {
	switch v := input.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		if role := strings.ToLower(strings.TrimSpace(pyStr(v["role"]))); role != "" && role != "user" {
			return ""
		}
		return extractPromptFromMessageContent(v["content"])
	case []any:
		parts := []string{}
		for _, raw := range v {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if strings.TrimSpace(pyStr(item["type"])) == "input_text" {
				if text := strings.TrimSpace(pyStr(item["text"])); text != "" {
					parts = append(parts, text)
				}
				continue
			}
			if role := strings.ToLower(strings.TrimSpace(pyStr(item["role"]))); role != "" && role != "user" {
				continue
			}
			if prompt := extractPromptFromMessageContent(item["content"]); prompt != "" {
				parts = append(parts, prompt)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

// extractResponseImage is extract_response_image.
func (d *Deps) extractResponseImage(input any) (*imageBytes, error) {
	first := func(content any) (*imageBytes, error) {
		images, err := d.Images.ExtractImages(content)
		if err != nil || len(images) == 0 {
			return nil, err
		}
		return &images[0], nil
	}
	switch v := input.(type) {
	case map[string]any:
		if strings.TrimSpace(pyStr(v["type"])) == "input_image" {
			return first([]any{v})
		}
		return first(v["content"])
	case []any:
		for i := len(v) - 1; i >= 0; i-- {
			item, ok := v[i].(map[string]any)
			if !ok {
				continue
			}
			if strings.TrimSpace(pyStr(item["type"])) == "input_image" {
				if image, err := first([]any{item}); err != nil || image != nil {
					return image, err
				}
			}
			if image, err := first(item["content"]); err != nil || image != nil {
				return image, err
			}
		}
	}
	return nil, nil
}

// inputImageParts is _input_image_parts.
func inputImageParts(input any) []any {
	parts := []any{}
	switch v := input.(type) {
	case map[string]any:
		if content, ok := v["content"].([]any); ok {
			for _, item := range content {
				if _, isMap := item.(map[string]any); isMap {
					parts = append(parts, item)
				}
			}
		}
	case []any:
		allTyped := true
		for _, item := range v {
			m, ok := item.(map[string]any)
			allTyped = allTyped && ok && truthy(m["type"])
		}
		if allTyped {
			return v
		}
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if content, ok := m["content"].([]any); ok {
					for _, part := range content {
						if _, isMap := part.(map[string]any); isMap {
							parts = append(parts, part)
						}
					}
				}
			}
		}
	}
	return parts
}

func isResponseContentPart(value any) bool {
	m, ok := value.(map[string]any)
	if !ok {
		return false
	}
	partType := strings.TrimSpace(pyStr(m["type"]))
	_, hasImageURL := m["image_url"]
	return responseContentPartTypes[partType] || (hasImageURL && partType != "message")
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func messageContentFromResponseItem(item map[string]any) any {
	switch content := item["content"].(type) {
	case []any:
		out := make([]any, len(content))
		for i, part := range content {
			if m, ok := part.(map[string]any); ok {
				out[i] = copyMap(m)
			} else {
				out[i] = part
			}
		}
		return out
	case string:
		return content
	}
	if prompt := extractResponsePrompt([]any{item}); prompt != "" {
		return prompt
	}
	if truthy(item["content"]) {
		return item["content"]
	}
	return ""
}

func appendResponseMessage(messages []any, role any, content any) []any {
	roleText := pyStr(role)
	if roleText == "" {
		roleText = "user"
	}
	switch v := content.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			messages = append(messages, map[string]any{"role": roleText, "content": strings.TrimSpace(v)})
		}
	case []any:
		if len(v) > 0 {
			messages = append(messages, map[string]any{"role": roleText, "content": v})
		}
	}
	return messages
}

// messagesFromInput is messages_from_input.
func messagesFromInput(input any, instructions any) []any {
	messages := []any{}
	if system := strings.TrimSpace(pyStr(instructions)); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	switch v := input.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": strings.TrimSpace(v)})
		}
	case map[string]any:
		if isResponseContentPart(v) {
			return appendResponseMessage(messages, "user", []any{copyMap(v)})
		}
		return appendResponseMessage(messages, firstTruthy(v["role"], "user"), messageContentFromResponseItem(v))
	case []any:
		allParts := true
		for _, item := range v {
			allParts = allParts && isResponseContentPart(item)
		}
		if allParts {
			parts := []any{}
			for _, item := range v {
				if m, ok := item.(map[string]any); ok {
					parts = append(parts, copyMap(m))
				}
			}
			return appendResponseMessage(messages, "user", parts)
		}
		pending := []any{}
		for _, item := range v {
			if isResponseContentPart(item) {
				pending = append(pending, copyMap(item.(map[string]any)))
				continue
			}
			if len(pending) > 0 {
				messages = appendResponseMessage(messages, "user", pending)
				pending = []any{}
			}
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			messages = appendResponseMessage(messages, firstTruthy(m["role"], "user"), messageContentFromResponseItem(m))
		}
		if len(pending) > 0 {
			messages = appendResponseMessage(messages, "user", pending)
		}
	}
	return messages
}

func textOutputItem(text, id, status string, annotations []map[string]any) map[string]any {
	if id == "" {
		id = hexID("msg_")
	}
	list := []any{}
	for _, item := range annotations {
		list = append(list, item)
	}
	return map[string]any{"id": id, "type": "message", "status": status, "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": list}}}
}

func webSearchCallItem(query, id, status string, sources []map[string]string) map[string]any {
	action := map[string]any{"type": "search", "query": query, "queries": []any{query}}
	if len(sources) > 0 {
		list := []any{}
		for _, source := range sources {
			if source["url"] != "" {
				list = append(list, map[string]any{"type": "url", "url": source["url"]})
			}
		}
		action["sources"] = list
	}
	return map[string]any{"id": id, "type": "web_search_call", "status": status, "action": action}
}

func imageOutputItems(prompt string, data []map[string]any) []any {
	out := []any{}
	for _, item := range data {
		if b64 := strings.TrimSpace(pyStr(item["b64_json"])); b64 != "" {
			revised := strings.TrimSpace(pyStr(firstTruthy(item["revised_prompt"], prompt)))
			if revised == "" {
				revised = prompt
			}
			out = append(out, map[string]any{"id": "ig_" + strconv.Itoa(len(out)+1), "type": "image_generation_call", "status": "completed",
				"result": b64, "revised_prompt": revised})
		}
	}
	return out
}

func responseBody(id, model string, created int64, status string, output []any) map[string]any {
	return map[string]any{"id": id, "object": "response", "created_at": created, "status": status, "error": nil,
		"incomplete_details": nil, "model": model, "output": output, "parallel_tool_calls": false}
}

func responseCompleted(id, model string, created int64, output []any, usage map[string]any) map[string]any {
	body := responseBody(id, model, created, "completed", output)
	if len(usage) > 0 {
		body["usage"] = usage
	}
	return map[string]any{"type": "response.completed", "response": body}
}

func (d *Deps) textResponseParts(body map[string]any) (string, []map[string]any, error) {
	model := modelOr(body, "auto")
	normalized, err := d.NormalizeMessages(messagesFromInput(body["input"], body["instructions"]), nil)
	if err != nil {
		return "", nil, err
	}
	messages := NormalizeTextMessages(d.Settings, normalized)
	if hasUnsupportedResponseTools(body) {
		messages = append([]map[string]any{{"role": "system", "content": toolUnavailableSystemMessage}}, messages...)
	}
	return model, messages, nil
}

func (d *Deps) streamTextResponse(ctx context.Context, body map[string]any, messages []map[string]any, emit func(map[string]any) error) error {
	model := modelOr(body, "auto")
	token, err := d.textToken(ctx, model, nil) // text_backend(model) runs before the first event
	if err != nil {
		return err
	}
	responseID, itemID, created := hexID("resp_"), hexID("msg_"), time.Now().Unix()
	if err := emit(map[string]any{"type": "response.created", "response": responseBody(responseID, model, created, "in_progress", []any{})}); err != nil {
		return err
	}
	if err := emit(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": textOutputItem("", itemID, "in_progress", nil)}); err != nil {
		return err
	}
	full := ""
	err = d.streamTextDeltas(ctx, token, textRequest{Model: model, Messages: messages, ThinkingEffort: responseThinkingEffort(body)}, func(delta string) error {
		full += delta
		return emit(map[string]any{"type": "response.output_text.delta", "item_id": itemID, "output_index": 0, "content_index": 0, "delta": delta})
	})
	if err != nil {
		return err
	}
	if err := emit(map[string]any{"type": "response.output_text.done", "item_id": itemID, "output_index": 0, "content_index": 0, "text": full}); err != nil {
		return err
	}
	item := textOutputItem(full, itemID, "completed", nil)
	if err := emit(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item}); err != nil {
		return err
	}
	usage := imagegen.TokenUsage(CountMessageTextTokens(messages, model), CountMessageImageTokens(messages), imagegen.CountTextTokens(full, model), 0)
	return emit(responseCompleted(responseID, model, created, []any{item}, usage))
}

func (d *Deps) streamWebSearchResponse(ctx context.Context, body map[string]any, messages []map[string]any, emit func(map[string]any) error) error {
	model := modelOr(body, "auto")
	query := searchQueryFromMessages(messages)
	if query == "" {
		query = extractResponsePrompt(body["input"])
	}
	if query == "" {
		return badRequest("input text is required for web_search")
	}
	responseID, searchID, itemID, created := hexID("resp_"), hexID("ws_"), hexID("msg_"), time.Now().Unix()
	events := []map[string]any{
		{"type": "response.created", "response": responseBody(responseID, model, created, "in_progress", []any{})},
		{"type": "response.output_item.added", "output_index": 0, "item": webSearchCallItem(query, searchID, "in_progress", nil)},
		{"type": "response.web_search_call.in_progress", "output_index": 0, "item_id": searchID},
		{"type": "response.web_search_call.searching", "output_index": 0, "item_id": searchID},
	}
	for _, event := range events {
		if err := emit(event); err != nil {
			return err
		}
	}
	result, err := d.runWebSearch(ctx, query)
	if err != nil {
		return err
	}
	searchItem := webSearchCallItem(query, searchID, "completed", normalizedSources(result))
	text, annotations := textWithURLCitations(result)
	events = []map[string]any{
		{"type": "response.web_search_call.completed", "output_index": 0, "item_id": searchID},
		{"type": "response.output_item.done", "output_index": 0, "item": searchItem},
		{"type": "response.output_item.added", "output_index": 1, "item": textOutputItem("", itemID, "in_progress", annotations)},
	}
	if text != "" {
		events = append(events, map[string]any{"type": "response.output_text.delta", "item_id": itemID, "output_index": 1, "content_index": 0, "delta": text})
	}
	messageItem := textOutputItem(text, itemID, "completed", annotations)
	usage := imagegen.TokenUsage(CountMessageTextTokens(messages, model), CountMessageImageTokens(messages), imagegen.CountTextTokens(text, model), 0)
	events = append(events,
		map[string]any{"type": "response.output_text.done", "item_id": itemID, "output_index": 1, "content_index": 0, "text": text},
		map[string]any{"type": "response.output_item.done", "output_index": 1, "item": messageItem},
		responseCompleted(responseID, model, created, []any{searchItem, messageItem}, usage))
	for _, event := range events {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

func (d *Deps) streamImageResponse(ctx context.Context, body map[string]any, emit func(map[string]any) error) error {
	prompt := extractResponsePrompt(body["input"])
	if prompt == "" {
		return badRequest("input text is required")
	}
	model := modelOr(body, "gpt-image-2")
	image, err := d.extractResponseImage(body["input"])
	if err != nil {
		return err
	}
	args := chatImageArgs{model: model, prompt: prompt, n: 1}
	if image != nil {
		args.images = []imageBytes{*image}
	}
	inputTokens := CountImageContentTokens(inputImageParts(body["input"]))
	tool := responseImageTool(body)
	size := pyStr(tool["size"])
	quality := pyStr(firstTruthy(tool["quality"], "auto"))
	request := imagegen.Request{Size: size, Quality: quality, OutputFormat: pyStr(firstTruthy(tool["output_format"], "png"))}
	if frames, ok := intValue(tool["frame_count"]); ok {
		request.FrameCount = &frames
	}
	if duration, ok := intValue(tool["frame_duration_ms"]); ok {
		request.FrameDurationMS = &duration
	}
	responseID, created := hexID("resp_"), time.Now().Unix()
	if err := emit(map[string]any{"type": "response.created", "response": responseBody(responseID, model, created, "in_progress", []any{})}); err != nil {
		return err
	}
	outputs, err := d.generateImages(ctx, args, request)
	if err != nil {
		return err
	}
	for _, output := range outputs {
		if output.Kind == "message" {
			item := textOutputItem(output.Text, "", "completed", nil)
			usage := imagegen.TokenUsage(imagegen.CountTextTokens(prompt, model), inputTokens, imagegen.CountTextTokens(output.Text, model), 0)
			for _, event := range []map[string]any{
				{"type": "response.output_text.delta", "item_id": item["id"], "output_index": 0, "content_index": 0, "delta": output.Text},
				{"type": "response.output_text.done", "item_id": item["id"], "output_index": 0, "content_index": 0, "text": output.Text},
				{"type": "response.output_item.done", "output_index": 0, "item": item},
				responseCompleted(responseID, model, created, []any{item}, usage),
			} {
				if err := emit(event); err != nil {
					return err
				}
			}
			return nil
		}
		if output.Kind != "result" {
			continue
		}
		items := imageOutputItems(prompt, output.Data)
		if len(items) == 0 {
			continue
		}
		usage := imagegen.ImageUsage(imagegen.CountTextTokens(prompt, model), inputTokens, imagegen.CountImageOutputItemsTokens(output.Data, size, quality))
		for index, item := range items {
			if err := emit(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item}); err != nil {
				return err
			}
		}
		completed := responseCompleted(responseID, model, created, items, usage)
		if output.AccountEmail != "" {
			completed["_account_email"] = output.AccountEmail
		}
		return emit(completed)
	}
	return errors.New("image generation failed")
}

func intValue(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case interface{ Int64() (int64, error) }:
		n, err := v.Int64()
		return int(n), err == nil
	}
	return 0, false
}

func (d *Deps) responseEvents(ctx context.Context, body map[string]any, emit func(map[string]any) error) error {
	if hasResponseImageGenerationTool(body) {
		return d.streamImageResponse(ctx, body, emit)
	}
	_, messages, err := d.textResponseParts(body)
	if err != nil {
		return err
	}
	if hasWebSearchTool(body) && !hasUnsupportedResponseTools(body) {
		return d.streamWebSearchResponse(ctx, body, messages, emit)
	}
	key := CacheKey(body, messages, truthy(body["stream"]))
	return d.Cache.Stream(key, func(emit func(map[string]any) error) error {
		return d.streamTextResponse(ctx, body, messages, emit)
	}, emit)
}

// Responses is openai_v1_response.handle.
func (d *Deps) Responses(ctx context.Context, body map[string]any) (Result, error) {
	if truthy(body["stream"]) {
		return Result{Stream: func(ctx context.Context, emit func(map[string]any) error) error {
			return d.responseEvents(ctx, body, emit)
		}}, nil
	}
	var completed map[string]any
	err := d.responseEvents(ctx, body, func(event map[string]any) error {
		if event["type"] == "response.completed" {
			completed, _ = event["response"].(map[string]any)
			if email, ok := event["_account_email"]; ok && completed != nil {
				completed["_account_email"] = email
			}
		}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if completed == nil {
		return Result{}, errors.New("response generation failed")
	}
	return Result{JSON: completed}, nil
}
