# ADR-0009: cocoindex-code Reference Analysis

- **Status**: Accepted
- **Date**: 2026-06-28
- **Project**: bit-multi-brain-rag
- **Decision owner**: Engineering
- **Reviewers**: _(pending)_
- **Supersedes**: —
- **Related**: ADR-0004 (Hybrid Architecture), ADR-0007 (Gap Analysis & Roadmap), ADR-0008 (Hybrid Search)

---

## 1. Context

ADR-0004 menetapkan `cocoindex-code` sebagai **conceptual reference** untuk
AST-aware chunking dan embedding pipeline. ADR-0007 merangkum kekuatan dan
kelemahannya secara singkat. ADR ini menyimpan **analisis teknis mendalam**
dari cocoindex-code supaya detail arsitektur, implementasi, dan lesson learned
terdokumentasi untuk future reference dan decision-making.

cocoindex-code (CLI: `ccc`) adalah AST-based semantic code search tool yang
dibangun di atas **CocoIndex engine** (Rust + Python). Lisensi Apache-2.0,
Python >=3.11, package `cocoindex-code` di PyPI.

**Problem yang dia selesaikan:**
- Grep/find tidak bisa cari code bila user tidak tahu keyword exact.
- Coding agent (Claude Code, Codex, Cursor, Grok) waste token baca file utuh
  untuk cari snippet relevan.
- Klaim: hemat 70% token untuk coding agent.

Analysis ini berdasarkan pembacaan seluruh 20 source file di
`src/cocoindex_code/`, plus direktori `docker/`, `skills/`, `hooks/`,
`scripts/`, `tests/`, serta `README.md`, `EMBEDDINGS.md`, `pyproject.toml`,
dan `CLAUDE.md`.

---

## 2. Architecture

### 2.1 High-Level Architecture

```
┌─────────────────────────────────────────────────────────┐
│  User / Coding Agent (Claude, Codex, Cursor, Grok)      │
└────────────┬───────────────────┬────────────────────────┘
             │ CLI (ccc)         │ MCP stdio
             ▼                   ▼
┌────────────────────┐  ┌──────────────────┐
│  cli.py (Typer)    │  │  server.py       │
│  ccc init/index/   │  │  (FastMCP)       │
│  search/grep/...   │  │  search tool     │
└────────┬───────────┘  └────────┬─────────┘
         │ IPC (msgpack)         │ delegates to client
         ▼                       ▼
┌────────────────────────────────────────────┐
│  client.py (IPC client)                    │
│  - connect + version handshake             │
│  - send request, read response             │
└────────┬───────────────────────────────────┘
         │ socket / named pipe
         ▼
┌─────────────────────────────────────────────────────────┐
│  daemon.py (background daemon process)                   │
│  - ProjectRegistry (caches loaded projects)              │
│  - Listener loop, per-request connection handling        │
│  - Loads embedder once, reuses across projects           │
└────────┬────────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────────────────┐
│  project.py (wraps CocoIndex Environment + App)          │
│  - run_index() → indexer.py → CocoIndex Rust engine      │
│  - search() → query.py → sqlite-vec KNN                  │
└─────────────────────────────────────────────────────────┘
         │
         ▼
┌─────────────────────────────────────────────────────────┐
│  Storage (in <project>/.cocoindex_code/)                 │
│  - cocoindex.db (LMDB - CocoIndex state/incremental)     │
│  - target_sqlite.db (SQLite + sqlite-vec vector index)   │
└─────────────────────────────────────────────────────────┘
```

### 2.2 Source Code Structure (`src/cocoindex_code/`)

| File | Lines | Purpose |
|------|-------|---------|
| `cli.py` | 1094 | Typer CLI: `ccc init`, `index`, `search`, `grep`, `status`, `mcp`, `doctor`, `reset`, `daemon` |
| `daemon.py` | 650 | Background daemon: listener loop, ProjectRegistry, request dispatch, signal handling |
| `client.py` | 570 | IPC client: per-request connections, daemon lifecycle (start/stop/restart), version handshake |
| `project.py` | 310 | Wraps `coco.Environment` + `coco.App`; orchestrates indexing and search per project |
| `indexer.py` | 95 | CocoIndex app definition: `process_file()` (chunk + embed + store) and `indexer_main()` (file walk + mount) |
| `query.py` | 130 | Vector search: vec0 KNN query, full-scan fallback with path filtering, L2→cosine conversion |
| `server.py` | 320 | MCP server (FastMCP): exposes search tool; legacy backward-compat entry point |
| `grep.py` | 440 | Structural search (`ccc grep`): by-example pattern matching via `cocoindex code_match`, no index needed |
| `file_walk.py` | 180 | Source-file walking with include/exclude globs + nested `.gitignore` awareness |
| `chunking.py` | 30 | Public API for custom chunkers: `Chunk`, `ChunkerFn`, `CHUNKER_REGISTRY` |
| `shared.py` | 140 | Context keys, embedder factory (`create_embedder`), `CodeChunk` schema, `check_embedding` |
| `litellm_embedder.py` | 120 | `PacedLiteLLMEmbedder`: request serialization, rate-limit retries, batching (64) |
| `embedder_defaults.py` | 140 | Curated default `indexing_params`/`query_params` for known models (used by `ccc init`) |
| `embedder_params.py` | 100 | Validation/resolution of embedder params at daemon runtime |
| `settings.py` | 580 | YAML settings schema (`UserSettings`, `ProjectSettings`), path helpers, `.gitignore` spec |
| `protocol.py` | 160 | IPC message types as `msgspec.Struct` tagged unions + msgpack encode/decode |
| `schema.py` | 20 | `CodeChunk` and `QueryResult` dataclasses |
| `_daemon_paths.py` | 50 | Platform-specific daemon socket/PID/log paths |

### 2.3 Other Directories

| Directory | Contents |
|-----------|----------|
| `docker/` | `Dockerfile` (slim + full variants), `docker-compose.yml`, `entrypoint.sh` |
| `skills/ccc/` | Agent skill (`SKILL.md`) teaching coding agents to use `ccc` autonomously |
| `hooks/` | `hooks.json` — SessionStart hook for auto-indexing (Grok plugin) |
| `scripts/` | `find_best_models.py` (MTEB model discovery), `MTEB-RANKINGS.md` |
| `tests/` | 17 test files: e2e, daemon, client, grep, settings, embedder params, chunker registry |
| `.github/workflows/` | `pre-commit.yml`, `release.yml` |
| `.claude-plugin/` | `marketplace.json` for Claude Code plugin marketplace |

---

## 3. Features

### 3.1 Core Features

| Feature | Description |
|---------|-------------|
| Semantic Code Search | Find code by natural language query or code snippet using vector similarity |
| AST-Aware Chunking | Splits files at logical boundaries (functions, classes) using Tree-Sitter, targeting ~1000 chars/chunk |
| Incremental Indexing | Only re-indexes changed files (CocoIndex fingerprinting), fast updates |
| Multi-Language Support | 33+ languages: Python, JS/TS, Rust, Go, Java, C/C++, C#, SQL, Shell, Ruby, PHP, Swift, Kotlin, Scala, etc. |
| Structural Search (`ccc grep`) | By-example pattern matching on syntax trees (metavariables `\NAME`, `\(ARGS*)`), no index needed |
| MCP Server | Exposes search tool via stdio for Claude/Codex/Cursor/Grok |
| Agent Skill | `ccc` skill teaches agents to auto-initialize, index, and search |
| SessionStart Hook | Auto-runs `ccc index` on session start when `.cocoindex_code/` exists |
| Daemon Architecture | Background process keeps embedder model warm across sessions; IPC via socket/named pipe |
| Custom Chunkers | Users register Python functions to control chunking for specific file types |
| Docker Support | Slim (~450MB) and Full (~5GB) images, docker compose one-liner |
| Doctor Diagnostics | `ccc doctor` checks settings, daemon health, model, file matching, index status |
| Telemetry | Anonymous usage tracking (opt-out via `COCOINDEX_DISABLE_USAGE_TRACKING=1`) |

### 3.2 Embedding Pipeline Features

| Feature | Description |
|---------|-------------|
| Local Embeddings | sentence-transformers (default: `Snowflake/snowflake-arctic-embed-xs`, 22M params) — no API key |
| Cloud Embeddings | LiteLLM bridge: 100+ providers (OpenAI, Voyage, Gemini, Cohere, Bedrock, Mistral, Nebius, etc.) |
| Local Server Embeddings | Ollama, llama.cpp, vLLM, LM Studio via OpenAI-compatible endpoints |
| Asymmetric Embedding | `indexing_params` / `query_params` for models with document vs query modes (Cohere, Voyage, Nomic, Gemini) |
| Request Pacing | `min_interval_ms` throttles LiteLLM requests to avoid rate limits |
| Rate-Limit Retries | Automatic retry with exponential backoff (up to 6 retries) on 429 errors |
| Batching | Up to 64 chunks batched into a single embedding API request |

### 3.3 Search/Retrieval Features

| Feature | Description |
|---------|-------------|
| Vector Similarity | L2 distance via sqlite-vec vec0 virtual table, converted to cosine similarity score |
| Language Filtering | Filter by programming language(s) — uses vec0 partition keys for index-level filtering |
| Path Filtering | Filter by file path GLOB patterns — triggers full scan with SQL-level distance computation |
| Pagination | `limit` and `offset` parameters |
| Refresh Index | `refresh_index=True` updates index before querying (MCP default) |

### 3.4 What's NOT Present

| Feature | Status |
|---------|--------|
| Reranking | Not implemented — pure vector similarity |
| Query Expansion | Not implemented — single query embedding |
| Hybrid Search | Not implemented — pure dense vector search (no BM25/sparse) |
| Benchmark/Evaluation Suite | No built-in eval; references external MTEB leaderboard only |

> **Note**: ADR-0008 menutup gap hybrid search untuk bit-multi-brain-rag.

---

## 4. Code Logic Deep Dive

### 4.1 Chunking (AST-Aware)

**Key files**: `indexer.py`, `chunking.py`, CocoIndex's `RecursiveSplitter`

Chunking adalah language-aware dan AST-based, didelegasikan ke CocoIndex's
`RecursiveSplitter` (Rust implementation menggunakan Tree-Sitter):

```python
# indexer.py
CHUNK_SIZE = 1000      # characters (~300 tokens)
MIN_CHUNK_SIZE = 250
CHUNK_OVERLAP = 150

splitter = RecursiveSplitter()

# In process_file():
chunks = splitter.split(
    content,
    chunk_size=CHUNK_SIZE,
    min_chunk_size=MIN_CHUNK_SIZE,
    chunk_overlap=CHUNK_OVERLAP,
    language=language,  # detected via detect_code_language()
)
```

`RecursiveSplitter` menggunakan Tree-Sitter untuk understand code structure
dan split di logical boundaries (functions, classes, methods) sambil
respect target chunk size. Parameter `language` dideteksi via
`cocoindex.ops.text.detect_code_language(filename=...)` berdasarkan file extension.

**Language detection chain** (`indexer.py`):
1. Check project `language_overrides` (custom ext→lang mapping)
2. Fall back to `detect_code_language()` (extension-based)
3. Default to `"text"`

**Custom chunker registry** (`chunking.py`): User dapat register Python
function via `settings.yml`:

```python
# Signature: (path: Path, content: str) -> (language_override: str|None, chunks: list[Chunk])
ChunkerFn = Callable[[Path, str], tuple[str | None, list[Chunk]]]
```

Registry di-resolve saat daemon startup di `daemon.py:_resolve_chunker_registry()`
dan di-inject via CocoIndex context keys.

> **Lesson untuk bit-rag**: cocoindex-code pakai fixed 1000 chars. bit-rag
> sudah improve dengan per-model chunk size auto-derive
> (`min(MaxContextTokens*0.8, 2000)`). Lihat ADR-0007 §7.

### 4.2 Embedding Pipeline

**Key files**: `shared.py`, `litellm_embedder.py`, `embedder_defaults.py`, `embedder_params.py`

**Embedder factory** (`shared.py:create_embedder()`):
- Jika `provider == "sentence-transformers"`: buat `SentenceTransformerEmbedder`
  (from `cocoindex.ops.sentence_transformers`), load model in-process.
- Otherwise: buat `PacedLiteLLMEmbedder` (custom subclass of `LiteLLMEmbedder`).

**`PacedLiteLLMEmbedder`** (`litellm_embedder.py`) menambahkan:
- **Request serialization** via `asyncio.Lock` — ensures `min_interval_ms`
  spacing between requests.
- **Rate-limit retries** — parses `"Please try again in Xms"` from error
  messages, retries up to 6 times with backoff.
- **Batching** — `@coco.fn.as_async(batching=True, max_batch_size=64)` —
  up to 64 texts per API call.

**Asymmetric embedding params**: Beberapa model (Cohere, Voyage, Nomic,
Snowflake) punya mode berbeda untuk documents vs queries. Daemon resolve
ini saat startup:
- `indexing_params` spread into `embed()` during indexing
  (e.g., `{"input_type": "search_document"}`)
- `query_params` spread into `embed()` at query time
  (e.g., `{"input_type": "search_query"}`)

Curated defaults di `embedder_defaults.py` di-apply oleh `ccc init` untuk
model yang dikenali.

**Data flow indexing** (`indexer.py:process_file()`):

```
file → read_text() → detect_language → RecursiveSplitter.split() →
  for each chunk: IdGenerator.next_id(chunk.text) → embedder.embed(text, **indexing_params) →
    CodeChunk(id, file_path, language, content, start_line, end_line, embedding) →
      table.declare_row()
```

Embedding disimpan sebagai `Annotated[NDArray[float32], EMBEDDER]` di
`CodeChunk` schema.

> **Lesson untuk bit-rag**: bit-rag sudah punya 6 backends (llama_q8, ollama,
> voyage, openai, cohere, openrouter) + dynamic discovery via `/v1/models`.
> Lihat ADR-0007 §3.1.

### 4.3 Search/Retrieval

**Key file**: `query.py`

Search menggunakan sqlite-vec's vec0 virtual table untuk KNN
(k-nearest-neighbor) search:

```python
async def query_codebase(query, target_sqlite_db_path, env, limit=10, offset=0, languages=None, paths=None):
    # 1. Generate query embedding
    query_embedding = await embedder.embed(query, **query_params)
    embedding_bytes = query_embedding.astype("float32").tobytes()

    # 2. Execute query
    with db.readonly() as conn:
        if paths:
            # Full scan with SQL-level distance computation
            rows = _full_scan_query(conn, embedding_bytes, limit, offset, languages, paths)
        elif not languages or len(languages) == 1:
            # vec0 KNN (uses partition key for language filter)
            rows = _knn_query(conn, embedding_bytes, limit + offset, lang)
        else:
            # Multiple languages: KNN per language, merge with heapq.nsmallest
            rows = heapq.nsmallest(...)
```

**KNN query** (`_knn_query`): Uses vec0's native MATCH operator dengan
partition key filtering:

```sql
SELECT file_path, language, content, start_line, end_line, distance
FROM code_chunks_vec
WHERE embedding MATCH ? AND k = ? AND language = ?
ORDER BY distance
```

**Full scan** (`_full_scan_query`): Saat path filtering dibutuhkan (vec0
tidak bisa filter arbitrary paths), fallback ke compute distance untuk
semua rows di SQL:

```sql
SELECT ..., vec_distance_L2(embedding, ?) as distance
FROM code_chunks_vec
WHERE language IN (...) AND (file_path GLOB ? OR ...)
ORDER BY distance
LIMIT ? OFFSET ?
```

**Score conversion**: L2 distance → cosine similarity:
`score = 1.0 - distance² / 2.0` (exact untuk unit-normalized vectors).

> **Lesson untuk bit-rag**: cocoindex-code pakai SQLite + sqlite-vec (embedded).
> bit-rag pilih Qdrant untuk multi-tenant scalability. ADR-0008 menambah
> sparse vector + RRF fusion untuk hybrid search.

### 4.4 MCP Tool Interface

**Key file**: `server.py`

MCP server menggunakan FastMCP in stdio mode. Exposes single search tool:

```python
@mcp.tool(name="search", description="...")
async def search(
    query: str,                      # Natural language query or code snippet
    limit: int = 5,                  # Max results (1-100)
    offset: int = 0,                 # Pagination
    refresh_index: bool = True,      # Update index before searching
    languages: list[str] | None = None,  # Filter by language
    paths: list[str] | None = None,      # Filter by path GLOB
) -> SearchResultModel:
```

Returns `CodeChunkResult` objects dengan `file_path`, `language`, `content`,
`start_line`, `end_line`, `score`.

MCP tool delegates ke daemon via `client.search()` / `client.index()`,
running blocking IPC calls di thread pool executor.

> **Lesson untuk bit-rag**: cocoindex-code hanya expose 1 MCP tool.
> ADR-0007 Phase 9 merencanakan expansion ke 5+ tools
> (`rag_retrieve_context`, `rag_index_project`, `rag_list_projects`,
> `rag_get_chunk`, `rag_compare_models`).

### 4.5 Data Flow: Source Files → Searchable Index

```
1. ccc init
   ├── Creates ~/.cocoindex_code/global_settings.yml (embedding config)
   └── Creates <project>/.cocoindex_code/settings.yml (include/exclude patterns)

2. ccc index (or MCP search with refresh_index=True)
   ├── client.index() → daemon IPC → Project.run_index()
   ├── CocoIndex App.update() triggers incremental processing:
   │   ├── localfs.walk_dir(CODEBASE_DIR, path_matcher=GitignoreAwareMatcher)
   │   │   - include/exclude globs + nested .gitignore
   │   │   - CocoIndex fingerprints files (content hash); only changed files reprocessed
   │   └── For each changed file → process_file():
   │       ├── read_text() (skip on UnicodeDecodeError)
   │       ├── detect language (overrides → extension → "text")
   │       ├── chunk: custom chunker OR RecursiveSplitter (AST-aware)
   │       ├── For each chunk:
   │       │   ├── IdGenerator.next_id(text) → deterministic ID
   │       │   ├── embedder.embed(text, **indexing_params) → float32 vector
   │       │   └── table.declare_row(CodeChunk(...))
   │       └── CocoIndex writes to target_sqlite.db (vec0 virtual table)
   └── Streams IndexingProgress updates back to client

3. ccc search "query"
   ├── client.search() → daemon IPC → Project.search()
   ├── embedder.embed(query, **query_params) → query vector
   ├── vec0 KNN: embedding MATCH query_vector AND k=limit
   │   (partition filter on language if specified)
   │   (full scan if path filter specified)
   └── Return sorted results with cosine similarity scores
```

---

## 5. Configuration & Deployment

### 5.1 Configuration (Two YAML files)

**User Settings** (`~/.cocoindex_code/global_settings.yml`):

```yaml
embedding:
  provider: sentence-transformers  # or "litellm"
  model: Snowflake/snowflake-arctic-embed-xs
  device: mps                      # cpu, cuda, mps (auto-detected)
  min_interval_ms: 300             # LiteLLM request pacing
  indexing_params: {input_type: search_document}  # optional
  query_params: {input_type: search_query}        # optional
envs:
  OPENAI_API_KEY: your-key         # only if not in shell env
```

**Project Settings** (`<project>/.cocoindex_code/settings.yml`):

```yaml
include_patterns: ["**/*.py", "**/*.ts", ...]   # 60+ defaults
exclude_patterns: ["**/node_modules", "**/.*", ...]
language_overrides: [{ext: inc, lang: php}]
chunkers: [{ext: toml, module: my_module:toml_chunker}]
```

### 5.2 Key Environment Variables

| Variable | Purpose |
|----------|---------|
| `COCOINDEX_CODE_DIR` | Override `~/.cocoindex_code/` location |
| `COCOINDEX_CODE_DB_PATH_MAPPING` | Remap DB location (e.g., for Docker: `/workspace=/db-files`) |
| `COCOINDEX_CODE_HOST_PATH_MAPPING` | Translate container paths to host paths for display |
| `COCOINDEX_CODE_HOST_CWD` | Forward host pwd into Docker exec |
| `COCOINDEX_CODE_DAEMON_SUPERVISED` | External supervisor owns daemon respawn |
| `COCOINDEX_LMDB_MAP_SIZE` | LMDB max size in bytes (default 4 GiB) |
| `COCOINDEX_DISABLE_USAGE_TRACKING` | Opt out of telemetry |
| `COCOINDEX_CODE_EXTRA_EXTENSIONS` | Legacy: extra file extensions |
| `COCOINDEX_CODE_EXCLUDE_PATTERNS` | Legacy: exclude patterns |

### 5.3 Docker Deployment

Two image variants:
- `:latest` (slim, ~450MB): LiteLLM-only, cloud embeddings.
- `:full` (~5GB): Includes sentence-transformers + torch + pre-baked default model.

`docker-compose.yml` mounts `$HOME:/workspace`, persists data in
`cocoindex-data` volume, sets `COCOINDEX_CODE_DAEMON_SUPERVISED=1`
(entrypoint restarts daemon).

---

## 6. Benchmark/Evaluation

**Tidak ada built-in benchmark atau evaluation suite** di repository ini.
Project rely pada external benchmarks:

- `scripts/find_best_models.py`: Queries live MTEB (Massive Text Embedding
  Benchmark) results dataset di HuggingFace untuk find top-performing
  embedding models. Evaluates on code retrieval tasks:
  `CodeSearchNetRetrieval`, `CosQA`, `StackOverflowQA`, `HumanEvalRetrieval`,
  `MBPPRetrieval`.
- `scripts/MTEB-RANKINGS.md`: Pre-generated rankings (data from 2026-06-15)
  showing code search scores by model size tier.

Tidak ada metrics seperti recall@k, precision@k, atau latency yang di-compute
di dalam tool itu sendiri. Tidak ada evaluation datasets atau test queries
bundled. Test suite (`tests/`) adalah functional/integration testing, bukan
retrieval quality evaluation.

> **Lesson untuk bit-rag**: bit-rag sudah punya `cmd/bench` (recall@k +
> latency) dan `cmd/embed-bench` (embedder latency). Lihat ADR-0007 §7.

---

## 7. Strengths

1. **Excellent developer experience**: Zero-config setup
   (`pipx install cocoindex-code[full]` then go), interactive `ccc init`
   wizard, sensible defaults (22M param model yang run di CPU).
2. **Strong agent integration story**: Works as MCP server, agent skill,
   dan dengan SessionStart hooks. Supports Claude Code, Codex, Cursor, Grok,
   OpenCode, Kilo Code out of the box.
3. **Daemon architecture**: Keeps embedding model warm in memory, supports
   multiple projects, version handshake untuk safe upgrades. Well-designed
   lifecycle management dengan graceful shutdown escalation.
4. **AST-aware chunking**: Tree-Sitter-based splitting di logical boundaries
   produces coherent code chunks, avoiding the "split mid-function" problem
   of naive character-based chunkers.
5. **Incremental indexing**: CocoIndex fingerprints files dan hanya
   reprocess changed ones, making re-indexing fast bahkan untuk codebase besar.
6. **Flexible embedding support**: 100+ providers via LiteLLM + local
   SentenceTransformers. Curated asymmetric params untuk known models.
   Rate-limit handling dengan pacing dan retries.
7. **Structural search (`ccc grep`)**: Bonus feature — by-example AST
   pattern matching tanpa needing an index. Useful untuk finding function
   definitions, call sites, etc.
8. **Production-quality code**: Strong typing (mypy strict), comprehensive
   tests (e2e, integration, unit), good separation of concerns, clear
   documentation, backward compatibility bridges.
9. **gitignore-aware file matching**: Respects nested `.gitignore` files
   throughout the project tree, not just the root.
10. **Docker support**: Two well-optimized image variants, path mapping
    untuk cross-container path translation, volume persistence.

---

## 8. Weaknesses/Limitations

1. **No reranking**: Pure vector similarity search. No cross-encoder atau
   LLM-based reranking step untuk improve precision. Modern code search
   systems typically benefit dari a reranker.
2. **No hybrid search**: Only dense vector search. No BM25/sparse keyword
   search atau fusion (RRF). Untuk exact keyword atau identifier matching,
   users must fall back ke `ccc grep` atau external grep.
3. **No query expansion**: Single query embedding tanpa synonym expansion,
   query rewriting, atau multi-vector querying. A single poorly-phrased
   query dapat miss relevant results.
4. **No built-in evaluation**: No way untuk measure retrieval quality
   (recall@k, precision@k, MRR, nDCG) di user's own codebase. Relies
   entirely on external MTEB benchmarks yang mungkin tidak reflect
   real-world usage.
5. **SQLite/vec0 scalability**: Uses SQLite + sqlite-vec untuk vector
   storage. While embedded dan portable, ini mungkin tidak scale ke very
   large enterprise codebases (hundreds of thousands of files) sebaik
   dedicated vector databases (Qdrant, Milvus).
6. **LMDB map size limit**: Fixed at 4 GiB by default, requires manual env
   var adjustment untuk large codebases. Known friction point.
7. **No cross-repo search**: Indexes adalah per-project. No mechanism untuk
   search across multiple repositories simultaneously.
8. **Full scan untuk path filtering**: Saat path GLOB filters digunakan,
   system falls back ke full table scan computing distance di SQL, yang
   adalah O(n) rather than using the vector index. Bisa slow di large indexes.
9. **No streaming search results**: Search returns all results at once;
   no streaming/pagination beyond offset/limit.
10. **macOS Docker limitation**: Local embeddings adalah CPU-only di Docker
    on macOS (no Metal/MPS access). Users wanting GPU acceleration must
    install natively.
11. **Single MCP tool**: Only exposes `search`. No tools untuk listing
    indexed files, getting chunk context, atau exploring the index structure
    through MCP.
12. **No incremental/embedding cache invalidation on model change**:
    Switching embedding models requires `ccc reset && ccc index` (full rebuild).

---

## 9. Key Files Reference

| File | Importance | Purpose |
|------|------------|---------|
| `src/cocoindex_code/indexer.py` | ★★★ | Core indexing logic: file processing, chunking config, embedding, storage schema |
| `src/cocoindex_code/query.py` | ★★★ | Vector search: vec0 KNN, full-scan fallback, L2→cosine conversion |
| `src/cocoindex_code/server.py` | ★★★ | MCP server: search tool definition, FastMCP setup |
| `src/cocoindex_code/shared.py` | ★★★ | Embedder factory, context keys, CodeChunk schema, embedding validation |
| `src/cocoindex_code/litellm_embedder.py` | ★★☆ | LiteLLM embedder with pacing, rate-limit retries, batching |
| `src/cocoindex_code/chunking.py` | ★★☆ | Custom chunker API and registry |
| `src/cocoindex_code/daemon.py` | ★★☆ | Daemon process: project registry, request dispatch, lifecycle |
| `src/cocoindex_code/project.py` | ★★☆ | Project orchestration: wraps CocoIndex Environment + App |
| `src/cocoindex_code/cli.py` | ★★☆ | CLI commands (Typer) |
| `src/cocoindex_code/grep.py` | ★★☆ | Structural code search (`code_match`) |
| `src/cocoindex_code/settings.py` | ★★☆ | YAML settings schema, path helpers, gitignore loading |
| `src/cocoindex_code/embedder_defaults.py` | ★☆☆ | Curated params for known embedding models |
| `src/cocoindex_code/file_walk.py` | ★☆☆ | gitignore-aware file walking |
| `src/cocoindex_code/protocol.py` | ★☆☆ | IPC message types (msgspec structs) |
| `skills/ccc/SKILL.md` | ★☆☆ | Agent skill instructions |
| `docker/Dockerfile` | ★☆☆ | Container build (slim + full variants) |
| `EMBEDDINGS.md` | ★☆☆ | Embedding model selection guide |
| `scripts/find_best_models.py` | ★☆☆ | MTEB model discovery script |

---

## 10. Decision

ADR ini adalah **reference analysis document** (bukan architecture decision
baru). Tujuannya: menyimpan analisis teknis mendalam cocoindex-code sebagai
permanent record supaya:

1. Future development bisa refer ke sini untuk detail implementasi tanpa
   baca ulang source code Python.
2. Decision-making untuk improvement phases (ADR-0007 roadmap) punya
   evidence base yang lengkap.
3. Lesson learned terdokumentasi: apa yang perlu di-port, apa yang perlu
   di-improve, apa yang sudah di-exceed oleh bit-rag.

**Yang sudah di-adopt ke bit-rag** (via ADR-0004):
- AST-aware chunking concept (implemented with `smacker/go-tree-sitter`).

**Yang di-improve oleh bit-rag**:
- Fixed chunk size → per-model auto-derive.
- SQLite/vec0 → Qdrant (multi-tenant scalability).
- Single MCP tool → planned 5+ tools (ADR-0007 Phase 9).
- No eval → built-in recall@k + latency bench.
- No GPU → CDI acceleration with auto-switch (ADR-0006).
- No hot-swap → runtime embedder switch.

**Yang akan di-adopt di future phases**:
- Incremental indexing via file fingerprinting (ADR-0007 Phase 8).
- Hybrid search (already addressed by ADR-0008).
- MCP tool expansion (ADR-0007 Phase 9).
- Custom chunker registry (ADR-0007 — not yet phased).

---

## 11. References

- ADR-0004 — Hybrid Architecture (Best of cocoindex-code + enowx-rag)
- ADR-0007 — Gap Analysis & Improvement Roadmap (§2.1 brief cocoindex-code summary)
- ADR-0008 — Hybrid Search (Dense + Sparse + RRF Fusion)
- `references/cocoindex-code/` — full source tree (read-only reference)
- cocoindex-code PyPI: https://pypi.org/project/cocoindex-code/
- CocoIndex engine: https://github.com/coco-index/coco-index
- MTEB leaderboard: https://huggingface.co/spaces/mteb/leaderboard
