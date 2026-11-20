// Package app wires config, repository, services and HTTP routes into one engine,
// so main and the tests build exactly the same router.
package app

import (
	"context"
	"time"

	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/handler"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/gin-gonic/gin"
)

// App is the assembled server: the HTTP engine plus the background scheduler.
type App struct {
	Engine    *gin.Engine
	scheduler *service.Scheduler
	repo      *repository.Repository
}

// New builds every service and registers all routes. It does not start background jobs.
func New(cfg *config.Config, repo *repository.Repository) *App {
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
	engine.Use(gin.Recovery(), handler.CORS, handler.LimitBody(cfg))
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
	return &App{Engine: engine, scheduler: scheduler, repo: repo}
}

// Start resets state left over from a previous run and starts the background jobs.
func (a *App) Start(ctx context.Context) {
	a.repo.FailRunningTasks(context.Background(), time.Now().UTC())
	a.repo.ResetAccountImageInflight(context.Background())
	a.scheduler.Run(ctx)
}
