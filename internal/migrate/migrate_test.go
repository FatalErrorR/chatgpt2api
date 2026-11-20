package migrate

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

// Each test runs on its own fresh database. With C2A_TEST_POSTGRES_URL set (an admin URL such as
// postgres://c2a:c2a@127.0.0.1:55432/c2a?sslmode=disable) that is a new PostgreSQL database per
// test, dropped afterwards; otherwise SQLite.
func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	admin := os.Getenv("C2A_TEST_POSTGRES_URL")
	if admin == "" {
		repo, err := repository.Open("sqlite:///" + filepath.Join(t.TempDir(), "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { repo.Close() })
		return repo.DB()
	}
	adminRepo, err := repository.Open(admin)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("c2a_migrate_test_%d", time.Now().UnixNano())
	if err := adminRepo.DB().Exec("CREATE DATABASE " + name).Error; err != nil {
		t.Fatal(err)
	}
	target := strings.Replace(admin, "/c2a?", "/"+name+"?", 1)
	repo, err := repository.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		repo.Close()
		adminRepo.DB().Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
		adminRepo.Close()
	})
	return repo.DB()
}

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
		{"accessToken": "tok-legacy", "email": "legacy@x", "status": "正常"},
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
	legacy := `{"time":"2026-11-19 10:00:00","type":"account","summary":"legacy line without id","detail":{}}`
	write(t, dir, "logs.jsonl", strings.Join([]string{
		`{"id":"old","time":"2026-09-01 10:00:00","type":"call","summary":"文生图调用完成","detail":{"endpoint":"/v1/images/generations","status":"success","urls":["u"]}}`,
		legacy,
		`not json`,
		legacy, // the same line again: a separate event, must not be merged
		`{"id":"new","time":"2026-11-20 08:00:00","type":"call","summary":"图生图调用完成","detail":{"endpoint":"/v1/images/edits","status":"success","urls":["a","b"]}}`,
	}, "\n")+"\n")
	return dir
}

func options(dir string) Options {
	return Options{DataDir: dir, LogDays: 30, Now: time.Date(2026, 11, 20, 12, 0, 0, 0, time.Local), Location: time.Local}
}

func checksums(dir string) map[string]string {
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

func mustRun(t *testing.T, db *gorm.DB, opts Options) *Report {
	t.Helper()
	report, err := Run(db, opts)
	if err != nil {
		t.Fatalf("migration failed: %v %+v", err, report)
	}
	for _, check := range report.Checks {
		if !check.OK {
			t.Fatalf("check failed: %+v", check)
		}
	}
	return report
}

func accountTokens(t *testing.T, db *gorm.DB) []string {
	accounts, err := pool.NewDBStore(db).Load()
	if err != nil {
		t.Fatal(err)
	}
	tokens := []string{}
	for _, account := range accounts {
		tokens = append(tokens, account["access_token"].(string))
	}
	return tokens
}

func TestMigrationIsFaithfulIdempotentAndReadOnly(t *testing.T) {
	dir := fixture(t)
	before := checksums(dir)
	db := openDB(t)
	mustRun(t, db, options(dir))
	mustRun(t, db, options(dir)) // D3: a second run gives the same rows
	after := checksums(dir)
	for path, sum := range before {
		if after[path] != sum {
			t.Fatalf("%s changed", path) // D4
		}
	}
	if len(after) != len(before) {
		t.Fatal("files were added to the source")
	}

	// accounts: file order; a repeated token keeps its first position and its last values; the
	// legacy accessToken account is migrated (Python loads it) with the key renamed
	accounts, _ := pool.NewDBStore(db).Load()
	if got := accountTokens(t, db); strings.Join(got, ",") != "tok-b,tok-a,tok-legacy" {
		t.Fatalf("tokens %v", got)
	}
	if accounts[0]["email"] != "dup@x" || accounts[1]["quota"].(json.Number).String() != "0" {
		t.Fatalf("accounts %v", accounts)
	}
	if _, has := accounts[2]["accessToken"]; has || accounts[2]["email"] != "legacy@x" {
		t.Fatalf("legacy account %v", accounts[2])
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

	// logs: only the window; both identical legacy lines kept, newest first
	entries, _ := logs.New(db).List("", "", "", 10)
	if len(entries) != 3 || entries[0]["id"] != "new" || entries[1]["id"] == entries[2]["id"] || entries[1]["summary"] != "legacy line without id" {
		t.Fatalf("logs %v", entries)
	}
}

func TestMalformedSourcesChangeNothing(t *testing.T) {
	dir := fixture(t)
	db := openDB(t)
	mustRun(t, db, options(dir))
	want := accountTokens(t, db)
	var logCount int64
	db.Model(&logs.Entry{}).Count(&logCount)

	cases := map[string][2]string{
		"accounts as an object":         {"accounts.json", `{"items": [{"access_token": "x"}]}`},
		"accounts not JSON":             {"accounts.json", `[{"access_token": "x"`},
		"accounts without any token":    {"accounts.json", `[{"email": "a@x"}, {"email": "b@x"}]`},
		"later file broken":             {"image_tags.json", `{"broken"`},
		"later file in the wrong shape": {"image_index.json", `{"rows": []}`},
		"proxies not a list":            {"proxies.json", `{"url": "http://p:1"}`},
		"counts in the wrong shape":     {"image_output_counts.json", `{"2026-11-20": 3}`},
	}
	for name, change := range cases {
		broken := t.TempDir()
		for _, file := range []string{"accounts.json", ".cumulative_total", "auth_keys.json", "proxies.json", "image_index.json", "image_tags.json", "image_tasks.json", "logs.jsonl"} {
			raw, _ := os.ReadFile(filepath.Join(dir, file))
			write(t, broken, file, string(raw))
		}
		write(t, broken, "accounts.json", `[{"access_token": "would-replace"}]`) // a valid change that must not land
		write(t, broken, change[0], change[1])
		report, err := Run(db, options(broken))
		var sourceErr *SourceError
		if !errors.As(err, &sourceErr) {
			t.Fatalf("%s: expected a source error, got %v %+v", name, err, report)
		}
		if got := accountTokens(t, db); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: accounts changed to %v", name, got)
		}
		var n int64
		db.Model(&logs.Entry{}).Count(&n)
		if n != logCount {
			t.Fatalf("%s: logs changed", name)
		}
	}
	if _, err := Run(db, options(t.TempDir())); err == nil || !strings.Contains(err.Error(), "accounts.json") {
		t.Fatalf("a directory without accounts.json must be refused: %v", err)
	}
}

func TestFailedVerificationRollsEverythingBack(t *testing.T) {
	dir := fixture(t)
	db := openDB(t)
	mustRun(t, db, options(dir))
	want := accountTokens(t, db)
	write(t, dir, "accounts.json", `[{"access_token": "tok-c"}, {"access_token": "tok-d"}]`)
	opts := options(dir)
	opts.afterWrite = func(tx *gorm.DB) error { // a write path that loses an account
		return tx.Where("token = ?", "tok-d").Delete(&accountRow{}).Error
	}
	report, err := Run(db, opts)
	if !errors.Is(err, ErrVerification) || !report.Failed() {
		t.Fatalf("expected a verification failure: %v", err)
	}
	if got := accountTokens(t, db); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("accounts changed to %v after a failed verification", got)
	}
	// verification also catches merged log lines and changed contents
	opts.afterWrite = func(tx *gorm.DB) error {
		return tx.Model(&logs.Entry{}).Where("id = ?", "new").Update("summary", "changed").Error
	}
	if _, err := Run(db, opts); !errors.Is(err, ErrVerification) {
		t.Fatalf("changed log not caught: %v", err)
	}
}

func TestDailyCountsComeFromTheFileOrAreRebuiltFromAllLogs(t *testing.T) {
	dir := fixture(t)
	db := openDB(t)
	mustRun(t, db, options(dir))
	var rows []logs.DailyCount
	db.Order("day").Find(&rows)
	if len(rows) != 2 || rows[0].Day != "2026-09-01" || rows[0].Generations != 1 || rows[1].Edits != 2 {
		t.Fatalf("rebuilt %+v", rows)
	}
	write(t, dir, "image_output_counts.json", `{"days": {"2026-09-09": {"edits": 4}, "bad-day": {"edits": 1}}}`)
	db2 := openDB(t)
	mustRun(t, db2, options(dir))
	var fromFile []logs.DailyCount
	db2.Find(&fromFile)
	if len(fromFile) != 1 || fromFile[0].Day != "2026-09-09" || fromFile[0].Edits != 4 || fromFile[0].Generations != 0 {
		t.Fatalf("from file %+v", fromFile)
	}
}

func TestRerunFollowsTheSource(t *testing.T) {
	dir := fixture(t)
	db := openDB(t)
	mustRun(t, db, options(dir))
	write(t, dir, "accounts.json", `[{"access_token": "tok-c"}]`)
	mustRun(t, db, options(dir))
	if got := accountTokens(t, db); strings.Join(got, ",") != "tok-c" {
		t.Fatalf("accounts %v", got)
	}
}

func TestOptionalFilesMayBeMissing(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "accounts.json", `[]`)
	report := mustRun(t, openDB(t), options(dir))
	missing := 0
	for _, item := range report.Items {
		if strings.Contains(item.Note, "不存在") {
			missing++
		}
	}
	if missing < 8 {
		t.Fatalf("report %+v", report.Items)
	}
}
