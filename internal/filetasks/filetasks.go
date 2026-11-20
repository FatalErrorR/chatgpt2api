// Package filetasks is services/editable_file_task_service.py: PPT / PSD exports that run in
// the background; files land in data/files/<kind>/<task id>/ and are served under /files/.
package filetasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/pool"
)

const (
	Model        = "gpt-5-5-thinking"
	restartError = "服务已重启，未完成的任务已中断"
)

var planTypes = map[string]bool{"Plus": true, "Team": true, "Pro": true, "Enterprise": true}

type row struct {
	Key  string `gorm:"type:varchar(512);primaryKey"`
	Body string `gorm:"type:text"`
}

func (row) TableName() string { return "compat_editable_file_tasks" }

// Migrate creates the table.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&row{}) }

// Export runs one export with the given account token into dir.
type Export func(ctx context.Context, token, kind string, images []string, prompt, dir string) (conversationID, primary, zip string, err error)

// Service keeps the tasks.
type Service struct {
	db      *gorm.DB
	pool    *pool.Service
	logs    *logs.Service
	Root    string // data/files
	Export  Export
	mu      sync.Mutex
	tasks   map[string]map[string]any
	running sync.WaitGroup
}

func nowISO() string { return time.Now().Format("2006-01-02 15:04:05") }
func nowTS() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func str(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func num(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

// New loads the tasks and fails the ones a restart interrupted.
func New(db *gorm.DB, accounts *pool.Service, logService *logs.Service, root string) (*Service, error) {
	s := &Service{db: db, pool: accounts, logs: logService, Root: root, tasks: map[string]map[string]any{}}
	var rows []row
	if err := db.Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		decoder := json.NewDecoder(strings.NewReader(r.Body))
		decoder.UseNumber()
		var task map[string]any
		if decoder.Decode(&task) != nil || task == nil {
			continue
		}
		s.tasks[r.Key] = task
		if status := str(task["status"]); status == "queued" || status == "running" {
			now := nowTS()
			task["status"], task["error"], task["ended_ts"], task["updated_at"], task["updated_ts"] = "error", restartError, now, nowISO(), now
			s.save(r.Key)
		}
	}
	return s, nil
}

// Wait blocks until background runs finish.
func (s *Service) Wait() { s.running.Wait() }

func (s *Service) save(key string) {
	raw, _ := json.Marshal(s.tasks[key])
	if err := s.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row{Key: key, Body: string(raw)}).Error; err != nil {
		log.Printf("editable task save failed: %v", err)
	}
}

func owner(identity map[string]any) string {
	if id := str(identity["id"]); id != "" {
		return id
	}
	return "anonymous"
}

func elapsed(task map[string]any) int {
	start := num(task["started_ts"])
	if start == 0 {
		start = num(task["created_ts"])
	}
	if start == 0 {
		return 0
	}
	end := num(task["ended_ts"])
	if end == 0 {
		end = nowTS()
	}
	return max(0, int(end-start))
}

func public(task map[string]any) map[string]any {
	item := map[string]any{"id": task["id"], "taskId": task["id"], "status": task["status"], "kind": task["kind"],
		"created_at": task["created_at"], "updated_at": task["updated_at"], "elapsed_seconds": elapsed(task)}
	for _, key := range []string{"result", "error"} {
		if value := task[key]; value != nil && value != "" {
			if m, ok := value.(map[string]any); !ok || len(m) > 0 {
				item[key] = value
			}
		}
	}
	return item
}

// List is list_tasks.
func (s *Service) List(identity map[string]any, ids []string) map[string]any {
	who := owner(identity)
	s.mu.Lock()
	defer s.mu.Unlock()
	requested := []string{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			requested = append(requested, id)
		}
	}
	items, missing := []any{}, []any{}
	if len(requested) > 0 {
		for _, id := range requested {
			if task, ok := s.tasks[who+":"+id]; ok {
				items = append(items, public(task))
			} else {
				missing = append(missing, id)
			}
		}
		return map[string]any{"items": items, "missing_ids": missing}
	}
	owned := []map[string]any{}
	for _, task := range s.tasks {
		if task["owner_id"] == who {
			owned = append(owned, task)
		}
	}
	sort.SliceStable(owned, func(i, j int) bool { return str(owned[i]["updated_at"]) > str(owned[j]["updated_at"]) })
	for _, task := range owned {
		items = append(items, public(task))
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
	task["updated_at"], task["updated_ts"] = nowISO(), nowTS()
	s.save(key)
}

// Submit is submit_ppt / submit_psd.
func (s *Service) Submit(identity map[string]any, clientTaskID, kind, prompt string, images []string, baseURL string) map[string]any {
	id := strings.TrimSpace(clientTaskID)
	if id == "" {
		id = uuid.NewString()
	}
	key := owner(identity) + ":" + id
	s.mu.Lock()
	if task, ok := s.tasks[key]; ok {
		s.mu.Unlock()
		return public(task)
	}
	now, ts := nowISO(), nowTS()
	task := map[string]any{"id": id, "owner_id": owner(identity), "status": "queued", "kind": kind, "model": Model,
		"created_at": now, "updated_at": now, "created_ts": ts, "updated_ts": ts}
	s.tasks[key] = task
	s.save(key)
	result := public(task)
	s.mu.Unlock()
	s.running.Add(1)
	go func() {
		defer s.running.Done()
		s.run(key, id, kind, prompt, images, identity, baseURL)
	}()
	return result
}

// pickToken is _editable_access_token: the least recently used Plus/Team/Pro/Enterprise account.
func (s *Service) pickToken(ctx context.Context) (string, error) {
	candidates := []pool.Account{}
	for _, account := range s.pool.ListAccounts() {
		status := account["status"]
		if str(account["access_token"]) != "" && status != pool.StatusDisabled && status != pool.StatusAbnormal && planTypes[pool.NormalizeAccountType(account["type"])] {
			candidates = append(candidates, account)
		}
	}
	if len(candidates) == 0 {
		return "", errors.New("no available plus/team/pro account")
	}
	sort.SliceStable(candidates, func(i, j int) bool { return str(candidates[i]["last_used_at"]) < str(candidates[j]["last_used_at"]) })
	token := str(candidates[0]["access_token"])
	if refreshed := s.pool.RefreshAccessToken(ctx, token, false, "editable_file_task"); refreshed != "" {
		return refreshed, nil
	}
	return token, nil
}

// fileURL is _file_url.
func (s *Service) fileURL(p, baseURL string) string {
	root, _ := filepath.Abs(s.Root)
	abs, _ := filepath.Abs(p)
	rel, _ := filepath.Rel(root, abs)
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/files/" + strings.Join(parts, "/")
}

func (s *Service) run(key, id, kind, prompt string, images []string, identity map[string]any, baseURL string) {
	started := time.Now()
	s.update(key, map[string]any{"status": "running", "error": "", "started_ts": nowTS()})
	email := ""
	err := func() error {
		if kind == "psd" && len(images) == 0 {
			return errors.New("base64_images is empty")
		}
		ctx := context.Background()
		token, err := s.pickToken(ctx)
		if err != nil {
			return err
		}
		email = str(s.pool.GetAccount(token)["email"])
		conversationID, primary, zip, err := s.Export(ctx, token, kind, images, prompt, filepath.Join(s.Root, kind, id))
		if err != nil {
			return err
		}
		s.pool.MarkTextUsed(token)
		result := map[string]any{"conversation_id": conversationID, "primary_url": s.fileURL(primary, baseURL), "zip_url": s.fileURL(zip, baseURL)}
		s.update(key, map[string]any{"status": "success", "result": result, "account_email": email, "error": "", "ended_ts": nowTS()})
		s.logCall(identity, kind, started, prompt, "success", "", email, result)
		return nil
	}()
	if err != nil {
		message := err.Error()
		if message == "" {
			message = "editable file task failed"
		}
		s.update(key, map[string]any{"status": "error", "error": message, "account_email": email, "ended_ts": nowTS()})
		s.logCall(identity, kind, started, prompt, "failed", message, email, nil)
	}
}

func (s *Service) logCall(identity map[string]any, kind string, started time.Time, prompt, status, errText, email string, result map[string]any) {
	if s.logs == nil {
		return
	}
	detail := map[string]any{"key_id": identity["id"], "key_name": identity["name"], "role": identity["role"], "endpoint": "/v1/" + kind + "/generations",
		"model": Model, "started_at": started.Format("2006-01-02 15:04:05"), "ended_at": nowISO(), "duration_ms": time.Since(started).Milliseconds(), "status": status}
	if text := strings.TrimSpace(prompt); text != "" {
		detail["request_text"] = text
	}
	if email != "" {
		detail["account_email"] = email
	}
	if errText != "" {
		detail["error"] = errText
	}
	if result != nil {
		detail["result"] = result
	}
	suffix := "完成"
	if status == "failed" {
		suffix = "失败"
	}
	s.logs.Add(logs.TypeCall, strings.ToUpper(kind)+"生成任务"+suffix, detail)
}

// PublicFile is public_file_path: a file under data/files, or an error.
func (s *Service) PublicFile(rel string) (string, error) {
	root, _ := filepath.Abs(s.Root)
	raw := strings.TrimLeft(strings.ReplaceAll(rel, "\\", "/"), "/")
	full := filepath.Clean(filepath.Join(root, filepath.FromSlash(raw)))
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", errors.New("outside files")
	}
	info, err := os.Stat(full)
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("not found")
	}
	return full, nil
}
