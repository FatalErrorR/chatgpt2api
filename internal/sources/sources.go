// Package sources is services/cpa_service.py and services/sub2api_service.py: remote account
// sources (CLIProxyAPI pools and sub2api servers) whose tokens can be imported into the pool.
// Their settings stay JSON documents (cpa_config.json, sub2api_config.json).
package sources

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/basketikun/chatgpt2api/internal/docstore"
	"github.com/basketikun/chatgpt2api/internal/pool"
)

const (
	cpaDocument     = "cpa_config.json"
	sub2apiDocument = "sub2api_config.json"
	tokenSkew       = 5 * time.Minute
)

func newID() string {
	buf := make([]byte, 6)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

func nowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00") }

func clean(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func intOf(value any) int {
	switch v := value.(type) {
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	}
	return 0
}

// normalizeJob is _normalize_import_job.
func normalizeJob(raw any, failUnfinished bool) map[string]any {
	job, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	status := clean(job["status"])
	if status == "" {
		status = "failed"
	}
	if failUnfinished && (status == "pending" || status == "running") {
		status = "failed"
	}
	jobID := clean(job["job_id"])
	if jobID == "" {
		jobID = strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	created := clean(job["created_at"])
	if created == "" {
		created = nowISO()
	}
	updated := clean(job["updated_at"])
	if updated == "" {
		updated = created
	}
	errs, ok := job["errors"].([]any)
	if !ok {
		errs = []any{}
	}
	return map[string]any{"job_id": jobID, "status": status, "created_at": created, "updated_at": updated,
		"total": intOf(job["total"]), "completed": intOf(job["completed"]), "added": intOf(job["added"]), "skipped": intOf(job["skipped"]),
		"refreshed": intOf(job["refreshed"]), "failed": intOf(job["failed"]), "errors": errs}
}

// store is the shared list-of-dicts document with an import job per entry.
type store struct {
	docs      *docstore.Store
	name      string
	normalize func(map[string]any) map[string]any
	mu        sync.Mutex
	items     []map[string]any
}

func newStore(docs *docstore.Store, name string, normalize func(map[string]any) map[string]any) (*store, error) {
	s := &store{docs: docs, name: name, normalize: normalize}
	var raw any
	if _, err := docs.Load(name, &raw); err != nil {
		return nil, err
	}
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				s.items = append(s.items, normalize(m))
			}
		}
	case map[string]any: // the old single-pool cpa_config.json
		if _, ok := v["base_url"]; ok && name == cpaDocument {
			if item := normalize(v); clean(item["base_url"]) != "" {
				s.items = append(s.items, item)
			}
		}
	}
	return s, nil
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (s *store) saveLocked() error {
	if s.items == nil {
		return s.docs.Save(s.name, []any{})
	}
	return s.docs.Save(s.name, s.items)
}

func (s *store) list() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	for _, item := range s.items {
		out = append(out, copyMap(item))
	}
	return out
}

func (s *store) get(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.items {
		if item["id"] == id {
			return copyMap(item)
		}
	}
	return nil
}

func (s *store) add(fields map[string]any) (map[string]any, error) {
	fields["id"] = newID()
	item := s.normalize(fields)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, item)
	return copyMap(item), s.saveLocked()
}

func (s *store) update(id string, updates map[string]any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, item := range s.items {
		if item["id"] != id {
			continue
		}
		merged := copyMap(item)
		for k, v := range updates {
			if v != nil {
				merged[k] = v
			}
		}
		merged["id"] = id
		s.items[i] = s.normalize(merged)
		return copyMap(s.items[i]), s.saveLocked()
	}
	return nil, nil
}

func (s *store) remove(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := []map[string]any{}
	for _, item := range s.items {
		if item["id"] != id {
			kept = append(kept, item)
		}
	}
	if len(kept) == len(s.items) {
		return false, nil
	}
	s.items = kept
	return true, s.saveLocked()
}

func (s *store) job(id string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.items {
		if item["id"] == id {
			if job, ok := item["import_job"].(map[string]any); ok {
				return copyMap(job)
			}
		}
	}
	return nil
}

func (s *store) setJob(id string, job map[string]any) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, item := range s.items {
		if item["id"] != id {
			continue
		}
		next := copyMap(item)
		next["import_job"] = normalizeJob(job, false)
		s.items[i] = next
		s.saveLocked()
		return copyMap(next)
	}
	return nil
}

func (s *store) updateJob(id string, updates map[string]any) {
	current := s.job(id)
	if current == nil {
		return
	}
	for k, v := range updates {
		current[k] = v
	}
	current["updated_at"] = nowISO()
	s.setJob(id, current)
}

func (s *store) appendError(id, name, message string) {
	current := s.job(id)
	if current == nil {
		return
	}
	errs, _ := current["errors"].([]any)
	errs = append(append([]any{}, errs...), map[string]any{"name": name, "error": message})
	s.updateJob(id, map[string]any{"errors": errs, "failed": len(errs)})
}

func newJob(total int) map[string]any {
	now := nowISO()
	return map[string]any{"job_id": strings.ReplaceAll(uuid.NewString(), "-", ""), "status": "pending", "created_at": now, "updated_at": now,
		"total": total, "completed": 0, "added": 0, "skipped": 0, "refreshed": 0, "failed": 0, "errors": []any{}}
}

func errorCount(job map[string]any) int {
	errs, _ := job["errors"].([]any)
	return len(errs)
}

// finishImport is the end of _run_import: add the tokens as codex accounts and refresh them.
func finishImport(s *store, id string, accounts *pool.Service, tokens []string, total int) {
	if len(tokens) == 0 {
		current := s.job(id)
		s.updateJob(id, map[string]any{"status": "failed", "completed": intOf(current["total"]), "failed": errorCount(current)})
		return
	}
	added := accounts.AddAccounts(tokens, "codex")
	refreshed := accounts.RefreshAccounts(context.Background(), tokens, "", false)
	s.updateJob(id, map[string]any{"status": "completed", "completed": total, "added": added.Added, "skipped": added.Skipped,
		"refreshed": refreshed.Refreshed, "failed": errorCount(s.job(id))})
}

// ---- HTTP ----

type httpResult struct {
	status int
	body   []byte
}

func (r httpResult) ok() bool { return r.status >= 200 && r.status < 400 }

func (r httpResult) text(n int) string {
	text := string(r.body)
	runes := []rune(text)
	if len(runes) > n {
		return string(runes[:n])
	}
	return text
}

func (r httpResult) json() (any, error) {
	var out any
	decoder := json.NewDecoder(strings.NewReader(string(r.body)))
	decoder.UseNumber()
	return out, decoder.Decode(&out)
}

func doRequest(ctx context.Context, client *http.Client, method, target string, headers map[string]string, query url.Values, body any) (httpResult, error) {
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = strings.NewReader(string(raw))
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return httpResult{}, err
	}
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return httpResult{}, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return httpResult{response.StatusCode, raw}, nil
}

var errNotFound = errors.New("not found")
