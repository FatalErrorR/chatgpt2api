// Package textapi is the text side of services/protocol: /v1/chat/completions,
// /v1/responses, /v1/messages and /v1/search on top of the ChatGPT web conversation, with the
// response cache, web search tool, model catalog and token counting.
package textapi

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
)

// HTTPError is FastAPI's HTTPException(status, detail={"error": message}).
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return e.Message }

func badRequest(format string, args ...any) *HTTPError {
	return &HTTPError{Status: 400, Message: fmt.Sprintf(format, args...)}
}

const (
	maxJSONImageBytes  = 10 * 1024 * 1024
	remoteImageTimeout = 20 * time.Second
)

var (
	dataURLImageRE        = regexp.MustCompile(`(?s)^data:([-+./\w]+);base64,(.*)$`)
	supportedJSONImageMIM = map[string]bool{"image/png": true, "image/jpeg": true, "image/jpg": true, "image/webp": true, "image/gif": true}
)

func str(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// pyStr is str(value or "") for the scalar types the request bodies use.
func pyStr(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		if v {
			return "True"
		}
		return ""
	}
	return fmt.Sprint(value)
}

func asList(value any) ([]any, bool) {
	list, ok := value.([]any)
	return list, ok
}

func asMap(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	return m, ok
}

// MessageText is conversation.message_text.
func MessageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		parts := []string{}
		for _, item := range v {
			switch p := item.(type) {
			case string:
				parts = append(parts, p)
			case map[string]any:
				if t := str(p["type"]); t == "text" || t == "input_text" || t == "output_text" {
					parts = append(parts, pyStr(p["text"]))
				}
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

type imageBytes struct {
	data []byte
	mime string
}

// decodeJSONImageString is _decode_json_image_string.
func decodeJSONImageString(value, mimeType string) (imageBytes, error) {
	text := strings.TrimSpace(value)
	if text == "" {
		return imageBytes{}, badRequest("image file is empty")
	}
	resolved, encoded := strings.ToLower(mimeType), text
	if match := dataURLImageRE.FindStringSubmatch(text); match != nil {
		resolved, encoded = strings.ToLower(match[1]), match[2]
		if resolved == "" {
			resolved = "image/png"
		}
	} else {
		if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
			return imageBytes{}, badRequest("remote image URLs are not supported")
		}
		if resolved == "" {
			resolved = "image/png"
		}
	}
	if resolved == "image/jpg" {
		resolved = "image/jpeg"
	}
	if !supportedJSONImageMIM[resolved] {
		return imageBytes{}, badRequest("unsupported image mime type")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return imageBytes{}, badRequest("invalid base64 image data")
	}
	if len(data) == 0 {
		return imageBytes{}, badRequest("image file is empty")
	}
	if len(data) > maxJSONImageBytes {
		return imageBytes{}, badRequest("image file is too large")
	}
	return imageBytes{data, resolved}, nil
}

// Images fetches message images (data URLs inline, http(s) through the global proxy).
type Images struct {
	Proxy func() string
}

func messageImageURL(value any) string {
	if m, ok := value.(map[string]any); ok {
		return strings.TrimSpace(pyStr(firstTruthy(m["url"], m["image_url"])))
	}
	return strings.TrimSpace(pyStr(value))
}

func firstTruthy(values ...any) any {
	for _, value := range values {
		if value != nil && value != "" && value != false {
			return value
		}
	}
	return nil
}

// decodeMessageImageURL is _decode_message_image_url; nil when the value is not a usable URL.
func (im Images) decodeMessageImageURL(value any) (*imageBytes, error) {
	source := messageImageURL(value)
	if strings.HasPrefix(source, "data:") {
		header, data, _ := strings.Cut(source, ",")
		mimeType := strings.TrimPrefix(strings.SplitN(header, ";", 2)[0], "data:")
		if mimeType == "" {
			mimeType = "image/png"
		}
		decoded, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return nil, fmt.Errorf("Incorrect padding")
		}
		return &imageBytes{decoded, mimeType}, nil
	}
	if !strings.HasPrefix(source, "http://") && !strings.HasPrefix(source, "https://") {
		return nil, nil
	}
	parsed, err := url.Parse(source)
	if err != nil || parsed.Host == "" {
		return nil, nil
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if im.Proxy != nil {
		if proxy := im.Proxy(); proxy != "" {
			if proxyURL, err := url.Parse(proxy); err == nil {
				transport.Proxy = http.ProxyURL(proxyURL)
			}
		}
	}
	request, _ := http.NewRequest(http.MethodGet, source, nil)
	request.Header.Set("Accept", "image/*,*/*;q=0.8")
	request.Header.Set("User-Agent", "chatgpt2api vision fetcher")
	response, err := (&http.Client{Timeout: remoteImageTimeout, Transport: transport}).Do(request)
	if err != nil {
		return nil, badRequest("image_url fetch failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, badRequest("image_url fetch failed: HTTP %d", response.StatusCode)
	}
	if length, err := strconv.Atoi(strings.TrimSpace(response.Header.Get("Content-Length"))); err == nil && length > maxJSONImageBytes {
		return nil, badRequest("image_url exceeds 10MB limit")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxJSONImageBytes+1))
	if err != nil {
		return nil, badRequest("image_url fetch failed: %v", err)
	}
	if len(data) == 0 {
		return nil, badRequest("image_url returned empty content")
	}
	if len(data) > maxJSONImageBytes {
		return nil, badRequest("image_url exceeds 10MB limit")
	}
	mimeType := strings.ToLower(strings.SplitN(response.Header.Get("Content-Type"), ";", 2)[0])
	if mimeType == "" {
		mimeType = "image/png"
	}
	guessed := mime.TypeByExtension(path.Ext(parsed.Path))
	if mimeType != "" && !strings.HasPrefix(mimeType, "image/") && mimeType != "application/octet-stream" && mimeType != "binary/octet-stream" {
		return nil, badRequest("image_url must point to an image")
	}
	if !strings.HasPrefix(mimeType, "image/") && strings.HasPrefix(guessed, "image/") {
		mimeType = guessed
	}
	if !strings.HasPrefix(mimeType, "image/") {
		mimeType = "image/png"
	}
	return &imageBytes{data, mimeType}, nil
}

// decodeMessageImageObject is _decode_message_image_object.
func (im Images) decodeMessageImageObject(item map[string]any) (*imageBytes, error) {
	if data, ok := item["data"].([]byte); ok {
		mimeType := pyStr(firstTruthy(item["mime"], item["mime_type"]))
		if mimeType == "" {
			mimeType = "image/png"
		}
		return &imageBytes{data, mimeType}, nil
	}
	for _, key := range []string{"image_url", "url"} {
		image, err := im.decodeMessageImageURL(item[key])
		if err != nil || image != nil {
			return image, err
		}
	}
	if value, ok := firstTruthy(item["b64_json"], item["base64"]).(string); ok && strings.TrimSpace(value) != "" {
		mimeType := pyStr(firstTruthy(item["mime"], item["mime_type"], item["mimeType"]))
		if mimeType == "" {
			mimeType = "image/png"
		}
		image, err := decodeJSONImageString(value, mimeType)
		return &image, err
	}
	if source, ok := item["source"].(map[string]any); ok && pyStr(source["type"]) == "base64" {
		mimeType := pyStr(firstTruthy(source["media_type"], source["mime_type"]))
		if mimeType == "" {
			mimeType = "image/png"
		}
		image, err := decodeJSONImageString(pyStr(source["data"]), mimeType)
		return &image, err
	}
	return nil, nil
}

// ExtractImages is extract_image_from_message_content.
func (im Images) ExtractImages(content any) ([]imageBytes, error) {
	list, ok := content.([]any)
	if !ok {
		return nil, nil
	}
	images := []imageBytes{}
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		var image *imageBytes
		var err error
		switch strings.TrimSpace(pyStr(item["type"])) {
		case "image_url":
			image, err = im.decodeMessageImageURL(firstTruthy(item["image_url"], item["url"], item))
		case "input_image", "image":
			image, err = im.decodeMessageImageObject(item)
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		if image != nil {
			images = append(images, *image)
		}
	}
	return images, nil
}

// NormalizeMessages is conversation.normalize_messages: global system prompt, system text, and
// user messages with images as {"type": "image", "data": []byte, "mime": ...} parts.
func (d *Deps) NormalizeMessages(messages any, system any) ([]map[string]any, error) {
	normalized := []map[string]any{}
	if prompt := d.Settings.GlobalSystemPrompt(); prompt != "" {
		normalized = append(normalized, map[string]any{"role": "system", "content": prompt})
	}
	if text := MessageText(system); text != "" {
		normalized = append(normalized, map[string]any{"role": "system", "content": text})
	}
	list, _ := messages.([]any)
	for _, raw := range list {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, present := message["role"]
		if !present {
			role = "user"
		}
		content, present := message["content"]
		if !present {
			content = ""
		}
		text := MessageText(content)
		images := []imageBytes{}
		if role == "user" {
			found, err := d.Images.ExtractImages(content)
			if err != nil {
				return nil, err
			}
			images = append(images, found...)
			if parts, ok := content.([]any); ok {
				for _, rawPart := range parts {
					part, ok := rawPart.(map[string]any)
					if !ok || part["type"] != "image" {
						continue
					}
					data, ok := part["data"].([]byte)
					if !ok {
						continue
					}
					duplicate := false
					for _, existing := range images {
						duplicate = duplicate || bytes.Equal(existing.data, data)
					}
					if !duplicate {
						mimeType := pyStr(part["mime"])
						if mimeType == "" {
							mimeType = "image/png"
						}
						images = append(images, imageBytes{data, mimeType})
					}
				}
			}
		}
		if len(images) > 0 {
			parts := []any{}
			if text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			for _, image := range images {
				parts = append(parts, map[string]any{"type": "image", "data": image.data, "mime": image.mime})
			}
			normalized = append(normalized, map[string]any{"role": role, "content": parts})
		} else {
			normalized = append(normalized, map[string]any{"role": role, "content": text})
		}
	}
	return normalized, nil
}

// ---- tokens ----

// CountMessageTextTokens is count_message_text_tokens.
func CountMessageTextTokens(messages []map[string]any, model string) int {
	total := 0
	for _, message := range messages {
		total += 3
		for _, key := range orderedKeys(message) {
			value := message[key]
			if list, ok := value.([]any); ok && key == "content" {
				total += imagegen.CountTextTokens(MessageText(list), model)
			} else if text, ok := value.(string); ok {
				total += imagegen.CountTextTokens(text, model)
			} else {
				continue
			}
			if key == "name" {
				total++
			}
		}
	}
	return total + 3
}

// orderedKeys puts role and content first, as the dicts are built in Python.
func orderedKeys(m map[string]any) []string {
	keys := []string{}
	for _, key := range []string{"role", "content"} {
		if _, ok := m[key]; ok {
			keys = append(keys, key)
		}
	}
	for key := range m {
		if key != "role" && key != "content" {
			keys = append(keys, key)
		}
	}
	return keys
}

// CountImageContentTokens is count_image_content_tokens.
func CountImageContentTokens(content any) int {
	list, ok := content.([]any)
	if !ok {
		return 0
	}
	total := 0
	for _, raw := range list {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		partType := strings.TrimSpace(pyStr(part["type"]))
		if partType != "image" && partType != "image_url" && partType != "input_image" && part["source"] == nil {
			continue
		}
		width, height, ok := partSize(part)
		if !ok {
			continue
		}
		detail := strings.ToLower(strings.TrimSpace(pyStr(part["detail"])))
		if detail == "" {
			detail = "auto"
		}
		total += imagegen.CountImageInputTokens(width, height, detail)
	}
	return total
}

func partSize(part map[string]any) (int, int, bool) {
	width, _ := strconv.Atoi(pyStr(part["width"]))
	height, _ := strconv.Atoi(pyStr(part["height"]))
	if width > 0 && height > 0 {
		return width, height, true
	}
	if data, ok := part["data"].([]byte); ok {
		return imagegen.ImageSizeFromBytes(data)
	}
	imageURL := part["image_url"]
	if m, ok := imageURL.(map[string]any); ok {
		imageURL = firstTruthy(m["url"], m["image_url"])
	}
	if text, ok := imageURL.(string); ok && strings.HasPrefix(text, "data:") {
		if _, encoded, found := strings.Cut(text, ","); found {
			if data, err := base64.StdEncoding.DecodeString(encoded); err == nil {
				return imagegen.ImageSizeFromBytes(data)
			}
		}
		return 0, 0, false
	}
	if source, ok := part["source"].(map[string]any); ok && pyStr(source["type"]) == "base64" {
		if data, err := base64.StdEncoding.DecodeString(pyStr(source["data"])); err == nil {
			return imagegen.ImageSizeFromBytes(data)
		}
	}
	return 0, 0, false
}

// CountMessageImageTokens is count_message_image_tokens.
func CountMessageImageTokens(messages []map[string]any) int {
	total := 0
	for _, message := range messages {
		total += CountImageContentTokens(message["content"])
	}
	return total
}
