package compat

import (
	"net/http"
	"strings"

	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/proxyops"
	"github.com/basketikun/chatgpt2api/internal/settings"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Deps are the shared pieces every compat handler may use.
type Deps struct {
	DB          *gorm.DB
	Settings    *settings.Store
	Auth        *AuthService
	Pool        *pool.Service
	Proxies     ProxyReserver // proxy list reservations for imports
	ProxyOps    *proxyops.Ops
	ImageProbe  pool.ImageProbe // one small test image per account
	Images      *imagegen.Generator
	Logs        *logs.Service
	DataDir     string
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
	registerAccounts(admin, d)
	registerProxies(admin, d)
	registerImages(engine, d)
	engine.GET("/health", func(c *gin.Context) { health(c, d) })
}

// runtimeStatus is proxy_settings.get_runtime_status() for the upstream profile.
func runtimeStatus(store *settings.Store) gin.H {
	runtime := store.ProxyRuntime()
	clearance, _ := runtime["clearance"].(map[string]any)
	enabled, _ := runtime["enabled"].(bool)
	egress, _ := runtime["egress_mode"].(string)
	source, proxy := "direct", ""
	if enabled && egress == "single_proxy" {
		if url, _ := runtime["proxy_url"].(string); url != "" {
			source, proxy = "runtime", url
		}
	}
	if proxy == "" && store.Proxy() != "" {
		source, proxy = "global", store.Proxy()
	}
	mode, _ := clearance["mode"].(string)
	clearanceEnabled, _ := clearance["enabled"].(bool)
	return gin.H{
		"enabled": enabled, "egress_mode": egress, "proxy_source": source, "has_proxy": proxy != "",
		"clearance_enabled": enabled && clearanceEnabled && (mode == "manual" || mode == "flaresolverr"),
		"clearance_mode":    mode, "has_clearance_bundle": false, "cached_clearance_hosts": []string{},
	}
}

// health is GET /health: JSON with ?format=json, otherwise the HTML dashboard.
func health(c *gin.Context, d *Deps) {
	stats := d.Pool.Stats()
	healthy := stats["active"].(int) > 0
	status := "degraded"
	if healthy {
		status = "ok"
	}
	info := storageInfo(d)
	payload := gin.H{"status": status, "healthy": healthy, "version": d.Version, "storage": gin.H{"backend": info["backend"], "health": info["health"]},
		"proxy_runtime": runtimeStatus(d.Settings), "accounts": stats}
	if c.Query("format") == "json" {
		c.JSON(http.StatusOK, payload)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(healthHTML(payload)))
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
