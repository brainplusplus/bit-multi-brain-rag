package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OpenAIEmbedder implements EmbeddingClient for OpenAI-compatible APIs.
// Works with: OpenAI, OpenRouter, Azure OpenAI, any /v1/embeddings endpoint.
type OpenAIEmbedder struct {
	endpoint string // base URL (e.g. "https://api.openai.com")
	apiKey   string
	model    string // e.g. "text-embedding-3-small"
	dim      int
	backend  string // "openai" or "openrouter"
	http     *http.Client
}

// OpenAIConfig configures an OpenAI-compatible embedder.
type OpenAIConfig struct {
	Endpoint string // base URL; defaults to https://api.openai.com
	APIKey   string
	Model    string
	Dim      int
	Backend  string // "openai" or "openrouter"
	Timeout  time.Duration
}

// NewOpenAIEmbedder creates an OpenAI-compatible embedding client.
func NewOpenAIEmbedder(cfg OpenAIConfig) *OpenAIEmbedder {
	if cfg.Endpoint == "" {
		if cfg.Backend == "openrouter" {
			cfg.Endpoint = "https://openrouter.ai/api"
		} else {
			cfg.Endpoint = "https://api.openai.com"
		}
	}
	if cfg.Backend == "" {
		cfg.Backend = "openai"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	return &OpenAIEmbedder{
		endpoint: cfg.Endpoint,
		apiKey:   cfg.APIKey,
		model:    cfg.Model,
		dim:      cfg.Dim,
		backend:  cfg.Backend,
		http:     &http.Client{Timeout: cfg.Timeout},
	}
}

func (e *OpenAIEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body := map[string]any{
		"model": e.model,
		"input": texts,
	}
	if e.dim > 0 {
		body["dimensions"] = e.dim
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", e.endpoint+"/v1/embeddings", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("openai: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	if e.backend == "openrouter" {
		req.Header.Set("HTTP-Referer", "https://bit-multi-brain-rag")
		req.Header.Set("X-Title", "bit-multi-brain-rag")
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai: http call: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, truncateBytes(respBody, 300))
	}
	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("openai: unmarshal: %w", err)
	}
	vecs := make([][]float32, len(result.Data))
	for i, d := range result.Data {
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

func (e *OpenAIEmbedder) VectorSize() int  { return e.dim }
func (e *OpenAIEmbedder) Backend() string  { return e.backend }
func (e *OpenAIEmbedder) Model() string    { return e.model }

func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
