// Package main is the benchmark runner CLI for bit-multi-brain-rag (ADR-0002 §4).
//
// Usage:
//
//	bit-multi-brain-rag-bench -dataset <path.json> -project <id> -root <path> [-k 5] [-reindex]
//
// Dataset JSON schema (compact, BENCHMARK.md-compatible):
//
//	[
//	  {
//	    "query": "function to parse JSON config",
//	    "relevant": ["pkg/config/config.go:Load", "pkg/config/config.go:getEnv"]
//	  }
//	]
//
// Flow:
//  1. Load dataset (query + relevant chunk IDs).
//  2. Optionally re-index project root (slow; skip if collection already built).
//  3. For each query: embed query → search Qdrant → record latency + recall@k.
//  4. Print human-readable report + write JSON report to stdout.
//
// Prereqs: Qdrant running, llama.cpp embedder reachable.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/bench"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/chunker"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/config"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/indexer"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
)

// datasetItem is one row of the dataset JSON.
type datasetItem struct {
	Query    string   `json:"query"`
	Relevant []string `json:"relevant"`
}

func main() {
	datasetPath := flag.String("dataset", "", "path to dataset JSON (required)")
	projectID := flag.String("project", "bench", "project ID for the benchmark collection")
	rootPath := flag.String("root", ".", "project root to index (only with -reindex)")
	k := flag.Int("k", 5, "k for recall@k")
	reindex := flag.Bool("reindex", false, "re-index project root before benchmarking (slow)")
	flag.Parse()

	if *datasetPath == "" {
		fmt.Fprintln(os.Stderr, "error: -dataset is required")
		flag.Usage()
		os.Exit(2)
	}

	// --- Load dataset ---
	raw, err := os.ReadFile(*datasetPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read dataset: %v\n", err)
		os.Exit(1)
	}
	var items []datasetItem
	if err := json.Unmarshal(raw, &items); err != nil {
		fmt.Fprintf(os.Stderr, "parse dataset: %v\n", err)
		os.Exit(1)
	}
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "error: dataset is empty")
		os.Exit(1)
	}
	queries := make([]bench.Query, len(items))
	for i, it := range items {
		queries[i] = bench.Query{Text: it.Query, Relevant: it.Relevant}
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// --- Load config + wire backend ---
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	emb := rag.NewLlamaEmbedder(rag.LlamaConfig{
		Endpoint: cfg.EmbeddingEndpoint,
		APIKey:   cfg.EmbeddingAPIKey,
		Model:    cfg.EmbeddingModel,
		Dim:      cfg.EmbeddingDim,
		Timeout:  time.Duration(cfg.EmbeddingTimeoutS) * time.Second,
	})
	qc := rag.NewQdrantClient(cfg.QdrantURL, cfg.QdrantAPIKey, cfg.EmbeddingTimeoutS)

	if err := qc.Ping(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "qdrant unreachable (%s): %v\n", cfg.QdrantURL, err)
		fmt.Fprintln(os.Stderr, "start Qdrant first, e.g. docker run -p 6333:6333 qdrant/qdrant")
		os.Exit(1)
	}

	// --- Optional re-index ---
	if *reindex {
		ch := chunker.New()
		idx := indexer.New(ch, emb, qc, logger)
		logger.Info("indexing project root for benchmark", "root", *rootPath, "project", *projectID)
		stats, err := idx.IndexProject(context.Background(), *projectID, *rootPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "index: %v\n", err)
			os.Exit(1)
		}
		logger.Info("index done",
			"files", stats.FilesScanned,
			"chunks", stats.Chunks,
			"embedded", stats.Embedded,
			"indexed", stats.Indexed,
			"skipped", stats.Skipped,
			"duration", stats.Duration,
			"errors", len(stats.Errors),
		)
		for _, e := range stats.Errors {
			logger.Warn("index error", "err", e)
		}
	}

	// --- Run benchmark ---
	key := rag.CollectionKey{
		Project: *projectID,
		Domain:  rag.DomainCode,
		Model:   cfg.ActiveModel,
		Dim:     cfg.EmbeddingDim,
		Backend: cfg.ActiveBackend,
	}
	runner := &bench.Runner{
		Provider: qc,
		Embedder: emb,
		Key:      key,
		TopK:     *k,
	}
	ds := bench.Dataset{Name: *datasetPath, Queries: queries}

	report, err := runner.Run(context.Background(), ds)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchmark: %v\n", err)
		os.Exit(1)
	}

	// --- Human-readable report ---
	fmt.Println("\n=== BENCHMARK REPORT ===")
	fmt.Printf("Dataset:      %s\n", report.Dataset)
	fmt.Printf("Backend:      %s\n", report.Backend)
	fmt.Printf("Model:        %s\n", report.Model)
	fmt.Printf("Dimension:    %d\n", report.Dim)
	fmt.Printf("Queries:      %d (errors: %d)\n", report.NumQueries, report.Errors)
	fmt.Printf("K (recall@k): %d\n", *k)
	fmt.Println()
	fmt.Printf("Recall@%d mean:  %.4f (%.1f%%)\n", *k, report.RecallMean, report.RecallMean*100)
	fmt.Printf("Recall@%d min:   %.4f\n", *k, report.RecallMin)
	fmt.Printf("Recall@%d max:   %.4f\n", *k, report.RecallMax)
	fmt.Printf("Latency p50:     %d ms\n", report.LatencyP50)
	fmt.Printf("Latency p95:     %d ms\n", report.LatencyP95)
	fmt.Printf("Latency p99:     %d ms\n", report.LatencyP99)
	fmt.Printf("Latency mean:    %d ms\n", report.LatencyMean)
	fmt.Println()
	fmt.Println("Per-query:")
	for i, qr := range report.PerQuery {
		status := "MISS"
		if qr.RecallAtK > 0 {
			status = "HIT "
		}
		if qr.Error != "" {
			status = "ERR "
		}
		fmt.Printf("  [%s] q%d  recall=%.2f  hits=%d  ret=%d  lat=%dms\n",
			status, i+1, qr.RecallAtK, qr.HitCount, qr.Retrieved, qr.LatencyMS)
		if qr.Error != "" {
			fmt.Printf("         err: %s\n", qr.Error)
		}
	}

	// --- JSON report to stderr (machine-readable) ---
	jsonReport, _ := json.MarshalIndent(report, "", "  ")
	fmt.Fprintln(os.Stderr, string(jsonReport))

	// Sanity: warn if recall is terrible.
	if report.Errors == 0 && report.RecallMean < 0.1 {
		fmt.Fprintln(os.Stderr, "\nWARNING: recall@k < 10% — check pooling config (--pooling mean) or re-index.")
	}
}
