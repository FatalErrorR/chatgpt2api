package settings

import "strings"

func strOr(value any, def string) string {
	if !pyTruthy(value) {
		return def
	}
	return pyStr(value)
}

func normalizeBackup(value any) map[string]any {
	source := asMap(value)
	include := asMap(source["include"])
	normalizedInclude := map[string]any{}
	for _, key := range backupIncludeOrder {
		normalizedInclude[key] = normalizeBool(include[key], defaultBackupInclude[key])
	}
	prefix := strings.Trim(strOr(source["prefix"], "backups"), "/")
	if prefix == "" {
		prefix = "backups"
	}
	return map[string]any{
		"enabled":           normalizeBool(source["enabled"], false),
		"provider":          "cloudflare_r2",
		"account_id":        pyStr(source["account_id"]),
		"access_key_id":     pyStr(source["access_key_id"]),
		"secret_access_key": pyStr(source["secret_access_key"]),
		"bucket":            pyStr(source["bucket"]),
		"prefix":            prefix,
		"interval_minutes":  normalizePositiveInt(source["interval_minutes"], 360, 1),
		"rotation_keep":     normalizePositiveInt(source["rotation_keep"], 10, 0),
		"encrypt":           normalizeBool(source["encrypt"], false),
		"passphrase":        pyStr(source["passphrase"]),
		"include":           normalizedInclude,
	}
}

func normalizeImageStorage(value any) map[string]any {
	source := asMap(value)
	mode := strings.ToLower(strOr(source["mode"], "local"))
	if mode != "local" && mode != "webdav" && mode != "both" {
		mode = "local"
	}
	enabled := normalizeBool(source["enabled"], false)
	if !enabled {
		mode = "local"
	}
	root := strings.Trim(strOr(source["webdav_root_path"], defaultWebDAVRoot), "/")
	if root == "" {
		root = defaultWebDAVRoot
	}
	return map[string]any{
		"enabled":          enabled,
		"mode":             mode,
		"webdav_url":       strings.TrimRight(pyStr(source["webdav_url"]), "/"),
		"webdav_username":  pyStr(source["webdav_username"]),
		"webdav_password":  pyStr(source["webdav_password"]),
		"webdav_root_path": root,
		"public_base_url":  strings.TrimRight(pyStr(source["public_base_url"]), "/"),
	}
}

type validationError string

func (e validationError) Error() string { return string(e) }

func validateImageStorage(storage map[string]any) error {
	if !normalizeBool(storage["enabled"], false) {
		return nil
	}
	if pyStr(storage["webdav_url"]) == "" {
		return validationError("启用 WebDAV 图片存储后必须填写 WebDAV URL")
	}
	if pyStr(storage["webdav_password"]) == "" {
		return validationError("启用 WebDAV 图片存储后必须填写 WebDAV 密码")
	}
	return nil
}

// IsValidationError reports whether Update failed on user input (HTTP 400) rather than on I/O.
func IsValidationError(err error) bool {
	_, ok := err.(validationError)
	return ok
}

func normalizeChatCompletionCache(value any) map[string]any {
	source := asMap(value)
	return map[string]any{
		"enabled":                  normalizeBool(source["enabled"], true),
		"ttl_seconds":              normalizePositiveInt(source["ttl_seconds"], 60, 0),
		"max_entries":              normalizePositiveInt(source["max_entries"], 256, 1),
		"dedupe_inflight":          normalizeBool(source["dedupe_inflight"], true),
		"stream_cache":             normalizeBool(source["stream_cache"], true),
		"normalize_messages":       normalizeBool(source["normalize_messages"], true),
		"drop_adjacent_duplicates": normalizeBool(source["drop_adjacent_duplicates"], true),
		"drop_assistant_history":   normalizeBool(source["drop_assistant_history"], false),
	}
}

func normalizeStatusCodes(value any) []any {
	items, ok := value.([]any)
	if !ok {
		return []any{403}
	}
	out := []any{}
	seen := map[int]bool{}
	for _, item := range items {
		if _, isBool := item.(bool); isBool {
			continue
		}
		status, valid := pyInt(item)
		if !valid || status < 100 || status > 599 || seen[status] {
			continue
		}
		seen[status] = true
		out = append(out, status)
	}
	if len(out) == 0 {
		return []any{403}
	}
	return out
}

func normalizeProxyRuntime(value any) map[string]any {
	source := asMap(value)
	clearanceSource := asMap(source["clearance"])
	egress := strings.ToLower(strOr(source["egress_mode"], "direct"))
	if egress != "direct" && egress != "single_proxy" {
		egress = "direct"
	}
	mode := strings.ToLower(strOr(clearanceSource["mode"], "none"))
	if mode != "none" && mode != "manual" && mode != "flaresolverr" {
		mode = "none"
	}
	userAgent := strOr(clearanceSource["user_agent"], DefaultProxyRuntimeUserAgent)
	browser := strOr(clearanceSource["browser"], "chrome")
	cookies := pyStr(clearanceSource["cf_cookies"])
	clearance := pyStr(clearanceSource["cf_clearance"])
	if cookies == "" && normalizeBool(clearanceSource["has_cf_cookies"], false) {
		cookies = pyStr(source["_existing_cf_cookies"])
	}
	if clearance == "" && normalizeBool(clearanceSource["has_cf_clearance"], false) {
		clearance = pyStr(source["_existing_cf_clearance"])
	}
	if userAgent == "" {
		userAgent = DefaultProxyRuntimeUserAgent
	}
	if browser == "" {
		browser = "chrome"
	}
	return map[string]any{
		"enabled":                    normalizeBool(source["enabled"], false),
		"egress_mode":                egress,
		"proxy_url":                  pyStr(source["proxy_url"]),
		"resource_proxy_url":         pyStr(source["resource_proxy_url"]),
		"skip_ssl_verify":            normalizeBool(source["skip_ssl_verify"], false),
		"reset_session_status_codes": normalizeStatusCodes(source["reset_session_status_codes"]),
		"clearance": map[string]any{
			"enabled":          normalizeBool(clearanceSource["enabled"], false),
			"mode":             mode,
			"cf_cookies":       cookies,
			"cf_clearance":     clearance,
			"user_agent":       userAgent,
			"browser":          browser,
			"flaresolverr_url": pyStr(clearanceSource["flaresolverr_url"]),
			"timeout_sec":      normalizePositiveInt(clearanceSource["timeout_sec"], 60, 1),
			"refresh_interval": normalizePositiveInt(clearanceSource["refresh_interval"], 3600, 60),
			"warm_up_on_start": normalizeBool(clearanceSource["warm_up_on_start"], false),
		},
	}
}

func normalizeThirdPartyApps(value any) map[string]any {
	source := asMap(value)
	canvas := asMap(source["infinite_canvas"])
	return map[string]any{
		"infinite_canvas": map[string]any{
			"enabled": normalizeBool(canvas["enabled"], false),
			"url":     strOr(canvas["url"], "https://canvas.best"),
		},
	}
}
