// Package app wires config, repository, services and HTTP routes into one engine,
// so main and the tests build exactly the same router.
package app

import (
	"context"
	"time"

	"github.com/basketikun/chatgpt2api/internal/compat"
	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/handler"
	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/basketikun/chatgpt2api/internal/settings"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// App is the assembled server: the HTTP engine plus the background scheduler.
//
// Engine serves the compat routes (the Python version's API, internal/compat). Requests it has no
// route for go to Upstream, which holds every route of upstream v2. As modules are ported, their
// paths are taken over by compat; Upstream keeps serving the rest.
type App struct {
	Engine    *gin.Engine
	Upstream  *gin.Engine
	scheduler *service.Scheduler
	repo      *repository.Repository
}

// Routes lists the routes of both routers.
func (a *App) Routes() gin.RoutesInfo {
	return append(a.Engine.Routes(), a.Upstream.Routes()...)
}

// New builds every service and registers all routes. It does not start background jobs.
func New(cfg *config.Config, repo *repository.Repository, store *settings.Store) (*App, error) {
	proxyService := service.NewProxyService(cfg)
	accountService := service.NewAccountService(repo, proxyService)
	imageService := service.NewImageService(repo, cfg)
	logService := service.NewLogService(repo)
	conversationService := service.NewConversationService(repo)
	catalogService := service.NewCatalogService(accountService, proxyService, cfg)
	aiService := service.NewAIService(accountService, catalogService, imageService, logService, proxyService, cfg)
	audioService := service.NewAudioService(accountService, logService, proxyService, cfg)
	authService := service.NewAuthService(repo, cfg.APIKey)
	taskService := service.NewTaskService(repo, aiService, cfg)
	authHandler := handler.NewAuthHandler(authService, cfg.Version)
	systemHandler := handler.NewSystemHandler(cfg, proxyService)
	engine := gin.New()
	engine.Use(gin.Recovery())
	authHandler.RegisterLogin(engine)
	systemHandler.RegisterVersion(engine)
	imageHandler := handler.NewImageHandler(imageService)
	imageHandler.RegisterPublic(engine)
	taskHandler := handler.NewTaskHandler(taskService)
	identity := handler.Identity(authService)
	api := engine.Group("/api", identity)
	handler.NewConversationHandler(conversationService).RegisterRoutes(api)
	taskHandler.RegisterRoutes(api)
	admin := engine.Group("/api", identity, handler.Admin)
	handler.NewAccountHandler(accountService).RegisterRoutes(admin)
	authHandler.RegisterRoutes(admin)
	systemHandler.RegisterRoutes(admin)
	handler.NewOAuthHandler(service.NewOAuthService(proxyService), accountService).RegisterRoutes(admin)
	handler.NewSourceHandler(service.NewSourceService(repo, accountService, proxyService)).RegisterRoutes(admin)
	imageHandler.RegisterRoutes(admin)
	handler.NewLogHandler(logService).RegisterRoutes(admin)
	handler.NewOverviewHandler(service.NewOverviewService(repo)).RegisterRoutes(admin)
	v1 := engine.Group("/v1", identity)
	handler.NewCatalogHandler(catalogService, cfg).RegisterRoutes(v1)
	handler.NewAIHandler(aiService, logService).RegisterRoutes(v1)
	handler.NewAudioHandler(audioService, logService).RegisterRoutes(v1)
	engine.NoRoute(handler.Static("web"))
	scheduler := service.NewScheduler(accountService, imageService, taskService, logService, cfg)

	db := repo.DB()
	for _, migrate := range []func(*gorm.DB) error{compat.Migrate, logs.Migrate, pool.Migrate} {
		if err := migrate(db); err != nil {
			return nil, err
		}
	}
	// config.json is the source of truth; the upstream services read their copy of it
	compat.SyncUpstreamConfig(cfg, store)
	store.OnChange(func() { compat.SyncUpstreamConfig(cfg, store) })
	compatLogs := logs.New(db)
	accountPool, err := pool.New(pool.NewDBStore(db), store, compatLogs)
	if err != nil {
		return nil, err
	}
	accountPool.Remote = &compat.ChatGPTRemote{Proxy: proxyService, Config: cfg}
	main := gin.New()
	main.Use(gin.Recovery(), handler.CORS, handler.LimitBody(cfg))
	compat.Register(main, &compat.Deps{
		DB: db, Settings: store, Auth: compat.NewAuthService(db, store), Pool: accountPool,
		Version: cfg.Version, DatabaseURL: cfg.DatabaseURL,
	})
	main.NoRoute(func(c *gin.Context) { engine.ServeHTTP(c.Writer, c.Request) })
	return &App{Engine: main, Upstream: engine, scheduler: scheduler, repo: repo}, nil
}

// Start resets state left over from a previous run and starts the background jobs.
func (a *App) Start(ctx context.Context) {
	a.repo.FailRunningTasks(context.Background(), time.Now().UTC())
	a.repo.ResetAccountImageInflight(context.Background())
	a.scheduler.Run(ctx)
}
