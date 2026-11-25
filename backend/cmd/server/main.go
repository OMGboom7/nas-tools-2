package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/0xforee/nas-tools/backend/internal/backuprestore"
	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/httpserver"
)

func main() {
	if len(os.Args) > 1 {
		if os.Args[1] != "restore" {
			slog.Error("unknown command")
			os.Exit(2)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := restoreCommand(ctx, os.Args[2:], config.ApplicationPath(), os.Stdout); err != nil {
			slog.Error("backup restore", "error", err)
			os.Exit(1)
		}
		return
	}
	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	release, err := holdApplication(cfg)
	if err != nil {
		slog.Error("cannot start with application configuration", "path", cfg.ApplicationConfigPath, "error", err)
		os.Exit(1)
	}
	defer release()
	shutdownSignal, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	workerContext, cancelWorkers := context.WithCancel(shutdownSignal)
	defer cancelWorkers()
	handler, waitWorkers, err := httpserver.NewWithContext(workerContext, cfg)
	if err != nil {
		slog.Error("cannot create HTTP server", "error", err)
		os.Exit(1)
	}
	defer waitWorkers()

	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	shutdown := make(chan error, 1)
	go func() {
		<-shutdownSignal.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdown <- server.Shutdown(ctx)
	}()

	slog.Info("Go server started", "address", cfg.Address, "native", cfg.DisableLegacy)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		cancelWorkers()
		waitWorkers()
		slog.Error("server stopped unexpectedly", "error", err)
		os.Exit(1)
	}
	cancelWorkers()
	if err := <-shutdown; err != nil {
		_ = server.Close()
		waitWorkers()
		slog.Error("server shutdown failed", "error", err)
		// Keep the directory lock until process exit if handlers exceeded the
		// graceful shutdown deadline.
		os.Exit(1)
	}
}

func holdApplication(cfg config.Config) (func(), error) {
	path := cfg.ApplicationConfigPath
	if path == "" {
		return nil, errors.New("set NASTOOL_CONFIG to the application configuration on the data host")
	}
	if cfg.DisableLegacy && os.Getenv("NASTOOL_CONFIG") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
	}
	release, err := backuprestore.HoldStartup(path)
	if err != nil || !cfg.DisableLegacy {
		return release, err
	}
	created, err := config.InitializeApplication(path)
	if err == nil {
		_, err = config.LoadApplication(path)
	}
	if err == nil {
		if created {
			slog.Info("initial configuration created; read app.login_user and app.login_password from this file to sign in", "path", path)
		}
		return release, nil
	}
	release()
	if !errors.Is(err, config.ErrMissingAPIKey) {
		return nil, err
	}
	if err := backuprestore.PrepareStartup(path, func() error { return config.InitializeSigningKey(path) }); err != nil {
		return nil, fmt.Errorf("initialize security.api_key (or set it in the original configuration): %w", err)
	}
	return backuprestore.HoldStartup(path)
}
