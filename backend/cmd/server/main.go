package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
		if err := restoreCommand(ctx, os.Args[2:], os.Getenv("NASTOOL_CONFIG"), os.Stdout); err != nil {
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

	release, err := backuprestore.HoldStartup(cfg.ApplicationConfigPath)
	if err != nil {
		slog.Error("configuration directory unavailable", "error", err)
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

	slog.Info("Go migration gateway started", "address", cfg.Address, "legacy", cfg.LegacyBackendURL)
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
