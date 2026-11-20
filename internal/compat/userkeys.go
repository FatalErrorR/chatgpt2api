package compat

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func optionalString(body map[string]any, key string) (any, bool) {
	value, present := body[key]
	if !present || value == nil {
		return nil, true
	}
	text, ok := value.(string)
	return text, ok
}

// registerUserKeys serves /api/auth/users* (api/accounts.py).
func registerUserKeys(admin *gin.RouterGroup, d *Deps) {
	list := func(c *gin.Context) ([]map[string]any, bool) {
		items, err := d.Auth.List("user")
		if err != nil {
			abortDetail(c, http.StatusInternalServerError, err.Error())
			return nil, false
		}
		return items, true
	}
	admin.GET("/api/auth/users", func(c *gin.Context) {
		if items, ok := list(c); ok {
			c.JSON(http.StatusOK, gin.H{"items": items})
		}
	})
	admin.POST("/api/auth/users", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		name, valid := optionalString(body, "name")
		if !valid {
			abortValidation(c, "name", "Input should be a valid string")
			return
		}
		text, _ := name.(string)
		item, raw, err := d.Auth.Create("user", text)
		if err != nil {
			status := http.StatusInternalServerError
			if isInputError(err) {
				status = http.StatusBadRequest
			}
			abortDetail(c, status, err.Error())
			return
		}
		if items, ok := list(c); ok {
			c.JSON(http.StatusOK, gin.H{"item": item, "key": raw, "items": items})
		}
	})
	admin.POST("/api/auth/users/:key_id", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		updates := map[string]any{}
		for _, field := range []string{"name", "key"} {
			value, valid := optionalString(body, field)
			if !valid {
				abortValidation(c, field, "Input should be a valid string")
				return
			}
			if value != nil {
				updates[field] = value
			}
		}
		if value, present := body["enabled"]; present && value != nil {
			enabled, valid := pydanticBool(value)
			if !valid {
				abortValidation(c, "enabled", "Input should be a valid boolean")
				return
			}
			updates["enabled"] = enabled
		}
		if len(updates) == 0 {
			abortDetail(c, http.StatusBadRequest, "还没有检测到改动，请修改后再保存")
			return
		}
		item, err := d.Auth.Update(c.Param("key_id"), updates, "user")
		if err != nil {
			status := http.StatusInternalServerError
			if isInputError(err) {
				status = http.StatusBadRequest
			}
			abortDetail(c, status, err.Error())
			return
		}
		if item == nil {
			abortDetail(c, http.StatusNotFound, "这条用户密钥不存在，可能已经被删除")
			return
		}
		if items, ok := list(c); ok {
			c.JSON(http.StatusOK, gin.H{"item": item, "items": items})
		}
	})
	admin.DELETE("/api/auth/users/:key_id", func(c *gin.Context) {
		deleted, err := d.Auth.Delete(c.Param("key_id"), "user")
		if err != nil {
			abortDetail(c, http.StatusInternalServerError, err.Error())
			return
		}
		if !deleted {
			abortDetail(c, http.StatusNotFound, "这条用户密钥不存在，可能已经被删除")
			return
		}
		if items, ok := list(c); ok {
			c.JSON(http.StatusOK, gin.H{"items": items})
		}
	})
}
