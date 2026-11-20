package pool

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

// gatedStore blocks every Save while closed is set, until the gate opens.
type gatedStore struct {
	*DBStore
	gate   chan struct{}
	mu     sync.Mutex
	closed bool
	held   int
}

func (g *gatedStore) Save(rows []SavedRow, removed []string) error {
	g.mu.Lock()
	closed := g.closed
	if closed {
		g.held++
	}
	g.mu.Unlock()
	if closed {
		<-g.gate
	}
	return g.DBStore.Save(rows, removed)
}

func dbService(t *testing.T, store Store) (*Service, *repository.Repository) {
	t.Helper()
	repo, err := repository.Open("sqlite:///" + filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	Migrate(repo.DB())
	if gated, ok := store.(*gatedStore); ok {
		gated.DBStore = NewDBStore(repo.DB())
	}
	if store == nil {
		store = NewDBStore(repo.DB())
	}
	service, err := New(store, settings.NewForTest(map[string]any{"auto_remove_invalid_accounts": false}, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	return service, repo
}

// Every request takes the pool lock; a slow database write used to be made while holding it, so
// 300 concurrent image requests queued behind each other's writes (about 0.5 s at p95).
func TestPoolDoesNotWaitForTheDatabase(t *testing.T) {
	store := &gatedStore{gate: make(chan struct{})}
	service, repo := dbService(t, store)
	service.AddAccountItems([]Account{{"access_token": "a"}, {"access_token": "b"}})
	service.Flush()
	store.mu.Lock()
	store.closed = true // the database hangs from here on
	store.mu.Unlock()
	opened := false
	open := func() {
		if !opened {
			opened = true
			close(store.gate)
		}
	}
	defer open()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			service.UpdateAccount("a", Account{"quota": i}, true)
			service.MarkImageResult("b", true)
			service.GetAccount("a")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pool operations waited for the blocked database write")
	}
	open()
	service.Flush()
	restarted, err := New(NewDBStore(repo.DB()), settings.NewForTest(nil, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := intOf(restarted.GetAccount("a")["quota"]); got != 49 {
		t.Fatalf("quota after restart %d, want the last value 49", got)
	}
	if got := intOf(restarted.GetAccount("b")["success"]); got != 50 {
		t.Fatalf("success after restart %d, want 50", got)
	}
	store.mu.Lock()
	writes := store.held
	store.mu.Unlock()
	if writes >= 100 {
		t.Fatalf("%d writes for 100 saves: queued saves should be merged", writes)
	}
}

// Writes land in the order the saves were made, and the order of the pool survives a delete
// followed by an add (positions are rewritten, so no two accounts share one).
func TestSavedOrderMatchesThePool(t *testing.T) {
	service, repo := dbService(t, nil)
	var items []Account
	for i := 0; i < 6; i++ {
		items = append(items, Account{"access_token": fmt.Sprintf("t%d", i)})
	}
	service.AddAccountItems(items)
	service.DeleteAccounts([]string{"t0", "t2"})
	service.AddAccountItems([]Account{{"access_token": "n1"}, {"access_token": "n2"}})
	for i := 0; i < 20; i++ {
		service.UpdateAccount("t3", Account{"quota": i}, true)
	}
	service.Flush()
	restarted, err := New(NewDBStore(repo.DB()), settings.NewForTest(nil, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(service.ListTokens(), ",")
	if got := strings.Join(restarted.ListTokens(), ","); got != want || want != "t1,t3,t4,t5,n1,n2" {
		t.Fatalf("order after restart %s, pool %s", got, want)
	}
	if got := intOf(restarted.GetAccount("t3")["quota"]); got != 19 {
		t.Fatalf("quota %d, want 19", got)
	}
}
