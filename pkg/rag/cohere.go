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

// CohereEmbedder implements EmbeddingClient for the Cohere Embed API.
type CohereEmbedder struct {
	endpoint string
	apiKey   string
	model    string // e.g. "embed-english-v3.0"
	dim      int
	http     *http.Client
}

// CohereConfig configures a Cohere embedder.
type CohereConfig struct {
	Endpoint string // defaults to https://api.cohere.com
	APIKey   string
	Model    string
	Dim      int
	Timeout  time.Duration
}

// NewCohereEmbedder creates a Cohere embedding client.
func NewCohereEmbedder(cfg CohereConfig) *CohereEmbedder {
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://api.cohere.com"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	return &CohereEmbedder{
		endpoint: cfg.Endpoint,
		apiKey:   cfg.APIKey,
		model:    cfg.Model,
		dim:      cfg.Dim,
		http:     &http.Client{Timeout: cfg.Timeout},
	}
}

func (e *CohereEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	// Cohere max 96 texts per call; batch if needed.
	const batchSize = 96
	var allVecs [][]float32
	for i := 0; i < len(texts); i += batchSize {
		end := i + batchSize
		if end > len(texts) {
			end = len(texts)
		}
		vecs, err := e.embedBatch(ctx, texts[i:end])
		if err != nil {
			return nil, err
		}
		allVecs = append(allVecs, vecs...)
	}
	return allVecs, nil
}

func (e *CohereEmbedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body := map[string]any{
		"model":      e.model,
		"texts":      texts,
		"input_type": "search_document",
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("cohere: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", e.endpoint+"/v1/embed", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("cohere: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cohere: http call: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("cohere: HTTP %d: %s", resp.StatusCode, truncateBytes(respBody, 300))
	}
	var result struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("cohere: unmarshal: %w", err)
	}
	return result.Embeddings, nil
}

func (e *CohereEmbedder) VectorSize() int  { return e.dim }
func (e *CohereEmbedder) Backend() string  { return "cohere" }
func (e *CohereEmbedder) Model() string    { return e.model }
