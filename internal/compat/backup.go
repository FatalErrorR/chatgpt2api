package compat

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/basketikun/chatgpt2api/internal/backup"
)

// The backup routes of api/system.py.

func abortBackup(c *gin.Context, err error) {
	var backupErr *backup.Error
	if errors.As(err, &backupErr) {
		abortDetail(c, http.StatusBadRequest, backupErr.Message)
		return
	}
	abortDetail(c, http.StatusInternalServerError, err.Error())
}

func registerBackup(admin *gin.RouterGroup, d *Deps) {
	b := d.Backup
	admin.POST("/api/backup/test", func(c *gin.Context) {
		result, err := b.Test()
		if err != nil {
			abortBackup(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"result": result})
	})
	admin.GET("/api/backups", func(c *gin.Context) {
		items, err := b.List()
		if err != nil {
			abortBackup(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items, "state": b.Status(), "settings": b.PublicSettings()})
	})
	admin.POST("/api/backups/run", func(c *gin.Context) {
		result, err := b.Run("manual")
		if err != nil {
			abortBackup(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"result": result})
	})
	admin.POST("/api/backups/delete", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok || !checkOptional(c, body, []string{"key"}, nil, nil, nil, nil) {
			return
		}
		key, _ := body["key"].(string)
		if err := b.Delete(key); err != nil {
			abortBackup(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	admin.GET("/api/backups/detail", func(c *gin.Context) {
		item, err := b.Detail(c.Query("key"))
		if err != nil {
			abortBackup(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"item": item})
	})
	admin.GET("/api/backups/download", func(c *gin.Context) {
		item, err := b.Download(c.Query("key"))
		if err != nil {
			abortBackup(c, err)
			return
		}
		payload := item["payload"].([]byte)
		name, _ := item["name"].(string)
		if name == "" {
			name = "backup.bin"
		}
		c.Header("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
		c.Header("Content-Length", strconv.Itoa(len(payload)))
		c.Data(http.StatusOK, item["content_type"].(string), payload)
	})
}
