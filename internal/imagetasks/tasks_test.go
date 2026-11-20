package imagetasks

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/settings"
	"gorm.io/gorm"
)

// Ported from test_image_task_service.py; plus resume-poll and retention.

func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	repo, err := repository.Open("sqlite:///" + filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := Migrate(repo.DB()); err != nil {
		t.Fatal(err)
	}
	if err := logs.Migrate(repo.DB()); err != nil {
		t.Fatal(err)
	}
	return repo.DB()
}

func newService(t *testing.T, db *gorm.DB) *Service {
	t.Helper()
	callLogs := logs.New(db)
	t.Cleanup(callLogs.Flush) // counts are written asynchronously; before the database closes
	s, err := New(db, settings.NewForTest(map[string]any{"image_retention_days": 30}, ""), callLogs)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var alice = map[string]any{"id": "alice", "name": "Alice", "role": "user"}

func waitStatus(t *testing.T, s *Service, identity map[string]any, id, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		items := s.List(identity, []string{id})["items"].([]any)
		if len(items) == 1 && items[0].(map[string]any)["status"] == want {
			return items[0].(map[string]any)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s never reached %s: %v", id, want, s.List(identity, []string{id}))
	return nil
}

func okRun(calls *int) RunFunc {
	return func(_ context.Context, progress func(string)) (Result, error) {
		*calls++
		progress("image_stream_resolve_start")
		return Result{"data": []map[string]any{{"url": "http://x/images/a.png"}}, "usage": map[string]any{"total_tokens": 1}, "_account_email": "a@test"}, nil
	}
}

func TestDuplicateSubmitUsesExistingTask(t *testing.T) {
	s := newService(t, openDB(t))
	calls := 0
	first, err := s.Submit(alice, "task-1", "generate", "gpt-image-2", "1024x1024", "auto", "cat", okRun(&calls))
	if err != nil || first["status"] != StatusQueued {
		t.Fatalf("first %v %v", first, err)
	}
	waitStatus(t, s, alice, "task-1", StatusSuccess)
	second, _ := s.Submit(alice, "task-1", "generate", "gpt-image-2", "", "auto", "cat", okRun(&calls))
	if calls != 1 || second["status"] != StatusSuccess {
		t.Fatalf("calls %d second %v", calls, second)
	}
}

func TestDifferentOwnerCannotQueryTask(t *testing.T) {
	s := newService(t, openDB(t))
	calls := 0
	s.Submit(alice, "task-1", "generate", "gpt-image-2", "", "auto", "cat", okRun(&calls))
	result := s.List(map[string]any{"id": "bob"}, []string{"task-1"})
	if len(result["items"].([]any)) != 0 || result["missing_ids"].([]any)[0] != "task-1" {
		t.Fatalf("result %v", result)
	}
	if all := s.List(map[string]any{"id": "bob"}, nil); len(all["items"].([]any)) != 0 {
		t.Fatalf("bob sees %v", all)
	}
}

func TestSuccessTaskPersistsToNewServiceInstance(t *testing.T) {
	db := openDB(t)
	s := newService(t, db)
	calls := 0
	s.Submit(alice, "task-1", "generate", "gpt-image-2", "", "auto", "cat", okRun(&calls))
	waitStatus(t, s, alice, "task-1", StatusSuccess)
	s.Wait()
	item := waitStatus(t, newService(t, db), alice, "task-1", StatusSuccess)
	if item["data"] == nil || item["usage"] == nil || item["duration_ms"] == nil {
		t.Fatalf("item %v", item)
	}
	if entries, _ := logs.New(db).List("call", "", "", 10); len(entries) != 1 || entries[0]["summary"] != "文生图调用完成" {
		t.Fatalf("logs %v", entries)
	}
}

func TestStartupMarksUnfinishedTasksAsError(t *testing.T) {
	db := openDB(t)
	s := newService(t, db)
	block := make(chan struct{})
	s.Submit(alice, "task-1", "generate", "gpt-image-2", "", "auto", "cat", func(context.Context, func(string)) (Result, error) {
		<-block
		return nil, errors.New("late")
	})
	defer close(block)
	waitStatus(t, s, alice, "task-1", StatusRunning)
	item := waitStatus(t, newService(t, db), alice, "task-1", StatusError)
	if item["error"] != restartError {
		t.Fatalf("item %v", item)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string             { return "图片生成超时，未拿到结果" }
func (timeoutErr) Account() (string, string) { return "a@test", "conv-1" }

func TestEmptyResultFailsAndTimeoutCanBeResumed(t *testing.T) {
	s := newService(t, openDB(t))
	s.Submit(alice, "empty", "generate", "gpt-image-2", "", "auto", "cat", func(context.Context, func(string)) (Result, error) {
		return Result{"data": []map[string]any{}}, nil
	})
	if item := waitStatus(t, s, alice, "empty", StatusError); item["error"] != emptyResult {
		t.Fatalf("item %v", item)
	}
	if _, err := s.ResumePoll(alice, "empty", 30); err == nil || err.Error() != "task error is not a timeout error" {
		t.Fatalf("err %v", err)
	}
	s.Submit(alice, "slow", "generate", "gpt-image-2", "1024x1024", "high", "cat", func(context.Context, func(string)) (Result, error) {
		return nil, timeoutErr{}
	})
	if item := waitStatus(t, s, alice, "slow", StatusError); item["conversation_id"] != "conv-1" {
		t.Fatalf("item %v", item)
	}
	var got []any
	s.Resume = func(_ context.Context, email, conversation string, extra float64, size, quality string) ([]map[string]any, error) {
		got = []any{email, conversation, extra, size, quality}
		return []map[string]any{{"url": "http://x/images/b.png"}}, nil
	}
	if _, err := s.ResumePoll(alice, "slow", 45); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, s, alice, "slow", StatusSuccess)
	if got[0] != "a@test" || got[1] != "conv-1" || got[2] != 45.0 || got[3] != "1024x1024" || got[4] != "high" {
		t.Fatalf("resume got %v", got)
	}
	if _, err := s.ResumePoll(alice, "missing", 30); err == nil || err.Error() != "task not found" {
		t.Fatalf("err %v", err)
	}
}

func TestFinishedTasksPastRetentionAreDropped(t *testing.T) {
	s := newService(t, openDB(t))
	calls := 0
	s.Submit(alice, "old", "generate", "gpt-image-2", "", "auto", "cat", okRun(&calls))
	waitStatus(t, s, alice, "old", StatusSuccess)
	s.Wait()
	s.now = func() time.Time { return time.Now().AddDate(0, 0, 31) }
	if result := s.List(alice, []string{"old"}); len(result["missing_ids"].([]any)) != 1 {
		t.Fatalf("result %v", result)
	}
}
