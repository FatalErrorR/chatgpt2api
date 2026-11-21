package migrate

// Rollback: back to the Python version after the Go version took writes (step 13, acceptance G2).
//
// Inputs, all read only: the Go database; the snapshot the migration read (checked against the
// checksums the migration recorded); Python's data directory as it is now; the Go instance's data
// directory (images, thumbnails, files). Output: a new directory holding the files to lay over
// Python's data directory (rsync -a OUT/ data/ while Python is stopped). Nothing else is written.
//
// What the Go period changed, relative to the snapshot, is merged into Python's current files:
//   - accounts: Go's current version replaces Python's (matched by token, then by email, then by
//     user_id, so rotated tokens are found); accounts added on Go are appended
//   - logs: lines Go wrote (seq above the migration's max) are appended to logs.jsonl, by id once
//   - daily image counts: Python's plus (Go now - snapshot)
//   - images: entries Go added, with their files and thumbnails; tags of those images
//   - image and editable-file tasks Go added, with the editable files
// Admin data (API keys, proxies, CPA, sub2api, backup state) must not change while both run; a
// change is reported and refuses the rollback unless AllowAdminChanges (it is never merged).
// Deletions on the Go side (removed accounts, cleaned images) are reported, not applied.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gorm.io/gorm"

	"github.com/basketikun/chatgpt2api/internal/compat"
	"github.com/basketikun/chatgpt2api/internal/logs"
)

type RollbackOptions struct {
	Snapshot          string // the data directory the migration read, unchanged since
	Python            string // Python's data directory now
	GoData            string // the Go instance's data directory
	Out               string // a new directory for the merged files
	AllowAdminChanges bool
}

// goState is everything in the Go database, in the migration's row shapes.
type goState struct {
	accounts   []accountRow
	cumulative int
	hasCumul   bool
	authKeys   []compat.AuthKey
	documents  map[string]string
	images     []imageRow
	tags       map[string]string
	tasks      []taskRow
	fileTasks  []fileTaskRow
	counts     map[countKey]int64
	newLogs    []logs.Entry
}

type countKey struct{ day, column string }

func loadGo(db *gorm.DB, info Info) (*goState, error) {
	g := &goState{documents: map[string]string{}, tags: map[string]string{}, counts: map[countKey]int64{}}
	steps := []func() error{
		func() error { return db.Order("position").Find(&g.accounts).Error },
		func() error {
			var rows []kvRow
			if err := db.Where("key = ?", "cumulative_total").Find(&rows).Error; err != nil || len(rows) == 0 {
				return err
			}
			n, err := strconv.Atoi(rows[0].Value)
			g.cumulative, g.hasCumul = n, err == nil
			return nil
		},
		func() error { return db.Order("position").Find(&g.authKeys).Error },
		func() error {
			var rows []documentRow
			if err := db.Find(&rows).Error; err != nil {
				return err
			}
			for _, row := range rows {
				g.documents[row.Name] = row.Data
			}
			return nil
		},
		func() error { return db.Order("seq").Find(&g.images).Error },
		func() error {
			var rows []tagRow
			if err := db.Find(&rows).Error; err != nil {
				return err
			}
			for _, row := range rows {
				g.tags[row.Rel] = row.Tags
			}
			return nil
		},
		func() error { return db.Find(&g.tasks).Error },
		func() error { return db.Find(&g.fileTasks).Error },
		func() error {
			var rows []logs.DailyCount
			if err := db.Find(&rows).Error; err != nil {
				return err
			}
			for _, row := range rows {
				g.counts[countKey{row.Day, "edits"}] += row.Edits
				g.counts[countKey{row.Day, "generations"}] += row.Generations
			}
			return nil
		},
		func() error { return db.Where("seq > ?", info.MaxLogSeq).Order("seq").Find(&g.newLogs).Error },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	return g, nil
}

func countsOf(rows []logs.DailyCount) map[countKey]int64 {
	out := map[countKey]int64{}
	for _, row := range rows {
		out[countKey{row.Day, "edits"}] += row.Edits
		out[countKey{row.Day, "generations"}] += row.Generations
	}
	return out
}

func decodeObject(raw string) map[string]any {
	var out map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	decoder.Decode(&out)
	return out
}

// writeJSONFile writes like Python's json.dumps(value, ensure_ascii=False, indent=2) + "\n".
func writeJSONFile(path string, value any, indent bool) error {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if indent {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(value); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func copyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return n, err
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		_, err = copyFile(path, filepath.Join(dst, rel))
		return err
	})
}

func fileSHA(path string) (string, int64, error) {
	handle, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer handle.Close()
	sum := sha256.New()
	n, err := io.Copy(sum, handle)
	return hex.EncodeToString(sum.Sum(nil)), n, err
}

// logLine is Python's _serialize_item: id, time, type, summary, detail; compact, unescaped.
func logLine(entry logs.Entry) string {
	detail := json.RawMessage(entry.Detail)
	if !json.Valid(detail) {
		detail = json.RawMessage("{}")
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.Encode(struct {
		ID      string          `json:"id"`
		Time    string          `json:"time"`
		Type    string          `json:"type"`
		Summary string          `json:"summary"`
		Detail  json.RawMessage `json:"detail"`
	}{entry.ID, entry.Time, entry.Type, entry.Summary, detail})
	return strings.TrimRight(buf.String(), "\n")
}

// pythonLogIDs are the explicit ids in a logs.jsonl.
func pythonLogIDs(path string) (map[string]bool, error) {
	ids := map[string]bool{}
	_, err := eachLogLine(path, func(_ int, raw string) error {
		if value, err := decodeJSONStrict([]byte(strings.TrimSpace(raw))); err == nil {
			if item, ok := value.(map[string]any); ok && str(item["id"]) != "" {
				ids[str(item["id"])] = true
			}
		}
		return nil
	})
	return ids, err
}

func sha256New() hash.Hash { return sha256.New() }
