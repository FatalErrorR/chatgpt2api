package migrate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/logs"
)

// orderedItems is a JSON object that keeps insertion order (Python dicts do).
type orderedItems struct {
	keys   []string
	values map[string]any
}

func newOrderedItems() *orderedItems { return &orderedItems{values: map[string]any{}} }

func (o *orderedItems) set(key string, value any) {
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
}

func (o *orderedItems) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		name, _ := json.Marshal(key)
		buf.Write(name)
		buf.WriteByte(':')
		var value bytes.Buffer
		encoder := json.NewEncoder(&value)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(o.values[key]); err != nil {
			return nil, err
		}
		buf.Write(bytes.TrimRight(value.Bytes(), "\n"))
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// appendLogs copies Python's logs.jsonl to dst byte for byte, then appends the new lines.
func appendLogs(src, dst string, entries []logs.Entry) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	writer := bufio.NewWriter(out)
	in, err := os.Open(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	default:
		_, err = io.Copy(writer, in)
		in.Close()
		if err != nil {
			return err
		}
		if info, statErr := os.Stat(src); statErr == nil && info.Size() > 0 {
			last := make([]byte, 1)
			if handle, err := os.Open(src); err == nil {
				handle.ReadAt(last, info.Size()-1)
				handle.Close()
			}
			if last[0] != '\n' {
				writer.WriteByte('\n')
			}
		}
	}
	for _, entry := range entries {
		writer.WriteString(logLine(entry))
		writer.WriteByte('\n')
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	return out.Sync()
}

// verifyRollback reads the written files back and checks them against Python's files and the Go
// database, independently of how they were written.
func verifyRollback(opts RollbackOptions, py *plan, g *goState, r *mergeResult) ([]Check, error) {
	c := &checker{}
	out := func(name string) string { return filepath.Join(opts.Out, name) }

	// accounts: every Go account as Go has it; every other Python account untouched
	value, _, err := readJSON(out("accounts.json"))
	if err != nil {
		return nil, err
	}
	list, _ := value.([]any)
	written := map[string]any{}
	for _, raw := range list {
		if account, ok := raw.(map[string]any); ok {
			written[accountToken(account)] = normalizeJSON(account)
		}
	}
	bad := []string{}
	for token, data := range r.goByToken {
		if !reflect.DeepEqual(written[token], normalizeJSON(decodeJSON(data))) {
			bad = append(bad, "go:"+token[:min(8, len(token))])
		}
	}
	goTokens := 0
	for _, row := range py.accounts {
		if _, isGo := r.goByToken[row.Token]; isGo {
			goTokens++
			continue
		}
		if got, ok := written[row.Token]; ok && !reflect.DeepEqual(got, normalizeJSON(decodeJSON(row.Data))) {
			bad = append(bad, "py:"+row.Token[:min(8, len(row.Token))])
		}
	}
	c.add("accounts（Go 的账号与 Go 一致，其他与 Python 一致）", len(r.accounts), len(list), sample(bad), len(bad) == 0)

	// logs: Python's bytes first, then each new line once
	pySum, pySize := "", int64(0)
	if _, err := os.Stat(py.logFile); err == nil {
		pySum, pySize, err = fileSHA(py.logFile)
		if err != nil {
			return nil, err
		}
	}
	prefixOK := true
	if pySize > 0 {
		handle, err := os.Open(out("logs.jsonl"))
		if err != nil {
			return nil, err
		}
		sum := sha256New()
		io.CopyN(sum, handle, pySize)
		handle.Close()
		prefixOK = fmt.Sprintf("%x", sum.Sum(nil)) == pySum
	}
	want := map[string]bool{}
	for _, entry := range r.newLogs {
		want[entry.ID] = true
	}
	seen := map[string]int{}
	eachLogLine(out("logs.jsonl"), func(_ int, raw string) error {
		if item, ok := mustObject(raw); ok && want[str(item["id"])] {
			seen[str(item["id"])]++
		}
		return nil
	})
	missingLogs := []string{}
	for id := range want {
		if seen[id] != 1 {
			missingLogs = append(missingLogs, id)
		}
	}
	detail := sample(missingLogs)
	if !prefixOK {
		detail = "Python 原有的日志内容被改动；" + detail
	}
	c.add("logs（原文件不变，新行各一次）", len(want), len(seen), detail, prefixOK && len(missingLogs) == 0)

	// counts
	value, _, err = readJSON(out("image_output_counts.json"))
	if err != nil {
		return nil, err
	}
	root, _ := value.(map[string]any)
	daysOut, _ := root["days"].(map[string]any)
	got := map[countKey]int64{}
	for day, raw := range daysOut {
		kinds, _ := raw.(map[string]any)
		for _, column := range []string{"edits", "generations"} {
			if n := number(kinds[column]); n != 0 {
				got[countKey{day, column}] = n
			}
		}
	}
	wantCounts := map[countKey]int64{}
	for key, n := range r.counts {
		if n != 0 {
			wantCounts[key] = n
		}
	}
	c.add("image_output_counts（Python 的 + Go 期间新增）", len(wantCounts), len(got), "", reflect.DeepEqual(wantCounts, got))

	// images: Python's entries as they were, new entries with their files
	value, _, err = readJSON(out("image_index.json"))
	if err != nil {
		return nil, err
	}
	root, _ = value.(map[string]any)
	items, _ := root["items"].(map[string]any)
	badImages := []string{}
	for _, row := range r.pyIndex {
		if !reflect.DeepEqual(normalizeJSON(items[row.Rel]), normalizeJSON(decodeJSON(row.Item))) {
			badImages = append(badImages, row.Rel)
		}
	}
	for _, row := range r.newImages {
		if !reflect.DeepEqual(normalizeJSON(items[row.Rel]), normalizeJSON(decodeJSON(row.Item))) {
			badImages = append(badImages, row.Rel)
			continue
		}
		item := decodeObject(row.Item)
		if local, _ := item["local"].(bool); local || item["storage"] == "local" || item["storage"] == "both" {
			a, _, errA := fileSHA(filepath.Join(opts.GoData, "images", row.Rel))
			b, _, errB := fileSHA(out(filepath.Join("images", row.Rel)))
			if errA != nil || errB != nil || a != b {
				badImages = append(badImages, row.Rel)
			}
		}
	}
	c.add("images（索引与文件）", len(r.pyIndex)+len(r.newImages), len(items), sample(badImages), len(badImages) == 0)

	// tasks
	for _, spec := range []struct {
		file string
		want int
	}{{"image_tasks.json", len(py.tasks) + r.tasks}, {"editable_file_tasks.json", len(py.fileTasks) + r.fileTasks}} {
		value, _, err := readJSON(out(spec.file))
		if err != nil {
			return nil, err
		}
		list, _ := listShape(spec.file, value, "tasks")
		c.add(spec.file+"（任务数）", spec.want, len(list), "", true)
	}
	return c.checks, nil
}

func mustObject(raw string) (map[string]any, bool) {
	value, err := decodeJSONStrict([]byte(strings.TrimSpace(raw)))
	item, ok := value.(map[string]any)
	return item, err == nil && ok
}
