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
	// Register phase 1 tools.
	s.Register(&CodeRAGTool{client: client})
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
