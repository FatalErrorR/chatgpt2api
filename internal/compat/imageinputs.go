package compat

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// api/image_inputs.py: the request parsing of /v1/images/edits (multipart and JSON).

const maxImageReferenceBytes = 50 * 1024 * 1024

var (
	imageReferenceFields = map[string]bool{"image": true, "image[]": true, "images": true, "images[]": true, "image_url": true, "image_url[]": true}
	maskReferenceFields  = map[string]bool{"mask": true, "mask[]": true}
)

// httpError is FastAPI's HTTPException(status, {"error": message}).
type httpError struct {
	status  int
	message string
}

func (e *httpError) Error() string { return e.message }

func badRequest(format string, args ...any) *httpError {
	return &httpError{status: 400, message: fmt.Sprintf(format, args...)}
}

// ImageInput is one reference image.
type ImageInput struct {
	Data     []byte
	Filename string
	MimeType string
}

// imageSource is a reference before reading: inline data, a data/http URL, or an uploaded file.
type imageSource struct {
	inline *ImageInput
	url    string
	upload *multipart.FileHeader
}

func cleanString(value any, def string) string {
	text := ""
	switch v := value.(type) {
	case nil:
		text = def
	case string:
		text = v
	case json.Number:
		text = v.String()
	case bool:
		if v {
			text = "True"
		} else {
			text = "False"
		}
	default:
		text = fmt.Sprint(v)
	}
	if text = strings.TrimSpace(text); text == "" {
		return def
	}
	return text
}

func parseStreamFlag(value any) (any, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case bool:
		return v, nil
	case string:
		if v == "" {
			return nil, nil
		}
	}
	switch strings.ToLower(cleanString(value, "")) {
	case "true", "1", "yes", "y", "on":
		return true, nil
	case "false", "0", "no", "n", "off":
		return false, nil
	}
	return nil, badRequest("stream must be a boolean")
}

func parseCount(value any) (int, error) {
	if value == nil || value == "" || value == false {
		return 1, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(cleanString(value, "1")))
	if err != nil {
		if f, ferr := strconv.ParseFloat(cleanString(value, ""), 64); ferr == nil && fmt.Sprint(value) != "" {
			if _, isNumber := value.(json.Number); isNumber {
				n, err = int(f), nil
			}
		}
		if err != nil {
			return 0, badRequest("n must be an integer")
		}
	}
	if n == 0 {
		n = 1 // int(value or 1)
	}
	if n < 1 || n > 4 {
		return 0, badRequest("n must be between 1 and 4")
	}
	return n, nil
}

// payloadFromFields is _payload_from_fields.
func payloadFromFields(fields map[string]any) (map[string]any, error) {
	prompt := cleanString(fields["prompt"], "")
	if prompt == "" {
		return nil, badRequest("prompt is required")
	}
	n, err := parseCount(fields["n"])
	if err != nil {
		return nil, err
	}
	stream, err := parseStreamFlag(fields["stream"])
	if err != nil {
		return nil, err
	}
	var size any
	if text := cleanString(fields["size"], ""); text != "" {
		size = text
	}
	payload := map[string]any{
		"prompt": prompt, "model": cleanString(fields["model"], "gpt-image-2"), "n": n, "size": size,
		"quality": cleanString(fields["quality"], "auto"), "output_format": cleanString(fields["output_format"], "png"),
		"response_format": cleanString(fields["response_format"], "b64_json"), "stream": stream,
	}
	if _, ok := fields["client_task_id"]; ok {
		payload["client_task_id"] = cleanString(fields["client_task_id"], "")
	}
	return payload, nil
}

func decodeBase64Image(value any, filename, mimeType string) (*ImageInput, error) {
	data, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(fmt.Sprint(value)))
	if err != nil {
		return nil, badRequest("invalid base64 image data")
	}
	if len(data) == 0 {
		return nil, badRequest("image file is empty")
	}
	if len(data) > maxImageReferenceBytes {
		return nil, badRequest("image URL exceeds 50MB limit")
	}
	return &ImageInput{Data: data, Filename: filename, MimeType: mimeType}, nil
}

func sourcesFromObject(value map[string]any) ([]imageSource, error) {
	_, hasImageURL := value["image_url"]
	_, hasURL := value["url"]
	if value["file_id"] != nil && value["file_id"] != "" {
		return nil, badRequest("file_id image references are not supported; use image_url instead")
	}
	inline := value["b64_json"]
	if inline == nil || inline == "" {
		inline = value["base64"]
	}
	if inline != nil && inline != "" {
		filename := cleanString(firstOf(value["filename"], value["file_name"]), "image.png")
		mimeType := cleanString(firstOf(value["mime_type"], value["mimeType"]), "image/png")
		image, err := decodeBase64Image(inline, filename, mimeType)
		if err != nil {
			return nil, err
		}
		return []imageSource{{inline: image}}, nil
	}
	if !hasImageURL && !hasURL {
		return nil, badRequest("image reference must include image_url")
	}
	imageURL, ok := value["image_url"]
	if !ok {
		imageURL = value["url"]
	}
	if nested, isMap := imageURL.(map[string]any); isMap {
		imageURL = nested["url"]
	}
	return sourcesFromValue(imageURL)
}

func firstOf(values ...any) any {
	for _, value := range values {
		if value != nil && value != "" {
			return value
		}
	}
	return nil
}

// sourcesFromValue is _sources_from_value.
func sourcesFromValue(value any) ([]imageSource, error) {
	if text, ok := value.(string); ok {
		if trimmed := strings.TrimSpace(text); trimmed != "" && (trimmed[0] == '[' || trimmed[0] == '{') {
			var decoded any
			if json.Unmarshal([]byte(trimmed), &decoded) == nil {
				value = decoded
			}
		}
	}
	switch v := value.(type) {
	case *multipart.FileHeader:
		return []imageSource{{upload: v}}, nil
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return nil, nil
		}
		lower := strings.ToLower(text)
		if strings.HasPrefix(lower, "data:") || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			return []imageSource{{url: text}}, nil
		}
		image, err := decodeBase64Image(text, "image.png", "image/png")
		if err != nil {
			return nil, err
		}
		return []imageSource{{inline: image}}, nil
	case []any:
		out := []imageSource{}
		for _, item := range v {
			sources, err := sourcesFromValue(item)
			if err != nil {
				return nil, err
			}
			out = append(out, sources...)
		}
		return out, nil
	case map[string]any:
		return sourcesFromObject(v)
	case nil:
		return nil, nil
	}
	return nil, badRequest("invalid image reference")
}

// hasJSONImageReference: a generations request that carries reference images goes to edits.
func hasJSONImageReference(contentType string, body map[string]any) bool {
	if mediaType(contentType) != "application/json" {
		return false
	}
	for _, key := range []string{"images", "image", "image_url"} {
		if truthyValue(body[key]) {
			return true
		}
	}
	return false
}

func truthyValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	case bool:
		return v
	}
	return true
}

func mediaType(contentType string) string {
	return strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
}

// parseImageEditRequest is parse_image_edit_request: (payload, image sources, mask sources).
func parseImageEditRequest(c *gin.Context, raw []byte) (map[string]any, []imageSource, []imageSource, error) {
	if mediaType(c.GetHeader("Content-Type")) == "application/json" {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var parsed any
		if err := decoder.Decode(&parsed); err != nil {
			return nil, nil, nil, badRequest("invalid JSON body")
		}
		body, ok := parsed.(map[string]any)
		if !ok {
			return nil, nil, nil, badRequest("JSON body must be an object")
		}
		payload, err := payloadFromFields(body)
		if err != nil {
			return nil, nil, nil, err
		}
		images := []imageSource{}
		for _, key := range []string{"images", "image", "image_url"} {
			if value, present := body[key]; present {
				sources, err := sourcesFromValue(value)
				if err != nil {
					return nil, nil, nil, err
				}
				images = append(images, sources...)
			}
		}
		masks := []imageSource{}
		if value, present := body["mask"]; present && value != nil {
			if masks, err = sourcesFromValue(value); err != nil {
				return nil, nil, nil, err
			}
		}
		return payload, images, masks, nil
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	form, err := c.MultipartForm()
	if err != nil {
		if err := c.Request.ParseForm(); err != nil {
			return nil, nil, nil, badRequest("invalid form body")
		}
		form = &multipart.Form{Value: c.Request.PostForm}
	}
	fields := map[string]any{}
	for _, key := range []string{"client_task_id", "prompt", "model", "n", "size", "quality", "response_format", "stream"} {
		if values := form.Value[key]; len(values) > 0 {
			fields[key] = values[0]
		}
	}
	images, masks := []imageSource{}, []imageSource{}
	add := func(key string, value any) error {
		sources, err := sourcesFromValue(value)
		if err != nil {
			return err
		}
		if imageReferenceFields[key] {
			images = append(images, sources...)
		} else if maskReferenceFields[key] {
			masks = append(masks, sources...)
		}
		return nil
	}
	for key, values := range form.Value {
		if imageReferenceFields[key] || maskReferenceFields[key] {
			for _, value := range values {
				if err := add(key, value); err != nil {
					return nil, nil, nil, err
				}
			}
		}
	}
	for key, files := range form.File {
		if imageReferenceFields[key] || maskReferenceFields[key] {
			for _, file := range files {
				if err := add(key, file); err != nil {
					return nil, nil, nil, err
				}
			}
		}
	}
	payload, err := payloadFromFields(fields)
	if err != nil {
		return nil, nil, nil, err
	}
	return payload, images, masks, nil
}

var unsafeFilename = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func extensionFromMime(mimeType string) string {
	subtype := "png"
	if _, after, ok := strings.Cut(mimeType, "/"); ok {
		subtype = strings.SplitN(after, "+", 2)[0]
	}
	if subtype == "jpeg" {
		return "jpg"
	}
	if cleaned := regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(subtype), ""); cleaned != "" {
		return cleaned
	}
	return "png"
}

func safeFilename(name, mimeType, fallback string) string {
	cleaned := strings.Trim(unsafeFilename.ReplaceAllString(name, "_"), "._")
	if cleaned == "" {
		cleaned = fallback
	}
	if !strings.Contains(cleaned, ".") {
		cleaned += "." + extensionFromMime(mimeType)
	}
	return cleaned
}

func decodeDataURL(raw string) (*ImageInput, error) {
	header, payload, ok := strings.Cut(raw, ",")
	if !ok {
		return nil, badRequest("invalid data image URL")
	}
	mimeType := strings.TrimPrefix(strings.SplitN(header, ";", 2)[0], "data:")
	if mimeType == "" {
		mimeType = "image/png"
	}
	if !strings.HasPrefix(mimeType, "image/") {
		return nil, badRequest("image_url must point to an image")
	}
	var data []byte
	var err error
	if strings.Contains(header, ";base64") {
		data, err = base64.StdEncoding.Strict().DecodeString(payload)
	} else {
		var text string
		text, err = url.PathUnescape(payload)
		data = []byte(text)
	}
	if err != nil {
		return nil, badRequest("invalid data image URL")
	}
	if len(data) == 0 {
		return nil, badRequest("image URL is empty")
	}
	if len(data) > maxImageReferenceBytes {
		return nil, badRequest("image URL exceeds 50MB limit")
	}
	return &ImageInput{Data: data, Filename: "image_url." + extensionFromMime(mimeType), MimeType: mimeType}, nil
}

// downloadImageURL is _download_image_url (through the global proxy when one is set).
func downloadImageURL(source string, proxy string) (*ImageInput, error) {
	source = strings.TrimSpace(source)
	if strings.HasPrefix(source, "data:") {
		return decodeDataURL(source)
	}
	parsed, err := url.Parse(source)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, badRequest("image_url must be an http or https URL")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		if proxyURL, perr := url.Parse(proxy); perr == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}
	client := &http.Client{Timeout: 60 * time.Second, Transport: transport}
	req, _ := http.NewRequest(http.MethodGet, source, nil)
	req.Header.Set("Accept", "image/*,*/*;q=0.8")
	req.Header.Set("User-Agent", "chatgpt2api image fetcher")
	response, err := client.Do(req)
	if err != nil {
		return nil, badRequest("image_url fetch failed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, badRequest("image_url fetch failed: HTTP %d", response.StatusCode)
	}
	if length := strings.TrimSpace(response.Header.Get("Content-Length")); length != "" {
		if n, err := strconv.Atoi(length); err == nil && n > maxImageReferenceBytes {
			return nil, badRequest("image_url exceeds 50MB limit")
		}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxImageReferenceBytes+1))
	if err != nil {
		return nil, badRequest("image_url fetch failed: %v", err)
	}
	if len(data) == 0 {
		return nil, badRequest("image_url returned empty content")
	}
	if len(data) > maxImageReferenceBytes {
		return nil, badRequest("image_url exceeds 50MB limit")
	}
	headerType := strings.ToLower(strings.TrimSpace(strings.SplitN(response.Header.Get("Content-Type"), ";", 2)[0]))
	guessed := mime.TypeByExtension(path.Ext(parsed.Path))
	mimeType := ""
	switch {
	case strings.HasPrefix(headerType, "image/"):
		mimeType = headerType
	case headerType != "" && headerType != "application/octet-stream" && headerType != "binary/octet-stream":
		return nil, badRequest("image_url must point to an image")
	case strings.HasPrefix(guessed, "image/"):
		mimeType = guessed
	default:
		mimeType = "image/png"
	}
	name, _ := url.PathUnescape(path.Base(parsed.Path))
	return &ImageInput{Data: data, Filename: safeFilename(name, mimeType, "image_url"), MimeType: mimeType}, nil
}

// readImageSources is read_image_sources.
func readImageSources(sources []imageSource, proxy string) ([]ImageInput, error) {
	images := []ImageInput{}
	for _, source := range sources {
		switch {
		case source.inline != nil:
			images = append(images, *source.inline)
		case source.upload != nil:
			file, err := source.upload.Open()
			if err != nil {
				return nil, badRequest("image file is empty")
			}
			data, _ := io.ReadAll(file)
			file.Close()
			if len(data) == 0 {
				return nil, badRequest("image file is empty")
			}
			filename := source.upload.Filename
			if filename == "" {
				filename = "image.png"
			}
			contentType := source.upload.Header.Get("Content-Type")
			if contentType == "" {
				contentType = "image/png"
			}
			images = append(images, ImageInput{Data: data, Filename: filename, MimeType: contentType})
		default:
			image, err := downloadImageURL(source.url, proxy)
			if err != nil {
				return nil, err
			}
			images = append(images, *image)
		}
	}
	if len(images) == 0 {
		return nil, badRequest("image file or image_url is required")
	}
	return images, nil
}
