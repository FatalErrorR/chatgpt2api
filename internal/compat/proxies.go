package compat

import (
	"net/http"

	"github.com/basketikun/chatgpt2api/internal/proxylist"
	"github.com/basketikun/chatgpt2api/internal/proxyops"
	"github.com/basketikun/chatgpt2api/internal/settings"
	"github.com/gin-gonic/gin"
)

func isProxyInput(err error) bool {
	if proxylist.IsInputError(err) {
		return true
	}
	_, ok := err.(proxyops.ErrReassign)
	return ok
}

func abortProxyErr(c *gin.Context, err error) {
	if isProxyInput(err) {
		abortDetail(c, http.StatusBadRequest, err.Error())
		return
	}
	abortDetail(c, http.StatusInternalServerError, err.Error())
}

func withItems(ops *proxyops.Ops, out map[string]any) gin.H {
	result := gin.H{}
	for key, value := range out {
		result[key] = value
	}
	result["items"] = ops.Entries()
	return result
}

// registerProxies serves api/proxies.py, /api/accounts/reassign-proxy and the /api/proxy/* runtime routes.
func registerProxies(admin *gin.RouterGroup, d *Deps) {
	ops := d.ProxyOps
	entry := func(c *gin.Context) (string, string, bool) {
		body, ok := bindJSON(c)
		if !ok {
			return "", "", false
		}
		url, validURL := optionalString(body, "url")
		name, validName := optionalString(body, "name")
		if !validURL || !validName {
			abortValidation(c, "url", "Input should be a valid string")
			return "", "", false
		}
		u, _ := url.(string)
		n, _ := name.(string)
		return u, n, true
	}
	urlsOf := func(c *gin.Context) ([]string, map[string]any, bool) {
		body, ok := bindJSON(c)
		if !ok {
			return nil, nil, false
		}
		urls, valid := stringList(body, "urls")
		if !valid {
			abortValidation(c, "urls", "Input should be a valid list")
			return nil, nil, false
		}
		return urls, body, true
	}
	list := func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"items": ops.Entries()}) }

	admin.GET("/api/proxies", list)
	admin.POST("/api/proxies", func(c *gin.Context) {
		url, name, ok := entry(c)
		if !ok {
			return
		}
		if err := ops.List.Add(url, name); err != nil {
			abortProxyErr(c, err)
			return
		}
		list(c)
	})
	admin.POST("/api/proxies/batch", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		text, _ := body["text"].(string)
		vendor, _ := body["vendor"].(string)
		result, err := ops.List.AddBatch(text, vendor)
		if err != nil {
			abortProxyErr(c, err)
			return
		}
		c.JSON(http.StatusOK, withItems(ops, result))
	})
	admin.POST("/api/proxies/test", func(c *gin.Context) {
		urls, _, ok := urlsOf(c)
		if ok {
			c.JSON(http.StatusOK, ops.TestProxies(c.Request.Context(), urls))
		}
	})
	admin.POST("/api/proxies/check", func(c *gin.Context) {
		c.JSON(http.StatusOK, withItems(ops, ops.Check(c.Request.Context())))
	})
	admin.POST("/api/proxies/retire", func(c *gin.Context) {
		urls, body, ok := urlsOf(c)
		if !ok {
			return
		}
		dryRun := true
		if value, present := body["dry_run"]; present && value != nil {
			if dryRun, ok = pydanticBool(value); !ok {
				abortValidation(c, "dry_run", "Input should be a valid boolean")
				return
			}
		}
		result, err := ops.Retire(urls, dryRun)
		if err != nil {
			abortProxyErr(c, err)
			return
		}
		c.JSON(http.StatusOK, withItems(ops, result))
	})
	admin.POST("/api/proxies/enable", func(c *gin.Context) {
		urls, _, ok := urlsOf(c)
		if ok {
			c.JSON(http.StatusOK, withItems(ops, map[string]any{"enabled": ops.Enable(urls)}))
		}
	})
	admin.POST("/api/proxies/update", func(c *gin.Context) {
		url, name, ok := entry(c)
		if !ok {
			return
		}
		if err := ops.List.Rename(url, name); err != nil {
			abortProxyErr(c, err)
			return
		}
		list(c)
	})
	admin.POST("/api/proxies/delete", func(c *gin.Context) {
		url, _, ok := entry(c)
		if !ok {
			return
		}
		if err := ops.List.Delete(url, d.Pool.ProxyAccountCounts()); err != nil {
			abortProxyErr(c, err)
			return
		}
		list(c)
	})

	admin.POST("/api/accounts/reassign-proxy", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		tokens, validTokens := stringList(body, "tokens")
		proxies, validProxies := stringList(body, "proxies")
		if !validTokens || !validProxies {
			abortValidation(c, "tokens", "Input should be a valid list")
			return
		}
		dryRun := true
		if value, present := body["dry_run"]; present && value != nil {
			if dryRun, ok = pydanticBool(value); !ok {
				abortValidation(c, "dry_run", "Input should be a valid boolean")
				return
			}
		}
		result, err := ops.Reassign(tokens, proxies, dryRun)
		if err != nil {
			abortProxyErr(c, err)
			return
		}
		if !dryRun {
			result["items"] = d.Pool.ListAccounts()
		}
		c.JSON(http.StatusOK, result)
	})

	admin.POST("/api/proxy/test", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		url, _ := body["url"].(string)
		c.JSON(http.StatusOK, gin.H{"result": ops.Test(c.Request.Context(), url)})
	})
	runtime := func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"runtime": d.Settings.PublicProxyRuntime(), "status": runtimeStatus(d.Settings)})
	}
	admin.GET("/api/proxy/runtime", runtime)
	admin.POST("/api/proxy/runtime", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		if _, err := d.Settings.Update(map[string]any{"proxy_runtime": body}); err != nil {
			if settings.IsValidationError(err) {
				abortDetail(c, http.StatusBadRequest, err.Error())
				return
			}
			abortDetail(c, http.StatusInternalServerError, err.Error())
			return
		}
		runtime(c)
	})
}
