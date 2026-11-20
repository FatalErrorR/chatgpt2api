package pool

import (
	"bytes"
	"encoding/json"
	"strconv"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// accountRow stores one account document. Position keeps the order of the Python dict, which the
// round-robin selection and the account list follow.
type accountRow struct {
	Token    string `gorm:"type:text;primaryKey"`
	Position int64  `gorm:"not null;index"`
	Data     string `gorm:"type:text;not null"`
}

func (accountRow) TableName() string { return "compat_accounts" }

// kvRow holds small values such as the cumulative account total (data/.cumulative_total in Python).
type kvRow struct {
	Key   string `gorm:"type:varchar(64);primaryKey"`
	Value string `gorm:"type:text;not null"`
}

func (kvRow) TableName() string { return "compat_kv" }

// Store persists accounts. The service writes only the documents that changed.
// SavedRow is one account as written: its position in the pool order and its JSON.
type SavedRow struct {
	Token    string
	Position int64
	Data     string
}

type Store interface {
	Load() ([]Account, error)
	Save(rows []SavedRow, removed []string) error
	LoadInt(key string) (int, bool)
	SaveInt(key string, value int) error
}

// Migrate creates the pool tables.
func Migrate(db *gorm.DB) error { return db.AutoMigrate(&accountRow{}, &kvRow{}) }

// DBStore is the database Store.
type DBStore struct{ db *gorm.DB }

func NewDBStore(db *gorm.DB) *DBStore { return &DBStore{db: db} }

func decodeAccount(raw string) (Account, error) {
	decoder := json.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.UseNumber()
	var account Account
	err := decoder.Decode(&account)
	return account, err
}

func (s *DBStore) Load() ([]Account, error) {
	var rows []accountRow
	if err := s.db.Order("position, token").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Account, 0, len(rows))
	for _, row := range rows {
		if account, err := decodeAccount(row.Data); err == nil && account != nil {
			out = append(out, account)
		}
	}
	return out, nil
}

func (s *DBStore) Save(rows []SavedRow, removed []string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if len(removed) > 0 {
			if err := tx.Where("token IN ?", removed).Delete(&accountRow{}).Error; err != nil {
				return err
			}
		}
		if len(rows) == 0 {
			return nil
		}
		out := make([]accountRow, len(rows))
		for i, row := range rows {
			out[i] = accountRow{Token: row.Token, Position: row.Position, Data: row.Data}
		}
		return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "token"}}, DoUpdates: clause.AssignmentColumns([]string{"position", "data"})}).
			CreateInBatches(out, 200).Error
	})
}

func (s *DBStore) LoadInt(key string) (int, bool) {
	var rows []kvRow
	if err := s.db.Where("key = ?", key).Limit(1).Find(&rows).Error; err != nil || len(rows) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rows[0].Value)
	return n, err == nil
}

func (s *DBStore) SaveInt(key string, value int) error {
	return s.db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoUpdates: clause.AssignmentColumns([]string{"value"})}).
		Create(&kvRow{Key: key, Value: strconv.Itoa(value)}).Error
}
