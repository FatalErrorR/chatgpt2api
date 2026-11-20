// Package app wires config, repository, services and HTTP routes into one engine,
// so main and the tests build exactly the same router.
package app

import (
	"context"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/chatgptweb"
	"github.com/basketikun/chatgpt2api/internal/compat"
	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/docstore"
	"github.com/basketikun/chatgpt2api/internal/handler"
	"github.com/basketikun/chatgpt2api/internal/imagefit"
	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/basketikun/chatgpt2api/internal/imagesize"
	"github.com/basketikun/chatgpt2api/internal/imagestore"
	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/proxylist"
	"github.com/basketikun/chatgpt2api/internal/proxyops"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/basketikun/chatgpt2api/internal/settings"
	"github.com/basketikun/chatgpt2api/internal/upstream"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// App is the assembled server: the HTTP engine plus the background scheduler.
//
// Engine serves the compat routes (the Python version's API, internal/compat). Requests it has no
// route for go to Upstream, which holds every route of upstream v2. As modules are ported, their
// paths are taken over by compat; Upstream keeps serving the rest.
type App struct {
	Engine   *gin.Engine
	Upstream *gin.Engine
	repo     *repository.Repository
	pool     *pool.Service
	images   *imagestore.Service
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
	for _, migrate := range []func(*gorm.DB) error{compat.Migrate, logs.Migrate, pool.Migrate, docstore.Migrate, imagestore.Migrate} {
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
	proxyList, err := proxylist.New(docstore.New(db))
	if err != nil {
		return nil, err
	}
	proxyOps := &proxyops.Ops{List: proxyList, Pool: accountPool, Settings: store, Log: compatLogs,
		Test: func(ctx context.Context, proxyURL string) map[string]any { return proxyService.Test(ctx, proxyURL) }}
	dataDir := store.DataDir
	if dataDir == "" {
		dataDir = "data"
	}
	imageStore := imagestore.New(db, store, dataDir)
	imagePipeline := imagegen.NewPipeline(store, imagegen.Hooks{
		Save: imageStore.Save,
		Upscale: func(data []byte, size string) []byte {
			return imagesize.MaybeUpscale(data, size, imagesize.Settings{
				ExactSize: store.ImageExactSizeEnabled(), Upscale: store.ImageUpscaleEnabled(),
				UpscaleMethod: store.ImageUpscaleMethod(), UpscaleToEdge: store.ImageUpscaleTargetEdge(),
			})
		},
	})
	images := &imagegen.Generator{
		Pipeline: imagePipeline,
		Accounts: accountPool,
		Codex:    chatgptweb.CodexAttempt(imagePipeline.Hooks),
		NewBackend: func(token string, account pool.Account) (imagegen.Backend, error) {
			proxy, _ := account["proxy"].(string)
			api := upstream.NewOpenAIBackendAPI(token, strings.TrimSpace(proxy), proxyService, cfg)
			if accountID, _ := account["account_id"].(string); accountID != "" {
				api.WithAccountID(accountID)
			}
			return &chatgptweb.Backend{API: api, Settings: store, Health: proxyList, Fit: func(images [][]byte, prompt string) [][]byte {
				return imagefit.FitWebReferenceImages(images, prompt, imagefit.Options{
					RefEnabled: store.ImageRefFitEnabled(), RefBudget: store.ImageRefFitBudgetBytes(),
					ReqEnabled: store.ImageReqFitEnabled(), ReqBudget: store.ImageReqFitBudgetBytes(), MaxEdge: store.ImageRefFitMaxEdge(),
				})
			}}, nil
		},
	}
	// test_image_accounts: one small image with the account's own proxy; the account is not changed
	imageProbe := func(ctx context.Context, token string) (string, string) {
		backend, err := images.NewBackend(token, accountPool.GetAccount(token))
		if err != nil {
			return "失败", pool.ImageProbeReason(err.Error())
		}
		defer backend.Close()
		// like the Python loop: the first result or message decides
		status, reason, decided := "失败", "没有返回图片", false
		err = imagePipeline.StreamImageOutputs(ctx, backend, imagegen.Request{
			Model: "gpt-image-2.5-flare", Prompt: "a single red apple on a plain white background", N: 1, Size: "1024x1024", Quality: "low", PollTimeoutSecs: 90,
		}, 1, 1, func(output imagegen.Output) {
			if decided {
				return
			}
			if output.Kind == "result" && len(output.Data) > 0 {
				status, reason, decided = "成功", "", true
			} else if output.Kind == "message" && strings.TrimSpace(output.Text) != "" {
				reason, decided = pool.ImageProbeReason(output.Text), true
			}
		})
		if err != nil && !decided {
			return "失败", pool.ImageProbeReason(err.Error())
		}
		return status, reason
	}
	main := gin.New()
	main.Use(gin.Recovery(), handler.CORS, handler.LimitBody(cfg))
	compat.Register(main, &compat.Deps{
		DB: db, Settings: store, Auth: compat.NewAuthService(db, store), Pool: accountPool, Proxies: proxyOps, ProxyOps: proxyOps,
		Images: images, ImageStore: imageStore, Logs: compatLogs, DataDir: dataDir, ImageProbe: imageProbe,
		Version: cfg.Version, DatabaseURL: cfg.DatabaseURL,
	})
	main.NoRoute(func(c *gin.Context) { engine.ServeHTTP(c.Writer, c.Request) })
	_ = scheduler
	return &App{Engine: main, Upstream: engine, repo: repo, pool: accountPool, images: imageStore}, nil
}

// Start resets state left over from a previous run and starts the background jobs.
func (a *App) Start(ctx context.Context) {
	a.repo.FailRunningTasks(context.Background(), time.Now().UTC())
	a.repo.ResetAccountImageInflight(context.Background())
	// The upstream scheduler is not started: it refreshes the upstream accounts table, which the
	// compat routes do not use, and cleans data/images by file age instead of by calendar day.
	go a.pool.Watch(ctx)
	go a.images.Run(ctx)
}
