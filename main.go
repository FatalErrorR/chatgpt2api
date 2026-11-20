package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/basketikun/chatgpt2api/internal/app"
	"github.com/basketikun/chatgpt2api/internal/config"
	"github.com/basketikun/chatgpt2api/internal/repository"
	"github.com/basketikun/chatgpt2api/internal/settings"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// config.json in the Python format is the source of truth (internal/settings).
	configFile := os.Getenv("CHATGPT2API_CONFIG_FILE")
	if configFile == "" {
		configFile = "config.json"
	}
	store, err := settings.Load(configFile, "data")
	if err != nil {
		log.Fatal(err)
	}
	// Upstream's config.Load refuses to start without a key; hand it the same admin key.
	if os.Getenv("CHATGPT2API_AUTH_KEY") == "" {
		os.Setenv("CHATGPT2API_AUTH_KEY", store.AuthKey())
	}
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatal(err)
	}
	repo, err := repository.Open(cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer repo.Close()
	application, err := app.New(cfg, repo, store)
	if err != nil {
		log.Fatal(err)
	}
	application.Start(ctx)
	server := &http.Server{Addr: ":" + cfg.Port, Handler: application.Engine, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	if err = server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
