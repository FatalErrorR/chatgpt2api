package imagestore

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io/fs"
	"log"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	xdraw "golang.org/x/image/draw"

	"github.com/basketikun/chatgpt2api/internal/imagefit"
)

const (
	thumbnailSize    = 320
	pageSizeMax      = 200
	cleanupEvery     = 30 * time.Minute
	saveCleanupEvery = time.Minute
)

// ---- thumbnails ----

// ThumbnailPath is _thumbnail_path: data/image_thumbnails/<rel>.png.
func (s *Service) ThumbnailPath(rel string) (string, error) {
	safe, err := SafeRel(rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(s.ThumbnailsDir(), filepath.FromSlash(safe)+".png"), nil
}

// thumbnailFit is PIL's Image.thumbnail size rule (keep the ratio, only shrink, at least 1px).
func thumbnailFit(width, height, box int) (int, int, bool) {
	if box >= width && box >= height {
		return width, height, false
	}
	aspect := float64(width) / float64(height)
	x, y := box, box
	pick := func(value float64, key func(int) float64) int {
		lo, hi := int(math.Floor(value)), int(math.Ceil(value))
		best := lo
		if key(hi) < key(lo) {
			best = hi
		}
		return max(best, 1)
	}
	if float64(x)/float64(y) >= aspect {
		x = pick(float64(y)*aspect, func(n int) float64 { return math.Abs(aspect - float64(n)/float64(y)) })
	} else {
		y = pick(float64(x)/aspect, func(n int) float64 {
			if n == 0 {
				return 0
			}
			return math.Abs(aspect - float64(x)/float64(n))
		})
	}
	return x, y, true
}

// EnsureThumbnail is ensure_thumbnail: reuse a thumbnail newer than the image, else make one.
func (s *Service) EnsureThumbnail(rel string) (string, error) {
	target, err := s.ThumbnailPath(rel)
	if err != nil {
		return "", err
	}
	local, err := s.HasLocal(rel)
	if err != nil {
		return "", err
	}
	var sourceTime time.Time
	source := ""
	if local {
		source, _ = s.localPath(rel)
		if info, err := os.Stat(source); err == nil {
			sourceTime = info.ModTime()
		}
	}
	if info, err := os.Stat(target); err == nil && (sourceTime.IsZero() || !info.ModTime().Before(sourceTime)) {
		return target, nil
	}
	os.MkdirAll(filepath.Dir(target), 0o755)
	var data []byte
	if source != "" {
		data, err = os.ReadFile(source)
	} else {
		data, err = s.GetBytes(rel)
	}
	if err != nil {
		if _, ok := IsHTTPError(err); ok {
			return "", err
		}
		return "", &HTTPError{Status: 422, Detail: "failed to create thumbnail"}
	}
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", &HTTPError{Status: 422, Detail: "failed to create thumbnail"}
	}
	if format == "jpeg" {
		img = imagefit.Orient(img, data)
	}
	bounds := img.Bounds()
	var out draw.Image
	if w, h, resize := thumbnailFit(bounds.Dx(), bounds.Dy(), thumbnailSize); resize {
		out = image.NewNRGBA(image.Rect(0, 0, w, h))
		xdraw.CatmullRom.Scale(out, out.Bounds(), img, bounds, draw.Src, nil)
	} else {
		out = image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
		draw.Draw(out, out.Bounds(), img, bounds.Min, draw.Src)
	}
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, out); err != nil {
		return "", &HTTPError{Status: 422, Detail: "failed to create thumbnail"}
	}
	if err := os.WriteFile(target, buf.Bytes(), 0o644); err != nil {
		return "", &HTTPError{Status: 422, Detail: "failed to create thumbnail"}
	}
	return target, nil
}

// ---- listing ----

func (s *Service) filtered(baseURL, startDate, endDate string, tags []string, refresh bool) ([]item, map[string][]string, error) {
	allTags, err := s.Tags()
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.ListItems(baseURL, startDate, endDate, refresh)
	if err != nil {
		return nil, nil, err
	}
	wanted := []string{}
	for _, tag := range tags {
		if tag != "" {
			wanted = append(wanted, tag)
		}
	}
	if len(wanted) == 0 {
		return rows, allTags, nil
	}
	kept := []item{}
	for _, row := range rows {
		have := map[string]bool{}
		for _, tag := range allTags[row["path"].(string)] {
			have[tag] = true
		}
		all := true
		for _, tag := range wanted {
			all = all && have[tag]
		}
		if all {
			kept = append(kept, row)
		}
	}
	return kept, allTags, nil
}

// ListImages is list_images: everything, or one page when page > 0, grouped by date.
func (s *Service) ListImages(baseURL, startDate, endDate string, tags []string, page, pageSize int, refresh bool) (map[string]any, error) {
	rows, allTags, err := s.filtered(baseURL, startDate, endDate, tags, refresh)
	if err != nil {
		return nil, err
	}
	total := len(rows)
	result := map[string]any{}
	if page > 0 {
		pageSize = max(1, min(pageSizeMax, pageSize))
		pageCount := max(1, (total+pageSize-1)/pageSize)
		page = min(page, pageCount)
		rows = rows[min(total, (page-1)*pageSize):min(total, page*pageSize)]
		result["page"], result["page_size"], result["page_count"] = page, pageSize, pageCount
	}
	base := strings.TrimRight(baseURL, "/")
	items := make([]item, 0, len(rows))
	groups := []map[string]any{}
	groupIndex := map[string]int{}
	for _, row := range rows {
		rel := row["path"].(string)
		if u, _ := row["url"].(string); u == "" {
			row["url"] = base + "/images/" + rel
		}
		row["thumbnail_url"] = base + "/image-thumbnails/" + rel
		tags := allTags[rel]
		if tags == nil {
			tags = []string{}
		}
		row["tags"] = tags
		items = append(items, row)
		day := fmt.Sprint(row["date"])
		if i, ok := groupIndex[day]; ok {
			groups[i]["items"] = append(groups[i]["items"].([]item), row)
		} else {
			groupIndex[day] = len(groups)
			groups = append(groups, map[string]any{"date": day, "items": []item{row}})
		}
	}
	result["items"], result["groups"], result["total"] = items, groups, total
	return result, nil
}

// ImagePaths is list_image_paths.
func (s *Service) ImagePaths(startDate, endDate string, tags []string) ([]string, error) {
	rows, _, err := s.filtered("", startDate, endDate, tags, false)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(rows))
	for _, row := range rows {
		paths = append(paths, row["path"].(string))
	}
	return paths, nil
}

// ---- deleting ----

// cleanupEmptyDirs removes empty folders below root, deepest first (root itself too, as Python).
func cleanupEmptyDirs(root string) {
	dirs := []string{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != root {
			dirs = append(dirs, p)
		}
		return nil
	})
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], string(filepath.Separator)) > strings.Count(dirs[j], string(filepath.Separator))
	})
	for _, dir := range dirs {
		os.Remove(dir)
	}
}

// deleteRels is _delete_rels: images, their thumbnails and tags.
func (s *Service) deleteRels(rels []string) (int, error) {
	if len(rels) == 0 {
		return 0, nil
	}
	removed, err := s.DeleteMany(rels)
	if err != nil {
		return removed, err
	}
	for _, rel := range rels {
		safe, err := SafeRel(rel)
		if err != nil {
			continue
		}
		thumb := filepath.Join(s.ThumbnailsDir(), filepath.FromSlash(safe))
		for _, candidate := range []string{thumb + ".png", thumb} {
			if isFile(candidate) {
				os.Remove(candidate)
			}
		}
	}
	if err := s.RemoveTagsMany(rels); err != nil {
		return removed, err
	}
	cleanupEmptyDirs(s.ImagesDir())
	cleanupEmptyDirs(s.ThumbnailsDir())
	return removed, nil
}

// DeleteImages is delete_images: the given paths, or every image in the date range.
func (s *Service) DeleteImages(paths []string, startDate, endDate string, allMatching bool) (int, error) {
	targets := paths
	if allMatching {
		rows, err := s.ListItems("", startDate, endDate, false)
		if err != nil {
			return 0, err
		}
		targets = make([]string, 0, len(rows))
		for _, row := range rows {
			targets = append(targets, row["path"].(string))
		}
	}
	root, _ := filepath.Abs(s.ImagesDir())
	rels := []string{}
	for _, target := range targets {
		full := filepath.Clean(filepath.Join(root, filepath.FromSlash(target)))
		if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
			continue
		}
		rels = append(rels, target)
	}
	return s.deleteRels(rels)
}

// Zip is download_images_zip: local files only; duplicate names get _2, _3, …
func (s *Service) Zip(paths []string) ([]byte, error) {
	root, _ := filepath.Abs(s.ImagesDir())
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	used := map[string]bool{}
	added := 0
	for _, item := range paths {
		safe, err := SafeRel(item)
		if err != nil {
			return nil, err
		}
		full := filepath.Join(root, filepath.FromSlash(safe))
		if (full != root && !strings.HasPrefix(full, root+string(filepath.Separator))) || !isFile(full) {
			continue
		}
		payload, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		name := filepath.Base(full)
		if used[name] {
			ext := filepath.Ext(name)
			stem := strings.TrimSuffix(name, ext)
			counter := 2
			for used[stem+"_"+strconv.Itoa(counter)+ext] {
				counter++
			}
			name = stem + "_" + strconv.Itoa(counter) + ext
		}
		used[name] = true
		entry, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
		if err != nil {
			return nil, err
		}
		entry.Write(payload)
		added++
	}
	writer.Close()
	if added == 0 {
		return nil, &HTTPError{Status: 404, Detail: "no images found"}
	}
	return buf.Bytes(), nil
}

// ---- storage and cleanup ----

func diskUsage(dir string) (total, used, free uint64) {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return 0, 0, 0
	}
	block := uint64(st.Bsize)
	return st.Blocks * block, (st.Blocks - st.Bfree) * block, st.Bavail * block
}

const mb = 1024 * 1024

// StorageStats is storage_stats.
func (s *Service) StorageStats() map[string]any {
	total, used, free := diskUsage(s.ImagesDir())
	count, size := 0, int64(0)
	for _, file := range s.walkImages(false) {
		count++
		size += file.info.Size()
	}
	today := s.now()
	return map[string]any{
		"disk_total_mb": total / mb, "disk_used_mb": used / mb, "disk_free_mb": free / mb,
		"image_count": count, "image_size_mb": size / mb, "image_size_bytes": size,
		"retention_days": s.settings.ImageRetentionDays(), "retention_keep_from": s.settings.ImageRetentionKeepFrom(today),
		"today": today.Format("2006-01-02"),
	}
}

// DeleteToTarget is delete_to_target: delete the oldest .png/.gif until target_free_mb is free.
func (s *Service) DeleteToTarget(targetFreeMB int64, dryRun bool) (map[string]any, error) {
	_, _, freeBytes := diskUsage(s.ImagesDir())
	currentFree := int64(freeBytes / mb)
	if currentFree >= targetFreeMB && !dryRun {
		return map[string]any{"removed": 0, "current_free_mb": currentFree, "target_free_mb": targetFreeMB, "done": true}, nil
	}
	files := []diskFile{}
	for _, file := range s.walkImages(false) {
		if ext := strings.ToLower(path.Ext(file.rel)); ext == ".png" || ext == ".gif" {
			files = append(files, file)
		}
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].info.ModTime().Before(files[j].info.ModTime()) })
	removed, freed := 0, int64(0)
	doomed := []string{}
	for _, file := range files {
		if currentFree+freed/mb >= targetFreeMB {
			break
		}
		freed += file.info.Size()
		removed++
		doomed = append(doomed, file.rel)
	}
	if !dryRun {
		if _, err := s.deleteRels(doomed); err != nil {
			return nil, err
		}
	}
	after := currentFree + freed/mb
	return map[string]any{"removed": removed, "freed_mb": freed / mb, "target_free_mb": targetFreeMB,
		"current_free_mb": after, "done": after >= targetFreeMB, "dry_run": dryRun}, nil
}

// CleanupOldImages is config.cleanup_old_images: delete the days before the retention window
// (by the YYYY/MM/DD folder, else by the file's mtime), then empty folders.
func (s *Service) CleanupOldImages(today time.Time) int {
	keepFrom := s.settings.ImageRetentionKeepFrom(today)
	removed := 0
	for _, file := range s.walkImages(false) {
		parts := strings.Split(file.rel, "/")
		day := ""
		if len(parts) >= 4 && isDigits(parts[0]) && isDigits(parts[1]) && isDigits(parts[2]) {
			day = parts[0] + "-" + parts[1] + "-" + parts[2]
		} else {
			day = file.info.ModTime().Format("2006-01-02")
		}
		if day < keepFrom && os.Remove(file.full) == nil {
			removed++
		}
	}
	cleanupEmptyDirs(s.ImagesDir())
	return removed
}

func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// maybeCleanupOld runs the retention cleanup before a save, at most once a minute (Python runs it
// on every save; the result is the same, the folder walk just happens less often).
func (s *Service) maybeCleanupOld() {
	s.seqMu.Lock()
	due := time.Since(s.lastCleanup) >= saveCleanupEvery
	if due {
		s.lastCleanup = time.Now()
	}
	s.seqMu.Unlock()
	if due {
		s.CleanupOldImages(s.now())
	}
}

// CleanupThumbnails is cleanup_image_thumbnails: drop thumbnails whose image is gone.
func (s *Service) CleanupThumbnails() int {
	root := s.ThumbnailsDir()
	removed := 0
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, ".png") || !s.Exists(strings.TrimSuffix(rel, ".png")) {
			if os.Remove(p) == nil {
				removed++
			}
		}
		return nil
	})
	cleanupEmptyDirs(root)
	return removed
}

// Run is _auto_cleanup_worker plus the startup cleanup: retention, thumbnails, and free space
// below image_min_free_mb, every 30 minutes.
func (s *Service) Run(ctx context.Context) {
	s.CleanupOldImages(s.now())
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(cleanupEvery):
		}
		func() {
			defer func() { recover() }()
			s.CleanupOldImages(s.now())
			s.CleanupThumbnails()
			minFree := int64(s.settings.ImageMinFreeMB())
			if _, _, free := diskUsage(s.ImagesDir()); int64(free/mb) < minFree {
				log.Printf("image_auto_cleanup free_mb=%d min_free_mb=%d", free/mb, minFree)
				result, _ := s.DeleteToTarget(minFree, false)
				log.Printf("image_auto_cleanup_done %v", result)
			}
		}()
	}
}
