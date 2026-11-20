package compat

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/basketikun/chatgpt2api/internal/sources"
)

// The CPA and sub2api routes of api/accounts.py and POST /api/proxy/clearance/test.

// optionalFields reads the str | None fields of an update model (model_dump(exclude_none=True)).
func optionalFields(c *gin.Context, body map[string]any, fields ...string) (map[string]any, bool) {
	out := map[string]any{}
	for _, field := range fields {
		value, present := body[field]
		if !present || value == nil {
			continue
		}
		text, ok := value.(string)
		if !ok {
			abortValidation(c, field, "Input should be a valid string")
			return nil, false
		}
		out[field] = text
	}
	return out, true
}

func stringField(body map[string]any, field string) string {
	text, _ := body[field].(string)
	return text
}

func sanitizedList(items []map[string]any) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

func registerSources(admin *gin.RouterGroup, d *Deps) {
	cpa, sub := d.CPA, d.Sub2API
	admin.GET("/api/cpa/pools", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"pools": sanitizedList(cpa.Pools())}) })
	admin.POST("/api/cpa/pools", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok || !checkOptional(c, body, []string{"name", "base_url", "secret_key"}, nil, nil, nil, nil) {
			return
		}
		if strings.TrimSpace(stringField(body, "base_url")) == "" {
			abortDetail(c, http.StatusBadRequest, "base_url is required")
			return
		}
		if strings.TrimSpace(stringField(body, "secret_key")) == "" {
			abortDetail(c, http.StatusBadRequest, "secret_key is required")
			return
		}
		item, err := cpa.Add(stringField(body, "name"), stringField(body, "base_url"), stringField(body, "secret_key"))
		if err != nil {
			abortDetail(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"pool": sources.SanitizePool(item), "pools": sanitizedList(cpa.Pools())})
	})
	admin.POST("/api/cpa/pools/:pool_id", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		updates, ok := optionalFields(c, body, "name", "base_url", "secret_key")
		if !ok {
			return
		}
		item, err := cpa.Update(c.Param("pool_id"), updates)
		if err != nil {
			abortDetail(c, http.StatusInternalServerError, err.Error())
			return
		}
		if item == nil {
			abortDetail(c, http.StatusNotFound, "pool not found")
			return
		}
		c.JSON(http.StatusOK, gin.H{"pool": sources.SanitizePool(item), "pools": sanitizedList(cpa.Pools())})
	})
	admin.DELETE("/api/cpa/pools/:pool_id", func(c *gin.Context) {
		if removed, _ := cpa.Delete(c.Param("pool_id")); !removed {
			abortDetail(c, http.StatusNotFound, "pool not found")
			return
		}
		c.JSON(http.StatusOK, gin.H{"pools": sanitizedList(cpa.Pools())})
	})
	cpaPool := func(c *gin.Context) map[string]any {
		item := cpa.Get(c.Param("pool_id"))
		if item == nil {
			abortDetail(c, http.StatusNotFound, "pool not found")
		}
		return item
	}
	admin.GET("/api/cpa/pools/:pool_id/files", func(c *gin.Context) {
		item := cpaPool(c)
		if item == nil {
			return
		}
		files, err := cpa.Files(c.Request.Context(), item)
		if err != nil {
			// Python lets the exception through as a 500
			abortDetail(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"pool_id": c.Param("pool_id"), "files": sanitizedList(files)})
	})
	admin.POST("/api/cpa/pools/:pool_id/import", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		names, okNames := stringList(body, "names")
		if !okNames {
			abortValidation(c, "names", "Input should be a valid list")
			return
		}
		item := cpaPool(c)
		if item == nil {
			return
		}
		job, err := cpa.Import(item, names)
		if err != nil {
			abortDetail(c, http.StatusBadRequest, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"import_job": job})
	})
	admin.GET("/api/cpa/pools/:pool_id/import", func(c *gin.Context) {
		if item := cpaPool(c); item != nil {
			c.JSON(http.StatusOK, gin.H{"import_job": cpa.Job(c.Param("pool_id"))})
		}
	})

	serverFields := []string{"name", "base_url", "email", "password", "api_key", "group_id"}
	admin.GET("/api/sub2api/servers", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"servers": sanitizedList(sub.Servers())}) })
	admin.POST("/api/sub2api/servers", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok || !checkOptional(c, body, serverFields, nil, nil, nil, nil) {
			return
		}
		if strings.TrimSpace(stringField(body, "base_url")) == "" {
			abortDetail(c, http.StatusBadRequest, "base_url is required")
			return
		}
		hasLogin := strings.TrimSpace(stringField(body, "email")) != "" && strings.TrimSpace(stringField(body, "password")) != ""
		if !hasLogin && strings.TrimSpace(stringField(body, "api_key")) == "" {
			abortDetail(c, http.StatusBadRequest, "email+password or api_key is required")
			return
		}
		fields := map[string]any{}
		for _, field := range serverFields {
			fields[field] = stringField(body, field)
		}
		item, err := sub.Add(fields)
		if err != nil {
			abortDetail(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"server": sources.SanitizeServer(item), "servers": sanitizedList(sub.Servers())})
	})
	admin.POST("/api/sub2api/servers/:server_id", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		updates, ok := optionalFields(c, body, serverFields...)
		if !ok {
			return
		}
		item, err := sub.Update(c.Param("server_id"), updates)
		if err != nil {
			abortDetail(c, http.StatusInternalServerError, err.Error())
			return
		}
		if item == nil {
			abortDetail(c, http.StatusNotFound, "server not found")
			return
		}
		c.JSON(http.StatusOK, gin.H{"server": sources.SanitizeServer(item), "servers": sanitizedList(sub.Servers())})
	})
	admin.DELETE("/api/sub2api/servers/:server_id", func(c *gin.Context) {
		if removed, _ := sub.Delete(c.Param("server_id")); !removed {
			abortDetail(c, http.StatusNotFound, "server not found")
			return
		}
		c.JSON(http.StatusOK, gin.H{"servers": sanitizedList(sub.Servers())})
	})
	server := func(c *gin.Context) map[string]any {
		item := sub.Get(c.Param("server_id"))
		if item == nil {
			abortDetail(c, http.StatusNotFound, "server not found")
		}
		return item
	}
	admin.GET("/api/sub2api/servers/:server_id/groups", func(c *gin.Context) {
		item := server(c)
		if item == nil {
			return
		}
		groups, err := sub.Groups(c.Request.Context(), item)
		if err != nil {
			abortDetail(c, http.StatusBadGateway, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"server_id": c.Param("server_id"), "groups": sanitizedList(groups)})
	})
	admin.GET("/api/sub2api/servers/:server_id/accounts", func(c *gin.Context) {
		item := server(c)
		if item == nil {
			return
		}
		accounts, err := sub.RemoteAccounts(c.Request.Context(), item)
		if err != nil {
			abortDetail(c, http.StatusBadGateway, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"server_id": c.Param("server_id"), "accounts": sanitizedList(accounts)})
	})
	admin.POST("/api/sub2api/servers/:server_id/import", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		ids, okIDs := stringList(body, "account_ids")
		if !okIDs {
			abortValidation(c, "account_ids", "Input should be a valid list")
			return
		}
		item := server(c)
		if item == nil {
			return
		}
		job, err := sub.Import(item, ids)
		if err != nil {
			abortDetail(c, http.StatusBadRequest, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"import_job": job})
	})
	admin.GET("/api/sub2api/servers/:server_id/import", func(c *gin.Context) {
		if item := server(c); item != nil {
			c.JSON(http.StatusOK, gin.H{"import_job": sub.Job(c.Param("server_id"))})
		}
	})

	admin.POST("/api/proxy/clearance/test", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok || !checkOptional(c, body, []string{"target_url"}, nil, nil, nil, nil) {
			return
		}
		target, present := body["target_url"].(string)
		if !present {
			target = "https://chatgpt.com"
		}
		c.JSON(http.StatusOK, gin.H{"result": clearanceTest(c.Request.Context(), d, target)})
	})
}

var credentialsInURL = regexp.MustCompile(`(?i)((?:https?|socks5h?|socks)://)([^\s/@:]+):([^\s/@]+)@`)

// clearanceTest is proxy_service.test_clearance.
func clearanceTest(ctx context.Context, d *Deps, target string) map[string]any {
	if target = strings.TrimSpace(target); target == "" {
		target = "https://chatgpt.com"
	}
	result := func(ok bool, status string, latency int64, cookies bool, agent string, errText any) map[string]any {
		return map[string]any{"ok": ok, "status": status, "latency_ms": latency, "has_cookies": cookies, "user_agent": agent, "error": errText,
			"runtime": runtimeStatus(d.Settings)}
	}
	if enabled, _ := runtimeStatus(d.Settings)["clearance_enabled"].(bool); !enabled || d.RefreshClearance == nil {
		return result(false, "disabled", 0, false, "", "clearance is disabled")
	}
	started := time.Now()
	agent, cookies, err := d.RefreshClearance(ctx, target)
	latency := time.Since(started).Milliseconds()
	if err != nil {
		message := err.Error()
		if message == "" {
			message = "RuntimeError"
		}
		return result(false, "error", latency, false, "", credentialsInURL.ReplaceAllString(message, "${1}[REDACTED]@"))
	}
	return result(true, "ok", latency, len(cookies) > 0, agent, nil)
}
