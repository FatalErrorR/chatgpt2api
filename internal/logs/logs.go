// Package logs is services/log_service.py: call and account logs, plus the daily counts of images
// returned, which the overview reads. Listing and the count windows are added with step 6.
package logs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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
	ID      string `gorm:"type:varchar(32);primaryKey"`
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

// Service writes logs.
type Service struct {
	db  *gorm.DB
	now func() time.Time
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
	entry := Entry{ID: newID(), Time: now.Format("2006-01-02 15:04:05"), Type: kind, Summary: summary, Detail: string(raw)}
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
