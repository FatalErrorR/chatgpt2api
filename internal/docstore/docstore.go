// Package docstore keeps small JSON documents (the Python version's data/*.json files such as
// proxies.json) as rows of one table, written whole like the files were.
package docstore

import (
	"bytes"
	"encoding/json"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type document struct {
	Name string `gorm:"type:varchar(64);primaryKey"`
	Data string `gorm:"type:text;not null"`
}

func (document) TableName() string { return "compat_documents" }

// Store reads and writes named documents.
type Store struct{ db *gorm.DB }

func New(db *gorm.DB) *Store { return &Store{db: db} }

func Migrate(db *gorm.DB) error { return db.AutoMigrate(&document{}) }

// Load decodes the document into out (numbers as json.Number). A missing document leaves out
// untouched and returns false.
func (s *Store) Load(name string, out any) (bool, error) {
	var rows []document
	if err := s.db.Where("name = ?", name).Limit(1).Find(&rows).Error; err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	row := rows[0]
	decoder := json.NewDecoder(bytes.NewReader([]byte(row.Data)))
	decoder.UseNumber()
	return true, decoder.Decode(out)
}

// Save replaces the document.
func (s *Store) Save(name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "name"}}, DoUpdates: clause.AssignmentColumns([]string{"data"})}).
		Create(&document{Name: name, Data: string(raw)}).Error
}
