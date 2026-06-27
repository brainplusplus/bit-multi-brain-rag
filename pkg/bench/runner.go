// Package bench implements the benchmark runner (ADR-0002 §4, ADR-0005 future).
//
// Purpose: measure recall@k and latency (p50/p95/p99) of the RAG search
// pipeline across backends (llama_q8, st_fp16, ...) and models.
//
// Flow:
//  1. Load a dataset: list of {query, relevant_doc_ids[]} pairs.
//  2. For each query:
//     a. Embed the query via the active EmbeddingClient.
//     b. SemanticSearch against the active Provider (Qdrant collection).
//     c. Record latency + retrieved IDs.
//  3. Compute recall@k = |retrieved ∩ relevant| / |relevant|.
//  4. Aggregate latencies into percentiles.
//
// Output: JSON report + human-readable table.
//
// NOTE (phase 5): the runner is wired to the same rag.Provider + rag.EmbeddingClient
// used by the dashboard. It does NOT run its own embedder/server — those must be
// reachable (Qdrant up, llama.cpp up) before invoking Run().
package bench

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
)

// Query is a single benchmark query with its ground-truth relevant doc IDs.
type Query struct {
	Text     string   // the natural-language query
	Relevant []string // ground-truth relevant chunk/doc IDs
}

// Dataset is a named collection of queries.
type Dataset struct {
	Name    string
	Queries []Query
}

// QueryResult is the outcome of one query.
type QueryResult struct {
	Query       string
	RecallAtK   float64 // |retrieved ∩ relevant| / |relevant|, in [0,1]
	LatencyMS   int64   // embed + search wall-clock, milliseconds
	Retrieved   int     // how many IDs came back
	HitCount    int     // how many retrieved IDs are in Relevant
	Error       string  // empty if success
}

// Report aggregates results across all queries in a dataset.
type Report struct {
	Dataset     string  `json:"dataset"`
	Backend     string  `json:"backend"`
	Model       string  `json:"model"`
	Dim         int     `json:"dim"`
	NumQueries  int     `json:"num_queries"`
	RecallMean  float64 `json:"recall_mean"`
	RecallMin   float64 `json:"recall_min"`
	RecallMax   float64 `json:"recall_max"`
	LatencyP50  int64   `json:"latency_p50_ms"`
	LatencyP95  int64   `json:"latency_p95_ms"`
	LatencyP99  int64   `json:"latency_p99_ms"`
	LatencyMean int64   `json:"latency_mean_ms"`
	Errors      int     `json:"errors"`
	PerQuery    []QueryResult `json:"per_query,omitempty"`
}

// Runner executes a benchmark against one backend.
type Runner struct {
	Provider rag.Provider
	Embedder rag.EmbeddingClient
	Key      rag.CollectionKey
	TopK     int // how many results to retrieve per query (default 10)
}

// Run executes all queries in the dataset and returns an aggregate report.
// It is sequential (no concurrency) for deterministic latency measurement;
// the server is expected to handle batching on its own.
func (r *Runner) Run(ctx context.Context, ds Dataset) (*Report, error) {
	if r.Provider == nil || r.Embedder == nil {
		return nil, fmt.Errorf("bench: provider and embedder must be set")
	}
	k := r.TopK
	if k <= 0 {
		k = 10
	}

	results := make([]QueryResult, 0, len(ds.Queries))
	latencies := make([]int64, 0, len(ds.Queries))
	recalls := make([]float64, 0, len(ds.Queries))
	errCount := 0

	for _, q := range ds.Queries {
		qr := QueryResult{Query: q.Text}

		start := time.Now()

		// 1. Embed the query.
		vecs, err := r.Embedder.Embed(ctx, []string{q.Text})
		if err != nil {
			qr.Error = "embed: " + err.Error()
			errCount++
			results = append(results, qr)
			continue
		}
		if len(vecs) == 0 {
			qr.Error = "embed: empty result"
			errCount++
			results = append(results, qr)
			continue
		}

		// 2. Search.
		hits, err := r.Provider.SemanticSearch(ctx, r.Key, vecs[0], k)
		if err != nil {
			qr.Error = "search: " + err.Error()
			errCount++
			results = append(results, qr)
			continue
		}

		qr.LatencyMS = time.Since(start).Milliseconds()
		qr.Retrieved = len(hits)

		// 3. Recall@k.
		relevantSet := make(map[string]struct{}, len(q.Relevant))
		for _, id := range q.Relevant {
			relevantSet[id] = struct{}{}
		}
		hitsCount := 0
		for _, h := range hits {
			if _, ok := relevantSet[h.ID]; ok {
				hitsCount++
			}
		}
		qr.HitCount = hitsCount
		if len(q.Relevant) > 0 {
			qr.RecallAtK = float64(hitsCount) / float64(len(q.Relevant))
		}

		results = append(results, qr)
		latencies = append(latencies, qr.LatencyMS)
		recalls = append(recalls, qr.RecallAtK)
	}

	// Aggregate.
	rep := &Report{
		Dataset:    ds.Name,
		Backend:    r.Embedder.Backend(),
		Model:      r.Embedder.Model(),
		Dim:        r.Embedder.VectorSize(),
		NumQueries: len(ds.Queries),
		Errors:     errCount,
		PerQuery:   results,
	}

	if len(recalls) > 0 {
		rep.RecallMean = meanFloat(recalls)
		rep.RecallMin = minFloat(recalls)
		rep.RecallMax = maxFloat(recalls)
	}
	if len(latencies) > 0 {
		sorted := append([]int64(nil), latencies...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		rep.LatencyP50 = percentile(sorted, 50)
		rep.LatencyP95 = percentile(sorted, 95)
		rep.LatencyP99 = percentile(sorted, 99)
		rep.LatencyMean = meanInt(latencies)
	}

	return rep, nil
}

// percentile returns the p-th percentile from a SORTED slice of int64.
// p in [0,100]. Uses nearest-rank method.
func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	// nearest-rank: idx = ceil(p/100 * N) - 1, clamped.
	idx := (p*len(sorted) + 99) / 100
	if idx > len(sorted)-1 {
		idx = len(sorted) - 1
	}
	if idx < 0 {
		idx = 0
	}
	return sorted[idx]
}

func meanFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func minFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := xs[0]
	for _, x := range xs[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

func maxFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := xs[0]
	for _, x := range xs[1:] {
		if x > m {
			m = x
		}
	}
	return m
}

func meanInt(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	var s int64
	for _, x := range xs {
		s += x
	}
	return s / int64(len(xs))
}
