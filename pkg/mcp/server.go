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
	"strings"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/ragclient"
)

// Server is the MCP stdio server.
type Server struct {
	rag    *ragclient.Client
	tools  map[string]Tool
	logger *slog.Logger
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
	s := &Server{
		rag:    client,
		tools:  make(map[string]Tool),
		logger: logger,
	}
	// Register tools (ADR-0007 Phase 9 expansion).
	s.Register(&CodeRAGTool{client: client})
	s.Register(&RetrieveContextTool{client: client})
	s.Register(&IndexProjectTool{client: client})
	s.Register(&ListProjectsTool{client: client})
	s.Register(&CreateProjectTool{client: client})
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
			"project": map[string]any{
				"type":        "string",
				"description": "Project name to search within (must already be indexed via the dashboard).",
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
		"required": []string{"project", "query"},
	}
}

func (t *CodeRAGTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	project, _ := args["project"].(string)
	query, _ := args["query"].(string)
	if project == "" || query == "" {
		return ToolResult{}, fmt.Errorf("project and query are required")
	}
	limit := 5
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	results, err := t.client.Search(ctx, project, query, limit)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: formatResults(query, project, results)}},
	}, nil
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
			"project": map[string]any{
				"type":        "string",
				"description": "Project name to search within.",
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
		"required": []string{"project", "query"},
	}
}

func (t *RetrieveContextTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	project, _ := args["project"].(string)
	query, _ := args["query"].(string)
	if project == "" || query == "" {
		return ToolResult{}, fmt.Errorf("project and query are required")
	}
	limit := 5
	if l, ok := args["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	results, err := t.client.Search(ctx, project, query, limit)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: formatContext(query, project, results)}},
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

// IndexProjectTool triggers a background indexing job for a project via the
// dashboard API. Returns job ID + initial status. Polling is the agent's
// responsibility (use rag_list_projects to check or just wait ~30s).
type IndexProjectTool struct {
	client *ragclient.Client
}

func (t *IndexProjectTool) Name() string { return "rag_index_project" }

func (t *IndexProjectTool) Description() string {
	return "Trigger background indexing for a project (re-index all source files). " +
		"Returns immediately with job status. Use after significant code changes " +
		"to keep the RAG index fresh."
}

func (t *IndexProjectTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project": map[string]any{
				"type":        "string",
				"description": "Project name to index (must already be registered in the dashboard).",
			},
		},
		"required": []string{"project"},
	}
}

func (t *IndexProjectTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	project, _ := args["project"].(string)
	if project == "" {
		return ToolResult{}, fmt.Errorf("project is required")
	}
	jobID, err := t.client.IndexProject(ctx, project)
	if err != nil {
		return ToolResult{}, err
	}
	return ToolResult{
		Content: []ContentBlock{{Type: "text", Text: fmt.Sprintf(
			"Indexing started for project %q. Job ID: %s\nStatus: queued (check dashboard for progress).\n"+
				"Indexing runs in background; search will reflect new content once complete (~30s for small projects).",
			project, jobID)}},
	}, nil
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
		sb.WriteString("| Name | Root Path | Domains |\n")
		sb.WriteString("|------|-----------|--------|\n")
		for _, p := range projects {
			sb.WriteString(fmt.Sprintf("| %s | %s | %s |\n", p.Name, p.RootPath, p.Domains))
		}
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
}

func (t *CreateProjectTool) Name() string { return "rag_create_project" }

func (t *CreateProjectTool) Description() string {
	return "Register a project in the bit-multi-brain-rag dashboard and trigger initial indexing. " +
		"Idempotent: safe to call on already-registered projects (returns existing). " +
		"Use this when opening a new or existing project folder for the first time. " +
		"The root_path must be accessible from the dashboard server (NOT the MCP client). " +
		"For local dev (dashboard in Docker), mount the source folder as a volume."
}

func (t *CreateProjectTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Project name (unique identifier, e.g. 'my-app'). Used in all other tool calls.",
			},
			"root_path": map[string]any{
				"type":        "string",
				"description": "Root path of the source code, as seen from the DASHBOARD server (not the MCP client). Example: '/code' if mounted as Docker volume.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "Optional human-readable description.",
			},
			"index": map[string]any{
				"type":        "boolean",
				"description": "If true (default), trigger background indexing immediately after creation.",
				"default":     true,
			},
		},
		"required": []string{"name", "root_path"},
	}
}

func (t *CreateProjectTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	name, _ := args["name"].(string)
	rootPath, _ := args["root_path"].(string)
	if name == "" || rootPath == "" {
		return ToolResult{}, fmt.Errorf("name and root_path are required")
	}
	desc, _ := args["description"].(string)
	doIndex := true
	if v, ok := args["index"].(bool); ok {
		doIndex = v
	}

	// Check if already registered.
	existing, _ := t.client.GetProject(ctx, name)
	if existing != nil {
		// Already exists. Check index status.
		status, _ := t.client.GetIndexStatus(ctx, name)
		var msg string
		if status != nil && status.IndexedDone > 0 {
			msg = fmt.Sprintf("Project %q already registered and indexed (%d points). Ready to search.",
				name, status.IndexedDone)
		} else {
			msg = fmt.Sprintf("Project %q already registered but not yet indexed. Call rag_index_project to build the index.", name)
		}
		return ToolResult{Content: []ContentBlock{{Type: "text", Text: msg}}}, nil
	}

	// Create new project.
	p, err := t.client.CreateProject(ctx, name, rootPath, desc)
	if err != nil {
		return ToolResult{}, fmt.Errorf("create project: %w", err)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Project %q created (ID: %d, root: %s).\n", p.Name, p.ID, p.RootPath))

	if doIndex {
		jobID, err := t.client.IndexProject(ctx, name)
		if err != nil {
			sb.WriteString(fmt.Sprintf("WARNING: indexing failed to start: %v\n", err))
			sb.WriteString("Call rag_index_project manually to retry.\n")
		} else {
			sb.WriteString(fmt.Sprintf("Indexing started (job: %s). Search will be available in ~30s.\n", jobID))
		}
	}
	sb.WriteString("\nNext steps:\n")
	sb.WriteString("- Wait ~30s for indexing to complete\n")
	sb.WriteString("- Call rag_search_code or rag_retrieve_context to search\n")
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
			"name": map[string]any{
				"type":        "string",
				"description": "Project name to check.",
			},
		},
		"required": []string{"name"},
	}
}

func (t *ProjectStatusTool) Handle(ctx context.Context, args map[string]any) (ToolResult, error) {
	name, _ := args["name"].(string)
	if name == "" {
		return ToolResult{}, fmt.Errorf("name is required")
	}
	p, err := t.client.GetProject(ctx, name)
	if err != nil {
		return ToolResult{}, fmt.Errorf("check project: %w", err)
	}
	var sb strings.Builder
	if p == nil {
		sb.WriteString(fmt.Sprintf("Project %q is NOT registered. Call rag_create_project with name + root_path to onboard it.\n", name))
		return ToolResult{Content: []ContentBlock{{Type: "text", Text: sb.String()}}}, nil
	}
	sb.WriteString(fmt.Sprintf("Project %q is registered (ID: %d, root: %s, domains: %s).\n", p.Name, p.ID, p.RootPath, p.Domains))
	status, _ := t.client.GetIndexStatus(ctx, name)
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
