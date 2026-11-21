package migrate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/basketikun/chatgpt2api/internal/logs"
)

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	if err := copyDir(src, dst); err != nil {
		t.Fatal(err)
	}
}

func readLines(t *testing.T, path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

// goPeriod writes into the database what the Go version would during the parallel run.
func goPeriod(t *testing.T, db *gorm.DB, goData string) {
	t.Helper()
	exec := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	// tok-b's token rotated (same email), tok-a used, an account added
	var rows []accountRow
	db.Order("position").Find(&rows)
	for _, row := range rows {
		account := decodeObject(row.Data)
		switch row.Token {
		case "tok-b":
			exec(db.Delete(&row).Error)
			account["access_token"], account["refresh_token"] = "tok-b-rotated", "rt-new"
			raw, _ := json.Marshal(account)
			exec(db.Create(&accountRow{Token: "tok-b-rotated", Position: 10, Data: string(raw)}).Error)
		case "tok-a":
			account["success"] = 7
			raw, _ := json.Marshal(account)
			exec(db.Model(&accountRow{}).Where("token = ?", "tok-a").Update("data", string(raw)).Error)
		}
	}
	exec(db.Create(&accountRow{Token: "tok-new", Position: 11, Data: `{"access_token":"tok-new","email":"new@x"}`}).Error)
	exec(db.Model(&kvRow{}).Where("key = ?", "cumulative_total").Update("value", "58").Error)
	// two log lines (Go seq is nanoseconds), one of them a successful image call
	now := time.Now().UnixNano()
	exec(db.Create(&logs.Entry{ID: "go-1", Seq: now, Time: "2026-11-20 13:00:00", Type: "call", Summary: "文生图调用完成 <ok>",
		Detail: `{"endpoint":"/v1/images/generations","status":"success","urls":["u1"]}`}).Error)
	exec(db.Create(&logs.Entry{ID: "go-2", Seq: now + 1, Time: "2026-11-20 13:01:00", Type: "account", Summary: "更新账号", Detail: `{}`}).Error)
	exec(db.Model(&logs.DailyCount{}).Where("day = ?", "2026-11-20").Update("generations", gorm.Expr("generations + 1")).Error)
	// a new image with its file, thumbnail and tags
	rel := "2026/11/20/new.png"
	exec(db.Create(&imageRow{Rel: rel, Seq: 99, Date: "2026-11-20", CreatedAt: "2026-11-20 13:00:00",
		Item: `{"date":"2026-11-20","storage":"local","local":true,"webdav":false}`}).Error)
	exec(db.Create(&tagRow{Rel: rel, Seq: 99, Tags: `["新"]`}).Error)
	for _, path := range []string{filepath.Join(goData, "images", rel), filepath.Join(goData, "image_thumbnails", rel+".png")} {
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte("png "+path), 0o644)
	}
	// a new image task
	exec(db.Create(&taskRow{Key: "k1:t9", Owner: "k1", UpdatedAt: "2026-11-20 13:00:00", Body: `{"id":"t9","owner_id":"k1","status":"success"}`}).Error)
}

func TestRollbackMergesTheGoPeriodIntoPython(t *testing.T) {
	snapshot := fixture(t)
	db := openDB(t)
	opts := options(snapshot)
	mustRun(t, db, opts)

	goData := t.TempDir()
	goPeriod(t, db, goData)

	// Python kept running on its own data meanwhile: a log line of its own
	python := t.TempDir()
	copyTree(t, snapshot, python)
	pyLog, _ := os.OpenFile(filepath.Join(python, "logs.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	pyLog.WriteString(`{"id":"py-1","time":"2026-11-20 13:30:00","type":"account","summary":"python","detail":{}}` + "\n")
	pyLog.Close()
	pyBefore, _, _ := fileSHA(filepath.Join(python, "logs.jsonl"))

	out := filepath.Join(t.TempDir(), "out")
	report, err := Rollback(db, RollbackOptions{Snapshot: snapshot, Python: python, GoData: goData, Out: out})
	if err != nil {
		t.Fatalf("rollback: %v %+v", err, report)
	}
	for _, check := range report.Checks {
		if !check.OK {
			t.Fatalf("check %+v", check)
		}
	}

	// accounts: rotated token in place of the old one, the new account appended
	var accounts []map[string]any
	raw, _ := os.ReadFile(filepath.Join(out, "accounts.json"))
	json.Unmarshal(raw, &accounts)
	tokens := []string{}
	for _, account := range accounts {
		tokens = append(tokens, accountToken(account))
	}
	if strings.Join(tokens, ",") != "tok-b-rotated,tok-a,tok-legacy,tok-new" || accounts[0]["refresh_token"] != "rt-new" || accounts[1]["success"] != float64(7) {
		t.Fatalf("accounts %v", accounts)
	}
	if cumul, _ := os.ReadFile(filepath.Join(out, ".cumulative_total")); string(cumul) != "58" {
		t.Fatalf("cumulative %s", cumul)
	}
	// logs: Python's file unchanged as a prefix, then Go's two lines; Python's file itself untouched
	lines := readLines(t, filepath.Join(out, "logs.jsonl"))
	if !strings.Contains(lines[len(lines)-3], `"py-1"`) || !strings.HasPrefix(lines[len(lines)-2], `{"id":"go-1","time":"2026-11-20 13:00:00","type":"call","summary":"文生图调用完成 <ok>","detail":{`) ||
		!strings.Contains(lines[len(lines)-1], `"go-2"`) {
		t.Fatalf("log tail %v", lines[len(lines)-3:])
	}
	if after, _, _ := fileSHA(filepath.Join(python, "logs.jsonl")); after != pyBefore {
		t.Fatal("Python's own logs.jsonl was modified")
	}
	// counts: Python's (rebuilt from its logs: 2026-11-20 edits 2) plus Go's one generation
	var counts map[string]map[string]map[string]int
	raw, _ = os.ReadFile(filepath.Join(out, "image_output_counts.json"))
	json.Unmarshal(raw, &counts)
	if day := counts["days"]["2026-11-20"]; day["edits"] != 2 || day["generations"] != 1 {
		t.Fatalf("counts %v", counts)
	}
	// image, thumbnail, tags, task
	for _, path := range []string{"images/2026/11/20/new.png", "image_thumbnails/2026/11/20/new.png.png"} {
		if _, err := os.Stat(filepath.Join(out, path)); err != nil {
			t.Fatalf("%s not copied", path)
		}
	}
	var index map[string]map[string]any
	raw, _ = os.ReadFile(filepath.Join(out, "image_index.json"))
	json.Unmarshal(raw, &index)
	if len(index["items"]) != 3 || index["items"]["2026/11/20/new.png"] == nil {
		t.Fatalf("index %v", index)
	}
	var tags map[string][]string
	raw, _ = os.ReadFile(filepath.Join(out, "image_tags.json"))
	json.Unmarshal(raw, &tags)
	if tags["2026/11/20/new.png"][0] != "新" || tags["2026/11/20/z.png"][0] != "猫" {
		t.Fatalf("tags %v", tags)
	}
	var tasks map[string][]map[string]any
	raw, _ = os.ReadFile(filepath.Join(out, "image_tasks.json"))
	json.Unmarshal(raw, &tasks)
	if len(tasks["tasks"]) != 2 {
		t.Fatalf("tasks %v", tasks)
	}

	// the merged directory is a valid Python data directory: the migration accepts it
	merged := t.TempDir()
	copyTree(t, python, merged)
	copyTree(t, out, merged)
	if _, err := Validate(Options{DataDir: merged}); err != nil {
		t.Fatalf("merged data does not read: %v", err)
	}
}

func TestRollbackRefusesUnsafeInputs(t *testing.T) {
	snapshot := fixture(t)
	db := openDB(t)
	mustRun(t, db, options(snapshot))
	python := t.TempDir()
	copyTree(t, snapshot, python)
	run := func() error {
		_, err := Rollback(db, RollbackOptions{Snapshot: snapshot, Python: python, GoData: t.TempDir(), Out: filepath.Join(t.TempDir(), "out")})
		return err
	}
	// an API key added on Go during the parallel run: refused, nothing written
	db.Exec("UPDATE compat_auth_keys SET enabled = true WHERE id = 'k1'")
	if err := run(); err == nil || !strings.Contains(err.Error(), "管理数据") {
		t.Fatalf("admin change not refused: %v", err)
	}
	db.Exec("UPDATE compat_auth_keys SET enabled = false WHERE id = 'k1'")
	if err := run(); err != nil {
		t.Fatalf("clean rollback: %v", err)
	}
	// the snapshot changed after the migration
	write(t, snapshot, "proxies.json", `[]`)
	if err := run(); err == nil || !strings.Contains(err.Error(), "校验和") {
		t.Fatalf("changed snapshot not refused: %v", err)
	}
	// a database the migration did not make
	if _, err := Rollback(openDB(t), RollbackOptions{Snapshot: snapshot, Python: python, GoData: t.TempDir(), Out: filepath.Join(t.TempDir(), "o")}); err == nil {
		t.Fatal("database without migration_info accepted")
	}
}
