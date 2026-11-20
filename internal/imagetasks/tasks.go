// Package imagetasks is services/image_task_service.py: image generations that run in the
// background so the web page can poll them, kept per API key in PostgreSQL (image_tasks.json).
package imagetasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

const (
	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusError   = "error"

	restartError = "服务已重启，未完成的图片任务已中断"
	emptyResult  = "号池中没有可用账号或所有账号均被限流，请检查号池状态（账号额度、是否被封禁、是否到达生图上限）"
)

// row is one task; Body holds the task dict as JSON.
type row struct {
	Key       string `gorm:"type:varchar(512);primaryKey"`
	Owner     string `gorm:"type:varchar(128);index"`
	UpdatedAt string `gorm:"type:varchar(32)"`
	Body      string `gorm:"type:text"`
}

func (row) TableName() string { return "compat_image_tasks" }

// Migrate creates the task table.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&row{}) }

// Task is the stored task dict (keys as in image_tasks.json).
type Task = map[string]any

// Result is what a run returns: the image result dict (data, usage, message, _account_email).
type Result = map[string]any

// RunFunc generates the image; progress reports the pipeline steps.
type RunFunc func(ctx context.Context, progress func(step string)) (Result, error)

// ResumeFunc polls an existing conversation again for extra seconds with the account that started it.
type ResumeFunc func(ctx context.Context, accountEmail, conversationID string, extraSecs float64, size, quality string) ([]map[string]any, error)

// ErrorDetails lets a run error carry the account and conversation (ImageGenerationError attributes).
type ErrorDetails interface {
	error
	Account() (email, conversationID string)
}

// Service keeps the tasks.
type Service struct {
	db       *gorm.DB
	settings *settings.Store
	logs     *logs.Service
	Resume   ResumeFunc
	now      func() time.Time

	mu      sync.Mutex
	tasks   map[string]Task
	running sync.WaitGroup
}

// Wait blocks until every background run has finished (shutdown, tests).
func (s *Service) Wait() { s.running.Wait() }

func nowISO(t time.Time) string { return t.Format("2006-01-02 15:04:05") }

func clean(value any, def string) string {
	text := ""
	switch v := value.(type) {
	case nil:
	case string:
		text = v
	default:
		text = fmt.Sprint(v)
	}
	if text == "" {
		text = def
	}
	return strings.TrimSpace(text)
}

func ownerOf(identity map[string]any) string {
	if owner := clean(identity["id"], ""); owner != "" {
		return owner
	}
	return "anonymous"
}

func taskKey(owner, id string) string { return owner + ":" + id }

func floatOf(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

// New loads the tasks, fails the ones a restart interrupted and drops expired ones.
func New(db *gorm.DB, store *settings.Store, logService *logs.Service) (*Service, error) {
	s := &Service{db: db, settings: store, logs: logService, now: time.Now, tasks: map[string]Task{}}
	var rows []row
	if err := db.Find(&rows).Error; err != nil {
		return nil, err
	}
	changed := []string{}
	for _, r := range rows {
		decoder := json.NewDecoder(strings.NewReader(r.Body))
		decoder.UseNumber()
		var task Task
		if decoder.Decode(&task) != nil || task == nil {
			continue
		}
		if status := clean(task["status"], ""); status == StatusQueued || status == StatusRunning {
			task["status"], task["error"], task["updated_at"] = StatusError, restartError, nowISO(s.now())
			changed = append(changed, r.Key)
		}
		s.tasks[r.Key] = task
	}
	for _, key := range changed {
		s.saveLocked(key)
	}
	s.cleanupLocked()
	return s, nil
}

func (s *Service) saveLocked(key string) {
	task, ok := s.tasks[key]
	if !ok {
		s.db.Delete(&row{}, "key = ?", key)
		return
	}
	raw, _ := json.Marshal(task)
	r := row{Key: key, Owner: clean(task["owner_id"], ""), UpdatedAt: clean(task["updated_at"], ""), Body: string(raw)}
	if err := s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&r).Error; err != nil {
		log.Printf("image task save failed: %v", err)
	}
}

func parseTimestamp(value any) float64 {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return 0
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, text[:min(len(text), 26)], time.Local); err == nil {
			return float64(t.UnixNano()) / 1e9
		}
	}
	if t, err := time.Parse(time.RFC3339Nano, strings.Replace(text, "Z", "+00:00", 1)); err == nil {
		return float64(t.UnixNano()) / 1e9
	}
	return 0
}

// cleanupLocked is _cleanup_locked: finished tasks older than the retention days go.
func (s *Service) cleanupLocked() {
	days := max(1, s.settings.ImageRetentionDays())
	cutoff := float64(s.now().Unix()) - float64(days*86400)
	for key, task := range s.tasks {
		status := clean(task["status"], "")
		if (status == StatusSuccess || status == StatusError) && parseTimestamp(task["updated_at"]) < cutoff {
			delete(s.tasks, key)
			s.saveLocked(key)
		}
	}
}

// Public is _public_task.
func (s *Service) Public(task Task) map[string]any {
	item := map[string]any{"id": task["id"], "status": task["status"], "mode": task["mode"], "model": task["model"],
		"size": task["size"], "quality": task["quality"], "created_at": task["created_at"], "updated_at": task["updated_at"]}
	for _, key := range []string{"conversation_id", "error", "progress"} {
		if clean(task[key], "") != "" {
			item[key] = task[key]
		}
	}
	for _, key := range []string{"data", "usage", "duration_ms"} {
		if value, ok := task[key]; ok && value != nil {
			item[key] = value
		}
	}
	status := clean(task["status"], "")
	var base any
	if status == StatusRunning {
		base = task["started_ts"]
	} else if status == StatusQueued {
		base = task["created_ts"]
		if f, ok := floatOf(base); !ok || f == 0 {
			base = task["updated_ts"]
		}
	}
	if f, ok := floatOf(base); ok && f != 0 {
		elapsed := float64(s.now().UnixNano())/1e9 - f
		item["elapsed_secs"] = math.Round(elapsed*10) / 10
	}
	return item
}

// List is list_tasks: the requested ids (and which are missing), or all of the owner's tasks.
func (s *Service) List(identity map[string]any, ids []string) map[string]any {
	owner := ownerOf(identity)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupLocked()
	items, missing := []any{}, []any{}
	requested := []string{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			requested = append(requested, id)
		}
	}
	for _, id := range requested {
		if task, ok := s.tasks[taskKey(owner, id)]; ok {
			items = append(items, s.Public(task))
		} else {
			missing = append(missing, id)
		}
	}
	if len(requested) == 0 {
		owned := []map[string]any{}
		for _, task := range s.tasks {
			if task["owner_id"] == owner {
				owned = append(owned, s.Public(task))
			}
		}
		sort.SliceStable(owned, func(i, j int) bool { return clean(owned[i]["updated_at"], "") > clean(owned[j]["updated_at"], "") })
		items = []any{}
		for _, item := range owned {
			items = append(items, item)
		}
		missing = []any{}
	}
	return map[string]any{"items": items, "missing_ids": missing}
}

func (s *Service) update(key string, updates map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[key]
	if !ok {
		return
	}
	for k, v := range updates {
		task[k] = v
	}
	now := s.now()
	task["updated_at"], task["updated_ts"] = nowISO(now), float64(now.UnixNano())/1e9
	s.saveLocked(key)
}

// Submit is _submit: an existing id returns that task; a new one starts in the background.
func (s *Service) Submit(identity map[string]any, clientTaskID, mode, model, size, quality, prompt string, run RunFunc) (map[string]any, error) {
	id := strings.TrimSpace(clientTaskID)
	if id == "" {
		return nil, errors.New("client_task_id is required")
	}
	owner := ownerOf(identity)
	key := taskKey(owner, id)
	s.mu.Lock()
	s.cleanupLocked()
	if task, ok := s.tasks[key]; ok {
		s.mu.Unlock()
		return s.Public(task), nil
	}
	now := s.now()
	task := Task{"id": id, "owner_id": owner, "status": StatusQueued, "mode": mode, "model": clean(model, "gpt-image-2"),
		"size": clean(size, ""), "quality": clean(quality, "auto"), "created_at": nowISO(now), "updated_at": nowISO(now),
		"created_ts": float64(now.UnixNano()) / 1e9}
	s.tasks[key] = task
	s.saveLocked(key)
	public := s.Public(task)
	s.mu.Unlock()
	s.running.Add(1)
	go func() {
		defer s.running.Done()
		s.runTask(key, mode, clean(model, "gpt-image-2"), prompt, identity, run)
	}()
	return public, nil
}

func collectURLs(data []map[string]any) []string {
	urls := []string{}
	for _, item := range data {
		if url, ok := item["url"].(string); ok && url != "" {
			urls = append(urls, url)
		}
	}
	return urls
}

func dataList(value any) []map[string]any {
	switch v := value.(type) {
	case []map[string]any:
		return v
	case []any:
		out := []map[string]any{}
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

func (s *Service) runTask(key, mode, model, prompt string, identity map[string]any, run RunFunc) {
	started := s.now()
	s.update(key, map[string]any{"status": StatusRunning, "error": ""})
	progress := func(step string) {
		updates := map[string]any{"progress": step}
		if step == "image_stream_resolve_start" {
			updates["started_ts"] = float64(s.now().UnixNano()) / 1e9
		}
		s.update(key, updates)
	}
	result, err := func() (result Result, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		return run(context.Background(), progress)
	}()
	durationMS := s.now().Sub(started).Milliseconds()
	email := ""
	if err == nil {
		email = clean(result["_account_email"], clean(result["account_email"], ""))
		data := dataList(result["data"])
		if len(data) == 0 {
			message := clean(result["message"], "")
			if message == "" {
				message = emptyResult
			}
			err = errors.New(message)
		} else {
			s.update(key, map[string]any{"status": StatusSuccess, "data": data, "usage": result["usage"], "error": "", "duration_ms": durationMS})
			s.logCall(identity, mode, model, started, "调用完成", prompt, "success", "", collectURLs(data), email)
			return
		}
	}
	message := err.Error()
	if message == "" {
		message = "image task failed"
	}
	conversationID := ""
	var details ErrorDetails
	if errors.As(err, &details) {
		accountEmail, conversation := details.Account()
		if accountEmail != "" {
			email = accountEmail
		}
		conversationID = conversation
	}
	updates := map[string]any{"status": StatusError, "error": message, "data": []any{}, "duration_ms": durationMS}
	if conversationID != "" {
		updates["conversation_id"] = conversationID
	}
	if email != "" {
		updates["account_email"] = email // Go only: lets resume-poll use the same account
	}
	s.update(key, updates)
	s.logCall(identity, mode, model, started, "调用失败", prompt, "failed", message, nil, email)
}

// logCall is _log_call: the task is logged as the matching /v1/images call.
func (s *Service) logCall(identity map[string]any, mode, model string, started time.Time, suffix, prompt, status, errText string, urls []string, email string) {
	if s.logs == nil {
		return
	}
	endpoint, summary := "/v1/images/generations", "文生图"
	if mode == "edit" {
		endpoint, summary = "/v1/images/edits", "图生图"
	}
	detail := map[string]any{"key_id": identity["id"], "key_name": identity["name"], "role": identity["role"], "endpoint": endpoint,
		"model": model, "started_at": nowISO(started), "ended_at": nowISO(s.now()), "duration_ms": s.now().Sub(started).Milliseconds(), "status": status}
	if text := strings.TrimSpace(prompt); text != "" {
		detail["request_text"] = text
	}
	if errText != "" {
		detail["error"] = errText
	}
	if email != "" {
		detail["account_email"] = email
	}
	if len(urls) > 0 {
		unique, seen := []any{}, map[string]bool{}
		for _, url := range urls {
			if !seen[url] {
				seen[url] = true
				unique = append(unique, url)
			}
		}
		detail["urls"] = unique
	}
	s.logs.Add(logs.TypeCall, summary+suffix, detail)
}

// ResumePoll is resume_poll: a task that failed on a timeout and has a conversation id is polled again.
func (s *Service) ResumePoll(identity map[string]any, taskID string, extraSecs float64) (map[string]any, error) {
	key := taskKey(ownerOf(identity), strings.TrimSpace(taskID))
	s.mu.Lock()
	task, ok := s.tasks[key]
	if !ok {
		s.mu.Unlock()
		return nil, errors.New("task not found")
	}
	if clean(task["status"], "") != StatusError {
		s.mu.Unlock()
		return nil, errors.New("task is not in error state")
	}
	if !strings.Contains(clean(task["error"], ""), "超时") {
		s.mu.Unlock()
		return nil, errors.New("task error is not a timeout error")
	}
	conversationID := clean(task["conversation_id"], "")
	if conversationID == "" {
		s.mu.Unlock()
		return nil, errors.New("task has no conversation_id")
	}
	mode, model := clean(task["mode"], "generate"), clean(task["model"], "gpt-image-2")
	email, size, quality := clean(task["account_email"], ""), clean(task["size"], ""), clean(task["quality"], "auto")
	s.mu.Unlock()
	s.update(key, map[string]any{"status": StatusRunning, "error": ""})
	s.mu.Lock()
	public := s.Public(s.tasks[key])
	s.mu.Unlock()
	s.running.Add(1)
	go func() {
		defer s.running.Done()
		started := s.now()
		var data []map[string]any
		var err error
		if s.Resume == nil {
			err = errors.New("resume poll is not available")
		} else {
			data, err = s.Resume(context.Background(), email, conversationID, extraSecs, size, quality)
		}
		durationMS := s.now().Sub(started).Milliseconds()
		if err != nil {
			message := err.Error()
			if message == "" {
				message = "resume poll failed"
			}
			s.update(key, map[string]any{"status": StatusError, "error": message, "data": []any{}, "duration_ms": durationMS})
			s.logCall(identity, mode, model, started, "调用失败（续轮询）", "", "failed", message, nil, "")
			return
		}
		s.update(key, map[string]any{"status": StatusSuccess, "data": data, "error": "", "duration_ms": durationMS})
		s.logCall(identity, mode, model, started, "调用完成（续轮询）", "", "success", "", collectURLs(data), "")
	}()
	return public, nil
}

// Export is image_tasks.json: {"tasks": [...]}, newest update first (backups).
func (s *Service) Export() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := []map[string]any{}
	for _, task := range s.tasks {
		items = append(items, task)
	}
	sort.SliceStable(items, func(i, j int) bool { return clean(items[i]["updated_at"], "") > clean(items[j]["updated_at"], "") })
	return map[string]any{"tasks": items}
}
