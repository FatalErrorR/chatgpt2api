package textapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/settings"
)

// chat_completion_cache.py: identical text requests within the TTL get the stored answer, and
// identical requests in flight wait for the first one.

var cacheableTextKeys = []string{"frequency_penalty", "max_completion_tokens", "max_tokens", "metadata", "model", "presence_penalty",
	"reasoning_effort", "response_format", "seed", "stop", "temperature", "thinking_effort", "tool_choice", "tools", "top_p", "user", "reasoning"}

// jsonSafe is _json_safe: bytes become their sha256 and length.
func jsonSafe(value any) any {
	switch v := value.(type) {
	case []byte:
		sum := sha256.Sum256(v)
		return map[string]any{"__bytes_sha256__": hex.EncodeToString(sum[:]), "length": len(v)}
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = jsonSafe(item)
		}
		return out
	case []map[string]any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = jsonSafe(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = jsonSafe(item)
		}
		return out
	}
	return value
}

// canonicalJSON is json.dumps(sort_keys=True, separators=(",", ":"), ensure_ascii=False).
func canonicalJSON(value any) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.Encode(value) // Go sorts map keys
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}

// CacheKey is cache_key.
func CacheKey(body map[string]any, messages []map[string]any, stream bool) string {
	payload := map[string]any{}
	for _, key := range cacheableTextKeys {
		if value, ok := body[key]; ok {
			payload[key] = value
		}
	}
	payload["messages"] = messages
	payload["stream"] = stream
	sum := sha256.Sum256([]byte(canonicalJSON(jsonSafe(payload))))
	return hex.EncodeToString(sum[:])
}

// NormalizeTextMessages is normalize_text_messages.
func NormalizeTextMessages(store *settings.Store, messages []map[string]any) []map[string]any {
	cfg := store.ChatCompletionCache()
	if cfg["normalize_messages"] != true {
		return messages
	}
	out := []map[string]any{}
	previous := ""
	for _, message := range messages {
		if cfg["drop_assistant_history"] == true && pyStr(message["role"]) == "assistant" {
			continue
		}
		signature := canonicalJSON(jsonSafe(message))
		if cfg["drop_adjacent_duplicates"] == true && signature == previous {
			continue
		}
		out = append(out, message)
		previous = signature
	}
	return out
}

type cacheEntry struct {
	expires time.Time
	value   any // map[string]any or []map[string]any (stream chunks)
}

type inflight struct {
	done  chan struct{}
	value any
	err   error
}

// Cache is ChatCompletionCache.
type Cache struct {
	Settings *settings.Store
	mu       sync.Mutex
	entries  map[string]cacheEntry
	pending  map[string]*inflight
}

func NewCache(store *settings.Store) *Cache {
	return &Cache{Settings: store, entries: map[string]cacheEntry{}, pending: map[string]*inflight{}}
}

func intOf(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, _ := strconv.Atoi(v.String())
		return n
	}
	return 0
}

func (c *Cache) pruneLocked(now time.Time, maxEntries int) {
	for key, entry := range c.entries {
		if !entry.expires.After(now) {
			delete(c.entries, key)
		}
	}
	for len(c.entries) > maxEntries {
		oldest, first := "", true
		for key, entry := range c.entries {
			if first || entry.expires.Before(c.entries[oldest].expires) {
				oldest, first = key, false
			}
		}
		delete(c.entries, oldest)
	}
}

// deepCopy round-trips through JSON-compatible values.
func deepCopy(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = deepCopy(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = deepCopy(item)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(v))
		for i, item := range v {
			out[i] = deepCopy(item).(map[string]any)
		}
		return out
	}
	return value
}

// lookup returns a cached value, or the in-flight call to wait for, or makes this caller the owner.
func (c *Cache) lookup(key string, stream bool) (cached any, wait *inflight, owner *inflight, enabled bool) {
	cfg := c.Settings.ChatCompletionCache()
	ttl := intOf(cfg["ttl_seconds"])
	if cfg["enabled"] != true || ttl <= 0 || (stream && cfg["stream_cache"] != true) {
		return nil, nil, nil, false
	}
	dedupe := cfg["dedupe_inflight"] == true
	maxEntries := max(1, intOf(cfg["max_entries"]))
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.pruneLocked(now, maxEntries)
	if entry, ok := c.entries[key]; ok && entry.expires.After(now) {
		return deepCopy(entry.value), nil, nil, true
	}
	if dedupe {
		if pending, ok := c.pending[key]; ok {
			return nil, pending, nil, true
		}
	}
	call := &inflight{done: make(chan struct{})}
	if dedupe {
		c.pending[key] = call
	}
	return nil, nil, call, true
}

func (c *Cache) finish(key string, call *inflight, value any, err error) {
	cfg := c.Settings.ChatCompletionCache()
	c.mu.Lock()
	if err == nil {
		c.entries[key] = cacheEntry{expires: time.Now().Add(time.Duration(intOf(cfg["ttl_seconds"])) * time.Second), value: deepCopy(value)}
		c.pruneLocked(time.Now(), max(1, intOf(cfg["max_entries"])))
	}
	if c.pending[key] == call {
		delete(c.pending, key)
	}
	c.mu.Unlock()
	call.value, call.err = deepCopy(value), err
	close(call.done)
}

// Response is get_or_compute_response.
func (c *Cache) Response(key string, compute func() (map[string]any, error)) (map[string]any, error) {
	key = "response:" + key // JSON answers and stream chunks never share an entry
	cached, wait, owner, enabled := c.lookup(key, false)
	if !enabled {
		return compute()
	}
	if cached != nil {
		return cached.(map[string]any), nil
	}
	if wait != nil {
		<-wait.done
		if wait.err != nil {
			return nil, wait.err
		}
		return deepCopy(wait.value).(map[string]any), nil
	}
	value, err := compute()
	c.finish(key, owner, value, err)
	return value, err
}

// Stream is get_or_compute_stream: replay stored chunks, or produce and record them.
func (c *Cache) Stream(key string, compute func(emit func(map[string]any) error) error, emit func(map[string]any) error) error {
	key = "stream:" + key
	cached, wait, owner, enabled := c.lookup(key, true)
	if !enabled {
		return compute(emit)
	}
	replay := func(value any) error {
		for _, chunk := range deepCopy(value).([]map[string]any) {
			if err := emit(chunk); err != nil {
				return err
			}
		}
		return nil
	}
	if cached != nil {
		return replay(cached)
	}
	if wait != nil {
		<-wait.done
		if wait.err != nil {
			return wait.err
		}
		return replay(wait.value)
	}
	chunks := []map[string]any{}
	err := compute(func(chunk map[string]any) error {
		chunks = append(chunks, deepCopy(chunk).(map[string]any))
		return emit(chunk)
	})
	c.finish(key, owner, chunks, err)
	return err
}

// Clear drops every entry (tests).
func (c *Cache) Clear() {
	c.mu.Lock()
	c.entries, c.pending = map[string]cacheEntry{}, map[string]*inflight{}
	c.mu.Unlock()
}
