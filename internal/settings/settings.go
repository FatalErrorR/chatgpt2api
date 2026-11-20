// Package settings reads and writes config.json in the format of the Python version
// (services/config.py on main): same keys, defaults, bounds and validation, so the existing
// frontend settings page and the production config.json work unchanged.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var defaultBackupInclude = map[string]bool{
	"config": true, "cpa": true, "sub2api": true, "logs": true, "image_tasks": true,
	"accounts_snapshot": true, "auth_keys_snapshot": true, "images": false,
}

var backupIncludeOrder = []string{"config", "cpa", "sub2api", "logs", "image_tasks", "accounts_snapshot", "auth_keys_snapshot", "images"}

const defaultWebDAVRoot = "chatgpt2api/images"

// DefaultProxyRuntimeUserAgent matches DEFAULT_PROXY_RUNTIME_USER_AGENT.
const DefaultProxyRuntimeUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/145.0.0.0 Safari/537.36"

// ErrMissingAuthKey is returned when neither CHATGPT2API_AUTH_KEY nor config.json "auth-key" is set.
var ErrMissingAuthKey = errors.New("❌ auth-key 未设置！\n请在环境变量 CHATGPT2API_AUTH_KEY 中设置，或者在 config.json 中填写 auth-key。")

// Store holds config.json. All reads go through the getters below.
type Store struct {
	mu   sync.RWMutex
	path string
	data map[string]any
	// DataDir is where accounts, logs and images live (data/ next to config.json by default).
	DataDir string

	hooksMu sync.Mutex
	hooks   []func()
}

// OnChange registers a callback run after every successful Update.
func (s *Store) OnChange(fn func()) {
	s.hooksMu.Lock()
	defer s.hooksMu.Unlock()
	s.hooks = append(s.hooks, fn)
}

func (s *Store) runHooks() {
	s.hooksMu.Lock()
	hooks := append([]func(){}, s.hooks...)
	s.hooksMu.Unlock()
	for _, hook := range hooks {
		hook()
	}
}

// Load reads config.json. A missing or invalid file counts as an empty object, as in Python.
func Load(path string, dataDir string) (*Store, error) {
	s := &Store{path: path, data: readJSONObject(path), DataDir: dataDir}
	if s.AuthKey() == "" {
		return nil, ErrMissingAuthKey
	}
	return s, nil
}

// NewForTest builds a store from a map without touching the disk unless Update is called.
func NewForTest(data map[string]any, path string) *Store {
	if data == nil {
		data = map[string]any{}
	}
	return &Store{path: path, data: data}
}

func readJSONObject(path string) map[string]any {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return map[string]any{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	// UseNumber keeps 5 and 5.0 apart, as Python's json module does
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var data map[string]any
	if decoder.Decode(&data) != nil || data == nil {
		return map[string]any{}
	}
	return data
}

func (s *Store) get(key string) (any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.data[key]
	return value, ok
}

func (s *Store) getOr(key string, def any) any {
	if value, ok := s.get(key); ok {
		return value
	}
	return def
}

func intSetting(value any, def int, ok bool) int {
	if !ok {
		return def
	}
	n, valid := pyInt(value)
	if !valid {
		return def
	}
	return n
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// AuthKey is CHATGPT2API_AUTH_KEY or config.json "auth-key".
func (s *Store) AuthKey() string {
	if env := os.Getenv("CHATGPT2API_AUTH_KEY"); env != "" {
		return strings.TrimSpace(env)
	}
	return pyStr(s.getOr("auth-key", nil))
}

func (s *Store) RefreshAccountIntervalMinute() int {
	v, ok := s.get("refresh_account_interval_minute")
	return intSetting(v, 5, ok)
}

func (s *Store) ImageRetentionDays() int {
	v, ok := s.get("image_retention_days")
	return max(1, intSetting(v, 30, ok))
}

func (s *Store) ImagePollTimeoutSecs() int {
	v, ok := s.get("image_poll_timeout_secs")
	return max(1, intSetting(v, 120, ok))
}

func (s *Store) ProxyMaxAccounts() int {
	v, ok := s.get("proxy_max_accounts")
	return clampInt(intSetting(v, 4, ok), 1, 1000)
}

func (s *Store) ImageFailoverEnabled() bool {
	return normalizeBool(s.getOr("image_failover_enabled", nil), false)
}

func (s *Store) ImageFailoverSecs() int {
	v, ok := s.get("image_failover_secs")
	return clampInt(intSetting(v, 60, ok), 15, 600)
}

func floatSetting(s *Store, key string, def, minimum float64) float64 {
	v, ok := s.get(key)
	if !ok {
		return math.Max(minimum, def)
	}
	f, valid := pyFloat(v)
	if !valid {
		return def
	}
	return math.Max(minimum, f)
}

func (s *Store) ImagePollIntervalSecs() float64 {
	return floatSetting(s, "image_poll_interval_secs", 10, 0.5)
}
func (s *Store) ImagePollInitialWaitSecs() float64 {
	return floatSetting(s, "image_poll_initial_wait_secs", 10, 0)
}
func (s *Store) ImageSettleSecs() float64 { return floatSetting(s, "image_settle_secs", 2, 0.5) }

func (s *Store) ImageAccountConcurrency() int {
	v, ok := s.get("image_account_concurrency")
	return max(1, intSetting(v, 3, ok))
}

func (s *Store) ImageParallelGeneration() bool {
	return looseBool(s.getOr("image_parallel_generation", true))
}
func (s *Store) ImageSettleEnabled() bool { return looseBool(s.getOr("image_settle_enabled", true)) }
func (s *Store) ImageCheckBeforeHitEnabled() bool {
	return looseBool(s.getOr("image_check_before_hit_enabled", true))
}
func (s *Store) ImageRemoveConversationAfterResult() bool {
	return looseBool(s.getOr("image_remove_conversation_after_result", false))
}
func (s *Store) ImageRemoveConversationAlways() bool {
	return normalizeBool(s.getOr("image_remove_conversation_always", nil), false)
}
func (s *Store) ImageRefFitEnabled() bool {
	return normalizeBool(s.getOr("image_ref_fit_enabled", true), true)
}
func (s *Store) ImageRefFitBudgetBytes() int {
	return normalizePositiveInt(s.getOr("image_ref_fit_budget_bytes", 1468006), 1468006, 1)
}
func (s *Store) ImageRefFitMaxEdge() int {
	return normalizePositiveInt(s.getOr("image_ref_fit_max_edge", 2048), 2048, 256)
}
func (s *Store) ImageReqFitEnabled() bool {
	return normalizeBool(s.getOr("image_req_fit_enabled", true), true)
}
func (s *Store) ImageReqFitBudgetBytes() int {
	return normalizePositiveInt(s.getOr("image_req_fit_budget_bytes", 1468006), 1468006, 1)
}
func (s *Store) ImageUpscaleEnabled() bool {
	return normalizeBool(s.getOr("image_upscale_enabled", false), false)
}
func (s *Store) ImageExactSizeEnabled() bool {
	return normalizeBool(s.getOr("image_exact_size_enabled", false), false)
}
func (s *Store) ImageUpscaleTargetEdge() int {
	return min(normalizePositiveInt(s.getOr("image_upscale_target_edge", 2048), 2048, 1024), 4096)
}
func (s *Store) ImageUpscaleMethod() string {
	value := strings.ToLower(pyStr(s.getOr("image_upscale_method", "lanczos")))
	if value == "lanczos" || value == "model" {
		return value
	}
	return "lanczos"
}
func (s *Store) ImageEditUploadCooldownEnabled() bool {
	return normalizeBool(s.getOr("image_edit_upload_cooldown_enabled", true), true)
}

// ImageMinFreeMB is the free-space floor of the automatic cleanup (Python: getattr default 500).
func (s *Store) ImageMinFreeMB() int { return 500 }

func (s *Store) AutoRemoveInvalidAccounts() bool {
	return looseBool(s.getOr("auto_remove_invalid_accounts", false))
}
func (s *Store) AutoRemoveRateLimitedAccounts() bool {
	return looseBool(s.getOr("auto_remove_rate_limited_accounts", false))
}
func (s *Store) AutoReloginAfterRefresh() bool {
	return looseBool(s.getOr("auto_relogin_after_refresh", false))
}

func (s *Store) LogLevels() []string {
	levels, ok := s.getOr("log_levels", nil).([]any)
	out := []string{}
	if !ok {
		return out
	}
	allowed := map[string]bool{"debug": true, "info": true, "warning": true, "error": true}
	for _, item := range levels {
		level := strings.ToLower(pyStr(item))
		if allowed[level] {
			out = append(out, level)
		}
	}
	return out
}

func (s *Store) SensitiveWords() []string {
	words, ok := s.getOr("sensitive_words", nil).([]any)
	out := []string{}
	if !ok {
		return out
	}
	for _, item := range words {
		if word := pyStr(item); word != "" {
			out = append(out, word)
		}
	}
	return out
}

func (s *Store) AIReview() map[string]any {
	if value, ok := s.getOr("ai_review", nil).(map[string]any); ok {
		return value
	}
	return map[string]any{}
}

func (s *Store) GlobalSystemPrompt() string { return pyStr(s.getOr("global_system_prompt", nil)) }

// DefaultUpstreamModelName is str(value or "auto").strip(): a value of only spaces gives "".
func (s *Store) DefaultUpstreamModelName() string {
	value := s.getOr("default_upstream_model_name", nil)
	if !pyTruthy(value) {
		return "auto"
	}
	return pyStr(value)
}

func (s *Store) DefaultThinkingEffort() string {
	value := strings.ToLower(pyStr(s.getOr("default_thinking_effort", nil)))
	switch value {
	case "auto", "standard", "extended", "max":
		return value
	}
	return "auto"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func (s *Store) BaseURL() string {
	return strings.TrimRight(strings.TrimSpace(firstNonEmpty(os.Getenv("CHATGPT2API_BASE_URL"), pyStr(s.getOr("base_url", nil)))), "/")
}

func (s *Store) SentinelSolverURL() string {
	return strings.TrimRight(strings.TrimSpace(firstNonEmpty(os.Getenv("CHATGPT2API_SENTINEL_SOLVER_URL"), pyStr(s.getOr("sentinel_solver_url", nil)))), "/")
}

func (s *Store) SentinelSolverKey() string {
	return strings.TrimSpace(firstNonEmpty(os.Getenv("CHATGPT2API_SENTINEL_SOLVER_KEY"), pyStr(s.getOr("sentinel_solver_key", nil))))
}

// Proxy is the global proxy setting (config.json "proxy").
func (s *Store) Proxy() string { return pyStr(s.getOr("proxy", nil)) }

// ImageRetentionKeepFrom is the first kept day (YYYY-MM-DD) when keeping the last N calendar days.
func (s *Store) ImageRetentionKeepFrom(today time.Time) string {
	return today.AddDate(0, 0, -(s.ImageRetentionDays() - 1)).Format("2006-01-02")
}

func (s *Store) Backup() map[string]any { return normalizeBackup(s.getOr("backup", nil)) }
func (s *Store) ImageStorage() map[string]any {
	return normalizeImageStorage(s.getOr("image_storage", nil))
}
func (s *Store) ThirdPartyApps() map[string]any {
	return normalizeThirdPartyApps(s.getOr("third_party_apps", nil))
}
func (s *Store) ChatCompletionCache() map[string]any {
	return normalizeChatCompletionCache(s.getOr("chat_completion_cache", nil))
}
func (s *Store) ProxyRuntime() map[string]any {
	return normalizeProxyRuntime(s.getOr("proxy_runtime", nil))
}

// PublicProxyRuntime hides the clearance cookies and reports only whether they are set.
func (s *Store) PublicProxyRuntime() map[string]any {
	runtime := s.ProxyRuntime()
	clearance := asMap(runtime["clearance"])
	cookies, _ := clearance["cf_cookies"].(string)
	cf, _ := clearance["cf_clearance"].(string)
	clearance["cf_cookies"] = ""
	clearance["cf_clearance"] = ""
	clearance["has_cf_cookies"] = strings.TrimSpace(cookies) != ""
	clearance["has_cf_clearance"] = strings.TrimSpace(cf) != ""
	runtime["clearance"] = clearance
	return runtime
}

// Get is what GET /api/settings returns: the raw config with every known key normalized and
// the secrets (auth-key, sentinel_solver_key) removed.
func (s *Store) Get() map[string]any {
	s.mu.RLock()
	data := make(map[string]any, len(s.data)+40)
	for key, value := range s.data {
		data[key] = value
	}
	s.mu.RUnlock()
	data["refresh_account_interval_minute"] = s.RefreshAccountIntervalMinute()
	data["image_retention_days"] = s.ImageRetentionDays()
	data["image_poll_timeout_secs"] = s.ImagePollTimeoutSecs()
	data["image_poll_interval_secs"] = s.ImagePollIntervalSecs()
	data["image_failover_enabled"] = s.ImageFailoverEnabled()
	data["proxy_max_accounts"] = s.ProxyMaxAccounts()
	data["image_failover_secs"] = s.ImageFailoverSecs()
	data["image_poll_initial_wait_secs"] = s.ImagePollInitialWaitSecs()
	data["image_account_concurrency"] = s.ImageAccountConcurrency()
	data["image_parallel_generation"] = s.ImageParallelGeneration()
	data["image_remove_conversation_after_result"] = s.ImageRemoveConversationAfterResult()
	data["image_remove_conversation_always"] = s.ImageRemoveConversationAlways()
	data["image_ref_fit_enabled"] = s.ImageRefFitEnabled()
	data["image_ref_fit_budget_bytes"] = s.ImageRefFitBudgetBytes()
	data["image_ref_fit_max_edge"] = s.ImageRefFitMaxEdge()
	data["image_req_fit_enabled"] = s.ImageReqFitEnabled()
	data["image_req_fit_budget_bytes"] = s.ImageReqFitBudgetBytes()
	data["image_upscale_enabled"] = s.ImageUpscaleEnabled()
	data["image_exact_size_enabled"] = s.ImageExactSizeEnabled()
	data["image_upscale_target_edge"] = s.ImageUpscaleTargetEdge()
	data["image_upscale_method"] = s.ImageUpscaleMethod()
	data["image_edit_upload_cooldown_enabled"] = s.ImageEditUploadCooldownEnabled()
	data["sentinel_solver_url"] = s.SentinelSolverURL()
	data["auto_remove_invalid_accounts"] = s.AutoRemoveInvalidAccounts()
	data["auto_remove_rate_limited_accounts"] = s.AutoRemoveRateLimitedAccounts()
	data["auto_relogin_after_refresh"] = s.AutoReloginAfterRefresh()
	data["log_levels"] = s.LogLevels()
	data["sensitive_words"] = s.SensitiveWords()
	data["ai_review"] = s.AIReview()
	data["global_system_prompt"] = s.GlobalSystemPrompt()
	data["default_upstream_model_name"] = s.DefaultUpstreamModelName()
	data["default_thinking_effort"] = s.DefaultThinkingEffort()
	data["backup"] = s.Backup()
	data["image_storage"] = s.ImageStorage()
	data["chat_completion_cache"] = s.ChatCompletionCache()
	data["proxy_runtime"] = s.PublicProxyRuntime()
	data["third_party_apps"] = s.ThirdPartyApps()
	delete(data, "sentinel_solver_key")
	delete(data, "auth-key")
	return data
}

// Update merges the given keys, normalizes the nested sections, validates WebDAV and saves.
// The error text matches Python's ValueError messages (shown to the user as-is).
func (s *Store) Update(changes map[string]any) (map[string]any, error) {
	s.mu.Lock()
	next := make(map[string]any, len(s.data)+len(changes))
	for key, value := range s.data {
		next[key] = value
	}
	for key, value := range changes {
		next[key] = value
	}
	previousClearance := asMap(normalizeProxyRuntime(s.data["proxy_runtime"])["clearance"])
	if _, ok := next["backup"]; ok {
		next["backup"] = normalizeBackup(next["backup"])
	}
	if _, ok := next["image_storage"]; ok {
		storage := normalizeImageStorage(next["image_storage"])
		if err := validateImageStorage(storage); err != nil {
			s.mu.Unlock()
			return nil, err
		}
		next["image_storage"] = storage
	}
	if _, ok := next["chat_completion_cache"]; ok {
		next["chat_completion_cache"] = normalizeChatCompletionCache(next["chat_completion_cache"])
	}
	if _, ok := next["third_party_apps"]; ok {
		next["third_party_apps"] = normalizeThirdPartyApps(next["third_party_apps"])
	}
	if incoming, ok := next["proxy_runtime"]; ok {
		if runtime, isMap := incoming.(map[string]any); isMap {
			copied := make(map[string]any, len(runtime)+2)
			for key, value := range runtime {
				copied[key] = value
			}
			copied["_existing_cf_cookies"] = previousClearance["cf_cookies"]
			copied["_existing_cf_clearance"] = previousClearance["cf_clearance"]
			incoming = copied
		}
		next["proxy_runtime"] = normalizeProxyRuntime(incoming)
	}
	delete(next, "backup_state")
	s.data = next
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.runHooks()
	return s.Get(), nil
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
