package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/docstore"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

func TestSigV4MatchesPython(t *testing.T) {
	// golden values from CloudflareR2Client._aws_v4_headers on main at 2026-11-20T08:30:15Z
	r := &R2{AccountID: "acct", AccessKeyID: "AKID", SecretAccessKey: "SECRET", Bucket: "bkt", Prefix: "backups",
		Now: func() time.Time { return time.Date(2026, 11, 20, 8, 30, 15, 0, time.UTC) }}
	host := "acct.r2.cloudflarestorage.com"
	query, headers := r.sign("GET", "/bkt", map[string]string{"list-type": "2", "prefix": "backups/", "max-keys": "1000"}, nil, nil, host)
	if query != "list-type=2&max-keys=1000&prefix=backups%2F" ||
		headers["authorization"] != "AWS4-HMAC-SHA256 Credential=AKID/20261120/auto/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=a41f0b065a172e4d130fe3ea2ed8fd0f6cbc89750d813c34ac166739ff502b8d" {
		t.Fatalf("list %s %s", query, headers["authorization"])
	}
	_, headers = r.sign("PUT", "/bkt/backups/backup-x.tar.gz", nil, []byte("payload"), map[string]string{"content-type": "application/octet-stream", "x-amz-meta-trigger": "manual"}, host)
	if headers["authorization"] != "AWS4-HMAC-SHA256 Credential=AKID/20261120/auto/s3/aws4_request, SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date;x-amz-meta-trigger, Signature=0d783b0d4c9a20339692e8505c1e16606102fcf2fa3bad6a553699a77ba512ba" {
		t.Fatalf("put %s", headers["authorization"])
	}
}

func TestEncryptionIsOpenSSLCompatible(t *testing.T) {
	plain := []byte(strings.Repeat("backup payload 备份 ", 100))
	sealed, err := Encrypt(plain, "pass phrase")
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := Decrypt(sealed, "pass phrase"); err != nil || !bytes.Equal(opened, plain) {
		t.Fatalf("roundtrip %v", err)
	}
	if _, err := Decrypt(sealed, "wrong"); err == nil {
		t.Fatal("wrong passphrase accepted")
	}
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("no openssl")
	}
	run := func(args []string, input []byte) []byte {
		cmd := exec.Command("openssl", args...)
		cmd.Env = append(os.Environ(), "CHATGPT2API_BACKUP_PASSPHRASE=pass phrase")
		cmd.Stdin = bytes.NewReader(input)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("openssl %v: %v", args, err)
		}
		return out
	}
	// the exact commands of backup_service.py
	if out := run([]string{"enc", "-d", "-aes-256-cbc", "-pbkdf2", "-md", "sha256", "-pass", "env:CHATGPT2API_BACKUP_PASSPHRASE"}, sealed); !bytes.Equal(out, plain) {
		t.Fatal("openssl could not decrypt the Go output")
	}
	byOpenSSL := run([]string{"enc", "-aes-256-cbc", "-pbkdf2", "-salt", "-md", "sha256", "-pass", "env:CHATGPT2API_BACKUP_PASSPHRASE"}, plain)
	if opened, err := Decrypt(byOpenSSL, "pass phrase"); err != nil || !bytes.Equal(opened, plain) {
		t.Fatalf("Go could not decrypt openssl output: %v", err)
	}
}

// fakeR2 is an in-memory bucket.
type fakeR2 struct {
	mu      sync.Mutex
	objects map[string][]byte
	clock   int
	order   map[string]int
	authOK  bool
}

func (f *fakeR2) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("authorization"), "AWS4-HMAC-SHA256 Credential=AKID/") {
		w.WriteHeader(403)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/bkt")
	key = strings.TrimPrefix(key, "/")
	switch {
	case r.Method == "PUT":
		body, _ := io.ReadAll(r.Body)
		f.clock++
		f.objects[key], f.order[key] = body, f.clock
	case r.Method == "DELETE":
		delete(f.objects, key)
		w.WriteHeader(204)
	case r.Method == "GET" && key != "":
		body, ok := f.objects[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Write(body)
	default:
		keys := []string{}
		for k := range f.objects {
			if strings.HasPrefix(k, r.URL.Query().Get("prefix")) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var out strings.Builder
		out.WriteString("<ListBucketResult><IsTruncated>false</IsTruncated>")
		for _, k := range keys {
			fmt.Fprintf(&out, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-11-20T08:%02d:00.000Z</LastModified></Contents>", k, len(f.objects[k]), f.order[k])
		}
		out.WriteString("</ListBucketResult>")
		w.Write([]byte(out.String()))
	}
}

func newService(t *testing.T, config map[string]any) (*Service, *fakeR2) {
	dir := t.TempDir()
	repo, err := repository.Open("sqlite:///" + filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	docstore.Migrate(repo.DB())
	docs := docstore.New(repo.DB())
	docs.Save("proxies", map[string]any{"items": []any{}})
	configFile := filepath.Join(dir, "config.json")
	os.WriteFile(configFile, []byte(`{"auth-key":"x"}`), 0o644)
	images := filepath.Join(dir, "images", "2026", "11", "20")
	os.MkdirAll(images, 0o755)
	os.WriteFile(filepath.Join(images, "a.png"), []byte("png"), 0o644)
	fake := &fakeR2{objects: map[string][]byte{}, order: map[string]int{}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	store := settings.NewForTest(map[string]any{"backup": config}, "")
	s := &Service{Settings: store, Docs: docs,
		Sources: Sources{ConfigFile: configFile, ImagesDir: filepath.Join(dir, "images"), AppVersion: "test",
			Document: func(name string) []byte {
				var v any
				if ok, _ := docs.Load(name, &v); !ok {
					return nil
				}
				return jsonBytes(v)
			},
			Logs:       func() ([]byte, error) { return []byte(`{"id":"1"}` + "\n"), nil },
			ImageTasks: func() any { return map[string]any{"tasks": []any{}} },
			ImageIndex: func() (any, error) { return map[string]any{"items": map[string]any{"2026/11/20/a.png": map[string]any{}}}, nil },
			ImageTags:  func() (any, error) { return map[string]any{}, nil },
			Accounts:   func() (any, error) { return []any{map[string]any{"access_token": "t1"}, map[string]any{"access_token": "t2"}}, nil },
			AuthKeys:   func() (any, error) { return []any{map[string]any{"id": "k"}}, nil },
		},
		NewR2: func(settings map[string]any) *R2 {
			r := newR2(settings)
			r.Endpoint = server.URL
			return r
		}}
	return s, fake
}

func baseConfig() map[string]any {
	return map[string]any{"enabled": true, "account_id": "acct", "access_key_id": "AKID", "secret_access_key": "SECRET", "bucket": "bkt",
		"rotation_keep": 2, "include": map[string]any{"config": true, "cpa": true, "logs": true, "image_tasks": true, "accounts_snapshot": true,
			"auth_keys_snapshot": true, "images": true}}
}

func archiveNames(t *testing.T, payload []byte) []string {
	gz, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(gz)
	names := []string{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
	}
}

func TestRunUploadsDescribesAndRotates(t *testing.T) {
	s, fake := newService(t, baseConfig())
	for i := 0; i < 3; i++ {
		if _, err := s.Run("manual"); err != nil {
			t.Fatal(err)
		}
	}
	if len(fake.objects) != 2 {
		t.Fatalf("rotation kept %d", len(fake.objects))
	}
	items, err := s.List()
	if err != nil || len(items) != 2 || items[0]["encrypted"] != false {
		t.Fatalf("items %v err %v", items, err)
	}
	key := items[0]["key"].(string)
	downloaded, err := s.Download(key)
	if err != nil {
		t.Fatal(err)
	}
	names := archiveNames(t, downloaded["payload"].([]byte))
	want := []string{"backup-metadata.json", "config.json", "data/proxies.json", "data/logs.jsonl", "data/image_tasks.json", "data/image_index.json",
		"snapshots/accounts.json", "snapshots/auth_keys.json", "data/image_tags.json", "data/images/2026/11/20/a.png"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names %v", names)
	}
	detail, err := s.Detail(key)
	if err != nil || detail["trigger"] != "manual" || detail["app_version"] != "test" {
		t.Fatalf("detail %v %v", detail, err)
	}
	snapshots := detail["snapshots"].([]map[string]any)
	if snapshots[0]["name"] != "accounts" || snapshots[0]["count"] != 2 {
		t.Fatalf("snapshots %v", snapshots)
	}
	state := s.Status()
	if state["last_status"] != "success" || state["last_object_key"] == nil || state["running"] != false {
		t.Fatalf("state %v", state)
	}
	if err := s.Delete(key); err != nil || len(fake.objects) != 1 {
		t.Fatalf("delete %v", err)
	}
}

func TestEncryptedBackupNeedsAPassphraseAndDecrypts(t *testing.T) {
	config := baseConfig()
	config["encrypt"] = true
	s, _ := newService(t, config)
	if _, err := s.Run("manual"); err == nil || err.Error() != "已启用备份加密，但未设置加密口令" {
		t.Fatalf("err %v", err)
	}
	if state := s.Status(); state["last_status"] != "error" {
		t.Fatalf("state %v", state)
	}
	config["passphrase"] = "secret pass"
	s.Settings = settings.NewForTest(map[string]any{"backup": config}, "")
	result, err := s.Run("manual")
	if err != nil || result["encrypted"] != true || !strings.HasSuffix(result["key"].(string), ".tar.gz.enc") {
		t.Fatalf("result %v err %v", result, err)
	}
	downloaded, err := s.Download(result["key"].(string))
	if err != nil || !strings.HasSuffix(downloaded["name"].(string), ".tar.gz") || len(archiveNames(t, downloaded["payload"].([]byte))) == 0 {
		t.Fatalf("downloaded %v err %v", downloaded["name"], err)
	}
	if public := s.PublicSettings(); public["passphrase"] != "********" || public["secret_access_key"] != "********" {
		t.Fatalf("public %v", public)
	}
}

func TestValidationAndSchedule(t *testing.T) {
	s, fake := newService(t, map[string]any{"enabled": true, "bucket": "bkt"})
	if _, err := s.Run("manual"); err == nil || !strings.Contains(err.Error(), "缺少 Account ID、Access Key ID、Secret Access Key") {
		t.Fatalf("err %v", err)
	}
	if items, err := s.List(); err != nil || len(items) != 0 {
		t.Fatalf("unconfigured list %v %v", items, err)
	}
	s2, fake2 := newService(t, baseConfig())
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	s2.Now = func() time.Time { return now }
	s2.RunScheduled()
	s2.RunScheduled() // within the interval: no second backup
	if len(fake2.objects) != 1 {
		t.Fatalf("scheduled %d", len(fake2.objects))
	}
	now = now.Add(361 * time.Minute)
	s2.RunScheduled()
	if len(fake2.objects) != 2 {
		t.Fatalf("after interval %d", len(fake2.objects))
	}
	_ = fake
}
