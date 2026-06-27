// Package main is the dashboard server entrypoint for bit-multi-brain-rag.
//
// Usage:
//   bit-multi-brain-rag-dashboard
//
// Configuration is loaded from environment variables (and/or a .env file
// loaded externally). See pkg/config for the full list.
//
// Phase 1 scope: config load + auth middleware + health endpoint.
// Search/index/UI are stubbed and filled in later phases (ADR-0004 roadmap).
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

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/config"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/dashboard"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	logger.Info("config loaded",
		"http_addr", cfg.HTTPAddr,
		"environment", cfg.Environment,
		"qdrant_url", cfg.QdrantURL,
		"embedding_endpoint", cfg.EmbeddingEndpoint,
		"active_model", cfg.ActiveModel,
		"active_backend", cfg.ActiveBackend,
		"embedding_dim", cfg.EmbeddingDim,
		"api_keys_count", len(cfg.DashboardAPIKeys),
	)

	srv, err := dashboard.New(cfg, logger)
	if err != nil {
		logger.Error("dashboard init failed", "error", err)
		os.Exit(1)
	}

	// Run server in background; wait for signal.
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	logger.Info("dashboard listening", "addr", cfg.HTTPAddr)

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("shutdown signal received", "signal", sig.String())
	case err := <-errCh:
		logger.Error("server error", "error", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}
