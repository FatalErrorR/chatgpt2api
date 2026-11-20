package compat

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// registerLogs is the /api/logs routes of api/system.py.
func registerLogs(admin *gin.RouterGroup, d *Deps) {
	admin.GET("/api/logs", func(c *gin.Context) {
		items, err := d.Logs.List(strings.TrimSpace(c.Query("type")), strings.TrimSpace(c.Query("start_date")), strings.TrimSpace(c.Query("end_date")), 200)
		if err != nil {
			abortDetail(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	})
	admin.GET("/api/logs/image-counts", func(c *gin.Context) {
		counts, err := d.Logs.ImageOutputCounts(time.Now())
		if err != nil {
			abortDetail(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.JSON(http.StatusOK, counts)
	})
	admin.POST("/api/logs/delete", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		ids, okIDs := stringList(body, "ids")
		if !okIDs {
			abortValidation(c, "ids", "Input should be a valid list")
			return
		}
		trimmed := make([]string, 0, len(ids))
		for _, id := range ids {
			trimmed = append(trimmed, strings.TrimSpace(id))
		}
		removed, err := d.Logs.Delete(trimmed)
		if err != nil {
			abortDetail(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"removed": removed})
	})
}
