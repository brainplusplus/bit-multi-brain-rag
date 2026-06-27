// Package main is the bit-multi-brain-rag MCP server entrypoint.
//
// This binary runs LOCALLY on a developer machine. It speaks MCP over stdio
// to the host AI agent (Claude Desktop, Factory, OpenCode, Codex, etc.) and
// proxies semantic-search requests to a REMOTE bit-multi-brain-rag dashboard
// over HTTPS.
//
// Why no direct Qdrant/embedder connection?
//   - Only ONE public endpoint to secure (the dashboard).
//   - Qdrant + embedder stay on the internal Docker network of Easypanel.
//   - Single API key (DASHBOARD_API_KEYS) controls all access.
//
// Configuration (environment variables — these are READ FROM THE AGENT'S
// MCP config block, NOT from the dashboard's .env):
//
//   DASHBOARD_URL       e.g. "https://bit-rag.your-domain.com" (REQUIRED)
//   DASHBOARD_API_KEY   one of the keys in DASHBOARD_API_KEYS on the server (REQUIRED)
//   MCP_TIMEOUT_S       per-request HTTP timeout in seconds (default 30)
//
// Usage:
//   bit-rag-mcp                # reads env, serves MCP over stdio
//
// MCP client config example (Claude Desktop):
//   {
//     "mcpServers": {
//       "bit-rag": {
//         "command": "/usr/local/bin/bit-rag-mcp",
//         "env": {
//           "DASHBOARD_URL":     "https://bit-rag.your-domain.com",
//           "DASHBOARD_API_KEY": "your-strong-key"
//         }
//       }
//     }
//   }
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/mcp"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/ragclient"
)

func main() {
	// MCP uses STDERR for logs (stdout is reserved for JSON-RPC frames).
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("config error", "error", err)
		fmt.Fprintln(os.Stderr, helpMessage())
		os.Exit(1)
	}

	client, err := ragclient.New(ragclient.Config{
		BaseURL: cfg.DashboardURL,
		APIKey:  cfg.DashboardAPIKey,
		Timeout: time.Duration(cfg.TimeoutS) * time.Second,
	})
	if err != nil {
		logger.Error("ragclient init failed", "error", err)
		os.Exit(1)
	}

	// Fail fast if the dashboard is unreachable. Better to crash at boot
	// than to silently hang on every tool call.
	bootCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Healthz(bootCtx); err != nil {
		logger.Error("dashboard healthz failed at boot", "url", cfg.DashboardURL, "error", err)
		fmt.Fprintln(os.Stderr, "→ Check DASHBOARD_URL is reachable and the dashboard is running.")
		os.Exit(1)
	}
	logger.Info("dashboard healthy", "url", cfg.DashboardURL)

	srv := mcp.New(client, logger)

	ctx, cancelSrv := context.WithCancel(context.Background())
	defer cancelSrv()

	// Graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info("mcp shutdown signal received")
		cancelSrv()
	}()

	if err := srv.Serve(ctx); err != nil && ctx.Err() == nil {
		logger.Error("mcp serve failed", "error", err)
		os.Exit(1)
	}
}

// mcpConfig is the MCP-only config (minimal subset, NOT pkg/config which
// is dashboard-scoped).
type mcpConfig struct {
	DashboardURL    string
	DashboardAPIKey string
	TimeoutS        int
}

func loadConfig() (*mcpConfig, error) {
	url := os.Getenv("DASHBOARD_URL")
	if url == "" {
		return nil, fmt.Errorf("DASHBOARD_URL env is required")
	}
	key := os.Getenv("DASHBOARD_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("DASHBOARD_API_KEY env is required")
	}
	timeout := 30
	if v := os.Getenv("MCP_TIMEOUT_S"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			timeout = n
		}
	}
	return &mcpConfig{
		DashboardURL:    url,
		DashboardAPIKey: key,
		TimeoutS:        timeout,
	}, nil
}

func helpMessage() string {
	return `
bit-rag-mcp — Local MCP server that proxies to a remote bit-multi-brain-rag dashboard.

Required environment variables:
  DASHBOARD_URL       Full URL of the remote dashboard (e.g. https://bit-rag.example.com)
  DASHBOARD_API_KEY   API key matching one of the dashboard's DASHBOARD_API_KEYS

Optional:
  MCP_TIMEOUT_S       Per-request timeout in seconds (default 30)

See docs/INSTALL-MCP-LOCAL.md for full setup instructions for Claude Desktop,
Factory, OpenCode, Codex, Cursor, Continue, and Windsurf.
`
}
