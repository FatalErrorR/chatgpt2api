package migrate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/basketikun/chatgpt2api/internal/compat"
	"github.com/basketikun/chatgpt2api/internal/imagestore"
	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/pool"
)

// Verification compares the tables with what the source files say should be there. It reads the
// files again and works out the expected rows on its own, so a bug in the write path (a dropped
// account, merged log lines) shows up as a difference instead of agreeing with itself.

// normalizeJSON round-trips a value so numbers and maps compare equal.
func normalizeJSON(value any) any {
	raw, _ := json.Marshal(value)
	var out any
	json.Unmarshal(raw, &out)
	return out
}

func decodeJSON(raw string) any {
	var out any
	json.Unmarshal([]byte(raw), &out)
	return out
}

type checker struct {
	checks []Check
}

func (c *checker) add(name string, expected, actual int, detail string, ok bool) {
	c.checks = append(c.checks, Check{Name: name, Expected: expected, Actual: actual, Detail: detail, OK: ok && expected == actual})
}

func sample(items []string) string {
	if len(items) == 0 {
		return ""
	}
	sort.Strings(items)
	if len(items) > 5 {
		return fmt.Sprintf("%v 等 %d 个", items[:5], len(items))
	}
	return fmt.Sprint(items)
}

// Verify checks the database against the source files (acceptance D1 and D2).
func Verify(db *gorm.DB, opts Options) ([]Check, error) {
	opts = opts.withDefaults()
	c := &checker{}
	path := func(name string) string { return filepath.Join(opts.DataDir, name) }

	// accounts: every account the Python pool would load, in its order, field by field
	if value, found, err := readJSON(path("accounts.json")); err != nil {
		return nil, err
	} else if found {
		list, _ := value.([]any)
		expected, order := map[string]any{}, []string{}
		for _, raw := range list {
			account, ok := raw.(map[string]any)
			if !ok || accountToken(account) == "" {
				continue
			}
			token := accountToken(account)
			copied := map[string]any{}
			for key, value := range account {
				if key != "accessToken" {
					copied[key] = value
				}
			}
			copied["access_token"] = token
			if _, seen := expected[token]; !seen {
				order = append(order, token)
			}
			expected[token] = normalizeJSON(copied)
		}
		var rows []accountRow
		if err := db.Order("position").Find(&rows).Error; err != nil {
			return nil, err
		}
		mismatched := []string{}
		for i, token := range order {
			if i >= len(rows) || rows[i].Token != token || !reflect.DeepEqual(expected[token], decodeJSON(rows[i].Data)) {
				mismatched = append(mismatched, pool.AnonymizeToken(token))
			}
		}
		detail := ""
		if len(mismatched) > 0 {
			detail = "缺失、顺序或字段不一致：" + sample(mismatched)
		}
		c.add("accounts（逐个字段与顺序）", len(order), len(rows), detail, len(mismatched) == 0)
	}

	// cumulative account total
	if raw, err := readFile(path(".cumulative_total")); err == nil {
		want, _ := strconv.Atoi(raw)
		got, _ := pool.NewDBStore(db).LoadInt("cumulative_total")
		c.add(".cumulative_total", want, got, "", true)
	}

	// API keys: the same hashes, ids and enabled flags
	if value, found, err := readJSON(path("auth_keys.json")); err != nil {
		return nil, err
	} else if found {
		list, _ := listShape("auth_keys.json", value, "items")
		expected := map[string]bool{}
		for _, raw := range list {
			item, _ := raw.(map[string]any)
			role, hash := str(item["role"]), str(item["key_hash"])
			if (role == "admin" || role == "user") && hash != "" {
				enabled := true
				if flag, ok := item["enabled"].(bool); ok {
					enabled = flag
				}
				expected[hash] = enabled
			}
		}
		var rows []compat.AuthKey
		db.Find(&rows)
		mismatched := 0
		for _, row := range rows {
			if want, ok := expected[row.KeyHash]; !ok || want != row.Enabled {
				mismatched++
			}
		}
		c.add("auth_keys（哈希与启用状态）", len(expected), len(rows), "", mismatched == 0)
	}

	// whole documents
	for name, file := range map[string]string{"proxies": "proxies.json", "cpa_config.json": "cpa_config.json", "sub2api_config.json": "sub2api_config.json", "backup_state.json": "backup_state.json"} {
		value, found, err := readJSON(path(file))
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		var rows []documentRow
		db.Where("name = ?", name).Find(&rows)
		same := len(rows) == 1 && reflect.DeepEqual(normalizeJSON(value), decodeJSON(rows[0].Data))
		c.add(file+"（内容一致）", 1, len(rows), "", same)
	}

	// image index: every entry Python's clean index keeps, with the same contents
	if value, found, err := readJSON(path("image_index.json")); err != nil {
		return nil, err
	} else if found {
		root, _ := value.(map[string]any)
		items, _ := root["items"].(map[string]any)
		expected := map[string]any{}
		for rel, item := range items {
			if _, ok := item.(map[string]any); ok && isImageRel(rel) && cleanRel(rel) {
				expected[rel] = normalizeJSON(item)
			}
		}
		var rows []imageRow
		db.Find(&rows)
		mismatched := []string{}
		for _, row := range rows {
			if want, ok := expected[row.Rel]; !ok || !reflect.DeepEqual(want, decodeJSON(row.Item)) {
				mismatched = append(mismatched, row.Rel)
			}
		}
		c.add("image_index（条目与内容）", len(expected), len(rows), sample(mismatched), len(mismatched) == 0)
	}

	// tags
	if value, found, err := readJSON(path("image_tags.json")); err != nil {
		return nil, err
	} else if found {
		tags, _ := value.(map[string]any)
		expected := map[string]any{}
		for rel, list := range tags {
			if items, ok := list.([]any); ok && len(items) > 0 {
				expected[rel] = normalizeJSON(items)
			}
		}
		var rows []tagRow
		db.Find(&rows)
		actual := map[string]any{}
		for _, row := range rows {
			actual[row.Rel] = decodeJSON(row.Tags)
		}
		c.add("image_tags（内容一致）", len(expected), len(rows), "", reflect.DeepEqual(expected, actual))
	}

	// tasks
	for _, spec := range []struct {
		file  string
		table any
	}{{"image_tasks.json", &[]taskRow{}}, {"editable_file_tasks.json", &[]fileTaskRow{}}} {
		value, found, err := readJSON(path(spec.file))
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		list, _ := listShape(spec.file, value, "tasks")
		expected := map[string]bool{}
		for _, raw := range list {
			task, _ := raw.(map[string]any)
			if id, owner := str(task["id"]), str(task["owner_id"]); id != "" && owner != "" {
				expected[owner+":"+id] = true
			}
		}
		var n int64
		db.Model(spec.table).Count(&n)
		c.add(spec.file+"（任务数）", len(expected), int(n), "", true)
	}

	// logs: every line in the window, by id and content
	expected := map[string][3]string{}
	duplicateIDs := 0
	if err := windowLogs(path("logs.jsonl"), opts, func(entry logs.Entry) error {
		if _, seen := expected[entry.ID]; seen {
			duplicateIDs++
			return nil
		}
		expected[entry.ID] = [3]string{entry.Time, entry.Type, entry.Summary}
		return nil
	}); err != nil {
		return nil, err
	}
	var entries []logs.Entry
	if err := db.Select("id", "time", "type", "summary").Find(&entries).Error; err != nil {
		return nil, err
	}
	missing := []string{}
	actual := map[string]bool{}
	for _, entry := range entries {
		actual[entry.ID] = true
		if want, ok := expected[entry.ID]; !ok || want != [3]string{entry.Time, entry.Type, entry.Summary} {
			missing = append(missing, entry.ID)
		}
	}
	for id := range expected {
		if !actual[id] {
			missing = append(missing, id)
		}
	}
	detail := "从 " + windowStart(opts) + " 起"
	if duplicateIDs > 0 {
		detail += fmt.Sprintf("；源文件有 %d 行与前面的行 id 相同", duplicateIDs)
	}
	if len(missing) > 0 {
		detail += "；不一致：" + sample(missing)
	}
	c.add("logs（窗口内每一行）", len(expected), len(entries), detail, len(missing) == 0)

	// daily counts
	if value, found, err := readJSON(path("image_output_counts.json")); err != nil {
		return nil, err
	} else {
		want := map[string][2]int64{}
		if found {
			root, _ := value.(map[string]any)
			days, _ := root["days"].(map[string]any)
			for day, raw := range days {
				kinds, ok := raw.(map[string]any)
				if !ok || len(day) != 10 {
					continue
				}
				if _, err := parseDay(day); err != nil {
					continue
				}
				want[day] = [2]int64{number(kinds["edits"]), number(kinds["generations"])}
			}
		} else {
			eachLogLine(path("logs.jsonl"), func(_ int, raw string) error {
				if day, index, n, ok := successImages(raw, opts.Location); ok {
					counts := want[day]
					counts[index] += n
					want[day] = counts
				}
				return nil
			})
		}
		var rows []logs.DailyCount
		db.Find(&rows)
		got := map[string][2]int64{}
		for _, row := range rows {
			got[row.Day] = [2]int64{row.Edits, row.Generations}
		}
		c.add("image_output_counts（每天的数）", len(want), len(rows), "", reflect.DeepEqual(want, got))
	}
	return c.checks, nil
}

func readFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func parseDay(day string) (time.Time, error) { return time.Parse("2006-01-02", day) }

// cleanRel is SafeRel(rel) == rel: the index keys _load_clean_index keeps.
func cleanRel(rel string) bool {
	safe, err := imagestore.SafeRel(rel)
	return err == nil && safe == rel
}
