# ADR-0002: Dashboard UI and Multi-Project Index Isolation

- **Status**: Accepted
- **Date**: 2026-06-27
- **Project**: bit-multi-brain-rag
- **Decision owner**: Engineering
- **Reviewers**: _(pending)_
- **Supersedes**: —
- **Related**: [ADR-0001](0001-embedding-model-and-index-isolation.md)

---

## 1. Context

bit-multi-brain-rag requires a dashboard to (a) observe retrieval quality and
performance, and (b) explore query results per project. The dashboard must also
let users select an embedding model/engine per project and compare backends.

### 1.1 Stakeholder intent (clarified)

- **Audience**: developers / operators (not end-users of a code-search product).
- **Use cases**:
  1. Observability — latency, error rate, index size, model load time, cache hit.
  2. Query explorer — input a query, see top-K files with scores, per project.
  3. Model/engine selection per project — pick from a registry of available
     backends (not a runtime CPU/GPU toggle).
  4. On-demand benchmark runner — compare backends on a sample corpus, store
     results, show history.

### 1.2 Constraints & realities

- **CPU-only infrastructure now.** No GPU server exists. A "CPU vs GPU" toggle
  in the dashboard would be a phantom feature (dead code) until GPU infra is
  procured. GPU support is deferred to a future ADR.
- **CPU vs GPU does not change retrieval quality.** Embeddings are deterministic;
  a correct CPU implementation and a correct GPU implementation produce identical
  vectors. "Testing CPU vs GPU to choose" is a **latency/throughput** benchmark,
  not a quality benchmark. The 11-model quality benchmark (ADR-0001) does not
  need to be re-run for GPU.
- **Index isolation must cover project boundaries.** Project A and Project B
  must have separate indexes even when using the same model+dim. Otherwise a
  query in Project A returns files from Project B (cross-contamination).
- **Lazy unlimited model activation is operationally dangerous if unbounded.**
  10 users triggering 10 simultaneous index builds on one CPU host = OOM/thrash.
  Storage grows without bound. Requires queue + status + eviction.

### 1.3 "Project" definition

A **project** is a corpus boundary and the unit of index isolation. Concretely:

- A project has: `project_id` (uuid), `name`, `source_path` (filesystem or git
  URL), `active_backend` (key into backend registry), `created_at`,
  `last_indexed_at`, `corpus_checksum`.
- A project maps 1:1 to a cocoindex-code `project_root` + `COCOINDEX_CODE_DIR` +
  `COCOINDEX_CODE_RUNTIME_DIR` triple (the 3-path isolation used in the
  benchmark, generalized).

---

## 2. Decision

### 2.1 Index isolation key (extends ADR-0001)

Indexes are isolated by a **5-tuple**:

```
index/{project_id}/{model}_{dim}_{backend}_{quant}/
```

The `project_id` dimension is added on top of ADR-0001's `(model, dim, backend,
quant)`. Two projects using identical model+dim+backend get **separate** index
directories.

Each index directory carries a `manifest.json`:

```json
{
  "project_id": "uuid",
  "model": "voyage-4-nano",
  "dim": 1024,
  "backend": "llama_q8",
  "quantization": "q8",
  "doc_count": 142,
  "last_indexed_at": "2026-06-27T10:00:00Z",
  "corpus_checksum": "sha256:...",
  "status": "ready"
}
```

`status` ∈ `building | ready | stale | failed`. This drives the dashboard's
per-project status indicator and the query fallback logic.

### 2.2 Backend registry (replaces runtime CPU/GPU toggle)

The dashboard does **not** expose a "switch engine CPU/GPU" toggle. Instead it
reads a **backend registry** and offers only the backends that are actually
deployed:

```toml
[embedding.backends.voyage_llama_q8]
description = "voyage-4-nano Q8 via llama.cpp HTTP (production default)"
model = "voyage-4-nano"
dim = 1024
kind = "http"
endpoint = "https://voyage.bitsolution.my.id/v1/embeddings"
auth_header = "Authorization"
auth_env = "LLAMA_API_KEY"
pooling = "mean"
quantization = "q8"
recall_verified = false   # see ADR-0001 §1.3

[embedding.backends.voyage_st_cpu]
description = "voyage-4-nano FP16 via sentence-transformers (dev/benchmark)"
model = "voyageai/voyage-4-nano"
dim = 1024
kind = "inproc"
device = "cpu"
custom_module = "voyage_slice.Slice"
trust_remote_code = true
recall_verified = true

[embedding.backends.arctic_m_v2_st_cpu]
description = "arctic-embed-m-v2.0 FP16 (alternative, 8/8 gap 0.257)"
model = "Snowflake/snowflake-arctic-embed-m-v2.0"
dim = 768
kind = "inproc"
device = "cpu"
recall_verified = true
```

- When a GPU server is procured, a new `[embedding.backends.*_gpu]` entry is
  added to the registry. The dashboard then surfaces it automatically. **No code
  change, no toggle wiring.** This is why a dedicated toggle is unnecessary.
- The dashboard's "select model/engine" control is a dropdown populated from the
  registry. Disabling/retiring a backend = removing its registry entry.

### 2.3 On-demand benchmark runner (the "test to choose" feature)

The "test CPU/GPU to choose" intent is satisfied by an on-demand **benchmark
runner**, not a runtime toggle:

1. User picks one or more backends from the registry + a sample corpus.
2. Runner (background job): index sample corpus → run N sample queries → record
   latency p50/p95/avg + index time + recall (if ground truth available).
3. Results stored in a `benchmarks` table; dashboard shows a comparison view +
   history chart.

This is a **measurement tool**, not a runtime engine switch. It answers
"which backend is faster" without conflating speed with quality.

### 2.4 Lazy build with bounds (corrects "unlimited")

"Unlimited lazy build on demand" is accepted with three operational guards:

1. **Build queue**: at most **1 concurrent index build per host**. Additional
   requests are queued. Prevents CPU thrashing when many users request different
   models simultaneously.
2. **Status indicator**: dashboard shows `building | ready | stale | failed` per
   project×backend. A query against a non-ready index **falls back to the
   project's active backend** with a visible warning, rather than returning
   empty results.
3. **Eviction**: index directories untouched for >30 days are auto-deleted.
   Rebuild happens transparently on next access. Bounds storage growth.

### 2.5 Dashboard scope (phased)

| Feature                                              | Phase | Status |
| ---------------------------------------------------- | ----- | ------ |
| Project sidebar (list + select)                      | MVP   | Do     |
| Query explorer (query → top-K files + scores)        | MVP   | Do     |
| Per-project model/backend selector (from registry)   | MVP   | Do     |
| Index status indicator (`building/ready/stale/failed`)| MVP  | Do     |
| Observability metrics (latency, errors, index size)  | MVP   | Do     |
| On-demand benchmark runner + comparison view         | MVP   | Do     |
| Runtime CPU/GPU toggle                               | —     | **Skip** (no GPU infra) |
| GPU backend registry entry                           | P2    | When GPU procured |
| Auto CPU-vs-GPU comparison                           | P2    | When 2 backends exist |

**MVP implementation note**: a single-file Streamlit/Gradio app is sufficient
for Phase 1. It talks to cocoindex-code search + the backend registry + a small
SQLite store for projects/benchmarks. No need for a separate frontend project
at this stage.

---

## 3. Consequences

### 3.1 Positive

- **No cross-project contamination.** 5-tuple isolation guarantees Project A's
  query never returns Project B's files.
- **Model switching is safe.** Changing a project's `active_backend` cannot
  corrupt another project's index. The old index remains until evicted.
- **Backend selection is honest.** The dropdown only shows backends that are
  actually deployed. No phantom GPU toggle.
- **"Test to choose" is a real measurement**, not a guess. Benchmark runner
  produces comparable numbers across backends.
- **GPU path is future-proof.** Adding GPU = one registry entry + deploy a
  server. No dashboard rewrite.
- **Storage is bounded.** Eviction + build queue prevent unbounded growth and
  CPU thrash.

### 3.2 Negative

- **Storage multiplier.** Index size = `projects × active_backends × corpus`.
  With lazy activation this is bounded in practice, but a project actively using
  4 backends pays 4× storage.
- **Switching model is not instant.** Changing `active_backend` triggers a
  background re-index (minutes for large corpora). The dashboard must set user
  expectations (status indicator + fallback).
- **More moving parts.** Backend registry, build queue, status state machine,
  eviction job, benchmark runner — each is a small service/component to build
  and operate.
- **Q8 recall unverified (carried from ADR-0001).** The production default
  backend (`voyage_llama_q8`) has not been benchmarked for recall in
  cocoindex-code. There is a risk that Q8 quantization + mean pooling produces
  different rankings than the FP16 benchmark. Must be resolved before go-live.

### 3.3 Risks & mitigations

| Risk                                      | Mitigation                                                                 |
| ----------------------------------------- | -------------------------------------------------------------------------- |
| Q8 recall ≠ FP16 benchmark                | Re-run the 8-query benchmark against the Q8 HTTP endpoint before go-live.  |
| `voyage_slice.Slice` breaks on ST upgrade | Pin sentence-transformers version; add smoke test in CI.                   |
| Build queue starvation                    | Build timeout (e.g. 30 min); failed builds marked `failed`, not retried automatically. |
| Eviction deletes an in-use index          | Eviction checks `last_accessed_at`, not `last_indexed_at`; skip if <30d.   |
| Benchmark runner skews production latency | Runner uses a separate sample corpus and isolated process, not prod index. |

---

## 4. Action items

- [ ] Define `project` schema (SQLite migration) — `id, name, source_path, active_backend, created_at, last_indexed_at, corpus_checksum`.
- [ ] Implement backend registry loader (TOML → in-memory map).
- [ ] Implement index manager: build queue (1/host), status state machine, manifest writer.
- [ ] Implement eviction job (cron, >30d untouched).
- [ ] Implement benchmark runner (background job + results table).
- [ ] Build MVP dashboard (Streamlit/Gradio single file): sidebar + query explorer + selector + status + metrics + benchmark trigger.
- [ ] **Verify Q8 recall** against the ADR-0001 8-query benchmark before go-live.
- [ ] Future (P2): deploy GPU endpoint + add registry entry when GPU infra exists.

---

## 5. References

- [ADR-0001: Embedding Model Selection and Index Isolation](0001-embedding-model-and-index-isolation.md)
- Benchmark data: `poc-cocoindex/BENCHMARK.md`, `search10_results.txt`, `perf5_results.json`
- cocoindex-code 3-path isolation: `poc-cocoindex/setup_bench5.py`
- llama.cpp Q8 deployment: `poc-cocoindex/docker-voyage-nano/Dockerfile`
- voyage-4-nano pooling config: `1_Pooling/config.json` (`pooling_mode_mean_tokens: true`)
