# ADR-0001: Embedding Model Selection and Index Isolation Strategy

- **Status**: Accepted
- **Date**: 2026-06-27
- **Project**: bit-multi-brain-rag
- **Decision owner**: Engineering
- **Reviewers**: _(pending)_

---

## 1. Context

bit-multi-brain-rag is a multi-project code retrieval (RAG) system built on top of
`cocoindex-code`. The system must:

1. Index source code from multiple projects (project = corpus boundary).
2. Support multiple embedding models and dimensions, switchable per project.
3. Provide a dashboard UI (project sidebar + embedding result viewer + query explorer).
4. Run on CPU-only infrastructure (Easypanel VPS, no GPU available now).
5. Be self-hosted (no external API dependency such as OpenAI/Voyage cloud).

### 1.1 Benchmark evidence

A comprehensive benchmark of 11 embedding models was conducted on an 8-file Python
synthetic corpus with 8 natural-language queries. Each model was isolated via a
3-path cocoindex-code layout (`project_root` + `COCOINDEX_CODE_DIR` +
`COCOINDEX_CODE_RUNTIME_DIR`). Full methodology and raw data live in
`poc-cocoindex/` (BENCHMARK.md, search10_results.txt, perf5_results.json).

Key results (CPU-only, sentence-transformers FP16 in-proc):

| Model                 | Params | Dim  | Correct | avg_gap | cpu/query | Disk    |
| --------------------- | -----: | ---: | ------: | ------: | --------: | ------: |
| arctic                |   23M  |  384 |     8/8 |   0.134 |     8.45s |   87 MB |
| granite               |   97M  |  384 |     8/8 |   0.091 |    11.66s |  210 MB |
| arctic_m              |  109M  |  768 |     8/8 |   0.131 |  **6.57s**| 1634 MB |
| arctic_m_long         |  137M  |  768 |     8/8 |   0.117 |     7.96s | 2011 MB |
| gte                   |  149M  |  768 |   **7/8**|  0.212 |     8.69s |  288 MB |
| nightowl              |  151M  |  768 |   **7/8**|  0.135 |    10.91s |  579 MB |
| arctic_m_v2           |  305M  |  768 |     8/8 |   0.257 |     7.49s | 5858 MB |
| voyage_nano (2048)    |  340M  | 2048 |     8/8 |**0.282**|    13.57s |  672 MB |
| **voyage_nano_1024**  |  340M  | 1024 |     8/8 |   0.279 |  **5.34s**|  704 MB |
| nomic_v2              |  475M  |  768 |     8/8 |   0.195 |    16.24s | 1834 MB |
| qwen3                 |  596M  | 1024 |     8/8 |   0.233 |    13.54s | 5150 MB |

- `avg_gap` = mean(top1 − top2) cosine score. Higher = more decisive ranking.
- `cpu/query` = CPU-seconds consumed by the cocoindex-code daemon per search query.
- 9 of 11 models achieved 8/8 correctness; gte and nightowl failed Q1
  ("billing invoice customer" → models.py instead of billing.py).

### 1.2 Matryoshka truncation: 2048 → 1024

voyage-4-nano ships natively at 2048-dim. Truncating to 1024 via a custom
sentence-transformers module (`voyage_slice.Slice`):

| Metric        | 2048   | 1024   | Delta      |
| ------------- | -----: | -----: | ---------- |
| Correct       | 8/8    | 8/8    | same       |
| avg_gap       | 0.282  | 0.279  | −1.1%      |
| cpu/query     | 13.57s | 5.34s  | **−60.6%** |
| Disk          | 672 MB | 704 MB | +4.9%      |

Truncation cuts CPU cost 61% with negligible quality loss.

### 1.3 Deployment backend findings

Two backends exist for serving voyage-4-nano:

| Backend                          | How                                 | Recall verified | Latency          |
| -------------------------------- | ----------------------------------- | --------------- | ---------------- |
| sentence-transformers (FP16)     | Python in-process in cocoindex-code | ✅ 8/8 (gap 0.282) | 3–13 s/query   |
| llama.cpp Q8 GGUF (HTTP server)  | Docker, `voyage.bitsolution.my.id`  | ⚠️ **UNVERIFIED**  | 50–200 ms/query |

**Critical:** Q8 recall has NOT been verified against the FP16 benchmark.
Quantization cosine similarity vs FP16 is reported as 0.9999 in third-party tests
(_loss negligible), but no cocoindex-code benchmark has been run on the Q8 server.

**Critical:** voyage-4-nano requires **mean pooling** (`pooling_mode_mean_tokens: true`
in official `1_Pooling/config.json`). The llama.cpp server defaults to CLS pooling;
`--pooling mean` must be set explicitly or embeddings are wrong.

### 1.4 Constraints

- **CPU only.** No GPU infrastructure exists (VPS Easypanel). GPU toggle is out of
  scope for this ADR (deferred to a future ADR when GPU infra is procured).
- **Self-hosted.** No external paid embedding API.
- **Multi-project.** Index isolation must extend to project boundaries, not only
  model+dim.
- **Switchable model.** Users must be able to select model+dim per project without
  corrupting other projects' indexes.

---

## 2. Decision

### 2.1 Default embedding backend and model

- **Default model**: `voyage-4-nano` truncated to **1024 dimensions**
  (`voyage_nano_1024`).
- **Default backend for production / dashboard**: **llama.cpp Q8 HTTP server**
  (`https://voyage.bitsolution.my.id/v1/embeddings`), configured with
  `--pooling mean`.
- **Default backend for local benchmark / dev**: sentence-transformers FP16
  in-proc (recall verified 8/8).

> Rationale: voyage_nano_1024 wins the benchmark on the quality-vs-cost trade-off
> (gap 0.279 — 2nd highest of 11 models; cpu/query 5.34s — lowest). The HTTP Q8
> server is required for the multi-project dashboard (latency, multi-tenancy,
> horizontal scaling). The FP16 in-proc backend is retained as the verified
> recall reference and dev fallback.

### 2.2 Index isolation key

Index directories are keyed by a **4-tuple**, not 2-tuple:

```
index/{project_id}/{model}_{dim}_{backend}_{quant}/
```

Concrete examples:

```
index/
└── proj-A/
    ├── voyage_nano_1024_llama_q8/     ← production default
    ├── voyage_nano_1024_st_fp16/      ← dev/benchmark reference
    └── arctic_m_v2_768_st_fp16/       ← optional alt model
```

**Why `backend` and `quant` are in the key (not just model+dim):**
- voyage_nano_1024 produced by sentence-transformers (custom `Slice` module, FP16)
  is **not byte-identical** to voyage_nano_1024 produced by llama.cpp Q8 with
  `--pooling mean`. Different tokenization, pooling pipeline, and quantization
  produce different vectors even at the same dimensionality. Mixing them would
  corrupt retrieval.
- A backend or quantization switch **requires a full re-index**. There is no
  incremental migration path.

### 2.3 Index manifest contract

Every index directory MUST contain a `manifest.json`:

```json
{
  "project_id": "proj-A",
  "model": "voyage-4-nano",
  "dim": 1024,
  "backend": "llama_q8",
  "quantization": "q8_0",
  "pooling": "mean",
  "doc_count": 142,
  "corpus_checksum": "sha256:...",
  "last_indexed_at": "2026-06-27T10:30:00Z",
  "status": "ready",
  "recall_verified": false
}
```

Status enum: `building | ready | stale | failed`.

- On model/backend switch: create a new index dir with `status: "building"`.
  Queries during build fall back to the currently-`ready` index with a UI warning
  ("results from previous model") rather than returning empty.
- On corpus change (file add/edit/delete): set `status: "stale"` and trigger a
  background re-index. `corpus_checksum` detects drift.
- `recall_verified: false` for the Q8 backend until a cocoindex-code benchmark
  confirms 8/8 parity with the FP16 baseline.

### 2.4 Backend registry

Backends are declared in a registry, not hard-coded. The dashboard exposes only
backends that are **available** (endpoint reachable / model downloaded).

```toml
[embedding.backends.voyage_llama_q8]
model = "voyage-4-nano"
dim = 1024
backend = "llama.cpp"
endpoint = "https://voyage.bitsolution.my.id/v1/embeddings"
quantization = "q8_0"
pooling = "mean"
device = "cpu"

[embedding.backends.voyage_st_fp16]
model = "voyageai/voyage-4-nano"
dim = 1024
backend = "sentence-transformers"
custom_module = "voyage_slice.Slice"
trust_remote_code = true
device = "cpu"

# [embedding.backends.voyage_llama_gpu]   # Phase 2, deferred
# endpoint = "https://gpu-host:8080/v1/embeddings"
# device = "gpu"
```

### 2.5 Model lifecycle (unlimited, lazy, queued)

- **Unlimited** models are allowed in the registry, but **lazy-built** on first
  use per project. No upfront indexing of every model.
- **Queue depth = 1 per host**. Concurrent model builds thrash a CPU VPS; builds
  are serialized.
- **Eviction**: an index untouched for >30 days is auto-deleted; rebuilt on next
  access.
- Dashboard shows per-index status badges (`building | ready | stale | failed`)
  so the user is never surprised by empty/stale results.

### 2.6 Dashboard scope (MVP)

The dashboard is a **multi-project RAG query explorer**, not a generic search UI.

- **Layout**: project sidebar (left) + query + results (right).
- **Per project**: select active model+dim+backend from the registry dropdown
  (only available backends shown). Switching triggers a background re-index with
  fallback semantics (§2.3).
- **Query explorer**: input box → top-K results table (file, score, snippet).
- **Benchmark runner**: a "Run benchmark" button per backend that indexes a
  sample corpus, runs sample queries, and records latency p50/p95 + correctness
  into a history table. This replaces the ad-hoc `poc-cocoindex` scripts.
- **Out of scope (Phase 2)**: GPU toggle, real-time file watching, end-user
  search product UI, Grafana/Prometheus metrics export.

---

## 3. Alternatives considered

### 3.1 Default to arctic_m_v2 (gap 0.257, cpu 7.49s) — REJECTED
- Highest gap among non-voyage models, but disk footprint 5.9 GB and requires
  `xformers` + `trust_remote_code` (blocker in cocoindex-code). Too brittle for
  a default.

### 3.2 Default to voyage_nano full 2048-dim — REJECTED
- Highest gap (0.282) but 2.5× the CPU cost of 1024 (13.57s vs 5.34s) for +1.1%
  gap. Not justified on a CPU-only VPS.

### 3.3 Index key by (model, dim) only — REJECTED
- Would allow FP16-ST and Q8-llama vectors to share an index dir. Vectors are
  not interchangeable across backends → silent retrieval corruption. Rejected.

### 3.4 Toggle CPU/GPU at runtime in the dashboard — DEFERRED
- No GPU infrastructure exists today. A runtime toggle would be dead code and
  complicate the UI. Deferred to a future ADR contingent on GPU server
  procurement. The backend registry is designed to accept a GPU backend entry
  without schema change, so the deferral is reversible.

### 3.5 In-proc sentence-transformers as production backend — REJECTED for dashboard
- 3–13 s/query latency is acceptable for single-user benchmarking but
  unacceptable for a multi-project dashboard. Also blocks horizontal scaling
  (one model per process). Retained only as dev/benchmark reference.

---

## 4. Consequences

### 4.1 Positive
- **Verified-quality default**: voyage_nano_1024 is benchmarked 8/8 with the
  2nd-highest decisiveness gap and the lowest CPU cost.
- **Backend-agnostic isolation**: switching backend or quantization never
  corrupts existing indexes; each combo gets its own dir + manifest.
- **Multi-project safe**: project boundary is enforced in the index path, so
  cross-project leakage is impossible by construction.
- **Production-ready latency**: HTTP Q8 server (50–200 ms/query) supports the
  dashboard UX; in-proc ST (3–13 s) does not.
- **Reversible GPU path**: backend registry accepts a GPU entry later without
  schema migration.

### 4.2 Negative
- **Unverified Q8 recall**: production default (llama.cpp Q8) has NOT been
  benchmarked in cocoindex-code. Quantization may degrade retrieval vs the
  FP16 reference. **Mitigation**: `recall_verified: false` flag in manifest;
  the dashboard benchmark runner must be run on Q8 before go-live, and the
  result recorded.
- **Pooling misconfiguration risk**: a wrong `--pooling` flag on the llama.cpp
  server silently produces wrong embeddings. **Mitigation**: manifest stores
  `pooling`; the client asserts the server's `/health` or a probe vector
  matches expected shape before indexing.
- **Storage growth**: unlimited lazy models × multiple projects can grow disk
  unbounded. **Mitigation**: 30-day eviction + queue depth 1 + per-project
  model cap advisory.
- **Custom module dependency**: voyage_nano_1024-ST depends on `voyage_slice.Slice`
  + `trust_remote_code`. A sentence-transformers breaking change can break the
  dev backend. **Mitigation**: pin sentence-transformers version; the production
  Q8 backend does not depend on this module.
- **API key exposure**: the current Q8 server uses a weak key (`bismillah123`)
  shared in chat. **Mitigation**: rotate to a strong key in Easypanel env var
  before go-live; never commit keys.

### 4.3 Action items (blocked / follow-up)
1. **Run cocoindex-code benchmark against the Q8 llama.cpp server** with
   `--pooling mean` and record correctness + gap. Until done,
   `recall_verified` stays false.
2. **Rotate the llama.cpp API key** to a strong secret in Easypanel.
3. **Redeploy the Dockerfile** with `--pooling mean` (and the other 3 warning
   fixes) and capture clean startup logs.
4. **Define "project" concretely**: is it a git repo path? a cocoindex-code
   `project_root`? an abstract ID in a metadata DB? This is deferred to
   ADR-0002 (project model) but blocks dashboard implementation.
5. **Implement the dashboard benchmark runner** so Q8 verification can be done
   from the UI rather than ad-hoc Python scripts.

---

## 5. References

- `poc-cocoindex/BENCHMARK.md` — full 11-model benchmark methodology + results.
- `poc-cocoindex/search10_results.txt` — per-query top1/top2/gap for 10 models.
- `poc-cocoindex/perf5_results.json` — search latency p50/p95 for 5 models.
- `poc-cocoindex/setup_bench5.py` — dataset (8 files, 8 queries) + prompt profiles.
- `poc-cocoindex/docker-voyage-nano/Dockerfile` — production Q8 server Dockerfile
  with `--pooling mean` and warning fixes.
- voyage-4-nano `1_Pooling/config.json` — confirms `pooling_mode_mean_tokens: true`.
- HuggingFace `jsonMartin/voyage-4-nano-gguf` — Q8_0 GGUF source (372 MB).
