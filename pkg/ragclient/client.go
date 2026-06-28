// Package ragclient is an HTTP client for the bit-multi-brain-rag dashboard API.
//
// It is used by the local MCP server to query a remote dashboard (e.g. deployed
// on Easypanel) over HTTPS, so that only the dashboard endpoint is exposed
// publicly — Qdrant + embedder remain INTERNAL to the deployment network.
//
// Wire path:
//
//   AI agent (Claude/Factory/OpenCode)
//        │  stdio JSON-RPC
//        ▼
//   bit-rag MCP (local)              ──┐
//        │  HTTPS POST /api/v1/search   │   Single public endpoint.
//        ▼                              │   Auth: Bearer API key.
//   dashboard (Easypanel) :8081       ──┘
//        │  internal network
//        ▼
//   embedder + Qdrant (no public ports)
package ragclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
)

// Config holds the HTTP client configuration.
type Config struct {
	// BaseURL is the dashboard root (e.g. "https://bit-rag.your-domain.com").
	// Trailing slash is stripped automatically.
	BaseURL string

	// APIKey is the Bearer token (DASHBOARD_API_KEYS). REQUIRED.
	APIKey string

	// Timeout is the per-request HTTP timeout. Default: 30s.
	Timeout time.Duration
}

// Client is a thin HTTP client to the dashboard's /api/v1/* endpoints.
//
// It implements ONLY the surface MCP needs (search). It is NOT a full SDK.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New constructs a client.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("ragclient: BaseURL is required")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("ragclient: APIKey is required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:  cfg.APIKey,
		http:    &http.Client{Timeout: timeout},
	}, nil
}

// searchRequest mirrors dashboard.searchReq.
type searchRequest struct {
	Project string `json:"project"`
	Query   string `json:"query"`
	Limit   int    `json:"limit"`
}

// searchResponse mirrors what dashboard.search() returns.
type searchResponse struct {
	Query   string       `json:"query"`
	Project string       `json:"project"`
	Results []rag.Result `json:"results"`
	Error   string       `json:"error,omitempty"`
}

// Search performs POST /api/v1/search on the dashboard and returns results.
//
// On 503 (backend unavailable) it returns a wrapped error so the caller can
// distinguish "infra problem" from "no matches".
func (c *Client) Search(ctx context.Context, project, query string, limit int) ([]rag.Result, error) {
	if project == "" {
		return nil, fmt.Errorf("ragclient: project is required")
	}
	if query == "" {
		return nil, fmt.Errorf("ragclient: query is required")
	}
	if limit <= 0 {
		limit = 5
	}
	body, err := json.Marshal(searchRequest{Project: project, Query: query, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("ragclient: marshal request: %w", err)
	}
	url := c.baseURL + "/api/v1/search"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ragclient: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("User-Agent", "bit-rag-mcp/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ragclient: do request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ragclient: read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		// Try to decode structured error first.
		var sr searchResponse
		if jerr := json.Unmarshal(raw, &sr); jerr == nil && sr.Error != "" {
			return nil, fmt.Errorf("ragclient: dashboard %d: %s", resp.StatusCode, sr.Error)
		}
		return nil, fmt.Errorf("ragclient: dashboard %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var sr searchResponse
	if err := json.Unmarshal(raw, &sr); err != nil {
		return nil, fmt.Errorf("ragclient: decode response: %w", err)
	}
	return sr.Results, nil
}

// Healthz pings the dashboard public health endpoint. Used by MCP boot
// to fail fast if the configured DASHBOARD_URL is wrong.
func (c *Client) Healthz(ctx context.Context) error {
	url := c.baseURL + "/healthz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("ragclient: new healthz request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ragclient: healthz: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ragclient: healthz status %d", resp.StatusCode)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// --- Project management + indexing (ADR-0007 Phase 9) ---

// Project represents a registered project in the dashboard.
type Project struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	RootPath    string `json:"root_path"`
	Description string `json:"description"`
	Domains     string `json:"domains"`
}

// ListProjects returns all registered projects.
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	url := c.baseURL + "/api/v1/projects"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ragclient: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ragclient: do request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ragclient: dashboard %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out struct {
		Projects []Project `json:"projects"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ragclient: decode: %w", err)
	}
	return out.Projects, nil
}

// IndexProject triggers a background indexing job for the given project.
// Returns immediately with job info (status will be queued/running).
func (c *Client) IndexProject(ctx context.Context, project string) (string, error) {
	body, _ := json.Marshal(map[string]string{"project": project})
	url := c.baseURL + "/api/v1/index"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("ragclient: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ragclient: do request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("ragclient: dashboard %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("ragclient: decode: %w", err)
	}
	return out.ID, nil
}

// IndexStatus holds a snapshot of an indexing job's progress.
type IndexStatus struct {
	Status      string   `json:"status"`
	FilesDone   int      `json:"files_done"`
	FilesTotal  int      `json:"files_total"`
	ChunksDone  int      `json:"chunks_done"`
	IndexedDone int      `json:"indexed_done"`
	Errors      []string `json:"errors"`
}

// GetIndexStatus polls the status of the most recent indexing job for a project.
func (c *Client) GetIndexStatus(ctx context.Context, project string) (*IndexStatus, error) {
	url := c.baseURL + "/api/v1/index/status?project=" + project
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("ragclient: new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ragclient: do request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ragclient: dashboard %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out IndexStatus
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ragclient: decode: %w", err)
	}
	return &out, nil
}

// CreateProject registers a new project in the dashboard. Idempotent: if the
// project name already exists, returns the existing project (dashboard returns
// 409, which we catch and treat as success).
func (c *Client) CreateProject(ctx context.Context, name, rootPath, description string) (*Project, error) {
	body, _ := json.Marshal(map[string]string{
		"name":        name,
		"root_path":   rootPath,
		"description": description,
	})
	url := c.baseURL + "/api/v1/projects"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ragclient: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ragclient: do request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	// 409 = project already exists. Fetch it via ListProjects and return.
	if resp.StatusCode == 409 {
		projects, err := c.ListProjects(ctx)
		if err != nil {
			return nil, nil // already exists, but can't fetch — return nil (not error)
		}
		for _, p := range projects {
			if p.Name == name {
				return &p, nil
			}
		}
		return nil, nil
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ragclient: dashboard %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var p Project
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("ragclient: decode: %w", err)
	}
	return &p, nil
}

// GetProject returns a single project by name. Returns nil if not found.
func (c *Client) GetProject(ctx context.Context, name string) (*Project, error) {
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if p.Name == name {
			return &p, nil
		}
	}
	return nil, nil
}

// GetProjectByID returns a single project by numeric ID. Returns nil if not found.
func (c *Client) GetProjectByID(ctx context.Context, id int64) (*Project, error) {
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	for i := range projects {
		if projects[i].ID == id {
			return &projects[i], nil
		}
	}
	return nil, nil
}

// GetProjectByPath returns a project whose root_path matches (case-insensitive).
// Used for idempotent create: agent calls rag_create_project with root_path,
// we check if already registered before creating.
func (c *Client) GetProjectByPath(ctx context.Context, rootPath string) (*Project, error) {
	projects, err := c.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	normalized := normalizePath(rootPath)
	for i := range projects {
		if normalizePath(projects[i].RootPath) == normalized {
			return &projects[i], nil
		}
	}
	return nil, nil
}

// normalizePath normalizes a path for comparison: lowercases, strips trailing slash,
// converts backslashes to forward slashes.
func normalizePath(p string) string {
	p = strings.ToLower(p)
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimSuffix(p, "/")
	return p
}

// ResolveProjectIdentifier resolves either a project_id (numeric) or a
// project name (string) to the actual project name. This is the core
// helper for the ID-based MCP approach: agents pass project_id, we
// resolve it to the name for the dashboard API (which uses name internally).
//
// projectID takes precedence over projectName. Returns the resolved name,
// or empty string if neither is valid.
func (c *Client) ResolveProjectIdentifier(ctx context.Context, projectID int64, projectName string) (string, error) {
	if projectID > 0 {
		p, err := c.GetProjectByID(ctx, projectID)
		if err != nil {
			return "", fmt.Errorf("resolve project_id %d: %w", projectID, err)
		}
		if p == nil {
			return "", fmt.Errorf("project_id %d not found", projectID)
		}
		return p.Name, nil
	}
	if projectName != "" {
		p, err := c.GetProject(ctx, projectName)
		if err != nil {
			return "", fmt.Errorf("resolve project %q: %w", projectName, err)
		}
		if p == nil {
			return "", fmt.Errorf("project %q not found", projectName)
		}
		return p.Name, nil
	}
	return "", fmt.Errorf("either project_id or project must be provided")
}
