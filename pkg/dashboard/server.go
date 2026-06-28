// Package dashboard implements the HTTP dashboard server for bit-multi-brain-rag.
//
// It exposes:
//   - A web UI (HTMX, server-rendered) at /
//   - HTTP API endpoints under /api/v1/ (project CRUD, search)
//
// All endpoints (except /healthz and the UI root) require API key auth (ADR-0003).
package dashboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/auth"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/chunker"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/config"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/indexer"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/jobs"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/store"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

// Server is the dashboard HTTP server (built on Echo).
type Server struct {
	cfg     *config.Config
	logger  *slog.Logger
	echo    *echo.Echo
	store   *store.Store        // SQLite project registry
	rag     rag.Provider        // Qdrant vector store (may be nil if unreachable)
	embed   rag.EmbeddingClient // llama.cpp embedder
	chunker *chunker.Chunker
	indexer *indexer.Indexer
	jobs    *jobs.Manager // background index job orchestrator (ADR-0005)
	bm25    *rag.BM25Vectorizer // BM25 sparse vectorizer for hybrid search (ADR-0008)
}

// hybridEnabled returns true if hybrid search (dense + sparse + RRF) should
// be attempted. Checks: (1) bm25 vectorizer is fitted, (2) Settings toggle
// is on (default: on). Returns false in dev mode without Qdrant.
func (s *Server) hybridEnabled() bool {
	if s.bm25 == nil {
		return false
	}
	// Check settings table for hybrid_search toggle (default: enabled).
	val := s.store.GetSetting(context.Background(), "hybrid_search")
	return val != "off" // default on if setting doesn't exist or is empty
}

// New constructs a dashboard server with the given config.
// Opens SQLite store and initializes Qdrant + embedder (best-effort; failures
// are logged but do not block startup — UI + project CRUD still work).
func New(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	// Ensure DB directory exists.
	if dir := filepath.Dir(cfg.DBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir %s: %w", dir, err)
		}
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite store: %w", err)
	}
	logger.Info("sqlite store opened", "path", cfg.DBPath)

	// Best-effort Qdrant + embedder init. They may be offline in dev; the
	// dashboard still serves project CRUD. Search will 503 if these are nil.
	qc := rag.NewQdrantClient(cfg.QdrantURL, cfg.QdrantAPIKey, cfg.EmbeddingTimeoutS)
	if err := qc.Ping(context.Background()); err != nil {
		logger.Warn("qdrant unreachable, search disabled until available", "url", cfg.QdrantURL, "error", err)
	} else {
		logger.Info("qdrant connected", "url", cfg.QdrantURL)
	}

	emb := rag.NewLlamaEmbedder(rag.LlamaConfig{
		Endpoint: cfg.EmbeddingEndpoint,
		APIKey:   cfg.EmbeddingAPIKey,
		Model:    cfg.EmbeddingModel,
		Dim:      cfg.EmbeddingDim,
		Timeout:  time.Duration(cfg.EmbeddingTimeoutS) * time.Second,
	})

	// Seed default model in registry if empty (first boot).
	if err := st.SeedDefaultModel(context.Background(), cfg.EmbeddingEndpoint, cfg.EmbeddingAPIKey); err != nil {
		logger.Warn("failed to seed default model", "error", err)
	}

	chk := chunker.New()
	idx := indexer.New(chk, emb, qc, logger).WithStore(st)
	// Apply per-model chunk size from the active registry entry.
	if activeModel, err := st.GetActiveModel(context.Background()); err == nil {
		idx.MaxTokensPerChunk = activeModel.EffectiveChunkTokens()
		logger.Info("initial chunk size from active model",
			"model", activeModel.ModelName,
			"chunk_tokens", idx.MaxTokensPerChunk,
		)
	}
	// Background job manager: per-project locking, in-memory live progress,
	// SQLite-persisted final state, startup recovery for orphan jobs.
	jobMgr := jobs.NewManager(st, idx, logger)

	// Initialize BM25 vectorizer for hybrid search (ADR-0008).
	// Will be fitted on first indexing batch.
	idx.HybridEnabled = true // enable hybrid by default

	s := &Server{
		cfg:     cfg,
		logger:  logger,
		store:   st,
		rag:     qc,
		embed:   emb,
		chunker: chk,
		indexer: idx,
		jobs:    jobMgr,
		bm25:    rag.NewBM25Vectorizer(), // unfitted; fitted on first index batch
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	s.echo = e

	e.Use(middleware.Recover())
	e.Use(middleware.RemoveTrailingSlashWithConfig(middleware.TrailingSlashConfig{
		RedirectCode: 301,
	}))

	// --- Public routes ---
	e.GET("/healthz", s.healthz)

	// --- Protected API v1 routes ---
	api := e.Group("/api/v1", auth.EchoMiddleware(cfg.DashboardAPIKeys))
	api.GET("/projects", s.listProjects)
	api.POST("/projects", s.createProject)
	api.GET("/projects/:id", s.getProject)
	api.DELETE("/projects/:id", s.deleteProject)
	api.POST("/search", s.search)               // POST /api/v1/search
	api.POST("/index", s.indexAPI)              // POST /api/v1/index — enqueue (returns 202)
	api.POST("/index/upload", s.indexUploadAPI) // POST /api/v1/index/upload — accept pre-chunked docs (MCP upload)
	api.GET("/index/status", s.indexStatusAPI)  // GET  /api/v1/index/status?project=X
	api.POST("/index/cancel", s.indexCancelAPI) // POST /api/v1/index/cancel
	api.GET("/models", s.apiListModels)              // GET  /api/v1/models
	api.POST("/models", s.apiCreateModel)            // POST /api/v1/models
	api.PATCH("/models/:id", s.apiUpdateModel)       // PATCH /api/v1/models/:id
	api.DELETE("/models/:id", s.apiDeleteModel)      // DELETE /api/v1/models/:id
	api.POST("/models/active", s.apiSetActiveModel)  // POST /api/v1/models/active
	api.POST("/compare", s.apiCompare)               // POST /api/v1/compare
	api.GET("/health", s.apiHealth)                  // GET  /api/v1/health
	api.GET("/providers", s.apiProviders)            // GET  /api/v1/providers — registry
	api.GET("/providers/:id/models", s.apiProviderModels) // GET  /api/v1/providers/:id/models?refresh=1
	api.GET("/gpu/status", s.apiGPUStatus)            // GET  /api/v1/gpu/status — detection
	api.POST("/gpu/switch", s.apiGPUSwitch)           // POST /api/v1/gpu/switch — switch to gpu|cpu

	// --- Web UI ---
	e.GET("/", s.uiIndex)
	e.GET("/ui/health", s.uiHealth)                            // HTMX partial: health widget (polled 30s)
	e.GET("/ui/models", s.uiModelsPanel)                       // HTMX partial: model management panel
	e.GET("/ui/models/new", s.uiNewModelForm)                  // HTMX partial: 2-step wizard
	e.GET("/ui/models/:id/edit", s.uiEditModelForm)            // HTMX partial: edit existing model
	e.POST("/ui/models", s.uiCreateModel)                      // HTMX partial: add model
	e.POST("/ui/models/:id", s.uiUpdateModel)                  // HTMX partial: patch model
	e.GET("/ui/providers/:id/models", s.uiProviderModelOptions) // HTMX partial: <option> list for model picker
	e.POST("/ui/models/active", s.uiSetActiveModel)            // HTMX partial: switch active model
	e.POST("/ui/models/:id/delete", s.uiDeleteModel)           // HTMX partial: delete model
	e.POST("/ui/compare", s.uiCompare)                         // HTMX partial: comparison results
	e.GET("/ui/settings", s.uiSettingsPanel)                   // HTMX partial: settings page (GPU, runtimes)
	e.POST("/ui/settings/gpu/switch", s.uiGPUSwitch)           // HTMX partial: trigger GPU/CPU switch
	e.POST("/ui/settings/hybrid/toggle", s.uiHybridToggle)    // HTMX partial: toggle hybrid search
	e.GET("/ui/projects", s.uiProjectList)                     // HTMX partial: sidebar list
	e.GET("/ui/projects/:id", s.uiProjectDetail)               // HTMX partial: project detail
	e.GET("/ui/projects/:id/chunks", s.uiChunksPanel)          // HTMX partial: chunks browser panel
	e.GET("/ui/projects/:id/chunks/table", s.uiChunksTable)    // HTMX partial: chunks table/filter result
	e.GET("/ui/projects/:id/chunks/:pointID", s.uiChunkDetail) // HTMX partial: chunk side-panel detail
	e.POST("/ui/projects", s.uiCreateProject)                  // HTMX partial: create + refresh list
	e.GET("/ui/search", s.uiSearch)                            // HTMX partial: search results
	e.POST("/ui/index", s.uiRunIndex)                          // HTMX partial: enqueue indexing + live status
	e.GET("/ui/index/status", s.uiJobStatus)                   // HTMX partial: poll job state (every 2s)
	e.POST("/ui/index/cancel", s.uiCancelIndex)                // HTMX partial: cancel running job

	return s, nil
}

// ListenAndServe starts the HTTP server. Blocks until Shutdown is called.
func (s *Server) ListenAndServe() error {
	s.logger.Info("dashboard starting", "addr", s.cfg.HTTPAddr)
	s.echo.Server.Addr = s.cfg.HTTPAddr
	// All HTTP handlers complete in well under 1s now that indexing is
	// asynchronous (ADR-0005): /api/v1/index returns 202 immediately, and the
	// HTMX UI polls /ui/index/status. A short WriteTimeout protects against
	// slow-client / pipelining issues.
	s.echo.Server.ReadHeaderTimeout = 10 * time.Second
	s.echo.Server.ReadTimeout = 30 * time.Second
	s.echo.Server.WriteTimeout = 30 * time.Second
	s.echo.Server.IdleTimeout = 120 * time.Second
	return s.echo.StartServer(s.echo.Server)
}

// Shutdown gracefully stops the server + closes store.
func (s *Server) Shutdown(ctx context.Context) error {
	s.logger.Info("dashboard shutting down")
	if s.store != nil {
		_ = s.store.Close()
	}
	if s.rag != nil {
		_ = s.rag.Close()
	}
	return s.echo.Shutdown(ctx)
}

// --- API handlers ---

func (s *Server) healthz(c echo.Context) error {
	return c.JSON(200, map[string]string{
		"status":  "ok",
		"service": "bit-multi-brain-rag-dashboard",
	})
}

func (s *Server) listProjects(c echo.Context) error {
	projects, err := s.store.ListProjects(c.Request().Context())
	if err != nil {
		return c.JSON(500, map[string]string{"error": "list projects failed: " + err.Error()})
	}
	return c.JSON(200, map[string]any{"projects": projects})
}

type createProjectReq struct {
	Name        string `json:"name"`
	RootPath    string `json:"root_path"`
	Description string `json:"description"`
	Domains     string `json:"domains"`
}

func (s *Server) createProject(c echo.Context) error {
	var req createProjectReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(400, map[string]string{"error": "invalid request body"})
	}
	if req.Name == "" || req.RootPath == "" {
		return c.JSON(400, map[string]string{"error": "name and root_path are required"})
	}
	if req.Domains == "" {
		req.Domains = "code"
	}
	p := store.Project{
		Name:        req.Name,
		RootPath:    req.RootPath,
		Description: req.Description,
		Domains:     req.Domains,
	}
	created, err := s.store.CreateProject(c.Request().Context(), p)
	if err != nil {
		return c.JSON(500, map[string]string{"error": "create project failed: " + err.Error()})
	}
	return c.JSON(201, created)
}

func (s *Server) getProject(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.JSON(400, map[string]string{"error": "invalid project id"})
	}
	p, err := s.store.GetProject(c.Request().Context(), id)
	if err != nil {
		return c.JSON(404, map[string]string{"error": err.Error()})
	}
	return c.JSON(200, p)
}

func (s *Server) deleteProject(c echo.Context) error {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return c.JSON(400, map[string]string{"error": "invalid project id"})
	}
	if err := s.store.DeleteProject(c.Request().Context(), id); err != nil {
		return c.JSON(404, map[string]string{"error": err.Error()})
	}
	return c.JSON(200, map[string]string{"status": "deleted"})
}

// searchReq is the body for POST /api/v1/search.
type searchReq struct {
	Project string `json:"project"`
	Query   string `json:"query"`
	Limit   int    `json:"limit"`
}

func (s *Server) search(c echo.Context) error {
	var req searchReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(400, map[string]string{"error": "invalid request body"})
	}
	if req.Query == "" || req.Project == "" {
		return c.JSON(400, map[string]string{"error": "project and query are required"})
	}
	if req.Limit <= 0 {
		req.Limit = 5
	}
	results, err := s.doSearch(c.Request().Context(), req.Project, req.Query, req.Limit)
	if err != nil {
		status := 500
		if errors.Is(err, errBackendUnavailable) {
			status = 503
		}
		return c.JSON(status, map[string]string{"error": err.Error()})
	}
	return c.JSON(200, map[string]any{
		"query":   req.Query,
		"project": req.Project,
		"results": results,
	})
}
