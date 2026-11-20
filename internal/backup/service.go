// Package backup is services/backup_service.py: tar.gz backups (optionally encrypted) uploaded to
// Cloudflare R2, with rotation and a schedule. The archive keeps the Python layout so either
// version can read it; data that lives in PostgreSQL here is exported to the same JSON files.
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/docstore"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

const stateDocument = "backup_state.json"

// Error is BackupError: answered as 400 by the routes.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func clean(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func isoNow(now time.Time) string { return now.UTC().Format("2006-01-02T15:04:05Z") }

// Sources supply the archive's files. Each returns the file content, or nil when there is none.
type Sources struct {
	ConfigFile     string                   // config.json on disk
	ImagesDir      string                   // data/images
	Document       func(name string) []byte // docstore documents (proxies, cpa_config.json, ...)
	Logs           func() ([]byte, error)
	ImageTasks     func() any
	ImageIndex     func() (any, error)
	ImageTags      func() (any, error)
	Accounts       func() (any, error)
	AuthKeys       func() (any, error)
	StorageBackend func() any
	AppVersion     string
}

// Service runs backups.
type Service struct {
	Settings *settings.Store
	Docs     *docstore.Store
	Sources  Sources
	NewR2    func(settings map[string]any) *R2
	Now      func() time.Time

	mu      sync.Mutex
	running bool
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) settings() map[string]any { return s.Settings.Backup() }

func (s *Service) client() *R2 {
	if s.NewR2 != nil {
		return s.NewR2(s.settings())
	}
	return newR2(s.settings())
}

// normalizeState is _normalize_backup_state.
func normalizeState(raw map[string]any) map[string]any {
	nullable := func(key string) any {
		if text := clean(raw[key]); text != "" {
			return text
		}
		return nil
	}
	status := clean(raw["last_status"])
	if status == "" {
		status = "idle"
	}
	return map[string]any{"last_started_at": nullable("last_started_at"), "last_finished_at": nullable("last_finished_at"),
		"last_status": status, "last_error": nullable("last_error"), "last_object_key": nullable("last_object_key")}
}

func (s *Service) loadState() map[string]any {
	raw := map[string]any{}
	s.Docs.Load(stateDocument, &raw)
	return normalizeState(raw)
}

func (s *Service) saveState(state map[string]any) {
	if err := s.Docs.Save(stateDocument, normalizeState(state)); err != nil {
		log.Printf("backup state save failed: %v", err)
	}
}

// Status is get_status.
func (s *Service) Status() map[string]any {
	state := s.loadState()
	s.mu.Lock()
	state["running"] = s.running
	s.mu.Unlock()
	return state
}

// PublicSettings is get_settings: secrets masked.
func (s *Service) PublicSettings() map[string]any {
	out := map[string]any{}
	for k, v := range s.settings() {
		out[k] = v
	}
	for _, key := range []string{"secret_access_key", "passphrase"} {
		if clean(out[key]) != "" {
			out[key] = "********"
		} else {
			out[key] = ""
		}
	}
	return out
}

func (s *Service) configured() bool {
	r := s.client()
	return r.AccountID != "" && r.AccessKeyID != "" && r.SecretAccessKey != "" && r.Bucket != ""
}

// Test is test_connection.
func (s *Service) Test() (map[string]any, error) { return s.client().Test() }

// List is list_backups.
func (s *Service) List() ([]map[string]any, error) {
	if !s.configured() {
		return []map[string]any{}, nil
	}
	items, err := s.client().List()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, item := range items {
		key := clean(item["key"])
		name := key[strings.LastIndex(key, "/")+1:]
		out = append(out, map[string]any{"key": key, "name": name, "size": item["size"], "updated_at": item["updated_at"], "encrypted": strings.HasSuffix(name, ".enc")})
	}
	return out, nil
}

// Delete is delete_backup.
func (s *Service) Delete(key string) error {
	if key = strings.TrimSpace(key); key == "" {
		return &Error{"备份对象 key 不能为空"}
	}
	return s.client().Delete(key)
}

func (s *Service) passphrase() string { return clean(s.settings()["passphrase"]) }

// Download is download_backup: the object, decrypted when it is encrypted.
func (s *Service) Download(key string) (map[string]any, error) {
	if key = strings.TrimSpace(key); key == "" {
		return nil, &Error{"备份对象 key 不能为空"}
	}
	payload, err := s.client().Download(key)
	if err != nil {
		return nil, err
	}
	name := key[strings.LastIndex(key, "/")+1:]
	if name == "" {
		name = "backup.bin"
	}
	if strings.HasSuffix(key, ".enc") {
		passphrase := s.passphrase()
		if passphrase == "" {
			return nil, &Error{"当前未配置加密口令，无法下载并解密已加密备份"}
		}
		if payload, err = Decrypt(payload, passphrase); err != nil {
			return nil, &Error{err.Error()}
		}
		if name = strings.TrimSuffix(name, ".enc"); name == "" {
			name = "backup.tar.gz"
		}
	}
	return map[string]any{"key": key, "name": name, "content_type": contentType(name), "payload": payload, "size": len(payload)}, nil
}

func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	case strings.HasSuffix(name, ".jsonl"):
		return "application/x-ndjson"
	case strings.HasSuffix(name, ".gz"):
		return "application/gzip"
	}
	return "application/octet-stream"
}

// Detail is get_backup_detail: what the archive contains.
func (s *Service) Detail(key string) (map[string]any, error) {
	if key = strings.TrimSpace(key); key == "" {
		return nil, &Error{"备份对象 key 不能为空"}
	}
	payload, err := s.client().Download(key)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(key, ".enc") {
		passphrase := s.passphrase()
		if passphrase == "" {
			return nil, &Error{"当前未配置加密口令，无法查看已加密备份"}
		}
		if payload, err = Decrypt(payload, passphrase); err != nil {
			return nil, &Error{err.Error()}
		}
	}
	detail, err := DescribeArchive(payload)
	if err != nil {
		return nil, err
	}
	detail["key"], detail["name"], detail["encrypted"] = key, key[strings.LastIndex(key, "/")+1:], strings.HasSuffix(key, ".enc")
	return detail, nil
}

func countItems(value any) int {
	switch v := value.(type) {
	case []any:
		return len(v)
	case map[string]any:
		return len(v)
	}
	return 0
}

// DescribeArchive is _decode_archive_detail.
func DescribeArchive(payload []byte) (map[string]any, error) {
	broken := &Error{"解析备份压缩包失败，备份可能已损坏"}
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, broken
	}
	reader := tar.NewReader(gz)
	files, snapshots := []map[string]any{}, []map[string]any{}
	metadata := map[string]any{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, broken
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		raw, err := io.ReadAll(reader)
		if err != nil {
			return nil, broken
		}
		name := header.Name
		switch {
		case name == "backup-metadata.json":
			metadata = map[string]any{}
			json.Unmarshal(raw, &metadata)
		case strings.HasPrefix(name, "snapshots/") && strings.HasSuffix(name, ".json"):
			var parsed any
			count := 0
			if json.Unmarshal(raw, &parsed) == nil {
				count = countItems(parsed)
			}
			snapshots = append(snapshots, map[string]any{"name": strings.TrimSuffix(strings.TrimPrefix(name, "snapshots/"), ".json"), "count": count})
		default:
			files = append(files, map[string]any{"name": name, "exists": true, "content_type": contentType(name), "size": len(raw), "sha256": sha256Hex(raw)})
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i]["name"].(string) < files[j]["name"].(string) })
	sort.SliceStable(snapshots, func(i, j int) bool { return snapshots[i]["name"].(string) < snapshots[j]["name"].(string) })
	return map[string]any{"created_at": metadata["created_at"], "trigger": metadata["trigger"], "app_version": metadata["app_version"],
		"storage_backend": metadata["storage_backend"], "files": files, "snapshots": snapshots}, nil
}

// Run is run_backup.
func (s *Service) Run(trigger string) (map[string]any, error) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil, &Error{"当前已有备份任务正在执行"}
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()
	current := s.loadState()
	started := isoNow(s.now())
	s.saveState(map[string]any{"last_started_at": started, "last_finished_at": current["last_finished_at"], "last_status": "idle",
		"last_object_key": current["last_object_key"]})
	result, err := s.runOnce(trigger)
	if err != nil {
		s.saveState(map[string]any{"last_started_at": started, "last_finished_at": isoNow(s.now()), "last_status": "error",
			"last_error": err.Error(), "last_object_key": current["last_object_key"]})
		return nil, err
	}
	s.saveState(map[string]any{"last_started_at": started, "last_finished_at": isoNow(s.now()), "last_status": "success", "last_object_key": result["key"]})
	return result, nil
}

func (s *Service) runOnce(trigger string) (map[string]any, error) {
	settings := s.settings()
	client := s.client()
	if err := client.Validate(); err != nil {
		return nil, err
	}
	payload, err := s.BuildArchive(settings, trigger)
	if err != nil {
		return nil, err
	}
	encrypted := settings["encrypt"] == true
	suffix := ".tar.gz"
	if encrypted {
		passphrase := clean(settings["passphrase"])
		if passphrase == "" {
			return nil, &Error{"已启用备份加密，但未设置加密口令"}
		}
		if payload, err = Encrypt(payload, passphrase); err != nil {
			return nil, &Error{"加密备份失败：" + err.Error()}
		}
		suffix = ".tar.gz.enc"
	}
	key := fmt.Sprintf("%s/backup-%s-%04x%s", strings.TrimRight(client.Prefix, "/"), s.now().UTC().Format("20060102T150405Z"), rand.Intn(0x10000), suffix)
	encryptedText := "false"
	if encrypted {
		encryptedText = "true"
	}
	if _, err := client.Upload(key, payload, "application/octet-stream", map[string]string{"created-at": isoNow(s.now()), "encrypted": encryptedText, "trigger": trigger}); err != nil {
		return nil, err
	}
	if err := s.rotate(client, intOf(settings["rotation_keep"])); err != nil {
		return nil, err
	}
	return map[string]any{"key": key, "size": len(payload), "encrypted": encrypted}, nil
}

func intOf(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

func isBackupObject(key string) bool {
	name := key[strings.LastIndex(key, "/")+1:]
	return strings.HasPrefix(name, "backup-") && (strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tar.gz.enc"))
}

// rotate is _apply_rotation: keep the newest `keep` backups.
func (s *Service) rotate(client *R2, keep int) error {
	if keep <= 0 {
		return nil
	}
	items, err := client.List()
	if err != nil {
		return err
	}
	backups := []map[string]any{}
	for _, item := range items {
		if isBackupObject(clean(item["key"])) {
			backups = append(backups, item)
		}
	}
	for _, item := range backups[min(keep, len(backups)):] {
		if err := client.Delete(clean(item["key"])); err != nil {
			return err
		}
	}
	return nil
}

func jsonBytes(value any) []byte {
	raw, _ := json.MarshalIndent(value, "", "  ")
	return raw
}

// BuildArchive is _build_backup_archive.
func (s *Service) BuildArchive(settings map[string]any, trigger string) ([]byte, error) {
	include, _ := settings["include"].(map[string]any)
	src := s.Sources
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	archive := tar.NewWriter(gz)
	now := s.now()
	add := func(name string, payload []byte) error {
		if payload == nil {
			return nil
		}
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(payload)), ModTime: now, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := archive.Write(payload)
		return err
	}
	addJSON := func(name string, load func() (any, error)) error {
		if load == nil {
			return nil
		}
		value, err := load()
		if err != nil {
			return err
		}
		return add(name, jsonBytes(value))
	}
	document := func(name string) []byte {
		if src.Document == nil {
			return nil
		}
		return src.Document(name)
	}
	var backend any
	if src.StorageBackend != nil {
		backend = src.StorageBackend()
	}
	steps := []func() error{
		func() error {
			return add("backup-metadata.json", jsonBytes(map[string]any{"version": 2, "created_at": isoNow(now), "trigger": trigger,
				"app_version": src.AppVersion, "storage_backend": backend}))
		},
	}
	if include["config"] == true {
		steps = append(steps, func() error {
			if raw, err := os.ReadFile(src.ConfigFile); err == nil {
				if err := add("config.json", raw); err != nil {
					return err
				}
			}
			return add("data/proxies.json", document("proxies"))
		})
	}
	if include["cpa"] == true {
		steps = append(steps, func() error { return add("data/cpa_config.json", document("cpa_config.json")) })
	}
	if include["sub2api"] == true {
		steps = append(steps, func() error { return add("data/sub2api_config.json", document("sub2api_config.json")) })
	}
	if include["logs"] == true && src.Logs != nil {
		steps = append(steps, func() error {
			raw, err := src.Logs()
			if err != nil {
				return err
			}
			return add("data/logs.jsonl", raw)
		})
	}
	if include["image_tasks"] == true {
		steps = append(steps, func() error {
			if src.ImageTasks != nil {
				if err := add("data/image_tasks.json", jsonBytes(src.ImageTasks())); err != nil {
					return err
				}
			}
			return addJSON("data/image_index.json", src.ImageIndex)
		})
	}
	if include["accounts_snapshot"] == true {
		steps = append(steps, func() error { return addJSON("snapshots/accounts.json", src.Accounts) })
	}
	if include["auth_keys_snapshot"] == true {
		steps = append(steps, func() error { return addJSON("snapshots/auth_keys.json", src.AuthKeys) })
	}
	if include["images"] == true {
		steps = append(steps, func() error {
			if err := addJSON("data/image_tags.json", src.ImageTags); err != nil {
				return err
			}
			return addDirectory(archive, src.ImagesDir, "data/images")
		})
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func addDirectory(archive *tar.Writer, dir, root string) error {
	if dir == "" {
		return nil
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil
	}
	paths := []string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			paths = append(paths, p)
		}
		return nil
	})
	sort.Strings(paths)
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(dir, p)
		file, err := os.Open(p)
		if err != nil {
			continue
		}
		err = archive.WriteHeader(&tar.Header{Name: root + "/" + filepath.ToSlash(rel), Mode: 0o644, Size: info.Size(), ModTime: info.ModTime(), Typeflag: tar.TypeReg})
		if err == nil {
			_, err = io.Copy(archive, file)
		}
		file.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// RunScheduled is run_scheduled_backup_if_needed.
func (s *Service) RunScheduled() {
	settings := s.settings()
	if settings["enabled"] != true {
		return
	}
	state := s.Status()
	if state["running"] == true {
		return
	}
	interval := intOf(settings["interval_minutes"])
	if interval == 0 {
		interval = 360
	}
	if last := clean(state["last_finished_at"]); last != "" {
		if finished, err := time.Parse(time.RFC3339, strings.Replace(last, "Z", "+00:00", 1)); err == nil && s.now().Sub(finished) < time.Duration(interval)*time.Minute {
			return
		} else if err != nil {
			if finished, err := time.Parse(time.RFC3339, last); err == nil && s.now().Sub(finished) < time.Duration(interval)*time.Minute {
				return
			}
		}
	}
	if _, err := s.Run("schedule"); err != nil {
		log.Printf("scheduled backup failed: %v", err)
	}
}

// Start checks the schedule every 30 seconds until ctx ends.
func (s *Service) Start(ctx context.Context) {
	for {
		func() {
			defer func() { recover() }()
			s.RunScheduled()
		}()
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}
