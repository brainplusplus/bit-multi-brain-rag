// Package rag — Qdrant HTTP client implementing the Provider interface.
//
// Uses Qdrant REST API (port 6333) via net/http, no SDK dependency.
// Collection naming follows CollectionKey.String():
//   {project}_{domain}_{model}_{dim}_{backend}
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

// QdrantClient implements Provider against a Qdrant REST endpoint.
type QdrantClient struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewQdrantClient constructs a Qdrant REST client.
// baseURL must NOT have a trailing slash (e.g. http://localhost:6333).
func NewQdrantClient(baseURL, apiKey string, timeoutS int) *QdrantClient {
	if strings.HasSuffix(baseURL, "/") {
		baseURL = strings.TrimSuffix(baseURL, "/")
	}
	if timeoutS <= 0 {
		timeoutS = 30
	}
	return &QdrantClient{
		baseURL: baseURL,
		apiKey:  apiKey,
		http: &http.Client{
			Timeout: time.Duration(timeoutS) * time.Second,
		},
	}
}

// Close is a no-op for the HTTP client (connection pool handled by net/http).
func (q *QdrantClient) Close() error { return nil }

// --- REST helpers ---

func (q *QdrantClient) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, q.baseURL+path, rdr)
	if err != nil {
		return nil, 0, fmt.Errorf("new request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if q.apiKey != "" {
		req.Header.Set("api-key", q.apiKey)
	}
	resp, err := q.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("qdrant request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return data, resp.StatusCode, fmt.Errorf("qdrant %s %s: HTTP %d: %s", method, path, resp.StatusCode, truncate(string(data), 300))
	}
	return data, resp.StatusCode, nil
}

// --- Provider implementation ---

// CreateCollection creates the collection with cosine distance + named vectors.
// Idempotent: if it already exists, returns nil.
func (q *QdrantClient) CreateCollection(ctx context.Context, key CollectionKey) error {
	collection := key.String()
	// Check existence first to avoid error noise.
	if exists, err := q.collectionExists(ctx, collection); err != nil {
		return err
	} else if exists {
		return nil
	}
	body := map[string]any{
		"vectors": map[string]any{
			"size":     key.Dim,
			"distance": "Cosine",
		},
		"optimizers_config": map[string]any{
			"default_partition_number": 8,
		},
	}
	_, _, err := q.do(ctx, "PUT", "/collections/"+collection, body)
	return err
}

// DeleteCollection removes the collection and all points.
func (q *QdrantClient) DeleteCollection(ctx context.Context, key CollectionKey) error {
	_, _, err := q.do(ctx, "DELETE", "/collections/"+key.String(), nil)
	return err
}

// Index upserts points (UUID/string IDs, vector + payload).
func (q *QdrantClient) Index(ctx context.Context, key CollectionKey, docs []Document, vectors [][]float32) error {
	if len(docs) != len(vectors) {
		return fmt.Errorf("Index: docs (%d) and vectors (%d) length mismatch", len(docs), len(vectors))
	}
	if len(docs) == 0 {
		return nil
	}
	points := make([]map[string]any, 0, len(docs))
	for i, doc := range docs {
		points = append(points, map[string]any{
			"id":      doc.ID,
			"vector":  vectors[i],
			"payload": doc.Meta,
		})
	}
	body := map[string]any{
		"points": points,
	}
	_, _, err := q.do(ctx, "PUT", "/collections/"+key.String()+"/points?wait=true", body)
	return err
}

// searchResult maps the Qdrant REST search response shape.
type searchResult struct {
	Result []struct {
		ID      any            `json:"id"`
		Score   float64        `json:"score"`
		Payload map[string]any `json:"payload"`
	} `json:"result"`
}

// SemanticSearch returns top-k nearest points to queryVec.
func (q *QdrantClient) SemanticSearch(ctx context.Context, key CollectionKey, queryVec []float32, limit int) ([]Result, error) {
	if limit <= 0 {
		limit = 5
	}
	body := map[string]any{
		"vector":  queryVec,
		"limit":   limit,
		"with_payload": true,
	}
	data, _, err := q.do(ctx, "POST", "/collections/"+key.String()+"/points/search", body)
	if err != nil {
		return nil, err
	}
	var sr searchResult
	if err := json.Unmarshal(data, &sr); err != nil {
		return nil, fmt.Errorf("unmarshal search result: %w", err)
	}
	out := make([]Result, 0, len(sr.Result))
	for _, r := range sr.Result {
		meta := make(map[string]string, len(r.Payload))
		for k, v := range r.Payload {
			meta[k] = fmt.Sprintf("%v", v)
		}
		out = append(out, Result{
			ID:      fmt.Sprintf("%v", r.ID),
			Content: meta["content"],
			Score:   r.Score,
			Meta:    meta,
		})
	}
	return out, nil
}

// DeletePoints removes points by string IDs.
func (q *QdrantClient) DeletePoints(ctx context.Context, key CollectionKey, pointIDs []string) error {
	body := map[string]any{"points": pointIDs}
	_, _, err := q.do(ctx, "POST", "/collections/"+key.String()+"/points/delete?wait=true", body)
	return err
}

// scrollResult maps the Qdrant scroll (list) response.
type scrollResult struct {
	Result struct {
		Points []struct {
			ID      any            `json:"id"`
			Payload map[string]any `json:"payload"`
		} `json:"points"`
		NextPage any `json:"next_page_offset"`
	} `json:"result"`
}

// ListPoints returns all points with optional metadata filter (delta sync).
func (q *QdrantClient) ListPoints(ctx context.Context, key CollectionKey, metaFilter map[string]string) ([]PointInfo, error) {
	var out []PointInfo
	var offset any
	for {
		body := map[string]any{
			"limit":       256,
			"with_payload": true,
		}
		if offset != nil {
			body["offset"] = offset
		}
		if len(metaFilter) > 0 {
			must := []map[string]any{}
			for k, v := range metaFilter {
				must = append(must, map[string]any{
					"key": k,
					"match": map[string]any{"value": v},
				})
			}
			body["filter"] = map[string]any{"must": must}
		}
		data, _, err := q.do(ctx, "POST", "/collections/"+key.String()+"/points/scroll", body)
		if err != nil {
			return nil, err
		}
		var sr scrollResult
		if err := json.Unmarshal(data, &sr); err != nil {
			return nil, fmt.Errorf("unmarshal scroll: %w", err)
		}
		for _, p := range sr.Result.Points {
			pi := PointInfo{ID: fmt.Sprintf("%v", p.ID)}
			if sf, ok := p.Payload["source_file"]; ok {
				pi.SourceFile = fmt.Sprintf("%v", sf)
			}
			out = append(out, pi)
		}
		if sr.Result.NextPage == nil {
			break
		}
		offset = sr.Result.NextPage
	}
	return out, nil
}

// --- helpers ---

func (q *QdrantClient) collectionExists(ctx context.Context, collection string) (bool, error) {
	data, status, err := q.do(ctx, "GET", "/collections/"+collection, nil)
	if err != nil {
		if status == 404 {
			return false, nil
		}
		return false, err
	}
	var res struct {
		Result struct {
			Exists bool `json:"exists"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return false, fmt.Errorf("unmarshal collection info: %w", err)
	}
	return true, nil
}

// Ping checks Qdrant reachability.
func (q *QdrantClient) Ping(ctx context.Context) error {
	_, _, err := q.do(ctx, "GET", "/healthz", nil)
	return err
}
