// Package app wires config, repository, services and HTTP routes into one engine,
// so main and the tests build exactly the same router.
package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/basketikun/chatgpt2api/internal/backup"
	"github.com/basketikun/chatgpt2api/internal/chatgptweb"
	"github.com/basketikun/chatgpt2api/internal/compat"
	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/docstore"
	"github.com/basketikun/chatgpt2api/internal/filetasks"
	"github.com/basketikun/chatgpt2api/internal/handler"
	"github.com/basketikun/chatgpt2api/internal/imagefit"
	"github.com/basketikun/chatgpt2api/internal/imagegen"
	"github.com/basketikun/chatgpt2api/internal/imagesize"
	"github.com/basketikun/chatgpt2api/internal/imagestore"
	"github.com/basketikun/chatgpt2api/internal/imagetasks"
	"github.com/basketikun/chatgpt2api/internal/logs"
	"github.com/basketikun/chatgpt2api/internal/openailogin"
	"github.com/basketikun/chatgpt2api/internal/pool"
	"github.com/basketikun/chatgpt2api/internal/protocollogin"
	"github.com/basketikun/chatgpt2api/internal/proxylist"
	"github.com/basketikun/chatgpt2api/internal/proxyops"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/service"
	"github.com/basketikun/chatgpt2api/internal/settings"
	"github.com/basketikun/chatgpt2api/internal/sources"
	"github.com/basketikun/chatgpt2api/internal/textapi"
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
	backups  *backup.Service
	settings *settings.Store
	// background work that outlives its request (image tasks, file exports, login batches)
	waiters []func()
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
	for _, migrate := range []func(*gorm.DB) error{compat.Migrate, logs.Migrate, pool.Migrate, docstore.Migrate, imagestore.Migrate, imagetasks.Migrate, filetasks.Migrate} {
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
	passwordLogin := openailogin.Login{GlobalProxy: store.Proxy}
	loginFunc := func(ctx context.Context, email, password, totpSecret, proxy string) (map[string]any, error) {
		result, err := passwordLogin.PasswordLogin(ctx, email, password, totpSecret, proxy)
		return map[string]any(result), err
	}
	accountPool.Relogin = accountPool.PasswordRelogin(loginFunc)
	proxyList, err := proxylist.New(docstore.New(db))
	if err != nil {
		return nil, err
	}
	accountPool.Remote.(*compat.ChatGPTRemote).Health = proxyList
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
			api := upstream.NewOpenAIBackendAPI(token, strings.TrimSpace(proxy), proxyService, cfg).SingleAttempt()
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
	imageTasks, err := imagetasks.New(db, store, compatLogs)
	if err != nil {
		return nil, err
	}
	// resume-poll: the account that started the conversation polls it again (Python's version
	// built a client without a token and always failed).
	imageTasks.Resume = func(ctx context.Context, email, conversationID string, extraSecs float64, size, quality string) ([]map[string]any, error) {
		token := ""
		for _, account := range accountPool.ListAccounts() {
			if email != "" && strings.EqualFold(strings.TrimSpace(fmt.Sprint(account["email"])), email) {
				token, _ = account["access_token"].(string)
				break
			}
		}
		if token == "" {
			return nil, errors.New("the account that started this task is no longer in the pool")
		}
		backend, err := images.NewBackend(token, accountPool.GetAccount(token))
		if err != nil {
			return nil, err
		}
		web := backend.(*chatgptweb.Backend)
		files, sediments, err := web.PollImageResults(ctx, conversationID, extraSecs, nil, nil)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 && len(sediments) == 0 {
			return nil, fmt.Errorf("继续等待 %s 秒后仍未找到图片结果。", strconv.FormatFloat(extraSecs, 'f', -1, 64))
		}
		urls, err := web.ResolveImageURLs(ctx, conversationID, files, sediments, 0, false)
		if err != nil {
			return nil, err
		}
		if len(urls) == 0 {
			return nil, errors.New("图片 URL 解析失败")
		}
		downloaded, err := web.DownloadImages(ctx, urls)
		if err != nil {
			return nil, err
		}
		items := []map[string]any{}
		for _, data := range downloaded {
			items = append(items, map[string]any{"b64_json": base64.StdEncoding.EncodeToString(data)})
		}
		formatted, err := imagePipeline.Hooks.FormatImageResult(items, "", "b64_json", "", time.Now().Unix(), "", size)
		if err != nil {
			return nil, err
		}
		data, _ := formatted["data"].([]map[string]any)
		return data, nil
	}
	fileTasks, err := filetasks.New(db, accountPool, compatLogs, filepath.Join(dataDir, "files"))
	if err != nil {
		return nil, err
	}
	fileTasks.Export = func(ctx context.Context, token, kind string, refs []string, prompt, dir string) (string, string, string, error) {
		backend, err := images.NewBackend(token, accountPool.GetAccount(token))
		if err != nil {
			return "", "", "", err
		}
		result, err := backend.(*chatgptweb.Backend).ExportEditable(ctx, kind, refs, prompt, dir)
		return result.ConversationID, result.PrimaryPath, result.ZipPath, err
	}
	textDeps := &textapi.Deps{Settings: store, Pool: accountPool, Generator: images, Cache: textapi.NewCache(store),
		Images: textapi.Images{Proxy: store.Proxy},
		NewBackend: func(token string) (textapi.Backend, error) {
			account := pool.Account{}
			if token != "" {
				account = accountPool.GetAccount(token)
			}
			proxy, _ := account["proxy"].(string)
			api := upstream.NewOpenAIBackendAPI(token, strings.TrimSpace(proxy), proxyService, cfg).SingleAttempt()
			if accountID, _ := account["account_id"].(string); accountID != "" {
				api.WithAccountID(accountID)
			}
			return &chatgptweb.Backend{API: api, Settings: store, Health: proxyList}, nil
		}}
	textDeps.Catalog = &textapi.Catalog{Deps: textDeps}
	cpaSources, err := sources.NewCPA(docstore.New(db), accountPool, store.Proxy)
	if err != nil {
		return nil, err
	}
	sub2apiSources, err := sources.NewSub2API(docstore.New(db), accountPool)
	if err != nil {
		return nil, err
	}
	docs := docstore.New(db)
	compatAuth := compat.NewAuthService(db, store)
	backups := &backup.Service{Settings: store, Docs: docs, Sources: backup.Sources{
		ConfigFile: store.Path(), ImagesDir: imageStore.ImagesDir(), AppVersion: cfg.Version,
		Document: func(name string) []byte {
			var value any
			if found, err := docs.Load(name, &value); err != nil || !found {
				return nil
			}
			raw, _ := json.MarshalIndent(value, "", "  ")
			return raw
		},
		Logs:       compatLogs.ExportJSONL,
		ImageTasks: func() any { return imageTasks.Export() },
		ImageIndex: func() (any, error) { return imageStore.ExportIndex() },
		ImageTags:  func() (any, error) { return imageStore.Tags() },
		Accounts:   func() (any, error) { return pool.NewDBStore(db).Load() },
		AuthKeys:   compatAuth.Snapshot,
		StorageBackend: func() any {
			return map[string]any{"type": "database", "description": "数据库存储"}
		},
	}}
	protocolLogin := &protocollogin.Service{Pool: accountPool, Proxies: proxyOps, Login: loginFunc}
	main := gin.New()
	main.Use(gin.Recovery(), handler.CORS, handler.LimitBody(cfg), compat.Gzip)
	compat.Register(main, &compat.Deps{
		DB: db, Settings: store, Auth: compatAuth, Pool: accountPool, Proxies: proxyOps, ProxyOps: proxyOps,
		Images: images, ImageStore: imageStore, Text: textDeps, ImageTasks: imageTasks, FileTasks: fileTasks,
		ProtocolLogin: protocolLogin, OAuth: &openailogin.OAuth{Proxy: store.Proxy}, CPA: cpaSources, Sub2API: sub2apiSources, Backup: backups,
		RefreshClearance: func(ctx context.Context, target string) (string, map[string]string, error) {
			bundle, err := proxyService.RefreshClearance(ctx, target)
			return bundle.UserAgent, bundle.Cookies, err
		}, Logs: compatLogs, DataDir: dataDir, ImageProbe: imageProbe,
		Version: cfg.Version, DatabaseURL: cfg.DatabaseURL,
	})
	// every Python route is on the compat layer now; the upstream routes are no longer reachable
	// (their handlers use upstream's own tables). Anything else is the frontend, as in Python.
	webDir := os.Getenv("CHATGPT2API_WEB_DIR")
	if webDir == "" {
		webDir = "web_dist"
	}
	main.NoRoute(compat.ServeWeb(webDir))
	_ = scheduler
	return &App{Engine: main, Upstream: engine, repo: repo, pool: accountPool, images: imageStore, backups: backups, settings: store,
		waiters: []func(){imageTasks.Wait, fileTasks.Wait, protocolLogin.Wait}}, nil
}

// Start resets state left over from a previous run and starts the background jobs.
func (a *App) Start(ctx context.Context) {
	a.repo.FailRunningTasks(context.Background(), time.Now().UTC())
	a.repo.ResetAccountImageInflight(context.Background())
	// The upstream scheduler is not started: it refreshes the upstream accounts table, which the
	// compat routes do not use, and cleans data/images by file age instead of by calendar day.
	if !BackgroundJobsEnabled() {
		log.Printf("CHATGPT2API_BACKGROUND_JOBS=off: account watcher, image cleanup and scheduled backups are not started")
		return
	}
	go a.pool.Watch(ctx)
	go a.images.Run(ctx)
	go a.backups.Start(ctx)
}

// BackgroundJobsEnabled is false when CHATGPT2API_BACKGROUND_JOBS is "off". An instance started on
// a copy of production data (the migration rehearsal) must not refresh or rotate the real accounts'
// tokens, delete images by retention, or upload backups with production's R2 settings.
func BackgroundJobsEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("CHATGPT2API_BACKGROUND_JOBS")), "off")
}

// ShutdownGrace is how long a stop waits for requests and background work in flight: the image
// poll timeout plus a minute (an image request can run that long), or
// CHATGPT2API_SHUTDOWN_TIMEOUT_SECS.
func (a *App) ShutdownGrace() time.Duration {
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CHATGPT2API_SHUTDOWN_TIMEOUT_SECS"))); err == nil && value >= 0 {
		return time.Duration(value) * time.Second
	}
	return time.Duration(a.settings.ImagePollTimeoutSecs()+60) * time.Second
}

// Serve runs the HTTP server until ctx ends, then stops taking new connections and waits up to
// grace for requests in flight (image generations, streams) and background tasks to finish,
// instead of cutting them off.
func (a *App) Serve(ctx context.Context, server *http.Server, grace time.Duration) error {
	done := make(chan error, 1)
	go func() {
		<-ctx.Done()
		log.Printf("shutting down: waiting up to %s for requests and tasks in flight", grace)
		deadline, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		err := server.Shutdown(deadline)
		finished := make(chan struct{})
		go func() {
			for _, wait := range a.waiters {
				wait()
			}
			close(finished)
		}()
		select {
		case <-finished:
		case <-deadline.Done():
			if err == nil {
				err = deadline.Err()
			}
		}
		done <- err
	}()
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	if err := <-done; err != nil {
		log.Printf("shutdown did not finish within %s: %v", grace, err)
	}
	return nil
}
