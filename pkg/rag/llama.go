// Package rag provides RAG backend abstractions.
//
// llama.go: EmbeddingClient implementation backed by a self-hosted
// llama.cpp HTTP server (voyage-4-nano Q8 at voyage.bitsolution.my.id).
//
// Endpoint: POST {BASE}/v1/embeddings
// Auth:     Authorization: Bearer {LLAMA_API_KEY}
// Body:     {"model": "...", "input": ["text1", "text2", ...]}
// Resp:     {"data": [{"embedding": [...], "index": 0}, ...]}
//
// Critical: voyage-4-nano requires mean pooling. The server must be started
// with --pooling mean (see ADR-0001 §1.3 and Dockerfile). This client does
// NOT control pooling — it trusts the server config. Verify pooling via
// /health or logs before trusting embeddings.
package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LlamaConfig configures the llama.cpp HTTP embedder.
type LlamaConfig struct {
	Endpoint string // e.g. "https://voyage.bitsolution.my.id"
	APIKey   string // LLAMA_API_KEY (Bearer token)
	Model    string // e.g. "voyage-4-nano" (informational, server ignores)
	Dim      int    // expected vector dim (1024 for voyage_nano_1024)
	Timeout  time.Duration
}

// LlamaEmbedder implements EmbeddingClient over a llama.cpp HTTP server.
type LlamaEmbedder struct {
	cfg  LlamaConfig
	http *http.Client
}

// NewLlamaEmbedder creates an embedder client for a llama.cpp server.
func NewLlamaEmbedder(cfg LlamaConfig) *LlamaEmbedder {
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &LlamaEmbedder{
		cfg: cfg,
		http: &http.Client{
			Timeout: cfg.Timeout,
		},
	}
}

// embeddingsRequest is the JSON body sent to /v1/embeddings.
type embeddingsRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embeddingsResponse is the JSON body returned by /v1/embeddings.
type embeddingsResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// Embed sends texts to the llama.cpp server and returns their vectors.
// It batches all texts in a single HTTP request (the server supports array input).
// If the server returns an error, it is surfaced verbatim (no silent retry yet).
func (e *LlamaEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(embeddingsRequest{
		Model: e.cfg.Model,
		Input: texts,
	})
	if err != nil {
		return nil, fmt.Errorf("llama: marshal request: %w", err)
	}

	url := strings.TrimRight(e.cfg.Endpoint, "/") + "/v1/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llama: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}

	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llama: http call: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("llama: read body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llama: server returned %d: %s",
			resp.StatusCode, truncate(string(raw), 200))
	}

	var parsed embeddingsResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("llama: unmarshal response: %w", err)
	}

	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("llama: count mismatch: sent %d, got %d embeddings",
			len(texts), len(parsed.Data))
	}

	// Sort by index (server may return out-of-order for parallel batches).
	out := make([][]float32, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("llama: embedding index %d out of range", d.Index)
		}
		out[d.Index] = d.Embedding
	}

	// Dim validation (only on first to avoid per-vector cost).
	if e.cfg.Dim > 0 && len(out) > 0 && len(out[0]) != e.cfg.Dim {
		return nil, fmt.Errorf("llama: dim mismatch: expected %d, got %d (check --pooling mean on server)",
			e.cfg.Dim, len(out[0]))
	}

	return out, nil
}

// VectorSize returns the configured dimension.
func (e *LlamaEmbedder) VectorSize() int {
	return e.cfg.Dim
}

// Backend returns the backend identifier for collection naming.
func (e *LlamaEmbedder) Backend() string {
	// Determined by server config; default llama.cpp Q8.
	return "llama_q8"
}

// Model returns the configured model name.
func (e *LlamaEmbedder) Model() string {
	return e.cfg.Model
}

// Health checks the server /health endpoint. Returns nil if healthy.
// Useful for startup probe and pooling verification.
func (e *LlamaEmbedder) Health(ctx context.Context) error {
	url := strings.TrimRight(e.cfg.Endpoint, "/") + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("llama: build health request: %w", err)
	}

	resp, err := e.http.Do(req)
	if err != nil {
		return fmt.Errorf("llama: health http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("llama: health returned %d", resp.StatusCode)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
