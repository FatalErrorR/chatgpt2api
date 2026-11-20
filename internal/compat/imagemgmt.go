package compat

import (
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/imagestore"
	"github.com/gin-gonic/gin"
)

// Image management: the /api/images*, /images/*, /image-thumbnails/* and /api/image-storage/*
// routes of api/system.py.

var corsHeaders = map[string]string{
	"Access-Control-Allow-Origin":  "*",
	"Access-Control-Allow-Methods": "GET, OPTIONS",
	"Access-Control-Allow-Headers": "*",
}

func setCORS(c *gin.Context) {
	for key, value := range corsHeaders {
		c.Header(key, value)
	}
}

// abortImageError answers like the Python services: HTTPException(detail="…") is {"detail": "…"},
// ImageStorageError is a 400 {"detail": {"error": …}} where the route catches it.
func abortImageError(c *gin.Context, err error) {
	if httpErr, ok := imagestore.IsHTTPError(err); ok {
		c.AbortWithStatusJSON(httpErr.Status, gin.H{"detail": httpErr.Detail})
		return
	}
	abortDetail(c, http.StatusInternalServerError, err.Error())
}

// queryInt / queryBool parse FastAPI query parameters, answering 422 like FastAPI on bad input.
func queryInt(c *gin.Context, name string, fallback int) (int, bool) {
	raw, present := c.GetQuery(name)
	if !present {
		return fallback, true
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		c.AbortWithStatusJSON(422, gin.H{"detail": []gin.H{{"type": "int_parsing", "loc": []string{"query", name},
			"msg": "Input should be a valid integer, unable to parse string as an integer", "input": raw}}})
		return 0, false
	}
	return value, true
}

func queryBool(c *gin.Context, name string) (bool, bool) {
	raw, present := c.GetQuery(name)
	if !present {
		return false, true
	}
	value, ok := pydanticBool(raw)
	if !ok {
		c.AbortWithStatusJSON(422, gin.H{"detail": []gin.H{{"type": "bool_parsing", "loc": []string{"query", name},
			"msg": "Input should be a valid boolean, unable to interpret input", "input": raw}}})
		return false, false
	}
	return value, true
}

func splitTags(value string) []string {
	tags := []string{}
	for _, tag := range strings.Split(value, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

func registerImageManagement(engine *gin.Engine, admin *gin.RouterGroup, d *Deps) {
	store := d.ImageStore
	admin.GET("/api/images", func(c *gin.Context) {
		page, ok := queryInt(c, "page", 0)
		if !ok {
			return
		}
		pageSize, ok := queryInt(c, "page_size", 48)
		if !ok {
			return
		}
		refresh, ok := queryBool(c, "refresh")
		if !ok {
			return
		}
		result, err := store.ListImages(resolveImageBaseURL(c, d), strings.TrimSpace(c.Query("start_date")), strings.TrimSpace(c.Query("end_date")),
			splitTags(c.Query("tags")), page, pageSize, refresh)
		if err != nil {
			abortImageError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	admin.GET("/api/images/paths", func(c *gin.Context) {
		paths, err := store.ImagePaths(strings.TrimSpace(c.Query("start_date")), strings.TrimSpace(c.Query("end_date")), splitTags(c.Query("tags")))
		if err != nil {
			abortImageError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"paths": paths})
	})
	engine.GET("/images/*image_path", func(c *gin.Context) {
		rel := strings.TrimPrefix(c.Param("image_path"), "/")
		local, err := store.HasLocal(rel)
		if err != nil {
			abortImageError(c, err)
			return
		}
		if local {
			full, _ := imagestore.LocalPath(store, rel)
			setCORS(c)
			c.File(full)
			return
		}
		data, err := store.GetBytes(rel)
		if err != nil {
			abortImageError(c, err)
			return
		}
		mediaType := "image/png"
		if strings.HasSuffix(strings.ToLower(rel), ".gif") {
			mediaType = "image/gif"
		}
		setCORS(c)
		c.Data(http.StatusOK, mediaType, data)
	})
	engine.GET("/image-thumbnails/*image_path", func(c *gin.Context) {
		thumb, err := store.EnsureThumbnail(strings.TrimPrefix(c.Param("image_path"), "/"))
		if err != nil {
			abortImageError(c, err)
			return
		}
		setCORS(c)
		c.File(thumb)
	})
	admin.POST("/api/images/delete", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		paths, okPaths := stringList(body, "paths")
		if !okPaths {
			abortValidation(c, "paths", "Input should be a valid list")
			return
		}
		start, okStart := optionalString(body, "start_date")
		end, okEnd := optionalString(body, "end_date")
		if !okStart || !okEnd {
			abortValidation(c, "start_date", "Input should be a valid string")
			return
		}
		allMatching := false
		if raw, present := body["all_matching"]; present {
			value, okBool := pydanticBool(raw)
			if !okBool {
				abortValidation(c, "all_matching", "Input should be a valid boolean")
				return
			}
			allMatching = value
		}
		startText, _ := start.(string)
		endText, _ := end.(string)
		removed, err := store.DeleteImages(paths, strings.TrimSpace(startText), strings.TrimSpace(endText), allMatching)
		if err != nil {
			abortImageError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"removed": removed})
	})
	admin.POST("/api/images/download", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		if _, present := body["paths"]; !present {
			abortValidation(c, "paths", "Field required")
			return
		}
		paths, okPaths := stringList(body, "paths")
		if !okPaths || body["paths"] == nil {
			abortValidation(c, "paths", "Input should be a valid list")
			return
		}
		payload, err := store.Zip(paths)
		if err != nil {
			abortImageError(c, err)
			return
		}
		c.Header("Content-Disposition", `attachment; filename="images.zip"`)
		c.Data(http.StatusOK, "application/zip", payload)
	})
	admin.GET("/api/images/download/*image_path", func(c *gin.Context) {
		rel := strings.TrimPrefix(c.Param("image_path"), "/")
		local, err := store.HasLocal(rel)
		if err != nil {
			abortImageError(c, err)
			return
		}
		setCORS(c)
		if local {
			full, _ := imagestore.LocalPath(store, rel)
			c.Header("Content-Disposition", `attachment; filename="`+path.Base(full)+`"`)
			c.File(full)
			return
		}
		safe, _ := imagestore.SafeRel(rel)
		data, err := store.GetBytes(safe)
		if err != nil {
			abortImageError(c, err)
			return
		}
		c.Header("Content-Disposition", `attachment; filename="`+path.Base(safe)+`"`)
		c.Data(http.StatusOK, "image/png", data)
	})
	admin.GET("/api/images/tags", func(c *gin.Context) {
		tags, err := store.AllTags()
		if err != nil {
			abortImageError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"tags": tags})
	})
	admin.POST("/api/images/tags", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		for _, field := range []string{"path", "tags"} {
			if _, present := body[field]; !present {
				abortValidation(c, field, "Field required")
				return
			}
		}
		rel, isString := body["path"].(string)
		if !isString {
			abortValidation(c, "path", "Input should be a valid string")
			return
		}
		tags, okTags := stringList(body, "tags")
		if !okTags || body["tags"] == nil {
			abortValidation(c, "tags", "Input should be a valid list")
			return
		}
		rel = strings.TrimLeft(strings.TrimSpace(rel), "/")
		if rel == "" {
			abortDetail(c, http.StatusBadRequest, "path is required")
			return
		}
		saved, err := store.SetTags(rel, tags)
		if err != nil {
			abortImageError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "tags": saved})
	})
	admin.DELETE("/api/images/tags/:tag", func(c *gin.Context) {
		count, err := store.DeleteTag(c.Param("tag"))
		if err != nil {
			abortImageError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "removed_from": count})
	})
	admin.GET("/api/images/storage", func(c *gin.Context) { c.JSON(http.StatusOK, store.StorageStats()) })
	admin.POST("/api/images/storage/cleanup-to-target", func(c *gin.Context) {
		target, ok := queryInt(c, "target_free_mb", 500)
		if !ok {
			return
		}
		dryRun, ok := queryBool(c, "dry_run")
		if !ok {
			return
		}
		result, err := store.DeleteToTarget(int64(target), dryRun)
		if err != nil {
			abortImageError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	admin.POST("/api/image-storage/test", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"result": store.TestWebDAV()}) })
	admin.POST("/api/image-storage/sync", func(c *gin.Context) {
		result, err := store.SyncAll()
		var storageErr *imagestore.StorageError
		if errors.As(err, &storageErr) {
			abortDetail(c, http.StatusBadRequest, storageErr.Message)
			return
		}
		if err != nil {
			abortImageError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"result": result})
	})
}
