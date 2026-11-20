package sources

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/docstore"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

type echoRemote struct{}

func (echoRemote) UserInfo(_ context.Context, account pool.Account) (pool.Account, error) {
	return account, nil
}
func (echoRemote) RefreshAccessToken(context.Context, string, pool.Account) (map[string]string, error) {
	return nil, errors.New("no refresh token")
}

func setup(t *testing.T) (*docstore.Store, *pool.Service) {
	t.Helper()
	repo, err := repository.Open("sqlite:///" + filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	docstore.Migrate(repo.DB())
	pool.Migrate(repo.DB())
	accounts, err := pool.New(pool.NewDBStore(repo.DB()), settings.NewForTest(map[string]any{"auto_remove_invalid_accounts": false}, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	accounts.Remote = echoRemote{}
	t.Cleanup(accounts.Flush) // account writes are asynchronous; finish them before the database closes
	return docstore.New(repo.DB()), accounts
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func TestCPAPoolsAreSanitizedPersistedAndImported(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			writeJSON(w, 401, map[string]any{})
			return
		}
		switch r.URL.Path {
		case "/v0/management/auth-files":
			writeJSON(w, 200, map[string]any{"files": []any{map[string]any{"name": "a.json", "email": "a@x"}, map[string]any{"name": "b.json", "account": "b@x"}, map[string]any{"name": ""}}})
		case "/v0/management/auth-files/download":
			switch r.URL.Query().Get("name") {
			case "a.json":
				writeJSON(w, 200, map[string]any{"access_token": "tok-a"})
			case "b.json":
				writeJSON(w, 200, map[string]any{})
			default:
				writeJSON(w, 404, map[string]any{})
			}
		}
	}))
	defer server.Close()
	docs, accounts := setup(t)
	cpa, _ := NewCPA(docs, accounts, func() string { return "" })
	item, err := cpa.Add("main", server.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if pools := cpa.Pools(); len(pools) != 1 || pools[0]["secret_key"] != nil || pools[0]["name"] != "main" {
		t.Fatalf("pools %v", pools)
	}
	files, err := cpa.Files(context.Background(), item)
	if err != nil || len(files) != 2 || files[1]["email"] != "b@x" {
		t.Fatalf("files %v err %v", files, err)
	}
	if _, err := cpa.Import(item, []string{" "}); err == nil {
		t.Fatal("empty selection accepted")
	}
	job, err := cpa.Import(item, []string{"a.json", "b.json", "c.json"})
	if err != nil || job["status"] != "pending" || job["total"] != 3 {
		t.Fatalf("job %v err %v", job, err)
	}
	cpa.Wait()
	done := cpa.Job(clean(item["id"]))
	if done["status"] != "completed" || done["added"] != 1 || done["failed"] != 2 || done["completed"] != 3 {
		t.Fatalf("done %v", done)
	}
	if account := accounts.GetAccount("tok-a"); account == nil || account["source_type"] != "codex" {
		t.Fatalf("account %v", account)
	}
	// a fresh instance reads the document; a running job left behind becomes failed
	cpa.store.updateJob(clean(item["id"]), map[string]any{"status": "running"})
	again, _ := NewCPA(docs, accounts, nil)
	if again.Job(clean(item["id"]))["status"] != "failed" {
		t.Fatalf("reloaded %v", again.Job(clean(item["id"])))
	}
	updated, _ := again.Update(clean(item["id"]), map[string]any{"name": "renamed"})
	if updated["name"] != "renamed" || updated["secret_key"] != "secret" {
		t.Fatalf("updated %v", updated)
	}
	if removed, _ := again.Delete(clean(item["id"])); !removed || len(again.Pools()) != 0 {
		t.Fatal("delete")
	}
}

func TestOldSinglePoolDocumentIsRead(t *testing.T) {
	docs, accounts := setup(t)
	docs.Save(cpaDocument, map[string]any{"base_url": "http://cpa", "secret_key": "k"})
	cpa, _ := NewCPA(docs, accounts, nil)
	if pools := cpa.Pools(); len(pools) != 1 || pools[0]["base_url"] != "http://cpa" {
		t.Fatalf("pools %v", pools)
	}
}

func TestSub2APILoginIsCachedAndPagesAreFollowed(t *testing.T) {
	var logins int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/auth/login":
			atomic.AddInt32(&logins, 1)
			writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]any{"access_token": "jwt", "expires_in": 86400}})
			return
		}
		if r.Header.Get("Authorization") != "Bearer jwt" {
			writeJSON(w, 401, map[string]any{})
			return
		}
		switch r.URL.Path {
		case "/api/v1/admin/accounts":
			if r.URL.Query().Get("group") != "g1" || r.URL.Query().Get("platform") != "openai" {
				t.Errorf("query %v", r.URL.Query())
			}
			page := r.URL.Query().Get("page")
			items := []any{}
			count := 200
			if page == "2" {
				count = 3
			}
			for i := 0; i < count; i++ {
				items = append(items, map[string]any{"id": i, "name": "n", "status": "active",
					"credentials": map[string]any{"email": "e@x", "plan_type": "plus", "refresh_token": "r"}})
			}
			writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]any{"items": items, "total": 203}})
		case "/api/v1/admin/groups":
			writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]any{"items": []any{map[string]any{"id": 7, "name": "g", "account_count": 3}}, "total": 1}})
		case "/api/v1/admin/accounts/data":
			writeJSON(w, 200, map[string]any{"code": 0, "data": map[string]any{"accounts": []any{
				map[string]any{"id": 1, "credentials": map[string]any{"accessToken": "sub-tok-1"}},
				map[string]any{"id": 2, "credentials": map[string]any{}},
			}}})
		}
	}))
	defer server.Close()
	docs, accounts := setup(t)
	sub, _ := NewSub2API(docs, accounts)
	item, _ := sub.Add(map[string]any{"name": "s", "base_url": server.URL, "email": "admin@x", "password": "pw", "api_key": "", "group_id": "g1"})
	if servers := sub.Servers(); servers[0]["password"] != nil || servers[0]["has_api_key"] != false {
		t.Fatalf("servers %v", servers)
	}
	remote, err := sub.RemoteAccounts(context.Background(), item)
	if err != nil || len(remote) != 203 || remote[0]["has_refresh_token"] != true || remote[0]["id"] != "0" {
		t.Fatalf("remote %d err %v", len(remote), err)
	}
	groups, err := sub.Groups(context.Background(), item)
	if err != nil || len(groups) != 1 || groups[0]["id"] != "7" || groups[0]["account_count"] != 3 {
		t.Fatalf("groups %v err %v", groups, err)
	}
	if logins != 1 {
		t.Fatalf("logged in %d times", logins)
	}
	sub.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	sub.Groups(context.Background(), item)
	if logins != 2 {
		t.Fatalf("expired token not renewed: %d logins", logins)
	}
	sub.Now = time.Now
	if _, err := sub.Import(item, []string{"1", "2", "3"}); err != nil {
		t.Fatal(err)
	}
	sub.Wait()
	job := sub.Job(clean(item["id"]))
	errs := job["errors"].([]any)
	if job["status"] != "completed" || job["added"] != 1 || len(errs) != 2 || !strings.Contains(errs[1].(map[string]any)["error"].(string), "exported 2/3") {
		t.Fatalf("job %v", job)
	}
}

func TestSub2APIWithoutCredentialsFails(t *testing.T) {
	docs, accounts := setup(t)
	sub, _ := NewSub2API(docs, accounts)
	item, _ := sub.Add(map[string]any{"base_url": "http://127.0.0.1:1"})
	if _, err := sub.Groups(context.Background(), item); err == nil || !strings.Contains(err.Error(), "email+password or api_key") {
		t.Fatalf("err %v", err)
	}
}
