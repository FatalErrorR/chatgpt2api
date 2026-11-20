package compat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basketikun/chatgpt2api/internal/docstore"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/proxylist"
	"github.com/basketikun/chatgpt2api/internal/proxyops"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/settings"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const adminKey = "admin-secret"

type harness struct {
	t       *testing.T
	engine  *gin.Engine
	deps    *Deps
	log     *recordedLog
	failing map[string]string // proxy key -> error returned by the fake proxy test
}

type recordedLog struct{ summaries []string }

func (l *recordedLog) Add(kind, summary string, detail map[string]any) error {
	l.summaries = append(l.summaries, summary)
	return nil
}

// stubRemote stands in for ChatGPT: user info leaves the account unchanged (like the Python tests
// that patch refresh_accounts away).
type stubRemote struct{}

func (stubRemote) UserInfo(context.Context, pool.Account) (pool.Account, error) {
	return pool.Account{}, nil
}
func (stubRemote) RefreshAccessToken(context.Context, string, pool.Account) (map[string]string, error) {
	return nil, errors.New("no refresh in tests")
}

func newHarness(t *testing.T, config ...map[string]any) *harness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("CHATGPT2API_AUTH_KEY", "")
	repo, err := repository.Open("sqlite:///" + filepath.Join(t.TempDir(), "compat.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	db := repo.DB()
	for _, migrate := range []func(*gorm.DB) error{Migrate, pool.Migrate, docstore.Migrate} {
		if err := migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	cfg := map[string]any{"auth-key": adminKey}
	for _, extra := range config {
		for key, value := range extra {
			cfg[key] = value
		}
	}
	store := settings.NewForTest(cfg, filepath.Join(t.TempDir(), "config.json"))
	h := &harness{t: t, log: &recordedLog{}, failing: map[string]string{}}
	accounts, err := pool.New(pool.NewDBStore(db), store, h.log)
	if err != nil {
		t.Fatal(err)
	}
	accounts.Remote = stubRemote{}
	list, err := proxylist.New(docstore.New(db))
	if err != nil {
		t.Fatal(err)
	}
	ops := &proxyops.Ops{List: list, Pool: accounts, Settings: store, Log: h.log, Test: func(_ context.Context, url string) map[string]any {
		if cause, failing := h.failing[proxylist.Key(url)]; failing {
			return map[string]any{"ok": false, "status": 0, "latency_ms": 5, "error": cause}
		}
		return map[string]any{"ok": true, "status": 200, "latency_ms": 5, "error": nil}
	}}
	h.deps = &Deps{DB: db, Settings: store, Auth: NewAuthService(db, store), Pool: accounts, Proxies: ops, ProxyOps: ops, Version: "9.9.9", DatabaseURL: "postgres://u:pw@h/db"}
	h.engine = gin.New()
	Register(h.engine, h.deps)
	return h
}

func (h *harness) do(method, path, key string, body any) (int, map[string]any) {
	h.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.engine.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func detailError(out map[string]any) string {
	detail, _ := out["detail"].(map[string]any)
	text, _ := detail["error"].(string)
	return text
}

func TestLoginAndRoles(t *testing.T) {
	h := newHarness(t)
	if code, out := h.do("POST", "/auth/login", "", nil); code != 401 || detailError(out) != "密钥无效或已失效，请重新登录" {
		t.Fatalf("no key: %d %v", code, out)
	}
	code, out := h.do("POST", "/auth/login", adminKey, nil)
	if code != 200 || out["role"] != "admin" || out["subject_id"] != "admin" || out["name"] != "管理员" || out["version"] != "9.9.9" {
		t.Fatalf("admin login: %d %v", code, out)
	}
	_, created := h.do("POST", "/api/auth/users", adminKey, map[string]any{"name": ""})
	userKey, _ := created["key"].(string)
	if !strings.HasPrefix(userKey, "sk-") {
		t.Fatalf("key: %v", created)
	}
	if code, out := h.do("POST", "/auth/login", userKey, nil); code != 200 || out["role"] != "user" || out["name"] != "普通用户" {
		t.Fatalf("user login: %d %v", code, out)
	}
	if code, out := h.do("GET", "/api/settings", userKey, nil); code != 403 || detailError(out) != "需要管理员权限才能执行这个操作" {
		t.Fatalf("user on admin route: %d %v", code, out)
	}
	if code, _ := h.do("GET", "/api/third-party-apps", userKey, nil); code != 200 {
		t.Fatalf("user on identity route: %d", code)
	}
}

func TestUserKeyNamesAndConflicts(t *testing.T) {
	h := newHarness(t)
	names := []string{}
	for i := 0; i < 3; i++ {
		_, out := h.do("POST", "/api/auth/users", adminKey, map[string]any{})
		names = append(names, out["item"].(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "普通用户,普通用户 2,普通用户 3" {
		t.Fatalf("default names: %v", names)
	}
	if code, out := h.do("POST", "/api/auth/users", adminKey, map[string]any{"name": "普通用户 2"}); code != 400 || detailError(out) != "这个名称已经在使用中了，换一个更容易区分的名称吧" {
		t.Fatalf("duplicate name: %d %v", code, out)
	}
	_, list := h.do("GET", "/api/auth/users", adminKey, nil)
	items := list["items"].([]any)
	first := items[0].(map[string]any)["id"].(string)
	if code, out := h.do("POST", "/api/auth/users/"+first, adminKey, map[string]any{"key": adminKey}); code != 400 || detailError(out) != "这个密钥和管理员密钥冲突了，请换一个新的密钥" {
		t.Fatalf("admin key clash: %d %v", code, out)
	}
	if code, out := h.do("POST", "/api/auth/users/"+first, adminKey, map[string]any{"key": "  "}); code != 400 || detailError(out) != "请输入新的专用密钥" {
		t.Fatalf("blank key: %d %v", code, out)
	}
	if code, _ := h.do("POST", "/api/auth/users/"+first, adminKey, map[string]any{"key": "my-own-key"}); code != 200 {
		t.Fatalf("set own key: %d", code)
	}
	second := items[1].(map[string]any)["id"].(string)
	if code, out := h.do("POST", "/api/auth/users/"+second, adminKey, map[string]any{"key": "my-own-key"}); code != 400 || detailError(out) != "这个专用密钥已经存在，请换一个新的密钥" {
		t.Fatalf("duplicate key: %d %v", code, out)
	}
	if code, out := h.do("POST", "/api/auth/users/"+first, adminKey, map[string]any{}); code != 400 || detailError(out) != "还没有检测到改动，请修改后再保存" {
		t.Fatalf("no change: %d %v", code, out)
	}
	if code, out := h.do("POST", "/api/auth/users/nope", adminKey, map[string]any{"name": "x"}); code != 404 || detailError(out) != "这条用户密钥不存在，可能已经被删除" {
		t.Fatalf("missing: %d %v", code, out)
	}
	// disabled keys stop working
	if code, _ := h.do("POST", "/api/auth/users/"+first, adminKey, map[string]any{"enabled": false}); code != 200 {
		t.Fatalf("disable: %d", code)
	}
	if code, _ := h.do("POST", "/auth/login", "my-own-key", nil); code != 401 {
		t.Fatalf("disabled key still works: %d", code)
	}
	if code, out := h.do("DELETE", "/api/auth/users/"+first, adminKey, nil); code != 200 || len(out["items"].([]any)) != 2 {
		t.Fatalf("delete: %d %v", code, out)
	}
	if code, _ := h.do("DELETE", "/api/auth/users/"+first, adminKey, nil); code != 404 {
		t.Fatalf("delete twice: %d", code)
	}
}

func TestOnlyHashesAreStored(t *testing.T) {
	h := newHarness(t)
	_, out := h.do("POST", "/api/auth/users", adminKey, map[string]any{"name": "a"})
	raw := out["key"].(string)
	var rows []AuthKey
	h.deps.DB.Find(&rows)
	if len(rows) != 1 || rows[0].KeyHash != hashKey(raw) || strings.Contains(rows[0].KeyHash, raw) {
		t.Fatalf("stored row: %+v", rows)
	}
	// keys migrated from auth_keys.json (hash only) authenticate
	h.deps.DB.Create(&AuthKey{ID: "legacy", Name: "旧", Role: "user", KeyHash: hashKey("legacy-key"), Enabled: true, CreatedAt: nowISO(), Position: 9})
	if code, out := h.do("POST", "/auth/login", "legacy-key", nil); code != 200 || out["subject_id"] != "legacy" {
		t.Fatalf("legacy key: %d %v", code, out)
	}
	var row AuthKey
	h.deps.DB.First(&row, "id = ?", "legacy")
	if row.LastUsedAt == nil {
		t.Fatal("last_used_at not written on first use")
	}
}

func TestSettingsEndpoints(t *testing.T) {
	h := newHarness(t)
	code, out := h.do("POST", "/api/settings", adminKey, map[string]any{"image_retention_days": 7, "image_storage": map[string]any{"enabled": true}})
	if code != 400 || detailError(out) != "启用 WebDAV 图片存储后必须填写 WebDAV URL" {
		t.Fatalf("invalid storage: %d %v", code, out)
	}
	code, out = h.do("POST", "/api/settings", adminKey, map[string]any{"image_retention_days": 7})
	config, _ := out["config"].(map[string]any)
	if code != 200 || config["image_retention_days"] != float64(7) {
		t.Fatalf("save: %d %v", code, out)
	}
	if _, hasKey := config["auth-key"]; hasKey {
		t.Fatal("auth-key leaked through /api/settings")
	}
	if h.deps.Settings.ImageRetentionDays() != 7 {
		t.Fatal("not stored")
	}
	req := httptest.NewRequest("POST", "/api/settings", strings.NewReader("[1,2]"))
	req.Header.Set("Authorization", "Bearer "+adminKey)
	rec := httptest.NewRecorder()
	h.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("non-object body: %d", rec.Code)
	}
}

func TestStorageInfoMasksPassword(t *testing.T) {
	h := newHarness(t)
	_, out := h.do("GET", "/api/storage/info", adminKey, nil)
	backend := out["backend"].(map[string]any)
	if backend["database_url"] != "postgres://u:****@h/db" || backend["db_type"] != "postgresql" {
		t.Fatalf("backend: %v", backend)
	}
	var _ *gorm.DB = h.deps.DB
}
