package compat

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"net/http"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/gin-gonic/gin"
	xdraw "golang.org/x/image/draw"
)

// resolveImageBaseURL is resolve_image_base_url: config base_url, else this request's origin.
func resolveImageBaseURL(c *gin.Context, d *Deps) string {
	if base := d.Settings.BaseURL(); base != "" {
		return base
	}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host
}

// imageErrorResponse is _image_error_response.
func imageErrorResponse(c *gin.Context, err error) {
	message := imagegen.PublicImageErrorMessage(err.Error())
	if strings.Contains(strings.ToLower(message), "no available image quota") {
		c.JSON(429, gin.H{"error": gin.H{"message": "no available image quota", "type": "insufficient_quota", "param": nil, "code": "insufficient_quota"}})
		return
	}
	var generation *imagegen.GenerationError
	if errors.As(err, &generation) {
		c.JSON(generation.StatusCode, generation.OpenAIError())
		return
	}
	c.JSON(502, openAIError(message, 502))
}

func errorAccount(err error) (string, string) {
	var generation *imagegen.GenerationError
	var timeout *imagegen.PollTimeoutError
	switch {
	case errors.As(err, &generation):
		return generation.AccountEmail, generation.ConversationID
	case errors.As(err, &timeout):
		return timeout.AccountEmail, ""
	}
	return "", ""
}

// imageRequest is what both endpoints hand to the generator.
type imageRequest struct {
	payload map[string]any
	images  []ImageInput
	masks   []ImageInput
}

// runImages is the shared tail of openai_v1_image_generations.handle / openai_v1_image_edit.handle,
// wrapped in LoggedCall.run.
func runImages(c *gin.Context, d *Deps, call *CallLog, req imageRequest) {
	payload := req.payload
	str := func(key, def string) string {
		if text := cleanString(payload[key], ""); text != "" {
			return text
		}
		return def
	}
	prompt := str("prompt", "")
	model := str("model", "gpt-image-2")
	quality := str("quality", "auto")
	size := ""
	if value, ok := payload["size"].(string); ok {
		size = value
	}
	n, _ := payload["n"].(int)
	encoded := []string{}
	inputs := [][]byte{}
	edits := req.images != nil
	if edits {
		if strings.ToLower(str("output_format", "")) == "gif" {
			err := imagegen.NewGenerationError("GIF output is not supported for image edits")
			call.Log("调用失败", nil, "failed", err.Error(), nil, "", "")
			imageErrorResponse(c, err)
			return
		}
		composited := compositeMasks(req.images, req.masks)
		for _, image := range composited {
			if len(image.Data) > 0 {
				encoded = append(encoded, base64.StdEncoding.EncodeToString(image.Data))
				inputs = append(inputs, image.Data)
			}
		}
		if len(encoded) == 0 {
			err := imagegen.NewGenerationError("image is required")
			call.Log("调用失败", nil, "failed", err.Error(), nil, "", "")
			imageErrorResponse(c, err)
			return
		}
	}
	request := imagegen.Request{
		Prompt: prompt, Model: model, N: n, Size: size, Quality: quality, OutputFormat: str("output_format", "png"),
		ResponseFormat: str("response_format", "b64_json"), BaseURL: resolveImageBaseURL(c, d), MessageAsError: true, Images: encoded,
	}
	if edits {
		request.OutputFormat = "png"
	}
	if frames, ok := payload["frame_count"].(int); ok {
		request.FrameCount = &frames
	}
	if duration, ok := payload["frame_duration_ms"].(int); ok {
		request.FrameDurationMS = &duration
	}
	outputs, err := d.Images.StreamWithPool(c.Request.Context(), request)
	if err != nil {
		email, conversation := errorAccount(err)
		call.Log("调用失败", nil, "failed", err.Error(), nil, email, conversation)
		imageErrorResponse(c, err)
		return
	}
	if stream, _ := payload["stream"].(bool); stream {
		chunks := make([]any, 0, len(outputs))
		for _, output := range outputs {
			chunks = append(chunks, output.Chunk())
		}
		writeSSE(c, chunks)
		urls := collectURLs(chunks)
		emails := collectNamed(chunks, "_account_email", "account_email")
		ids := collectNamed(chunks, "_conversation_id")
		email, conversation := "", ""
		if len(emails) > 0 {
			email = emails[0]
		}
		if len(ids) > 0 {
			conversation = ids[0]
		}
		call.Log("流式调用结束", nil, "success", "", urls, email, conversation)
		return
	}
	result := imagegen.CollectImageOutputs(outputs)
	data, _ := result["data"].([]map[string]any)
	result["usage"] = imagegen.ImageUsage(imagegen.CountTextTokens(prompt, model), imagegen.CountImageInputsTokens(inputs), imagegen.CountImageOutputItemsTokens(data, size, quality))
	call.Log("调用完成", toJSONValue(result), "success", "", nil, "", "")
	delete(result, "_account_email")
	c.JSON(http.StatusOK, result)
}

// toJSONValue round-trips through JSON so collectors see plain maps and lists.
func toJSONValue(value any) any {
	raw, _ := json.Marshal(value)
	var out any
	json.Unmarshal(raw, &out)
	return out
}

// writeSSE is sse_json_stream: an opening comment, then one data line per item.
func writeSSE(c *gin.Context, items []any) {
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	io.WriteString(c.Writer, ": stream-open\n\n")
	for _, item := range items {
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		encoder.Encode(stripInternal(toJSONValue(item)))
		fmt.Fprintf(c.Writer, "data: %s\n\n", strings.TrimRight(buf.String(), "\n"))
	}
	c.Writer.Flush()
}

// compositeMasks is _composite_mask: the mask's alpha (or luminance) becomes the image alpha.
func compositeMasks(images, masks []ImageInput) []ImageInput {
	if len(masks) == 0 {
		return images
	}
	out := make([]ImageInput, 0, len(images))
	for i, input := range images {
		mask := masks[len(masks)-1]
		if i < len(masks) {
			mask = masks[i]
		}
		src, _, err := image.Decode(bytes.NewReader(input.Data))
		if err != nil {
			out = append(out, input)
			continue
		}
		maskImage, _, err := image.Decode(bytes.NewReader(mask.Data))
		if err != nil {
			out = append(out, input)
			continue
		}
		bounds := src.Bounds()
		rgba := image.NewNRGBA(bounds)
		draw.Draw(rgba, bounds, src, bounds.Min, draw.Src)
		alpha := image.NewGray(maskImage.Bounds())
		_, hasAlpha := maskImage.(*image.NRGBA)
		_, hasAlpha2 := maskImage.(*image.RGBA)
		for y := maskImage.Bounds().Min.Y; y < maskImage.Bounds().Max.Y; y++ {
			for x := maskImage.Bounds().Min.X; x < maskImage.Bounds().Max.X; x++ {
				if hasAlpha || hasAlpha2 {
					_, _, _, a := maskImage.At(x, y).RGBA()
					alpha.SetGray(x, y, color.Gray{Y: uint8(a >> 8)})
				} else {
					alpha.Set(x, y, color.GrayModel.Convert(maskImage.At(x, y)))
				}
			}
		}
		resized := image.NewGray(bounds)
		xdraw.CatmullRom.Scale(resized, bounds, alpha, alpha.Bounds(), draw.Src, nil)
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				pixel := rgba.NRGBAAt(x, y)
				pixel.A = resized.GrayAt(x, y).Y
				rgba.SetNRGBA(x, y, pixel)
			}
		}
		var buf bytes.Buffer
		png.Encode(&buf, rgba)
		out = append(out, ImageInput{Data: buf.Bytes(), Filename: input.Filename, MimeType: "image/png"})
	}
	return out
}

// imageGenerationBody validates the generations body like ImageGenerationRequest (pydantic).
func imageGenerationBody(c *gin.Context, body map[string]any) (map[string]any, bool) {
	prompt, ok := body["prompt"].(string)
	if !ok {
		abortValidation(c, "prompt", "Field required")
		return nil, false
	}
	if len([]rune(prompt)) < 1 {
		abortValidation(c, "prompt", "String should have at least 1 character")
		return nil, false
	}
	payload := map[string]any{"prompt": prompt, "model": "gpt-image-2", "n": 1, "size": nil, "quality": "auto", "output_format": "png",
		"frame_count": nil, "frame_duration_ms": nil, "response_format": "b64_json", "history_disabled": true, "stream": nil}
	for _, key := range []string{"model", "quality", "output_format", "response_format"} {
		if value, present := body[key]; present {
			text, isString := value.(string)
			if !isString {
				abortValidation(c, key, "Input should be a valid string")
				return nil, false
			}
			payload[key] = text
		}
	}
	if value, present := body["size"]; present && value != nil {
		text, isString := value.(string)
		if !isString {
			abortValidation(c, "size", "Input should be a valid string")
			return nil, false
		}
		payload["size"] = text
	}
	if value, present := body["n"]; present {
		n, valid := pydanticInt(value)
		if !valid {
			abortValidation(c, "n", "Input should be a valid integer")
			return nil, false
		}
		if n < 1 || n > 4 {
			abortValidation(c, "n", fmt.Sprintf("Input should be %s", map[bool]string{true: "greater than or equal to 1", false: "less than or equal to 4"}[n < 1]))
			return nil, false
		}
		payload["n"] = n
	}
	for _, key := range []string{"frame_count", "frame_duration_ms"} {
		if value, present := body[key]; present && value != nil {
			n, valid := pydanticInt(value)
			if !valid {
				abortValidation(c, key, "Input should be a valid integer")
				return nil, false
			}
			payload[key] = n
		}
	}
	for _, key := range []string{"history_disabled", "stream"} {
		if value, present := body[key]; present && value != nil {
			flag, valid := pydanticBool(value)
			if !valid {
				abortValidation(c, key, "Input should be a valid boolean")
				return nil, false
			}
			payload[key] = flag
		}
	}
	return payload, true
}

// registerImages serves /v1/images/generations and /v1/images/edits (api/ai.py).
func registerImages(engine *gin.Engine, d *Deps) {
	edit := func(c *gin.Context, raw []byte) {
		identity := d.Auth.Identify(c.GetHeader("Authorization"))
		if identity == nil {
			abortDetail(c, 401, "密钥无效或已失效，请重新登录")
			return
		}
		payload, imageSources, maskSources, err := parseImageEditRequest(c, raw)
		if err != nil {
			abortHTTPError(c, err)
			return
		}
		prompt, _ := payload["prompt"].(string)
		model, _ := payload["model"].(string)
		size, _ := payload["size"].(string)
		call := newCallLog(d.Logs, identity, "/v1/images/edits", model, "图生图", prompt, size)
		if rejected := CheckRequest(d.Settings, prompt); rejected != nil {
			call.Log("调用失败", nil, "failed", detailRepr(rejected.message), nil, "", "")
			abortHTTPError(c, rejected)
			return
		}
		images, err := readImageSources(imageSources, d.Settings.Proxy())
		if err != nil {
			abortHTTPError(c, err)
			return
		}
		var masks []ImageInput
		if len(maskSources) > 0 {
			if masks, err = readImageSources(maskSources, d.Settings.Proxy()); err != nil {
				abortHTTPError(c, err)
				return
			}
		}
		runImages(c, d, call, imageRequest{payload: payload, images: images, masks: masks})
	}
	engine.POST("/v1/images/generations", func(c *gin.Context) {
		raw, _ := io.ReadAll(c.Request.Body)
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var body map[string]any
		if err := decoder.Decode(&body); err != nil || body == nil {
			abortValidation(c, "", "Input should be a valid dictionary or object to extract fields from")
			return
		}
		payload, ok := imageGenerationBody(c, body)
		if !ok {
			return
		}
		if hasJSONImageReference(c.GetHeader("Content-Type"), body) {
			edit(c, raw)
			return
		}
		identity := d.Auth.Identify(c.GetHeader("Authorization"))
		if identity == nil {
			abortDetail(c, 401, "密钥无效或已失效，请重新登录")
			return
		}
		size, _ := payload["size"].(string)
		call := newCallLog(d.Logs, identity, "/v1/images/generations", payload["model"].(string), "文生图", payload["prompt"].(string), size)
		if rejected := CheckRequest(d.Settings, payload["prompt"].(string)); rejected != nil {
			call.Log("调用失败", nil, "failed", detailRepr(rejected.message), nil, "", "")
			abortHTTPError(c, rejected)
			return
		}
		runImages(c, d, call, imageRequest{payload: payload})
	})
	engine.POST("/v1/images/edits", func(c *gin.Context) {
		raw, _ := io.ReadAll(c.Request.Body)
		edit(c, raw)
	})
}

// detailRepr is str(HTTPException.detail) for {"error": message}: Python's dict repr.
func detailRepr(message string) string {
	quoted := "'" + strings.ReplaceAll(strings.ReplaceAll(message, "\\", "\\\\"), "'", "\\'") + "'"
	if strings.Contains(message, "'") && !strings.Contains(message, `"`) {
		quoted = `"` + message + `"`
	}
	return "{'error': " + quoted + "}"
}

func abortHTTPError(c *gin.Context, err error) {
	var input *httpError
	if errors.As(err, &input) {
		abortDetail(c, input.status, input.message)
		return
	}
	abortDetail(c, 500, err.Error())
}

