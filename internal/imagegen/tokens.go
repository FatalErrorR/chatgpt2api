package imagegen

import (
	"bytes"
	"encoding/base64"
	"image"
	_ "image/gif" // the decoders image.DecodeConfig needs, whoever imports this package
	_ "image/jpeg"
	_ "image/png"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/tiktoken-go/tokenizer"
	_ "golang.org/x/image/webp"
)

// Token counting of utils/image_tokens.py and conversation.count_text_tokens.

const (
	defaultImageWidth    = 1024
	defaultImageHeight   = 1024
	imageInputTokenModel = "gpt-5.4-mini"
	patchSize            = 32
	tileSize             = 512
	tileHighShortSide    = 768
)

var patch1536Models = []string{"gpt-5.4-mini", "gpt-5.4-nano", "gpt-5-mini", "gpt-5-nano", "gpt-5.2", "gpt-5.3-codex", "gpt-5-codex-mini",
	"gpt-5.1-codex-mini", "gpt-5.2-codex", "gpt-5.2-chat-latest", "o4-mini", "gpt-4.1-mini", "gpt-4.1-nano"}

// patchMultipliers keeps the Python dict order (first matching prefix wins).
var patchMultipliers = []struct {
	prefix string
	value  float64
}{{"gpt-5.4-mini", 1.62}, {"gpt-5.4-nano", 2.46}, {"gpt-5-mini", 1.62}, {"gpt-5-nano", 2.46}, {"gpt-4.1-mini", 1.62}, {"gpt-4.1-nano", 2.46}, {"o4-mini", 1.72}}

// CountTextTokens is count_text_tokens: tiktoken for the model, else o200k_base, else cl100k_base.
func CountTextTokens(text, model string) int {
	codec, err := tokenizer.ForModel(tokenizer.Model(model))
	if err != nil {
		codec, err = tokenizer.Get(tokenizer.O200kBase)
	}
	if err != nil {
		codec, err = tokenizer.Get(tokenizer.Cl100kBase)
	}
	if err != nil {
		return 0
	}
	count, _ := codec.Count(text)
	return count
}

// ImageSizeFromBytes is image_size_from_bytes.
func ImageSizeFromBytes(data []byte) (int, int, bool) {
	if len(data) == 0 {
		return 0, 0, false
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return 0, 0, false
	}
	return config.Width, config.Height, true
}

var sizeRE = regexp.MustCompile(`(\d{2,5})\D+(\d{2,5})`)

// ParseImageSize is parse_image_size with the 1024x1024 default.
func ParseImageSize(size string) (int, int) {
	match := sizeRE.FindStringSubmatch(size)
	if match == nil {
		return defaultImageWidth, defaultImageHeight
	}
	width, _ := strconv.Atoi(match[1])
	height, _ := strconv.Atoi(match[2])
	if width <= 0 || height <= 0 {
		return defaultImageWidth, defaultImageHeight
	}
	return width, height
}

func patchCount(width, height float64) int {
	return int(math.Ceil(width/patchSize) * math.Ceil(height/patchSize))
}

func patchMultiplier(model string) float64 {
	name := strings.ToLower(strings.TrimSpace(model))
	for _, item := range patchMultipliers {
		if strings.HasPrefix(name, item.prefix) {
			return item.value
		}
	}
	return 1
}

func patchLimits(model, detail string) (int, float64, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range patch1536Models {
		if strings.HasPrefix(name, prefix) {
			return 1536, 2048, true
		}
	}
	if strings.HasPrefix(name, "gpt-5.5") {
		if detail == "auto" || detail == "original" {
			return 10000, 6000, true
		}
		return 2500, 2048, true
	}
	if strings.HasPrefix(name, "gpt-5.4") {
		if detail == "original" {
			return 10000, 6000, true
		}
		return 2500, 2048, true
	}
	return 0, 0, false
}

func patchTokens(width, height int, model, detail string) int {
	multiplier := patchMultiplier(model)
	if detail == "low" {
		return int(math.Ceil(256 * multiplier))
	}
	budget, maxDimension, ok := patchLimits(model, detail)
	if !ok {
		return 0
	}
	scale := math.Min(1, maxDimension/float64(max(width, height)))
	w, h := float64(width)*scale, float64(height)*scale
	if patchCount(w, h) > budget {
		shrink := math.Sqrt(float64(patchSize*patchSize*budget) / (w * h))
		wu, hu := w*shrink/patchSize, h*shrink/patchSize
		fw, fh := 1.0, 1.0
		if wu != 0 {
			fw = math.Floor(wu) / wu
		}
		if hu != 0 {
			fh = math.Floor(hu) / hu
		}
		adjusted := shrink * math.Min(fw, fh)
		w, h = w*adjusted, h*adjusted
	}
	tokens := min(patchCount(math.Max(1, w), math.Max(1, h)), budget)
	return int(math.Ceil(float64(tokens) * multiplier))
}

// CountImageInputTokens is count_image_input_tokens (always the gpt-5.4-mini patch rules).
func CountImageInputTokens(width, height int, detail string) int {
	if width <= 0 || height <= 0 {
		return 0
	}
	detail = strings.ToLower(strings.TrimSpace(detail))
	if detail == "" {
		detail = "auto"
	}
	return patchTokens(width, height, imageInputTokenModel, detail)
}

// CountImageInputsTokens is count_image_inputs_tokens for raw images.
func CountImageInputsTokens(images [][]byte) int {
	total := 0
	for _, data := range images {
		if width, height, ok := ImageSizeFromBytes(data); ok {
			total += CountImageInputTokens(width, height, "auto")
		}
	}
	return total
}

// CountGeneratedImageTokens is count_generated_image_tokens.
func CountGeneratedImageTokens(width, height int, quality string) int {
	patches := float64(patchCount(float64(width), float64(height)))
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "low":
		return int(math.Ceil(patches * 17 / 64))
	case "high", "hd":
		return int(math.Ceil(patches * 65 / 16))
	}
	return int(math.Ceil(patches * 33 / 32))
}

// CountImageOutputItemsTokens is count_image_output_items_tokens: the real image size when b64 is
// present, else the requested size.
func CountImageOutputItemsTokens(items []map[string]any, size, quality string) int {
	fallbackW, fallbackH := ParseImageSize(size)
	total := 0
	for _, item := range items {
		width, height := fallbackW, fallbackH
		if b64, _ := item["b64_json"].(string); strings.TrimSpace(b64) != "" {
			if data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64)); err == nil {
				if w, h, ok := ImageSizeFromBytes(data); ok {
					width, height = w, h
				}
			}
		}
		total += CountGeneratedImageTokens(width, height, quality)
	}
	return total
}

// TokenUsage is token_usage.
func TokenUsage(inputText, inputImage, outputText, outputImage int) map[string]any {
	inputText, inputImage, outputText, outputImage = max(0, inputText), max(0, inputImage), max(0, outputText), max(0, outputImage)
	input, output := inputText+inputImage, outputText+outputImage
	return map[string]any{
		"input_tokens": input, "output_tokens": output, "total_tokens": input + output,
		"input_tokens_details":  map[string]any{"text_tokens": inputText, "image_tokens": inputImage, "cached_tokens": 0},
		"output_tokens_details": map[string]any{"text_tokens": outputText, "image_tokens": outputImage, "reasoning_tokens": 0},
	}
}

// ImageUsage is image_usage.
func ImageUsage(inputText, inputImage, output int) map[string]any {
	return TokenUsage(inputText, inputImage, 0, output)
}

// ChatUsageFromImageUsage is chat_usage_from_image_usage.
func ChatUsageFromImageUsage(usage map[string]any) map[string]any {
	n := func(value any) int { v, _ := value.(int); return v }
	inDetails, _ := usage["input_tokens_details"].(map[string]any)
	outDetails, _ := usage["output_tokens_details"].(map[string]any)
	input, output := n(usage["input_tokens"]), n(usage["output_tokens"])
	return map[string]any{
		"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output,
		"prompt_tokens_details":     map[string]any{"text_tokens": n(inDetails["text_tokens"]), "image_tokens": n(inDetails["image_tokens"]), "cached_tokens": n(inDetails["cached_tokens"])},
		"completion_tokens_details": map[string]any{"text_tokens": n(outDetails["text_tokens"]), "image_tokens": n(outDetails["image_tokens"]), "reasoning_tokens": n(outDetails["reasoning_tokens"])},
	}
}

// CollectImageOutputs is collect_image_outputs.
func CollectImageOutputs(outputs []Output) map[string]any {
	var created int64
	data := []map[string]any{}
	message := ""
	progress := []string{}
	email := ""
	for _, output := range outputs {
		if created == 0 {
			created = output.Created
		}
		if output.AccountEmail != "" && email == "" {
			email = output.AccountEmail
		}
		switch output.Kind {
		case "progress":
			if output.Text != "" {
				progress = append(progress, output.Text)
			}
		case "message":
			message = output.Text
		case "result":
			data = append(data, output.Data...)
		}
	}
	if created == 0 {
		created = nowUnix()
	}
	result := map[string]any{"created": created, "data": data}
	if len(data) == 0 {
		text := message
		if text == "" {
			text = strings.TrimSpace(strings.Join(progress, ""))
		}
		if text != "" {
			result["message"] = text
		}
	}
	if email != "" {
		result["_account_email"] = email
	}
	return result
}
