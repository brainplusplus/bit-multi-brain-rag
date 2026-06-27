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
