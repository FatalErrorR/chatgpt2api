package imagegen

import (
	"sort"
	"strings"
)

var (
	webImageModels = set("gpt-image-2", "gpt-image-2.5", "gpt-image-2.5-flare", "gpt-image-2.5-sunburst", "gpt-image-2.6-flare", "gpt-image-2.6-sunburst")
	// CodexToolModels maps a Codex model to its image tool model.
	CodexToolModels = map[string]string{
		"codex-gpt-image-2": "gpt-image-2", "codex-gpt-image-2.5": "gpt-image-2.5-flare", "codex-gpt-image-2.5-flare": "gpt-image-2.5-flare",
		"codex-gpt-image-2.5-sunburst": "gpt-image-2.5-sunburst", "codex-gpt-image-2.6": "gpt-image-2.6-flare",
		"codex-gpt-image-2.6-flare": "gpt-image-2.6-flare", "codex-gpt-image-2.6-sunburst": "gpt-image-2.6-sunburst",
	}
	imageModelPlanTypes = []string{"plus", "team", "pro"}
	codexImageModels    = map[string]bool{}
	baseImageModels     = map[string]bool{}
)

func init() {
	for model := range CodexToolModels {
		codexImageModels[model] = true
	}
	for model := range webImageModels {
		baseImageModels[model] = true
	}
	for model := range codexImageModels {
		baseImageModels[model] = true
	}
}

func set(values ...string) map[string]bool {
	out := map[string]bool{}
	for _, value := range values {
		out[value] = true
	}
	return out
}

// SplitImageModel is split_image_model: (plan prefix, base model), "" when not an image model.
func SplitImageModel(model string) (string, string) {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if normalized == "" {
		return "", ""
	}
	if baseImageModels[normalized] {
		return "", normalized
	}
	for _, plan := range imageModelPlanTypes {
		if base, ok := strings.CutPrefix(normalized, plan+"-"); ok && codexImageModels[base] {
			return plan, base
		}
	}
	return "", ""
}

// IsSupportedImageModel reports any known image model.
func IsSupportedImageModel(model string) bool { _, base := SplitImageModel(model); return base != "" }

// IsCodexImageModel reports a Codex image model.
func IsCodexImageModel(model string) bool {
	_, base := SplitImageModel(model)
	return codexImageModels[base]
}

// ImageModels lists every accepted model name, sorted (IMAGE_MODELS).
func ImageModels() []string {
	out := []string{}
	for model := range baseImageModels {
		out = append(out, model)
	}
	for _, plan := range imageModelPlanTypes {
		for model := range codexImageModels {
			out = append(out, plan+"-"+model)
		}
	}
	sort.Strings(out)
	return out
}
