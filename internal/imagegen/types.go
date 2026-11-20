package imagegen

import (
	"bytes"
	"encoding/base64"
	"strings"
	"time"
)

// Request is ConversationRequest (image fields only).
type Request struct {
	Model           string
	Prompt          string
	Images          []string // base64 or data URLs
	N               int
	Size            string // "" = None
	Quality         string
	OutputFormat    string
	FrameCount      *int
	FrameDurationMS *int
	ResponseFormat  string
	BaseURL         string
	MessageAsError  bool
	// PollTimeoutSecs is set by callers that want one fixed wait (account test); 0 = None.
	PollTimeoutSecs int
	// FailoverTimeoutSecs is set by the retry loop for one attempt (换号生成); 0 = None.
	FailoverTimeoutSecs int
	Progress            func(step string)
}

func (r Request) withDefaults() Request {
	if r.Model == "" {
		r.Model = "auto"
	}
	if r.N <= 0 {
		r.N = 1
	}
	if r.Quality == "" {
		r.Quality = "auto"
	}
	if r.OutputFormat == "" {
		r.OutputFormat = "png"
	}
	if r.ResponseFormat == "" {
		r.ResponseFormat = "b64_json"
	}
	return r
}

func (r Request) progress(step string) {
	if r.Progress != nil {
		r.Progress(step)
	}
}

// Output is ImageOutput.
type Output struct {
	Kind              string // progress | message | result
	Model             string
	Index             int
	Total             int
	Created           int64
	Text              string
	UpstreamEventType string
	Data              []map[string]any
	AccountEmail      string
	ConversationID    string
}

func newOutput(kind string, req Request, index, total int) Output {
	return Output{Kind: kind, Model: req.Model, Index: index, Total: total, Created: time.Now().Unix(), Data: []map[string]any{}}
}

// Chunk is ImageOutput.to_chunk().
func (o Output) Chunk() map[string]any {
	data := o.Data
	if data == nil {
		data = []map[string]any{}
	}
	chunk := map[string]any{
		"object": "image.generation.chunk", "created": o.Created, "model": o.Model, "index": o.Index, "total": o.Total,
		"progress_text": o.Text, "upstream_event_type": o.UpstreamEventType, "data": []map[string]any{},
	}
	if o.AccountEmail != "" {
		chunk["_account_email"] = o.AccountEmail
	}
	if o.ConversationID != "" {
		chunk["_conversation_id"] = o.ConversationID
	}
	switch o.Kind {
	case "message":
		chunk["object"] = "image.generation.message"
		chunk["message"] = o.Text
		delete(chunk, "progress_text")
		delete(chunk, "upstream_event_type")
	case "result":
		chunk["object"] = "image.generation.result"
		chunk["data"] = data
		delete(chunk, "progress_text")
		delete(chunk, "upstream_event_type")
	}
	return chunk
}

// Hooks are the parts outside the pipeline: storing an image and the optional upscale.
type Hooks struct {
	// Save stores the image and returns its public URL (save_image_bytes).
	Save func(data []byte, baseURL string) string
	// Upscale is maybe_upscale_image; nil means no upscaling.
	Upscale func(data []byte, size string) []byte
}

// IsGIF reports GIF bytes (utils/helper.py is_gif_bytes).
func IsGIF(data []byte) bool {
	return bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))
}

// FormatImageResult is format_image_result: decode, upscale (not GIFs), store, and build the
// OpenAI data items.
func (h Hooks) FormatImageResult(items []map[string]any, prompt, responseFormat, baseURL string, created int64, message, size string) map[string]any {
	data := []map[string]any{}
	for _, item := range items {
		b64, _ := item["b64_json"].(string)
		b64 = strings.TrimSpace(b64)
		if b64 == "" {
			continue
		}
		revised, _ := item["revised_prompt"].(string)
		if revised == "" {
			revised = prompt
		}
		if revised = strings.TrimSpace(revised); revised == "" {
			revised = prompt
		}
		image, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		if !IsGIF(image) && h.Upscale != nil {
			if upscaled := h.Upscale(image, size); upscaled != nil && !bytes.Equal(upscaled, image) {
				image = upscaled
				b64 = base64.StdEncoding.EncodeToString(image)
			}
		}
		url := ""
		if h.Save != nil {
			url = h.Save(image, baseURL)
		}
		if responseFormat == "b64_json" {
			data = append(data, map[string]any{"b64_json": b64, "url": url, "revised_prompt": revised})
		} else {
			data = append(data, map[string]any{"url": url, "revised_prompt": revised})
		}
	}
	if created == 0 {
		created = time.Now().Unix()
	}
	result := map[string]any{"created": created, "data": data}
	if message != "" && len(data) == 0 {
		result["message"] = message
	}
	return result
}

func nowUnix() int64 { return time.Now().Unix() }
