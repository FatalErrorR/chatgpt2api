package chatgptweb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	http "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

// Editable PPT / PSD export (OpenAIBackendAPI.export_ppt_zip / export_psd_zip): a thinking-model
// conversation builds the file in its sandbox; we poll the conversation for the file and the zip
// of parts, then download both.

const (
	EditableFileModel             = "gpt-5-5-thinking"
	editableThinkingEffort        = "extended"
	editableTimeout               = 1200 * time.Second
	editablePollInterval          = 5 * time.Second
	editableClientVersion         = "prod-bede35f9dcd856d080e012478f0c1031faa2588e"
	editableClientBuildNumber     = "6631702"
	EditablePPTPrompt             = "我需要你根据用户的需求，来制作一个可以编辑的PPT，你可以使用Agent来做，你不要再继续询问用户问题，内容风格、版式、配色、内容结构和页面信息你可以自行补充并直接执行。整体的流程如下：\n1. 用生图的方式，帮我生成一个精美的产品介绍ppt，5-6个页面\n2. 帮我把以上涉及到的所有图像和形状素材拆分成单独png，每个素材单独一张图片，不要有遗漏，让我可以直接在ppt里拼接素材还原，不要文字\n3. 利用以上所有图片和形状素材，帮我还原你第一次生成的展示ppt，我需要是可编辑的ppt格式，主要部分需要你单独还原插入，文字需要可以编辑\n最后只需要给我生成一个PPT文件，以及生成中遇到的各种素材压缩包zip文件就行。"
	EditablePSDPrompt             = "帮我生成这个图像，把这张海报分成若干图像，包括背景图，每个元素不要改位置，这样子我可以直接在 平时里无需拖动，底色为白色，不要伪透明底。再帮我将以上拆分的图像拼合成一个psd文件，去除白色底，不要改变每个图层的相应位置，保留每个元素所在图层的相应位置，保留每个元素的图层，最后只需要给我输出psd文件，以及每个图层的zip文件"
	editableUserSupplementHeading = "\n\n以下是用户补充需求，请直接结合执行：\n"
)

var (
	editableAssetPointerRE = regexp.MustCompile(`(?:file-service|sediment)://([A-Za-z0-9_-]+)`)
	editableFileIDRE       = regexp.MustCompile(`\b(file[-_][A-Za-z0-9_-]+)\b`)
	editablePSDExportRE    = regexp.MustCompile(`(?i)(?:sandbox:)?(/mnt/data/[^\s"'\)\]]+\.(?:psd|zip))`)
	editablePPTExportRE    = regexp.MustCompile(`(?i)(?:sandbox:)?(/mnt/data/[^\s"'\)\]]+\.(?:pptx?|zip))`)
	contentDispositionExt  = regexp.MustCompile(`(?i)filename\*=UTF-8''([^;]+)`)
	contentDispositionName = regexp.MustCompile(`(?i)filename="([^"]+)"`)
	editableZipMimeTypes   = map[string]bool{"application/zip": true, "application/x-zip-compressed": true}
	extensionForMime       = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif",
		"application/pdf": ".pdf", "text/plain": ".txt", "application/zip": ".zip"}
)

// fileIDMatch is FILE_ID_RE without Python's (?!service\b) lookahead.
func fileIDMatch(text string) string {
	for _, match := range editableFileIDRE.FindAllStringSubmatchIndex(text, -1) {
		id := text[match[2]:match[3]]
		if id == "file-service" || id == "file_service" {
			continue
		}
		return id
	}
	return ""
}

func allFileIDs(text string) []string {
	out := []string{}
	for _, match := range editableFileIDRE.FindAllStringSubmatch(text, -1) {
		if match[1] != "file-service" && match[1] != "file_service" {
			out = append(out, match[1])
		}
	}
	return out
}

type editableKind struct {
	label            string
	suffixes         []string
	mimeTypes        map[string]bool
	mimeKeywords     []string
	defaultExtension string
	exportRE         *regexp.Regexp
}

var (
	pptKind = editableKind{label: "ppt", suffixes: []string{".ppt", ".pptx"}, defaultExtension: ".pptx", exportRE: editablePPTExportRE,
		mimeTypes:    map[string]bool{"application/vnd.openxmlformats-officedocument.presentationml.presentation": true, "application/vnd.ms-powerpoint": true},
		mimeKeywords: []string{"presentationml.presentation", "ms-powerpoint"}}
	psdKind = editableKind{label: "psd", suffixes: []string{".psd"}, defaultExtension: ".psd", exportRE: editablePSDExportRE,
		mimeTypes: map[string]bool{"image/vnd.adobe.photoshop": true, "application/vnd.adobe.photoshop": true}, mimeKeywords: []string{"photoshop"}}
)

// EditableResult is EditableFileExportResult.
type EditableResult struct {
	ConversationID string
	PrimaryPath    string
	ZipPath        string
}

type artifact struct {
	AttachmentID, FileID, Name, MimeType, AuthorRole, SandboxPath, MessageID string
	CreateTime                                                               float64
}

func editablePrompt(fixed, user string) string {
	if extra := strings.TrimSpace(user); extra != "" {
		return fixed + editableUserSupplementHeading + extra
	}
	return fixed
}

// ExportEditable runs export_ppt_zip ("ppt") or export_psd_zip ("psd") into outputDir.
func (b *Backend) ExportEditable(ctx context.Context, kind string, images []string, prompt, outputDir string) (EditableResult, error) {
	spec, fixed := pptKind, EditablePPTPrompt
	if kind == "psd" {
		spec, fixed = psdKind, EditablePSDPrompt
		if len(images) == 0 {
			return EditableResult{}, errors.New("base64_images is empty")
		}
	}
	if b.anonymous() {
		return EditableResult{}, errors.New("access_token is required for editable file export")
	}
	prompt = editablePrompt(fixed, prompt)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return EditableResult{}, err
	}
	uploaded := []editableUpload{}
	for i, value := range images {
		item, err := b.uploadEditable(ctx, value, i+1)
		if err != nil {
			return EditableResult{}, err
		}
		uploaded = append(uploaded, item)
	}
	conduit, err := b.prepareEditable(ctx, prompt, uploaded)
	if err != nil {
		return EditableResult{}, err
	}
	conversationID, err := b.runEditable(ctx, prompt, uploaded, conduit)
	if err != nil {
		return EditableResult{}, err
	}
	targets, err := b.waitEditableArtifacts(ctx, conversationID, spec)
	if err != nil {
		return EditableResult{}, err
	}
	result := EditableResult{ConversationID: conversationID}
	downloaded := []string{}
	for _, target := range targets {
		saved, err := b.downloadArtifact(ctx, conversationID, target, outputDir, spec)
		if err != nil {
			return EditableResult{}, err
		}
		downloaded = append(downloaded, saved)
	}
	for _, saved := range downloaded {
		ext := strings.ToLower(filepath.Ext(saved))
		if result.PrimaryPath == "" && containsSuffix(spec.suffixes, ext) {
			result.PrimaryPath = saved
		}
		if result.ZipPath == "" && ext == ".zip" {
			result.ZipPath = saved
		}
	}
	if result.PrimaryPath == "" || result.ZipPath == "" {
		return EditableResult{}, fmt.Errorf("download finished but did not get both %s and zip files: %v", spec.label, downloaded)
	}
	return result, nil
}

func containsSuffix(suffixes []string, ext string) bool {
	for _, suffix := range suffixes {
		if suffix == ext {
			return true
		}
	}
	return false
}

func (b *Backend) editableHeaders(extra map[string]string) map[string]string {
	headers := map[string]string{"OAI-Client-Version": editableClientVersion, "OAI-Client-Build-Number": editableClientBuildNumber}
	for key, value := range extra {
		headers[key] = value
	}
	return headers
}

type editableUpload struct {
	FileID, LibraryFileID, FileName, MimeType string
	FileSize, Width, Height                   int
}

var dataURLRE = regexp.MustCompile(`(?is)^data:([^;]+);base64,(.*)$`)

func (b *Backend) uploadEditable(ctx context.Context, value string, index int) (editableUpload, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return editableUpload{}, errors.New("base64 image is empty")
	}
	payload := raw
	if match := dataURLRE.FindStringSubmatch(raw); match != nil {
		payload = strings.TrimSpace(match[2])
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return editableUpload{}, err
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return editableUpload{}, fmt.Errorf("cannot identify image file: %w", err)
	}
	mimeType := mimeByFormat[format]
	if mimeType == "" {
		mimeType = "image/png"
	}
	ext := extensionForMime[mimeType]
	if ext == "" {
		ext = ".png"
	}
	fileName := fmt.Sprintf("image_%d%s", index, ext)
	meta, err := b.getJSON(ctx, http.MethodPost, "/backend-api/files", map[string]any{
		"file_name": fileName, "file_size": len(data), "use_case": "multimodal", "timezone_offset_min": -480, "reset_rate_limits": false,
		"store_in_library": true, "library_persistence_mode": "opportunistic",
	}, b.editableHeaders(map[string]string{"Accept": "*/*"}), true)
	if err != nil {
		return editableUpload{}, err
	}
	uploadURL, fileID := firstString(meta["upload_url"]), firstString(meta["file_id"])
	if uploadURL == "" || fileID == "" {
		return editableUpload{}, fmt.Errorf("invalid upload response: %v", meta)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return editableUpload{}, err
	}
	for key, value := range map[string]string{"Content-Type": mimeType, "x-ms-blob-type": "BlockBlob", "x-ms-version": "2020-04-08",
		"Origin": b.API.BaseURL(), "Referer": b.API.BaseURL() + "/", "User-Agent": b.API.UserAgent(), "Accept": "application/json, text/plain, */*",
		"Accept-Language": "en-US,en;q=0.8"} {
		request.Header.Set(key, value)
	}
	response, err := b.API.Do(request)
	if err != nil {
		return editableUpload{}, err
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return editableUpload{}, &imagegen.UpstreamHTTPError{Context: "image_upload", StatusCode: response.StatusCode, Body: string(body)}
	}
	uploadedPath := "/backend-api/files/" + fileID + "/uploaded"
	if _, err := b.withChallengeRetry(ctx, func() (*http.Response, error) {
		return b.API.RequestBody(ctx, http.MethodPost, uploadedPath, strings.NewReader("{}"), "application/json", b.editableHeaders(map[string]string{"Accept": "*/*"}))
	}); err != nil {
		return editableUpload{}, err
	}
	return editableUpload{FileID: fileID, LibraryFileID: firstString(meta["library_file_id"]), FileName: fileName, MimeType: mimeType,
		FileSize: len(data), Width: config.Width, Height: config.Height}, nil
}

func (b *Backend) prepareEditable(ctx context.Context, prompt string, uploaded []editableUpload) (string, error) {
	payload := map[string]any{
		"action": "next", "fork_from_shared_post": false, "parent_message_id": clientCreatedRoot, "model": EditableFileModel,
		"client_prepare_state": "success", "timezone_offset_min": -480, "timezone": "Asia/Shanghai",
		"conversation_mode": map[string]any{"kind": "primary_assistant"}, "system_hints": []any{},
		"partial_query":      map[string]any{"id": uuid.NewString(), "author": map[string]any{"role": "user"}, "content": map[string]any{"content_type": "text", "parts": []string{prompt}}},
		"supports_buffering": true, "supported_encodings": []string{"v1"}, "client_contextual_info": map[string]any{"app_name": "chatgpt.com"},
		"thinking_effort": editableThinkingEffort,
	}
	if len(uploaded) > 0 {
		mimes := []string{}
		for _, item := range uploaded {
			mimes = append(mimes, item.MimeType)
		}
		payload["attachment_mime_types"] = mimes
	}
	result, err := b.getJSON(ctx, http.MethodPost, "/backend-api/f/conversation/prepare", payload,
		b.editableHeaders(map[string]string{"Accept": "*/*", "X-Conduit-Token": "no-token"}), true)
	if err != nil {
		return "", err
	}
	conduit := firstString(result["conduit_token"])
	if conduit == "" {
		raw, _ := json.Marshal(result)
		return "", fmt.Errorf("missing conduit_token: %s", raw)
	}
	return conduit, nil
}

func (b *Backend) runEditable(ctx context.Context, prompt string, uploaded []editableUpload, conduit string) (string, error) {
	if err := convertErr(b.API.Bootstrap(ctx)); err != nil {
		return "", err
	}
	if err := convertErr(b.API.ChatRequirements(ctx)); err != nil {
		return "", err
	}
	message := map[string]any{"id": uuid.NewString(), "author": map[string]any{"role": "user"}, "create_time": float64(time.Now().UnixNano()) / 1e9}
	if len(uploaded) > 0 {
		parts, attachments := []any{}, []any{}
		for _, item := range uploaded {
			parts = append(parts, map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "sediment://" + item.FileID,
				"size_bytes": item.FileSize, "width": item.Width, "height": item.Height})
			attachments = append(attachments, map[string]any{"id": item.FileID, "size": item.FileSize, "name": item.FileName, "mime_type": item.MimeType,
				"width": item.Width, "height": item.Height, "source": "library", "library_file_id": item.LibraryFileID, "is_big_paste": false})
		}
		parts = append(parts, prompt)
		message["content"] = map[string]any{"content_type": "multimodal_text", "parts": parts}
		message["metadata"] = map[string]any{"attachments": attachments, "developer_mode_connector_ids": []any{}, "selected_sources": []any{},
			"selected_github_repos": []any{}, "selected_all_github_repos": false, "serialization_metadata": map[string]any{"custom_symbol_offsets": []any{}}}
	} else {
		message["content"] = map[string]any{"content_type": "text", "parts": []string{prompt}}
	}
	payload := map[string]any{
		"action": "next", "messages": []any{message}, "parent_message_id": clientCreatedRoot, "model": EditableFileModel,
		"client_prepare_state": "sent", "timezone_offset_min": -480, "timezone": "Asia/Shanghai",
		"conversation_mode": map[string]any{"kind": "primary_assistant"}, "enable_message_followups": true, "system_hints": []any{},
		"supports_buffering": true, "supported_encodings": []string{"v1"},
		"client_contextual_info": map[string]any{"is_dark_mode": false, "time_since_loaded": 401, "page_height": 1138, "page_width": 803,
			"pixel_ratio": 2, "screen_height": 1440, "screen_width": 2560, "app_name": "chatgpt.com"},
		"paragen_cot_summary_display_override": "allow", "force_parallel_switch": "auto", "thinking_effort": editableThinkingEffort,
	}
	response, err := b.withChallengeRetry(ctx, func() (*http.Response, error) {
		return b.API.Request(ctx, http.MethodPost, "/backend-api/f/conversation", payload,
			b.editableHeaders(map[string]string{"Accept": "text/event-stream", "X-Conduit-Token": conduit}))
	})
	if err != nil {
		return "", err
	}
	payloads, wait, _ := b.capStream(response, 0)
	conversationID := ""
	for item := range payloads {
		if item == "[DONE]" {
			break
		}
		if conversationID == "" {
			conversationID = findSearchValue(item, "conversation_id")
		}
	}
	response.Body.Close()
	go func() {
		for range payloads {
		}
	}()
	wait()
	if conversationID == "" {
		return "", errors.New("conversation_id not found in stream")
	}
	return conversationID, nil
}

func (b *Backend) waitEditableArtifacts(ctx context.Context, conversationID string, spec editableKind) ([]artifact, error) {
	deadline := time.Now().Add(editableTimeout)
	for time.Now().Before(deadline) {
		conversation, err := b.getJSON(ctx, http.MethodGet, "/backend-api/conversation/"+conversationID, nil, b.editableHeaders(map[string]string{
			"Accept": "*/*", "Referer": b.API.BaseURL() + "/c/" + conversationID, "X-OpenAI-Target-Route": "/backend-api/conversation/{conversation_id}"}), false)
		if err != nil {
			var httpErr *imagegen.UpstreamHTTPError
			if !errors.As(err, &httpErr) || !map[int]bool{404: true, 409: true, 423: true, 429: true, 500: true, 502: true, 503: true, 504: true}[httpErr.StatusCode] {
				return nil, err
			}
		} else if targets := pickTargets(extractArtifacts(conversation, spec.exportRE), spec); targets != nil {
			return targets, nil
		}
		b.sleep(ctx, editablePollInterval)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("timed out waiting for %s/zip outputs", spec.label)
}

func editableMessageText(message map[string]any) string {
	parts := []string{}
	switch content := message["content"].(type) {
	case map[string]any:
		if text, ok := content["text"].(string); ok {
			parts = append(parts, text)
		}
		list, _ := content["parts"].([]any)
		for _, part := range list {
			switch p := part.(type) {
			case string:
				parts = append(parts, p)
			case map[string]any:
				for _, key := range []string{"text", "asset_pointer", "model_set_context"} {
					if value, ok := p[key]; ok && value != nil && value != "" {
						parts = append(parts, fmt.Sprint(value))
					}
				}
			}
		}
	case string:
		parts = append(parts, content)
	}
	kept := []string{}
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func exportPaths(payload any, re *regexp.Regexp) []string {
	text, ok := payload.(string)
	if !ok {
		var buf bytes.Buffer
		encoder := json.NewEncoder(&buf)
		encoder.SetEscapeHTML(false)
		encoder.Encode(payload)
		text = buf.String()
	}
	out := []string{}
	for _, match := range re.FindAllStringSubmatch(text, -1) {
		if p := strings.TrimSpace(match[1]); p != "" && !containsSuffix(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func cleanMime(value any) string {
	text := strings.ToLower(strings.TrimSpace(textOf(value)))
	if !strings.Contains(text, "/") {
		return ""
	}
	return strings.SplitN(text, ";", 2)[0]
}

func sanitizeFilename(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(path.Base(strings.ReplaceAll(value, "\\", "/")), "\x00", ""))
}

func artifactFromDict(payload map[string]any, messageID, role string, created float64, re *regexp.Regexp) *artifact {
	relevant := false
	for _, key := range []string{"id", "file_id", "asset_pointer", "name", "file_name", "filename", "mime_type", "mimeType"} {
		if _, ok := payload[key]; ok {
			relevant = true
		}
	}
	if !relevant {
		return nil
	}
	attachmentID := fileIDMatch(textOf(payload["id"]))
	fileID := fileIDMatch(textOf(payload["file_id"]))
	name := sanitizeFilename(firstString(payload["name"], payload["file_name"], payload["filename"], payload["title"]))
	mime := cleanMime(firstNonNil(payload["mime_type"], payload["mimeType"]))
	for _, match := range editableAssetPointerRE.FindAllStringSubmatch(textOf(payload["asset_pointer"]), -1) {
		if attachmentID == "" {
			attachmentID = match[1]
		}
		if fileID == "" {
			fileID = match[1]
		}
	}
	if attachmentID == "" || fileID == "" {
		raw, _ := json.Marshal(payload)
		ids := []string{}
		for _, match := range editableAssetPointerRE.FindAllStringSubmatch(string(raw), -1) {
			ids = append(ids, match[1])
		}
		ids = append(ids, allFileIDs(string(raw))...)
		if len(ids) > 0 {
			if attachmentID == "" {
				attachmentID = ids[0]
			}
			if fileID == "" {
				fileID = ids[0]
			}
		}
	}
	if attachmentID == "" && fileID == "" {
		return nil
	}
	sandbox := ""
	if paths := exportPaths(payload, re); len(paths) > 0 {
		sandbox = paths[0]
	}
	return &artifact{AttachmentID: attachmentID, FileID: fileID, Name: name, MimeType: mime, CreateTime: created, AuthorRole: role, SandboxPath: sandbox, MessageID: messageID}
}

func mergeArtifact(current *artifact, latest artifact) artifact {
	if current == nil {
		return latest
	}
	pick := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	return artifact{AttachmentID: pick(latest.AttachmentID, current.AttachmentID), FileID: pick(latest.FileID, current.FileID),
		Name: pick(latest.Name, current.Name), MimeType: pick(latest.MimeType, current.MimeType), CreateTime: max(current.CreateTime, latest.CreateTime),
		AuthorRole: pick(latest.AuthorRole, current.AuthorRole), SandboxPath: pick(latest.SandboxPath, current.SandboxPath), MessageID: pick(latest.MessageID, current.MessageID)}
}

// extractArtifacts is _extract_editable_artifacts.
func extractArtifacts(conversation map[string]any, re *regexp.Regexp) []artifact {
	mapping, _ := conversation["mapping"].(map[string]any)
	type node struct {
		message map[string]any
		created float64
	}
	nodes := []node{}
	for _, key := range sortedKeys(mapping) {
		n, _ := mapping[key].(map[string]any)
		message, _ := n["message"].(map[string]any)
		if message == nil {
			message = map[string]any{}
		}
		nodes = append(nodes, node{message, numberOf(message["create_time"])})
	}
	sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].created < nodes[j].created })
	found := map[string]artifact{}
	order := []string{}
	add := func(key string, item artifact) {
		if current, ok := found[key]; ok {
			found[key] = mergeArtifact(&current, item)
			return
		}
		found[key] = item
		order = append(order, key)
	}
	for _, n := range nodes {
		author, _ := n.message["author"].(map[string]any)
		role := strings.TrimSpace(textOf(author["role"]))
		if role != "assistant" && role != "tool" {
			continue
		}
		messageID := textOf(n.message["id"])
		candidates := []map[string]any{}
		metadata, _ := n.message["metadata"].(map[string]any)
		attachments, _ := metadata["attachments"].([]any)
		for _, item := range attachments {
			if m, ok := item.(map[string]any); ok {
				candidates = append(candidates, m)
			}
		}
		candidates = append(candidates, walkDicts(n.message)...)
		for _, candidate := range candidates {
			item := artifactFromDict(candidate, messageID, role, n.created, re)
			if item == nil {
				continue
			}
			key := item.AttachmentID
			for _, alt := range []string{item.FileID, item.Name, item.SandboxPath} {
				if key == "" {
					key = alt
				}
			}
			if key != "" {
				add(key, *item)
			}
		}
		for _, exportPath := range exportPaths(editableMessageText(n.message), re) {
			add(exportPath, artifact{Name: path.Base(exportPath), CreateTime: n.created, AuthorRole: role, SandboxPath: exportPath, MessageID: messageID})
		}
	}
	out := []artifact{}
	for _, key := range order {
		out = append(out, found[key])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreateTime < out[j].CreateTime })
	return out
}

func hasAnySuffix(text string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(text, suffix) {
			return true
		}
	}
	return false
}

func looksPrimary(item artifact, spec editableKind) bool {
	p, name, mime := strings.ToLower(item.SandboxPath), strings.ToLower(item.Name), item.MimeType
	if hasAnySuffix(name, spec.suffixes) || hasAnySuffix(p, spec.suffixes) || spec.mimeTypes[mime] {
		return true
	}
	for _, keyword := range spec.mimeKeywords {
		if strings.Contains(mime, keyword) {
			return true
		}
	}
	return false
}

func looksZip(item artifact) bool {
	p, name, mime := strings.ToLower(item.SandboxPath), strings.ToLower(item.Name), item.MimeType
	return strings.HasSuffix(name, ".zip") || strings.HasSuffix(p, ".zip") || editableZipMimeTypes[mime] || strings.HasSuffix(mime, "/zip")
}

// pickTargets is _pick_editable_target_artifacts: the newest primary file and the newest zip.
func pickTargets(items []artifact, spec editableKind) []artifact {
	var primary, zip *artifact
	for i := len(items) - 1; i >= 0; i-- {
		if primary == nil && looksPrimary(items[i], spec) {
			primary = &items[i]
		}
		if zip == nil && looksZip(items[i]) {
			zip = &items[i]
		}
	}
	if primary == nil || zip == nil {
		return nil
	}
	return []artifact{*primary, *zip}
}

func (b *Backend) downloadURLFrom(ctx context.Context, conversationID, p string, route string) string {
	result, err := b.getJSON(ctx, http.MethodGet, p, nil, b.editableHeaders(map[string]string{"Accept": "*/*",
		"Referer": b.API.BaseURL() + "/c/" + conversationID, "X-OpenAI-Target-Route": route}), false)
	if err != nil {
		return ""
	}
	return firstString(result["download_url"], result["url"])
}

func (b *Backend) resolveDownloadURL(ctx context.Context, conversationID string, item artifact) string {
	ids := []string{}
	for _, id := range []string{item.AttachmentID, item.FileID} {
		if id != "" && !containsSuffix(ids, id) {
			ids = append(ids, id)
		}
	}
	if item.SandboxPath != "" && item.MessageID != "" {
		query := url.Values{"message_id": {item.MessageID}, "sandbox_path": {item.SandboxPath}}.Encode()
		if link := b.downloadURLFrom(ctx, conversationID, "/backend-api/conversation/"+conversationID+"/interpreter/download?"+query,
			"/backend-api/conversation/{conversation_id}/interpreter/download"); link != "" {
			return link
		}
	}
	for _, id := range ids {
		if link := b.downloadURLFrom(ctx, conversationID, "/backend-api/conversation/"+conversationID+"/attachment/"+id+"/download",
			"/backend-api/conversation/{conversation_id}/attachment/{attachment_id}/download"); link != "" {
			return link
		}
	}
	for _, id := range ids {
		if link := b.downloadURLFrom(ctx, conversationID, "/backend-api/files/download/"+id+"?post_id=&inline=false", "/backend-api/files/download/{file_id}"); link != "" {
			return link
		}
	}
	for _, id := range ids {
		if link := b.downloadURLFrom(ctx, conversationID, "/backend-api/files/"+id+"/download", "/backend-api/files/download/{file_id}"); link != "" {
			return link
		}
	}
	return ""
}

func extensionFor(mime string, spec editableKind) string {
	if spec.mimeTypes[mime] {
		return spec.defaultExtension
	}
	for _, keyword := range spec.mimeKeywords {
		if strings.Contains(mime, keyword) {
			return spec.defaultExtension
		}
	}
	if editableZipMimeTypes[mime] || strings.HasSuffix(mime, "/zip") {
		return ".zip"
	}
	return extensionForMime[mime]
}

func uniquePath(p string) (string, error) {
	if _, err := os.Stat(p); err != nil {
		return p, nil
	}
	ext := filepath.Ext(p)
	stem := strings.TrimSuffix(p, ext)
	for i := 1; i < 1000; i++ {
		candidate := fmt.Sprintf("%s_%d%s", stem, i, ext)
		if _, err := os.Stat(candidate); err != nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("failed to allocate output path for %s", p)
}

func (b *Backend) downloadArtifact(ctx context.Context, conversationID string, item artifact, outputDir string, spec editableKind) (string, error) {
	link := b.resolveDownloadURL(ctx, conversationID, item)
	if link == "" {
		return "", fmt.Errorf("download url not found for artifact: %+v", item)
	}
	var response *http.Response
	var err error
	if p, ok := strings.CutPrefix(link, b.API.BaseURL()); ok && strings.HasPrefix(p, "/") {
		response, err = b.API.Request(ctx, http.MethodGet, p, nil, b.editableHeaders(nil)) // chatgpt.com links need the account
		err = convertErr(err)
	} else {
		request, rerr := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
		if rerr != nil {
			return "", rerr
		}
		request.Header.Set("User-Agent", b.API.UserAgent())
		response, err = b.API.Do(request)
	}
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", &imagegen.UpstreamHTTPError{Context: "artifact_download", StatusCode: response.StatusCode, Body: string(data)}
	}
	contentType := cleanMime(firstNonNil(response.Header.Get("Content-Type"), item.MimeType))
	name := sanitizeFilename(item.Name)
	if name == "" && item.SandboxPath != "" {
		name = sanitizeFilename(path.Base(item.SandboxPath))
	}
	if name == "" {
		disposition := response.Header.Get("Content-Disposition")
		if match := contentDispositionExt.FindStringSubmatch(disposition); match != nil {
			unescaped, _ := url.PathUnescape(match[1])
			name = sanitizeFilename(unescaped)
		} else if match := contentDispositionName.FindStringSubmatch(disposition); match != nil {
			name = sanitizeFilename(match[1])
		}
	}
	if name == "" {
		if parsed, perr := url.Parse(link); perr == nil {
			name = sanitizeFilename(path.Base(parsed.Path))
		}
	}
	ext := extensionFor(contentType, spec)
	if name == "" || name == "." || name == "/" {
		name = "artifact" + ext
	} else if filepath.Ext(name) == "" {
		name += ext
	}
	target, err := uniquePath(filepath.Join(outputDir, name))
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return "", err
	}
	return target, nil
}
