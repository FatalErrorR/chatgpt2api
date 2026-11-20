package textapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

// Ported from test_chat_completion_cache.py, test_text_model_routing.py, test_model_catalog_service.py,
// test_v1_messages.py and test_v1_responses.py (text parts).

var png1x1, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADUlEQVR4nGP8z8BQDwAFgwJ/luzl4wAAAABJRU5ErkJggg==")

type keepRemote struct{}

func (keepRemote) UserInfo(context.Context, pool.Account) (pool.Account, error) {
	return pool.Account{}, nil
}
func (keepRemote) RefreshAccessToken(context.Context, string, pool.Account) (map[string]string, error) {
	return nil, errors.New("no refresh token")
}

// fakeBackend answers every conversation with the scripted reply and records what it got.
type fakeBackend struct {
	h     *harness
	token string
}

type textCall struct {
	token, model, effort string
	messages             []map[string]any
}

type harness struct {
	deps      *Deps
	mu        sync.Mutex
	calls     []textCall
	reply     func(call textCall) []string // SSE payloads
	failToken map[string]error
	searches  []string
	search    map[string]any
	models    map[string]any // token -> map[string]any list result or error
	modelHits []string
}

func (b *fakeBackend) StreamText(ctx context.Context, messages []map[string]any, model, effort string) (<-chan string, func() error, error) {
	call := textCall{b.token, model, effort, messages}
	b.h.mu.Lock()
	b.h.calls = append(b.h.calls, call)
	fail := b.h.failToken[b.token]
	b.h.mu.Unlock()
	if fail != nil {
		return nil, nil, fail
	}
	payloads := []string{finalMessage("ok")}
	if b.h.reply != nil {
		payloads = b.h.reply(call)
	}
	ch := make(chan string, len(payloads)+1)
	for _, payload := range payloads {
		ch <- payload
	}
	close(ch)
	return ch, func() error { return nil }, nil
}

func (b *fakeBackend) ListModels(context.Context) (map[string]any, error) {
	b.h.mu.Lock()
	defer b.h.mu.Unlock()
	b.h.modelHits = append(b.h.modelHits, b.token)
	switch v := b.h.models[b.token].(type) {
	case error:
		return nil, v
	case map[string]any:
		return v, nil
	}
	return modelList(), nil
}

func (b *fakeBackend) Search(_ context.Context, prompt string) (map[string]any, error) {
	b.h.mu.Lock()
	defer b.h.mu.Unlock()
	b.h.searches = append(b.h.searches, prompt)
	return deepCopy(b.h.search).(map[string]any), nil
}

func finalMessage(text string) string {
	raw, _ := json.Marshal(map[string]any{"message": map[string]any{"author": map[string]any{"role": "assistant"}, "channel": "final",
		"content": map[string]any{"content_type": "text", "parts": []any{text}}, "recipient": "all"}})
	return string(raw)
}

func modelList(ids ...string) map[string]any {
	data := []map[string]any{}
	for _, id := range ids {
		data = append(data, map[string]any{"id": id, "object": "model", "created": 0, "owned_by": "chatgpt", "permission": []any{}, "root": id, "parent": nil})
	}
	return map[string]any{"object": "list", "data": data}
}

func newHarness(t *testing.T, accounts []pool.Account, config map[string]any) *harness {
	t.Helper()
	repo, err := repository.Open("sqlite:///" + filepath.Join(t.TempDir(), "text.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := pool.Migrate(repo.DB()); err != nil {
		t.Fatal(err)
	}
	if config == nil {
		config = map[string]any{}
	}
	if _, ok := config["chat_completion_cache"]; !ok {
		config["chat_completion_cache"] = map[string]any{"enabled": true, "ttl_seconds": 60, "max_entries": 32, "dedupe_inflight": true,
			"stream_cache": true, "normalize_messages": true, "drop_adjacent_duplicates": true, "drop_assistant_history": false}
	}
	config["auto_remove_invalid_accounts"] = false
	store := settings.NewForTest(config, "")
	accountPool, err := pool.New(pool.NewDBStore(repo.DB()), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	accountPool.Remote = keepRemote{}
	t.Cleanup(accountPool.Flush) // account writes are asynchronous; finish them before the database closes
	if accounts == nil {
		accounts = []pool.Account{{"access_token": "tok-1", "type": "Plus", "status": "正常", "email": "a@test"}}
	}
	accountPool.AddAccountItems(accounts)
	h := &harness{failToken: map[string]error{}, models: map[string]any{}}
	h.deps = &Deps{Settings: store, Pool: accountPool, Cache: NewCache(store),
		NewBackend: func(token string) (Backend, error) { return &fakeBackend{h: h, token: token}, nil }}
	h.deps.Catalog = &Catalog{Deps: h.deps}
	return h
}

func (h *harness) chat(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	result, err := h.deps.ChatCompletions(context.Background(), body)
	if err != nil {
		t.Fatal(err)
	}
	return result.JSON
}

func collectStream(t *testing.T, result Result, err error) []map[string]any {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	out := []map[string]any{}
	if err := result.Stream(context.Background(), func(chunk map[string]any) error { out = append(out, chunk); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func chatContent(response map[string]any) string {
	choice := response["choices"].([]any)[0].(map[string]any)
	return choice["message"].(map[string]any)["content"].(string)
}

func list(values ...any) []any { return values }

func userMessage(text string) map[string]any { return map[string]any{"role": "user", "content": text} }

// ---- cache ----

func TestRepeatedNonStreamTextCompletionUsesCache(t *testing.T) {
	h := newHarness(t, nil, nil)
	n := 0
	h.reply = func(textCall) []string { n++; return []string{finalMessage("cached answer " + string(rune('0'+n)))} }
	body := map[string]any{"model": "auto", "messages": list(userMessage("cache this exact prompt"))}
	first, second := h.chat(t, body), h.chat(t, body)
	if len(h.calls) != 1 || chatContent(first) != chatContent(second) {
		t.Fatalf("calls %d: %q vs %q", len(h.calls), chatContent(first), chatContent(second))
	}
}

func TestCacheKeyDistinguishesThinkingEffortInputs(t *testing.T) {
	messages := []map[string]any{userMessage("same prompt")}
	base := map[string]any{"model": "auto", "messages": messages}
	withEffort := map[string]any{"model": "auto", "thinking_effort": "high"}
	withReasoning := map[string]any{"model": "auto", "reasoning": map[string]any{"effort": "high"}}
	a, b, c := CacheKey(base, messages, false), CacheKey(withEffort, messages, false), CacheKey(withReasoning, messages, false)
	if a == b || a == c || b == c {
		t.Fatal("keys collide")
	}
}

func TestReasoningEffortReachesConversation(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.chat(t, map[string]any{"model": "auto", "reasoning_effort": "xhigh", "messages": list(userMessage("use more reasoning"))})
	if _, err := h.deps.Responses(context.Background(), map[string]any{"model": "auto", "input": "use more", "reasoning": map[string]any{"effort": "xhigh"}}); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 2 || h.calls[0].effort != "extended" || h.calls[1].effort != "extended" {
		t.Fatalf("calls %+v", h.calls)
	}
}

func TestRepeatedStreamTextCompletionReplaysCachedChunks(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.reply = func(textCall) []string {
		return []string{`{"p":"/message/content/parts/0","o":"append","v":"streamed"}`, `{"p":"/message/content/parts/0","o":"append","v":" answer"}`, "[DONE]"}
	}
	body := map[string]any{"model": "auto", "stream": true, "messages": list(userMessage("stream cache"))}
	first, err := h.deps.ChatCompletions(context.Background(), body)
	firstChunks := collectStream(t, first, err)
	second, err := h.deps.ChatCompletions(context.Background(), body)
	secondChunks := collectStream(t, second, err)
	if len(h.calls) != 1 || !reflect.DeepEqual(firstChunks, secondChunks) {
		t.Fatalf("calls %d", len(h.calls))
	}
	content := ""
	for _, chunk := range secondChunks {
		content += pyStr(chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"])
	}
	if content != "streamed answer" {
		t.Fatalf("content %q", content)
	}
}

func TestAdjacentDuplicateMessagesAreRemovedBeforeUpstreamCall(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.chat(t, map[string]any{"model": "auto", "messages": list(userMessage("repeat me"), userMessage("repeat me"),
		map[string]any{"role": "assistant", "content": "old answer"}, userMessage("next prompt"))})
	want := []map[string]any{userMessage("repeat me"), {"role": "assistant", "content": "old answer"}, userMessage("next prompt")}
	if !reflect.DeepEqual(h.calls[0].messages, want) {
		t.Fatalf("messages %v", h.calls[0].messages)
	}
}

func TestUsageIncludesCachedAndReasoningTokens(t *testing.T) {
	h := newHarness(t, nil, nil)
	usage := h.chat(t, map[string]any{"model": "auto", "messages": list(userMessage("usage shape"))})["usage"].(map[string]any)
	if usage["prompt_tokens_details"].(map[string]any)["cached_tokens"] != 0 || usage["completion_tokens_details"].(map[string]any)["reasoning_tokens"] != 0 {
		t.Fatalf("usage %v", usage)
	}
	result, err := h.deps.Responses(context.Background(), map[string]any{"model": "auto", "input": "usage shape"})
	if err != nil {
		t.Fatal(err)
	}
	usage = result.JSON["usage"].(map[string]any)
	if usage["input_tokens_details"].(map[string]any)["cached_tokens"] != 0 || usage["output_tokens_details"].(map[string]any)["reasoning_tokens"] != 0 {
		t.Fatalf("usage %v", usage)
	}
}

func TestRepeatedResponsesTextRequestUsesCache(t *testing.T) {
	h := newHarness(t, nil, nil)
	body := map[string]any{"model": "auto", "input": "cache this responses prompt", "stream": true}
	first, err := h.deps.Responses(context.Background(), body)
	a := collectStream(t, first, err)
	second, err := h.deps.Responses(context.Background(), body)
	b := collectStream(t, second, err)
	if len(h.calls) != 1 || !reflect.DeepEqual(a, b) {
		t.Fatalf("calls %d", len(h.calls))
	}
}

func TestChatAndResponsesNeverShareACacheEntry(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.chat(t, map[string]any{"model": "auto", "messages": list(userMessage("hi"))})
	if _, err := h.deps.Responses(context.Background(), map[string]any{"model": "auto", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
}

// ---- conversation stream parsing (imagegen events) ----

func deltas(payloads ...string) string {
	ch := make(chan string, len(payloads))
	for _, payload := range payloads {
		ch <- payload
	}
	close(ch)
	out := ""
	imagegen.IterConversationPayloads(ch, "", nil, func(event imagegen.Event) {
		if event.Type == "conversation.delta" {
			out += event.Delta
		}
	})
	return out
}

func TestOutputSanitizer(t *testing.T) {
	cases := map[string]string{
		"Repo: urlbasketikun/chatgpt2apihttps://github.com/basketikun/chatgpt2api details citeturn0search0.": "Repo: basketikun/chatgpt2api (https://github.com/basketikun/chatgpt2api) details.",
		"The character is from entityInvincible, which is based on the comic series entityInvincible.":        "The character is from Invincible, which is based on the comic series Invincible.",
		"The character is citeInvincibleturn0search0.":                                                          "The character is Invincible.",
		"find .":               "find .",
		"if ! test -f x; then": "if ! test -f x; then",
	}
	for input, want := range cases {
		if got := imagegen.SanitizeOutputText(input); got != want {
			t.Errorf("%q -> %q, want %q", input, got, want)
		}
	}
}

func TestStreamSanitizerDoesNotEmitPartialAnnotation(t *testing.T) {
	got := deltas(`{"p":"/message/content/parts/0","o":"append","v":"Repo: `+"urlchat"+`"}`,
		`{"p":"/message/content/parts/0","o":"append","v":"gpt2api`+"turn0search0 done citeturn0."+`"}`, "[DONE]")
	if got != "Repo: chatgpt2api done." || strings.ContainsAny(got, "") {
		t.Fatalf("got %q", got)
	}
}

func TestStreamIgnoresInternalHiddenAndNonFinalMessages(t *testing.T) {
	got := deltas(
		`{"message":{"author":{"role":"assistant"},"content":{"content_type":"code","text":"search(\"x\")"},"recipient":"web","status":"finished_successfully"}}`,
		`{"message":{"author":{"name":"web.run","role":"tool"},"content":{"content_type":"text","parts":[""]},"recipient":"all"}}`,
		`{"message":{"author":{"role":"assistant"},"channel":"final","content":{"content_type":"text","parts":["Hidden text."]},"metadata":{"is_visually_hidden_from_conversation":true},"recipient":"all"}}`,
		`{"message":{"author":{"role":"assistant"},"channel":"analysis","content":{"content_type":"text","parts":["Private reasoning."]},"recipient":"all"}}`,
		finalMessage("Final answer."), "[DONE]")
	if got != "Final answer." {
		t.Fatalf("got %q", got)
	}
	if got := deltas(`{"message":{"author":{"role":"assistant"},"channel":"final","content":{"content_type":"code","text":"print(\"hello\")"},"recipient":"all"}}`, "[DONE]"); got != `print("hello")` {
		t.Fatalf("code message %q", got)
	}
}

// ---- tools and web search ----

func TestResponsesToolsAddHonestNoToolGuard(t *testing.T) {
	h := newHarness(t, nil, nil)
	model, messages, err := h.deps.textResponseParts(map[string]any{"model": "auto", "input": "run echo hi", "tools": list(map[string]any{"type": "function", "name": "shell"})})
	if err != nil || model != "auto" || messages[0]["role"] != "system" || !strings.Contains(pyStr(messages[0]["content"]), "cannot execute local tools") {
		t.Fatalf("messages %v %v", messages, err)
	}
}

func searchHarness(t *testing.T, answer, url string) *harness {
	h := newHarness(t, nil, nil)
	h.search = map[string]any{"answer": answer, "sources": list(map[string]any{"title": "Example", "url": url, "snippet": "Snippet"})}
	return h
}

func TestResponsesWebSearchToolReturnsSearchOutput(t *testing.T) {
	for _, tool := range []string{"web_search", "web_search_preview_2025_03_11"} {
		h := searchHarness(t, "Latest answer.", "https://example.com/news")
		result, err := h.deps.Responses(context.Background(), map[string]any{"model": "auto", "input": "latest example news", "tools": list(map[string]any{"type": tool})})
		if err != nil {
			t.Fatal(err)
		}
		output := result.JSON["output"].([]any)
		search := output[0].(map[string]any)
		content := output[1].(map[string]any)["content"].([]any)[0].(map[string]any)
		annotation := content["annotations"].([]any)[0].(map[string]any)
		if !reflect.DeepEqual(h.searches, []string{"latest example news"}) || search["type"] != "web_search_call" || search["status"] != "completed" ||
			search["action"].(map[string]any)["query"] != "latest example news" || !strings.Contains(content["text"].(string), "Latest answer.") ||
			annotation["type"] != "url_citation" || annotation["url"] != "https://example.com/news" {
			t.Fatalf("%s: %v", tool, result.JSON)
		}
	}
}

func TestResponsesWebSearchToolStreamsSearchEvents(t *testing.T) {
	h := searchHarness(t, "Streamed search answer.", "https://example.com/stream")
	result, err := h.deps.Responses(context.Background(), map[string]any{"model": "auto", "stream": true, "input": "stream search", "tools": list(map[string]any{"type": "web_search_preview"})})
	events := collectStream(t, result, err)
	types := map[string]bool{}
	for _, event := range events {
		types[event["type"].(string)] = true
	}
	for _, want := range []string{"response.web_search_call.in_progress", "response.web_search_call.searching", "response.web_search_call.completed"} {
		if !types[want] {
			t.Fatalf("missing %s", want)
		}
	}
	output := events[len(events)-1]["response"].(map[string]any)["output"].([]any)
	if output[0].(map[string]any)["type"] != "web_search_call" || output[1].(map[string]any)["type"] != "message" {
		t.Fatalf("output %v", output)
	}
}

func TestChatCompletionsWebSearchTriggers(t *testing.T) {
	for _, body := range []map[string]any{
		{"model": "auto", "messages": list(userMessage("search chat")), "tools": list(map[string]any{"type": "web_search"})},
		{"model": "auto", "messages": list(userMessage("search chat")), "web_search_options": map[string]any{"search_context_size": "low"}},
		{"model": "gpt-5-search-api-2026-06-01", "messages": list(userMessage("search chat"))},
	} {
		h := searchHarness(t, "Chat search answer.", "https://example.com/chat")
		response := h.chat(t, body)
		message := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
		if !reflect.DeepEqual(h.searches, []string{"search chat"}) || !strings.Contains(message["content"].(string), "Chat search answer.") || response["model"] != body["model"] {
			t.Fatalf("%v: %v", body, response)
		}
		if body["tools"] != nil {
			annotation := message["annotations"].([]any)[0].(map[string]any)
			if annotation["type"] != "url_citation" || annotation["url_citation"].(map[string]any)["url"] != "https://example.com/chat" {
				t.Fatalf("annotation %v", annotation)
			}
		}
	}
}

func TestSearchLikeModelDoesNotTriggerSearch(t *testing.T) {
	h := searchHarness(t, "x", "https://example.com")
	h.models["tok-1"] = modelList("gpt-5-search-apiary")
	h.reply = func(textCall) []string { return []string{finalMessage("plain text answer")} }
	response := h.chat(t, map[string]any{"model": "gpt-5-search-apiary", "messages": list(userMessage("not actually a search model"))})
	if len(h.searches) != 0 || chatContent(response) != "plain text answer" {
		t.Fatalf("%v", response)
	}
}

func TestSourcesBecomeCitationsWithCodePointOffsets(t *testing.T) {
	text, annotations := textWithURLCitations(map[string]any{"answer": "答案", "sources": list(map[string]any{"title": "标题", "url": "https://e.com/a"})})
	if text != "答案\n\nSources:\n1. 标题 - https://e.com/a" {
		t.Fatalf("text %q", text)
	}
	if annotations[0]["start_index"] != pyLen("答案\n\nSources:\n1. 标题 - ") || annotations[0]["end_index"] != pyLen(text) {
		t.Fatalf("annotations %v", annotations)
	}
}

// ---- images in text requests ----

func TestChatCompletionsAcceptsRemoteImageURL(t *testing.T) {
	hits := int32(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "image/png")
		w.Write(png1x1)
	}))
	defer server.Close()
	h := newHarness(t, nil, nil)
	model, messages, err := h.deps.textChatParts(map[string]any{"model": "auto", "messages": list(map[string]any{"role": "user", "content": list(
		map[string]any{"type": "text", "text": "Describe this"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": server.URL + "/image.png"}})})})
	if err != nil || model != "auto" || hits != 1 {
		t.Fatalf("err %v hits %d", err, hits)
	}
	content := messages[0]["content"].([]any)
	image := content[1].(map[string]any)
	if !reflect.DeepEqual(content[0], map[string]any{"type": "text", "text": "Describe this"}) || image["type"] != "image" ||
		!bytes.Equal(image["data"].([]byte), png1x1) || image["mime"] != "image/png" {
		t.Fatalf("content %v", content)
	}
}

func TestResponsesTextRequestPreservesInputImage(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.reply = func(textCall) []string { return []string{finalMessage("red")} }
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png1x1)
	result, err := h.deps.Responses(context.Background(), map[string]any{"model": "auto", "input": list(
		map[string]any{"type": "input_text", "text": "What color is this image?"}, map[string]any{"type": "input_image", "image_url": dataURL})})
	if err != nil {
		t.Fatal(err)
	}
	text := result.JSON["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]
	content := h.calls[0].messages[0]["content"].([]any)
	image := content[1].(map[string]any)
	if text != "red" || !reflect.DeepEqual(content[0], map[string]any{"type": "text", "text": "What color is this image?"}) ||
		!bytes.Equal(image["data"].([]byte), png1x1) || image["mime"] != "image/png" {
		t.Fatalf("content %v", content)
	}
	if tokens := result.JSON["usage"].(map[string]any)["input_tokens_details"].(map[string]any)["image_tokens"].(int); tokens <= 0 {
		t.Fatalf("image tokens %d", tokens)
	}
}

func TestImageExtractorSupportsExtraImageObjectShapes(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(png1x1)
	images, err := Images{}.ExtractImages(list(
		map[string]any{"type": "image", "data": png1x1, "mime": "image/png"},
		map[string]any{"type": "input_image", "base64": encoded, "mime_type": "image/png"},
		map[string]any{"type": "input_image", "source": map[string]any{"type": "base64", "data": encoded, "media_type": "image/png"}}))
	if err != nil || len(images) != 3 {
		t.Fatalf("images %d err %v", len(images), err)
	}
	for _, image := range images {
		if image.mime != "image/png" || !bytes.Equal(image.data, png1x1) {
			t.Fatalf("image %v", image)
		}
	}
}

// ---- model routing ----

func routingHarness(t *testing.T) *harness {
	h := newHarness(t, []pool.Account{
		{"access_token": "free", "type": "free", "status": "正常"}, {"access_token": "plus", "type": "Plus", "status": "正常"},
		{"access_token": "pro", "type": "Pro", "status": "正常"}, {"access_token": "pro-disabled", "type": "Pro", "status": "禁用"},
	}, nil)
	h.models = map[string]any{"": modelList("anon-only", "shared"), "free": modelList("shared"), "plus": modelList("shared", "plus-only"), "pro": modelList("pro-only")}
	return h
}

func TestExplicitModelSelectsOnlyAdvertisingAccountType(t *testing.T) {
	h := routingHarness(t)
	if token, err := h.deps.textToken(context.Background(), "pro-only", nil); err != nil || token != "pro" {
		t.Fatalf("token %q err %v", token, err)
	}
}

func TestAutoModelKeepsUnfilteredRotation(t *testing.T) {
	h := routingHarness(t)
	tokens := map[string]bool{}
	for i := 0; i < 3; i++ {
		token, _ := h.deps.textToken(context.Background(), "auto", nil)
		tokens[token] = true
	}
	if !reflect.DeepEqual(tokens, map[string]bool{"free": true, "plus": true, "pro": true}) || len(h.modelHits) != 0 {
		t.Fatalf("tokens %v, catalog loaded %v", tokens, h.modelHits)
	}
}

func TestAnonymousModelUsesAnonymousBackend(t *testing.T) {
	h := routingHarness(t)
	if token, err := h.deps.textToken(context.Background(), "anon-only", nil); err != nil || token != "" {
		t.Fatalf("token %q err %v", token, err)
	}
}

func TestModelWithoutEligibleAccountFailsClosed(t *testing.T) {
	h := routingHarness(t)
	_, err := h.deps.textToken(context.Background(), "team-only", nil)
	if err == nil || !strings.Contains(err.Error(), "team-only") {
		t.Fatalf("err %v", err)
	}
}

func TestEndpointsPassTheRequestedModel(t *testing.T) {
	h := routingHarness(t)
	h.chat(t, map[string]any{"model": "plus-only", "messages": list(userMessage("a"))})
	if _, err := h.deps.Responses(context.Background(), map[string]any{"model": "pro-only", "input": "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.deps.Messages(context.Background(), map[string]any{"model": "plus-only", "messages": list(userMessage("c"))}); err != nil {
		t.Fatal(err)
	}
	if h.calls[0].token != "plus" || h.calls[0].model != "plus-only" || h.calls[1].token != "pro" || h.calls[2].token != "plus" {
		t.Fatalf("calls %+v", h.calls)
	}
}

func TestInvalidTokenRetryKeepsRequestedModelFilter(t *testing.T) {
	h := newHarness(t, []pool.Account{
		{"access_token": "plus-a", "type": "Plus", "status": "正常"}, {"access_token": "free", "type": "free", "status": "正常"},
		{"access_token": "plus-b", "type": "Plus", "status": "正常"},
	}, nil)
	h.models = map[string]any{"": modelList(), "free": modelList(), "plus-a": modelList("plus-only"), "plus-b": modelList("plus-only")}
	h.failToken["plus-a"] = errors.New("token_invalidated: 401")
	h.failToken["plus-b"] = errors.New("token_invalidated: 401")
	h.reply = func(textCall) []string { return []string{finalMessage("ok")} }
	_, err := h.deps.ChatCompletions(context.Background(), map[string]any{"model": "plus-only", "messages": list(userMessage("x"))})
	for _, call := range h.calls {
		if call.token == "free" {
			t.Fatalf("retry left the model's account types: %+v", h.calls)
		}
	}
	if err == nil {
		t.Fatal("both Plus accounts are invalid; expected an error")
	}
}

// ---- catalog ----

func catalogHarness(t *testing.T) (*harness, *time.Time) {
	h := newHarness(t, []pool.Account{
		{"access_token": "free-bad", "type": "free", "status": "正常"}, {"access_token": "free-good", "type": "FREE", "status": "正常"},
		{"access_token": "plus", "type": "Plus", "status": "正常"}, {"access_token": "pro", "type": "pro", "status": "正常"},
		{"access_token": "team-disabled", "type": "Team", "status": "禁用"},
	}, nil)
	h.models = map[string]any{"": modelList("anon", "shared"), "free-bad": errors.New("expired"), "free-good": modelList("free-only", "shared"),
		"plus": modelList("plus-only", "shared"), "pro": modelList("pro-only")}
	now := time.Unix(1000, 0)
	h.deps.Catalog.Now = func() time.Time { return now }
	return h, &now
}

func ids(result map[string]any) []string {
	out := []string{}
	for _, item := range result["data"].([]any) {
		if id := item.(map[string]any)["id"].(string); !imagegen.IsSupportedImageModel(id) {
			out = append(out, id)
		}
	}
	return out
}

func TestCatalogUnionsAnonymousAndEachActiveAccountType(t *testing.T) {
	h, _ := catalogHarness(t)
	if got := ids(h.deps.Catalog.ListModels(context.Background())); !reflect.DeepEqual(got, []string{"anon", "free-only", "plus-only", "pro-only", "shared"}) {
		t.Fatalf("ids %v", got)
	}
	for _, token := range h.modelHits {
		if token == "team-disabled" {
			t.Fatal("disabled account asked")
		}
	}
	route := h.deps.Catalog.RouteForModel(context.Background(), "shared")
	if !reflect.DeepEqual(route.AccountTypes, []string{"Plus", "free"}) || !route.AllowAnonymous {
		t.Fatalf("route %+v", route)
	}
}

func TestCatalogIsCachedUntilTTLExpires(t *testing.T) {
	h, now := catalogHarness(t)
	h.deps.Catalog.ListModels(context.Background())
	hits := len(h.modelHits)
	h.deps.Catalog.ListModels(context.Background())
	if len(h.modelHits) != hits {
		t.Fatal("refreshed within the TTL")
	}
	*now = now.Add(301 * time.Second)
	h.deps.Catalog.ListModels(context.Background())
	if len(h.modelHits) == hits {
		t.Fatal("not refreshed after the TTL")
	}
}

func TestConcurrentReadersShareOneCatalogRefresh(t *testing.T) {
	h, _ := catalogHarness(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.deps.Catalog.ListModels(context.Background()) }()
	}
	wg.Wait()
	anonymous := 0
	for _, token := range h.modelHits {
		if token == "" {
			anonymous++
		}
	}
	if anonymous != 1 {
		t.Fatalf("anonymous fetched %d times", anonymous)
	}
}

func TestFailedRefreshKeepsLastModelsForThatType(t *testing.T) {
	h, now := catalogHarness(t)
	h.deps.Catalog.ListModels(context.Background())
	h.models["plus"] = errors.New("down")
	*now = now.Add(301 * time.Second)
	if got := ids(h.deps.Catalog.ListModels(context.Background())); !contains(got, "plus-only") {
		t.Fatalf("ids %v", got)
	}
}

func TestRemovedAccountTypeDropsItsCapabilities(t *testing.T) {
	h, _ := catalogHarness(t)
	h.deps.Catalog.ListModels(context.Background())
	h.deps.Pool.DeleteAccounts([]string{"pro"})
	if got := ids(h.deps.Catalog.ListModels(context.Background())); contains(got, "pro-only") {
		t.Fatalf("ids %v", got)
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// ---- /v1/messages and /v1/responses shapes ----

func TestMessageResponseAndStream(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.reply = func(textCall) []string { return []string{finalMessage("hello")} }
	result, err := h.deps.Messages(context.Background(), map[string]any{"model": "auto", "messages": list(userMessage("hi"))})
	if err != nil || result.JSON["type"] != "message" || result.JSON["stop_reason"] != "end_turn" ||
		result.JSON["content"].([]any)[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("result %v err %v", result.JSON, err)
	}
	stream, err := h.deps.Messages(context.Background(), map[string]any{"model": "auto", "stream": true, "messages": list(userMessage("hi"))})
	events := collectStream(t, stream, err)
	types := []string{}
	for _, event := range events {
		types = append(types, event["type"].(string))
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("types %v", types)
	}
}

func TestMessageToolCallsBecomeToolUse(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.reply = func(textCall) []string {
		return []string{finalMessage("Let me check.<tool_calls><tool_call><tool_name>ls</tool_name><parameters><path><![CDATA[/tmp]]></path></parameters></tool_call></tool_calls>")}
	}
	tools := list(map[string]any{"name": "ls", "description": "list", "input_schema": map[string]any{"type": "object"}})
	result, err := h.deps.Messages(context.Background(), map[string]any{"model": "auto", "tools": tools, "messages": list(userMessage("list /tmp"))})
	if err != nil {
		t.Fatal(err)
	}
	content := result.JSON["content"].([]any)
	use := content[1].(map[string]any)
	if result.JSON["stop_reason"] != "tool_use" || content[0].(map[string]any)["text"] != "Let me check." || use["name"] != "ls" ||
		!reflect.DeepEqual(use["input"], map[string]any{"path": "/tmp"}) {
		t.Fatalf("content %v", content)
	}
	if system := pyStr(h.calls[0].messages[0]["content"]); !strings.Contains(system, "Tool: ls") {
		t.Fatalf("system %q", system)
	}
}

func TestTextResponseStreamEvents(t *testing.T) {
	h := newHarness(t, nil, nil)
	result, err := h.deps.Responses(context.Background(), map[string]any{"model": "auto", "stream": true, "input": "hi"})
	events := collectStream(t, result, err)
	types := []string{}
	for _, event := range events {
		types = append(types, event["type"].(string))
	}
	want := []string{"response.created", "response.output_item.added", "response.output_text.delta", "response.output_text.done", "response.output_item.done", "response.completed"}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("types %v", types)
	}
}
