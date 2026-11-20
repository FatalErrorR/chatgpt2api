package migrate

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"

	"gorm.io/gorm"

	"github.com/basketikun/chatgpt2api/internal/compat"
	"github.com/basketikun/chatgpt2api/internal/docstore"
	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/pool"
)

// Check is one reconciliation result.
type Check struct {
	Name     string
	Expected int
	Actual   int
	Detail   string
	OK       bool
}

func count(db *gorm.DB, model any) int {
	var n int64
	db.Model(model).Count(&n)
	return int(n)
}

// normalizeJSON round-trips a value so numbers and maps compare equal.
func normalizeJSON(value any) any {
	raw, _ := json.Marshal(value)
	var out any
	json.Unmarshal(raw, &out)
	return out
}

// Verify re-reads the database and checks it against the source files and the run's report:
// row counts for every table (D1) and every account field by field (D2).
func Verify(db *gorm.DB, opts Options, report *Report) []Check {
	opts = opts.withDefaults()
	written := map[string]int{}
	for _, item := range report.Items {
		written[item.Name] = item.Written
	}
	checks := []Check{}
	add := func(name string, expected, actual int, detail string) {
		checks = append(checks, Check{Name: name, Expected: expected, Actual: actual, Detail: detail, OK: expected == actual})
	}
	add("accounts（表行数）", written["accounts"], count(db, &accountRow{}), "")
	add("auth_keys（表行数）", written["auth_keys"], count(db, &compat.AuthKey{}), "")
	add("image_index（表行数）", written["image_index"], count(db, &imageRow{}), "")
	add("image_tags（表行数）", written["image_tags"], count(db, &tagRow{}), "")
	add("image_tasks（表行数）", written["image_tasks"], count(db, &taskRow{}), "")
	add("editable_file_tasks（表行数）", written["editable_file_tasks"], count(db, &fileTaskRow{}), "")
	add("image_output_counts（天数）", written["image_output_counts"], count(db, &logs.DailyCount{}), "")
	if from := windowStart(opts); from != "" {
		var n int64
		db.Model(&logs.Entry{}).Where("time >= ?", from).Count(&n)
		add("logs（窗口内行数）", written["logs"], int(n), "从 "+from+" 起")
	}

	// every account as stored equals the source account
	value, found, err := readJSON(filepath.Join(opts.DataDir, "accounts.json"))
	if err == nil && found {
		stored, err := pool.NewDBStore(db).Load()
		byToken := map[string]pool.Account{}
		for _, account := range stored {
			byToken[str(account["access_token"])] = account
		}
		effective, order := map[string]map[string]any{}, []string{}
		for _, raw := range listOf(value, "items") {
			source, ok := raw.(map[string]any)
			if !ok || str(source["access_token"]) == "" {
				continue
			}
			token := str(source["access_token"])
			if _, seen := effective[token]; !seen {
				order = append(order, token)
			}
			effective[token] = source // the last entry wins, as in the Python dict
		}
		mismatched, compared := []string{}, 0
		for i, token := range order {
			source := effective[token]
			compared++
			if i < len(stored) && str(stored[i]["access_token"]) != token {
				mismatched = append(mismatched, pool.AnonymizeToken(token)+"（顺序）")
				continue
			}
			target, ok := byToken[token]
			if !ok || !reflect.DeepEqual(normalizeJSON(source), normalizeJSON(map[string]any(target))) {
				mismatched = append(mismatched, pool.AnonymizeToken(token))
			}
		}
		detail := ""
		if err != nil {
			detail = err.Error()
		} else if len(mismatched) > 0 {
			detail = fmt.Sprintf("不一致：%v", mismatched)
		}
		add("accounts（逐个字段一致）", compared, compared-len(mismatched), detail)
	}

	// documents equal their source files
	docs := docstore.New(db)
	for name, file := range map[string]string{"proxies": "proxies.json", "cpa_config.json": "cpa_config.json", "sub2api_config.json": "sub2api_config.json"} {
		source, found, err := readJSON(filepath.Join(opts.DataDir, file))
		if err != nil || !found {
			continue
		}
		var stored any
		docs.Load(name, &stored)
		same := 0
		if reflect.DeepEqual(normalizeJSON(source), normalizeJSON(stored)) {
			same = 1
		}
		add(file+"（内容一致）", 1, same, "")
	}
	return checks
}
