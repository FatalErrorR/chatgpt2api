package chatgptweb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	http "github.com/bogdanfinn/fhttp"
)

var contentPolicyKeywords = []string{
	"内容政策", "防护限制", "违反", "moderation", "policy", "blocked",
	"不能生成", "无法生成", "不能帮助", "无法帮助",
	"裸体", "裸露", "色情", "性内容", "未成年",
	"抱歉，我不能",
}

// IsContentPolicyError is _is_content_policy_error.
func IsContentPolicyError(message string) bool {
	if message == "" {
		return false
	}
	lower := strings.ToLower(message)
	for _, keyword := range contentPolicyKeywords {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

var (
	fileServiceRE = regexp.MustCompile(`file-service://([A-Za-z0-9_-]+)`)
	realFileRE    = regexp.MustCompile(`\bfile_00000000[a-f0-9]{24}\b`)
	sedimentRE    = regexp.MustCompile(`sediment://([A-Za-z0-9_-]+)`)
)

func addUnique(values []string, candidates ...string) []string {
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		dup := false
		for _, value := range values {
			if value == candidate {
				dup = true
				break
			}
		}
		if !dup {
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

// extractReferenceIDs is _extract_image_reference_ids: every id in every string of the payload.
// Map values are walked in key order for a stable result.
func extractReferenceIDs(payload any) ([]string, []string) {
	files, sediments := []string{}, []string{}
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case string:
			files = addUnique(files, submatches(fileServiceRE, v)...)
			files = addUnique(files, realFileRE.FindAllString(v, -1)...)
			sediments = addUnique(sediments, submatches(sedimentRE, v)...)
		case map[string]any:
			keys := make([]string, 0, len(v))
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				walk(v[key])
			}
		case []any:
			for _, item := range v {
				walk(item)
			}
		}
	}
	walk(payload)
	return files, sediments
}

func hasImageAssetPointer(payload any) bool {
	switch v := payload.(type) {
	case map[string]any:
		if v["content_type"] == "image_asset_pointer" {
			return true
		}
		if pointer, _ := v["asset_pointer"].(string); strings.HasPrefix(pointer, "file-service://") || strings.HasPrefix(pointer, "sediment://") {
			return true
		}
		for _, item := range v {
			if hasImageAssetPointer(item) {
				return true
			}
		}
	case []any:
		for _, item := range v {
			if hasImageAssetPointer(item) {
				return true
			}
		}
	}
	return false
}

type toolRecord struct {
	createTime float64
	files      []string
	sediments  []string
}

// imageToolRecords is _extract_image_tool_records, sorted by create time.
func imageToolRecords(conversation map[string]any) []toolRecord {
	mapping, _ := conversation["mapping"].(map[string]any)
	ids := make([]string, 0, len(mapping))
	for id := range mapping {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	records := []toolRecord{}
	for _, id := range ids {
		node, _ := mapping[id].(map[string]any)
		message, _ := node["message"].(map[string]any)
		author, _ := message["author"].(map[string]any)
		metadata, _ := message["metadata"].(map[string]any)
		content := message["content"]
		role := strings.ToLower(strings.TrimSpace(fmt.Sprint(author["role"])))
		if role != "tool" && role != "assistant" {
			continue
		}
		isImageGen := metadata["async_task_type"] == "image_gen"
		hasPointer := hasImageAssetPointer(content) || hasImageAssetPointer(metadata)
		if role == "assistant" && !(isImageGen || hasPointer) {
			continue
		}
		files, sediments := extractReferenceIDs(map[string]any{"content": content, "metadata": metadata})
		if !isImageGen && !hasPointer && len(files) == 0 && len(sediments) == 0 {
			continue
		}
		records = append(records, toolRecord{createTime: numberOf(message["create_time"]), files: files, sediments: sediments})
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].createTime < records[j].createTime })
	return records
}

// contentPolicyErrorInConversation is _find_content_policy_error_in_conversation.
func contentPolicyErrorInConversation(conversation map[string]any) string {
	mapping, _ := conversation["mapping"].(map[string]any)
	for _, node := range mapping {
		message, _ := node.(map[string]any)["message"].(map[string]any)
		author, _ := message["author"].(map[string]any)
		role := strings.ToLower(strings.TrimSpace(fmt.Sprint(author["role"])))
		if role != "assistant" && role != "tool" {
			continue
		}
		texts := []string{}
		switch content := message["content"].(type) {
		case map[string]any:
			if parts, ok := content["parts"].([]any); ok {
				for _, part := range parts {
					if s, ok := part.(string); ok && strings.TrimSpace(s) != "" {
						texts = append(texts, strings.TrimSpace(s))
					}
				}
			}
			if text, _ := content["text"].(string); strings.TrimSpace(text) != "" {
				texts = append(texts, strings.TrimSpace(text))
			}
		case string:
			if strings.TrimSpace(content) != "" {
				texts = append(texts, strings.TrimSpace(content))
			}
		}
		text := strings.Join(texts, "\n")
		if text != "" && IsContentPolicyError(text) {
			if runes := []rune(text); len(runes) > 500 {
				return string(runes[:500])
			}
			return text
		}
	}
	return ""
}

func formatSecs(value float64) string {
	if value == math.Trunc(value) {
		return strconv.Itoa(int(value))
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func equalLists(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// PollImageResults is _poll_image_results.
func (b *Backend) PollImageResults(ctx context.Context, conversationID string, timeoutSecs float64, initialFiles, initialSediments []string) ([]string, []string, error) {
	start := time.Now()
	interval := b.Settings.ImagePollIntervalSecs()
	initialWait := b.Settings.ImagePollInitialWaitSecs()
	files := addUnique(nil, initialFiles...)
	sediments := addUnique(nil, initialSediments...)
	hasInitial := len(files) > 0 || len(sediments) > 0
	var lastFiles, lastSediments []string
	haveLast := hasInitial
	if hasInitial {
		lastFiles, lastSediments = append([]string(nil), files...), append([]string(nil), sediments...)
	}
	remaining := func() float64 { return timeoutSecs - time.Since(start).Seconds() }
	sleepUpTo := func(d float64) {
		if d = math.Min(d, math.Max(0, remaining())); d > 0 {
			b.sleep(ctx, time.Duration(d*float64(time.Second)))
		}
	}
	if hasInitial && b.Settings.ImageSettleEnabled() {
		sleepUpTo(b.Settings.ImageSettleSecs())
	} else if initialWait > 0 {
		sleepUpTo(initialWait + jitter(math.Min(2, initialWait*0.2)).Seconds())
	}
	attempt := 0
	retrySleep := func(retryAfter int) bool {
		base := math.Min(math.Pow(2, math.Min(float64(attempt), 4)), 16)
		if retryAfter >= 0 {
			base = float64(retryAfter)
		}
		if remaining() <= 0 {
			return false
		}
		sleepUpTo(base + jitter(0.5).Seconds())
		return true
	}
	lastTaskError := ""
	for remaining() > 0 && ctx.Err() == nil {
		attempt++
		lastTaskError = ""
		if tasks, err := b.QueryTasks(ctx, conversationID, 5*time.Second); err == nil {
			for _, task := range tasks {
				if isError, message := b.CheckTaskError(task); isError && message != "" {
					lastTaskError = message
				}
			}
		}
		conversation, err := b.getConversation(ctx, conversationID)
		if err != nil {
			var httpErr *imagegen.UpstreamHTTPError
			if errors.As(err, &httpErr) {
				switch httpErr.StatusCode {
				case 429, 500, 502, 503, 504:
					retryAfter := -1
					if httpErr.RetryAfter > 0 {
						retryAfter = httpErr.RetryAfter
					}
					if retrySleep(retryAfter) {
						continue
					}
					goto timeout
				}
				return nil, nil, err
			}
			if retrySleep(-1) { // network error
				continue
			}
			break
		}
		for _, record := range imageToolRecords(conversation) {
			files = addUnique(files, record.files...)
			sediments = addUnique(sediments, record.sediments...)
		}
		if len(files) == 0 && len(sediments) == 0 {
			if policy := contentPolicyErrorInConversation(conversation); policy != "" {
				return nil, nil, &imagegen.ContentPolicyError{Message: policy, ConversationID: conversationID}
			}
		}
		if len(files) > 0 || len(sediments) > 0 {
			if !b.Settings.ImageCheckBeforeHitEnabled() {
				return files, sediments, nil
			}
			if haveLast && equalLists(lastFiles, files) && equalLists(lastSediments, sediments) {
				return files, sediments, nil
			}
			lastFiles, lastSediments, haveLast = append([]string(nil), files...), append([]string(nil), sediments...), true
			if !b.Settings.ImageSettleEnabled() {
				return files, sediments, nil
			}
			if wait := math.Min(b.Settings.ImageSettleSecs(), math.Max(0, remaining())); wait > 0 {
				b.sleep(ctx, time.Duration(wait*float64(time.Second)))
				continue
			}
			return files, sediments, nil
		}
		sleepUpTo(interval)
	}
timeout:
	return nil, nil, &pollTimeout{
		PollTimeoutError: imagegen.PollTimeoutError{
			Message: fmt.Sprintf("ChatGPT 生图超时（已等待 %s 秒）。当前超时阈值可在 config.json 中调大 image_poll_timeout_secs，也可能是账号被限流或生图队列拥堵导致。",
				formatSecs(timeoutSecs)),
			ConversationID: conversationID,
		},
		taskError: lastTaskError,
	}
}

// pollTimeout carries the last task error with the timeout (the Python task_error attribute).
type pollTimeout struct {
	imagegen.PollTimeoutError
	taskError string
}

func (p *pollTimeout) Unwrap() error { return &p.PollTimeoutError }

func (b *Backend) downloadURL(ctx context.Context, path string) (string, error) {
	data, err := b.getJSON(ctx, http.MethodGet, path, nil, map[string]string{"Accept": "application/json"}, false)
	if err != nil {
		return "", err
	}
	return firstString(data["download_url"], data["url"]), nil
}

// resolveURLs is _resolve_image_urls: file ids, then sediment ids, to download URLs (errors skipped).
func (b *Backend) resolveURLs(ctx context.Context, conversationID string, files, sediments []string) []string {
	urls := []string{}
	for _, id := range files {
		if id == "file_upload" {
			continue
		}
		if link, err := b.downloadURL(ctx, "/backend-api/files/"+id+"/download"); err == nil && link != "" {
			urls = addUnique(urls, link)
		}
	}
	if conversationID == "" || len(sediments) == 0 {
		return urls
	}
	for _, id := range sediments {
		if link, err := b.downloadURL(ctx, "/backend-api/conversation/"+conversationID+"/attachment/"+id+"/download"); err == nil && link != "" {
			urls = addUnique(urls, link)
		}
	}
	return urls
}

// ResolveImageURLs is resolve_conversation_image_urls.
func (b *Backend) ResolveImageURLs(ctx context.Context, conversationID string, files, sediments []string, pollTimeoutSecs float64, poll bool) ([]string, error) {
	kept := []string{}
	for _, id := range files {
		if id != "file_upload" {
			kept = append(kept, id)
		}
	}
	files = kept
	sediments = append([]string(nil), sediments...)
	timeout := pollTimeoutSecs
	if timeout <= 0 {
		timeout = float64(b.Settings.ImagePollTimeoutSecs())
	}
	if poll && conversationID != "" && (len(files) > 0 || len(sediments) > 0) &&
		!b.Settings.ImageCheckBeforeHitEnabled() && !b.Settings.ImageSettleEnabled() {
		return b.resolveURLs(ctx, conversationID, files, sediments), nil
	}
	if poll && conversationID != "" {
		polledFiles, polledSediments, err := b.PollImageResults(ctx, conversationID, timeout, files, sediments)
		if err != nil {
			var timedOut *pollTimeout
			if errors.As(err, &timedOut) && len(files) == 0 && len(sediments) == 0 {
				if timedOut.taskError != "" {
					return nil, &imagegen.ContentPolicyError{Message: timedOut.taskError, ConversationID: conversationID}
				}
				return nil, &timedOut.PollTimeoutError
			}
			if len(files) == 0 && len(sediments) == 0 {
				return nil, err
			}
		} else {
			files = addUnique(files, polledFiles...)
			sediments = addUnique(sediments, polledSediments...)
		}
	}
	return b.resolveURLs(ctx, conversationID, files, sediments), nil
}

// DownloadImages is download_image_bytes (duplicates dropped).
func (b *Backend) DownloadImages(ctx context.Context, urls []string) ([][]byte, error) {
	images := [][]byte{}
	for _, link := range urls {
		data, err := b.download(ctx, link)
		if err != nil {
			return nil, err
		}
		dup := false
		for _, image := range images {
			if string(image) == string(data) {
				dup = true
				break
			}
		}
		if !dup {
			images = append(images, data)
		}
	}
	return images, nil
}

// download is _download_image: retry 429/5xx and connection errors after 1, 2, 4 seconds
// (Retry-After up to 10 s), then one last try.
func (b *Backend) download(ctx context.Context, link string) ([]byte, error) {
	get := func() (int, []byte, http.Header, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
		if err != nil {
			return 0, nil, nil, err
		}
		response, err := b.API.Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		return response.StatusCode, data, response.Header, err
	}
	for attempt := 1; attempt < downloadAttempts; attempt++ {
		wait := time.Duration(math.Pow(2, float64(attempt-1)) * float64(time.Second))
		status, data, header, err := get()
		if err == nil {
			if !downloadRetryStatuses[status] {
				if status < 200 || status >= 300 {
					return nil, &imagegen.UpstreamHTTPError{Context: "image_download", StatusCode: status, Body: string(data)}
				}
				return data, nil
			}
			if retryAfter := strings.TrimSpace(header.Get("Retry-After")); retryAfter != "" {
				if seconds, convErr := strconv.Atoi(retryAfter); convErr == nil {
					wait = time.Duration(math.Min(float64(seconds), 10) * float64(time.Second))
				}
			}
		}
		b.sleep(ctx, wait)
	}
	status, data, _, err := get()
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, &imagegen.UpstreamHTTPError{Context: "image_download", StatusCode: status, Body: string(data)}
	}
	return data, nil
}

// Close releases nothing: the transport is shared per proxy.
func (b *Backend) Close() {}

// ImageGenerationSettings maps a size to the web aspect-ratio setting (main d515317).
func ImageGenerationSettings(size string) map[string]any {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(size)), "x")
	if len(parts) != 2 {
		return nil
	}
	width, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
	height, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errW != nil || errH != nil || width <= 0 || height <= 0 {
		return nil
	}
	ratio := float64(width) / float64(height)
	options := []struct {
		label string
		value float64
	}{{"1:1", 1}, {"4:3", 4.0 / 3}, {"3:4", 3.0 / 4}, {"16:9", 16.0 / 9}, {"9:16", 9.0 / 16}}
	best, bestDiff := "", math.Inf(1)
	for _, option := range options {
		if diff := math.Abs(math.Log(ratio / option.value)); diff < bestDiff {
			best, bestDiff = option.label, diff
		}
	}
	return map[string]any{"aspect_ratio": best, "image_count": 1}
}
