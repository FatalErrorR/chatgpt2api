// Package migrate copies the Python version's data directory (JSON files) into the Go tables.
//
// It runs in three phases so a bad source can never leave the target half-written:
//
//  1. read: every source file is parsed and checked against the shapes the Python version loads;
//     a file in any other shape (or a missing accounts.json) stops the migration before anything
//     is written.
//  2. write: all tables are replaced (mirrored) in one database transaction.
//  3. verify: inside the same transaction, the tables are compared with what the source files
//     say should be there, computed separately from the write path; any difference rolls the
//     whole transaction back.
//
// Source files are opened read-only. config.json and the data/images, data/image_thumbnails and
// data/files directories are not copied: the Go version reads them in place.
package migrate

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

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
	// afterWrite runs inside the transaction between writing and verifying (tests use it to
	// prove that a failed verification rolls everything back).
	afterWrite func(tx *gorm.DB) error
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

// Report counts what was read and written per item, and the verification results.
type Report struct {
	Items  []Item
	Checks []Check
}

// Item is one migrated source.
type Item struct {
	Name    string
	Source  int // entries in the source file (for logs: valid lines inside the window)
	Written int
	Note    string // missing file, entries Python would drop, where the data came from
}

// Check is one verification result.
type Check struct {
	Name     string
	Expected int
	Actual   int
	Detail   string
	OK       bool
}

func (r *Report) add(name string, source, written int, note string) {
	r.Items = append(r.Items, Item{Name: name, Source: source, Written: written, Note: note})
}

// Failed reports whether any verification failed.
func (r *Report) Failed() bool {
	for _, check := range r.Checks {
		if !check.OK {
			return true
		}
	}
	return false
}

// SourceError is a source file that is not in a shape the Python version loads.
type SourceError struct {
	File   string
	Reason string
}

func (e *SourceError) Error() string { return e.File + "：" + e.Reason }

// ---- target rows (they mirror the packages' own models) ----

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

type documentRow struct {
	Name string `gorm:"primaryKey"`
	Data string
}

func (documentRow) TableName() string { return "compat_documents" }

// ---- reading ----

// readJSON reads a source file; found is false when it does not exist. Invalid JSON is a
// SourceError: Python would read it as empty, and mirroring that would wipe the table.
func readJSON(path string) (any, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	value, err := decodeJSONStrict(raw)
	if err != nil {
		return nil, true, &SourceError{filepath.Base(path), "不是有效的 JSON：" + err.Error()}
	}
	return value, true, nil
}

// decodeJSONStrict is json.loads: exactly one JSON value, with only whitespace after it. A bare
// Decoder.Decode stops after the first value, so "[] garbage" or a second value would pass as [].
func decodeJSONStrict(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("第一个 JSON 值之后还有其他内容（偏移 %d）", decoder.InputOffset())
	}
	return value, nil
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

// accountToken is the token Python's _normalize_account reads: access_token, else accessToken.
func accountToken(account map[string]any) string {
	if token := str(account["access_token"]); token != "" {
		return token
	}
	return str(account["accessToken"])
}

// plan is everything read from the sources, ready to write.
type plan struct {
	opts        Options
	accounts    []accountRow
	cumulative  *int
	authKeys    []compat.AuthKey
	documents   []documentRow
	images      []imageRow
	tags        []tagRow
	tasks       []taskRow
	fileTasks   []fileTaskRow
	counts      []logs.DailyCount
	countSource string
	logFile     string
	logFound    bool
	maxLogSeq   int64
	report      *Report
}

// InfoKey is the compat_kv row describing the last migration: what the rollback needs to tell the
// rows the Go version wrote from the rows it was given.
const InfoKey = "migration_info"

// Info is the migration_info row.
type Info struct {
	MigratedAt string            `json:"migrated_at"`
	Source     string            `json:"source"`
	LogDays    int               `json:"log_days"`
	Now        string            `json:"now"`
	MaxLogSeq  int64             `json:"max_log_seq"` // migrated log lines keep their line number as seq; new ones are nanoseconds
	Files      map[string]string `json:"files"`       // sha256 of each source file, to check the snapshot was kept as it was
}

// snapshotFiles are the source files whose checksums the migration records.
var snapshotFiles = []string{"accounts.json", ".cumulative_total", "auth_keys.json", "proxies.json", "cpa_config.json", "sub2api_config.json",
	"backup_state.json", "image_index.json", "image_tags.json", "image_tasks.json", "editable_file_tasks.json", "image_output_counts.json", "logs.jsonl"}

// FileChecksums is the sha256 of each snapshot file that exists.
func FileChecksums(dir string) (map[string]string, error) {
	out := map[string]string{}
	for _, name := range snapshotFiles {
		handle, err := os.Open(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sum := sha256.New()
		_, err = io.Copy(sum, handle)
		handle.Close()
		if err != nil {
			return nil, err
		}
		out[name] = hex.EncodeToString(sum.Sum(nil))
	}
	return out, nil
}

func (p *plan) writeInfo(tx *gorm.DB) error {
	files, err := FileChecksums(p.opts.DataDir)
	if err != nil {
		return err
	}
	source, _ := filepath.Abs(p.opts.DataDir)
	raw, _ := json.Marshal(Info{MigratedAt: time.Now().Format(time.RFC3339), Source: source, LogDays: p.opts.LogDays,
		Now: p.opts.Now.Format(time.RFC3339), MaxLogSeq: p.maxLogSeq, Files: files})
	if err := tx.Where("key = ?", InfoKey).Delete(&kvRow{}).Error; err != nil {
		return err
	}
	return tx.Create(&kvRow{Key: InfoKey, Value: string(raw)}).Error
}

// LoadInfo reads the migration_info row; found is false when the database was not made by this tool.
func LoadInfo(db *gorm.DB) (Info, bool, error) {
	var rows []kvRow
	if err := db.Where("key = ?", InfoKey).Limit(1).Find(&rows).Error; err != nil || len(rows) == 0 {
		return Info{}, false, err
	}
	var info Info
	if err := json.Unmarshal([]byte(rows[0].Value), &info); err != nil {
		return Info{}, false, err
	}
	return info, true, nil
}

func (p *plan) path(name string) string { return filepath.Join(p.opts.DataDir, name) }

func listShape(file string, value any, key string) ([]any, error) {
	switch v := value.(type) {
	case []any:
		return v, nil
	case map[string]any:
		if key != "" {
			if list, ok := v[key].([]any); ok {
				return list, nil
			}
		}
	}
	expect := "一个列表"
	if key != "" {
		expect = fmt.Sprintf("一个列表或 {\"%s\": [...]}", key)
	}
	return nil, &SourceError{file, "格式不对，应为" + expect}
}

func (p *plan) readAccounts() error {
	value, found, err := readJSON(p.path("accounts.json"))
	if err != nil {
		return err
	}
	if !found {
		// the one file that must exist: without it this is most likely the wrong directory
		return &SourceError{"accounts.json", "不存在（-data 应指向 Python 版的 data 目录）"}
	}
	list, ok := value.([]any)
	if !ok {
		return &SourceError{"accounts.json", "格式不对，应为账号列表（Python 只读列表，其他格式会被当成空号池）"}
	}
	// like the dict Python builds: a repeated token keeps its first position, the last entry's values
	index, dropped := map[string]int{}, 0
	for _, raw := range list {
		account, ok := raw.(map[string]any)
		token := ""
		if ok {
			token = accountToken(account)
		}
		if token == "" {
			dropped++
			continue
		}
		normalized := map[string]any{}
		for key, value := range account {
			if key != "accessToken" {
				normalized[key] = value
			}
		}
		normalized["access_token"] = token
		data, _ := json.Marshal(normalized)
		if i, seen := index[token]; seen {
			p.accounts[i].Data = string(data)
			continue
		}
		index[token] = len(p.accounts)
		p.accounts = append(p.accounts, accountRow{Token: token, Position: int64(len(p.accounts)), Data: string(data)})
	}
	if len(list) > 0 && len(p.accounts) == 0 {
		return &SourceError{"accounts.json", fmt.Sprintf("有 %d 条记录但没有一条带 access_token / accessToken", len(list))}
	}
	note := ""
	if dropped > 0 {
		note = fmt.Sprintf("%d 条没有 token，Python 同样不加载", dropped)
	}
	p.report.add("accounts", len(list), len(p.accounts), note)

	raw, err := os.ReadFile(p.path(".cumulative_total"))
	if err == nil {
		n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			return &SourceError{".cumulative_total", "不是整数"}
		}
		p.cumulative = &n
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (p *plan) readAuthKeys() error {
	value, found, err := readJSON(p.path("auth_keys.json"))
	if err != nil || !found {
		if !found && err == nil {
			p.report.add("auth_keys", 0, 0, "auth_keys.json 不存在")
		}
		return err
	}
	list, err := listShape("auth_keys.json", value, "items")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, raw := range list {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, hash := strings.ToLower(str(item["role"])), str(item["key_hash"])
		if (role != "admin" && role != "user") || hash == "" || seen[hash] {
			continue // AuthService._normalize_item drops these too
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
		p.authKeys = append(p.authKeys, compat.AuthKey{ID: id, Name: name, Role: role, KeyHash: hash, Enabled: enabled,
			CreatedAt: str(item["created_at"]), LastUsedAt: lastUsed, Position: int64(len(p.authKeys))})
	}
	p.report.add("auth_keys", len(list), len(p.authKeys), "")
	return nil
}

// readDocument reads a whole-file JSON document; check validates its shape.
func (p *plan) readDocument(file, name string, check func(any) error, count func(any) int) error {
	value, found, err := readJSON(p.path(file))
	if err != nil {
		return err
	}
	if !found {
		p.report.add(name, 0, 0, file+" 不存在")
		return nil
	}
	if err := check(value); err != nil {
		return err
	}
	data, _ := json.Marshal(value)
	p.documents = append(p.documents, documentRow{Name: name, Data: string(data)})
	n := count(value)
	p.report.add(name, n, n, "")
	return nil
}

func isImageRel(rel string) bool {
	ext := strings.ToLower(filepath.Ext(rel))
	return ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".webp" || ext == ".gif"
}

func (p *plan) readImageIndex() error {
	value, found, err := readJSON(p.path("image_index.json"))
	if err != nil {
		return err
	}
	if !found {
		p.report.add("image_index", 0, 0, "image_index.json 不存在（Go 启动后会按磁盘重建）")
		return nil
	}
	root, _ := value.(map[string]any)
	items, ok := root["items"].(map[string]any)
	if !ok {
		return &SourceError{"image_index.json", "格式不对，应为 {\"items\": {路径: 条目}}"}
	}
	for _, rel := range orderedKeys(p.path("image_index.json"), "items") {
		item, ok := items[rel].(map[string]any)
		if !ok {
			continue
		}
		safe, err := imagestore.SafeRel(rel)
		if err != nil || safe != rel || !isImageRel(rel) {
			continue // _load_clean_index drops these too
		}
		webdav, _ := item["webdav"].(bool)
		data, _ := json.Marshal(item)
		p.images = append(p.images, imageRow{Rel: rel, Seq: int64(len(p.images) + 1), Date: str(item["date"]), CreatedAt: str(item["created_at"]), Webdav: webdav, Item: string(data)})
	}
	p.report.add("image_index", len(items), len(p.images), "")
	return nil
}

func (p *plan) readTags() error {
	value, found, err := readJSON(p.path("image_tags.json"))
	if err != nil {
		return err
	}
	if !found {
		p.report.add("image_tags", 0, 0, "image_tags.json 不存在")
		return nil
	}
	tags, ok := value.(map[string]any)
	if !ok {
		return &SourceError{"image_tags.json", "格式不对，应为 {路径: [标签]}"}
	}
	for _, rel := range orderedKeys(p.path("image_tags.json"), "") {
		list, ok := tags[rel].([]any)
		if !ok || len(list) == 0 {
			continue
		}
		data, _ := json.Marshal(list)
		p.tags = append(p.tags, tagRow{Rel: rel, Seq: int64(len(p.tags) + 1), Tags: string(data)})
	}
	p.report.add("image_tags", len(tags), len(p.tags), "")
	return nil
}

// readTaskFile reads image_tasks.json / editable_file_tasks.json ({"tasks": [...]} or a list).
func (p *plan) readTaskFile(file string, add func(key, owner string, task map[string]any, data string)) error {
	name := strings.TrimSuffix(file, ".json")
	value, found, err := readJSON(p.path(file))
	if err != nil {
		return err
	}
	if !found {
		p.report.add(name, 0, 0, file+" 不存在")
		return nil
	}
	list, err := listShape(file, value, "tasks")
	if err != nil {
		return err
	}
	seen, written := map[string]bool{}, 0
	for _, raw := range list {
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
		add(owner+":"+id, owner, task, string(data))
		written++
	}
	p.report.add(name, len(list), written, "")
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

func (p *plan) readCounts() error {
	value, found, err := readJSON(p.path("image_output_counts.json"))
	if err != nil {
		return err
	}
	days := map[string][2]int64{}
	if found {
		root, _ := value.(map[string]any)
		entries, ok := root["days"].(map[string]any)
		if !ok {
			return &SourceError{"image_output_counts.json", "格式不对，应为 {\"days\": {日期: {edits, generations}}}"}
		}
		for day, raw := range entries {
			kinds, ok := raw.(map[string]any)
			if _, err := time.Parse("2006-01-02", day); !ok || err != nil {
				continue // image_output_counts skips these too
			}
			days[day] = [2]int64{number(kinds["edits"]), number(kinds["generations"])}
		}
		p.countSource = "image_output_counts.json"
	} else {
		// like _scan_daily_counts: rebuild the daily totals from every log line
		if _, err := eachLogLine(p.logFile, func(_ int, raw string) error {
			day, index, n, ok := successImages(raw, p.opts.Location)
			if ok {
				counts := days[day]
				counts[index] += n
				days[day] = counts
			}
			return nil
		}); err != nil {
			return err
		}
		p.countSource = "logs.jsonl（重新统计）"
	}
	keys := make([]string, 0, len(days))
	for day := range days {
		keys = append(keys, day)
	}
	sort.Strings(keys)
	for _, day := range keys {
		p.counts = append(p.counts, logs.DailyCount{Day: day, Edits: days[day][0], Generations: days[day][1]})
	}
	p.report.add("image_output_counts", len(days), len(p.counts), "来源："+p.countSource)
	return nil
}

var beijing = time.FixedZone("UTC+8", 8*3600)

// successImages is LogService._success_images for one raw line: day (UTC+8), column, count.
func successImages(raw string, location *time.Location) (string, int, int64, bool) {
	if !strings.Contains(raw, `"type":"call"`) || !strings.Contains(raw, "/v1/images/") {
		return "", 0, 0, false
	}
	var item map[string]any
	if json.Unmarshal([]byte(raw), &item) != nil || item["type"] != "call" {
		return "", 0, 0, false
	}
	detail, _ := item["detail"].(map[string]any)
	if detail == nil || detail["status"] != "success" {
		return "", 0, 0, false
	}
	index, ok := map[string]int{"/v1/images/edits": 0, "/v1/images/generations": 1}[str(detail["endpoint"])]
	urls, _ := detail["urls"].([]any)
	at := str(item["time"])
	if !ok || len(urls) == 0 || len(at) < 19 {
		return "", 0, 0, false
	}
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", at[:19], location)
	if err != nil {
		return "", 0, 0, false
	}
	return parsed.In(beijing).Format("2006-01-02"), index, int64(len(urls)), true
}

// read builds the plan; nothing is written.
func read(opts Options) (*plan, error) {
	p := &plan{opts: opts, report: &Report{}, logFile: filepath.Join(opts.DataDir, "logs.jsonl")}
	if info, err := os.Stat(opts.DataDir); err != nil || !info.IsDir() {
		return nil, &SourceError{opts.DataDir, "数据目录不存在"}
	}
	isList := func(file string) func(any) error {
		return func(value any) error {
			if _, ok := value.([]any); !ok {
				return &SourceError{file, "格式不对，应为一个列表"}
			}
			return nil
		}
	}
	lengthOf := func(value any) int {
		if list, ok := value.([]any); ok {
			return len(list)
		}
		return 1
	}
	steps := []func() error{
		p.readAccounts,
		p.readAuthKeys,
		func() error { return p.readDocument("proxies.json", "proxies", isList("proxies.json"), lengthOf) },
		func() error {
			return p.readDocument("cpa_config.json", "cpa_config.json", func(value any) error {
				if _, ok := value.([]any); ok {
					return nil
				}
				if m, ok := value.(map[string]any); ok && m["base_url"] != nil {
					return nil // the old single-pool file
				}
				return &SourceError{"cpa_config.json", "格式不对，应为连接列表"}
			}, lengthOf)
		},
		func() error {
			return p.readDocument("sub2api_config.json", "sub2api_config.json", isList("sub2api_config.json"), lengthOf)
		},
		func() error {
			return p.readDocument("backup_state.json", "backup_state.json", func(value any) error {
				if _, ok := value.(map[string]any); !ok {
					return &SourceError{"backup_state.json", "格式不对，应为一个对象"}
				}
				return nil
			}, lengthOf)
		},
		p.readImageIndex,
		p.readTags,
		func() error {
			return p.readTaskFile("image_tasks.json", func(key, owner string, task map[string]any, data string) {
				p.tasks = append(p.tasks, taskRow{Key: key, Owner: owner, UpdatedAt: str(task["updated_at"]), Body: data})
			})
		},
		func() error {
			return p.readTaskFile("editable_file_tasks.json", func(key, _ string, _ map[string]any, data string) {
				p.fileTasks = append(p.fileTasks, fileTaskRow{Key: key, Body: data})
			})
		},
		p.readCounts,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(p.logFile); err == nil {
		p.logFound = true
		handle, err := os.Open(p.logFile)
		if err != nil {
			return nil, err
		}
		handle.Close()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return p, nil
}

// ---- writing ----

func replace[T any](tx *gorm.DB, model T, rows []T) error {
	if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&model).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	return tx.CreateInBatches(rows, 200).Error
}

// LegacyLogID gives a log line without an id a stable id. The first occurrence of a line gets the id
// Python's log list shows for it (_legacy_id(raw_line, 0)); later identical lines, which Python's list
// shows under that same id, each get one of their own so they stay separate rows. A rerun gives the
// same ids.
func LegacyLogID(raw string, occurrence int) string {
	payload := "0:" + raw
	if occurrence > 0 {
		payload = fmt.Sprintf("legacy:%d:%s", occurrence, raw)
	}
	sum := sha1.Sum([]byte(payload))
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

// windowLogs walks the log lines Python's list would show (JSON objects) inside the window.
func windowLogs(file string, opts Options, fn func(entry logs.Entry) error) error {
	from := windowStart(opts)
	occurrences := map[string]int{}
	_, err := eachLogLine(file, func(index int, raw string) error {
		line := strings.TrimSpace(raw)
		if line == "" {
			return nil
		}
		value, err := decodeJSONStrict([]byte(line))
		item, _ := value.(map[string]any)
		if err != nil || item == nil {
			return nil // _parse_line (json.loads) drops it too
		}
		at := str(item["time"])
		if from != "" && (len(at) < 10 || at[:10] < from) {
			return nil
		}
		id := str(item["id"])
		if id == "" {
			id = LegacyLogID(line, occurrences[line])
			occurrences[line]++
		}
		detail := item["detail"]
		if detail == nil {
			detail = map[string]any{}
		}
		data, _ := json.Marshal(detail)
		return fn(logs.Entry{ID: id, Seq: int64(index), Time: at, Type: str(item["type"]), Summary: str(item["summary"]), Detail: string(data)})
	})
	return err
}

func (p *plan) write(tx *gorm.DB) error {
	steps := []func() error{
		func() error { return replace(tx, accountRow{}, p.accounts) },
		func() error {
			if p.cumulative == nil {
				return nil
			}
			if err := tx.Where("key = ?", "cumulative_total").Delete(&kvRow{}).Error; err != nil {
				return err
			}
			return tx.Create(&kvRow{Key: "cumulative_total", Value: strconv.Itoa(*p.cumulative)}).Error
		},
		func() error { return replace(tx, compat.AuthKey{}, p.authKeys) },
		func() error {
			// only the documents this migration owns; others (written by the Go version itself) stay
			for _, name := range []string{"proxies", "cpa_config.json", "sub2api_config.json", "backup_state.json"} {
				if err := tx.Where("name = ?", name).Delete(&documentRow{}).Error; err != nil {
					return err
				}
			}
			if len(p.documents) == 0 {
				return nil
			}
			return tx.Create(&p.documents).Error
		},
		func() error { return replace(tx, imageRow{}, p.images) },
		func() error { return replace(tx, tagRow{}, p.tags) },
		func() error { return replace(tx, taskRow{}, p.tasks) },
		func() error { return replace(tx, fileTaskRow{}, p.fileTasks) },
		func() error { return replace(tx, logs.DailyCount{}, p.counts) },
		func() error {
			if err := tx.Session(&gorm.Session{AllowGlobalUpdate: true}).Delete(&logs.Entry{}).Error; err != nil {
				return err
			}
			batch, written, seen := []logs.Entry{}, 0, map[string]bool{}
			flush := func() error {
				if len(batch) == 0 {
					return nil
				}
				err := tx.CreateInBatches(batch, 500).Error
				written += len(batch)
				batch = batch[:0]
				return err
			}
			duplicates := 0
			var maxSeq int64
			err := windowLogs(p.logFile, p.opts, func(entry logs.Entry) error {
				maxSeq = max(maxSeq, entry.Seq)
				if seen[entry.ID] {
					duplicates++ // the same explicit id twice: log_service.list would show both, the table keeps one
					return nil
				}
				seen[entry.ID] = true
				batch = append(batch, entry)
				if len(batch) >= 2000 {
					return flush()
				}
				return nil
			})
			if err != nil {
				return err
			}
			if err := flush(); err != nil {
				return err
			}
			note := ""
			if !p.logFound {
				note = "logs.jsonl 不存在"
			} else if duplicates > 0 {
				note = fmt.Sprintf("%d 行与前面的行 id 相同，只保留第一行", duplicates)
			}
			p.report.add("logs", written+duplicates, written, note)
			p.maxLogSeq = maxSeq
			return nil
		},
		func() error { return p.writeInfo(tx) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// ErrVerification is returned when the tables do not match the sources; nothing is kept.
var ErrVerification = errors.New("对账不一致，已回滚，目标库未改动")

// Validate reads and validates every source file like Run, without touching any database (dry run).
func Validate(opts Options) (*Report, error) {
	p, err := read(opts.withDefaults())
	if err != nil {
		return nil, err
	}
	return p.report, nil
}

// Run migrates everything under opts.DataDir in one transaction, verifies it against the sources
// and commits only when every check passes.
func Run(db *gorm.DB, opts Options) (*Report, error) {
	opts = opts.withDefaults()
	p, err := read(opts)
	if err != nil {
		return nil, err
	}
	for _, migrate := range []func(*gorm.DB) error{compat.Migrate, logs.Migrate, pool.Migrate, docstore.Migrate, imagestore.Migrate,
		imagetasks.Migrate, filetasks.Migrate} {
		if err := migrate(db); err != nil {
			return nil, err
		}
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := p.write(tx); err != nil {
			return err
		}
		if opts.afterWrite != nil {
			if err := opts.afterWrite(tx); err != nil {
				return err
			}
		}
		checks, err := Verify(tx, opts)
		if err != nil {
			return err
		}
		p.report.Checks = checks
		if p.report.Failed() {
			return ErrVerification
		}
		return nil
	})
	return p.report, err
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
