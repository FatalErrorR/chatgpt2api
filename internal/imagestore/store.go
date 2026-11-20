// Package imagestore is services/image_storage_service.py, image_service.py and
// image_tags_service.py: generated images stay as files under data/images (and optionally on
// WebDAV); the index (image_index.json) and the tags (image_tags.json) live in PostgreSQL.
package imagestore

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "golang.org/x/image/webp"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/basketikun/chatgpt2api/internal/settings"
)

const reconcileInterval = 60 * time.Second

var imageExtensions = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true}

// HTTPError is a FastAPI HTTPException with a plain-string detail.
type HTTPError struct {
	Status int
	Detail string
}

func (e *HTTPError) Error() string { return e.Detail }

var errNotFound = &HTTPError{Status: 404, Detail: "image not found"}

// StorageError is ImageStorageError (WebDAV failures, sync with WebDAV off).
type StorageError struct{ Message string }

func (e *StorageError) Error() string { return e.Message }

// indexRow is one image_index.json entry. Item holds the whole entry as JSON; the other columns
// are copies for filtering and ordering. Seq keeps the dict order Python sorts ties by.
type indexRow struct {
	Rel       string `gorm:"type:varchar(512);primaryKey"`
	Seq       int64  `gorm:"not null;default:0;index"`
	Date      string `gorm:"type:varchar(16);index"`
	CreatedAt string `gorm:"type:varchar(32);index"`
	Webdav    bool   `gorm:"not null;default:false"`
	Item      string `gorm:"type:text"`
}

func (indexRow) TableName() string { return "compat_images" }

// tagRow is one image_tags.json entry.
type tagRow struct {
	Rel  string `gorm:"type:varchar(512);primaryKey"`
	Seq  int64  `gorm:"not null;default:0;index"`
	Tags string `gorm:"type:text"`
}

func (tagRow) TableName() string { return "compat_image_tags" }

// Migrate creates the image tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&indexRow{}, &tagRow{}) }

// Service owns the image files, the index and the tags.
type Service struct {
	db       *gorm.DB
	settings *settings.Store
	dataDir  string
	now      func() time.Time

	mu            sync.RWMutex // the index (IMAGE_INDEX_LOCK): saves of different images share it, whole-index work (reconcile, delete, sync) holds it alone
	lastReconcile time.Time
	lastCleanup   time.Time
	seqMu         sync.Mutex
	lastSeq       int64
}

// New returns the service for dataDir (data/ by default).
func New(db *gorm.DB, store *settings.Store, dataDir string) *Service {
	return &Service{db: db, settings: store, dataDir: dataDir, now: time.Now}
}

func (s *Service) ImagesDir() string {
	dir := filepath.Join(s.dataDir, "images")
	os.MkdirAll(dir, 0o755)
	return dir
}

func (s *Service) ThumbnailsDir() string {
	dir := filepath.Join(s.dataDir, "image_thumbnails")
	os.MkdirAll(dir, 0o755)
	return dir
}

func (s *Service) nextSeq() int64 {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	s.lastSeq = max(time.Now().UnixNano(), s.lastSeq+1)
	return s.lastSeq
}

// SafeRel is _safe_relative_path: drop empty and "." segments, refuse "..".
func SafeRel(rel string) (string, error) {
	value := strings.TrimLeft(strings.TrimSpace(strings.ReplaceAll(rel, "\\", "/")), "/")
	parts := []string{}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", errNotFound
		}
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if value == "" {
		return "", errNotFound
	}
	if len(parts) == 0 {
		return ".", nil
	}
	return strings.Join(parts, "/"), nil
}

func isImageRel(rel string) bool {
	safe, err := SafeRel(rel)
	return err == nil && imageExtensions[strings.ToLower(path.Ext(safe))]
}

// localPath is _local_image_path.
func (s *Service) localPath(rel string) (string, error) {
	safe, err := SafeRel(rel)
	if err != nil {
		return "", err
	}
	root, _ := filepath.Abs(s.ImagesDir())
	full := filepath.Join(root, filepath.FromSlash(safe))
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", errNotFound
	}
	return full, nil
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func dimensions(data []byte) (int, int, bool) {
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, false
	}
	return config.Width, config.Height, true
}

func fileDimensions(p string) (int, int, bool) {
	f, err := os.Open(p)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	config, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, false
	}
	return config.Width, config.Height, true
}

func isGIF(data []byte) bool {
	return bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))
}

// dateOf is the item "date": YYYY-MM-DD from the YYYY/MM/DD folders, else the file's mtime.
func dateOf(rel string, mtime time.Time) string {
	parts := strings.Split(rel, "/")
	if len(parts) >= 4 {
		return strings.Join(parts[:3], "-")
	}
	return mtime.Format("2006-01-02")
}

// ---- index ----

type item = map[string]any

func decodeItem(raw string) item {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var out item
	if decoder.Decode(&out) != nil || out == nil {
		return item{}
	}
	return out
}

func encodeItem(it item) string {
	raw, _ := json.Marshal(it)
	return string(raw)
}

func rowFor(rel string, seq int64, it item) indexRow {
	webdav, _ := it["webdav"].(bool)
	return indexRow{Rel: rel, Seq: seq, Date: fmt.Sprint(it["date"]), CreatedAt: fmt.Sprint(it["created_at"]), Webdav: webdav, Item: encodeItem(it)}
}

func (s *Service) upsert(tx *gorm.DB, row indexRow) error {
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "rel"}},
		DoUpdates: clause.AssignmentColumns([]string{"date", "created_at", "webdav", "item"})}).Create(&row).Error
}

// webdavItem is the index entry of rel when it has a WebDAV copy.
func (s *Service) indexItem(rel string) item {
	var row indexRow
	if s.db.Where("rel = ?", rel).Limit(1).Find(&row).Error != nil || row.Rel == "" {
		return item{}
	}
	return decodeItem(row.Item)
}

func (s *Service) mode() string {
	mode, _ := s.settings.ImageStorage()["mode"].(string)
	if mode != "local" && mode != "webdav" && mode != "both" {
		return "local"
	}
	return mode
}

func (s *Service) publicURL(rel, baseURL string) string {
	safe, _ := SafeRel(rel)
	if public, _ := s.settings.ImageStorage()["public_base_url"].(string); strings.TrimSpace(public) != "" {
		return strings.TrimRight(strings.TrimSpace(public), "/") + "/" + safe
	}
	if baseURL == "" {
		baseURL = s.settings.BaseURL()
	}
	return strings.TrimRight(baseURL, "/") + "/images/" + safe
}

// Save is ImageStorageService.save: write the file (and/or WebDAV), index it, return its URL.
func (s *Service) Save(data []byte, baseURL string) (string, error) {
	s.maybeCleanupOld()
	now := s.now()
	sum := md5.Sum(data)
	ext := "png"
	if isGIF(data) {
		ext = "gif"
	}
	rel := fmt.Sprintf("%s/%d_%s.%s", now.Format("2006/01/02"), now.Unix(), hex.EncodeToString(sum[:]), ext)
	mode := s.mode()
	storedLocal, storedWebDAV, remoteURL := false, false, ""
	if mode == "local" || mode == "both" {
		full, err := s.localPath(rel)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			return "", err
		}
		storedLocal = true
	}
	if mode == "webdav" || mode == "both" {
		url, err := s.webdav().Put(rel, data, "image/png")
		if err != nil {
			return "", err
		}
		remoteURL, storedWebDAV = url, true
	}
	storage := "local"
	if storedLocal && storedWebDAV {
		storage = "both"
	} else if storedWebDAV {
		storage = "webdav"
	}
	it := item{
		"rel": rel, "path": rel, "name": path.Base(rel), "date": strings.Join(strings.Split(rel, "/")[:3], "-"),
		"size": len(data), "created_at": now.Format("2006-01-02 15:04:05"), "storage": storage,
		"local": storedLocal, "webdav": storedWebDAV, "remote_url": remoteURL,
	}
	if w, h, ok := dimensions(data); ok {
		it["width"], it["height"] = w, h
	}
	s.mu.RLock() // one new row: concurrent saves do not conflict, only whole-index work must wait
	err := s.upsert(s.db, rowFor(rel, s.nextSeq(), it))
	s.mu.RUnlock()
	if err != nil {
		return "", err
	}
	return s.publicURL(rel, baseURL), nil
}

// GetBytes is get_bytes: the local file, else the WebDAV copy.
func (s *Service) GetBytes(rel string) ([]byte, error) {
	safe, err := SafeRel(rel)
	if err != nil {
		return nil, err
	}
	if !isImageRel(safe) {
		return nil, errNotFound
	}
	full, err := s.localPath(safe)
	if err != nil {
		return nil, err
	}
	if isFile(full) {
		return os.ReadFile(full)
	}
	if webdav, _ := s.indexItem(safe)["webdav"].(bool); webdav {
		return s.webdav().Get(safe)
	}
	return nil, errNotFound
}

// Exists is exists(): a local file or an indexed WebDAV copy.
func (s *Service) Exists(rel string) bool {
	safe, err := SafeRel(rel)
	if err != nil || !isImageRel(safe) {
		return false
	}
	if full, err := s.localPath(safe); err == nil && isFile(full) {
		return true
	}
	webdav, _ := s.indexItem(safe)["webdav"].(bool)
	return webdav
}

// HasLocal is has_local (an invalid path is a 404, as in Python).
func (s *Service) HasLocal(rel string) (bool, error) {
	safe, err := SafeRel(rel)
	if err != nil {
		return false, err
	}
	if !isImageRel(safe) {
		return false, nil
	}
	full, err := s.localPath(safe)
	if err != nil {
		return false, err
	}
	return isFile(full), nil
}

// ListItems is list_items: index entries in the date range, newest first; reconciles with the
// disk at most once a minute, or now with refresh.
func (s *Service) ListItems(baseURL, startDate, endDate string, refresh bool) ([]item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if refresh || time.Since(s.lastReconcile) >= reconcileInterval {
		if err := s.reconcileLocked(); err != nil {
			return nil, err
		}
		s.lastReconcile = time.Now()
	}
	query := s.db.Model(&indexRow{})
	if startDate != "" {
		query = query.Where("date >= ?", startDate)
	}
	if endDate != "" {
		query = query.Where("date <= ?", endDate)
	}
	var rows []indexRow
	if err := query.Order("created_at DESC").Order("seq ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]item, 0, len(rows))
	for _, row := range rows {
		if !isImageRel(row.Rel) {
			continue
		}
		it := decodeItem(row.Item)
		it["rel"], it["path"], it["url"] = row.Rel, row.Rel, s.publicURL(row.Rel, baseURL)
		out = append(out, it)
	}
	return out, nil
}

type diskFile struct {
	rel  string
	full string
	info fs.FileInfo
}

// walkImages lists the image files under data/images, sorted by path.
func (s *Service) walkImages(onlyImages bool) []diskFile {
	root := s.ImagesDir()
	files := []diskFile{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if onlyImages && !isImageRel(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		files = append(files, diskFile{rel: filepath.ToSlash(rel), full: p, info: info})
		return nil
	})
	return files
}

func localItem(file diskFile) item {
	it := item{
		"rel": file.rel, "path": file.rel, "name": path.Base(file.rel), "date": dateOf(file.rel, file.info.ModTime()),
		"size": file.info.Size(), "created_at": file.info.ModTime().Format("2006-01-02 15:04:05"),
		"storage": "local", "local": true, "webdav": false,
	}
	if w, h, ok := fileDimensions(file.full); ok {
		it["width"], it["height"] = w, h
	}
	return it
}

// reconcileLocked is _reconcile_locked: add files missing from the index, drop entries whose file
// is gone (and that have no WebDAV copy), fix local/storage flags.
func (s *Service) reconcileLocked() error {
	onDisk := map[string]diskFile{}
	for _, file := range s.walkImages(true) {
		onDisk[file.rel] = file
	}
	var rows []indexRow
	if err := s.db.Order("seq ASC").Find(&rows).Error; err != nil {
		return err
	}
	indexed := map[string]bool{}
	return s.db.Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			indexed[row.Rel] = true
			_, local := onDisk[row.Rel]
			if !isImageRel(row.Rel) || (!local && !row.Webdav) {
				if err := tx.Delete(&indexRow{}, "rel = ?", row.Rel).Error; err != nil {
					return err
				}
				continue
			}
			it := decodeItem(row.Item)
			storage := "local"
			if local && row.Webdav {
				storage = "both"
			} else if row.Webdav {
				storage = "webdav"
			}
			if it["local"] != local || it["storage"] != storage {
				it["local"], it["storage"] = local, storage
				if err := s.upsert(tx, rowFor(row.Rel, row.Seq, it)); err != nil {
					return err
				}
			}
		}
		missing := []diskFile{}
		for _, file := range onDisk {
			if !indexed[file.rel] {
				missing = append(missing, file)
			}
		}
		sort.Slice(missing, func(i, j int) bool { return missing[i].rel < missing[j].rel })
		for _, file := range missing {
			if err := s.upsert(tx, rowFor(file.rel, s.nextSeq(), localItem(file))); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteMany is delete_many: remove the files and WebDAV copies, drop the index entries; returns
// how many were actually deleted.
func (s *Service) DeleteMany(rels []string) (int, error) {
	safeRels, seen := []string{}, map[string]bool{}
	for _, rel := range rels {
		safe, err := SafeRel(rel)
		if err != nil {
			return 0, err
		}
		if !seen[safe] {
			seen[safe] = true
			safeRels = append(safeRels, safe)
		}
	}
	removed := map[string]bool{}
	for _, safe := range safeRels {
		full, err := s.localPath(safe)
		if err != nil {
			return 0, err
		}
		if isFile(full) && os.Remove(full) == nil {
			removed[safe] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []indexRow
	for start := 0; start < len(safeRels); start += 500 {
		batch := []indexRow{}
		if err := s.db.Where("rel IN ?", safeRels[start:min(len(safeRels), start+500)]).Find(&batch).Error; err != nil {
			return len(removed), err
		}
		rows = append(rows, batch...)
	}
	var client *WebDAV
	for _, row := range rows {
		if !row.Webdav {
			continue
		}
		if client == nil {
			client = s.webdav()
		}
		deleted, err := client.Delete(row.Rel)
		if err != nil && !removed[row.Rel] {
			return len(removed), err
		}
		if deleted {
			removed[row.Rel] = true
		}
	}
	for start := 0; start < len(rows); start += 500 {
		batch := []string{}
		for _, row := range rows[start:min(len(rows), start+500)] {
			batch = append(batch, row.Rel)
		}
		if err := s.db.Where("rel IN ?", batch).Delete(&indexRow{}).Error; err != nil {
			return len(removed), err
		}
	}
	return len(removed), nil
}

// SyncAll is sync_all: upload every local image that has no WebDAV copy yet.
func (s *Service) SyncAll() (map[string]int, error) {
	if mode := s.mode(); mode != "webdav" && mode != "both" {
		return nil, &StorageError{"WebDAV 图片存储未启用"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	client := s.webdav()
	uploaded, skipped, failed := 0, 0, 0
	for _, file := range s.walkImages(true) {
		var row indexRow
		s.db.Where("rel = ?", file.rel).Limit(1).Find(&row)
		existing := item{}
		if row.Rel != "" {
			existing = decodeItem(row.Item)
		}
		if webdav, _ := existing["webdav"].(bool); webdav {
			skipped++
			continue
		}
		payload, err := os.ReadFile(file.full)
		if err != nil {
			failed++
			continue
		}
		remoteURL, err := client.Put(file.rel, payload, "image/png")
		if err != nil {
			failed++
			continue
		}
		it := item{}
		for k, v := range existing {
			it[k] = v
		}
		createdAt := fmt.Sprint(existing["created_at"])
		if existing["created_at"] == nil || createdAt == "" {
			createdAt = file.info.ModTime().Format("2006-01-02 15:04:05")
		}
		for k, v := range (item{"rel": file.rel, "path": file.rel, "name": path.Base(file.rel), "date": dateOf(file.rel, file.info.ModTime()),
			"size": len(payload), "created_at": createdAt, "storage": "both", "local": true, "webdav": true, "remote_url": remoteURL}) {
			it[k] = v
		}
		if w, h, ok := dimensions(payload); ok {
			it["width"], it["height"] = w, h
		}
		seq := row.Seq
		if row.Rel == "" {
			seq = s.nextSeq()
		}
		if s.upsert(s.db, rowFor(file.rel, seq, it)) != nil {
			failed++
			continue
		}
		uploaded++
	}
	return map[string]int{"uploaded": uploaded, "skipped": skipped, "failed": failed}, nil
}

// TestWebDAV is test_webdav.
func (s *Service) TestWebDAV() map[string]any { return s.webdav().Test() }

// ---- tags ----

func (s *Service) loadTagRows() ([]tagRow, error) {
	var rows []tagRow
	err := s.db.Order("seq ASC").Find(&rows).Error
	return rows, err
}

func decodeTags(raw string) []string {
	var tags []string
	if json.Unmarshal([]byte(raw), &tags) != nil {
		return []string{}
	}
	return tags
}

// Tags is load_tags.
func (s *Service) Tags() (map[string][]string, error) {
	rows, err := s.loadTagRows()
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, row := range rows {
		out[row.Rel] = decodeTags(row.Tags)
	}
	return out, nil
}

// SetTags is set_tags: unique, trimmed, non-empty; none removes the entry.
func (s *Service) SetTags(rel string, tags []string) ([]string, error) {
	cleaned, seen := []string{}, map[string]bool{}
	for _, tag := range tags {
		if tag = strings.TrimSpace(tag); tag != "" && !seen[tag] {
			seen[tag] = true
			cleaned = append(cleaned, tag)
		}
	}
	if len(cleaned) == 0 {
		return cleaned, s.db.Delete(&tagRow{}, "rel = ?", rel).Error
	}
	raw, _ := json.Marshal(cleaned)
	var existing tagRow
	s.db.Where("rel = ?", rel).Limit(1).Find(&existing)
	if existing.Rel != "" {
		return cleaned, s.db.Model(&tagRow{}).Where("rel = ?", rel).Update("tags", string(raw)).Error
	}
	return cleaned, s.db.Create(&tagRow{Rel: rel, Seq: s.nextSeq(), Tags: string(raw)}).Error
}

// RemoveTagsMany is remove_tags_many.
func (s *Service) RemoveTagsMany(rels []string) error {
	for start := 0; start < len(rels); start += 500 {
		if err := s.db.Where("rel IN ?", rels[start:min(len(rels), start+500)]).Delete(&tagRow{}).Error; err != nil {
			return err
		}
	}
	return nil
}

// DeleteTag is delete_tag: remove the tag everywhere; returns the number of images that had it.
func (s *Service) DeleteTag(tag string) (int, error) {
	rows, err := s.loadTagRows()
	if err != nil {
		return 0, err
	}
	count := 0
	err = s.db.Transaction(func(tx *gorm.DB) error {
		for _, row := range rows {
			tags := decodeTags(row.Tags)
			kept := []string{}
			for _, t := range tags {
				if t != tag {
					kept = append(kept, t)
				}
			}
			if len(kept) == len(tags) {
				continue
			}
			count++
			if len(kept) == 0 {
				if err := tx.Delete(&tagRow{}, "rel = ?", row.Rel).Error; err != nil {
					return err
				}
				continue
			}
			raw, _ := json.Marshal(kept)
			if err := tx.Model(&tagRow{}).Where("rel = ?", row.Rel).Update("tags", string(raw)).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return count, err
}

// AllTags is get_all_tags: every tag once, in first-seen order.
func (s *Service) AllTags() ([]string, error) {
	rows, err := s.loadTagRows()
	if err != nil {
		return nil, err
	}
	seen, out := map[string]bool{}, []string{}
	for _, row := range rows {
		for _, tag := range decodeTags(row.Tags) {
			if !seen[tag] {
				seen[tag] = true
				out = append(out, tag)
			}
		}
	}
	return out, nil
}

// IsHTTPError unwraps an HTTPError.
func IsHTTPError(err error) (*HTTPError, bool) {
	var target *HTTPError
	ok := errors.As(err, &target)
	return target, ok
}

// LocalPath is the file path of rel under data/images.
func LocalPath(s *Service, rel string) (string, error) { return s.localPath(rel) }

// ExportIndex is image_index.json: {"items": {rel: item}} (backups).
func (s *Service) ExportIndex() (map[string]any, error) {
	var rows []indexRow
	if err := s.db.Order("seq ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	items := map[string]any{}
	for _, row := range rows {
		items[row.Rel] = decodeItem(row.Item)
	}
	return map[string]any{"items": items}, nil
}
