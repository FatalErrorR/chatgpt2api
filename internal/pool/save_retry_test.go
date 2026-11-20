package pool

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

// flakyStore fails Save while fail is set.
type flakyStore struct {
	*DBStore
	fail bool
}

func (f *flakyStore) Save(order []string, changed map[string]Account, removed []string) error {
	if f.fail {
		return errors.New("database is down")
	}
	return f.DBStore.Save(order, changed, removed)
}

// A delete whose write failed must still reach the database with the next successful write;
// before the fix the account came back after a restart.
func TestDeleteSurvivesAFailedWrite(t *testing.T) {
	repo, err := repository.Open("sqlite:///" + filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	Migrate(repo.DB())
	store := &flakyStore{DBStore: NewDBStore(repo.DB())}
	cfg := settings.NewForTest(map[string]any{"auto_remove_invalid_accounts": false}, "")
	service, err := New(store, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	service.AddAccountItems([]Account{{"access_token": "keep", "email": "k@x"}, {"access_token": "gone", "email": "g@x"}})

	store.fail = true
	service.DeleteAccounts([]string{"gone"})
	store.fail = false
	service.UpdateAccount("keep", Account{"email": "k2@x"}, true) // a later write that succeeds

	restarted, err := New(NewDBStore(repo.DB()), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.GetAccount("gone") != nil {
		t.Fatal("the deleted account came back after a restart")
	}
	if account := restarted.GetAccount("keep"); account == nil || account["email"] != "k2@x" {
		t.Fatalf("keep %v", account)
	}

	// a failed update is retried too
	store.fail = true
	service.UpdateAccount("keep", Account{"email": "k3@x"}, true)
	store.fail = false
	service.AddAccountItems([]Account{{"access_token": "new", "email": "n@x"}})
	restarted, _ = New(NewDBStore(repo.DB()), cfg, nil)
	if account := restarted.GetAccount("keep"); account == nil || account["email"] != "k3@x" || restarted.GetAccount("new") == nil {
		t.Fatalf("after retry %v", restarted.ListAccounts())
	}
}
