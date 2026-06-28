# ADR-0007: Gap Analysis & Improvement Roadmap

- **Status**: Accepted
- **Date**: 2026-06-28
- **Project**: bit-multi-brain-rag
- **Decision owner**: Engineering
- **Reviewers**: _(pending)_
- **Supersedes**: —
- **Related**: ADR-0001 (Embedding Model), ADR-0002 (Dashboard), ADR-0003 (Auth), ADR-0004 (Hybrid Architecture), ADR-0005 (Background Indexing), ADR-0006 (GPU Acceleration)

---

## 1. Context

ADR-0004 menetapkan strategi *hybrid architecture*: ambil ide terbaik dari
`cocoindex-code` (Python, AST-aware chunker, recall 8/8) dan `enowx-rag`
(Go, MCP, Qdrant, multi-project), implementasikan dalam satu codebase Go.

Setelah 6 ADR dan implementasi phase 1-6, project sudah fungsional. Pertanyaan
yang muncul: **apa yang masih kurang dibanding sistem RAG state-of-the-art,
dan apa prioritas improvement-nya?**

ADR ini melakukan riset mendalam terhadap 3 sistem referensi, lalu
membandingkannya dengan state bit-multi-brain-rag saat ini untuk menghasilkan
**gap analysis** dan **improvement roadmap** yang konkret.

---

## 2. Reference Systems Analyzed

### 2.1 cocoindex-code (`ccc`)

Python CLI tool untuk semantic code search, dibangun di atas **CocoIndex engine**
(Rust + Python).

**Kekuatan utama:**
- **AST-aware chunking** via CocoIndex `RecursiveSplitter` (Tree-Sitter, Rust):
  split di boundary fungsi/class/method, target ~1000 chars/chunk, overlap 150.
  Mendukung 33+ bahasa.
- **Incremental indexing**: CocoIndex fingerprint file (content hash), hanya
  re-process file yang berubah. Sangat cepat untuk re-index codebase besar.
- **Daemon architecture**: Background process keep embedder model warm di
  memory. IPC via socket/named pipe. Multiple project cache.
- **Flexible embedding**: Local SentenceTransformers (default Snowflake 22M)
  + 100+ cloud providers via LiteLLM. Asymmetric params (document vs query).
  Rate-limit pacing + retry + batching (64).
- **Structural search** (`ccc grep`): By-example AST pattern matching pakai
  metavariables (`\NAME`, `\(ARGS*)`). Tidak butuh index. Bonus powerful.
- **Custom chunker registry**: User register Python function untuk chunk
  file-type tertentu.
- **Docker**: 2 variants (slim ~450MB LiteLLM-only, full ~5GB dengan torch).
  Path mapping untuk cross-container translation.
- **Developer experience**: `pipx install`, interactive `ccc init` wizard,
  `ccc doctor` diagnostics.
- **Agent integration**: MCP tool, skill files, hooks untuk auto-index.

**Kelemahan:**
- **No reranking** — pure vector similarity.
- **No hybrid search** — tidak ada BM25/sparse fusion.
- **No query expansion** — single query embedding.
- **No built-in evaluation** — tidak ada recall@k/precision measurement.
  Hanya rely external MTEB leaderboard.
- **SQLite/sqlite-vec scalability** — embedded store, mungkin tidak scale ke
  enterprise codebase (ratusan ribu file).
- **LMDB map size limit** — default 4GiB, perlu manual adjust.
- **No cross-repo search** — index per-project, tidak ada shared index.
- **Full scan untuk path filtering** — O(n) saat pakai path GLOB filter.
- **Single MCP tool** — hanya `search`. Tidak ada list-files, get-context,
  explore-index tools.

### 2.2 CocoIndex Engine (Rust + Python)

Data transformation engine yang jadi backbone cocoindex-code.

**Kekuatan utama:**
- **Rust core**: Ultra-performant, memory-safe. Operations di-compile ke native.
- **Incremental processing**: State management via LMDB. Fingerprinting untuk
  skip unchanged data. Ini adalah *the killer feature* — perubahan kecil di
  codebase besar tidak trigger full rebuild.
- **Composable operations**: `split`, `embed`, `detect_language`, `code_match`,
  dll. sebagai building blocks yang dirakit jadi data flow.
- **Extensible**: Custom Python functions via `@coco.fn` decorator. Batching
  otomatis (`batching=True, max_batch_size=64`).
- **Storage targets**: SQLite + sqlite-vec (embedded), dengan enterprise
  path ke vector DB dedicated.

**Kelemahan:**
- **Python orchestration layer** — menambah complexity deployment. Bit-rag
  pilih pure Go untuk single-binary.
- **Embedded SQLite** — tidak cocok multi-tenant scale (bit-rag pakai Qdrant).
- **Learning curve API** — flow definition verbose dibanding simple SDK.

### 2.3 enowx-rag

Go MCP server untuk per-project RAG memory.

**Kekuatan utama:**
- **MCP-first design**: 6 tools ter exposed via stdio. Plug-and-play ke 11 AI
  tools (the assistant, Cursor, Cline, OpenCode, Codex, Factory Droid, dll).
- **Clean Provider interface** (9 methods): trivial add vector store baru.
  Implement: Qdrant, Chroma, pgvector.
- **Asymmetric embedding** (Voyage): `input_type=document` untuk index,
  `input_type=query` untuk search. Implement via optional `QueryEmbedder`
  interface — pattern Go yang elegan.
- **Deterministic point IDs** (UUIDv5): Re-index upsert same point, no
  duplicates. Stale-file reconciliation via `source_file` payload + scroll.
- **Multi-backend**: Qdrant (primary), Chroma, pgvector (HNSW).
- **Compact context retrieval**: `rag_retrieve_context` pre-format hasil jadi
  ready-to-paste LLM context string dengan `[score]` prefix.
- **Robust chunking edge cases**: Hard-split long lines (minified code) supaya
  tidak exceed embedder token limit. Real-world failure mode yang banyak
  RAG system miss.
- **Auto embedder fallback**: Voyage → TEI bila no API key.
- **Factory Droid skill**: 20KB setup wizard dengan per-tool config templates.

**Kelemahan:**
- **No incremental indexing** — full re-chunk + re-embed setiap run. Wasteful
  untuk API berbayar/rate-limited. Hanya stale-file deletion yang "smart".
- **Naive chunking** — character-count based, no AST/semantic awareness.
  Bisa split mid-function. No overlap. Byte hard-split corrupt multibyte UTF-8.
- **No hybrid search** — pure dense. No BM25, no reranking.
- **`.env` indexed by default** — secret leakage risk!
- **Dead OpenAI config code** — parsed tapi tidak wired.
- **No tests** — zero `_test.go` files.
- **No retry/backoff/circuit breaker** — bare `http.Client{}` no timeout.
- **`tei.go` bug**: `context.WithTimeout(bg, 5)` = 5 nanoseconds, bukan 5
  seconds. Selalu timeout, always return default 384. Works by accident.
- **No metrics/observability**.
- **Single-embedder-model assumption** — switching model butuh recreate all
  collections, no migration path.

---

## 3. Bit-Multi-Brain-RAG Current State

### 3.1 Yang sudah dimiliki

| Area | Implementasi | Source of inspiration |
|------|--------------|----------------------|
| AST-aware chunking | tree-sitter Go bindings, 8 bahasa (Go/Python/JS/TS/Rust/Java/C#/C++), fallback naive 60-line | cocoindex-code RecursiveSplitter |
| Multi-project isolation | 5-tuple collection key `{project}_{domain}_{model}_{dim}_{backend}` | enowx-rag per-project collection |
| Async indexing | Background jobs (ADR-0005), per-project lock, live progress, SQLite persistence, cancellation, startup recovery | — (novel) |
| Provider registry | 6 backends (llama_q8, ollama, voyage, openai, cohere, openrouter), curated models, dynamic discovery via `/v1/models` + `/api/tags` | cocoindex-code LiteLLM + enowx-rag multi-backend |
| Hot-swap embedder | Runtime switch tanpa restart, per-model chunk size auto | — (novel) |
| GPU acceleration | RTX 3090 52-236x faster, CDI mode, auto-switch + rollback (ADR-0006) | — (novel) |
| MCP tool | `rag_search_code` over stdio | enowx-rag MCP-first |
| Chunks browser | Full UI: keyword + semantic search, filter, pagination, detail drawer | — (novel) |
| Model comparison | Side-by-side recall + latency benchmark (ADR-0002) | — (novel) |
| Dashboard UI | HTMX server-side render, settings/models/projects/chunks pages | — (novel) |
| Qdrant | REST client, cosine, UUIDv5 IDs, scroll, collection info | enowx-rag Qdrant |
| Health widget | Polled 30s, Qdrant + embedder liveness | — (novel) |

### 3.2 Yang belum ada (gaps)

Dikategorikan berdasarkan impact ke RAG quality dan UX:

#### Search Quality (HIGH impact)
- ❌ **No hybrid search** — pure dense vector. Exact identifier match lemah.
- ❌ **No reranking** — no cross-encoder / LLM rerank.
- ❌ **No query expansion** — no HyDE, no multi-query, no synonym.
- ❌ **No metadata-filtered search** di main search path (chunks browser punya,
  tapi `/api/v1/search` tidak expose filter language/path/symbol).

#### Indexing Efficiency (HIGH impact)
- ❌ **No incremental indexing** — full re-walk + re-embed setiap run. ADR-0005
  acknowledge sebagai future work.
- ❌ **No file watching** — no fsnotify/inotify untuk auto-re-index.
- ❌ **No corpus checksum / staleness detection** — ADR-0001/0002 specify
  tapi tidak implemented.

#### Data Domains (MEDIUM impact)
- ❌ **Code-only** — `doc` dan `task` domain defined tapi no chunker/tool.
- ❌ **No context window assembly** — tidak ada prompt construction /
  token budget management untuk downstream LLM.

#### Security & Auth (MEDIUM impact)
- ❌ **No rate limiting** — ADR-0003 specify, not implemented.
- ❌ **No per-project ACL** — global key = all access.
- ❌ **No audit logging** — ADR-0003 specify, not implemented.
- ❌ **No login page** — ADR-0003 specify, not implemented.
- ❌ **API keys not encrypted at rest** — SQLite plaintext.

#### Observability (LOW-MEDIUM impact)
- ❌ **No Prometheus metrics**.
- ❌ **No structured access logs**.
- ❌ **No job history UI** — ring buffer 50 jobs di memory, no UI.

#### Scalability (LOW impact untuk now)
- ❌ **Single-replica** — in-memory job map, no Redis-backed.
- ❌ **No eviction job** — ADR-0001/0002 specify 30-day, not implemented.

#### Code Search Specific (LOW-MEDIUM impact)
- ❌ **No graph-based retrieval** — no call-graph / relationship-aware search.
- ❌ **No structural search** seperti `ccc grep` (AST pattern matching).
- ❌ **No custom chunker registry** — user tidak bisa register chunker custom.

#### Infrastructure (LOW impact)
- ❌ **Empty `web/templates/`** — HTML render inline di Go strings.
- ❌ **Empty `cmd/mcp-server/`** — leftover, actual MCP di `cmd/mcp/`.
- ❌ **Empty `internal/`**.

---

## 4. Gap Analysis Matrix

Perbandingan fitur per system. ✅ = implemented, ⚠️ = partial, ❌ = missing.

| Feature | cocoindex-code | enowx-rag | bit-multi-brain-rag |
|---------|:---:|:---:|:---:|
| AST-aware chunking | ✅ (33+ langs) | ❌ (naive) | ✅ (8 langs) |
| Incremental indexing | ✅ (fingerprint) | ❌ (full re-embed) | ❌ |
| File watching | ❌ | ❌ | ❌ |
| Hybrid search (BM25+dense) | ❌ | ❌ | ❌ |
| Reranking | ❌ | ❌ | ❌ |
| Query expansion | ❌ | ❌ | ❌ |
| Metadata-filtered search | ⚠️ (lang/path) | ❌ | ⚠️ (chunks browser only) |
| Multi-project isolation | ✅ | ✅ | ✅ |
| MCP tools | ⚠️ (1: search) | ✅ (6 tools) | ⚠️ (1: search) |
| Compact context retrieval | ❌ | ✅ (`rag_retrieve_context`) | ❌ |
| Multi vector store | ❌ (sqlite-vec only) | ✅ (Qdrant/Chroma/pgvector) | ⚠️ (Qdrant only) |
| Multi embedder provider | ✅ (LiteLLM 100+) | ⚠️ (Voyage/TEI) | ✅ (6 backends) |
| Asymmetric embedding | ✅ | ✅ | ⚠️ (Voyage supports, not wired) |
| Hot-swap embedder | ❌ | ❌ | ✅ |
| GPU acceleration | ⚠️ (mps/cuda via ST) | ❌ | ✅ (CDI, auto-switch) |
| Deterministic point IDs | ✅ | ✅ (UUIDv5) | ✅ (UUIDv5) |
| Stale-file reconciliation | ✅ (fingerprint) | ✅ (scroll+delete) | ⚠️ (ListPoints ada, tidak wired) |
| Background indexing | ⚠️ (daemon) | ❌ (sync) | ✅ (jobs manager) |
| Live progress UI | ❌ | ❌ | ✅ (HTMX polling) |
| Chunks browser UI | ❌ | ❌ | ✅ |
| Model comparison | ❌ | ❌ | ✅ |
| Benchmark/evaluation | ❌ (external MTEB) | ❌ | ⚠️ (recall@k + embed-bench) |
| Structural search (`grep`) | ✅ | ❌ | ❌ |
| Custom chunker registry | ✅ | ❌ | ❌ |
| Rate limiting | ❌ | ❌ | ❌ |
| Per-project ACL | ❌ | ❌ | ❌ |
| Audit logging | ❌ | ❌ | ❌ |
| API key encryption at rest | ❌ | ❌ | ❌ |
| Prometheus metrics | ❌ | ❌ | ❌ |
| Cross-repo search | ❌ | ❌ | ❌ |
| Call-graph retrieval | ❌ | ❌ | ❌ |
| Context window assembly | ❌ | ❌ | ❌ |

---

## 5. Improvement Roadmap

Prioritas berdasarkan **impact ke RAG quality × effort implementasi**.
Diurutkan dari highest priority.

### Phase 7: Hybrid Search + Reranking (HIGH impact, MEDIUM effort)

**Problem**: Pure dense vector search lemah untuk exact identifier match.
User search "parseConfig" tidak akan match function `parseConfig` jika
embedding-nya jauh.

**Solution**:
1. **BM25 sparse search**: Tambah Qdrant sparse vector index (supported
   sejak Qdrant 1.7). Index content + symbol + file_path sebagai sparse.
2. **Fusion**: Reciprocal Rank Fusion (RRF) merge dense + sparse results.
   RRF robust, tidak butuh score calibration.
3. **Reranking** (optional): Cross-encoder rerank top-50 → top-10. Bisa
   pakai `BAAI/bge-reranker-base` via local server, atau LLM-based
   (prompt: "rank these chunks by relevance to query").

**Inspiration**: enowx-rag & cocoindex-code both lack this. Industry
standard (Vespa, Weaviate, Qdrant) all support hybrid. Bit-rag bisa lead.

**Files to touch**: `pkg/rag/qdrant.go` (sparse index + fusion), `pkg/dashboard/search.go` (rerank step), new `pkg/rerank/`.

### Phase 8: Incremental Indexing (HIGH impact, MEDIUM effort)

**Problem**: Full re-embed setiap index run. Wasteful untuk API berbayar
(Voyage $0.12/M tokens) dan lambat untuk codebase besar.

**Solution**:
1. **File fingerprinting**: SHA-256 content hash per file, stored di SQLite
   `file_fingerprints` table. Skip embed jika hash match.
2. **Stale detection**: Compare current file tree vs stored fingerprints.
   Hanya re-embed changed/added files. Delete points untuk removed files
   (ListPoints sudah ada, tinggal wire).
3. **Git-diff mode** (optional): Bila project adalah git repo, pakai
   `git diff HEAD~N --name-only` untuk detect changed files. Lebih cepat
   dari full walk.

**Inspiration**: cocoindex-code (CocoIndex fingerprinting), enowx-rag
(stale-file deletion via scroll — bit-rag punya `ListPoints` tapi belum wired).

**Files to touch**: `pkg/indexer/indexer.go` (fingerprint check), `pkg/store/` (new table), `pkg/rag/qdrant.go` (delete by filter).

### Phase 9: MCP Tool Expansion (MEDIUM impact, LOW effort)

**Problem**: Hanya 1 MCP tool (`rag_search_code`). Agent tidak bisa
explore index structure, get context, atau manage projects via MCP.

**Solution**: Tambah tools mengikuti enowx-rag pattern:
1. `rag_retrieve_context` — search + pre-format jadi LLM-ready context
   string dengan `[score]` prefix. Copy enowx-rag pattern.
2. `rag_index_project` — trigger indexing via MCP (delegate ke dashboard
   `/api/v1/index`).
3. `rag_list_projects` — list registered projects.
4. `rag_get_chunk` — fetch single chunk by point ID (untuk follow-up
   inspection).
5. `rag_compare_models` — trigger model comparison via MCP.

**Inspiration**: enowx-rag (6 tools), cocoindex-code (single tool — bit-rag
bisa exceed both).

**Files to touch**: `cmd/mcp/main.go` (register new tools), new tools di `pkg/mcp/`.

### Phase 10: File Watching + Auto-Reindex (MEDIUM impact, MEDIUM effort)

**Problem**: User harus manual trigger re-index setelah code changes.

**Solution**:
1. **fsnotify watcher**: Watch project root_path. On file change/add/delete,
   enqueue delta index job (hanya changed files).
2. **Debounce**: Batch changes dalam 5s window, trigger 1 job (avoid
   index-per-keystroke).
3. **Dashboard toggle**: Settings page "Auto-reindex: on/off" per project.
4. **MCP hook**: Optional hook untuk auto-index on session start
   (seperti cocoindex-code hooks).

**Inspiration**: cocoindex-code hooks (session start auto-index). enowx-rag
tidak punya. Bit-rag bisa lead dengan real-time.

**Files to touch**: new `pkg/watcher/`, `pkg/dashboard/settings.go` (toggle).

### Phase 11: Query Expansion + Context Assembly (MEDIUM impact, MEDIUM effort)

**Problem**: Single query embedding bisa miss relevant results.
Tidak ada prompt construction untuk downstream LLM.

**Solution**:
1. **HyDE** (Hypothetical Document Embeddings): Generate hypothetical
   answer/code via LLM, embed itu sebagai query. Boost recall.
2. **Multi-query**: Generate 3 paraphrased queries via LLM, embed all,
   merge results (RRF atau score averaging).
3. **Context window assembly**: Given top-K retrieved chunks, assemble
   menjadi coherent context string dengan token budget management.
   Include file path, line range, language tag. Copy enowx-rag
   `rag_retrieve_context` pattern tapi lebih sophisticated.

**Inspiration**: Industry standard RAG pattern. Neither reference impl this.

**Files to touch**: new `pkg/expand/` (HyDE, multi-query), `pkg/rag/context.go` (assembly).

### Phase 12: Security Hardening (MEDIUM impact, MEDIUM effort)

**Problem**: No rate limiting, no audit log, no per-project ACL, API keys
plaintext.

**Solution**:
1. **Rate limiting**: Token bucket per API key. Echo middleware.
   Configurable limit (default 100 req/min).
2. **Audit logging**: SQLite `audit_log` table. Log mutating actions
   (index, create/delete project, switch GPU, create/delete model) dengan
   key fingerprint + timestamp.
3. **Per-project ACL**: API key → project scope mapping. New `api_key_projects`
   table. Key tanpa scope = all projects (backward compat).
4. **API key encryption at rest**: AES-256-GCM. Key dari env var
   `DASHBOARD_ENCRYPTION_KEY`. Fallback: plaintext jika key tidak set
   (dev mode).
5. **Login page**: Simple HTML form → sessionStorage. ADR-0003 specify.

**Inspiration**: ADR-0003 specify all these. enowx-rag & cocoindex-code
juga tidak implement — bit-rag bisa lead.

**Files to touch**: `pkg/auth/` (rate limit, ACL), `pkg/store/` (audit log, key_projects), `pkg/dashboard/settings.go` (login UI).

### Phase 13: Multi-Domain Support (LOW-MEDIUM impact, MEDIUM effort)

**Problem**: Code-only. `doc` and `task` domains defined tapi no impl.

**Solution**:
1. **Doc domain**: Chunk markdown/docs (heading-aware splitter). Index ke
   collection terpisah. `DocRAGTool` MCP tool.
2. **Task domain**: Index task/ticket descriptions + comments. `TaskRAGTool`.
3. **Cross-domain search**: Query bisa span multiple domains (fusion results
   dari code + doc + task collections).

**Inspiration**: ADR-0002 specify domains. Neither reference impl multi-domain.

**Files to touch**: new `pkg/chunker/doc.go`, `pkg/chunker/task.go`, `cmd/mcp/` (new tools).

### Phase 14: Observability (LOW impact, LOW effort)

**Problem**: No metrics, no structured logs, no job history UI.

**Solution**:
1. **Prometheus metrics**: `/metrics` endpoint. Counters: search_requests,
   index_jobs, embed_latency. Histograms: search_latency, embed_batch_size.
2. **Structured access logs**: slog JSON handler. Request method, path,
   status, latency, key fingerprint.
3. **Job history UI**: Settings page tab "Job History". Render dari ring
   buffer (50 jobs) + SQLite audit trail.

**Inspiration**: enowx-rag & cocoindex-code both lack this. Industry standard.

**Files to touch**: `pkg/dashboard/server.go` (middleware), new `pkg/metrics/`.

### Phase 15: Structural Search (LOW impact, MEDIUM effort)

**Problem**: Tidak ada by-example AST pattern matching seperti `ccc grep`.

**Solution**: Implement `ccc grep` equivalent di Go:
1. Parse query sebagai AST pattern dengan metavariables (`$NAME`, `$ARGS`).
2. Match pattern terhadap tree-sitter AST setiap indexed file.
3. Return matching nodes dengan capture groups.

**Inspiration**: cocoindex-code `grep.py` (code_match operation). enowx-rag
tidak punya.

**Files to touch**: new `pkg/structural/`.

---

## 6. Priority Summary

| Phase | Feature | Impact | Effort | Priority |
|-------|---------|--------|--------|----------|
| 7  | Hybrid Search + Reranking | HIGH | MEDIUM | 🔴 P0 |
| 8  | Incremental Indexing | HIGH | MEDIUM | 🔴 P0 |
| 9  | MCP Tool Expansion | MEDIUM | LOW | 🟡 P1 |
| 10 | File Watching + Auto-Reindex | MEDIUM | MEDIUM | 🟡 P1 |
| 11 | Query Expansion + Context Assembly | MEDIUM | MEDIUM | 🟡 P1 |
| 12 | Security Hardening | MEDIUM | MEDIUM | 🟡 P1 |
| 13 | Multi-Domain Support | LOW-MED | MEDIUM | 🟢 P2 |
| 14 | Observability | LOW | LOW | 🟢 P2 |
| 15 | Structural Search | LOW | MEDIUM | 🟢 P2 |

**P0 (must-have untuk production-grade RAG)**: Hybrid search + incremental indexing.
Ini 2 gap yang paling impact ke quality + cost.

**P1 (should-have untuk competitive)**: MCP expansion, file watching, query
expansion, security.

**P2 (nice-to-have untuk completeness)**: Multi-domain, observability,
structural search.

---

## 7. What Bit-Multi-Brain-RAG Already Leads On

Jangan lupa — bit-rag sudah unggul di beberapa area yang reference systems
tidak punya:

1. **GPU acceleration with auto-switch** — neither cocoindex-code maupun
   enowx-rag punya CDI mode + automated CPU↔GPU switch + rollback.
2. **Hot-swap embedder** — runtime ganti model tanpa restart. cocoindex-code
   butuh `ccc reset && ccc index`. enowx-rag butuh recreate collections.
3. **Live progress UI** — HTMX polling job status real-time. Both references
   blind di sini.
4. **Chunks browser** — full UI untuk inspect indexed points dengan filter.
   Neither reference punya.
5. **Model comparison** — side-by-side recall + latency benchmark. Neither
   reference punya built-in eval.
6. **Per-model chunk size** — auto-derive dari `min(MaxContextTokens*0.8, 2000)`.
   cocoindex-code pakai fixed 1000 chars. enowx-rag pakai fixed 1500 chars.
7. **Background job manager** — per-project lock, cancellation, startup
   recovery, SQLite audit trail. cocoindex-code daemon-based (different
   model), enowx-rag sync (no background).

Bit-rag **bukan** mengikuti references — dia **menggabungkan ide terbaik
dan menambah inovasi sendiri**.

---

## 8. Decision

Adopsi roadmap Phase 7-15 sebagai sequence improvement. Eksekusi berurutan
dari P0 → P1 → P2. Setiap phase menghasilkan ADR baru yang supersede atau
extend ADR terkait.

**Immediate next**: Phase 7 (Hybrid Search + Reranking) dan Phase 8
(Incremental Indexing) sebagai dua paralel workstream, karena keduanya
P0 dan independen.

---

## 9. References

- ADR-0001 — Embedding Model Selection and Index Isolation
- ADR-0002 — Dashboard Scope and Multi-Project Index Isolation
- ADR-0003 — Dashboard Authentication via API Key
- ADR-0004 — Hybrid Architecture (Best of cocoindex-code + enowx-rag)
- ADR-0005 — Background Indexing Jobs
- ADR-0006 — GPU Embedding Acceleration
- `references/cocoindex-code/` — Python semantic code search (CocoIndex engine)
- `references/cocoindex/` — Rust + Python data transformation engine
- `references/enowx-rag/` — Go MCP RAG memory server
- `pkg/dashboard/gpu.go` — GPU detection + switch logic
- `pkg/indexer/indexer.go` — current indexing pipeline
- `pkg/chunker/chunker.go` — AST-aware tree-sitter chunker
- `cmd/embed-bench/main.go` — embedder latency benchmark tool
