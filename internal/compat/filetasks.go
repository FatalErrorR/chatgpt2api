package compat

import (
	"net/http"
	"net/url"
	"path/filepath"

	"github.com/gin-gonic/gin"
)

// The editable file routes of api/ai.py: /v1/ppt/generations, /v1/psd/generations,
// /v1/editable-file-tasks and /files/*.

func registerFileTasks(engine *gin.Engine, d *Deps) {
	tasks := d.FileTasks
	engine.GET("/v1/editable-file-tasks", d.Auth.RequireIdentity, func(c *gin.Context) {
		c.JSON(http.StatusOK, tasks.List(identityMap(identityOf(c)), parseTaskIDs(c.Query("ids"))))
	})
	engine.GET("/files/*file_path", func(c *gin.Context) {
		full, err := tasks.PublicFile(c.Param("file_path"))
		if err != nil {
			abortDetail(c, http.StatusNotFound, "file not found")
			return
		}
		name := filepath.Base(full)
		c.Header("Content-Disposition", "attachment; filename*=utf-8''"+url.PathEscape(name))
		c.File(full)
	})
	submit := func(kind, endpoint, summary string) gin.HandlerFunc {
		return func(c *gin.Context) {
			body, ok := bindJSON(c)
			if !ok || !checkOptional(c, body, []string{"prompt", "client_task_id"}, nil, nil, nil, nil) {
				return
			}
			images, okImages := stringList(body, "base64_images")
			if !okImages {
				abortValidation(c, "base64_images", "Input should be a valid list")
				return
			}
			identity := d.Auth.Identify(c.GetHeader("Authorization"))
			if identity == nil {
				abortDetail(c, 401, "密钥无效或已失效，请重新登录")
				return
			}
			prompt, _ := body["prompt"].(string)
			clientTaskID, _ := body["client_task_id"].(string)
			call := newCallLog(d.Logs, identity, endpoint, "gpt-5-5-thinking", summary, prompt, "")
			if rejected := CheckRequest(d.Settings, prompt); rejected != nil {
				call.Log("调用失败", nil, "failed", detailRepr(rejected.message), nil, "", "")
				abortHTTPError(c, rejected)
				return
			}
			c.JSON(http.StatusOK, tasks.Submit(identityMap(identity), clientTaskID, kind, prompt, images, resolveImageBaseURL(c, d)))
		}
	}
	engine.POST("/v1/ppt/generations", submit("ppt", "/v1/ppt/generations", "PPT生成任务"))
	engine.POST("/v1/psd/generations", submit("psd", "/v1/psd/generations", "PSD生成任务"))
}
