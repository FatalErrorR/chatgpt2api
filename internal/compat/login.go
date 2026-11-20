package compat

import (
	"errors"
	"net/http"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/openailogin"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/gin-gonic/gin"
)

// The login routes of api/accounts.py: batch protocol login and the manual OAuth bridge.

func registerLogin(admin *gin.RouterGroup, d *Deps) {
	admin.POST("/api/accounts/protocol-login", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok || !checkOptional(c, body, []string{"text", "proxy"}, nil, nil, nil, nil) {
			return
		}
		distribute, okList := stringList(body, "distribute_proxies")
		if !okList {
			abortValidation(c, "distribute_proxies", "Input should be a valid list")
			return
		}
		text, _ := body["text"].(string)
		proxy, _ := body["proxy"].(string)
		if strings.TrimSpace(text) == "" {
			abortDetail(c, http.StatusBadRequest, "text is required")
			return
		}
		result, err := d.ProtocolLogin.Start(text, proxy, distribute)
		if err != nil {
			abortDetail(c, http.StatusBadRequest, err.Error())
			return
		}
		c.JSON(http.StatusOK, result)
	})
	admin.GET("/api/accounts/protocol-login/progress/:progress_id", func(c *gin.Context) {
		progress := d.ProtocolLogin.Progress(c.Param("progress_id"))
		if progress == nil {
			abortDetail(c, http.StatusNotFound, "progress not found")
			return
		}
		c.JSON(http.StatusOK, progress)
	})
	admin.POST("/api/accounts/oauth/start", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok || !checkOptional(c, body, []string{"email_hint"}, nil, nil, nil, nil) {
			return
		}
		hint, _ := body["email_hint"].(string)
		c.JSON(http.StatusOK, d.OAuth.Start(hint))
	})
	admin.POST("/api/accounts/oauth/finish", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok || !checkOptional(c, body, []string{"session_id", "callback", "proxy"}, nil, nil, nil, nil) {
			return
		}
		distribute, okList := stringList(body, "distribute_proxies")
		if !okList {
			abortValidation(c, "distribute_proxies", "Input should be a valid list")
			return
		}
		sessionID, _ := body["session_id"].(string)
		callback, _ := body["callback"].(string)
		proxy, _ := body["proxy"].(string)
		tokens, err := d.OAuth.Finish(c.Request.Context(), sessionID, callback)
		if err != nil {
			var oauthErr *openailogin.OAuthError
			if errors.As(err, &oauthErr) {
				abortDetail(c, http.StatusBadRequest, oauthErr.Message)
				return
			}
			abortDetail(c, http.StatusInternalServerError, err.Error())
			return
		}
		token := tokens["access_token"]
		payload := pool.Account{"access_token": token, "refresh_token": tokens["refresh_token"], "id_token": tokens["id_token"], "source_type": "oauth_login"}
		proxyFor, release, err := importProxies(d, []string{token}, proxy, distribute)
		if err != nil {
			abortDetail(c, http.StatusBadRequest, err.Error())
			return
		}
		if proxyFor[token] != "" {
			payload["proxy"] = proxyFor[token]
		}
		added := d.Pool.AddAccountItems([]pool.Account{payload})
		release()
		refreshed := d.Pool.RefreshAccounts(c.Request.Context(), []string{token}, "", false)
		out := addResultMap(added)
		out["refreshed"] = refreshed.Refreshed
		errs := make([]any, len(refreshed.Errors))
		for i, item := range refreshed.Errors {
			errs[i] = item
		}
		out["errors"] = errs
		out["items"] = refreshed.Items
		c.JSON(http.StatusOK, out)
	})
}
