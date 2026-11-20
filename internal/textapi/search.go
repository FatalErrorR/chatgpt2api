package textapi

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// web_search_tool.py and openai_search.py.

var (
	webSearchToolTypes       = map[string]bool{"web_search": true, "web_search_preview": true, "web_search_preview_2025_03_11": true}
	searchChatModelPrefixes  = []string{"gpt-4o-search-preview", "gpt-4o-mini-search-preview", "gpt-5-search-api"}
	searchAnnotationRE       = regexp.MustCompile("([^]*)")
	searchTrailingAnnotation = regexp.MustCompile("[^]*$")
	spaceBeforePunctRE       = regexp.MustCompile(`\s+([.,;:!?])`)
	digitsRE                 = regexp.MustCompile(`^\d+$`)
)

func toolType(tool any) string {
	if m, ok := tool.(map[string]any); ok {
		return strings.TrimSpace(pyStr(m["type"]))
	}
	return ""
}

func hasWebSearchTool(body map[string]any) bool {
	if tools, ok := body["tools"].([]any); ok {
		for _, tool := range tools {
			if webSearchToolTypes[toolType(tool)] {
				return true
			}
		}
		return false
	}
	return webSearchToolTypes[toolType(body["tool_choice"])]
}

func isWebSearchChatRequest(body map[string]any) bool {
	if hasWebSearchTool(body) {
		return true
	}
	if _, ok := body["web_search_options"].(map[string]any); ok {
		return true
	}
	model := strings.TrimSpace(pyStr(body["model"]))
	for _, prefix := range searchChatModelPrefixes {
		if model == prefix || strings.HasPrefix(model, prefix+"-") {
			return true
		}
	}
	return false
}

func hasUnsupportedTools(body map[string]any, allowed map[string]bool) bool {
	tools, ok := body["tools"].([]any)
	if !ok {
		return false
	}
	for _, tool := range tools {
		if _, isMap := tool.(map[string]any); isMap && !allowed[toolType(tool)] {
			return true
		}
	}
	return false
}

// searchMessageText is web_search_tool.message_text.
func searchMessageText(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		parts := []string{}
		for _, item := range v {
			text := ""
			switch p := item.(type) {
			case string:
				text = strings.TrimSpace(p)
			case map[string]any:
				text = strings.TrimSpace(pyStr(firstTruthy(p["text"], p["input_text"])))
			}
			if text != "" {
				parts = append(parts, text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

func searchQueryFromMessages(messages []map[string]any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.ToLower(strings.TrimSpace(pyStr(messages[i]["role"]))) != "user" {
			continue
		}
		if text := searchMessageText(messages[i]["content"]); text != "" {
			return text
		}
	}
	return ""
}

func readableSearchPart(parts []string) string {
	for _, part := range parts {
		value := strings.TrimSpace(part)
		lower := strings.ToLower(value)
		if value != "" && !(strings.HasPrefix(lower, "turn") || strings.HasPrefix(lower, "source") || digitsRE.MatchString(value)) {
			return value
		}
	}
	return ""
}

func cleanSearchText(text string) string {
	text = searchAnnotationRE.ReplaceAllStringFunc(text, func(match string) string {
		inner := searchAnnotationRE.FindStringSubmatch(match)[1]
		parts := strings.Split(inner, "")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		kind, data := strings.ToLower(parts[0]), parts[1:]
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
		return readableSearchPart(data)
	})
	text = searchTrailingAnnotation.ReplaceAllString(text, "")
	return strings.TrimSpace(spaceBeforePunctRE.ReplaceAllString(text, "$1"))
}

func normalizedSources(result map[string]any) []map[string]string {
	out := []map[string]string{}
	seen := map[string]bool{}
	var list []any
	switch v := result["sources"].(type) {
	case []any:
		list = v
	case []map[string]any:
		for _, item := range v {
			list = append(list, item)
		}
	default:
		return out
	}
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		url := strings.TrimSpace(pyStr(item["url"]))
		if url == "" || seen[url] {
			continue
		}
		seen[url] = true
		out = append(out, map[string]string{"title": strings.TrimSpace(pyStr(item["title"])), "url": url, "snippet": strings.TrimSpace(pyStr(item["snippet"]))})
	}
	return out
}

// pyLen is len() of a Python str: code points.
func pyLen(text string) int { return len([]rune(text)) }

// textWithURLCitations is text_with_url_citations.
func textWithURLCitations(result map[string]any) (string, []map[string]any) {
	text := cleanSearchText(pyStr(result["answer"]))
	annotations := []map[string]any{}
	sources := normalizedSources(result)
	if len(sources) > 0 {
		text = strings.TrimRight(text, " \t\n\r\f\v")
		if text != "" {
			text += "\n\n"
		}
		text += "Sources:\n"
		for i, source := range sources {
			title := source["title"]
			if title == "" {
				title = source["url"]
			}
			text += strings.Join([]string{strconv.Itoa(i + 1), ". ", title}, "")
			if source["url"] != "" {
				if source["title"] != "" {
					text += " - "
				}
				start := pyLen(text)
				text += source["url"]
				annotationTitle := source["title"]
				if annotationTitle == "" {
					annotationTitle = source["url"]
				}
				annotations = append(annotations, map[string]any{"type": "url_citation", "start_index": start, "end_index": pyLen(text),
					"url": source["url"], "title": annotationTitle})
			}
			text += "\n"
		}
	}
	return strings.TrimSpace(text), annotations
}

// runWebSearch is run_web_search.
func (d *Deps) runWebSearch(ctx context.Context, query string) (map[string]any, error) {
	token, err := d.Pool.GetTextAccessToken(ctx, nil, "auto", nil)
	if err != nil {
		return nil, err
	}
	backend, err := d.NewBackend(token)
	if err != nil {
		return nil, err
	}
	result, err := backend.Search(ctx, query)
	if err != nil {
		return nil, err
	}
	d.Pool.MarkTextUsed(token)
	return result, nil
}

// Search is openai_search.handle (/v1/search).
func (d *Deps) Search(ctx context.Context, prompt string) (map[string]any, error) {
	token, err := d.Pool.GetTextAccessToken(ctx, nil, "auto", nil)
	if err != nil {
		return nil, err
	}
	account := d.Pool.GetAccount(token)
	backend, err := d.NewBackend(token)
	if err != nil {
		return nil, err
	}
	result, err := backend.Search(ctx, prompt)
	if err != nil {
		return nil, err
	}
	d.Pool.MarkTextUsed(token)
	result["_account_email"] = pyStr(account["email"])
	return result, nil
}
