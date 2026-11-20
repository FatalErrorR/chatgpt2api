// Package logs is services/log_service.py: call and account logs, plus the daily counts of images
// returned, which the overview reads. Listing and the count windows are added with step 6.
package logs

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	TypeCall    = "call"
	TypeAccount = "account"
)

// Entry is one line of logs.jsonl.
type Entry struct {
	ID string `gorm:"type:varchar(32);primaryKey"`
	// Seq keeps the append order of logs.jsonl (newest = highest), including lines in the same second.
	Seq     int64  `gorm:"not null;default:0;index"`
	Time    string `gorm:"type:varchar(19);not null;index"` // server local time, "2006-01-02 15:04:05"
	Type    string `gorm:"type:varchar(16);not null;index"`
	Summary string `gorm:"type:text;not null"`
	Detail  string `gorm:"type:text;not null"` // JSON object
}

func (Entry) TableName() string { return "compat_logs" }

// DailyCount is one day of image_output_counts.json (days are UTC+8, as in Python).
type DailyCount struct {
	Day         string `gorm:"type:varchar(10);primaryKey"`
	Edits       int64  `gorm:"not null;default:0"`
	Generations int64  `gorm:"not null;default:0"`
}

func (DailyCount) TableName() string { return "compat_image_output_counts" }

// Service writes and reads logs.
type Service struct {
	db      *gorm.DB
	now     func() time.Time
	lastSeq atomic.Int64
}

// nextSeq is strictly increasing: nanoseconds, bumped past the previous value when the clock repeats.
func (s *Service) nextSeq() int64 {
	for {
		last := s.lastSeq.Load()
		next := max(time.Now().UnixNano(), last+1)
		if s.lastSeq.CompareAndSwap(last, next) {
			return next
		}
	}
}

func New(db *gorm.DB) *Service { return &Service{db: db, now: time.Now} }

// Migrate creates the log tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&Entry{}, &DailyCount{}) }

func newID() string {
	buf := make([]byte, 16)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

var beijing = time.FixedZone("UTC+8", 8*3600)

// Add appends one log entry and, for a successful image call, adds its images to the daily count.
func (s *Service) Add(kind, summary string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	now := s.now()
	entry := Entry{ID: newID(), Seq: s.nextSeq(), Time: now.Format("2006-01-02 15:04:05"), Type: kind, Summary: summary, Detail: string(raw)}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&entry).Error; err != nil {
			return err
		}
		day, column, count, ok := successImages(kind, detail, now)
		if !ok {
			return nil
		}
		row := DailyCount{Day: day}
		if column == "edits" {
			row.Edits = int64(count)
		} else {
			row.Generations = int64(count)
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "day"}},
			DoUpdates: clause.Assignments(map[string]any{column: gorm.Expr("compat_image_output_counts."+column+" + ?", count)}),
		}).Create(&row).Error
	})
}

// successImages is log_service._success_images: a successful /v1/images/* call counts its urls,
// on the UTC+8 day of the log time.
func successImages(kind string, detail map[string]any, at time.Time) (string, string, int, bool) {
	if kind != TypeCall || detail["status"] != "success" {
		return "", "", 0, false
	}
	column := map[string]string{"/v1/images/edits": "edits", "/v1/images/generations": "generations"}[stringOf(detail["endpoint"])]
	urls, _ := detail["urls"].([]any)
	if strs, ok := detail["urls"].([]string); ok {
		urls = make([]any, len(strs))
	}
	if column == "" || len(urls) == 0 {
		return "", "", 0, false
	}
	return at.In(beijing).Format("2006-01-02"), column, len(urls), true
}

func stringOf(value any) string {
	text, _ := value.(string)
	return text
}

// Item is one log as the API returns it.
func (e Entry) Item() map[string]any {
	decoder := json.NewDecoder(bytes.NewReader([]byte(e.Detail)))
	decoder.UseNumber()
	var detail any
	if decoder.Decode(&detail) != nil {
		detail = map[string]any{}
	}
	return map[string]any{"id": e.ID, "time": e.Time, "type": e.Type, "summary": e.Summary, "detail": detail}
}

// List is log_service.list: the newest matching entries (type, and day range on the time's date).
func (s *Service) List(kind, startDate, endDate string, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		return []map[string]any{}, nil
	}
	query := s.db.Model(&Entry{})
	if kind != "" {
		query = query.Where("type = ?", kind)
	}
	if startDate != "" {
		query = query.Where("time >= ?", startDate)
	}
	if endDate != "" {
		query = query.Where("time < ?", endDate+"~") // the whole end day: "~" sorts after any time text
	}
	var rows []Entry
	if err := query.Order("seq DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, row.Item())
	}
	return items, nil
}

// Delete removes entries by id and reports how many existed.
func (s *Service) Delete(ids []string) (int64, error) {
	wanted := []string{}
	for _, id := range ids {
		if id != "" {
			wanted = append(wanted, id)
		}
	}
	if len(wanted) == 0 {
		return 0, nil
	}
	result := s.db.Where("id IN ?", wanted).Delete(&Entry{})
	return result.RowsAffected, result.Error
}

// ImageOutputCounts is image_output_counts: successful images per kind in UTC+8 windows.
func (s *Service) ImageOutputCounts(now time.Time) (map[string]map[string]int64, error) {
	var rows []DailyCount
	if err := s.db.Find(&rows).Error; err != nil {
		return nil, err
	}
	today := now.In(beijing)
	day := func(offset int) string { return today.AddDate(0, 0, -offset).Format("2006-01-02") }
	windows := map[string][2]string{
		"today": {day(0), day(0)}, "yesterday": {day(1), day(1)}, "d7": {day(6), day(0)}, "d14": {day(13), day(0)},
		"d15": {day(14), day(0)}, "d30": {day(29), day(0)}, "all": {"0000-00-00", day(0)},
	}
	counts := map[string]map[string]int64{"edits": {}, "generations": {}}
	for key := range windows {
		counts["edits"][key], counts["generations"][key] = 0, 0
	}
	for _, row := range rows {
		if _, err := time.Parse("2006-01-02", row.Day); err != nil {
			continue
		}
		for key, window := range windows {
			if window[0] <= row.Day && row.Day <= window[1] {
				counts["edits"][key] += row.Edits
				counts["generations"][key] += row.Generations
			}
		}
	}
	return counts, nil
}
