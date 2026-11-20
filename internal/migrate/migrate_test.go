package migrate

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/basketikun/chatgpt2api/internal/docstore"
	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/repository"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture is a small Python data directory.
func fixture(t *testing.T) string {
	dir := t.TempDir()
	write(t, dir, "accounts.json", `[
		{"access_token": "tok-b", "email": "b@x", "status": "正常", "quota": 3, "proxy": "http://p:1", "edit_upload_blocked_until": "2026-11-20 10:00:00", "limits_progress": [{"feature_name": "image_gen", "remaining": 3}]},
		{"access_token": "tok-a", "email": "a@x", "status": "限流", "quota": 0},
		{"access_token": "tok-b", "email": "dup@x"},
		{"email": "no-token@x"}]`)
	write(t, dir, ".cumulative_total", "57\n")
	write(t, dir, "auth_keys.json", `{"items": [{"id": "k1", "name": "u", "role": "user", "key_hash": "h1", "enabled": false, "created_at": "2026-10-01T00:00:00+00:00"},
		{"role": "admin", "key_hash": "h2"}, {"role": "bogus", "key_hash": "h3"}]}`)
	write(t, dir, "proxies.json", `[{"url": "http://p:1", "name": "A", "disabled": false}]`)
	write(t, dir, "image_index.json", `{"items": {
		"2026/11/20/z.png": {"date": "2026-11-20", "created_at": "2026-11-20 09:00:00", "storage": "local", "local": true, "webdav": false},
		"2026/11/19/a.png": {"date": "2026-11-19", "created_at": "2026-11-19 09:00:00", "storage": "both", "webdav": true},
		".DS_Store": {"date": "x"}}}`)
	write(t, dir, "image_tags.json", `{"2026/11/20/z.png": ["猫"], "2026/11/19/a.png": []}`)
	write(t, dir, "image_tasks.json", `{"tasks": [{"id": "t1", "owner_id": "k1", "status": "success", "updated_at": "2026-11-20 09:00:00"}, {"id": "t2"}]}`)
	write(t, dir, "logs.jsonl", strings.Join([]string{
		`{"id":"old","time":"2026-09-01 10:00:00","type":"call","summary":"文生图调用完成","detail":{"endpoint":"/v1/images/generations","status":"success","urls":["u"]}}`,
		`{"time":"2026-11-19 10:00:00","type":"account","summary":"legacy line without id","detail":{}}`,
		`not json`,
		`{"id":"new","time":"2026-11-20 08:00:00","type":"call","summary":"图生图调用完成","detail":{"endpoint":"/v1/images/edits","status":"success","urls":["a","b"]}}`,
	}, "\n")+"\n")
	return dir
}

func openDB(t *testing.T) *gorm.DB {
	repo, err := repository.Open("sqlite:///" + filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	return repo.DB()
}

func checksums(t *testing.T, dir string) map[string]string {
	out := map[string]string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			raw, _ := os.ReadFile(p)
			sum := md5.Sum(raw)
			info, _ := d.Info()
			out[p] = hex.EncodeToString(sum[:]) + info.ModTime().String()
		}
		return nil
	})
	return out
}

func options(dir string) Options {
	return Options{DataDir: dir, LogDays: 30, Now: time.Date(2026, 11, 20, 12, 0, 0, 0, time.Local), Location: time.Local}
}

func TestMigrationIsFaithfulIdempotentAndReadOnly(t *testing.T) {
	dir := fixture(t)
	before := checksums(t, dir)
	db := openDB(t)
	for run := 0; run < 2; run++ { // D3: a second run gives the same rows
		report, err := Run(db, options(dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, check := range Verify(db, options(dir), report) {
			if !check.OK {
				t.Fatalf("run %d: %+v", run, check)
			}
		}
	}
	if after := checksums(t, dir); len(after) != len(before) {
		t.Fatal("files were added to the source")
	} else {
		for path, sum := range before {
			if after[path] != sum {
				t.Fatalf("%s changed", path) // D4
			}
		}
	}

	// accounts: file order, duplicates and token-less entries dropped, every field kept
	accounts, _ := pool.NewDBStore(db).Load()
	if len(accounts) != 2 || accounts[0]["access_token"] != "tok-b" || accounts[1]["access_token"] != "tok-a" ||
		accounts[0]["email"] != "dup@x" || accounts[1]["quota"].(json.Number).String() != "0" {
		t.Fatalf("accounts %v", accounts)
	}
	if total, ok := pool.NewDBStore(db).LoadInt("cumulative_total"); !ok || total != 57 {
		t.Fatalf("cumulative %d %v", total, ok)
	}
	var keys []map[string]any
	db.Table("compat_auth_keys").Order("position").Find(&keys)
	if len(keys) != 2 || keys[0]["id"] != "k1" || keys[1]["name"] != "管理员密钥" {
		t.Fatalf("keys %v", keys)
	}
	var proxies any
	if found, _ := docstore.New(db).Load("proxies", &proxies); !found {
		t.Fatal("proxies missing")
	}
	var images []imageRow
	db.Order("seq").Find(&images)
	if len(images) != 2 || images[0].Rel != "2026/11/20/z.png" || !images[1].Webdav {
		t.Fatalf("images %v", images)
	}
	var tags []tagRow
	db.Find(&tags)
	if len(tags) != 1 || tags[0].Rel != "2026/11/20/z.png" {
		t.Fatalf("tags %v", tags)
	}
	var tasks []taskRow
	db.Find(&tasks)
	if len(tasks) != 1 || tasks[0].Key != "k1:t1" {
		t.Fatalf("tasks %v", tasks)
	}

	// logs: only the window, legacy lines get the id Python's list shows, file order kept
	entries, _ := logs.New(db).List("", "", "", 10)
	if len(entries) != 2 || entries[0]["id"] != "new" || entries[1]["id"] != LegacyLogID(`{"time":"2026-11-19 10:00:00","type":"account","summary":"legacy line without id","detail":{}}`) {
		t.Fatalf("logs %v", entries)
	}
}

func TestDailyCountsComeFromTheFileOrAreRebuiltFromAllLogs(t *testing.T) {
	dir := fixture(t)
	db := openDB(t)
	if _, err := Run(db, options(dir)); err != nil {
		t.Fatal(err)
	}
	var rows []logs.DailyCount
	db.Order("day").Find(&rows)
	// no image_output_counts.json: rebuilt from every line, including the one older than the log window
	if len(rows) != 2 || rows[0].Day != "2026-09-01" || rows[0].Generations != 1 || rows[1].Edits != 2 {
		t.Fatalf("rebuilt %+v", rows)
	}
	write(t, dir, "image_output_counts.json", `{"days": {"2026-09-09": {"edits": 4}, "bad-day": {"edits": 1}}}`)
	db2 := openDB(t)
	if _, err := Run(db2, options(dir)); err != nil {
		t.Fatal(err)
	}
	var fromFile []logs.DailyCount
	db2.Find(&fromFile)
	if len(fromFile) != 1 || fromFile[0].Day != "2026-09-09" || fromFile[0].Edits != 4 || fromFile[0].Generations != 0 {
		t.Fatalf("from file %+v", fromFile)
	}
}

func TestRerunFollowsTheSource(t *testing.T) {
	dir := fixture(t)
	db := openDB(t)
	Run(db, options(dir))
	write(t, dir, "accounts.json", `[{"access_token": "tok-c"}]`)
	if _, err := Run(db, options(dir)); err != nil {
		t.Fatal(err)
	}
	accounts, _ := pool.NewDBStore(db).Load()
	if len(accounts) != 1 || accounts[0]["access_token"] != "tok-c" {
		t.Fatalf("accounts %v", accounts)
	}
}

func TestMissingFilesAreReportedNotFatal(t *testing.T) {
	report, err := Run(openDB(t), options(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	skipped := 0
	for _, item := range report.Items {
		if item.Skipped != "" {
			skipped++
		}
	}
	if skipped < 10 {
		t.Fatalf("report %+v", report.Items)
	}
}
