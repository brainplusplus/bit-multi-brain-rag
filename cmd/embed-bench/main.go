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
	iters := flag.Int("iters", 10, "iterations per scenario (warm-up excluded)")
	flag.Parse()

	endpoint := strings.TrimRight(*url, "/") + "/v1/embeddings"

	// Scenarios: (label, text, batch).
	base := "The quick brown fox jumps over the lazy dog near the riverbank."
	scenarios := []struct {
		label string
		text  string
		batch int
	}{
		{"short_~12tok_b1", base, 1},
		{"short_~12tok_b4", base, 4},
		{"short_~12tok_b16", base, 16},
		{"med_~120tok_b1", rep(base, 10), 1},
		{"med_~120tok_b4", rep(base, 10), 4},
		{"med_~120tok_b16", rep(base, 10), 16},
		{"long_~600tok_b1", rep(base, 50), 1},
		{"long_~600tok_b4", rep(base, 50), 4},
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

	fmt.Printf("# Embedder benchmark\n\n")
	fmt.Printf("- endpoint : %s\n", endpoint)
	fmt.Printf("- model    : %s\n", *model)
	fmt.Printf("- mode     : %s\n", mode)
	fmt.Printf("- iters    : %d (warm-up excluded)\n\n", *iters)

	fmt.Println("| scenario | batch | iters | total_ms | avg_ms | p50_ms | p95_ms | tokens | tok/s |")
	fmt.Println("|----------|-------|-------|----------|--------|--------|--------|--------|-------|")

	client := &http.Client{Timeout: 5 * time.Minute}

	for _, sc := range scenarios {
		// Warm-up: 2 calls (not counted).
		for w := 0; w < 2; w++ {
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
		avg := total / time.Duration(*iters)
		tps := float64(totalTok) / total.Seconds()

		fmt.Printf("| %s | %d | %d | %d | %d | %d | %d | %d | %.0f |\n",
			sc.label, sc.batch, *iters,
			total.Milliseconds(), avg.Milliseconds(),
			p50.Milliseconds(), p95.Milliseconds(),
			totalTok, math.Round(tps))
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
