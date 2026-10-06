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

	"github.com/0xforee/nas-tools/backend/internal/config"
	"github.com/0xforee/nas-tools/backend/internal/httpserver"
)

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

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

	go func() {
		<-shutdownSignal.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			slog.Error("server shutdown failed", "error", err)
		}
	}()

	slog.Info("Go migration gateway started", "address", cfg.Address, "legacy", cfg.LegacyBackendURL)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		cancelWorkers()
		waitWorkers()
		slog.Error("server stopped unexpectedly", "error", err)
		os.Exit(1)
	}
	cancelWorkers()
}
