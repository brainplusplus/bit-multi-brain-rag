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

// VoyageEmbedder implements EmbeddingClient for the Voyage AI API.
type VoyageEmbedder struct {
	endpoint string
	apiKey   string
	model    string // e.g. "voyage-3", "voyage-code-3"
	dim      int
	http     *http.Client
}

// VoyageConfig configures a Voyage AI embedder.
type VoyageConfig struct {
	Endpoint string // defaults to https://api.voyageai.com
	APIKey   string
	Model    string
	Dim      int
	Timeout  time.Duration
}

// NewVoyageEmbedder creates a Voyage AI embedding client.
func NewVoyageEmbedder(cfg VoyageConfig) *VoyageEmbedder {
	if cfg.Endpoint == "" {
		cfg.Endpoint = "https://api.voyageai.com"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	return &VoyageEmbedder{
		endpoint: cfg.Endpoint,
		apiKey:   cfg.APIKey,
		model:    cfg.Model,
		dim:      cfg.Dim,
		http:     &http.Client{Timeout: cfg.Timeout},
	}
}

func (e *VoyageEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	// Voyage max 128 texts per call.
	const batchSize = 128
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

func (e *VoyageEmbedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body := map[string]any{
		"model":      e.model,
		"input":      texts,
		"input_type": "document",
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("voyage: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, "POST", e.endpoint+"/v1/embeddings", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("voyage: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("voyage: http call: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("voyage: HTTP %d: %s", resp.StatusCode, truncateBytes(respBody, 300))
	}
	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("voyage: unmarshal: %w", err)
	}
	vecs := make([][]float32, len(result.Data))
	for i, d := range result.Data {
		vecs[i] = d.Embedding
	}
	return vecs, nil
}

func (e *VoyageEmbedder) VectorSize() int  { return e.dim }
func (e *VoyageEmbedder) Backend() string  { return "voyage" }
func (e *VoyageEmbedder) Model() string    { return e.model }
