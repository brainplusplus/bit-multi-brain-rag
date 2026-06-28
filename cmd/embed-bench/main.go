// Package main is a focused embedder latency benchmark.
//
// It measures raw embedding throughput of the llama.cpp embedder at varying
// input lengths and batch sizes. Run once against the CPU embedder, then
// again against the GPU embedder, and compare the two reports.
//
// Usage:
//
//	embed-bench -url http://localhost:8080 -model voyage-4-nano
//
// The embedder must expose POST /v1/embeddings (OpenAI-compatible).
// Each input length x batch size is warmed up, then measured for N iterations.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

type embeddingsReq struct {
	Input []string `json:"input"`
	Model string   `json:"model"`
}

type embeddingsResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

// rep builds a string of n repetitions of base joined by spaces.
func rep(base string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = base
	}
	return strings.Join(parts, " ")
}

func main() {
	url := flag.String("url", "http://localhost:8080", "embedder base URL")
	model := flag.String("model", "voyage-4-nano", "model name")
	iters := flag.Int("iters", 30, "iterations per scenario (warm-up excluded)")
	jsonOut := flag.String("json", "", "write full results as JSON to this path")
	flag.Parse()

	endpoint := strings.TrimRight(*url, "/") + "/v1/embeddings"

	// Scenarios: (label, text, batch).
	base := "The quick brown fox jumps over the lazy dog near the riverbank."
	scenarios := []struct {
		label string
		text  string
		batch int
	}{
		{"short_b1", base, 1},
		{"short_b4", base, 4},
		{"short_b8", base, 8},
		{"short_b16", base, 16},
		{"short_b32", base, 32},
		{"med_b1", rep(base, 10), 1},
		{"med_b4", rep(base, 10), 4},
		{"med_b8", rep(base, 10), 8},
		{"med_b16", rep(base, 10), 16},
		{"med_b32", rep(base, 10), 32},
	}

	// Detect mode: env or file /app/MODE inside container (best-effort).
	mode := os.Getenv("MODE")
	if mode == "" {
		if b, err := os.ReadFile("/app/MODE"); err == nil {
			mode = strings.TrimSpace(string(b))
		}
	}
	if mode == "" {
		mode = "unknown"
	}

	fmt.Fprintf(os.Stderr, "# Embedder benchmark\n\n")
	fmt.Fprintf(os.Stderr, "- endpoint : %s\n", endpoint)
	fmt.Fprintf(os.Stderr, "- model    : %s\n", *model)
	fmt.Fprintf(os.Stderr, "- mode     : %s\n", mode)
	fmt.Fprintf(os.Stderr, "- iters    : %d (warm-up excluded)\n\n", *iters)

	fmt.Println("| scenario | batch | cold_ms | warm_avg_ms | p50_ms | p95_ms | p99_ms | std_ms | tokens | tok/s |")
	fmt.Println("|----------|-------|---------|-------------|--------|--------|--------|--------|--------|-------|")

	type resultRow struct {
		Scenario string `json:"scenario"`
		Batch    int    `json:"batch"`
		ColdMs   int64  `json:"cold_ms"`
		WarmAvgMs float64 `json:"warm_avg_ms"`
		P50Ms    int64  `json:"p50_ms"`
		P95Ms    int64  `json:"p95_ms"`
		P99Ms    int64  `json:"p99_ms"`
		StdMs    float64 `json:"std_ms"`
		Tokens   int    `json:"tokens"`
		TokS     float64 `json:"tok_s"`
		LatenciesMs []float64 `json:"latencies_ms"`
	}
	type reportJSON struct {
		Mode     string `json:"mode"`
		Model    string `json:"model"`
		Endpoint string `json:"endpoint"`
		Iters    int    `json:"iters"`
		Results  []resultRow `json:"results"`
	}
	report := reportJSON{Mode: mode, Model: *model, Endpoint: endpoint, Iters: *iters}

	client := &http.Client{Timeout: 5 * time.Minute}

	for _, sc := range scenarios {
		// Cold start: first call (model may not be loaded / cache cold).
		coldStart := time.Now()
		_, cerr := call(client, endpoint, *model, sc.text, sc.batch)
		coldDur := time.Since(coldStart)
		if cerr != nil {
			// Mark as error scenario — record cold and skip warm iters.
			fmt.Fprintf(os.Stderr, "cold-start error in %s: %v\n", sc.label, cerr)
			row := resultRow{
				Scenario: sc.label, Batch: sc.batch,
				ColdMs: coldDur.Milliseconds(),
				WarmAvgMs: -1, P50Ms: -1, P95Ms: -1, P99Ms: -1, StdMs: -1, Tokens: 0, TokS: 0,
				LatenciesMs: []float64{},
			}
			report.Results = append(report.Results, row)
			fmt.Printf("| %s | %d | %d | ERROR | - | - | - | - | - | - |\n", sc.label, sc.batch, coldDur.Milliseconds())
			continue
		}

		// Warm-up: 3 calls (not counted).
		for w := 0; w < 3; w++ {
			call(client, endpoint, *model, sc.text, sc.batch)
		}

		latencies := make([]time.Duration, 0, *iters)
		totalTok := 0

		start := time.Now()
		for i := 0; i < *iters; i++ {
			tok, d, err := timedCall(client, endpoint, *model, sc.text, sc.batch)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error in %s iter %d: %v\n", sc.label, i, err)
				os.Exit(1)
			}
			latencies = append(latencies, d)
			totalTok += tok
		}
		total := time.Since(start)

		// Compute percentiles.
		p50 := percentile(latencies, 0.50)
		p95 := percentile(latencies, 0.95)
		p99 := percentile(latencies, 0.99)
		avg := total / time.Duration(*iters)
		std := stddev(latencies, avg)
		tps := float64(totalTok) / total.Seconds()

		latsMs := make([]float64, len(latencies))
		for i, l := range latencies {
			latsMs[i] = float64(l.Microseconds()) / 1000.0
		}

		row := resultRow{
			Scenario: sc.label, Batch: sc.batch,
			ColdMs: coldDur.Milliseconds(),
			WarmAvgMs: float64(avg.Microseconds()) / 1000.0,
			P50Ms: p50.Milliseconds(), P95Ms: p95.Milliseconds(), P99Ms: p99.Milliseconds(),
			StdMs: float64(std.Microseconds()) / 1000.0,
			Tokens: totalTok, TokS: math.Round(tps),
			LatenciesMs: latsMs,
		}
		report.Results = append(report.Results, row)

		fmt.Printf("| %s | %d | %d | %.1f | %d | %d | %d | %.2f | %d | %.0f |\n",
			sc.label, sc.batch, coldDur.Milliseconds(),
			float64(avg.Microseconds())/1000.0,
			p50.Milliseconds(), p95.Milliseconds(), p99.Milliseconds(),
			float64(std.Microseconds())/1000.0,
			totalTok, math.Round(tps))
	}

	if *jsonOut != "" {
		b, _ := json.MarshalIndent(report, "", "  ")
		os.WriteFile(*jsonOut, b, 0644)
		fmt.Fprintf(os.Stderr, "wrote JSON report to %s\n", *jsonOut)
	}
}

func call(c *http.Client, endpoint, model, text string, batch int) ([]float32, error) {
	body := embeddingsReq{Input: make([]string, batch), Model: model}
	for i := range body.Input {
		body.Input[i] = text
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", endpoint, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, string(raw))
	}
	var r embeddingsResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if len(r.Data) == 0 {
		return nil, fmt.Errorf("empty data")
	}
	return r.Data[0].Embedding, nil
}

func timedCall(c *http.Client, endpoint, model, text string, batch int) (int, time.Duration, error) {
	body := embeddingsReq{Input: make([]string, batch), Model: model}
	for i := range body.Input {
		body.Input[i] = text
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", endpoint, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	d := time.Since(start)
	if resp.StatusCode != 200 {
		return 0, d, fmt.Errorf("http %d: %s", resp.StatusCode, string(raw))
	}
	var r embeddingsResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, d, fmt.Errorf("unmarshal: %w", err)
	}
	return r.Usage.PromptTokens * batch, d, nil
}

// percentile returns the q-th percentile from a list of durations (input may
// be unsorted; we sort a copy). q is in [0,1].
func percentile(ds []time.Duration, q float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := make([]time.Duration, len(ds))
	copy(s, ds)
	// Simple insertion sort — fine for small n (≤ 100).
	for i := 1; i < len(s); i++ {
		key := s[i]
		j := i - 1
		for j >= 0 && s[j] > key {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = key
	}
	idx := int(math.Floor(q * float64(len(s)-1)))
	return s[idx]
}

// stddev returns the population standard deviation of ds around mean.
func stddev(ds []time.Duration, mean time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	var sumSq float64
	for _, d := range ds {
		diff := float64(d - mean)
		sumSq += diff * diff
	}
	return time.Duration(math.Sqrt(sumSq / float64(len(ds))))
}
