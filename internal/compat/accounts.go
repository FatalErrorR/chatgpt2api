package compat

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ProxyReserver hands out proxies for new accounts, spreading them under the per-proxy cap
// (proxy_list.reserve). Wired with the proxy pool (step 5); nil until then.
type ProxyReserver interface {
	Reserve(candidates []string, count int) ([]string, error)
	Release(urls []string)
}

func stringList(body map[string]any, field string) ([]string, bool) {
	value, present := body[field]
	if !present || value == nil {
		return []string{}, true
	}
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, text)
	}
	return out, true
}

func uniqueTrimmed(tokens []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token != "" && !seen[token] {
			seen[token] = true
			out = append(out, token)
		}
	}
	return out
}

func payloadToken(item map[string]any) string {
	for _, key := range []string{"access_token", "accessToken"} {
		if text, ok := item[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func addResultMap(result pool.AddResult) map[string]any {
	return map[string]any{"added": result.Added, "skipped": result.Skipped, "items": result.Items}
}

// addPlainTokens is _add_plain_tokens.
func addPlainTokens(p *pool.Service, tokens []string, proxyFor map[string]string) pool.AddResult {
	any := false
	for _, token := range tokens {
		if proxyFor[token] != "" {
			any = true
			break
		}
	}
	if !any {
		return p.AddAccounts(tokens, "web")
	}
	items := make([]pool.Account, 0, len(tokens))
	for _, token := range tokens {
		item := pool.Account{"access_token": token, "source_type": "web"}
		if proxyFor[token] != "" {
			item["proxy"] = proxyFor[token]
		}
		items = append(items, item)
	}
	return p.AddAccountItems(items)
}

var exportNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeExportName(value, fallback string) string {
	clean := strings.Trim(exportNameUnsafe.ReplaceAllString(strings.TrimSpace(value), "-"), "-._")
	if clean == "" {
		clean = fallback
	}
	if len(clean) > 80 {
		clean = clean[:80]
	}
	return clean
}

func accountZip(items []map[string]any) []byte {
	var buf bytes.Buffer
	archive := zip.NewWriter(&buf)
	used := map[string]bool{}
	for index, item := range items {
		fallback := fmt.Sprintf("account-%03d", index+1)
		raw := fallback
		for _, key := range []string{"email", "account_id"} {
			if text, _ := item[key].(string); text != "" {
				raw = text
				break
			}
		}
		base := safeExportName(raw, fallback)
		name := base
		for suffix := 2; used[name]; suffix++ {
			name = fmt.Sprintf("%s-%d", base, suffix)
		}
		used[name] = true
		writer, _ := archive.Create(name + ".json")
		data, _ := json.MarshalIndent(item, "", "  ")
		writer.Write(append(data, '\n'))
	}
	archive.Close()
	return buf.Bytes()
}

// registerAccounts serves the /api/accounts* routes of api/accounts.py (CPA, sub2api, OAuth and
// protocol login come with step 10).
func registerAccounts(admin *gin.RouterGroup, d *Deps) {
	p := d.Pool
	admin.GET("/api/accounts", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"items": p.ListAccounts()}) })

	admin.POST("/api/accounts", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		rawTokens, okTokens := stringList(body, "tokens")
		distribute, okDistribute := stringList(body, "distribute_proxies")
		proxy, okProxy := optionalString(body, "proxy")
		rawAccounts, _ := body["accounts"].([]any)
		if !okTokens || !okDistribute || !okProxy {
			abortValidation(c, "tokens", "Input should be a valid list")
			return
		}
		proxyText, _ := proxy.(string)
		payloads := []map[string]any{}
		payloadTokens := []string{}
		for _, item := range rawAccounts {
			if m, isMap := item.(map[string]any); isMap {
				payloads = append(payloads, m)
				payloadTokens = append(payloadTokens, payloadToken(m))
			}
		}
		tokens := uniqueTrimmed(append(append([]string{}, rawTokens...), payloadTokens...))
		if len(tokens) == 0 {
			abortDetail(c, http.StatusBadRequest, "tokens is required")
			return
		}
		proxyFor, release, err := importProxies(d, tokens, proxyText, distribute)
		if err != nil {
			abortDetail(c, http.StatusBadRequest, err.Error())
			return
		}
		var result pool.AddResult
		if len(payloads) > 0 {
			items := make([]pool.Account, 0, len(payloads))
			for i, item := range payloads {
				copied := pool.Account{}
				for key, value := range item {
					copied[key] = value
				}
				if proxyFor[payloadTokens[i]] != "" {
					copied["proxy"] = proxyFor[payloadTokens[i]]
				} else if len(distribute) > 0 {
					// with distribution, existing accounts keep their proxy (even none), not the one in the JSON
					delete(copied, "proxy")
				}
				items = append(items, copied)
			}
			result = p.AddAccountItems(items)
			inPayload := map[string]bool{}
			for _, token := range uniqueTrimmed(payloadTokens) {
				inPayload[token] = true
			}
			extra := []string{}
			for _, token := range tokens {
				if !inPayload[token] {
					extra = append(extra, token)
				}
			}
			if len(extra) > 0 {
				more := addPlainTokens(p, extra, proxyFor)
				result.Added += more.Added
				result.Skipped += more.Skipped
			}
		} else {
			result = addPlainTokens(p, tokens, proxyFor)
		}
		release()
		refreshed := p.RefreshAccounts(c.Request.Context(), tokens, "", true)
		out := addResultMap(result)
		out["refreshed"] = refreshed.Refreshed
		errs := make([]any, len(refreshed.Errors))
		for i, item := range refreshed.Errors {
			errs[i] = item
		}
		out["errors"] = errs
		out["items"] = refreshed.Items
		c.JSON(http.StatusOK, out)
	})

	admin.DELETE("/api/accounts", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		raw, valid := stringList(body, "tokens")
		if !valid {
			abortValidation(c, "tokens", "Input should be a valid list")
			return
		}
		tokens := []string{}
		for _, token := range raw {
			if token = strings.TrimSpace(token); token != "" {
				tokens = append(tokens, token)
			}
		}
		if len(tokens) == 0 {
			abortDetail(c, http.StatusBadRequest, "tokens is required")
			return
		}
		removed, items := p.DeleteAccounts(tokens)
		c.JSON(http.StatusOK, gin.H{"removed": removed, "items": items})
	})

	accessTokens := func(c *gin.Context) ([]string, bool) {
		body, ok := bindJSON(c)
		if !ok {
			return nil, false
		}
		raw, valid := stringList(body, "access_tokens")
		if !valid {
			abortValidation(c, "access_tokens", "Input should be a valid list")
			return nil, false
		}
		out := []string{}
		for _, token := range raw {
			if token = strings.TrimSpace(token); token != "" {
				out = append(out, token)
			}
		}
		return out, true
	}
	progress := func(get func(string) map[string]any) gin.HandlerFunc {
		return func(c *gin.Context) {
			value := get(c.Param("progress_id"))
			if value == nil {
				abortDetail(c, http.StatusNotFound, "progress not found")
				return
			}
			c.JSON(http.StatusOK, value)
		}
	}

	admin.POST("/api/accounts/refresh", func(c *gin.Context) {
		tokens, ok := accessTokens(c)
		if !ok {
			return
		}
		if len(tokens) == 0 {
			tokens = p.ListTokens()
		}
		if len(tokens) == 0 {
			abortDetail(c, http.StatusBadRequest, "access_tokens is required")
			return
		}
		id := uuid.NewString()
		// the progress exists before the id is returned, so the first poll never sees a 404
		p.InitRefreshProgress(id, len(tokens))
		go func() {
			defer func() {
				if r := recover(); r != nil {
					p.FinishRefreshProgress(id, nil, fmt.Sprint(r))
				}
			}()
			p.RefreshAccounts(context.Background(), tokens, id, false)
		}()
		c.JSON(http.StatusOK, gin.H{"progress_id": id})
	})
	admin.GET("/api/accounts/refresh/progress/:progress_id", progress(p.RefreshProgress))

	admin.POST("/api/accounts/re-login", func(c *gin.Context) {
		tokens, ok := accessTokens(c)
		if !ok {
			return
		}
		if len(tokens) == 0 {
			abortDetail(c, http.StatusBadRequest, "access_tokens is required")
			return
		}
		id := uuid.NewString()
		p.InitReloginProgress(id, len(tokens))
		go p.ReloginAccounts(tokens, id)
		c.JSON(http.StatusOK, gin.H{"progress_id": id})
	})
	admin.GET("/api/accounts/re-login/progress/:progress_id", progress(p.ReloginProgress))

	admin.POST("/api/accounts/image-test", func(c *gin.Context) {
		tokens, ok := accessTokens(c)
		if !ok {
			return
		}
		if len(tokens) == 0 {
			abortDetail(c, http.StatusBadRequest, "access_tokens is required")
			return
		}
		id := uuid.NewString()
		p.InitReloginProgress(id, len(tokens))
		go p.TestImageAccounts(context.Background(), tokens, id, d.ImageProbe)
		c.JSON(http.StatusOK, gin.H{"progress_id": id})
	})
	admin.GET("/api/accounts/image-test/progress/:progress_id", progress(p.ReloginProgress))

	admin.POST("/api/accounts/export", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		raw, valid := stringList(body, "access_tokens")
		format, _ := body["format"].(string)
		if format == "" {
			format = "json"
		}
		if !valid || (format != "json" && format != "zip") {
			abortValidation(c, "format", "Input should be 'json' or 'zip'")
			return
		}
		items := p.BuildExportItems(uniqueTrimmed(raw))
		if len(items) == 0 {
			abortDetail(c, http.StatusBadRequest, "没有可导出的完整账号，需要同时有 access_token、refresh_token 和 id_token")
			return
		}
		stamp := time.Now().Format("20060102-150405")
		if format == "zip" {
			c.Header("Content-Disposition", `attachment; filename="codex-accounts-`+stamp+`.zip"`)
			c.Data(http.StatusOK, "application/zip", accountZip(items))
			return
		}
		var payload any = items
		if len(items) == 1 {
			payload = items[0]
		}
		data, _ := json.MarshalIndent(payload, "", "  ")
		c.Header("Content-Disposition", `attachment; filename="codex-accounts-`+stamp+`.json"`)
		c.Data(http.StatusOK, "application/json", append(data, '\n'))
	})

	admin.POST("/api/accounts/update", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		token, _ := body["access_token"].(string)
		token = strings.TrimSpace(token)
		if token == "" {
			abortDetail(c, http.StatusBadRequest, "access_token is required")
			return
		}
		updates := pool.Account{}
		for _, field := range []string{"type", "status", "proxy"} {
			value, valid := optionalString(body, field)
			if !valid {
				abortValidation(c, field, "Input should be a valid string")
				return
			}
			if value != nil {
				updates[field] = value
			}
		}
		if value, present := body["quota"]; present && value != nil {
			quota, valid := pydanticInt(value)
			if !valid {
				abortValidation(c, "quota", "Input should be a valid integer")
				return
			}
			updates["quota"] = quota
		}
		if len(updates) == 0 {
			abortDetail(c, http.StatusBadRequest, "还没有检测到改动，请修改后再保存")
			return
		}
		account := p.UpdateAccount(token, updates, false)
		if account == nil {
			abortDetail(c, http.StatusNotFound, "account not found")
			return
		}
		items := p.ListAccounts()
		var item any = account
		for _, view := range items {
			if view["access_token"] == account["access_token"] {
				item = view
				break
			}
		}
		c.JSON(http.StatusOK, gin.H{"item": item, "items": items})
	})
}

// importProxies is _import_proxies: the proxy each imported token gets, and a release for the
// reservation made while distributing.
func importProxies(d *Deps, tokens []string, proxy string, distribute []string) (map[string]string, func(), error) {
	noop := func() {}
	proxy = strings.TrimSpace(proxy)
	if len(distribute) == 0 {
		out := map[string]string{}
		if proxy != "" {
			for _, token := range tokens {
				out[token] = proxy
			}
		}
		return out, noop, nil
	}
	if d.Proxies == nil {
		return nil, noop, fmt.Errorf("代理清单还没有接入")
	}
	fresh := []string{}
	for _, token := range tokens {
		if d.Pool.GetAccount(token) == nil {
			fresh = append(fresh, token)
		}
	}
	assigned, err := d.Proxies.Reserve(distribute, len(fresh))
	if err != nil {
		return nil, noop, err
	}
	out := map[string]string{}
	for i, token := range fresh {
		if i < len(assigned) {
			out[token] = assigned[i]
		}
	}
	return out, func() { d.Proxies.Release(assigned) }, nil
}
