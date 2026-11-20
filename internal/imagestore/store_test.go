package imagestore

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

// Ported from test_image_storage_service.py, test_image_delete.py, test_image_list_pagination.py
// and test_image_retention.py.

func pngBytes(t *testing.T, seed uint8, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{seed, 0, 0, 255})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

type fixture struct {
	svc      *Service
	data     string
	images   string
	thumbs   string
	settings map[string]any
}

func newFixture(t *testing.T, config map[string]any) *fixture {
	t.Helper()
	dir := t.TempDir()
	repo, err := repository.Open("sqlite:///" + filepath.Join(dir, "images.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	if err := Migrate(repo.DB()); err != nil {
		t.Fatal(err)
	}
	if config == nil {
		config = map[string]any{}
	}
	if _, ok := config["base_url"]; !ok {
		config["base_url"] = "http://app.test"
	}
	store := settings.NewForTest(config, "")
	data := filepath.Join(dir, "data")
	return &fixture{svc: New(repo.DB(), store, data), data: data, images: filepath.Join(data, "images"), thumbs: filepath.Join(data, "image_thumbnails"), settings: config}
}

func (f *fixture) write(t *testing.T, dir, rel string, payload []byte) string {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	return full
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// fakeDAV is an in-memory WebDAV server.
type fakeDAV struct {
	mu      sync.Mutex
	files   map[string][]byte
	deleted []string
}

func newDAV(t *testing.T) (*fakeDAV, string) {
	dav := &fakeDAV{files: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dav.mu.Lock()
		defer dav.mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/")
		switch r.Method {
		case "MKCOL":
			w.WriteHeader(201)
		case "PUT":
			body, _ := io.ReadAll(r.Body)
			dav.files[key] = body
			w.WriteHeader(201)
		case "GET":
			body, ok := dav.files[key]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Write(body)
		case "DELETE":
			dav.deleted = append(dav.deleted, key)
			if _, ok := dav.files[key]; !ok {
				w.WriteHeader(404)
				return
			}
			delete(dav.files, key)
			w.WriteHeader(204)
		}
	}))
	t.Cleanup(server.Close)
	return dav, server.URL
}

func webdavConfig(url, mode, public string) map[string]any {
	return map[string]any{"image_storage": map[string]any{"enabled": true, "mode": mode, "webdav_url": url, "webdav_password": "secret",
		"webdav_root_path": "chatgpt2api/images", "public_base_url": public}}
}

func relOf(t *testing.T, url string) string {
	t.Helper()
	i := strings.Index(url, "/images/")
	if i < 0 {
		t.Fatalf("url %s", url)
	}
	return url[i+len("/images/"):]
}

func TestLocalModeSavesToLocalDirectory(t *testing.T) {
	f := newFixture(t, nil)
	url, err := f.svc.Save(pngBytes(t, 255, 2, 2), "http://app.test")
	if err != nil {
		t.Fatal(err)
	}
	rel := relOf(t, url)
	if !exists(filepath.Join(f.images, rel)) || url != "http://app.test/images/"+rel {
		t.Fatalf("url %s", url)
	}
	items, _ := f.svc.ListItems("http://app.test", "", "", false)
	if len(items) != 1 || items[0]["storage"] != "local" || items[0]["width"] == nil {
		t.Fatalf("items %v", items)
	}
}

func TestWebDAVModeUploadsWithoutLocalFile(t *testing.T) {
	dav, url := newDAV(t)
	f := newFixture(t, webdavConfig(url, "webdav", ""))
	saved, err := f.svc.Save(pngBytes(t, 1, 2, 2), "http://app.test")
	if err != nil {
		t.Fatal(err)
	}
	rel := relOf(t, saved)
	if exists(filepath.Join(f.images, rel)) {
		t.Fatal("webdav mode wrote a local file")
	}
	remote, ok := dav.files["chatgpt2api/images/"+rel]
	if !ok {
		t.Fatalf("not uploaded: %v", dav.files)
	}
	payload, err := f.svc.GetBytes(rel)
	if err != nil || !bytes.Equal(payload, remote) {
		t.Fatalf("get bytes %v", err)
	}
	// the WebDAV copy keeps the entry through a reconcile
	items, _ := f.svc.ListItems("", "", "", true)
	if len(items) != 1 || items[0]["storage"] != "webdav" {
		t.Fatalf("items %v", items)
	}
	if removed, err := f.svc.DeleteMany([]string{rel}); err != nil || removed != 1 || len(dav.files) != 0 {
		t.Fatalf("delete %d %v %v", removed, err, dav.files)
	}
}

func TestListItemsIgnoresNonImageFiles(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, f.images, "2026/05/07/sample.png", pngBytes(t, 1, 2, 2))
	f.write(t, f.images, ".DS_Store", []byte("not an image"))
	f.write(t, f.images, "2026/.DS_Store", []byte("not an image"))
	items, _ := f.svc.ListItems("http://app.test", "", "", false)
	if len(items) != 1 || items[0]["rel"] != "2026/05/07/sample.png" || items[0]["storage"] != "local" || items[0]["date"] != "2026-05-07" {
		t.Fatalf("items %v", items)
	}
}

func TestBothModeSavesToLocalAndWebDAV(t *testing.T) {
	dav, url := newDAV(t)
	f := newFixture(t, webdavConfig(url, "both", "https://cdn.example.test/images"))
	saved, err := f.svc.Save(pngBytes(t, 1, 2, 2), "http://app.test")
	if err != nil {
		t.Fatal(err)
	}
	rel := relOf(t, saved)
	if !exists(filepath.Join(f.images, rel)) || dav.files["chatgpt2api/images/"+rel] == nil || saved != "https://cdn.example.test/images/"+rel {
		t.Fatalf("saved %s", saved)
	}
	result, err := f.svc.SyncAll()
	if err != nil || result["skipped"] != 1 || result["uploaded"] != 0 {
		t.Fatalf("sync %v %v", result, err)
	}
}

func TestWebDAVFailureFailsTheSave(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	f := newFixture(t, webdavConfig(server.URL, "webdav", ""))
	if _, err := f.svc.Save(pngBytes(t, 1, 2, 2), ""); err == nil || !strings.Contains(err.Error(), "WebDAV MKCOL failed: HTTP 500") {
		t.Fatalf("err %v", err)
	}
}

func TestTestWebDAVWritesAndDeletesProbeFile(t *testing.T) {
	dav, url := newDAV(t)
	f := newFixture(t, webdavConfig(url, "webdav", ""))
	if result := f.svc.TestWebDAV(); result["ok"] != true {
		t.Fatalf("result %v", result)
	}
	if len(dav.deleted) != 1 || !strings.HasSuffix(dav.deleted[0], ".chatgpt2api_webdav_test.txt") {
		t.Fatalf("deleted %v", dav.deleted)
	}
	if result := newFixture(t, nil).svc.TestWebDAV(); result["error"] != "WebDAV URL is required" {
		t.Fatalf("result %v", result)
	}
}

func TestSyncNeedsWebDAV(t *testing.T) {
	_, err := newFixture(t, nil).svc.SyncAll()
	if storageErr, ok := err.(*StorageError); !ok || storageErr.Message != "WebDAV 图片存储未启用" {
		t.Fatalf("err %v", err)
	}
}

// ---- delete ----

func deleteFixture(t *testing.T) (*fixture, []string) {
	f := newFixture(t, nil)
	rels := []string{}
	for _, day := range []string{"1", "2"} {
		for n := 0; n < 3; n++ {
			rel := "2026/11/0" + day + "/img" + string(rune('0'+n)) + ".png"
			rels = append(rels, rel)
			f.write(t, f.images, rel, []byte("png"))
			f.write(t, f.thumbs, rel+".png", []byte("thumb"))
		}
	}
	f.svc.ListItems("", "", "", true)
	f.svc.SetTags(rels[0], []string{"keep"})
	f.svc.SetTags(rels[5], []string{"x"})
	return f, rels
}

func indexRels(t *testing.T, f *fixture) []string {
	var rows []indexRow
	f.svc.db.Find(&rows)
	out := []string{}
	for _, row := range rows {
		out = append(out, row.Rel)
	}
	sort.Strings(out)
	return out
}

func TestDeletesFilesThumbnailsIndexAndTags(t *testing.T) {
	f, rels := deleteFixture(t)
	doomed := rels[:4]
	removed, err := f.svc.DeleteImages(append(append([]string{}, doomed...), "../outside.png", "2026/11/09/missing.png"), "", "", false)
	if err != nil || removed != 4 {
		t.Fatalf("removed %d err %v", removed, err)
	}
	for _, rel := range doomed {
		if exists(filepath.Join(f.images, rel)) || exists(filepath.Join(f.thumbs, rel+".png")) {
			t.Fatalf("%s still there", rel)
		}
	}
	for _, rel := range rels[4:] {
		if !exists(filepath.Join(f.images, rel)) {
			t.Fatalf("%s deleted", rel)
		}
	}
	if got := indexRels(t, f); !reflect.DeepEqual(got, rels[4:]) {
		t.Fatalf("index %v", got)
	}
	if tags, _ := f.svc.Tags(); !reflect.DeepEqual(tags, map[string][]string{rels[5]: {"x"}}) {
		t.Fatalf("tags %v", tags)
	}
	if exists(filepath.Join(f.images, "2026/11/01")) {
		t.Fatal("empty day folder kept")
	}
}

func TestDeleteAllMatchingUsesTheDateRange(t *testing.T) {
	f, rels := deleteFixture(t)
	removed, err := f.svc.DeleteImages(nil, "2026-11-02", "2026-11-02", true)
	if err != nil || removed != 3 || !reflect.DeepEqual(indexRels(t, f), rels[:3]) {
		t.Fatalf("removed %d err %v index %v", removed, err, indexRels(t, f))
	}
}

func TestSingleDeleteStillReportsResult(t *testing.T) {
	f, rels := deleteFixture(t)
	if n, _ := f.svc.DeleteMany([]string{rels[2]}); n != 1 {
		t.Fatal("first delete")
	}
	if n, _ := f.svc.DeleteMany([]string{rels[2]}); n != 0 {
		t.Fatal("second delete")
	}
}

func TestSafeRelMatchesPython(t *testing.T) {
	// expected values from the pathlib version in test_image_delete.py
	cases := map[string]string{
		"2026/11/01/a.png": "2026/11/01/a.png", "/2026/11/01/a.png": "2026/11/01/a.png", "2026//11/./01/a.png": "2026/11/01/a.png",
		"2026\\11\\01\\a.png": "2026/11/01/a.png", "  2026/11/01/a.png  ": "2026/11/01/a.png", "a.png": "a.png", "./a.png": "a.png",
		"a/../b.png": "rejected", "../a.png": "rejected", "..": "rejected", ".": ".", "./": ".", "//": "rejected", "": "rejected",
		"   ": "rejected", "a/b/": "a/b", "a/.../b.png": "a/.../b.png", "a/..b/c.png": "a/..b/c.png", "2026/11/01/a.PNG": "2026/11/01/a.PNG",
		"a/./../b.png": "rejected",
	}
	for input, want := range cases {
		got, err := SafeRel(input)
		if err != nil {
			got = "rejected"
		}
		if got != want {
			t.Errorf("%q: got %q want %q", input, got, want)
		}
	}
}

func TestReconcileDropsMissingFilesAndAddsNewOnes(t *testing.T) {
	f, rels := deleteFixture(t)
	os.Remove(filepath.Join(f.images, rels[0]))
	f.write(t, f.images, "2026/11/03/fresh.png", []byte("png"))
	items, _ := f.svc.ListItems("", "", "", true)
	got := []string{}
	for _, it := range items {
		got = append(got, it["path"].(string))
	}
	sort.Strings(got)
	want := append(append([]string{}, rels[1:]...), "2026/11/03/fresh.png")
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(indexRels(t, f), want) {
		t.Fatalf("got %v", got)
	}
}

// ---- list ----

func listFixture(t *testing.T) (*fixture, []string) {
	f := newFixture(t, nil)
	paths := []string{}
	for seed := 0; seed < 5; seed++ {
		moment := time.Date(2026, 11, 10, 0, 0, seed, 0, time.Local)
		f.svc.now = func() time.Time { return moment }
		url, err := f.svc.Save(pngBytes(t, uint8(seed), 4, 4), "http://app.test")
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, relOf(t, url))
	}
	f.svc.now = time.Now
	return f, paths
}

func pathsOf(items any) []string {
	out := []string{}
	for _, it := range items.([]item) {
		out = append(out, it["path"].(string))
	}
	return out
}

func TestPageReturnsOnlyThatPageNewestFirst(t *testing.T) {
	f, paths := listFixture(t)
	data, _ := f.svc.ListImages("http://app.test", "", "", nil, 2, 2, false)
	if data["total"] != 5 || data["page"] != 2 || data["page_size"] != 2 || data["page_count"] != 3 {
		t.Fatalf("paging %v", data)
	}
	if got := pathsOf(data["items"]); !reflect.DeepEqual(got, []string{paths[2], paths[1]}) {
		t.Fatalf("items %v", got)
	}
	first := data["items"].([]item)[0]
	if first["thumbnail_url"] != "http://app.test/image-thumbnails/"+paths[2] || first["url"] != "http://app.test/images/"+paths[2] {
		t.Fatalf("urls %v", first)
	}
	groups := data["groups"].([]map[string]any)
	if len(groups) != 1 || groups[0]["date"] != "2026-11-10" {
		t.Fatalf("groups %v", groups)
	}
}

func TestPagePastTheEndIsClampedToTheLastPage(t *testing.T) {
	f, paths := listFixture(t)
	data, _ := f.svc.ListImages("http://app.test", "", "", nil, 9, 2, false)
	if data["page"] != 3 || !reflect.DeepEqual(pathsOf(data["items"]), []string{paths[0]}) {
		t.Fatalf("data %v", data)
	}
}

func TestPageSizeIsCapped(t *testing.T) {
	f, _ := listFixture(t)
	if data, _ := f.svc.ListImages("http://app.test", "", "", nil, 1, 10000, false); data["page_size"] != pageSizeMax {
		t.Fatalf("page_size %v", data["page_size"])
	}
}

func TestTagsFilterRequiresEveryTag(t *testing.T) {
	f, paths := listFixture(t)
	f.svc.SetTags(paths[0], []string{"猫", "夜景"})
	f.svc.SetTags(paths[1], []string{"猫"})
	data, _ := f.svc.ListImages("http://app.test", "", "", []string{"猫", "夜景"}, 1, 10, false)
	items := data["items"].([]item)
	if data["total"] != 1 || !reflect.DeepEqual(items[0]["tags"], []string{"猫", "夜景"}) {
		t.Fatalf("data %v", data)
	}
	if tags, _ := f.svc.AllTags(); !reflect.DeepEqual(tags, []string{"猫", "夜景"}) {
		t.Fatalf("all tags %v", tags)
	}
}

func TestWithoutPageReturnsEverything(t *testing.T) {
	f, _ := listFixture(t)
	data, _ := f.svc.ListImages("http://app.test", "", "", nil, 0, 48, false)
	if _, paged := data["page"]; paged || len(data["items"].([]item)) != 5 {
		t.Fatalf("data %v", data)
	}
}

func TestPathsListsEveryMatchAcrossPages(t *testing.T) {
	f, paths := listFixture(t)
	f.svc.SetTags(paths[0], []string{"猫"})
	f.svc.SetTags(paths[3], []string{"猫"})
	if got, _ := f.svc.ImagePaths("", "", []string{"猫"}); !reflect.DeepEqual(got, []string{paths[3], paths[0]}) {
		t.Fatalf("paths %v", got)
	}
}

func TestDiskChangesShowUpOnRefreshNotOnEveryRequest(t *testing.T) {
	f, paths := listFixture(t)
	f.svc.ListImages("http://app.test", "", "", nil, 0, 48, false) // first reconcile
	os.Remove(filepath.Join(f.images, paths[0]))
	f.write(t, f.images, "2026/01/01/stray.png", pngBytes(t, 9, 4, 4))
	cached, _ := f.svc.ListImages("http://app.test", "", "", nil, 0, 48, false)
	if got := pathsOf(cached["items"]); !contains(got, paths[0]) || contains(got, "2026/01/01/stray.png") {
		t.Fatalf("cached %v", got)
	}
	fresh, _ := f.svc.ListImages("http://app.test", "", "", nil, 0, 48, true)
	if got := pathsOf(fresh["items"]); contains(got, paths[0]) || !contains(got, "2026/01/01/stray.png") {
		t.Fatalf("fresh %v", got)
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestDeleteTagCountsImages(t *testing.T) {
	f, paths := listFixture(t)
	f.svc.SetTags(paths[0], []string{"a", "b"})
	f.svc.SetTags(paths[1], []string{"a"})
	if n, _ := f.svc.DeleteTag("a"); n != 2 {
		t.Fatalf("count %d", n)
	}
	if tags, _ := f.svc.Tags(); !reflect.DeepEqual(tags, map[string][]string{paths[0]: {"b"}}) {
		t.Fatalf("tags %v", tags)
	}
	if saved, _ := f.svc.SetTags(paths[0], []string{" ", "x", "x "}); !reflect.DeepEqual(saved, []string{"x"}) {
		t.Fatalf("saved %v", saved)
	}
}

// ---- retention ----

func retentionFixture(t *testing.T, days int) *fixture {
	return newFixture(t, map[string]any{"image_retention_days": days})
}

func (f *fixture) image(t *testing.T, rel, mtimeDay string) string {
	full := f.write(t, f.images, rel, []byte("png"))
	if mtimeDay != "" {
		stamp, _ := time.ParseInLocation("2006-01-02 15:04", mtimeDay+" 12:00", time.Local)
		os.Chtimes(full, stamp, stamp)
	}
	return full
}

func TestKeepsTheLastNCalendarDaysIncludingToday(t *testing.T) {
	f := retentionFixture(t, 3)
	today := time.Date(2026, 11, 20, 9, 0, 0, 0, time.Local)
	if keep := f.svc.settings.ImageRetentionKeepFrom(today); keep != "2026-11-18" {
		t.Fatalf("keep from %s", keep)
	}
	kept := []string{f.image(t, "2026/11/18/a.png", ""), f.image(t, "2026/11/20/b.png", "")}
	doomed := []string{f.image(t, "2026/11/17/c.png", "2026-11-20"), f.image(t, "2026/10/31/d.png", "")}
	if n := f.svc.CleanupOldImages(today); n != 2 {
		t.Fatalf("removed %d", n)
	}
	for _, p := range kept {
		if !exists(p) {
			t.Fatalf("%s removed", p)
		}
	}
	for _, p := range doomed {
		if exists(p) {
			t.Fatalf("%s kept", p)
		}
	}
	if exists(filepath.Join(f.images, "2026/10")) {
		t.Fatal("empty month folder kept")
	}
}

func TestImagesOutsideDateFoldersUseTheirModifiedDay(t *testing.T) {
	f := retentionFixture(t, 3)
	old := f.image(t, "legacy/old.png", "2026-11-01")
	recent := f.image(t, "legacy/new.png", "2026-11-19")
	if n := f.svc.CleanupOldImages(time.Date(2026, 11, 20, 9, 0, 0, 0, time.Local)); n != 1 || exists(old) || !exists(recent) {
		t.Fatalf("removed %d", n)
	}
}

func TestOneDayKeepsOnlyToday(t *testing.T) {
	f := retentionFixture(t, 1)
	if keep := f.svc.settings.ImageRetentionKeepFrom(time.Date(2026, 11, 20, 9, 0, 0, 0, time.Local)); keep != "2026-11-20" {
		t.Fatalf("keep from %s", keep)
	}
}

// ---- thumbnails, zip, gif ----

func TestThumbnailFitsIn320AndIsReused(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, f.images, "2026/11/20/wide.png", pngBytes(t, 7, 640, 200))
	thumb, err := f.svc.EnsureThumbnail("2026/11/20/wide.png")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(thumb)
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || config.Width != 320 || config.Height != 100 {
		t.Fatalf("thumbnail %v %v", config, err)
	}
	if _, err := f.svc.EnsureThumbnail("2026/11/20/missing.png"); err == nil || err.(*HTTPError).Status != 404 {
		t.Fatalf("missing image: %v", err)
	}
	f.write(t, f.images, "2026/11/20/broken.png", []byte("not a png"))
	if _, err := f.svc.EnsureThumbnail("2026/11/20/broken.png"); err == nil || err.(*HTTPError).Status != 422 {
		t.Fatalf("broken image: %v", err)
	}
}

func TestThumbnailFitMatchesPIL(t *testing.T) {
	for _, c := range [][4]int{{640, 200, 320, 100}, {200, 640, 100, 320}, {1536, 1024, 320, 213}, {1024, 1536, 213, 320}, {100, 50, 100, 50}, {3000, 1, 320, 1}} {
		w, h, _ := thumbnailFit(c[0], c[1], 320)
		if w != c[2] || h != c[3] {
			t.Errorf("%dx%d -> %dx%d, want %dx%d", c[0], c[1], w, h, c[2], c[3])
		}
	}
}

func TestZipRenamesDuplicatesAndNeedsOneFile(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, f.images, "2026/11/19/a.png", []byte("one"))
	f.write(t, f.images, "2026/11/20/a.png", []byte("two"))
	payload, err := f.svc.Zip([]string{"2026/11/19/a.png", "2026/11/20/a.png", "2026/11/20/missing.png"})
	if err != nil || !bytes.Contains(payload, []byte("a_2.png")) {
		t.Fatalf("zip %v", err)
	}
	if _, err := f.svc.Zip([]string{"2026/11/20/missing.png"}); err == nil || err.(*HTTPError).Detail != "no images found" {
		t.Fatalf("err %v", err)
	}
}

func TestSavedGIFUsesGIFSuffix(t *testing.T) {
	f := newFixture(t, nil)
	url, err := f.svc.Save([]byte("GIF89a fake"), "")
	if err != nil || !strings.HasSuffix(url, ".gif") {
		t.Fatalf("url %s err %v", url, err)
	}
}
