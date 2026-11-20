package compat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/basketikun/chatgpt2api/internal/imagetasks"
	"github.com/gin-gonic/gin"
)

// api/image_tasks.py.

// taskError gives the task service the account and conversation of an image error.
type taskError struct{ error }

func (e taskError) Unwrap() error { return e.error }
func (e taskError) Account() (string, string) {
	return errorAccount(e.error)
}

// imageResult is openai_v1_image_generations / openai_v1_image_edit .handle without streaming.
func imageResult(ctx context.Context, d *Deps, request imagegen.Request, inputs [][]byte) (map[string]any, error) {
	outputs, err := d.Images.StreamWithPool(ctx, request)
	if err != nil {
		return nil, taskError{err}
	}
	result := imagegen.CollectImageOutputs(outputs)
	data, _ := result["data"].([]map[string]any)
	result["usage"] = imagegen.ImageUsage(imagegen.CountTextTokens(request.Prompt, request.Model), imagegen.CountImageInputsTokens(inputs),
		imagegen.CountImageOutputItemsTokens(data, request.Size, request.Quality))
	return result, nil
}

func parseTaskIDs(value string) []string {
	ids := []string{}
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			ids = append(ids, item)
		}
	}
	return ids
}

func identityMap(identity Identity) map[string]any {
	out := map[string]any{}
	for key, value := range identity {
		out[key] = value
	}
	return out
}

func abortTaskValueError(c *gin.Context, err error) {
	abortDetail(c, http.StatusBadRequest, err.Error())
}

func registerImageTasks(engine *gin.Engine, d *Deps) {
	tasks := d.ImageTasks
	engine.GET("/api/image-tasks", d.Auth.RequireIdentity, func(c *gin.Context) {
		c.JSON(http.StatusOK, tasks.List(identityMap(identityOf(c)), parseTaskIDs(c.Query("ids"))))
	})

	createEdit := func(c *gin.Context, raw []byte) {
		identity := d.Auth.Identify(c.GetHeader("Authorization"))
		if identity == nil {
			abortDetail(c, 401, "密钥无效或已失效，请重新登录")
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		payload, imageSources, maskSources, err := parseImageEditRequest(c, raw)
		if err != nil {
			abortHTTPError(c, err)
			return
		}
		clientTaskID := strings.TrimSpace(cleanString(payload["client_task_id"], ""))
		if clientTaskID == "" {
			abortDetail(c, http.StatusBadRequest, "client_task_id is required")
			return
		}
		prompt, model := payload["prompt"].(string), payload["model"].(string)
		call := newCallLog(d.Logs, identity, "/api/image-tasks/edits", model, "图生图任务", prompt, "")
		if rejected := CheckRequest(d.Settings, prompt); rejected != nil {
			call.Log("调用失败", nil, "failed", detailRepr(rejected.message), nil, "", "")
			abortHTTPError(c, rejected)
			return
		}
		proxy := d.Settings.Proxy()
		images, err := readImageSources(imageSources, proxy)
		if err != nil {
			abortHTTPError(c, err)
			return
		}
		var masks []ImageInput
		if len(maskSources) > 0 {
			if masks, err = readImageSources(maskSources, proxy); err != nil {
				abortHTTPError(c, err)
				return
			}
		}
		size, _ := payload["size"].(string)
		quality := cleanString(payload["quality"], "auto")
		baseURL := resolveImageBaseURL(c, d)
		composited := compositeMasks(images, masks)
		encoded, inputs := []string{}, [][]byte{}
		for _, image := range composited {
			if len(image.Data) > 0 {
				encoded = append(encoded, base64.StdEncoding.EncodeToString(image.Data))
				inputs = append(inputs, image.Data)
			}
		}
		task, err := tasks.Submit(identityMap(identity), clientTaskID, "edit", model, size, quality, prompt,
			func(ctx context.Context, progress func(string)) (imagetasks.Result, error) {
				if len(encoded) == 0 {
					return nil, imagegen.NewGenerationError("image is required")
				}
				return imageResult(ctx, d, imagegen.Request{Prompt: prompt, Model: model, N: 1, Size: size, Quality: quality, OutputFormat: "png",
					ResponseFormat: "url", BaseURL: baseURL, MessageAsError: true, Images: encoded, Progress: progress}, inputs)
			})
		if err != nil {
			abortTaskValueError(c, err)
			return
		}
		c.JSON(http.StatusOK, task)
	}

	engine.POST("/api/image-tasks/generations", func(c *gin.Context) {
		raw, _ := io.ReadAll(c.Request.Body)
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var body map[string]any
		if err := decoder.Decode(&body); err != nil || body == nil {
			abortValidation(c, "", "Input should be a valid dictionary or object to extract fields from")
			return
		}
		for _, field := range []string{"client_task_id", "prompt"} {
			value, present := body[field]
			if !present {
				abortValidation(c, field, "Field required")
				return
			}
			text, isString := value.(string)
			if !isString {
				abortValidation(c, field, "Input should be a valid string")
				return
			}
			if text == "" {
				abortValidation(c, field, "String should have at least 1 character")
				return
			}
		}
		if !checkOptional(c, body, []string{"model", "size", "quality", "output_format"}, nil, nil, nil, []string{"frame_count", "frame_duration_ms"}) {
			return
		}
		if hasJSONImageReference(c.GetHeader("Content-Type"), body) {
			createEdit(c, raw)
			return
		}
		identity := d.Auth.Identify(c.GetHeader("Authorization"))
		if identity == nil {
			abortDetail(c, 401, "密钥无效或已失效，请重新登录")
			return
		}
		text := func(key, def string) string {
			if value, ok := body[key].(string); ok {
				return value
			}
			return def
		}
		prompt, model := body["prompt"].(string), text("model", "gpt-image-2")
		call := newCallLog(d.Logs, identity, "/api/image-tasks/generations", model, "文生图任务", prompt, "")
		if rejected := CheckRequest(d.Settings, prompt); rejected != nil {
			call.Log("调用失败", nil, "failed", detailRepr(rejected.message), nil, "", "")
			abortHTTPError(c, rejected)
			return
		}
		size, quality := text("size", ""), text("quality", "auto")
		request := imagegen.Request{Prompt: prompt, Model: model, N: 1, Size: size, Quality: quality, ResponseFormat: "url",
			BaseURL: resolveImageBaseURL(c, d), MessageAsError: true}
		if strings.ToLower(text("output_format", "png")) == "gif" {
			request.OutputFormat = "gif"
			if n, ok := body["frame_count"].(int); ok {
				request.FrameCount = &n
			}
			if n, ok := body["frame_duration_ms"].(int); ok {
				request.FrameDurationMS = &n
			}
		}
		task, err := tasks.Submit(identityMap(identity), body["client_task_id"].(string), "generate", model, size, quality, prompt,
			func(ctx context.Context, progress func(string)) (imagetasks.Result, error) {
				request.Progress = progress
				return imageResult(ctx, d, request, nil)
			})
		if err != nil {
			abortTaskValueError(c, err)
			return
		}
		c.JSON(http.StatusOK, task)
	})
	engine.POST("/api/image-tasks/edits", func(c *gin.Context) {
		raw, _ := io.ReadAll(c.Request.Body)
		createEdit(c, raw)
	})
	engine.POST("/api/image-tasks/:task_id/resume-poll", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		extra := 30.0
		if value, present := body["extra_timeout_secs"]; present {
			number, isNumber := value.(json.Number)
			parsed, err := number.Float64()
			if !isNumber || err != nil {
				abortValidation(c, "extra_timeout_secs", "Input should be a valid number")
				return
			}
			if parsed < 5 {
				abortValidation(c, "extra_timeout_secs", "Input should be greater than or equal to 5")
				return
			}
			if parsed > 120 {
				abortValidation(c, "extra_timeout_secs", "Input should be less than or equal to 120")
				return
			}
			extra = parsed
		}
		identity := d.Auth.Identify(c.GetHeader("Authorization"))
		if identity == nil {
			abortDetail(c, 401, "密钥无效或已失效，请重新登录")
			return
		}
		task, err := tasks.ResumePoll(identityMap(identity), c.Param("task_id"), extra)
		if err != nil {
			abortTaskValueError(c, err)
			return
		}
		c.JSON(http.StatusOK, task)
	})
}
