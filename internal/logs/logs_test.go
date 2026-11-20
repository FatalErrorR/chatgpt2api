package logs

import (
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/repository"
)

// Ported from test/test_image_output_counts.py.

func newService(t *testing.T) *Service {
	t.Helper()
	repo, err := repository.Open("sqlite:///" + filepath.Join(t.TempDir(), "logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := Migrate(repo.DB()); err != nil {
		t.Fatal(err)
	}
	s := New(repo.DB())
	t.Cleanup(s.Flush) // counts are written asynchronously; before the database closes
	return s
}

// Concurrent successful calls all bump the same day's row. They are added up off the call's path;
// the counts read the same before and after the write, and the table ends up exact.
func TestConcurrentCountsAreExact(t *testing.T) {
	s := newService(t)
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, beijing)
	s.now = func() time.Time { return at }
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			endpoint := "/v1/images/generations"
			if i%4 == 0 {
				endpoint = "/v1/images/edits"
			}
			s.Add("call", "", map[string]any{"endpoint": endpoint, "status": "success", "urls": []any{"a", "b"}})
		}(i)
	}
	wg.Wait()
	want := map[string]int64{"edits": 100, "generations": 300}
	check := func(when string) {
		counts, err := s.ImageOutputCounts(at)
		if err != nil {
			t.Fatal(err)
		}
		if counts["edits"]["today"] != want["edits"] || counts["generations"]["today"] != want["generations"] {
			t.Fatalf("%s: edits %d generations %d", when, counts["edits"]["today"], counts["generations"]["today"])
		}
	}
	check("before the write")
	s.Flush()
	check("after the write")
	var rows []DailyCount
	s.db.Find(&rows)
	if len(rows) != 1 || rows[0].Edits != 100 || rows[0].Generations != 300 {
		t.Fatalf("table %+v", rows)
	}
	var entries int64
	s.db.Model(&Entry{}).Count(&entries)
	if entries != 200 {
		t.Fatalf("%d log entries", entries)
	}
}

func call(s *Service, at time.Time, endpoint string, urls []any, status string) {
	s.now = func() time.Time { return at }
	s.Add("call", "", map[string]any{"endpoint": endpoint, "status": status, "urls": urls})
}

func TestCountsSuccessfulImagesInBeijingWindows(t *testing.T) {
	s := newService(t)
	cst := func(m time.Month, d, h int) time.Time { return time.Date(2026, m, d, h, 0, 0, 0, beijing) }
	call(s, cst(9, 23, 1), "/v1/images/edits", []any{"a", "b"}, "success")
	call(s, cst(9, 22, 23), "/v1/images/generations", []any{"c"}, "success")
	call(s, cst(9, 10, 12), "/v1/images/edits", []any{"d"}, "success")
	call(s, cst(9, 9, 12), "/v1/images/edits", []any{"e"}, "success") // in d15, not d14
	call(s, cst(8, 1, 12), "/v1/images/edits", []any{"old"}, "success")
	call(s, cst(9, 23, 2), "/v1/images/edits", []any{"x"}, "failed")
	call(s, cst(9, 23, 3), "/v1/images/generations", []any{}, "success")
	s.Add("account", "", map[string]any{})

	counts, err := s.ImageOutputCounts(cst(9, 23, 15))
	if err != nil {
		t.Fatal(err)
	}
	wantEdits := map[string]int64{"today": 2, "yesterday": 0, "d7": 2, "d14": 3, "d15": 4, "d30": 4, "all": 5}
	wantGen := map[string]int64{"today": 0, "yesterday": 1, "d7": 1, "d14": 1, "d15": 1, "d30": 1, "all": 1}
	if !reflect.DeepEqual(counts["edits"], wantEdits) || !reflect.DeepEqual(counts["generations"], wantGen) {
		t.Fatalf("got %v", counts)
	}
}

func TestListReturnsNewestFirstWithFilters(t *testing.T) {
	s := newService(t)
	for i := 0; i < 40; i++ {
		s.now = func() time.Time { return time.Date(2026, 10, 1, 0, i, 0, 0, time.Local) }
		kind := "call"
		if i%2 == 1 {
			kind = "account"
		}
		s.Add(kind, "", map[string]any{"i": i})
	}
	items, _ := s.List("", "", "", 3)
	if len(items) != 3 || items[0]["time"] != "2026-10-01 00:39:00" || items[2]["time"] != "2026-10-01 00:37:00" {
		t.Fatalf("items %v", items)
	}
	if items, _ := s.List("", "2026-10-02", "", 200); len(items) != 0 {
		t.Fatalf("start_date after every log: %v", items)
	}
	if items, _ := s.List("", "", "2026-10-01", 200); len(items) != 40 {
		t.Fatalf("end_date includes the whole day: %d", len(items))
	}
	if items, _ := s.List("account", "2026-10-01", "2026-10-01", 200); len(items) != 20 || items[0]["type"] != "account" {
		t.Fatalf("type filter: %d", len(items))
	}
}

func TestDeleteByID(t *testing.T) {
	s := newService(t)
	s.Add("call", "a", nil)
	s.Add("call", "b", nil)
	items, _ := s.List("", "", "", 200)
	removed, err := s.Delete([]string{items[0]["id"].(string), "missing", ""})
	if err != nil || removed != 1 {
		t.Fatalf("removed %d err %v", removed, err)
	}
	if items, _ := s.List("", "", "", 200); len(items) != 1 || items[0]["summary"] != "a" {
		t.Fatalf("left %v", items)
	}
}

// A failed count write loses nothing: the counts stay pending and the next Flush writes them.
func TestCountsSurviveAFailedWrite(t *testing.T) {
	s := newService(t)
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, beijing)
	s.now = func() time.Time { return at }
	if err := s.db.Exec("ALTER TABLE compat_image_output_counts RENAME TO counts_away").Error; err != nil {
		t.Fatal(err)
	}
	s.Add("call", "", map[string]any{"endpoint": "/v1/images/generations", "status": "success", "urls": []any{"a", "b", "c"}})
	s.Flush() // the write fails
	if err := s.db.Exec("ALTER TABLE counts_away RENAME TO compat_image_output_counts").Error; err != nil {
		t.Fatal(err)
	}
	if counts, _ := s.ImageOutputCounts(at); counts["generations"]["today"] != 3 {
		t.Fatalf("pending count not read: %v", counts)
	}
	s.Flush() // written now
	var rows []DailyCount
	s.db.Find(&rows)
	if len(rows) != 1 || rows[0].Generations != 3 {
		t.Fatalf("table %+v", rows)
	}
}
