// Package mcp implements a Model Context Protocol (MCP) server for bit-multi-brain-rag.
//
// MCP exposes RAG tools (semantic code search) to AI agents via stdio JSON-RPC 2.0.
//
// Architecture (post-refactor):
//
//   The MCP server runs LOCALLY on the developer's machine. It does NOT
//   connect directly to Qdrant or the embedder. Instead it calls the
//   dashboard's HTTPS API (deployed on Easypanel) which proxies the search
//   to internal Qdrant + embedder. This means:
//
//     - Only ONE public endpoint to secure (the dashboard).
//     - Qdrant + embedder remain on the internal Docker network.
//     - Source code stays local — only the query text leaves the machine.
//
// Tool registry pattern (ADR-0004): each tool implements the Tool interface.
// Phase 1 ships CodeRAGTool (semantic search). Future: DocRAGTool, TaskRAGTool.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/chunker"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/indexer"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/ragclient"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/watcher"
)

// Server is the MCP stdio server.
type Server struct {
	rag     *ragclient.Client
	tools   map[string]Tool
	logger  *slog.Logger
	wm      *WatcherManager
}

// WatcherManager tracks active file watchers per project. When a project
// is created or indexed, a watcher starts for its root_path. On file change,
// it triggers a debounced delta re-index (only changed files).
type WatcherManager struct {
	mu       sync.Mutex
	watchers map[string]*watcher.Watcher // key = project name
	client   *ragclient.Client
	logger   *slog.Logger
	ctx      context.Context
}

// NewWatcherManager creates a WatcherManager tied to the MCP server's context.
func NewWatcherManager(client *ragclient.Client, logger *slog.Logger, ctx context.Context) *WatcherManager {
	return &WatcherManager{
		watchers: make(map[string]*watcher.Watcher),
		client:   client,
		logger:   logger,
		ctx:      ctx,
	}
}

// StartWatching begins watching rootPath for project. If a watcher already
// exists for this project, it is replaced.
func (wm *WatcherManager) StartWatching(project, rootPath string) {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	// Stop existing watcher for this project.
	if old, ok := wm.watchers[project]; ok {
		old.Stop()
		delete(wm.watchers, project)
	}

	onChange := func(changedFiles []string) {
		wm.logger.Info("delta re-index triggered", "project", project, "root", rootPath, "files", len(changedFiles))
		go func() {
			stats, err := deltaIndex(wm.ctx, wm.client, project, rootPath, changedFiles)
			if err != nil {
				wm.logger.Error("delta re-index failed", "project", project, "error", err)
				return
			}
			wm.logger.Info("delta re-index complete",
				"project", project,
				"files_scanned", stats.FilesScanned,
				"chunks", stats.Chunks,
				"embedded", stats.Embedded,
				"duration", stats.Duration)
		}()
	}

	w, err := watcher.New(rootPath, onChange, wm.logger)
	if err != nil {
		wm.logger.Error("watcher create failed", "project", project, "root", rootPath, "error", err)
		return
	}
	wm.watchers[project] = w
	go w.Start(wm.ctx)
	wm.logger.Info("watcher started", "project", project, "root", rootPath)
}

// StopAll stops all active watchers (called on MCP shutdown).
func (wm *WatcherManager) StopAll() {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	for project, w := range wm.watchers {
		w.Stop()
		delete(wm.watchers, project)
	}
}

// Tool is the registry contract for an MCP tool.
type Tool interface {
	Name() string
	Description() string
	// InputSchema returns the JSON Schema for the tool's input arguments.
	InputSchema() map[string]any
	// Handle executes the tool with the given arguments.
	Handle(ctx context.Context, args map[string]any) (ToolResult, error)
}

// ToolResult is the structured output of a tool call.
type ToolResult struct {
	Content []ContentBlock `json:"content"`
}

// ContentBlock is one piece of tool output (text only in phase 1).
type ContentBlock struct {
	Type string `json:"type"` // always "text"
	Text string `json:"text"`
}

// New constructs an MCP server backed by a dashboard HTTP client.
func New(client *ragclient.Client, logger *slog.Logger) *Server {
	ctx := context.Background()
	wm := NewWatcherManager(client, logger, ctx)
	s := &Server{
		rag:    client,
		tools:  make(map[string]Tool),
		logger: logger,
		wm:     wm,
	}
	// Register tools (ADR-0007 Phase 9 expansion).
	s.Register(&CodeRAGTool{client: client})
	s.Register(&RetrieveContextTool{client: client})
	s.Register(&IndexProjectTool{client: client, wm: wm})
	s.Register(&ListProjectsTool{client: client})
	s.Register(&CreateProjectTool{client: client, wm: wm})
	s.Register(&ProjectStatusTool{client: client})
	return s
}

// Register adds a tool to the registry.
func (s *Server) Register(t Tool) {
	s.tools[t.Name()] = t
}

// Serve runs the MCP JSON-RPC loop on stdio until ctx is cancelled or stdin EOF.
func (s *Server) Serve(ctx context.Context) error {
	s.logger.Info("mcp serving on stdio", "tools", len(s.tools))
	// Stop all file watchers on shutdown.
	defer func() {
		if s.wm != nil {
			s.wm.StopAll()
		}
	}()
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var msg json.RawMessage
		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("decode rpc: %w", err)
		}
		s.handleMessage(ctx, msg, encoder)
	}
}

// rpcRequest is the JSON-RPC 2.0 request envelope.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// handleMessage dispatches a single JSON-RPC message.
func (s *Server) handleMessage(ctx context.Context, raw json.RawMessage, enc *json.Encoder) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		s.sendError(enc, nil, -32700, "parse error: "+err.Error())
		return
	}
	switch req.Method {
	case "initialize":
		s.sendResult(enc, req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "bit-multi-brain-rag",
				"version": "0.1.0",
			},
		})
	case "tools/list":
		tools := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			tools = append(tools, map[string]any{
				"name":        t.Name(),
				"description": t.Description(),
				"inputSchema": t.InputSchema(),
			})
		}
		s.sendResult(enc, req.ID, map[string]any{"tools": tools})
	case "tools/call":
		s.handleToolCall(ctx, req, enc)
	default:
		s.sendError(enc, req.ID, -32601, "method not found: "+req.Method)
	}
}

// handleToolCall executes a tools/call request.
func (s *Server) handleToolCall(ctx context.Context, req rpcRequest, enc *json.Encoder) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.sendError(enc, req.ID, -32602, "invalid params: "+err.Error())
		return
	}
	tool, ok := s.tools[params.Name]
	if !ok {
		s.sendError(enc, req.ID, -32602, "unknown tool: "+params.Name)
		return
	}
	result, err := tool.Handle(ctx, params.Arguments)
	if err != nil {
		s.sendResult(enc, req.ID, map[string]any{
			"content": []ContentBlock{{Type: "text", Text: "Error: " + err.Error()}},
			"isError": true,
		})
		return
	}
	s.sendResult(enc, req.ID, result)
}

// --- JSON-RPC response helpers ---

func (s *Server) sendResult(enc *json.Encoder, id json.RawMessage, result any) {
	_ = enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func (s *Server) sendError(enc *json.Encoder, id json.RawMessage, code int, message string) {
	_ = enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

// --- CodeRAGTool ---

// CodeRAGTool performs semantic code search by delegating to the dashboard's
// /api/v1/search endpoint over HTTPS.
type CodeRAGTool struct {
	client *ragclient.Client
}

func (t *CodeRAGTool) Name() string { return "rag_search_code" }

func (t *CodeRAGTool) Description() string {
	return "Semantic search across indexed source code for a project. " +
		"Calls the bit-multi-brain-rag dashboard (remote) over HTTPS. " +
		"Returns the most relevant code chunks with file paths and similarity scores."
}

func (t *CodeRAGTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_id": map[string]any{
				"type":        "integer",
				"description": "Numeric project ID (from rag_list_projects). Preferred over project name — guaranteed unique.",
			},
			"project": map[string]any{
				"type":        "string",
				"description": "Project name (fallback if project_id not known). May collide if multiple projects share similar names.",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "Natural-language query describing the code to find.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max results (default 5).",
				"default":     5,
			},
		},
		"required": []string{"query"},
	}
}

func (t *CodeRAGTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	projectID, projectName := extractProjectArgs(args)
	query, _ := args["query"].(string)
	if projectID == 0 && projectName == "" {
		return ToolResult{}, fmt.Errorf("project_id or project is required")
	}
	if query == "" {
		return ToolResult{}, fmt.Errorf("query is required")
	}
	limit := 5
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	name, err := t.client.ResolveProjectIdentifier(ctx, projectID, projectName)
	if err != nil {
		return ToolResult{}, err
	}
	results, err := t.client.Search(ctx, name, query, limit)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: formatResults(query, name, results)}},
	}, nil
}

// extractProjectArgs pulls project_id (float64 from JSON) and project (string)
// from the args map. Returns (0, "") if neither is present.
func extractProjectArgs(args map[string]any) (int64, string) {
	var id int64
	if v, ok := args["project_id"].(float64); ok {
		id = int64(v)
	}
	name, _ := args["project"].(string)
	return id, name
}

// formatResults renders search results as human-readable Markdown
// for AI agents to consume in tool_result.
func formatResults(query, project string, results []rag.Result) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d results for %q in project %q:\n\n", len(results), query, project))
	if len(results) == 0 {
		sb.WriteString("(no matches — try a different query or verify the project is indexed)\n")
		return sb.String()
	}
	for i, r := range results {
		file := r.Meta["source_file"]
		if file == "" {
			file = "(unknown)"
		}
		sb.WriteString(fmt.Sprintf("--- Result %d (score: %.3f) ---\n", i+1, r.Score))
		sb.WriteString(fmt.Sprintf("File: %s\n", file))
		sb.WriteString(fmt.Sprintf("Symbol: %s (%s)\n", r.Meta["name"], r.Meta["symbol"]))
		sb.WriteString(fmt.Sprintf("Lines: %s-%s\n", r.Meta["start_line"], r.Meta["end_line"]))
		sb.WriteString("\n```" + r.Meta["language"] + "\n")
		sb.WriteString(r.Content)
		sb.WriteString("\n```\n\n")
	}
	return sb.String()
}

// --- RetrieveContextTool ---

// RetrieveContextTool returns search results pre-formatted as a ready-to-paste
// context string for LLM consumption. Inspired by enowx-rag's rag_retrieve_context.
type RetrieveContextTool struct {
	client *ragclient.Client
}

func (t *RetrieveContextTool) Name() string { return "rag_retrieve_context" }

func (t *RetrieveContextTool) Description() string {
	return "Semantic search that returns results as a pre-formatted context string " +
		"with [score] prefixes, ready to paste into an LLM prompt. " +
		"Use this before writing code to find relevant existing implementations."
}

func (t *RetrieveContextTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_id": map[string]any{
				"type":        "integer",
				"description": "Numeric project ID (from rag_list_projects). Preferred.",
			},
			"project": map[string]any{
				"type":        "string",
				"description": "Project name (fallback if project_id not known).",
			},
			"query": map[string]any{
				"type":        "string",
				"description": "Natural-language query describing what you're looking for.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max results (default 5).",
				"default":     5,
			},
		},
		"required": []string{"query"},
	}
}

func (t *RetrieveContextTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	projectID, projectName := extractProjectArgs(args)
	query, _ := args["query"].(string)
	if projectID == 0 && projectName == "" {
		return ToolResult{}, fmt.Errorf("project_id or project is required")
	}
	if query == "" {
		return ToolResult{}, fmt.Errorf("query is required")
	}
	limit := 5
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	name, err := t.client.ResolveProjectIdentifier(ctx, projectID, projectName)
	if err != nil {
		return ToolResult{}, err
	}
	results, err := t.client.Search(ctx, name, query, limit)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: formatContext(query, name, results)}},
	}, nil
}

// formatContext renders results as a compact context string with [score] prefixes.
// Format matches enowx-rag's rag_retrieve_context for familiarity.
func formatContext(query, project string, results []rag.Result) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Context for %q (project: %s, %d results):\n\n", query, project, len(results)))
	if len(results) == 0 {
		sb.WriteString("(no relevant context found)\n")
		return sb.String()
	}
	for _, r := range results {
		file := r.Meta["source_file"]
		if file == "" {
			file = "(unknown)"
		}
		lines := r.Meta["start_line"] + "-" + r.Meta["end_line"]
		sb.WriteString(fmt.Sprintf("[score %.3f] %s:%s (%s)\n", r.Score, file, lines, r.Meta["name"]))
		sb.WriteString(r.Content)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// --- IndexProjectTool ---

// IndexProjectTool walks the LOCAL project folder, chunks files with
// tree-sitter, and uploads pre-chunked docs to the dashboard for embedding
// + storage. This is the "remote index" path: NO mounting needed on the
// dashboard side. The MCP client (running locally) reads files, the
// dashboard only embeds + stores.
type IndexProjectTool struct {
	client *ragclient.Client
	wm     *WatcherManager
}

func (t *IndexProjectTool) Name() string { return "rag_index_project" }

func (t *IndexProjectTool) Description() string {
	return "Index a project by scanning files locally, chunking them, and uploading to the dashboard for embedding + storage. " +
		"Returns indexing statistics. Use after significant code changes to keep the RAG index fresh."
}

func (t *IndexProjectTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_id": map[string]any{
				"type":        "integer",
				"description": "Numeric project ID (from rag_create_project or rag_list_projects). Preferred.",
			},
			"project": map[string]any{
				"type":        "string",
				"description": "Project name (fallback if project_id not known).",
			},
			"root_path": map[string]any{
				"type":        "string",
				"description": "Local filesystem path to the project root (if different from registered path).",
			},
		},
	}
}

func (t *IndexProjectTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	projectID, projectName := extractProjectArgs(args)
	if projectID == 0 && projectName == "" {
		return ToolResult{}, fmt.Errorf("project_id or project is required")
	}

	// Resolve project info from dashboard.
	name, err := t.client.ResolveProjectIdentifier(ctx, projectID, projectName)
	if err != nil {
		return ToolResult{}, err
	}
	proj, err := t.client.GetProject(ctx, name)
	if err != nil {
		return ToolResult{}, fmt.Errorf("get project: %w", err)
	}

	rootPath := proj.RootPath
	if override, ok := args["root_path"].(string); ok && override != "" {
		rootPath = override
	}
	if rootPath == "" {
		return ToolResult{}, fmt.Errorf("no root_path: project has no registered root_path and none provided")
	}

	// Walk local folder + chunk + upload.
	stats, err := localIndex(ctx, t.client, name, rootPath)
	if err != nil {
		return ToolResult{}, err
	}

	// Start/refresh file watcher for auto re-index on changes.
	if t.wm != nil {
		t.wm.StartWatching(name, rootPath)
	}

	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf(
			"Indexed project %q (ID: %d).\nFiles scanned: %d\nChunks: %d\nEmbedded: %d\nStored: %d\nDuration: %s\n"+
				"File watcher active — changes will auto-reindex.\n",
			name, projectID, stats.FilesScanned, stats.Chunks, stats.Embedded, stats.Stored, stats.Duration)}},
	}, nil
}

// localIndexStats holds results from local walk + chunk + upload.
type localIndexStats struct {
	FilesScanned int
	Chunks       int
	Embedded     int
	Stored       int
	Duration     string
}

// localIndex walks the local folder, chunks files, and uploads to dashboard.
// This replaces the old "dashboard walks the filesystem" approach — no mounting.
func localIndex(ctx context.Context, client *ragclient.Client, project, rootPath string) (*localIndexStats, error) {
	start := time.Now()
	ch := chunker.New()
	stats := &localIndexStats{}

	gi, err := indexer.LoadGitignore(rootPath)
	if err != nil {
		slog.Warn("gitignore load failed", "error", err)
	}

	const batchSize = 64
	var batch []ragclient.UploadDoc

	walkErr := filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		if info.IsDir() {
			name := info.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" ||
				name == "dist" || name == "build" || name == "__pycache__" {
				return filepath.SkipDir
			}
			return nil
		}
		if !indexer.IsSourceFilePublic(path) {
			return nil
		}
		if gi != nil {
			rel, _ := filepath.Rel(rootPath, path)
			if gi.Match(rel) {
				return nil
			}
		}

		data, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("read file failed", "file", path, "error", err)
			return nil
		}
		if len(data) > 1024*1024 { // skip files > 1MB
			return nil
		}

		stats.FilesScanned++

		relPath, _ := filepath.Rel(rootPath, path)
		chunks, err := ch.ChunkFile(ctx, data, relPath)
		if err != nil {
			slog.Warn("chunk failed", "file", relPath, "error", err)
			return nil
		}

		for _, c := range chunks {
			stats.Chunks++
			doc := ragclient.UploadDoc{
				ID:      uuidV5(project, relPath, c.StartLine),
				Content: c.Content,
				Meta: map[string]string{
					"source_file": relPath,
					"language":    c.Language,
					"symbol":      c.Symbol,
					"name":        c.Name,
					"start_line":  fmt.Sprintf("%d", c.StartLine),
					"end_line":    fmt.Sprintf("%d", c.EndLine),
				},
			}
			batch = append(batch, doc)
			if len(batch) >= batchSize {
				result, err := client.UploadAndIndex(ctx, project, batch, nil)
				if err != nil {
					return err
				}
				stats.Embedded += result.Embedded
				stats.Stored += result.Indexed
				batch = batch[:0]
			}
		}
		return nil
	})
	if walkErr != nil {
		return stats, fmt.Errorf("walk: %w", walkErr)
	}

	// Flush remaining.
	if len(batch) > 0 {
		result, err := client.UploadAndIndex(ctx, project, batch, nil)
		if err != nil {
			return stats, fmt.Errorf("upload final batch: %w", err)
		}
		stats.Embedded += result.Embedded
		stats.Stored += result.Indexed
	}

	stats.Duration = time.Since(start).Round(time.Millisecond).String()
	return stats, nil
}

// uuidV5 generates a deterministic UUID v5 for (project, file, line).
func uuidV5(project, file string, line int) string {
	return uuid.NewSHA1(uuid.NameSpaceURL,
		[]byte(fmt.Sprintf("%s:%s:%d", project, file, line))).String()
}

// deltaIndex chunks and uploads ONLY the changed files (not full walk).
// Called by the file watcher when source files change.
// deletedFiles are files that no longer exist — their Qdrant points are
// stale (not actively deleted yet; skipped: Qdrant upsert is idempotent,
// re-indexing a project overwrites stale points for the same UUID set).
func deltaIndex(ctx context.Context, client *ragclient.Client, project, rootPath string, changedFiles []string) (*localIndexStats, error) {
	start := time.Now()
	ch := chunker.New()
	stats := &localIndexStats{}

	const batchSize = 64
	var batch []ragclient.UploadDoc
	var deletedRelPaths []string

	for _, absPath := range changedFiles {
		relPath, _ := filepath.Rel(rootPath, absPath)

		// Deleted/renamed files: collect for Qdrant point deletion.
		if _, err := os.Stat(absPath); err != nil {
			deletedRelPaths = append(deletedRelPaths, relPath)
			continue
		}

		data, err := os.ReadFile(absPath)
		if err != nil {
			slog.Warn("delta: read failed", "file", absPath, "error", err)
			continue
		}
		if len(data) > 1024*1024 {
			continue
		}

		stats.FilesScanned++

		chunks, err := ch.ChunkFile(ctx, data, relPath)
		if err != nil {
			slog.Warn("delta: chunk failed", "file", relPath, "error", err)
			continue
		}

		for _, c := range chunks {
			stats.Chunks++
			batch = append(batch, ragclient.UploadDoc{
				ID:      uuidV5(project, relPath, c.StartLine),
				Content: c.Content,
				Meta: map[string]string{
					"source_file": relPath,
					"language":    c.Language,
					"symbol":      c.Symbol,
					"name":        c.Name,
					"start_line":  fmt.Sprintf("%d", c.StartLine),
					"end_line":    fmt.Sprintf("%d", c.EndLine),
				},
			})
			if len(batch) >= batchSize {
				result, err := client.UploadAndIndex(ctx, project, batch, deletedRelPaths)
				if err != nil {
					return stats, fmt.Errorf("delta upload: %w", err)
				}
				stats.Embedded += result.Embedded
				stats.Stored += result.Indexed
				batch = batch[:0]
				deletedRelPaths = nil // sent with first batch
			}
		}
	}

	// Upload remaining docs + deleted files.
	if len(batch) > 0 || len(deletedRelPaths) > 0 {
		result, err := client.UploadAndIndex(ctx, project, batch, deletedRelPaths)
		if err != nil {
			return stats, fmt.Errorf("delta upload final: %w", err)
		}
		stats.Embedded += result.Embedded
		stats.Stored += result.Indexed
	}

	stats.Duration = time.Since(start).Round(time.Millisecond).String()
	return stats, nil
}

// --- ListProjectsTool ---

// ListProjectsTool returns all registered projects. Useful for agents to
// discover available project names before calling rag_search_code.
type ListProjectsTool struct {
	client *ragclient.Client
}

func (t *ListProjectsTool) Name() string { return "rag_list_projects" }

func (t *ListProjectsTool) Description() string {
	return "List all registered projects in the bit-multi-brain-rag dashboard. " +
		"Use this to discover available project names for rag_search_code."
}

func (t *ListProjectsTool) InputSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *ListProjectsTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	projects, err := t.client.ListProjects(ctx)
	if err != nil {
		return ToolResult{}, err
	}
	var sb strings.Builder
	if len(projects) == 0 {
		sb.WriteString("No projects registered. Use rag_create_project to register one.\n")
	} else {
		sb.WriteString(fmt.Sprintf("Registered projects (%d):\n\n", len(projects)))
		sb.WriteString("| ID | Name | Root Path | Domains |\n")
		sb.WriteString("|----|------|-----------|--------|\n")
		for _, p := range projects {
			sb.WriteString(fmt.Sprintf("| %d | %s | %s | %s |\n", p.ID, p.Name, p.RootPath, p.Domains))
		}
		sb.WriteString("\nUse project_id in subsequent tool calls for guaranteed uniqueness.\n")
	}
	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: sb.String()}},
	}, nil
}

// --- CreateProjectTool ---

// CreateProjectTool registers a new project in the dashboard. This is the
// onboarding tool: when an agent opens a new/existing folder, it calls this
// to register the project + trigger initial indexing. Idempotent: safe to
// call on already-registered projects.
type CreateProjectTool struct {
	client *ragclient.Client
	wm     *WatcherManager
}

func (t *CreateProjectTool) Name() string { return "rag_create_project" }

func (t *CreateProjectTool) Description() string {
	return "Register a project in the bit-multi-brain-rag dashboard and trigger initial indexing. " +
		"Idempotent: safe to call on already-registered projects (returns existing). " +
		"Use this when opening a new or existing project folder for the first time. " +
		"root_path is the LOCAL filesystem path (where the MCP client runs). " +
		"Files are scanned and chunked locally, then sent to the dashboard for embedding — no mounting needed."
}

func (t *CreateProjectTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Project name. If omitted, auto-derived from root_path (format: {leaf}-{parent}, e.g. 'mitm-nodejs').",
			},
			"root_path": map[string]any{
				"type":        "string",
				"description": "LOCAL filesystem path to the project root (where the MCP client runs). Example: '/home/user/my-app' or 'C:/code/my-app'.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "Optional human-readable description.",
			},
			"index": map[string]any{
				"type":        "boolean",
				"description": "If true (default), scan files locally + upload to dashboard for embedding immediately.",
				"default":     true,
			},
		},
		"required": []string{"root_path"},
	}
}

func (t *CreateProjectTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	name, _ := args["name"].(string)
	rootPath, _ := args["root_path"].(string)
	if rootPath == "" {
		return ToolResult{}, fmt.Errorf("root_path is required")
	}

	// Idempotent: check if a project with this root_path already exists.
	// This is the PRIMARY entry point for agents — call on every project open.
	existingByPath, _ := t.client.GetProjectByPath(ctx, rootPath)
	if existingByPath != nil {
		// Already registered by path. Return the ID immediately.
		status, _ := t.client.GetIndexStatus(ctx, existingByPath.Name)
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("Project %q already registered (ID: %d, root: %s).\n", existingByPath.Name, existingByPath.ID, existingByPath.RootPath))
		sb.WriteString(fmt.Sprintf("Use project_id=%d for all subsequent calls.\n", existingByPath.ID))
		if status != nil {
			if status.IndexedDone > 0 {
				sb.WriteString(fmt.Sprintf("Index: %d points, status: %s. Ready to search.\n", status.IndexedDone, status.Status))
			} else {
				sb.WriteString("Index: empty. Call rag_index_project to build.\n")
			}
		}
		return ToolResult{Content: []ContentBlock{{Type: "text", Text: sb.String()}}}, nil
	}

	// Auto-derive project name from root_path if not provided.
	// Uses leaf-first disambiguation: {leaf}-{parent} (e.g. "mitm-nodejs").
	if name == "" {
		existing, _ := t.client.ListProjects(ctx)
		existingNames := make(map[string]bool, len(existing))
		for _, p := range existing {
			existingNames[p.Name] = true
		}
		name = indexer.DeriveProjectNameUnique(rootPath, existingNames)
	}
	desc, _ := args["description"].(string)
	doIndex := true
	if v, ok := args["index"].(bool); ok {
		doIndex = v
	}

	// Check if name collides (same name, different path).
	existing, _ := t.client.GetProject(ctx, name)
	if existing != nil {
		// Already exists with this name but different path.
		// Name collision → auto-rename.
		existingList, _ := t.client.ListProjects(ctx)
		existingNames := make(map[string]bool, len(existingList))
		for _, p := range existingList {
			existingNames[p.Name] = true
		}
		name = indexer.ResolveNameCollision(name, existingNames)
	}

	// Create new project.
	p, err := t.client.CreateProject(ctx, name, rootPath, desc)
	if err != nil {
		return ToolResult{}, fmt.Errorf("create project: %w", err)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Project %q created (ID: %d, root: %s).\n", p.Name, p.ID, p.RootPath))
	sb.WriteString(fmt.Sprintf("IMPORTANT: Use project_id=%d in all subsequent tool calls (rag_search_code, rag_index_project, etc).\n", p.ID))

	if doIndex {
		stats, err := localIndex(ctx, t.client, name, rootPath)
		if err != nil {
			sb.WriteString(fmt.Sprintf("WARNING: indexing failed: %v\n", err))
			sb.WriteString("Call rag_index_project manually to retry.\n")
		} else {
			sb.WriteString(fmt.Sprintf("Indexing complete: %d files, %d chunks, %d embedded, %d stored (%s).\n",
				stats.FilesScanned, stats.Chunks, stats.Embedded, stats.Stored, stats.Duration))
			// Start file watcher for auto re-index.
			if t.wm != nil {
				t.wm.StartWatching(name, rootPath)
				sb.WriteString("File watcher active — changes will auto-reindex.\n")
			}
		}
	}
	sb.WriteString("\nNext steps:\n")
	sb.WriteString(fmt.Sprintf("- Call rag_search_code with project_id=%d to search\n", p.ID))
	return ToolResult{Content: []ContentBlock{{Type: "text", Text: sb.String()}}}, nil
}

// --- ProjectStatusTool ---

// ProjectStatusTool checks whether a project is registered AND indexed.
// Agents call this at session start to decide whether to auto-onboard.
type ProjectStatusTool struct {
	client *ragclient.Client
}

func (t *ProjectStatusTool) Name() string { return "rag_project_status" }

func (t *ProjectStatusTool) Description() string {
	return "Check if a project is registered and indexed. Returns registration status + " +
		"index point count + last indexing job status. Use at session start to decide " +
		"whether to call rag_create_project or rag_index_project."
}

func (t *ProjectStatusTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_id": map[string]any{
				"type":        "integer",
				"description": "Numeric project ID. Preferred.",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Project name (fallback).",
			},
		},
	}
}

func (t *ProjectStatusTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	projectID, projectName := extractProjectArgs(args)
	if projectID == 0 && projectName == "" {
		return ToolResult{}, fmt.Errorf("project_id or name is required")
	}
	var p *ragclient.Project
	var err error
	if projectID > 0 {
		p, err = t.client.GetProjectByID(ctx, projectID)
	} else {
		p, err = t.client.GetProject(ctx, projectName)
	}
	if err != nil {
		return ToolResult{}, fmt.Errorf("check project: %w", err)
	}
	var sb strings.Builder
	if p == nil {
		sb.WriteString(fmt.Sprintf("Project (ID=%d, name=%q) is NOT registered. Call rag_create_project with root_path to onboard it.\n", projectID, projectName))
		return ToolResult{Content: []ContentBlock{{Type: "text", Text: sb.String()}}}, nil
	}
	sb.WriteString(fmt.Sprintf("Project %q (ID: %d, root: %s, domains: %s).\n", p.Name, p.ID, p.RootPath, p.Domains))
	status, _ := t.client.GetIndexStatus(ctx, p.Name)
	if status == nil {
		sb.WriteString("Index status: unknown.\n")
	} else {
		sb.WriteString(fmt.Sprintf("Index status: %s, files=%d/%d, indexed=%d", status.Status, status.FilesDone, status.FilesTotal, status.IndexedDone))
		if len(status.Errors) > 0 {
			sb.WriteString(fmt.Sprintf(", errors=%d", len(status.Errors)))
		}
		sb.WriteString("\n")
		if status.IndexedDone == 0 {
			sb.WriteString("Index is empty. Call rag_index_project to build it.\n")
		}
	}
	return ToolResult{Content: []ContentBlock{{Type: "text", Text: sb.String()}}}, nil
}
