package imagegen

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ---- assistant text (shared with the text endpoints) ----

func asMap(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func asString(value any) string {
	text, _ := value.(string)
	return text
}

// AssistantMessageText joins the string parts of a message (or its "text" field).
func AssistantMessageText(message map[string]any) string {
	content := asMap(message["content"])
	if parts, ok := content["parts"].([]any); ok && len(parts) > 0 {
		var out strings.Builder
		for _, part := range parts {
			if text, ok := part.(string); ok {
				out.WriteString(text)
			}
		}
		if out.Len() > 0 {
			return out.String()
		}
	}
	if text, ok := content["text"].(string); ok && text != "" {
		return text
	}
	return ""
}

// IsVisibleAssistantMessage: an assistant message for the user (not hidden, not a tool command,
// not a reasoning channel).
func IsVisibleAssistantMessage(message map[string]any) bool {
	author, ok := message["author"].(map[string]any)
	if !ok || strings.ToLower(strings.TrimSpace(asString(author["role"]))) != "assistant" {
		return false
	}
	if metadata := asMap(message["metadata"]); metadata != nil && metadata["is_visually_hidden_from_conversation"] == true {
		return false
	}
	if recipient := strings.ToLower(strings.TrimSpace(asString(message["recipient"]))); recipient != "" && recipient != "all" {
		return false
	}
	if channel := strings.ToLower(strings.TrimSpace(asString(message["channel"]))); channel != "" && channel != "final" {
		return false
	}
	return true
}

// StripHistory removes repeated history from the start of the text.
func StripHistory(text, history string) string {
	for history != "" && strings.HasPrefix(text, history) {
		text = text[len(history):]
	}
	return text
}

var (
	annotationRE        = regexp.MustCompile("([\\s\\p{Z}]*)([^]*)")
	annotationPlainRE   = regexp.MustCompile("([^]*)")
	annotationTrailerRE = regexp.MustCompile("[^]*$")
)

func isInternalAnnotationPart(part string) bool {
	value := strings.ToLower(strings.TrimSpace(part))
	return value == "" || strings.HasPrefix(value, "turn") || strings.HasPrefix(value, "source")
}

func readableAnnotationPart(parts []string) string {
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" && !isInternalAnnotationPart(value) {
			return value
		}
	}
	return ""
}

func annotationText(payload string) string {
	parts := strings.Split(payload, "")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	kind := strings.ToLower(parts[0])
	data := parts[1:]
	if kind == "url" {
		label, link := "", ""
		if len(data) > 0 {
			label = data[0]
		}
		if len(data) > 1 {
			link = data[1]
		}
		if label != "" && (strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://")) {
			return label + " (" + link + ")"
		}
		if label != "" {
			return label
		}
		return link
	}
	return readableAnnotationPart(data)
}

// SanitizeOutputText turns ChatGPT's private-use annotation markers into readable text and drops
// internal citation pointers.
func SanitizeOutputText(text string) string {
	// markers right before punctuation keep their leading space only when something is left
	var out strings.Builder
	last := 0
	for _, match := range annotationRE.FindAllStringSubmatchIndex(text, -1) {
		end := match[1]
		next, _ := utf8.DecodeRuneInString(text[end:])
		if end >= len(text) || !strings.ContainsRune(".,;:!?", next) {
			continue
		}
		out.WriteString(text[last:match[0]])
		if replacement := annotationText(text[match[4]:match[5]]); replacement != "" {
			out.WriteString(text[match[2]:match[3]] + replacement)
		}
		last = end
	}
	out.WriteString(text[last:])
	text = out.String()
	text = annotationPlainRE.ReplaceAllStringFunc(text, func(match string) string {
		return annotationText(annotationPlainRE.FindStringSubmatch(match)[1])
	})
	return annotationTrailerRE.ReplaceAllString(text, "")
}

func applyPatchOp(op map[string]any, current, history string) string {
	value := ""
	if v := op["v"]; v != nil {
		if text, ok := v.(string); ok {
			value = text
		} else {
			raw, _ := json.Marshal(v)
			value = string(raw)
		}
	}
	switch op["o"] {
	case "append":
		return current + value
	case "replace":
		return StripHistory(value, history)
	}
	return current
}

func applyTextPatch(event map[string]any, current, history string) string {
	if event["p"] == "/message/content/parts/0" {
		return applyPatchOp(event, current, history)
	}
	operations := event["v"]
	if text, ok := operations.(string); ok && current != "" && !truthy(event["p"]) && !truthy(event["o"]) {
		return current + text
	}
	list, ok := operations.([]any)
	if !ok {
		return current
	}
	text := current
	for _, item := range list {
		if op, ok := item.(map[string]any); ok {
			text = applyTextPatch(op, text, history)
		}
	}
	return text
}

func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case bool:
		return v
	}
	return true
}

// AssistantRawText is the assistant text after this event (full message or patches).
func AssistantRawText(event map[string]any, current, history string) string {
	for _, candidate := range []any{event, event["v"]} {
		m, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		message, ok := m["message"].(map[string]any)
		if !ok || !IsVisibleAssistantMessage(message) {
			continue
		}
		if text := AssistantMessageText(message); text != "" {
			return StripHistory(text, history)
		}
	}
	return applyTextPatch(event, current, history)
}

// EventAssistantText is the full visible assistant message of the event, if any.
func EventAssistantText(event map[string]any, history string) string {
	for _, candidate := range []any{event, event["v"]} {
		m, ok := candidate.(map[string]any)
		if !ok {
			continue
		}
		if message, ok := m["message"].(map[string]any); ok && IsVisibleAssistantMessage(message) {
			return StripHistory(AssistantMessageText(message), history)
		}
	}
	return ""
}

// ---- conversation ids and state ----

var (
	fileServiceIDRE = regexp.MustCompile(`file-service://([A-Za-z0-9_-]+)`)
	realImageFileRE = regexp.MustCompile(`\bfile_00000000[a-f0-9]{24}\b`)
	sedimentIDRE    = regexp.MustCompile(`sediment://([A-Za-z0-9_-]+)`)
	conversationRE  = regexp.MustCompile(`"conversation_id"\s*:\s*"([^"]+)"`)
)

func addUnique(values []string, candidates ...string) []string {
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		found := false
		for _, value := range values {
			if value == candidate {
				found = true
				break
			}
		}
		if !found {
			values = append(values, candidate)
		}
	}
	return values
}

func submatches(re *regexp.Regexp, text string) []string {
	out := []string{}
	for _, match := range re.FindAllStringSubmatch(text, -1) {
		out = append(out, match[1])
	}
	return out
}

// ExtractConversationIDs finds the conversation id, generated file ids and sediment ids in a payload.
func ExtractConversationIDs(payload string) (string, []string, []string) {
	conversationID := ""
	if match := conversationRE.FindStringSubmatch(payload); match != nil {
		conversationID = match[1]
	}
	fileIDs := addUnique(nil, submatches(fileServiceIDRE, payload)...)
	fileIDs = addUnique(fileIDs, realImageFileRE.FindAllString(payload, -1)...)
	return conversationID, fileIDs, submatches(sedimentIDRE, payload)
}

func eventMessage(event map[string]any) map[string]any {
	if message := asMap(event["message"]); message != nil {
		return message
	}
	return asMap(asMap(event["v"])["message"])
}

// IsImageToolEvent: a tool message of the image generator.
func IsImageToolEvent(event map[string]any) bool {
	message := eventMessage(event)
	if message == nil || asString(asMap(message["author"])["role"]) != "tool" {
		return false
	}
	if asMap(message["metadata"])["async_task_type"] == "image_gen" {
		return true
	}
	content := asMap(message["content"])
	if content["content_type"] != "multimodal_text" {
		return false
	}
	parts, _ := content["parts"].([]any)
	for _, part := range parts {
		item := asMap(part)
		if item == nil {
			continue
		}
		pointer := asString(item["asset_pointer"])
		if item["content_type"] == "image_asset_pointer" || strings.HasPrefix(pointer, "file-service://") || strings.HasPrefix(pointer, "sediment://") {
			return true
		}
	}
	return false
}

func isUserMessageEvent(event map[string]any) bool {
	message := eventMessage(event)
	return message != nil && strings.ToLower(strings.TrimSpace(asString(asMap(message["author"])["role"]))) == "user"
}

// State is ConversationState.
type State struct {
	Text           string
	RawText        string
	ConversationID string
	FileIDs        []string
	SedimentIDs    []string
	Blocked        bool
	ToolInvoked    *bool
	TurnUseCase    string
}

// Update is update_conversation_state: take ids only from image context, never from the user's
// own uploaded images.
func (s *State) Update(payload string, event map[string]any) {
	conversationID, fileIDs, sedimentIDs := ExtractConversationIDs(payload)
	if conversationID != "" && s.ConversationID == "" {
		s.ConversationID = conversationID
	}
	isPatch := event != nil && event["o"] == "patch"
	isUser := event != nil && isUserMessageEvent(event)
	imageContext := (event != nil && IsImageToolEvent(event)) ||
		(s.ToolInvoked != nil && *s.ToolInvoked && !isUser) ||
		(isPatch && !isUser && (strings.Contains(payload, "asset_pointer") || strings.Contains(payload, "file-service://")))
	if imageContext {
		s.FileIDs = addUnique(s.FileIDs, fileIDs...)
		s.SedimentIDs = addUnique(s.SedimentIDs, sedimentIDs...)
	}
	if event == nil {
		return
	}
	if id := asString(event["conversation_id"]); id != "" {
		s.ConversationID = id
	}
	if value := asMap(event["v"]); value != nil {
		if id := asString(value["conversation_id"]); id != "" {
			s.ConversationID = id
		}
	}
	if event["type"] == "moderation" {
		if moderation := asMap(event["moderation_response"]); moderation != nil && moderation["blocked"] == true {
			s.Blocked = true
		}
	}
	if event["type"] == "server_ste_metadata" {
		if metadata := asMap(event["metadata"]); metadata != nil {
			if invoked, ok := metadata["tool_invoked"].(bool); ok {
				s.ToolInvoked = &invoked
			}
			if useCase := asString(metadata["turn_use_case"]); useCase != "" {
				s.TurnUseCase = useCase
			}
		}
	}
}

// Event is one item of iter_conversation_payloads.
type Event struct {
	Type  string // conversation.delta | conversation.event | conversation.raw | conversation.done
	State State
	Raw   any
	Delta string
}

// IterConversationPayloads turns SSE data payloads into events with the running state.
func IterConversationPayloads(payloads <-chan string, history string, historyMessages []string, emit func(Event)) {
	state := State{}
	snapshot := func(kind string, raw any, delta string) Event {
		copied := state
		copied.FileIDs = append([]string(nil), state.FileIDs...)
		copied.SedimentIDs = append([]string(nil), state.SedimentIDs...)
		return Event{Type: kind, State: copied, Raw: raw, Delta: delta}
	}
	historyIndex := 0
	for payload := range payloads {
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			emit(snapshot("conversation.done", nil, ""))
			for range payloads { // drain so the producer can finish
			}
			return
		}
		var parsed any
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			state.Update(payload, nil)
			emit(snapshot("conversation.raw", payload, ""))
			continue
		}
		event, ok := parsed.(map[string]any)
		if !ok {
			emit(snapshot("conversation.event", parsed, ""))
			continue
		}
		state.Update(payload, event)
		if historyIndex < len(historyMessages) && EventAssistantText(event, history) == historyMessages[historyIndex] {
			historyIndex++
			state.RawText, state.Text = "", ""
			continue
		}
		raw := AssistantRawText(event, state.RawText, history)
		text := SanitizeOutputText(raw)
		state.RawText = raw
		if text != state.Text {
			delta := text
			if strings.HasPrefix(text, state.Text) {
				delta = text[len(state.Text):]
			}
			state.Text = text
			emit(snapshot("conversation.delta", event, delta))
			continue
		}
		emit(snapshot("conversation.event", event, ""))
	}
}

// BuildImagePrompt is build_image_prompt: size/quality (and GIF) hints after the prompt.
func BuildImagePrompt(prompt, size, quality, outputFormat string, frameCount, frameDurationMS *int) string {
	hints := []string{}
	if size != "" {
		hints = append(hints, "输出图片尺寸为 "+size+"。")
	}
	if quality != "" {
		hints = append(hints, "输出图片质量为 "+quality+"。")
	}
	if strings.ToLower(outputFormat) == "gif" {
		hints = append(hints, "输出动画 GIF，"+itoa(GIFFrameCount(frameCount))+" 帧，每帧 "+itoa(GIFFrameDurationMS(frameDurationMS))+" 毫秒。")
	}
	if len(hints) == 0 {
		return prompt
	}
	return strings.TrimSpace(prompt) + "\n\n" + strings.Join(hints, "")
}

// GIFFrameCount is gif_frame_count: 2..64, default 9.
func GIFFrameCount(value *int) int {
	n := 9
	if value != nil {
		n = *value
	}
	return min(64, max(2, n))
}

// GIFFrameDurationMS is gif_frame_duration_ms: 20..2000 in steps of 10, default 200.
func GIFFrameDurationMS(value *int) int {
	n := 200
	if value != nil {
		n = *value
	}
	d := min(2000, max(20, n))
	return d - d%10
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}
