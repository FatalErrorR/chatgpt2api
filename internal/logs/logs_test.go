package logs

import (
	"path/filepath"
	"reflect"
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
	return New(repo.DB())
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
