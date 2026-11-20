package textapi

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"regexp"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/google/uuid"
)

// anthropic_v1_messages.py: Anthropic Messages on top of the chat stream, with tools emulated
// through an XML convention in the prompt.

const xmlToolRule = `Tool output adapter: when calling tools, output ONLY this XML and no prose/markdown:
<tool_calls><tool_call><tool_name>TOOL_NAME</tool_name><parameters><PARAM><![CDATA[value]]></PARAM></parameters></tool_call></tool_calls>`

var (
	toolMarkupRE = regexp.MustCompile(`(?is)<tool_calls\b[^>]*>.*?</tool_calls>|<tool_call\b[^>]*>.*?</tool_call>|<function_call\b[^>]*>.*?</function_call>|<invoke\b[^>]*>.*?</invoke>`)
	toolStartRE  = regexp.MustCompile(`(?is)<tool_calls\b|<tool_call\b|<function_call\b|<invoke\b`)
	codeFenceRE  = regexp.MustCompile("(?s)```.*?```")
	toolBlockRE  = regexp.MustCompile(`(?is)<tool_call\b[^>]*>(.*?)</tool_call>|<function_call\b[^>]*>(.*?)</function_call>|<invoke\b[^>]*>(.*?)</invoke>`)
	cdataRE      = regexp.MustCompile(`(?is)^<!\[CDATA\[(.*?)]]>$`)
	xmlElementRE = regexp.MustCompile(`(?is)<([\w.-]+)\b[^>]*>(.*?)</([\w.-]+)>`)
	xmlValueREs  = map[string]*regexp.Regexp{}
)

// pyJSONDumps is json.dumps(value, ensure_ascii=False) with Python's ", " and ": " separators.
func pyJSONDumps(value any) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.Encode(value)
	compact := strings.TrimRight(buf.String(), "\n")
	// re-space outside strings
	var out strings.Builder
	inString, escaped := false, false
	for _, r := range compact {
		out.WriteRune(r)
		if inString {
			if escaped {
				escaped = false
			} else if r == '\\' {
				escaped = true
			} else if r == '"' {
				inString = false
			}
			continue
		}
		switch r {
		case '"':
			inString = true
		case ',', ':':
			out.WriteRune(' ')
		}
	}
	return out.String()
}

func toolMeta(tool map[string]any) (string, string, any) {
	fn, _ := tool["function"].(map[string]any)
	if fn == nil {
		fn = map[string]any{}
	}
	name := strings.TrimSpace(pyStr(firstTruthy(tool["name"], fn["name"])))
	desc := strings.TrimSpace(pyStr(firstTruthy(tool["description"], fn["description"])))
	schema := firstTruthy(tool["input_schema"], tool["parameters"], fn["input_schema"], fn["parameters"])
	if schema == nil {
		schema = map[string]any{}
	}
	return name, desc, schema
}

func buildToolPrompt(tools any) string {
	list, ok := tools.([]any)
	if !ok {
		return ""
	}
	blocks := []string{}
	for _, raw := range list {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, desc, schema := toolMeta(tool); name != "" {
			blocks = append(blocks, "Tool: "+name+"\nDescription: "+desc+"\nParameters: "+pyJSONDumps(schema))
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	return strings.TrimSpace("Available tools:\n" + strings.Join(blocks, "\n") + `

Tool use rules:
- If the user asks to list/read/search files, inspect project state, run a command, or answer from local code, you MUST call a suitable tool first. Do not say you cannot access files.
- To call tools, output ONLY XML and no prose/markdown:
<tool_calls><tool_call><tool_name>TOOL_NAME</tool_name><parameters><PARAM><![CDATA[value]]></PARAM></parameters></tool_call></tool_calls>
- Put parameters under <parameters> using the exact schema names.
`)
}

func hasClaudeCodeSystem(system any) bool {
	switch v := system.(type) {
	case string:
		return strings.Contains(v, "You are Claude Code")
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok && strings.Contains(pyStr(m["text"]), "You are Claude Code") {
				return true
			}
		}
	}
	return false
}

func mergeSystem(system any, extra string) any {
	if hasClaudeCodeSystem(system) {
		extra = xmlToolRule
	}
	if extra == "" {
		return system
	}
	if text, ok := system.(string); ok && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text) + "\n\n" + extra
	}
	if list, ok := system.([]any); ok {
		return append(append([]any{}, list...), map[string]any{"type": "text", "text": extra})
	}
	return extra
}

func preprocessBlock(block any) any {
	m, ok := block.(map[string]any)
	if !ok {
		return block
	}
	switch pyStr(m["type"]) {
	case "text":
		item := copyMap(m)
		item["text"] = pyStr(m["text"])
		return item
	case "tool_use":
		input := m["input"]
		if !truthy(input) {
			input = map[string]any{}
		}
		return map[string]any{"type": "text", "text": "<tool_calls><tool_call><tool_name>" + pyStr(m["name"]) + "</tool_name><parameters>" +
			pyJSONDumps(input) + "</parameters></tool_call></tool_calls>"}
	case "tool_result":
		return map[string]any{"type": "text", "text": "Tool result " + pyStr(m["tool_use_id"]) + ": " + pyContentStr(m["content"])}
	}
	return block
}

// pyContentStr is f"{value or ”}": strings as they are, other values as Python would print them.
func pyContentStr(value any) string {
	if !truthy(value) {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return pyRepr(value)
}

// pyRepr renders lists and dicts like Python's repr (single quotes, True/False/None).
func pyRepr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return pyQuote(v)
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		parts := []string{}
		for _, key := range orderedKeys(v) {
			parts = append(parts, pyQuote(key)+": "+pyRepr(v[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return pyStr(value)
}

func pyQuote(text string) string {
	if strings.Contains(text, "'") && !strings.Contains(text, `"`) {
		return `"` + text + `"`
	}
	return "'" + strings.ReplaceAll(strings.ReplaceAll(text, `\`, `\\`), "'", `\'`) + "'"
}

func preprocessMessages(messages any) any {
	list, ok := messages.([]any)
	if !ok {
		return messages
	}
	out := []any{}
	for _, raw := range list {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		item := copyMap(message)
		switch content := item["content"].(type) {
		case string:
			item["content"] = content
		case []any:
			blocks := make([]any, len(content))
			for i, block := range content {
				blocks[i] = preprocessBlock(block)
			}
			item["content"] = blocks
		}
		out = append(out, item)
	}
	return out
}

func stripToolMarkup(text string) string {
	return strings.TrimSpace(toolMarkupRE.ReplaceAllString(text, ""))
}

func streamableText(text string) string {
	if loc := toolStartRE.FindStringIndex(text); loc != nil {
		return strings.TrimRight(text[:loc[0]], " \t\n\r\f\v")
	}
	return text
}

func xmlValue(text, tag string) string {
	re, ok := xmlValueREs[tag]
	if !ok {
		re = regexp.MustCompile(`(?is)<` + regexp.QuoteMeta(tag) + `\b[^>]*>(.*?)</` + regexp.QuoteMeta(tag) + `>`)
		xmlValueREs[tag] = re
	}
	match := re.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	value := strings.TrimSpace(match[1])
	if cdata := cdataRE.FindStringSubmatch(value); cdata != nil {
		value = cdata[1]
	}
	return strings.TrimSpace(html.UnescapeString(value))
}

func parseToolValue(raw string) any {
	value := xmlValue("<x>"+raw+"</x>", "x")
	var parsed any
	if json.Unmarshal([]byte(value), &parsed) == nil {
		return parsed
	}
	return value
}

func parseToolParams(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		if m, ok := parsed.(map[string]any); ok {
			return m
		}
		return map[string]any{}
	}
	out := map[string]any{}
	for _, match := range xmlElementRE.FindAllStringSubmatch(raw, -1) {
		if match[1] == match[3] { // Go regexp has no backreference; keep only matching pairs
			out[match[1]] = parseToolValue(match[2])
		}
	}
	return out
}

type toolCall struct {
	name string
	args map[string]any
}

func parseToolCalls(text string) []toolCall {
	text = strings.TrimSpace(codeFenceRE.ReplaceAllString(text, ""))
	out := []toolCall{}
	for _, match := range toolBlockRE.FindAllStringSubmatch(text, -1) {
		block := ""
		for _, part := range match[1:] {
			if part != "" {
				block = part
				break
			}
		}
		name := firstNonEmpty(xmlValue(block, "tool_name"), xmlValue(block, "name"), xmlValue(block, "function"))
		params := firstNonEmpty(xmlValue(block, "parameters"), xmlValue(block, "input"), xmlValue(block, "arguments"), "{}")
		if name != "" {
			out = append(out, toolCall{name, parseToolParams(params)})
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func contentBlocks(text string, tools any) ([]map[string]any, string) {
	calls := []toolCall{}
	if list, ok := tools.([]any); ok && len(list) > 0 {
		calls = parseToolCalls(text)
	}
	text = stripToolMarkup(text)
	if len(calls) > 0 {
		content := []map[string]any{}
		if text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, call := range calls {
			content = append(content, map[string]any{"type": "tool_use", "id": "toolu_" + uuid.NewString(), "name": call.name, "input": call.args})
		}
		return content, "tool_use"
	}
	return []map[string]any{{"type": "text", "text": text}}, "end_turn"
}

func messageResponse(model, text string, inputTokens, outputTokens int, tools any) map[string]any {
	content, stopReason := contentBlocks(text, tools)
	list := make([]any, len(content))
	for i, block := range content {
		list[i] = block
	}
	return map[string]any{"id": "msg_" + uuid.NewString(), "type": "message", "role": "assistant", "model": model, "content": list,
		"stop_reason": stopReason, "stop_sequence": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens}}
}

func countMessageTokens(messages []map[string]any, model string) int {
	return CountMessageTextTokens(messages, model) + CountMessageImageTokens(messages)
}

func streamBufferedBlocks(content []map[string]any, start int, emit func(map[string]any) error) error {
	for offset, block := range content {
		index := start + offset
		var startBlock, delta map[string]any
		if block["type"] == "tool_use" {
			input := block["input"]
			if !truthy(input) {
				input = map[string]any{}
			}
			startBlock = map[string]any{"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{}}
			delta = map[string]any{"type": "input_json_delta", "partial_json": pyJSONDumps(input)}
		} else {
			startBlock = map[string]any{"type": "text", "text": ""}
			delta = map[string]any{"type": "text_delta", "text": pyStr(block["text"])}
		}
		for _, event := range []map[string]any{
			{"type": "content_block_start", "index": index, "content_block": startBlock},
			{"type": "content_block_delta", "index": index, "delta": delta},
			{"type": "content_block_stop", "index": index},
		} {
			if err := emit(event); err != nil {
				return err
			}
		}
	}
	return nil
}

// anthropicStream is stream_events over the chat completion chunks.
func anthropicStream(model string, inputTokens int, tools any, chunks func(emit func(map[string]any) error) error, emit func(map[string]any) error) error {
	messageID, created := "msg_"+uuid.NewString(), time.Now().Unix()
	current, streamed := "", ""
	list, _ := tools.([]any)
	toolMode := len(list) > 0
	toolStarted, textOpen, finished := false, false, false
	if err := emit(map[string]any{"type": "message_start", "message": map[string]any{"id": messageID, "type": "message", "role": "assistant", "model": model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0}}}); err != nil {
		return err
	}
	if !toolMode {
		textOpen = true
		if err := emit(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}); err != nil {
			return err
		}
	}
	err := chunks(func(chunk map[string]any) error {
		if finished {
			return nil
		}
		choices, _ := chunk["choices"].([]any)
		choice := map[string]any{}
		if len(choices) > 0 {
			choice, _ = choices[0].(map[string]any)
		}
		delta, _ := choice["delta"].(map[string]any)
		if textDelta := pyStr(delta["content"]); textDelta != "" {
			current += textDelta
			if !toolStarted {
				visible := current
				if toolMode {
					visible = streamableText(current)
				}
				if strings.HasPrefix(visible, streamed) {
					if piece := visible[len(streamed):]; piece != "" {
						if !textOpen {
							textOpen = true
							if err := emit(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}); err != nil {
								return err
							}
						}
						streamed = visible
						if err := emit(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": piece}}); err != nil {
							return err
						}
					}
				}
				toolStarted = toolMode && visible != current
			}
		}
		if !truthy(choice["finish_reason"]) {
			return nil
		}
		finished = true
		content, stopReason := contentBlocks(current, tools)
		if textOpen {
			if err := emit(map[string]any{"type": "content_block_stop", "index": 0}); err != nil {
				return err
			}
		}
		if stopReason == "tool_use" {
			start := 0
			if textOpen {
				start = 1
			}
			if len(content) > 0 && content[0]["type"] == "text" {
				full := pyStr(content[0]["text"])
				remaining := "" // Python slices by code points
				if runes, done := []rune(full), len([]rune(streamed)); len(runes) > done {
					remaining = string(runes[done:])
				}
				if remaining != "" {
					events := []map[string]any{}
					if !textOpen {
						events = append(events, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
					}
					events = append(events, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": remaining}})
					if !textOpen {
						events = append(events, map[string]any{"type": "content_block_stop", "index": 0})
					}
					for _, event := range events {
						if err := emit(event); err != nil {
							return err
						}
					}
				}
				start, content = 1, content[1:]
			}
			if err := streamBufferedBlocks(content, start, emit); err != nil {
				return err
			}
		}
		return emit(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": imagegen.CountTextTokens(current, model)}})
	})
	if err != nil {
		return err
	}
	return emit(map[string]any{"type": "message_stop", "created": created})
}

// Messages is anthropic_v1_messages.handle.
func (d *Deps) Messages(ctx context.Context, body map[string]any) (Result, error) {
	payload := copyMap(body)
	payload["messages"] = preprocessMessages(payload["messages"])
	payload["system"] = mergeSystem(payload["system"], buildToolPrompt(payload["tools"]))
	model := modelOr(payload, "auto")
	token, err := d.textToken(ctx, model, nil)
	if err != nil {
		return Result{}, err
	}
	messages, err := d.NormalizeMessages(payload["messages"], payload["system"])
	if err != nil {
		return Result{}, err
	}
	tools := payload["tools"]
	inputTokens := countMessageTokens(messages, model)
	if truthy(body["stream"]) {
		return Result{Stream: func(ctx context.Context, emit func(map[string]any) error) error {
			return anthropicStream(model, inputTokens, tools, func(chunk func(map[string]any) error) error {
				return d.streamTextChatCompletion(ctx, token, messages, model, "", chunk)
			}, emit)
		}}, nil
	}
	text, err := d.collectChat(ctx, token, messages, model, "")
	if err != nil {
		return Result{}, err
	}
	return Result{JSON: messageResponse(model, text, inputTokens, imagegen.CountTextTokens(text, model), tools)}, nil
}
