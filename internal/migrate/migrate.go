// Package migrate copies the Python version's data directory (JSON files) into the Go tables.
//
// Snapshot data (accounts, API keys, proxies, image index and tags, tasks, account sources,
// backup state) is mirrored: each table is replaced in one transaction, so running the
// migration again gives the same rows. Logs keep only the last N days and are upserted by id;
// daily image counts are upserted. Source files are opened read-only and never written.
// config.json and the data/images, data/image_thumbnails and data/files directories are not
// copied: the Go version reads them in place.
package migrate

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/basketikun/chatgpt2api/internal/compat"
	"github.com/basketikun/chatgpt2api/internal/docstore"
	"github.com/basketikun/chatgpt2api/internal/filetasks"
	"github.com/basketikun/chatgpt2api/internal/imagestore"
	"github.com/basketikun/chatgpt2api/internal/imagetasks"
	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/pool"
)

// Options say what to migrate.
type Options struct {
	DataDir  string
	LogDays  int            // keep log lines from the last LogDays calendar days (server local time); 0 = all
	Now      time.Time      // "today" for the log window
	Location *time.Location // the Python server's time zone (log times are local); default time.Local
}

func (o Options) withDefaults() Options {
	if o.Location == nil {
		o.Location = time.Local
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	return o
}

// Report counts what was read and written per item.
type Report struct {
	Items []Item
}

// Item is one migrated source.
type Item struct {
	Name    string
	Source  int // entries in the source file (for logs: lines inside the window)
	Written int
	Skipped string // why nothing was done (missing file)
}

func (r *Report) add(name string, source, written int, skipped string) {
	r.Items = append(r.Items, Item{Name: name, Source: source, Written: written, Skipped: skipped})
}

// Tables are the row types written here; they mirror the packages' own models.
type accountRow struct {
	Token    string `gorm:"primaryKey"`
	Position int64
	Data     string
}

func (accountRow) TableName() string { return "compat_accounts" }

type kvRow struct {
	Key   string `gorm:"primaryKey"`
	Value string
}

func (kvRow) TableName() string { return "compat_kv" }

type imageRow struct {
	Rel       string `gorm:"primaryKey"`
	Seq       int64
	Date      string
	CreatedAt string
	Webdav    bool
	Item      string
}

func (imageRow) TableName() string { return "compat_images" }

type tagRow struct {
	Rel  string `gorm:"primaryKey"`
	Seq  int64
	Tags string
}

func (tagRow) TableName() string { return "compat_image_tags" }

type taskRow struct {
	Key       string `gorm:"primaryKey"`
	Owner     string
	UpdatedAt string
	Body      string
}

func (taskRow) TableName() string { return "compat_image_tasks" }

type fileTaskRow struct {
	Key  string `gorm:"primaryKey"`
	Body string
}

func (fileTaskRow) TableName() string { return "compat_editable_file_tasks" }

// readJSON reads a source file; found is false when it does not exist.
func readJSON(path string) (any, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, true, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return value, true, nil
}

func str(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func listOf(value any, key string) []any {
	switch v := value.(type) {
	case []any:
		return v
	case map[string]any:
		if list, ok := v[key].([]any); ok {
			return list
		}
	}
	return nil
}

// replace empties a table and writes rows, in one transaction.
func replace[T any](db *gorm.DB, model T, rows []T) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		return tx.CreateInBatches(rows, 200).Error
	})
}

// Run migrates everything under opts.DataDir.
func Run(db *gorm.DB, opts Options) (*Report, error) {
	for _, migrate := range []func(*gorm.DB) error{compat.Migrate, logs.Migrate, pool.Migrate, docstore.Migrate, imagestore.Migrate,
		imagetasks.Migrate, filetasks.Migrate} {
		if err := migrate(db); err != nil {
			return nil, err
		}
	}
	opts = opts.withDefaults()
	report := &Report{}
	path := func(name string) string { return filepath.Join(opts.DataDir, name) }
	steps := []func() error{
		func() error { return migrateAccounts(db, path("accounts.json"), path(".cumulative_total"), report) },
		func() error { return migrateAuthKeys(db, path("auth_keys.json"), report) },
		func() error { return migrateDocument(db, path("proxies.json"), "proxies", report) },
		func() error { return migrateDocument(db, path("cpa_config.json"), "cpa_config.json", report) },
		func() error { return migrateDocument(db, path("sub2api_config.json"), "sub2api_config.json", report) },
		func() error { return migrateDocument(db, path("backup_state.json"), "backup_state.json", report) },
		func() error { return migrateImageIndex(db, path("image_index.json"), report) },
		func() error { return migrateTags(db, path("image_tags.json"), report) },
		func() error { return migrateImageTasks(db, path("image_tasks.json"), report) },
		func() error { return migrateFileTasks(db, path("editable_file_tasks.json"), report) },
		func() error { return migrateLogs(db, path("logs.jsonl"), opts, report) },
		func() error {
			return migrateCounts(db, path("image_output_counts.json"), path("logs.jsonl"), opts, report)
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return report, err
		}
	}
	return report, nil
}

func migrateAccounts(db *gorm.DB, file, cumulativeFile string, report *Report) error {
	value, found, err := readJSON(file)
	if err != nil {
		return err
	}
	if !found {
		report.add("accounts", 0, 0, "accounts.json 不存在")
		return nil
	}
	// like the Python dict built from the list: a repeated token keeps its first position and
	// takes the last entry's values
	rows, index := []accountRow{}, map[string]int{}
	source := listOf(value, "items")
	for _, raw := range source {
		account, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		token := str(account["access_token"])
		if token == "" {
			continue
		}
		data, _ := json.Marshal(account)
		if i, seen := index[token]; seen {
			rows[i].Data = string(data)
			continue
		}
		index[token] = len(rows)
		rows = append(rows, accountRow{Token: token, Position: int64(len(rows)), Data: string(data)})
	}
	if err := replace(db, accountRow{}, rows); err != nil {
		return err
	}
	report.add("accounts", len(source), len(rows), "")
	if raw, err := os.ReadFile(cumulativeFile); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			if err := db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&kvRow{Key: "cumulative_total", Value: strconv.Itoa(n)}).Error; err != nil {
				return err
			}
			report.add("cumulative_total", 1, 1, "")
		}
	}
	return nil
}

func migrateAuthKeys(db *gorm.DB, file string, report *Report) error {
	value, found, err := readJSON(file)
	if err != nil {
		return err
	}
	if !found {
		report.add("auth_keys", 0, 0, "auth_keys.json 不存在")
		return nil
	}
	source := listOf(value, "items")
	rows, seen := []compat.AuthKey{}, map[string]bool{}
	for _, raw := range source {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, hash := strings.ToLower(str(item["role"])), str(item["key_hash"])
		if (role != "admin" && role != "user") || hash == "" || seen[hash] {
			continue
		}
		seen[hash] = true
		id := str(item["id"])
		if id == "" {
			sum := sha1.Sum([]byte(hash))
			id = hex.EncodeToString(sum[:])[:12]
		}
		name := str(item["name"])
		if name == "" {
			name = map[string]string{"admin": "管理员密钥", "user": "普通用户"}[role]
		}
		enabled := true
		if flag, ok := item["enabled"].(bool); ok {
			enabled = flag
		}
		var lastUsed *string
		if text := str(item["last_used_at"]); text != "" {
			lastUsed = &text
		}
		rows = append(rows, compat.AuthKey{ID: id, Name: name, Role: role, KeyHash: hash, Enabled: enabled, CreatedAt: str(item["created_at"]),
			LastUsedAt: lastUsed, Position: int64(len(rows))})
	}
	if err := replace(db, compat.AuthKey{}, rows); err != nil {
		return err
	}
	report.add("auth_keys", len(source), len(rows), "")
	return nil
}

func migrateDocument(db *gorm.DB, file, name string, report *Report) error {
	value, found, err := readJSON(file)
	if err != nil {
		return err
	}
	if !found {
		report.add(name, 0, 0, filepath.Base(file)+" 不存在")
		return nil
	}
	if err := docstore.New(db).Save(name, value); err != nil {
		return err
	}
	count := 1
	if list := listOf(value, "items"); list != nil {
		count = len(list)
	}
	report.add(name, count, count, "")
	return nil
}

func isImageRel(rel string) bool {
	ext := strings.ToLower(filepath.Ext(rel))
	return ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".webp" || ext == ".gif"
}

func migrateImageIndex(db *gorm.DB, file string, report *Report) error {
	value, found, err := readJSON(file)
	if err != nil {
		return err
	}
	if !found {
		report.add("image_index", 0, 0, "image_index.json 不存在")
		return nil
	}
	root, _ := value.(map[string]any)
	items, _ := root["items"].(map[string]any)
	// json.Decoder into a map loses the file order; read the keys in order separately
	order := orderedKeys(file, "items")
	rows := []imageRow{}
	for _, rel := range order {
		item, ok := items[rel].(map[string]any)
		if !ok {
			continue
		}
		safe, err := imagestore.SafeRel(rel)
		if err != nil || safe != rel || !isImageRel(rel) {
			continue // the Go index drops these too (_load_clean_index)
		}
		webdav, _ := item["webdav"].(bool)
		data, _ := json.Marshal(item)
		rows = append(rows, imageRow{Rel: rel, Seq: int64(len(rows) + 1), Date: str(item["date"]), CreatedAt: str(item["created_at"]), Webdav: webdav, Item: string(data)})
	}
	if err := replace(db, imageRow{}, rows); err != nil {
		return err
	}
	report.add("image_index", len(items), len(rows), "")
	return nil
}

func migrateTags(db *gorm.DB, file string, report *Report) error {
	value, found, err := readJSON(file)
	if err != nil {
		return err
	}
	if !found {
		report.add("image_tags", 0, 0, "image_tags.json 不存在")
		return nil
	}
	tags, _ := value.(map[string]any)
	rows := []tagRow{}
	for _, rel := range orderedKeys(file, "") {
		list, ok := tags[rel].([]any)
		if !ok || len(list) == 0 {
			continue
		}
		data, _ := json.Marshal(list)
		rows = append(rows, tagRow{Rel: rel, Seq: int64(len(rows) + 1), Tags: string(data)})
	}
	if err := replace(db, tagRow{}, rows); err != nil {
		return err
	}
	report.add("image_tags", len(tags), len(rows), "")
	return nil
}

func migrateImageTasks(db *gorm.DB, file string, report *Report) error {
	value, found, err := readJSON(file)
	if err != nil {
		return err
	}
	if !found {
		report.add("image_tasks", 0, 0, "image_tasks.json 不存在")
		return nil
	}
	source := listOf(value, "tasks")
	rows, seen := []taskRow{}, map[string]bool{}
	for _, raw := range source {
		task, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, owner := str(task["id"]), str(task["owner_id"])
		if id == "" || owner == "" || seen[owner+":"+id] {
			continue
		}
		seen[owner+":"+id] = true
		data, _ := json.Marshal(task)
		rows = append(rows, taskRow{Key: owner + ":" + id, Owner: owner, UpdatedAt: str(task["updated_at"]), Body: string(data)})
	}
	if err := replace(db, taskRow{}, rows); err != nil {
		return err
	}
	report.add("image_tasks", len(source), len(rows), "")
	return nil
}

func migrateFileTasks(db *gorm.DB, file string, report *Report) error {
	value, found, err := readJSON(file)
	if err != nil {
		return err
	}
	if !found {
		report.add("editable_file_tasks", 0, 0, "editable_file_tasks.json 不存在")
		return nil
	}
	source := listOf(value, "tasks")
	rows, seen := []fileTaskRow{}, map[string]bool{}
	for _, raw := range source {
		task, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, owner := str(task["id"]), str(task["owner_id"])
		if id == "" || owner == "" || seen[owner+":"+id] {
			continue
		}
		seen[owner+":"+id] = true
		data, _ := json.Marshal(task)
		rows = append(rows, fileTaskRow{Key: owner + ":" + id, Body: string(data)})
	}
	if err := replace(db, fileTaskRow{}, rows); err != nil {
		return err
	}
	report.add("editable_file_tasks", len(source), len(rows), "")
	return nil
}

// LegacyLogID is _legacy_id(raw, 0): the id log_service.list shows for lines without one.
func LegacyLogID(raw string) string {
	sum := sha1.Sum([]byte("0:" + raw))
	return hex.EncodeToString(sum[:])[:24]
}

// windowStart is the first kept day (YYYY-MM-DD) of the log window.
func windowStart(opts Options) string {
	if opts.LogDays <= 0 {
		return ""
	}
	return opts.Now.In(opts.Location).AddDate(0, 0, -(opts.LogDays - 1)).Format("2006-01-02")
}

func eachLogLine(file string, fn func(index int, raw string) error) (bool, error) {
	handle, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer handle.Close()
	scanner := bufio.NewScanner(handle)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	index := 0
	for scanner.Scan() {
		index++
		if err := fn(index, scanner.Text()); err != nil {
			return true, err
		}
	}
	return true, scanner.Err()
}

func migrateLogs(db *gorm.DB, file string, opts Options, report *Report) error {
	from := windowStart(opts)
	batch := []logs.Entry{}
	inWindow, written := 0, 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.AssignmentColumns([]string{"seq", "time", "type", "summary", "detail"})}).
			CreateInBatches(batch, 500).Error
		written += len(batch)
		batch = batch[:0]
		return err
	}
	seenIDs := map[string]bool{}
	found, err := eachLogLine(file, func(index int, raw string) error {
		line := strings.TrimSpace(raw)
		if line == "" {
			return nil
		}
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber()
		var item map[string]any
		if decoder.Decode(&item) != nil || item == nil {
			return nil
		}
		at := str(item["time"])
		if from != "" && (len(at) < 10 || at[:10] < from) {
			return nil
		}
		inWindow++
		id := str(item["id"])
		if id == "" {
			id = LegacyLogID(raw)
		}
		if seenIDs[id] {
			return nil
		}
		seenIDs[id] = true
		detail := item["detail"]
		if detail == nil {
			detail = map[string]any{}
		}
		data, _ := json.Marshal(detail)
		batch = append(batch, logs.Entry{ID: id, Seq: int64(index), Time: at, Type: str(item["type"]), Summary: str(item["summary"]), Detail: string(data)})
		if len(batch) >= 2000 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		report.add("logs", 0, 0, "logs.jsonl 不存在")
		return nil
	}
	if err := flush(); err != nil {
		return err
	}
	report.add("logs", inWindow, written, "")
	return nil
}

var beijing = time.FixedZone("UTC+8", 8*3600)

func migrateCounts(db *gorm.DB, file, logFile string, opts Options, report *Report) error {
	days := map[string][2]int64{}
	value, found, err := readJSON(file)
	if err != nil {
		return err
	}
	if found {
		root, _ := value.(map[string]any)
		entries, _ := root["days"].(map[string]any)
		for day, raw := range entries {
			kinds, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if _, err := time.Parse("2006-01-02", day); err != nil {
				continue
			}
			days[day] = [2]int64{number(kinds["edits"]), number(kinds["generations"])}
		}
	} else {
		// like _scan_daily_counts: rebuild the daily totals from every log line
		_, err := eachLogLine(logFile, func(_ int, raw string) error {
			if !strings.Contains(raw, `"type":"call"`) || !strings.Contains(raw, "/v1/images/") {
				return nil
			}
			var item map[string]any
			if json.Unmarshal([]byte(raw), &item) != nil || item["type"] != "call" {
				return nil
			}
			detail, _ := item["detail"].(map[string]any)
			if detail == nil || detail["status"] != "success" {
				return nil
			}
			column := map[string]int{"/v1/images/edits": 0, "/v1/images/generations": 1}
			index, ok := column[str(detail["endpoint"])]
			urls, _ := detail["urls"].([]any)
			at := str(item["time"])
			if !ok || len(urls) == 0 || len(at) < 19 {
				return nil
			}
			parsed, err := time.ParseInLocation("2006-01-02 15:04:05", at[:19], opts.Location)
			if err != nil {
				return nil
			}
			day := parsed.In(beijing).Format("2006-01-02")
			counts := days[day]
			counts[index] += int64(len(urls))
			days[day] = counts
			return nil
		})
		if err != nil {
			return err
		}
	}
	rows := []logs.DailyCount{}
	keys := make([]string, 0, len(days))
	for day := range days {
		keys = append(keys, day)
	}
	sort.Strings(keys)
	for _, day := range keys {
		rows = append(rows, logs.DailyCount{Day: day, Edits: days[day][0], Generations: days[day][1]})
	}
	if len(rows) > 0 {
		if err := db.Clauses(clause.OnConflict{UpdateAll: true}).CreateInBatches(rows, 500).Error; err != nil {
			return err
		}
	}
	source := "image_output_counts.json"
	if !found {
		source = "logs.jsonl（重新统计）"
	}
	report.add("image_output_counts", len(days), len(rows), map[bool]string{true: "", false: "来源：" + source}[found])
	return nil
}

func number(value any) int64 {
	if n, ok := value.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i
		}
		if f, err := n.Float64(); err == nil {
			return int64(f)
		}
	}
	return 0
}

// orderedKeys lists the keys of a JSON object in file order: the whole document when key is "",
// else the object under that top-level key. Go maps lose the order the Python dicts kept.
func orderedKeys(path, key string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if key != "" {
		if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
			return nil
		}
		for decoder.More() {
			name, err := decoder.Token()
			if err != nil {
				return nil
			}
			if name == key {
				return objectKeys(decoder)
			}
			var skip json.RawMessage
			if decoder.Decode(&skip) != nil {
				return nil
			}
		}
		return nil
	}
	return objectKeys(decoder)
}

// objectKeys reads the next value; when it is an object, returns its keys in order.
func objectKeys(decoder *json.Decoder) []string {
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil
	}
	keys := []string{}
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil {
			return keys
		}
		if text, ok := name.(string); ok {
			keys = append(keys, text)
		}
		var skip json.RawMessage
		if decoder.Decode(&skip) != nil {
			return keys
		}
	}
	return keys
}
