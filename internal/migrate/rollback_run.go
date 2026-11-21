package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"time"

	"gorm.io/gorm"

	"github.com/basketikun/chatgpt2api/internal/logs"
)

// mergeResult is what the rollback writes, kept for the verification.
type mergeResult struct {
	accounts  []map[string]any
	goByToken map[string]string // Go's account JSON, by token
	counts    map[countKey]int64
	newLogs   []logs.Entry
	newImages []imageRow
	pyIndex   []imageRow
	tasks     int
	fileTasks int
}

// Rollback writes the merged files to opts.Out. On any error, or a failed check, Out is removed.
func Rollback(db *gorm.DB, opts RollbackOptions) (report *Report, err error) {
	report = &Report{}
	if opts.Out == "" || opts.Snapshot == "" || opts.Python == "" || opts.GoData == "" {
		return report, errors.New("需要 -snapshot、-python、-go-data、-out")
	}
	if _, statErr := os.Stat(opts.Out); statErr == nil {
		return report, fmt.Errorf("输出目录 %s 已存在，请换一个新目录", opts.Out)
	}
	info, found, err := LoadInfo(db)
	if err != nil {
		return report, err
	}
	if !found {
		return report, errors.New("库里没有迁移记录（migration_info），不是由迁移工具建立的库")
	}
	sums, err := FileChecksums(opts.Snapshot)
	if err != nil {
		return report, err
	}
	if !reflect.DeepEqual(sums, info.Files) {
		return report, fmt.Errorf("快照 %s 与迁移时记录的校验和不一致，迁移后被改动过", opts.Snapshot)
	}
	migratedAt, _ := time.Parse(time.RFC3339, info.Now)
	snap, err := read(Options{DataDir: opts.Snapshot, LogDays: info.LogDays, Now: migratedAt}.withDefaults())
	if err != nil {
		return report, fmt.Errorf("快照：%w", err)
	}
	py, err := read(Options{DataDir: opts.Python}.withDefaults())
	if err != nil {
		return report, fmt.Errorf("Python 现在的数据：%w", err)
	}
	g, err := loadGo(db, info)
	if err != nil {
		return report, err
	}

	// admin data must not have changed while both ran
	adminChanges := adminDiff(snap, g)
	for _, change := range adminChanges {
		report.add("管理数据变更（未合并）", 1, 0, change)
	}
	if len(adminChanges) > 0 && !opts.AllowAdminChanges {
		return report, fmt.Errorf("并行期间 Go 上有 %d 处管理数据变更（见上），需要先手工处理；确认不需要合并后加 -allow-admin-changes", len(adminChanges))
	}

	if err := os.MkdirAll(opts.Out, 0o755); err != nil {
		return report, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(opts.Out)
		}
	}()
	result, err := writeMerged(opts, snap, py, g, report)
	if err != nil {
		return report, err
	}
	report.Checks, err = verifyRollback(opts, py, g, result)
	if err != nil {
		return report, err
	}
	if report.Failed() {
		return report, ErrVerification
	}
	return report, nil
}

func adminDiff(snap *plan, g *goState) []string {
	var out []string
	type key struct {
		ID, Name, Role, Hash string
		Enabled              bool
	}
	set := func(rows []key) map[key]bool {
		m := map[key]bool{}
		for _, row := range rows {
			m[row] = true
		}
		return m
	}
	var before, after []key
	for _, k := range snap.authKeys {
		before = append(before, key{k.ID, k.Name, k.Role, k.KeyHash, k.Enabled})
	}
	for _, k := range g.authKeys {
		after = append(after, key{k.ID, k.Name, k.Role, k.KeyHash, k.Enabled})
	}
	if !reflect.DeepEqual(set(before), set(after)) {
		out = append(out, fmt.Sprintf("API key：迁移时 %d 个，现在 %d 个或内容不同", len(before), len(after)))
	}
	snapDocs := map[string]string{}
	for _, doc := range snap.documents {
		snapDocs[doc.Name] = doc.Data
	}
	for _, name := range []string{"proxies", "cpa_config.json", "sub2api_config.json", "backup_state.json"} {
		if !reflect.DeepEqual(normalizeJSON(decodeJSON(snapDocs[name])), normalizeJSON(decodeJSON(g.documents[name]))) {
			out = append(out, name+" 与迁移时不同")
		}
	}
	return out
}

func writeMerged(opts RollbackOptions, snap, py *plan, g *goState, report *Report) (*mergeResult, error) {
	r := &mergeResult{goByToken: map[string]string{}}
	out := func(name string) string { return filepath.Join(opts.Out, name) }

	// accounts
	snapByToken := map[string]string{}
	for _, row := range snap.accounts {
		snapByToken[row.Token] = row.Data
	}
	accounts := make([]map[string]any, len(py.accounts))
	byToken, byEmail, byUser := map[string]int{}, map[string][]int{}, map[string][]int{}
	for i, row := range py.accounts {
		accounts[i] = decodeObject(row.Data)
		byToken[row.Token] = i
		if email := str(accounts[i]["email"]); email != "" {
			byEmail[email] = append(byEmail[email], i)
		}
		if user := str(accounts[i]["user_id"]); user != "" {
			byUser[user] = append(byUser[user], i)
		}
	}
	replaced, added, conflicts := 0, 0, 0
	matched := map[int]bool{}
	for _, row := range g.accounts {
		r.goByToken[row.Token] = row.Data
		account := decodeObject(row.Data)
		idx, ok := byToken[row.Token]
		if !ok {
			for _, candidates := range [][]int{byEmail[str(account["email"])], byUser[str(account["user_id"])]} {
				if len(candidates) == 1 { // unique email, then unique user_id (empty ones are not indexed)
					idx, ok = candidates[0], true
					break
				}
			}
		}
		if ok && !matched[idx] {
			matched[idx] = true
			if before, inSnap := snapByToken[py.accounts[idx].Token]; inSnap && before != py.accounts[idx].Data {
				conflicts++ // Python changed it too: the account was not only Go's
			}
			accounts[idx] = account
			replaced++
		} else {
			accounts = append(accounts, account)
			added++
		}
	}
	removedOnGo := 0
	for _, row := range snap.accounts {
		if _, ok := r.goByToken[row.Token]; !ok {
			removedOnGo++
		}
	}
	note := ""
	if removedOnGo > 0 {
		note = fmt.Sprintf("迁移时有、Go 现在没有 %d 个（可能已轮换 token 或被移除，未从 Python 删除）", removedOnGo)
	}
	if conflicts > 0 {
		note += fmt.Sprintf("；%d 个账号两边都改过，以 Go 为准", conflicts)
	}
	report.add("accounts（替换/新增）", replaced, replaced+added, note)
	r.accounts = accounts
	if err := writeJSONFile(out("accounts.json"), accounts, true); err != nil {
		return nil, err
	}
	pyCumul := len(py.accounts)
	if py.cumulative != nil {
		pyCumul = *py.cumulative
	}
	if g.hasCumul && snap.cumulative != nil {
		pyCumul += g.cumulative - *snap.cumulative
	}
	if err := os.WriteFile(out(".cumulative_total"), []byte(strconv.Itoa(pyCumul)), 0o644); err != nil {
		return nil, err
	}

	// logs: Python's file, then the lines Go wrote
	ids, err := pythonLogIDs(py.logFile)
	if err != nil {
		return nil, err
	}
	for _, entry := range g.newLogs {
		if !ids[entry.ID] {
			r.newLogs = append(r.newLogs, entry)
		}
	}
	if err := appendLogs(py.logFile, out("logs.jsonl"), r.newLogs); err != nil {
		return nil, err
	}
	report.add("logs（追加 Go 期间的行）", len(g.newLogs), len(r.newLogs), "")

	// daily counts: Python's plus what Go added since the snapshot
	r.counts = countsOf(py.counts)
	snapCounts := countsOf(snap.counts)
	for key, n := range g.counts {
		if delta := n - snapCounts[key]; delta != 0 {
			r.counts[key] += delta
		}
	}
	days := map[string]map[string]int64{}
	for key, n := range r.counts {
		if days[key.day] == nil {
			days[key.day] = map[string]int64{"edits": 0, "generations": 0}
		}
		days[key.day][key.column] += n
	}
	if err := writeJSONFile(out("image_output_counts.json"), map[string]any{"days": days}, false); err != nil {
		return nil, err
	}

	// images, thumbnails, tags
	snapRels, pyRels := map[string]bool{}, map[string]bool{}
	for _, row := range snap.images {
		snapRels[row.Rel] = true
	}
	for _, row := range py.images {
		pyRels[row.Rel] = true
	}
	r.pyIndex = py.images
	index := newOrderedItems()
	for _, row := range py.images {
		index.set(row.Rel, decodeObject(row.Item))
	}
	missing := 0
	for _, row := range g.images {
		if snapRels[row.Rel] || pyRels[row.Rel] {
			continue
		}
		item := decodeObject(row.Item)
		if local, _ := item["local"].(bool); local || item["storage"] == "local" || item["storage"] == "both" {
			if _, err := copyFile(filepath.Join(opts.GoData, "images", row.Rel), out(filepath.Join("images", row.Rel))); err != nil {
				missing++
				continue
			}
			thumb := filepath.Join("image_thumbnails", row.Rel+".png")
			copyFile(filepath.Join(opts.GoData, thumb), out(thumb))
		}
		index.set(row.Rel, item)
		r.newImages = append(r.newImages, row)
	}
	note = ""
	if missing > 0 {
		note = fmt.Sprintf("%d 张在 Go 的图片目录里已不存在，未加入索引", missing)
	}
	report.add("images（Go 期间新增）", len(r.newImages)+missing, len(r.newImages), note)
	if err := writeJSONFile(out("image_index.json"), map[string]any{"items": index}, true); err != nil {
		return nil, err
	}
	tags := newOrderedItems()
	for _, row := range py.tags {
		tags.set(row.Rel, decodeJSON(row.Tags))
	}
	for _, row := range r.newImages {
		if raw, ok := g.tags[row.Rel]; ok {
			tags.set(row.Rel, decodeJSON(raw))
		}
	}
	if err := writeJSONFile(out("image_tags.json"), tags, true); err != nil {
		return nil, err
	}

	// tasks
	snapTasks, pyTasks := map[string]bool{}, map[string]bool{}
	for _, row := range snap.tasks {
		snapTasks[row.Key] = true
	}
	var taskList []any
	for _, row := range py.tasks {
		pyTasks[row.Key] = true
		taskList = append(taskList, decodeObject(row.Body))
	}
	for _, row := range g.tasks {
		if !snapTasks[row.Key] && !pyTasks[row.Key] {
			taskList = append(taskList, decodeObject(row.Body))
			r.tasks++
		}
	}
	if err := writeJSONFile(out("image_tasks.json"), map[string]any{"tasks": nonNil(taskList)}, true); err != nil {
		return nil, err
	}
	snapFiles, pyFiles := map[string]bool{}, map[string]bool{}
	for _, row := range snap.fileTasks {
		snapFiles[row.Key] = true
	}
	var fileList []any
	for _, row := range py.fileTasks {
		pyFiles[row.Key] = true
		fileList = append(fileList, decodeObject(row.Body))
	}
	for _, row := range g.fileTasks {
		if snapFiles[row.Key] || pyFiles[row.Key] {
			continue
		}
		task := decodeObject(row.Body)
		fileList = append(fileList, task)
		r.fileTasks++
		if id := str(task["id"]); id != "" {
			matches, _ := filepath.Glob(filepath.Join(opts.GoData, "files", "*", id))
			for _, dir := range matches {
				rel, _ := filepath.Rel(opts.GoData, dir)
				if err := copyDir(dir, out(rel)); err != nil {
					return nil, err
				}
			}
		}
	}
	if err := writeJSONFile(out("editable_file_tasks.json"), map[string]any{"tasks": nonNil(fileList)}, true); err != nil {
		return nil, err
	}
	report.add("tasks（生图/可编辑文件，Go 期间新增）", r.tasks+r.fileTasks, r.tasks+r.fileTasks, "")
	return r, nil
}

func nonNil(list []any) []any {
	if list == nil {
		return []any{}
	}
	return list
}
