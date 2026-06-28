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
	"strings"
	"syscall"
	"time"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/indexer"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/mcp"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/ragclient"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// CLI mode: subcommands (index, search, watch, list)
	if len(os.Args) > 1 {
		runCLI(logger)
		return
	}

	// MCP stdio mode (no args) — original behavior
	runMCP(logger)
}

// runMCP starts the MCP stdio server (for coding tools).
func runMCP(logger *slog.Logger) {
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

	// Retry dashboard healthz for up to 60s (dashboard may still be starting).
	bootCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	retries := 0
	for {
		if err := client.Healthz(bootCtx); err == nil {
			break
		}
		retries++
		if retries >= 30 {
			logger.Error("dashboard healthz failed after 30 retries", "url", cfg.DashboardURL)
			fmt.Fprintln(os.Stderr, "→ Check DASHBOARD_URL is reachable and the dashboard is running.")
			os.Exit(1)
		}
		logger.Warn("dashboard not ready, retrying...", "retry", retries, "url", cfg.DashboardURL)
		time.Sleep(2 * time.Second)
	}
	logger.Info("dashboard healthy", "url", cfg.DashboardURL, "retries", retries)

	srv := mcp.New(client, logger)

	ctx, cancelSrv := context.WithCancel(context.Background())
	defer cancelSrv()

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

// runCLI dispatches CLI subcommands.
func runCLI(logger *slog.Logger) {
	cmd := os.Args[1]
	args := os.Args[2:]

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("config error", "error", err)
		os.Exit(1)
	}

	// CLI uses long timeout for indexing (no coding tool kill timeout)
	client, err := ragclient.New(ragclient.Config{
		BaseURL: cfg.DashboardURL,
		APIKey:  cfg.DashboardAPIKey,
		Timeout: 1 * time.Hour, // CLI mode: no timeout pressure
	})
	if err != nil {
		logger.Error("ragclient init failed", "error", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		logger.Info("interrupt received, shutting down")
		cancel()
	}()

	switch cmd {
	case "index":
		cliIndex(ctx, client, logger, args)
	case "search":
		cliSearch(ctx, client, logger, args)
	case "watch":
		cliWatch(ctx, client, logger, args)
	case "list":
		cliList(ctx, client, logger)
	case "help", "-h", "--help":
		fmt.Print(cliHelp())
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", cmd)
		fmt.Print(cliHelp())
		os.Exit(1)
	}
}

// cliHelp returns CLI usage text.
func cliHelp() string {
	return `bit-rag — semantic code search (MCP + CLI dual mode)

MCP mode (no args): serves MCP over stdio for AI agents.
CLI mode: direct terminal commands.

Usage:
  bit-rag-mcp                    MCP stdio server (for coding tools)
  bit-rag-mcp index <root_path>  Index a project (create if needed)
  bit-rag-mcp search <project_id> --query "..."  Search indexed code
  bit-rag-mcp watch <project_id> --root-path X   Background file watcher
  bit-rag-mcp list               List all projects

Examples:
  bit-rag-mcp index D:/NodeJS/lowcode
  bit-rag-mcp search 3 --query "JWT validation middleware"
  bit-rag-mcp watch 3 --root-path D:/NodeJS/lowcode

Environment:
  DASHBOARD_URL       Dashboard URL (required)
  DASHBOARD_API_KEY   API key (required)

Flags:
  --name NAME         Override project name (index only)
  --query Q           Search query (search only)
  --root-path PATH    Project root (watch only)
  --limit N           Max results (search, default 5)
`
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

// --- CLI subcommands ---

// parseFlags extracts --key value pairs from args, returns map + positional args.
func parseFlags(args []string) (map[string]string, []string) {
	flags := make(map[string]string)
	var positional []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			key := args[i][2:]
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				flags[key] = args[i+1]
				i++
			} else {
				flags[key] = "true"
			}
		} else {
			positional = append(positional, args[i])
		}
	}
	return flags, positional
}

// cliIndex indexes a project from the terminal. No coding tool timeout.
func cliIndex(ctx context.Context, client *ragclient.Client, logger *slog.Logger, args []string) {
	flags, positional := parseFlags(args)
	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: bit-rag-mcp index <root_path> [--name NAME]")
		os.Exit(1)
	}
	rootPath := positional[0]
	name := flags["name"]

	// Health check
	bootCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := client.Healthz(bootCtx); err != nil {
		fmt.Fprintf(os.Stderr, "Dashboard unreachable: %v\n", err)
		os.Exit(1)
	}

	// Create project (idempotent)
	if name == "" {
		existing, _ := client.GetProjectByPath(ctx, rootPath)
		if existing != nil {
			name = existing.Name
			fmt.Fprintf(os.Stderr, "Project %q already registered (ID: %d)\n", name, existing.ID)
		} else {
			// Auto-derive name
			projects, _ := client.ListProjects(ctx)
			existingNames := make(map[string]bool, len(projects))
			for _, p := range projects {
				existingNames[p.Name] = true
			}
			name = indexer.DeriveProjectNameUnique(rootPath, existingNames)
		}
	}

	p, err := client.CreateProject(ctx, name, rootPath, "")
	if err != nil {
		// Maybe 409 (name exists with different path) - try GetProjectByPath
		existing, _ := client.GetProjectByPath(ctx, rootPath)
		if existing != nil {
			p = existing
			name = p.Name
		} else {
			fmt.Fprintf(os.Stderr, "Create project failed: %v\n", err)
			os.Exit(1)
		}
	}
	fmt.Fprintf(os.Stderr, "Indexing project %q (ID: %d, root: %s)\n", p.Name, p.ID, p.RootPath)

	// Index (no timeout — CLI mode)
	// Build pattern filter from --include/--exclude flags (comma-separated).
	var pf *indexer.PatternFilter
	if inc, ok := flags["include"]; ok && inc != "" {
		includes := strings.Split(inc, ",")
		var excludes []string
		if exc, ok := flags["exclude"]; ok && exc != "" {
			excludes = strings.Split(exc, ",")
		}
		pf = indexer.NewPatternFilter(includes, excludes)
	}

	proj, err := client.GetProject(ctx, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Get project: %v\n", err)
		os.Exit(1)
	}
	stats, wasDelta, err := mcp.IndexWithManifest(ctx, client, *proj, rootPath, pf)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Index failed: %v\n", err)
		os.Exit(1)
	}
	if stats.FilesScanned == 0 && !wasDelta {
		fmt.Printf("Already up to date — no changes since last index.\n")
	} else {
		mode := "full"
		if wasDelta {
			mode = "delta"
		}
		fmt.Printf("Done [%s]: %d files, %d chunks, %d embedded, %d stored (%s)\n",
			mode, stats.FilesScanned, stats.Chunks, stats.Embedded, stats.Stored, stats.Duration)
	}
}

// cliSearch searches a project from the terminal.
func cliSearch(ctx context.Context, client *ragclient.Client, logger *slog.Logger, args []string) {
	flags, positional := parseFlags(args)
	if len(positional) == 0 || flags["query"] == "" {
		fmt.Fprintln(os.Stderr, `Usage: bit-rag-mcp search <project_id> --query "..." [--limit 5]`)
		os.Exit(1)
	}

	projectID, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid project_id: %s\n", positional[0])
		os.Exit(1)
	}
	query := flags["query"]
	limit := 5
	if l, err := strconv.Atoi(flags["limit"]); err == nil && l > 0 {
		limit = l
	}

	name, err := client.ResolveProjectIdentifier(ctx, projectID, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Resolve project: %v\n", err)
		os.Exit(1)
	}

	results, err := client.Search(ctx, name, query, limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Search failed: %v\n", err)
		os.Exit(1)
	}

	if len(results) == 0 {
		fmt.Println("No results found.")
		return
	}
	for i, r := range results {
		fmt.Printf("%d. [%.3f] %s:%s (%s)\n", i+1, r.Score, r.Meta["source_file"], r.Meta["start_line"], r.Meta["name"])
		fmt.Printf("   %s:%s\n\n", r.Meta["start_line"], r.Meta["end_line"])
	}
}

// cliList lists all projects.
func cliList(ctx context.Context, client *ragclient.Client, logger *slog.Logger) {
	projects, err := client.ListProjects(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "List failed: %v\n", err)
		os.Exit(1)
	}
	if len(projects) == 0 {
		fmt.Println("No projects registered.")
		return
	}
	fmt.Printf("%-4s %-40s %s\n", "ID", "Name", "Root Path")
	fmt.Println("---", "---", "---")
	for _, p := range projects {
		fmt.Printf("%-4d %-40s %s\n", p.ID, p.Name, p.RootPath)
	}
}

// cliWatch runs a background file watcher daemon.
func cliWatch(ctx context.Context, client *ragclient.Client, logger *slog.Logger, args []string) {
	flags, positional := parseFlags(args)
	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: bit-rag-mcp watch <project_id> --root-path PATH")
		os.Exit(1)
	}

	projectID, err := strconv.ParseInt(positional[0], 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid project_id: %s\n", positional[0])
		os.Exit(1)
	}
	rootPath := flags["root-path"]
	if rootPath == "" {
		fmt.Fprintln(os.Stderr, "--root-path is required")
		os.Exit(1)
	}

	name, err := client.ResolveProjectIdentifier(ctx, projectID, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Resolve project: %v\n", err)
		os.Exit(1)
	}

	// Use MCP's WatcherManager
	wm := mcp.NewWatcherManager(client, logger, ctx)
	wm.StartWatching(name, fmt.Sprintf("%d", projectID), rootPath)

	fmt.Fprintf(os.Stderr, "Watching project %q (ID: %d, root: %s)\n", name, projectID, rootPath)
	fmt.Fprintf(os.Stderr, "Press Ctrl+C to stop.\n\n")

	// Block until interrupted
	<-ctx.Done()
	wm.StopAll()
	fmt.Fprintf(os.Stderr, "\nWatcher stopped.\n")
}
