package compat

import (
	"net/http"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/settings"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Deps are the shared pieces every compat handler may use.
type Deps struct {
	DB          *gorm.DB
	Settings    *settings.Store
	Auth        *AuthService
	Version     string
	DatabaseURL string
}

// Migrate creates the compat tables.
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(&AuthKey{})
}

// Register mounts the compat routes. Routes registered here win over the upstream ones
// (app.New sends anything unmatched to the upstream router).
func Register(engine *gin.Engine, d *Deps) {
	engine.GET("/version", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"version": d.Version}) })
	engine.POST("/auth/login", d.Auth.RequireIdentity, func(c *gin.Context) {
		identity := identityOf(c)
		c.JSON(http.StatusOK, gin.H{"ok": true, "version": d.Version, "role": identity["role"], "subject_id": identity["id"], "name": identity["name"]})
	})

	admin := engine.Group("", d.Auth.RequireAdmin)
	admin.GET("/api/settings", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"config": d.Settings.Get()}) })
	admin.POST("/api/settings", func(c *gin.Context) {
		body, ok := bindJSON(c)
		if !ok {
			return
		}
		config, err := d.Settings.Update(body)
		if err != nil {
			if settings.IsValidationError(err) {
				abortDetail(c, http.StatusBadRequest, err.Error())
				return
			}
			abortDetail(c, http.StatusInternalServerError, err.Error())
			return
		}
		c.JSON(http.StatusOK, gin.H{"config": config})
	})
	admin.GET("/api/storage/info", func(c *gin.Context) {
		c.JSON(http.StatusOK, storageInfo(d))
	})
	engine.GET("/api/third-party-apps", d.Auth.RequireIdentity, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"third_party_apps": d.Settings.ThirdPartyApps()})
	})
	registerUserKeys(admin, d)
}

// maskPassword is database_storage._mask_password.
func maskPassword(url string) string {
	protocol, rest, ok := strings.Cut(url, "://")
	if !ok {
		return url
	}
	credentials, host, ok := strings.Cut(rest, "@")
	if !ok {
		return url
	}
	username, _, ok := strings.Cut(credentials, ":")
	if !ok {
		return url
	}
	return protocol + "://" + username + ":****@" + host
}

// storageInfo has the shape of Python's database storage backend (services/storage/database_storage.py).
func storageInfo(d *Deps) gin.H {
	dbType := "unknown"
	switch {
	case strings.Contains(d.DatabaseURL, "sqlite"):
		dbType = "sqlite"
	case strings.Contains(d.DatabaseURL, "postgres"):
		dbType = "postgresql"
	case strings.Contains(d.DatabaseURL, "mysql"):
		dbType = "mysql"
	}
	backend := gin.H{"type": "database", "db_type": dbType, "description": "数据库存储 (" + dbType + ")", "database_url": maskPassword(d.DatabaseURL)}
	var accounts, keys int64
	health := gin.H{"status": "healthy", "backend": "database", "database_url": maskPassword(d.DatabaseURL)}
	if err := d.DB.Exec("SELECT 1").Error; err != nil {
		health = gin.H{"status": "unhealthy", "backend": "database", "error": err.Error()}
	} else {
		d.DB.Table("accounts").Count(&accounts)
		d.DB.Model(&AuthKey{}).Count(&keys)
		health["account_count"] = accounts
		health["auth_key_count"] = keys
	}
	return gin.H{"backend": backend, "health": health}
}
