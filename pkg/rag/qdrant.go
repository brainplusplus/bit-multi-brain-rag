// Package rag — Qdrant HTTP client implementing the Provider interface.
//
// Uses Qdrant REST API (port 6333) via net/http, no SDK dependency.
// Collection naming follows CollectionKey.String():
//
//	{project}_{domain}_{model}_{dim}_{backend}
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
		// Merge doc.Meta + doc.Content into a single payload. We store the
		// raw chunk content under "content" so the search path can return it
		// (otherwise UI / MCP would only show file path + score, never code).
		payload := make(map[string]any, len(doc.Meta)+1)
		for k, v := range doc.Meta {
			payload[k] = v
		}
		payload["content"] = doc.Content
		points = append(points, map[string]any{
			"id":      doc.ID,
			"vector":  vectors[i],
			"payload": payload,
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
		"vector":       queryVec,
		"limit":        limit,
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
			"limit":        256,
			"with_payload": true,
		}
		if offset != nil {
			body["offset"] = offset
		}
		if len(metaFilter) > 0 {
			must := []map[string]any{}
			for k, v := range metaFilter {
				must = append(must, map[string]any{
					"key":   k,
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

// Scroll returns one page of points (with payload, no vectors) for the
// chunks browser. The offset cursor is opaque — pass NextOffset from the
// previous call to advance. See ADR-0006.
func (q *QdrantClient) Scroll(ctx context.Context, key CollectionKey, opts ScrollOpts) (ScrollResult, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	body := map[string]any{
		"limit":        limit,
		"with_payload": true,
		"with_vector":  false,
	}
	if opts.Offset != "" {
		body["offset"] = opts.Offset
	}
	if opts.Filter != nil {
		body["filter"] = opts.Filter
	}
	data, _, err := q.do(ctx, "POST", "/collections/"+key.String()+"/points/scroll", body)
	if err != nil {
		return ScrollResult{}, err
	}
	var sr scrollResult
	if err := json.Unmarshal(data, &sr); err != nil {
		return ScrollResult{}, fmt.Errorf("unmarshal scroll: %w", err)
	}
	out := ScrollResult{Points: make([]Point, 0, len(sr.Result.Points))}
	for _, p := range sr.Result.Points {
		meta := make(map[string]string, len(p.Payload))
		var content string
		for k, v := range p.Payload {
			s := fmt.Sprintf("%v", v)
			meta[k] = s
			if k == "content" {
				content = s
			}
		}
		out.Points = append(out.Points, Point{
			ID:      fmt.Sprintf("%v", p.ID),
			Content: content,
			Meta:    meta,
		})
	}
	if sr.Result.NextPage != nil {
		out.NextOffset = fmt.Sprintf("%v", sr.Result.NextPage)
	}
	return out, nil
}

// GetPoint fetches a single point by its ID (UUID v5 string).
// Returns an error wrapping the HTTP status code if not found.
func (q *QdrantClient) GetPoint(ctx context.Context, key CollectionKey, pointID string) (Point, error) {
	data, _, err := q.do(ctx, "GET", "/collections/"+key.String()+"/points/"+pointID, nil)
	if err != nil {
		return Point{}, err
	}
	var resp struct {
		Result struct {
			ID      any            `json:"id"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return Point{}, fmt.Errorf("unmarshal point: %w", err)
	}
	meta := make(map[string]string, len(resp.Result.Payload))
	var content string
	for k, v := range resp.Result.Payload {
		s := fmt.Sprintf("%v", v)
		meta[k] = s
		if k == "content" {
			content = s
		}
	}
	return Point{
		ID:      fmt.Sprintf("%v", resp.Result.ID),
		Content: content,
		Meta:    meta,
	}, nil
}

// CollectionInfo fetches the collection's vital signs (status, point count,
// vector size) from Qdrant's /collections/{name} endpoint.
func (q *QdrantClient) CollectionInfo(ctx context.Context, key CollectionKey) (CollectionInfo, error) {
	data, _, err := q.do(ctx, "GET", "/collections/"+key.String(), nil)
	if err != nil {
		return CollectionInfo{}, err
	}
	var resp struct {
		Result struct {
			Status      string `json:"status"`
			PointsCount int    `json:"points_count"`
			Config      struct {
				Params struct {
					Vectors struct {
						Size int `json:"size"`
					} `json:"vectors"`
				} `json:"params"`
			} `json:"config"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return CollectionInfo{}, fmt.Errorf("unmarshal collection info: %w", err)
	}
	return CollectionInfo{
		Status:      resp.Result.Status,
		PointsCount: resp.Result.PointsCount,
		VectorsSize: resp.Result.Config.Params.Vectors.Size,
	}, nil
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

// --- Hybrid search support (ADR-0008) ---

// CreateCollectionWithSparse creates a collection with both dense (named "dense")
// and sparse (named "text") vector configs for hybrid search.
// Idempotent: returns nil if collection already exists.
func (q *QdrantClient) CreateCollectionWithSparse(ctx context.Context, key CollectionKey) error {
	collection := key.String()
	if exists, err := q.collectionExists(ctx, collection); err != nil {
		return err
	} else if exists {
		return nil
	}
	body := map[string]any{
		"vectors": map[string]any{
			"dense": map[string]any{
				"size":     key.Dim,
				"distance": "Cosine",
			},
		},
		"sparse_vectors": map[string]any{
			"text": map[string]any{},
		},
		"optimizers_config": map[string]any{
			"default_partition_number": 8,
		},
	}
	_, _, err := q.do(ctx, "PUT", "/collections/"+collection, body)
	return err
}

// IndexWithSparse upserts points with both dense (named "dense") and sparse
// (named "text") vectors. Used for hybrid search collections.
func (q *QdrantClient) IndexWithSparse(ctx context.Context, key CollectionKey, docs []Document, denseVecs [][]float32, sparseVecs []*SparseVector) error {
	if len(docs) != len(denseVecs) || len(docs) != len(sparseVecs) {
		return fmt.Errorf("IndexWithSparse: length mismatch docs=%d dense=%d sparse=%d", len(docs), len(denseVecs), len(sparseVecs))
	}
	if len(docs) == 0 {
		return nil
	}
	points := make([]map[string]any, 0, len(docs))
	for i, doc := range docs {
		payload := make(map[string]any, len(doc.Meta)+1)
		for k, v := range doc.Meta {
			payload[k] = v
		}
		payload["content"] = doc.Content

		vector := map[string]any{
			"dense": denseVecs[i],
		}
		if sparseVecs[i] != nil {
			vector["text"] = sparseVecs[i]
		}
		points = append(points, map[string]any{
			"id":      doc.ID,
			"vector":  vector,
			"payload": payload,
		})
	}
	body := map[string]any{"points": points}
	_, _, err := q.do(ctx, "PUT", "/collections/"+key.String()+"/points?wait=true", body)
	return err
}

// HybridSearch performs dense + sparse retrieval with RRF fusion via
// Qdrant's Query API (available since v1.10).
// Falls back to dense-only SemanticSearch if sparseVec is nil.
func (q *QdrantClient) HybridSearch(ctx context.Context, key CollectionKey, denseVec []float32, sparseVec *SparseVector, limit int) ([]Result, error) {
	if sparseVec == nil {
		return q.SemanticSearch(ctx, key, denseVec, limit)
	}
	if limit <= 0 {
		limit = 5
	}
	prefetchLimit := limit * 3
	if prefetchLimit < 20 {
		prefetchLimit = 20
	}
	body := map[string]any{
		"prefetch": []map[string]any{
			{
				"query": denseVec,
				"using": "dense",
				"limit": prefetchLimit,
			},
			{
				"query": map[string]any{
					"indices": sparseVec.Indices,
					"values":  sparseVec.Values,
				},
				"using": "text",
				"limit": prefetchLimit,
			},
		},
		"query":        map[string]any{"rrf": map[string]any{}},
		"limit":        limit,
		"with_payload": true,
	}
	data, _, err := q.do(ctx, "POST", "/collections/"+key.String()+"/points/query", body)
	if err != nil {
		// Fallback to dense-only if Query API not supported (old Qdrant).
		return q.SemanticSearch(ctx, key, denseVec, limit)
	}
	// Query API response shape: {"result": {"points": [...]}}
	var qr struct {
		Result struct {
			Points []struct {
				ID      any            `json:"id"`
				Score   float64        `json:"score"`
				Payload map[string]any `json:"payload"`
			} `json:"points"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &qr); err != nil {
		return nil, fmt.Errorf("unmarshal hybrid query result: %w", err)
	}
	out := make([]Result, 0, len(qr.Result.Points))
	for _, p := range qr.Result.Points {
		meta := make(map[string]string, len(p.Payload))
		for k, v := range p.Payload {
			meta[k] = fmt.Sprintf("%v", v)
		}
		out = append(out, Result{
			ID:      fmt.Sprintf("%v", p.ID),
			Content: meta["content"],
			Score:   p.Score,
			Meta:    meta,
		})
	}
	return out, nil
}

// CollectionHasSparse checks if a collection was created with sparse_vectors
// config (i.e., supports hybrid search). Used for fallback detection.
func (q *QdrantClient) CollectionHasSparse(ctx context.Context, key CollectionKey) bool {
	data, _, err := q.do(ctx, "GET", "/collections/"+key.String(), nil)
	if err != nil {
		return false
	}
	var info struct {
		Result struct {
			Params struct {
				SparseVectors map[string]any `json:"sparse_vectors"`
			} `json:"params"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return false
	}
	_, has := info.Result.Params.SparseVectors["text"]
	return has
}
