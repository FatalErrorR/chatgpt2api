// Package pool is the account pool of the Python version (services/account_service.py): accounts
// kept as JSON documents keyed by access token, round-robin image selection with per-account
// concurrency slots, cooldowns, token refresh and the statistics the overview shows.
//
// Accounts stay map[string]any like the Python dicts, so every field of accounts.json survives and
// the logic can be compared line by line with the Python source.
package pool

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/pyval"
)

// Account is one account document.
type Account = map[string]any

// Status values used by the Python version.
const (
	StatusNormal   = "正常"
	StatusLimited  = "限流"
	StatusAbnormal = "异常"
	StatusDisabled = "禁用"
)

// AnonymizeToken is utils/helper.py anonymize_token.
func AnonymizeToken(token any) string {
	value := pyval.Str(token)
	if value == "" {
		return "token:empty"
	}
	sum := sha256.Sum256([]byte(value))
	return "token:" + hex.EncodeToString(sum[:])[:10]
}

func str(value any) string { return pyval.Str(value) }

// intOf is int(value or 0) for counters.
func intOf(value any) int {
	n, _ := pyval.Int(value)
	return n
}

// orNil is `value or None`.
func orNil(value any) any {
	if pyval.Truthy(value) {
		return value
	}
	return nil
}

// NormalizeAccountType is AccountService._normalize_account_type ("" for none).
func NormalizeAccountType(value any) string {
	raw := str(value)
	if raw == "" {
		return ""
	}
	key := strings.ReplaceAll(strings.ReplaceAll(strings.ToLower(raw), "-", "_"), " ", "_")
	compact := strings.ReplaceAll(key, "_", "")
	aliases := map[string]string{"free": "free", "plus": "Plus", "pro": "Pro", "prolite": "ProLite", "team": "Team", "business": "Team", "enterprise": "Enterprise"}
	if alias, ok := aliases[compact]; ok {
		return alias
	}
	if alias, ok := aliases[key]; ok {
		return alias
	}
	return raw
}

// normalizeSourceType is str(value or "web").strip().lower() or "web".
func normalizeSourceType(value any) string {
	if !pyval.Truthy(value) {
		return "web"
	}
	if text := strings.ToLower(str(value)); text != "" {
		return text
	}
	return "web"
}

// nowUTC is AccountService._now(): UTC "YYYY-MM-DD HH:MM:SS".
func nowUTC() string { return time.Now().UTC().Format("2006-01-02 15:04:05") }

// nowLocal is datetime.now().strftime(...) on the server.
func nowLocal() string { return time.Now().Format("2006-01-02 15:04:05") }

// isoUTC is datetime.now(timezone.utc).isoformat().
func isoUTC(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000-07:00") }

// parseTime is AccountService._parse_time: ISO 8601 (with Z) or "YYYY-MM-DD HH:MM:SS", naive = UTC.
func parseTime(value any) (time.Time, bool) {
	raw := str(value)
	if raw == "" {
		return time.Time{}, false
	}
	raw = strings.Replace(raw, "Z", "+00:00", 1)
	layouts := []string{
		"2006-01-02T15:04:05.999999999-07:00", "2006-01-02T15:04:05-07:00", "2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05-07:00", "2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05", "2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

// DecodeJWTPayload is AccountService._decode_jwt_payload.
func DecodeJWTPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	payload := parts[1]
	payload += strings.Repeat("=", (4-len(payload)%4)%4)
	raw, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return map[string]any{}
	}
	var data map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&data) != nil || data == nil {
		return map[string]any{}
	}
	return data
}

func jwtClaimInt(token, claim string) int64 {
	n, ok := pyval.Int(DecodeJWTPayload(token)[claim])
	if !ok {
		return 0
	}
	return int64(n)
}

// timestampToISO is AccountService._timestamp_to_iso: unix seconds as UTC+8 ISO text.
func timestampToISO(value any) string {
	n, ok := pyval.Int(value)
	if !ok {
		return ""
	}
	return time.Unix(int64(n), 0).In(time.FixedZone("", 8*3600)).Format("2006-01-02T15:04:05-07:00")
}

func copyAccount(account Account) Account {
	if account == nil {
		return nil
	}
	out := make(Account, len(account))
	for key, value := range account {
		out[key] = value
	}
	return out
}

func merge(parts ...Account) Account {
	out := Account{}
	for _, part := range parts {
		for key, value := range part {
			out[key] = value
		}
	}
	return out
}

// accountToken is item.get("access_token") or item.get("accessToken") or "".
func accountToken(item Account) string {
	if token := item["access_token"]; pyval.Truthy(token) {
		text, _ := token.(string)
		return text
	}
	if token := item["accessToken"]; pyval.Truthy(token) {
		text, _ := token.(string)
		return text
	}
	return ""
}

// NormalizeAccount is AccountService._normalize_account. created is used when created_at is missing.
func NormalizeAccount(item Account, created string) Account {
	if item == nil {
		return nil
	}
	token := accountToken(item)
	if token == "" {
		return nil
	}
	n := copyAccount(item)
	delete(n, "accessToken")
	delete(n, "proxy_key")
	n["access_token"] = token
	if strings.ToLower(str(n["type"])) == "codex" {
		n["export_type"] = "codex"
		delete(n, "type")
	}
	if !pyval.Truthy(n["type"]) {
		n["type"] = "free"
	}
	if !pyval.Truthy(n["status"]) {
		n["status"] = StatusNormal
	}
	n["quota"] = max(0, intOf(n["quota"]))
	n["email"] = orNil(n["email"])
	n["user_id"] = orNil(n["user_id"])
	n["proxy"] = str(n["proxy"])
	source := n["source_type"]
	if !pyval.Truthy(source) && strings.ToLower(str(n["export_type"])) == "codex" {
		source = "codex"
	}
	n["source_type"] = normalizeSourceType(source)
	if _, ok := n["limits_progress"].([]any); !ok {
		n["limits_progress"] = []any{}
	}
	n["default_model_slug"] = orNil(n["default_model_slug"])
	n["restore_at"] = orNil(n["restore_at"])
	if blocked := str(n["edit_upload_blocked_until"]); blocked != "" {
		n["edit_upload_blocked_until"] = blocked
	} else {
		n["edit_upload_blocked_until"] = nil
	}
	n["success"] = intOf(n["success"])
	n["fail"] = intOf(n["fail"])
	n["invalid_count"] = intOf(n["invalid_count"])
	if _, ok := n["last_used_at"]; !ok {
		n["last_used_at"] = nil
	}
	for _, key := range []string{"last_invalid_at", "last_refresh_error", "last_refresh_error_at", "last_token_refresh_at", "last_token_refresh_error", "last_token_refresh_error_at"} {
		n[key] = orNil(n[key])
	}
	if !pyval.Truthy(n["created_at"]) {
		if created == "" {
			created = nowUTC()
		}
		n["created_at"] = created
	}
	return n
}

// isImageAccountAvailable: not 禁用/限流/异常 and quota > 0.
func isImageAccountAvailable(account Account) bool {
	if account == nil {
		return false
	}
	switch account["status"] {
	case StatusDisabled, StatusLimited, StatusAbnormal:
		return false
	}
	return intOf(account["quota"]) > 0
}

func matchesPlanType(account Account, planType string) bool {
	if planType == "" {
		return true
	}
	plan, own := NormalizeAccountType(planType), NormalizeAccountType(account["type"])
	return plan != "" && own != "" && strings.EqualFold(plan, own)
}

func matchesSourceType(account Account, sourceType string) bool {
	if sourceType == "" {
		return true
	}
	return normalizeSourceType(account["source_type"]) == normalizeSourceType(sourceType)
}

func matchesAnyPlanType(account Account, planTypes []string) bool {
	if len(planTypes) == 0 {
		return true
	}
	own := NormalizeAccountType(account["type"])
	if own == "" {
		return false
	}
	for _, plan := range planTypes {
		if NormalizeAccountType(plan) == own {
			return true
		}
	}
	return false
}

// Filter narrows image account selection, like the keyword arguments of the Python methods.
type Filter struct {
	PlanType   string
	SourceType string
	PlanTypes  []string
	Excluded   map[string]bool
}

func (f Filter) matches(account Account) bool {
	return matchesPlanType(account, f.PlanType) && matchesAnyPlanType(account, f.PlanTypes) && matchesSourceType(account, f.SourceType)
}
