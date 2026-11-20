package filetasks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/settings"
	"gorm.io/gorm"
)

type keepRemote struct{}

func (keepRemote) UserInfo(context.Context, pool.Account) (pool.Account, error) {
	return pool.Account{}, nil
}
func (keepRemote) RefreshAccessToken(context.Context, string, pool.Account) (map[string]string, error) {
	return nil, errors.New("no refresh token")
}

func setup(t *testing.T, accounts []pool.Account) (*Service, *gorm.DB) {
	t.Helper()
	dir := t.TempDir()
	repo, err := repository.Open("sqlite:///" + filepath.Join(dir, "files.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	db := repo.DB()
	for _, migrate := range []func(*gorm.DB) error{Migrate, pool.Migrate, logs.Migrate} {
		if err := migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	accountPool, err := pool.New(pool.NewDBStore(db), settings.NewForTest(map[string]any{"auto_remove_invalid_accounts": false}, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	accountPool.Remote = keepRemote{}
	accountPool.AddAccountItems(accounts)
	s, err := New(db, accountPool, logs.New(db), filepath.Join(dir, "files"))
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

var alice = map[string]any{"id": "alice", "name": "Alice", "role": "user"}

func waitFor(t *testing.T, s *Service, id, status string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		items := s.List(alice, []string{id})["items"].([]any)
		if len(items) == 1 && items[0].(map[string]any)["status"] == status {
			return items[0].(map[string]any)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never reached %s: %v", id, status, s.List(alice, []string{id}))
	return nil
}

func TestPPTTaskUsesLeastRecentlyUsedPaidAccountAndServesFiles(t *testing.T) {
	s, db := setup(t, []pool.Account{
		{"access_token": "free", "type": "free", "status": "正常"},
		{"access_token": "plus-new", "type": "Plus", "status": "正常", "last_used_at": "2026-11-20 10:00:00", "email": "new@test"},
		{"access_token": "team-old", "type": "Team", "status": "正常", "last_used_at": "2026-11-19 10:00:00", "email": "old@test"},
	})
	var usedToken string
	s.Export = func(_ context.Context, token, kind string, images []string, prompt, dir string) (string, string, string, error) {
		usedToken = token
		os.MkdirAll(dir, 0o755)
		primary, zip := filepath.Join(dir, "deck 1.pptx"), filepath.Join(dir, "parts.zip")
		os.WriteFile(primary, []byte("ppt"), 0o644)
		os.WriteFile(zip, []byte("zip"), 0o644)
		return "conv-1", primary, zip, nil
	}
	first := s.Submit(alice, "t1", "ppt", "Q2 review", nil, "http://app.test/")
	if first["status"] != "queued" || first["taskId"] != "t1" {
		t.Fatalf("first %v", first)
	}
	item := waitFor(t, s, "t1", "success")
	s.Wait()
	result := item["result"].(map[string]any)
	if usedToken != "team-old" || result["primary_url"] != "http://app.test/files/ppt/t1/deck%201.pptx" || result["zip_url"] != "http://app.test/files/ppt/t1/parts.zip" {
		t.Fatalf("token %s result %v", usedToken, result)
	}
	if full, err := s.PublicFile("ppt/t1/deck 1.pptx"); err != nil || filepath.Base(full) != "deck 1.pptx" {
		t.Fatalf("file %s %v", full, err)
	}
	for _, bad := range []string{"../files.db", "ppt/t1/missing.zip", "ppt"} {
		if _, err := s.PublicFile(bad); err == nil {
			t.Fatalf("%s was served", bad)
		}
	}
	if entries, _ := logs.New(db).List("call", "", "", 10); len(entries) != 1 || entries[0]["summary"] != "PPT生成任务完成" {
		t.Fatalf("logs %v", entries)
	}
	if again := s.Submit(alice, "t1", "ppt", "other", nil, ""); again["status"] != "success" {
		t.Fatalf("resubmit %v", again)
	}
}

func TestPSDWithoutImagesAndNoPaidAccountFail(t *testing.T) {
	s, _ := setup(t, []pool.Account{{"access_token": "free", "type": "free", "status": "正常"}})
	s.Export = func(context.Context, string, string, []string, string, string) (string, string, string, error) {
		t.Fatal("export must not run")
		return "", "", "", nil
	}
	s.Submit(alice, "psd", "psd", "", nil, "")
	if item := waitFor(t, s, "psd", "error"); item["error"] != "base64_images is empty" {
		t.Fatalf("item %v", item)
	}
	s.Submit(alice, "ppt", "ppt", "", nil, "")
	if item := waitFor(t, s, "ppt", "error"); item["error"] != "no available plus/team/pro account" {
		t.Fatalf("item %v", item)
	}
	s.Wait()
}

func TestRestartFailsUnfinishedTasksAndOwnersAreSeparate(t *testing.T) {
	s, db := setup(t, []pool.Account{{"access_token": "plus", "type": "Plus", "status": "正常"}})
	block := make(chan struct{})
	s.Export = func(context.Context, string, string, []string, string, string) (string, string, string, error) {
		<-block
		return "", "", "", errors.New("late")
	}
	s.Submit(alice, "t1", "ppt", "", nil, "")
	waitFor(t, s, "t1", "running")
	again, err := New(db, s.pool, nil, s.Root)
	if err != nil {
		t.Fatal(err)
	}
	items := again.List(alice, []string{"t1"})["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["error"] != restartError {
		t.Fatalf("items %v", items)
	}
	if other := again.List(map[string]any{"id": "bob"}, []string{"t1"}); len(other["missing_ids"].([]any)) != 1 {
		t.Fatalf("bob sees %v", other)
	}
	close(block)
	s.Wait()
}
