package compat

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/basketikun/chatgpt2api/internal/settings"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// AuthKey is one row of auth_keys.json (services/auth_service.py): only the SHA-256 of the key is kept.
type AuthKey struct {
	ID         string  `gorm:"type:varchar(32);primaryKey"`
	Name       string  `gorm:"type:varchar(255);not null"`
	Role       string  `gorm:"type:varchar(16);not null;index"`
	KeyHash    string  `gorm:"type:varchar(64);not null;uniqueIndex"`
	Enabled    bool    `gorm:"not null"`
	CreatedAt  string  `gorm:"type:varchar(40);not null"`
	LastUsedAt *string `gorm:"type:varchar(40)"`
	Position   int64   `gorm:"not null;default:0;index"` // keeps the file order of the Python list
}

func (AuthKey) TableName() string { return "compat_auth_keys" }

// Identity is what the handlers see as the caller, same fields as the Python identity dict.
type Identity map[string]any

func (i Identity) Role() string { s, _ := i["role"].(string); return s }

const identityKey = "compat.identity"

// ErrInput carries a user-facing message (HTTP 400), like Python's ValueError.
type ErrInput string

func (e ErrInput) Error() string { return string(e) }

// AuthService mirrors services/auth_service.py.
type AuthService struct {
	db        *gorm.DB
	settings  *settings.Store
	mu        sync.Mutex
	lastFlush map[string]time.Time
}

func NewAuthService(db *gorm.DB, store *settings.Store) *AuthService {
	return &AuthService{db: db, settings: store, lastFlush: map[string]time.Time{}}
}

func hashKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func nowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000-07:00") }

func defaultKeyName(role string) string {
	if role == "admin" {
		return "管理员密钥"
	}
	return "普通用户"
}

func publicKey(item AuthKey) map[string]any {
	var lastUsed any
	if item.LastUsedAt != nil {
		lastUsed = *item.LastUsedAt
	}
	return map[string]any{"id": item.ID, "name": item.Name, "role": item.Role, "enabled": item.Enabled, "created_at": item.CreatedAt, "last_used_at": lastUsed}
}

func (s *AuthService) all() ([]AuthKey, error) {
	var items []AuthKey
	err := s.db.Order("position, created_at, id").Find(&items).Error
	return items, err
}

// List returns the public items of one role ("" for all).
func (s *AuthService) List(role string) ([]map[string]any, error) {
	items, err := s.all()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, item := range items {
		if role == "" || item.Role == role {
			out = append(out, publicKey(item))
		}
	}
	return out, nil
}

func (s *AuthService) hasHash(items []AuthKey, hash string, excludeID string) bool {
	for _, item := range items {
		if excludeID != "" && item.ID == excludeID {
			continue
		}
		if item.KeyHash != "" && subtle.ConstantTimeCompare([]byte(item.KeyHash), []byte(hash)) == 1 {
			return true
		}
	}
	return false
}

func (s *AuthService) buildHash(items []AuthKey, raw string, excludeID string) (string, error) {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return "", ErrInput("请输入新的专用密钥")
	}
	if admin := s.settings.AuthKey(); admin != "" && subtle.ConstantTimeCompare([]byte(candidate), []byte(admin)) == 1 {
		return "", ErrInput("这个密钥和管理员密钥冲突了，请换一个新的密钥")
	}
	hash := hashKey(candidate)
	if s.hasHash(items, hash, excludeID) {
		return "", ErrInput("这个专用密钥已经存在，请换一个新的密钥")
	}
	return hash, nil
}

func hasName(items []AuthKey, name, role, excludeID string) bool {
	if name == "" {
		return false
	}
	for _, item := range items {
		if (excludeID != "" && item.ID == excludeID) || (role != "" && item.Role != role) {
			continue
		}
		if strings.TrimSpace(item.Name) == name {
			return true
		}
	}
	return false
}

func buildName(items []AuthKey, name, role, excludeID string) (string, error) {
	candidate := strings.TrimSpace(name)
	if candidate == "" {
		base := defaultKeyName(role)
		if !hasName(items, base, role, excludeID) {
			return base, nil
		}
		for suffix := 2; ; suffix++ {
			if next := fmt.Sprintf("%s %d", base, suffix); !hasName(items, next, role, excludeID) {
				return next, nil
			}
		}
	}
	if hasName(items, candidate, role, excludeID) {
		return "", ErrInput("这个名称已经在使用中了，换一个更容易区分的名称吧")
	}
	return candidate, nil
}

func randomID() string {
	buf := make([]byte, 6)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

// Create makes a new key and returns its public item and the raw key (shown once).
func (s *AuthService) Create(role, name string) (map[string]any, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.all()
	if err != nil {
		return nil, "", err
	}
	normalized, err := buildName(items, name, role, "")
	if err != nil {
		return nil, "", err
	}
	var raw, hash string
	for {
		buf := make([]byte, 24)
		rand.Read(buf)
		raw = "sk-" + base64.RawURLEncoding.EncodeToString(buf)
		if hash, err = s.buildHash(items, raw, ""); err == nil {
			break
		}
	}
	var position int64
	s.db.Model(&AuthKey{}).Select("COALESCE(MAX(position), 0) + 1").Scan(&position)
	item := AuthKey{ID: randomID(), Name: normalized, Role: role, KeyHash: hash, Enabled: true, CreatedAt: nowISO(), Position: position}
	if err := s.db.Create(&item).Error; err != nil {
		return nil, "", err
	}
	return publicKey(item), raw, nil
}

// Update changes name, enabled or the key itself. A nil map value means "not given".
func (s *AuthService) Update(id string, updates map[string]any, role string) (map[string]any, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items, err := s.all()
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.ID != id {
			continue
		}
		if role != "" && item.Role != role {
			return nil, nil
		}
		nextRole := "user"
		if strings.ToLower(strings.TrimSpace(item.Role)) == "admin" {
			nextRole = "admin"
		}
		if value, ok := updates["name"]; ok && value != nil {
			name, _ := value.(string)
			if item.Name, err = buildName(items, name, nextRole, id); err != nil {
				return nil, err
			}
		}
		if value, ok := updates["enabled"]; ok && value != nil {
			item.Enabled, _ = value.(bool)
		}
		if value, ok := updates["key"]; ok && value != nil {
			key, _ := value.(string)
			if item.KeyHash, err = s.buildHash(items, key, id); err != nil {
				return nil, err
			}
		}
		if err := s.db.Save(&item).Error; err != nil {
			return nil, err
		}
		return publicKey(item), nil
	}
	return nil, nil
}

// Delete removes a key of the given role ("" for any); false when it did not exist.
func (s *AuthService) Delete(id, role string) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, nil
	}
	query := s.db.Where("id = ?", id)
	if role != "" {
		query = query.Where("role = ?", role)
	}
	result := query.Delete(&AuthKey{})
	return result.RowsAffected > 0, result.Error
}

// Authenticate finds an enabled key by hash. last_used_at is written at most once a minute per key.
func (s *AuthService) Authenticate(raw string) Identity {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		return nil
	}
	hash := hashKey(candidate)
	var item AuthKey
	if err := s.db.Where("key_hash = ? AND enabled = ?", hash, true).Take(&item).Error; err != nil {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(item.KeyHash), []byte(hash)) != 1 {
		return nil
	}
	now := time.Now().UTC()
	stamp := now.Format("2006-01-02T15:04:05.000000-07:00")
	item.LastUsedAt = &stamp
	s.mu.Lock()
	last, seen := s.lastFlush[item.ID]
	flush := !seen || now.Sub(last) >= time.Minute
	if flush {
		s.lastFlush[item.ID] = now
	}
	s.mu.Unlock()
	if flush {
		s.db.Model(&AuthKey{}).Where("id = ?", item.ID).Update("last_used_at", stamp)
	}
	return Identity(publicKey(item))
}

// Identify resolves the bearer token: the admin key first, then user keys.
func (s *AuthService) Identify(authorization string) Identity {
	scheme, value, _ := strings.Cut(authorization, " ")
	token := strings.TrimSpace(value)
	if !strings.EqualFold(scheme, "bearer") || token == "" {
		return nil
	}
	if admin := strings.TrimSpace(s.settings.AuthKey()); admin != "" && token == admin {
		return Identity{"id": "admin", "name": "管理员", "role": "admin"}
	}
	return s.Authenticate(token)
}

// RequireIdentity is api/support.py require_identity as middleware.
func (s *AuthService) RequireIdentity(c *gin.Context) {
	identity := s.Identify(c.GetHeader("Authorization"))
	if identity == nil {
		abortDetail(c, 401, "密钥无效或已失效，请重新登录")
		return
	}
	c.Set(identityKey, identity)
	c.Next()
}

// RequireAdmin is api/support.py require_admin as middleware.
func (s *AuthService) RequireAdmin(c *gin.Context) {
	identity := s.Identify(c.GetHeader("Authorization"))
	if identity == nil {
		abortDetail(c, 401, "密钥无效或已失效，请重新登录")
		return
	}
	if identity.Role() != "admin" {
		abortDetail(c, 403, "需要管理员权限才能执行这个操作")
		return
	}
	c.Set(identityKey, identity)
	c.Next()
}

func identityOf(c *gin.Context) Identity {
	value, _ := c.Get(identityKey)
	identity, _ := value.(Identity)
	return identity
}

func isInputError(err error) bool {
	var input ErrInput
	return errors.As(err, &input)
}
