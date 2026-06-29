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
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/manifest"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/ragclient"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/watcher"
)

// Server is the MCP stdio server.
type Server struct {
	rag       *ragclient.Client
	tools     map[string]Tool
	logger    *slog.Logger
	wm        *WatcherManager
	indexingMu sync.Map // key=projectID → bool (true if indexing in progress)
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
	projects map[string]string // key = project name → value = project ID (for manifest)
}

// NewWatcherManager creates a WatcherManager tied to the MCP server's context.
func NewWatcherManager(client *ragclient.Client, logger *slog.Logger, ctx context.Context) *WatcherManager {
	return &WatcherManager{
		watchers: make(map[string]*watcher.Watcher),
		projects: make(map[string]string),
		client:   client,
		logger:   logger,
		ctx:      ctx,
	}
}

// StartWatching begins watching rootPath for project. If a watcher already
// exists for this project, it is replaced.
func (wm *WatcherManager) StartWatching(project, projectID, rootPath string) {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	// Track project ID for manifest updates.
	wm.projects[project] = projectID

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

			// Update manifest with new file states.
			if pid, ok := wm.projects[project]; ok {
				if m, err := manifest.Load(pid); err == nil && m != nil {
					fileList := walkSourceFiles(rootPath, nil)
					m.ApplyUpdate(rootPath, fileList)
				}
			}
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
	s.Register(&CodeRAGTool{client: client, srv: s})
	s.Register(&RetrieveContextTool{client: client, srv: s})
	s.Register(&IndexProjectTool{client: client, wm: wm, srv: s})
	s.Register(&ListProjectsTool{client: client})
	s.Register(&CreateProjectTool{client: client, wm: wm})
	s.Register(&ProjectStatusTool{client: client})
	s.Register(&DeleteProjectTool{client: client})
	s.Register(&StatsTool{client: client})
	s.Register(&GetChunkTool{client: client})
	s.Register(&SearchAcrossTool{client: client})
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
			// Malformed JSON — log and continue, don't crash the process.
			s.logger.Warn("decode rpc error (skipping message)", "error", err)
			continue
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
// Panic recovery prevents MCP process crash (which would break the session).
func (s *Server) handleMessage(ctx context.Context, raw json.RawMessage, enc *json.Encoder) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("panic in handleMessage", "panic", r)
			s.sendError(enc, nil, -32603, fmt.Sprintf("internal error: %v", r))
		}
	}()

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
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("panic in tool call", "tool", req.Method, "panic", r)
			s.sendResult(enc, req.ID, map[string]any{
				"content": []ContentBlock{{Type: "text", Text: fmt.Sprintf("Internal error (recovered): %v", r)}},
				"isError": true,
			})
		}
	}()

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
	srv    *Server // for auto-onboard (indexingMu, wm)
}

func (t *CodeRAGTool) Name() string { return "rag_search_code" }

func (t *CodeRAGTool) Description() string {
	return "Semantic search across indexed source code for a project. Uses hybrid retrieval (dense embeddings + BM25 keyword + RRF fusion). " +
		"Returns the most relevant code chunks with file paths, line numbers, and similarity scores. " +
		"PREFERRED over manual Grep/Glob exploration — one call replaces multiple search round trips."
}

func (t *CodeRAGTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Natural-language query describing the code to find.",
			},
			"root_path": map[string]any{
				"type":        "string",
				"description": "Project root path (auto-resolves project). Use this if you don't have project_id.",
			},
			"project_id": map[string]any{
				"type":        "integer",
				"description": "Numeric project ID (optional, auto-resolved from root_path if omitted).",
			},
			"project": map[string]any{
				"type":        "string",
				"description": "Project name (fallback).",
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
	query, _ := args["query"].(string)
	if query == "" {
		return ToolResult{}, fmt.Errorf("query is required")
	}

	// Auto-onboard: resolve project, auto-create+index if needed.
	name, ready, msg := ensureProject(ctx, t.srv, t.client, args)
	if !ready {
		return ToolResult{Content: []ContentBlock{{Type: "text", Text: msg}}}, nil
	}

	limit := 5
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	results, err := t.client.Search(ctx, name, query, limit)
	if err != nil {
		return ToolResult{}, err
	}
	// If no results (indexed but query didn't match), give guidance.
	if len(results) == 0 {
		return ToolResult{Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf(
			"No matches for %q in project %q. Try different keywords or use Grep.",
			query, name)}}}, nil
	}
	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: formatResults(query, name, results)}},
	}, nil
}

// emptySearchMessage diagnoses why search returned 0 results and returns
// an actionable message for the agent.
func emptySearchMessage(ctx context.Context, client *ragclient.Client, name string, projectID int64) string {
	stats, err := client.GetStats(ctx, name)
	if err == nil && stats.PointsCount > 0 {
		// Indexed but no match — query didn't hit anything.
		return fmt.Sprintf("No matches for query in project %q (%d chunks indexed).\n"+
			"Try different keywords or a more specific phrase.", name, stats.PointsCount)
	}

	// 0 points — either not indexed, or project folder has no source files.
	proj, pErr := client.GetProject(ctx, name)
	if pErr != nil {
		return fmt.Sprintf("No results from project %q and could not fetch project info: %v", name, pErr)
	}

	pid := projectID
	if pid == 0 {
		pid = proj.ID
	}

	// Walk root_path to check if there are source files on disk.
	sourceFileCount := 0
	if proj.RootPath != "" {
		sourceFileCount = countSourceFiles(proj.RootPath)
	}

	if sourceFileCount == 0 {
		return fmt.Sprintf(
			"No results — project %q has no source files to index (root_path: %q).\n"+
				"The folder may be empty or contain no recognized source file types.",
			name, proj.RootPath)
	}

	return fmt.Sprintf(
		"No results — project %q is not indexed yet (0 chunks, but %d source files found at %q).\n"+
			"Call rag_index_project with project_id=%d to index, then search again.\n"+
			"First index scans all source files (~20s for ~100 files). Subsequent indexes use manifest delta (only changed files).",
		name, sourceFileCount, proj.RootPath, pid)
}

// countSourceFiles walks a path and counts recognized source files.
func countSourceFiles(rootPath string) int {
	count := 0
	filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if indexer.ShouldSkipDirPublic(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if indexer.IsSourceFilePublic(path) {
			count++
		}
		return nil
	})
	return count
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

// resolveProject is the unified project resolver. It tries in order:
// 1. project_id (numeric, most precise)
// 2. project (name)
// 3. root_path (auto-resolve by path via dashboard)
// Returns the resolved project name, or empty string + error.
// ensureProject is the auto-onboard entry point. Called by search tools when
// project resolution fails or project has 0 points. It auto-creates the project,
// starts async indexing, and returns a guidance message for the agent.
//
// Returns:
//   - (projectName, true, nil) if project exists and is indexed — proceed with search
//   - ("", false, message) if indexing started or in progress — return message to agent
func ensureProject(ctx context.Context, srv *Server, client *ragclient.Client, args map[string]any) (string, bool, string) {
	rootPath, _ := args["root_path"].(string)
	projectID, projectName := extractProjectArgs(args)

	// Step 1: Resolve project (by ID, name, or root_path).
	var proj *ragclient.Project
	if projectID > 0 || projectName != "" {
		name, err := client.ResolveProjectIdentifier(ctx, projectID, projectName)
		if err == nil {
			proj, _ = client.GetProject(ctx, name)
		}
	} else if rootPath != "" {
		proj, _ = client.GetProjectByPath(ctx, rootPath)
	}

	// Step 2: If project not found, auto-create.
	if proj == nil && rootPath != "" {
		// Derive project name from path.
		existing, _ := client.ListProjects(ctx)
		existingNames := make(map[string]bool, len(existing))
		for _, p := range existing {
			existingNames[p.Name] = true
		}
		name := indexer.DeriveProjectNameUnique(rootPath, existingNames)
		proj, _ = client.CreateProject(ctx, name, rootPath, "")
	}

	if proj == nil {
		return "", false, fmt.Sprintf(
			"Project not found. Provide root_path so it can be auto-created.\n" +
				"Example: rag_search_code(root_path=\"D:/path/to/project\", query=\"...\")")
	}

	// Step 3: Check if already indexed.
	stats, _ := client.GetStats(ctx, proj.Name)
	if stats != nil && stats.PointsCount > 0 {
		return proj.Name, true, "" // Ready to search.
	}

	// Step 4: Not indexed. Check if already indexing.
	pidKey := fmt.Sprintf("%d", proj.ID)
	if srv != nil {
		if _, ok := srv.indexingMu.Load(pidKey); ok {
			// Already indexing in background.
			fileCount := 0
			if proj.RootPath != "" {
				fileCount = countSourceFiles(proj.RootPath)
			}
			estMin := 2
			if fileCount > 5000 {
				estMin = 5
			} else if fileCount > 1000 {
				estMin = 3
			}
			return "", false, fmt.Sprintf(
				"Project %q is still indexing in background (%d files, est. %d min).\n"+
					"While waiting, use Grep to search code directly.\n"+
					"Retry rag_search_code with root_path=%q in ~%d seconds.",
				proj.Name, fileCount, estMin, proj.RootPath, estMin*30)
		}

		// Step 5: Start indexing.
		srv.indexingMu.Store(pidKey, true)
		go func() {
			defer srv.indexingMu.Delete(pidKey)
			bgCtx := context.Background()
			pf := buildPatternFilter(args)
			_, _, err := IndexWithManifest(bgCtx, client, *proj, proj.RootPath, pf)
			if err != nil {
				slog.Error("auto-onboard index failed", "project", proj.Name, "error", err)
			} else {
				slog.Info("auto-onboard index complete", "project", proj.Name)
			}
			// Start watcher after index.
			if srv.wm != nil {
				srv.wm.StartWatching(proj.Name, pidKey, proj.RootPath)
			}
		}()
	}

	// Step 6: Return guidance message.
	fileCount := 0
	if proj.RootPath != "" {
		fileCount = countSourceFiles(proj.RootPath)
	}
	estMin := 2
	if fileCount > 5000 {
		estMin = 5
	} else if fileCount > 1000 {
		estMin = 3
	}
	return "", false, fmt.Sprintf(
		"Project %q not indexed yet (%d files detected). Indexing started in background (est. %d min).\n"+
			"While waiting, use Grep to search code directly.\n"+
			"Retry rag_search_code with root_path=%q in ~%d seconds.",
		proj.Name, fileCount, estMin, proj.RootPath, estMin*30)
}

func resolveProject(ctx context.Context, client *ragclient.Client, args map[string]any) (string, error) {
	projectID, projectName := extractProjectArgs(args)
	if projectID > 0 || projectName != "" {
		return client.ResolveProjectIdentifier(ctx, projectID, projectName)
	}
	// Try root_path auto-resolve.
	if rootPath, ok := args["root_path"].(string); ok && rootPath != "" {
		p, err := client.GetProjectByPath(ctx, rootPath)
		if err == nil && p != nil {
			return p.Name, nil
		}
		return "", fmt.Errorf("no project found for root_path %q — call rag_create_project first", rootPath)
	}
	return "", fmt.Errorf("project_id, project, or root_path is required")
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
	srv    *Server
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
			"query": map[string]any{
				"type":        "string",
				"description": "Natural-language query describing what you're looking for.",
			},
			"root_path": map[string]any{
				"type":        "string",
				"description": "Project root path (auto-resolves project). Use this if you don't have project_id.",
			},
			"project_id": map[string]any{
				"type":        "integer",
				"description": "Numeric project ID (optional, auto-resolved from root_path if omitted).",
			},
			"project": map[string]any{
				"type":        "string",
				"description": "Project name (fallback).",
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
	query, _ := args["query"].(string)
	if query == "" {
		return ToolResult{}, fmt.Errorf("query is required")
	}

	// Auto-onboard: resolve project, auto-create+index if needed.
	name, ready, msg := ensureProject(ctx, t.srv, t.client, args)
	if !ready {
		return ToolResult{Content: []ContentBlock{{Type: "text", Text: msg}}}, nil
	}

	limit := 5
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	results, err := t.client.Search(ctx, name, query, limit)
	if err != nil {
		return ToolResult{}, err
	}
	if len(results) == 0 {
		return ToolResult{Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf(
			"No context found for %q in project %q. Try different keywords or use Grep.",
			query, name)}}}, nil
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
	srv    *Server // for indexing guard
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
			"include_patterns": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Glob patterns for files to include (e.g. [\"**/*.ts\", \"**/*.go\"]). Empty = all source files.",
			},
			"exclude_patterns": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Glob patterns to exclude (e.g. [\"**/test/**\", \"**/*.generated.ts\"]). Applied after includes.",
			},
		},
	}
}

func (t *IndexProjectTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	projectID, projectName := extractProjectArgs(args)
	if projectID == 0 && projectName == "" {
		return ToolResult{}, fmt.Errorf("project_id or project is required")
	}

	// Guard: reject if already indexing this project.
	if t.srv != nil {
		if _, ok := t.srv.indexingMu.Load(fmt.Sprintf("%d", projectID)); ok {
			return ToolResult{Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf(
				"Project %d is already being indexed in the background.\n"+
					"Poll rag_project_status to check progress. Do not call rag_index_project again.",
				projectID)}}}, nil
		}
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

	// Build pattern filter from optional include/exclude args.
	pf := buildPatternFilter(args)

	// Quick check: if manifest exists and no changes, return instantly.
	pid := fmt.Sprintf("%d", proj.ID)
	m, _ := manifest.Load(pid)
	if m != nil {
		fileList := walkSourceFiles(rootPath, pf)
		diff := m.Compare(rootPath, fileList)
		if !diff.HasChanges() {
			// Up to date — start watcher and return immediately.
			if t.wm != nil {
				t.wm.StartWatching(name, pid, rootPath)
			}
			return ToolResult{
				Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf(
					"Project %q is already up to date — no changes detected since last index (%d files tracked).\n"+
						"File watcher active — changes will auto-reindex.",
					name, len(fileList))}},
			}, nil
		}

		// Few changes → sync (fast, <30s even on GPU).
		changedCount := len(diff.ChangedFiles())
		if changedCount <= 50 {
			stats, wasDelta, err := IndexWithManifest(ctx, t.client, *proj, rootPath, pf)
			if err != nil {
				return ToolResult{}, err
			}
			if t.wm != nil {
				t.wm.StartWatching(name, pid, rootPath)
			}
			mode := "delta"
			if !wasDelta {
				mode = "full"
			}
			return ToolResult{
				Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf(
					"Indexed [%s]: %d files, %d chunks, %d embedded, %d stored (%s).\nFile watcher active.",
					mode, stats.FilesScanned, stats.Chunks, stats.Embedded, stats.Stored, stats.Duration)}}}, nil
		}

		// Many changes → async (return immediately, index in background).
		pidKey := fmt.Sprintf("%d", proj.ID)
		if t.srv != nil {
			t.srv.indexingMu.Store(pidKey, true)
		}
		go func() {
			defer func() {
				if t.srv != nil {
					t.srv.indexingMu.Delete(pidKey)
				}
			}()
			bgCtx := context.Background()
			stats, wasDelta, err := IndexWithManifest(bgCtx, t.client, *proj, rootPath, pf)
			if err != nil {
				slog.Error("async index failed", "project", name, "error", err)
				return
			}
			mode := "delta"
			if !wasDelta {
				mode = "full"
			}
			slog.Info("async index complete", "project", name, "mode", mode,
				"files", stats.FilesScanned, "chunks", stats.Chunks, "embedded", stats.Embedded, "duration", stats.Duration)
			// Start watcher after async index.
			if t.wm != nil {
				t.wm.StartWatching(name, pid, rootPath)
			}
		}()

		return ToolResult{
			Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf(
				"Indexing started in background for %q (%d files changed).\n"+
					"This will take a few minutes. You can:\n"+
					"1. Search immediately with the existing index (rag_search_code)\n"+
					"2. Poll progress: call rag_project_status with project_id=%d every 30s\n"+
					"3. Watch live progress: http://localhost:8081/projects/%d\n"+
					"Do NOT call rag_index_project again — it will detect in-progress indexing and skip.",
				name, changedCount, proj.ID, proj.ID)}}}, nil
	}

	// No manifest → first index. For large projects, run async.
	stats, wasDelta, err := IndexWithManifest(ctx, t.client, *proj, rootPath, pf)
	if err != nil {
		return ToolResult{}, err
	}
	if t.wm != nil {
		t.wm.StartWatching(name, pid, rootPath)
	}

	mode := "full"
	if wasDelta {
		mode = "delta"
	}
	if stats.FilesScanned == 0 && !wasDelta {
		return ToolResult{
			Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf(
				"Project %q is already up to date — no changes since last index.\n", name)}}}, nil
	}

	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf(
			"Indexed project %q (ID: %d) [%s].\nFiles: %d, Chunks: %d, Embedded: %d, Stored: %d (%s)\n"+
				"File watcher active — changes will auto-reindex.\n",
			name, projectID, mode, stats.FilesScanned, stats.Chunks, stats.Embedded, stats.Stored, stats.Duration)}}}, nil
}

// LocalIndexStats holds results from local walk + chunk + upload.
type LocalIndexStats = localIndexStats

// LocalIndex walks a local folder, chunks files, and uploads to dashboard.
// Exported for CLI use.
func LocalIndex(ctx context.Context, client *ragclient.Client, project, rootPath string) (*LocalIndexStats, error) {
	return localIndex(ctx, client, project, rootPath)
}

// LocalIndexWithPatterns is like LocalIndex but with include/exclude pattern filter.
func LocalIndexWithPatterns(ctx context.Context, client *ragclient.Client, project, rootPath string, pf *indexer.PatternFilter) (*LocalIndexStats, error) {
	return localIndexWithPatterns(ctx, client, project, rootPath, pf)
}

// DeltaIndex chunks and uploads ONLY changed files.
// Exported for CLI watcher use.
func DeltaIndex(ctx context.Context, client *ragclient.Client, project, rootPath string, changedFiles []string) (*LocalIndexStats, error) {
	return deltaIndex(ctx, client, project, rootPath, changedFiles)
}
type localIndexStats struct {
	FilesScanned int
	Chunks       int
	Embedded     int
	Stored       int
	Duration     string
	ScannedFiles []string // rel paths of all files processed (for manifest update)
}

// localIndex walks the local folder, chunks files, and uploads to dashboard.
func localIndex(ctx context.Context, client *ragclient.Client, project, rootPath string) (*localIndexStats, error) {
	return localIndexWithPatterns(ctx, client, project, rootPath, nil)
}

// IndexWithManifest is the manifest-aware index entry point.
// It checks the local manifest for the project and decides:
//   - No manifest → full index
//   - Manifest exists, few changes → delta reindex only changed files
//   - Manifest exists, many changes (>50%) → full reindex
//   - Manifest says indexed but dashboard has 0 points → full reindex (drift)
//
// After indexing, the manifest is updated with new file states.
func IndexWithManifest(ctx context.Context, client *ragclient.Client, proj ragclient.Project, rootPath string, pf *indexer.PatternFilter) (*LocalIndexStats, bool, error) {
	pid := fmt.Sprintf("%d", proj.ID)

	// Load existing manifest.
	m, err := manifest.Load(pid)
	if err != nil {
		slog.Warn("manifest load failed, doing full index", "error", err)
	}

	// Sync drift check: if manifest exists but dashboard has 0 points,
	// the collection was likely deleted. Force full reindex.
	if m != nil {
		stats, err := client.GetStats(ctx, proj.Name)
		if err == nil && stats.PointsCount == 0 {
			slog.Info("manifest drift: dashboard has 0 points, full reindex", "project", proj.Name)
			m = nil // force full
		}
	}

	// No manifest → full index.
	if m == nil {
		m = manifest.New(pid, proj.Name, rootPath)
		stats, err := localIndexWithPatterns(ctx, client, proj.Name, rootPath, pf)
		if err != nil {
			return nil, false, err
		}
		// Save manifest with all scanned files.
		m.ApplyUpdate(rootPath, stats.ScannedFiles)
		return stats, false, nil
	}

	// Manifest exists → walk to get current file list, then diff.
	fileList := walkSourceFiles(rootPath, pf)
	diff := m.Compare(rootPath, fileList)

	if !diff.HasChanges() {
		// Nothing changed — return empty stats.
		slog.Info("manifest: no changes detected, skipping index", "project", proj.Name)
		return &localIndexStats{}, false, nil
	}

	// If >50% of files changed, full reindex is more efficient.
	// But first, delete stale points for files that no longer exist on disk.
	changedCount := len(diff.ChangedFiles())
	if len(fileList) > 0 && changedCount*100/len(fileList) > 50 {
		slog.Info("manifest: >50% files changed, full reindex", "project", proj.Name, "changed", changedCount, "total", len(fileList))

		// Clean up: delete points for files in manifest but not on disk.
		if len(diff.Deleted) > 0 {
			slog.Info("cleaning stale points before reindex", "project", proj.Name, "deleted_files", len(diff.Deleted))
			if _, err := client.UploadAndIndex(ctx, proj.Name, nil, diff.Deleted); err != nil {
				slog.Warn("stale cleanup failed (continuing)", "error", err)
			}
		}

		stats, err := localIndexWithPatterns(ctx, client, proj.Name, rootPath, pf)
		if err != nil {
			return nil, false, err
		}
		m.ApplyUpdate(rootPath, stats.ScannedFiles)
		return stats, false, nil
	}

	// Delta reindex only changed files.
	slog.Info("manifest: delta reindex", "project", proj.Name, "added", len(diff.Added), "modified", len(diff.Modified), "deleted", len(diff.Deleted))

	// Convert rel paths to abs for deltaIndex.
	absChanged := make([]string, 0, changedCount)
	for _, rel := range diff.Added {
		absChanged = append(absChanged, filepath.Join(rootPath, rel))
	}
	for _, rel := range diff.Modified {
		absChanged = append(absChanged, filepath.Join(rootPath, rel))
	}
	for _, rel := range diff.Deleted {
		absChanged = append(absChanged, filepath.Join(rootPath, rel))
	}

	stats, err := deltaIndex(ctx, client, proj.Name, rootPath, absChanged)
	if err != nil {
		return nil, false, err
	}

	// Update manifest: re-walk to get accurate current file list.
	m.ApplyUpdate(rootPath, fileList)
	return stats, true, nil
}

// walkSourceFiles walks rootPath and returns relative paths of all source files.
// Used for manifest comparison without reading file contents.
func walkSourceFiles(rootPath string, pf *indexer.PatternFilter) []string {
	gi, err := indexer.LoadGitignore(rootPath)
	if err != nil {
		slog.Warn("gitignore load failed", "error", err)
	}

	var files []string
	filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if indexer.ShouldSkipDirPublic(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !indexer.IsSourceFilePublic(path) {
			return nil
		}
		rel, _ := filepath.Rel(rootPath, path)
		if gi != nil && gi.Match(rel) {
			return nil
		}
		if pf != nil && !pf.Match(rel) {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	return files
}

// localIndexWithPatterns is the core walk+chunk+upload with optional pattern filter.
// Reports progress to dashboard via client.ReportProgress for live UI updates.
func localIndexWithPatterns(ctx context.Context, client *ragclient.Client, project, rootPath string, pf *indexer.PatternFilter) (*localIndexStats, error) {
	start := time.Now()
	ch := chunker.New()
	stats := &localIndexStats{}

	gi, err := indexer.LoadGitignore(rootPath)
	if err != nil {
		slog.Warn("gitignore load failed", "error", err)
	}

	// Phase 1: Walk to count total source files (fast — no file reads).
	client.ReportProgress(ctx, project, "counting", 0, 0, "Scanning files...")
	totalFiles := 0
	filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if indexer.ShouldSkipDirPublic(info.Name()) {
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
		if pf != nil {
			rel, _ := filepath.Rel(rootPath, path)
			if !pf.Match(rel) {
				return nil
			}
		}
		totalFiles++
		return nil
	})
	client.ReportProgress(ctx, project, "indexing", 0, totalFiles, fmt.Sprintf("Indexing %d files...", totalFiles))

	// Phase 2: Walk again — read + chunk + upload with progress.
	const batchSize = 64
	var batch []ragclient.UploadDoc

	walkErr := filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if indexer.ShouldSkipDirPublic(info.Name()) {
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
		if pf != nil {
			rel, _ := filepath.Rel(rootPath, path)
			if !pf.Match(rel) {
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
		if len(strings.TrimSpace(string(data))) == 0 { // skip empty/whitespace-only files
			return nil
		}

		stats.FilesScanned++
		relPath, _ := filepath.Rel(rootPath, path)
		stats.ScannedFiles = append(stats.ScannedFiles, relPath)
		chunks, err := ch.ChunkFile(ctx, data, relPath)
		if err != nil {
			slog.Warn("chunk failed", "file", relPath, "error", err)
			return nil
		}

		for _, c := range chunks {
			if len(strings.TrimSpace(c.Content)) == 0 { // skip empty chunks
				continue
			}
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
		// Report progress after each file
		client.ReportProgress(ctx, project, "indexing", stats.FilesScanned, totalFiles, relPath)
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
	client.ReportProgress(ctx, project, "done", stats.FilesScanned, totalFiles, fmt.Sprintf(
		"Done: %d files, %d chunks, %d embedded (%s)", stats.FilesScanned, stats.Chunks, stats.Embedded, stats.Duration))
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
		if len(strings.TrimSpace(string(data))) == 0 { // skip empty/whitespace-only files
			continue
		}

		stats.FilesScanned++
		stats.ScannedFiles = append(stats.ScannedFiles, relPath)

		chunks, err := ch.ChunkFile(ctx, data, relPath)
		if err != nil {
			slog.Warn("delta: chunk failed", "file", relPath, "error", err)
			continue
		}

		for _, c := range chunks {
			if len(strings.TrimSpace(c.Content)) == 0 { // skip empty chunks
				continue
			}
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
			"include_patterns": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Glob patterns for files to include (e.g. [\"**/*.ts\"]). Empty = all source files.",
			},
			"exclude_patterns": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Glob patterns to exclude (e.g. [\"**/test/**\"]).",
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
		pf := buildPatternFilter(args)
		// Estimate file count to decide sync vs async.
		fileList := walkSourceFiles(rootPath, pf)
		fileCount := len(fileList)

		if fileCount > 200 {
			// Large project: async index, return immediately.
			pidKey := fmt.Sprintf("%d", p.ID)
			if t.wm != nil {
				t.wm.StartWatching(name, pidKey, rootPath)
			}
			go func() {
				bgCtx := context.Background()
				_, _, err := IndexWithManifest(bgCtx, t.client, *p, rootPath, pf)
				if err != nil {
					slog.Error("async create+index failed", "project", name, "error", err)
				}
			}()
			sb.WriteString(fmt.Sprintf("Indexing started in background (%d files).\n", fileCount))
			sb.WriteString("You can search immediately once initial indexing completes.\n")
			sb.WriteString(fmt.Sprintf("Poll progress: call rag_project_status with project_id=%d\n", p.ID))
		} else {
			// Small project: sync index.
			stats, wasDelta, err := IndexWithManifest(ctx, t.client, *p, rootPath, pf)
			if err != nil {
				sb.WriteString(fmt.Sprintf("WARNING: indexing failed: %v\n", err))
				sb.WriteString("Call rag_index_project manually to retry.\n")
			} else {
				mode := "full"
				if wasDelta {
					mode = "delta"
				}
				if stats.FilesScanned == 0 && !wasDelta {
					sb.WriteString("Index: already up to date.\n")
				} else {
					sb.WriteString(fmt.Sprintf("Indexing complete [%s]: %d files, %d chunks, %d embedded, %d stored (%s).\n",
						mode, stats.FilesScanned, stats.Chunks, stats.Embedded, stats.Stored, stats.Duration))
				}
				if t.wm != nil {
					t.wm.StartWatching(name, fmt.Sprintf("%d", p.ID), rootPath)
					sb.WriteString("File watcher active.\n")
				}
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
			"root_path": map[string]any{
				"type":        "string",
				"description": "Project root path (auto-resolves project).",
			},
			"project_id": map[string]any{
				"type":        "integer",
				"description": "Numeric project ID (optional).",
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
		if rootPath, ok := args["root_path"].(string); ok && rootPath != "" {
			p, err := t.client.GetProjectByPath(ctx, rootPath)
			if err == nil && p != nil {
				projectID = p.ID
				projectName = p.Name
			}
		}
	}
	if projectID == 0 && projectName == "" {
		return ToolResult{}, fmt.Errorf("project_id, name, or root_path is required")
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
	// Check index status via job manager first, then fall back to collection stats.
	status, _ := t.client.GetIndexStatus(ctx, p.Name)
	hasIndexInfo := false
	if status != nil && status.IndexedDone > 0 {
		hasIndexInfo = true
		sb.WriteString(fmt.Sprintf("Index status: %s, files=%d/%d, indexed=%d", status.Status, status.FilesDone, status.FilesTotal, status.IndexedDone))
		if len(status.Errors) > 0 {
			sb.WriteString(fmt.Sprintf(", errors=%d", len(status.Errors)))
		}
		sb.WriteString("\n")
		if status.IndexedDone == 0 {
			sb.WriteString("Index is empty. Call rag_index_project to build it.\n")
		}
	}
	// Fallback: if job status is empty, check collection stats directly (embedded mode).
	if !hasIndexInfo {
		stats, statsErr := t.client.GetStats(ctx, p.Name)
		if statsErr == nil && stats.PointsCount > 0 {
			sb.WriteString(fmt.Sprintf("Index: %d chunks indexed (status: %s). Ready to search.\n", stats.PointsCount, stats.Status))
		} else {
			sb.WriteString("Index: empty. Call rag_index_project to build it.\n")
		}
	}
	return ToolResult{Content: []ContentBlock{{Type: "text", Text: sb.String()}}}, nil
}

// --- DeleteProjectTool ---

// DeleteProjectTool removes a project and its Qdrant collection.
type DeleteProjectTool struct {
	client *ragclient.Client
}

func (t *DeleteProjectTool) Name() string { return "rag_delete_project" }

func (t *DeleteProjectTool) Description() string {
	return "Delete a project and its entire vector index from the dashboard. " +
		"This is irreversible — all indexed chunks for the project are removed from Qdrant."
}

func (t *DeleteProjectTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_id": map[string]any{
				"type":        "integer",
				"description": "Numeric project ID to delete.",
			},
		},
		"required": []string{"project_id"},
	}
}

func (t *DeleteProjectTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	projectID, _ := args["project_id"].(float64)
	if projectID == 0 {
		return ToolResult{}, fmt.Errorf("project_id is required")
	}
	status, err := t.client.DeleteProject(ctx, int64(projectID))
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf(
			"Project %d deleted (%s). All vector data removed from Qdrant.\n",
			int64(projectID), status)}},
	}, nil
}

// --- StatsTool ---

// StatsTool returns collection statistics for a project.
type StatsTool struct {
	client *ragclient.Client
}

func (t *StatsTool) Name() string { return "rag_stats" }

func (t *StatsTool) Description() string {
	return "Get collection statistics for a project: total indexed points, vector dimensions, collection status."
}

func (t *StatsTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_id": map[string]any{
				"type":        "integer",
				"description": "Numeric project ID. Preferred.",
			},
			"project": map[string]any{
				"type":        "string",
				"description": "Project name (fallback).",
			},
		},
	}
}

func (t *StatsTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	projectID, projectName := extractProjectArgs(args)
	if projectID == 0 && projectName == "" {
		return ToolResult{}, fmt.Errorf("project_id or project is required")
	}
	name, err := t.client.ResolveProjectIdentifier(ctx, projectID, projectName)
	if err != nil {
		return ToolResult{}, err
	}
	stats, err := t.client.GetStats(ctx, name)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf(
			"Project %q stats:\nPoints (chunks): %d\nVector dimensions: %d\nCollection status: %s\n",
			name, stats.PointsCount, stats.VectorsSize, stats.Status)}},
	}, nil
}

// --- GetChunkTool ---

// GetChunkTool fetches a single chunk by its point ID for deeper inspection.
type GetChunkTool struct {
	client *ragclient.Client
}

func (t *GetChunkTool) Name() string { return "rag_get_chunk" }

func (t *GetChunkTool) Description() string {
	return "Fetch a single indexed chunk by its point ID. Use after rag_search_code to get the full content of a specific result."
}

func (t *GetChunkTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_id": map[string]any{
				"type":        "integer",
				"description": "Numeric project ID. Preferred.",
			},
			"project": map[string]any{
				"type":        "string",
				"description": "Project name (fallback).",
			},
			"point_id": map[string]any{
				"type":        "string",
				"description": "The chunk's point ID (UUID from search results).",
			},
		},
		"required": []string{"point_id"},
	}
}

func (t *GetChunkTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	projectID, projectName := extractProjectArgs(args)
	if projectID == 0 && projectName == "" {
		return ToolResult{}, fmt.Errorf("project_id or project is required")
	}
	pointID, _ := args["point_id"].(string)
	if pointID == "" {
		return ToolResult{}, fmt.Errorf("point_id is required")
	}
	name, err := t.client.ResolveProjectIdentifier(ctx, projectID, projectName)
	if err != nil {
		return ToolResult{}, err
	}
	chunk, err := t.client.GetChunk(ctx, name, pointID)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf(
			"Chunk %s:\nFile: %s\nSymbol: %s (%s)\nLines: %s-%s\n\n```%s\n%s\n```\n",
			pointID,
			chunk.Meta["source_file"],
			chunk.Meta["name"],
			chunk.Meta["symbol"],
			chunk.Meta["start_line"],
			chunk.Meta["end_line"],
			chunk.Meta["language"],
			chunk.Content)}},
	}, nil
}

// --- SearchAcrossTool ---

// SearchAcrossTool searches across ALL indexed projects at once.
type SearchAcrossTool struct {
	client *ragclient.Client
}

func (t *SearchAcrossTool) Name() string { return "rag_search_across" }

func (t *SearchAcrossTool) Description() string {
	return "Semantic search across ALL indexed projects. Returns results with project name + score. " +
		"Useful when the agent doesn't know which project contains the relevant code."
}

func (t *SearchAcrossTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Natural-language query.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Max results per project (default 3).",
				"default":     3,
			},
		},
		"required": []string{"query"},
	}
}

func (t *SearchAcrossTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	query, _ := args["query"].(string)
	if query == "" {
		return ToolResult{}, fmt.Errorf("query is required")
	}
	limit := 3
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}

	projects, err := t.client.ListProjects(ctx)
	if err != nil {
		return ToolResult{}, err
	}
	if len(projects) == 0 {
		return ToolResult{Content: []ContentBlock{{Type: "text", Text: "No projects registered."}}}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Searching across %d projects for %q:\n\n", len(projects), query))
	totalResults := 0
	for _, p := range projects {
		results, err := t.client.Search(ctx, p.Name, query, limit)
		if err != nil {
			sb.WriteString(fmt.Sprintf("[ERROR] %s: %v\n", p.Name, err))
			continue
		}
		if len(results) == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("--- %s (project_id=%d) ---\n", p.Name, p.ID))
		for i, r := range results {
			totalResults++
			sb.WriteString(fmt.Sprintf("%d. [%.3f] %s:%s (%s)\n",
				i+1, r.Score, r.Meta["source_file"], r.Meta["start_line"], r.Meta["name"]))
		}
		sb.WriteString("\n")
	}
	if totalResults == 0 {
		sb.WriteString("No results found in any project.\n")
	} else {
		sb.WriteString(fmt.Sprintf("Total: %d results across %d projects.\n", totalResults, len(projects)))
	}
	return ToolResult{Content: []ContentBlock{{Type: "text", Text: sb.String()}}}, nil
}

// buildPatternFilter extracts include/exclude pattern arrays from tool args.
func buildPatternFilter(args map[string]any) *indexer.PatternFilter {
	var includes, excludes []string
	if v, ok := args["include_patterns"].([]any); ok {
		for _, p := range v {
			if s, ok := p.(string); ok {
				includes = append(includes, s)
			}
		}
	}
	if v, ok := args["exclude_patterns"].([]any); ok {
		for _, p := range v {
			if s, ok := p.(string); ok {
				excludes = append(excludes, s)
			}
		}
	}
	if len(includes) == 0 && len(excludes) == 0 {
		return nil
	}
	return indexer.NewPatternFilter(includes, excludes)
}
