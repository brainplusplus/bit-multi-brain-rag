# ADR-0004: Hybrid Architecture — Best of cocoindex-code + enowx-rag

- **Status**: Accepted
- **Date**: 2026-06-27
- **Project**: bit-multi-brain-rag
- **Decision owner**: Engineering
- **Reviewers**: _(pending)_
- **Supersedes**: —
- **Related**: ADR-0001 (Embedding Model), ADR-0002 (Dashboard), ADR-0003 (Auth)

---

## 1. Context

bit-multi-brain-rag must combine the strengths of two existing systems:

| System | Language | Strength | Weakness |
| --- | --- | --- | --- |
| cocoindex-code (Python) | Python | AST-aware chunker, recall 8/8 verified | No HTTP API, LanceDB embedded, not multi-tenant |
| enowx-rag (Go) | Go | MCP tool, Qdrant, multi-project, single binary | Naive chunker (not AST-aware), no dashboard |

### 1.1 The "combine strengths" trap

A naive "combine both codebases" approach leads to:
- Two languages (Python + Go) → two toolchains, two deploys, two debug paths.
- Two vector stores (LanceDB + Qdrant) → sync nightmare.
- Blurred boundaries (who handles indexing? Python or Go?).

This ADR rejects that path. "Combine strengths" ≠ "combine codebases".
"Combine strengths" = **one new architecture that adopts the best ideas of both,
implemented in a single language.**

### 1.2 Language choice

**Go.** Single binary deploy. Consistent with `enowx-rag` (existing Go RAG) and the
`D:\golang\` workspace. No Python runtime in production.

cocoindex-code (Python) becomes a **conceptual reference only** — its AST chunker
algorithm, benchmark dataset (8 Python files + 8 queries), and prompt profiles are
ported to Go. The Python library itself is not invoked.

### 1.3 Chunker choice

Two Go tree-sitter options were evaluated:

| Option | Role | Ready-made chunker? | Effort | Production-readiness |
| --- | --- | --- | --- | --- |
| `gomantics/chunkx` | Chunker (CAST algorithm) | Yes (30+ langs) | 0.5 day | Low (v0.0.3, 11 stars, 1 maintainer) |
| `smacker/go-tree-sitter` | Parser (tree-sitter bindings) | No (parser only) | 2 days | High (560 stars, mature) |

**Decision: `smacker/go-tree-sitter` + write a custom chunker (~150-200 lines Go).**

Rationale: the chunker is a critical foundation component (recall depends on it).
For a long-lived system, production-readiness and full control outweigh saving
1.5 days. `chunkx` is too young (v0.0.3, single maintainer) to be a critical
dependency. The custom chunker is not complex — parse + query + split per node.

### 1.4 MCP scope: extensible by design, code-only in phase 1

The MCP server exposes tools to AI assistants. While phase 1 only implements
code RAG, the architecture must not lock out future domains (documentation,
tasks). This is achieved via a **tool registry pattern** and a `domain` field in
the index key — not by building multiple tools upfront.

---

## 2. Decision

### 2.1 Architecture overview

```
bit-multi-brain-rag/                        (100% Go, single binary)
│
├── INDEXING (strength from cocoindex-code)
│   └── AST-aware chunker via smacker/go-tree-sitter (Go native)
│       └── Parse Python/JS/Go/TS -> split at function/class boundary
│
├── STORAGE & QUERY (strength from enowx-rag)
│   ├── Qdrant (multi-project collection)
│   └── Provider interface (multi-backend: llama_q8, future st_fp16, gpu)
│
├── EMBEDDING (self-hosted, ADR-0001)
│   └── llama.cpp Q8 HTTP (voyage.bitsolution.my.id, pooling mean)
│
├── INTERFACE (strength from enowx-rag + new)
│   ├── MCP tool (for AI assistants, stdio)
│   └── Dashboard HTTP (Go + HTMX, API key auth per ADR-0003)
│
└── DATA LOCALITY
    ├── Source code: LOCAL (read by MCP via filesystem, never uploaded)
    └── Vectors: CLOUD (Qdrant on VPS, accessed via HTTPS)
```

### 2.2 Project structure

```
bit-multi-brain-rag/
├── cmd/
│   ├── mcp-server/            # MCP tool (stdio, for AI assistants)
│   └── dashboard/             # HTTP dashboard (HTMX + API)
├── pkg/
│   ├── rag/                   # Provider interface + Qdrant + llama embedder
│   │   ├── provider.go        # interface (from enowx-rag, adapted)
│   │   ├── qdrant.go          # Qdrant backend (from enowx-rag)
│   │   └── llama.go           # llama.cpp Q8 HTTP embedder (new)
│   ├── chunker/               # AST-aware chunker (smacker/go-tree-sitter)
│   ├── indexer/               # walk + chunk + embed + store (delta sync)
│   ├── mcp/                   # MCP tool registry + CodeRAGTool
│   ├── dashboard/             # HTTP handlers + auth middleware
│   └── bench/                 # benchmark runner (recall + latency)
├── docs/adr/                  # ADR-0001 .. 0004
├── web/                       # HTMX templates (sidebar + result viewer)
└── go.mod
```

### 2.3 Index isolation key

Collection name in Qdrant = 5-tuple:

```
{project_id}_{domain}_{model}_{dim}_{backend}
```

Concrete examples:

```
project_acme_code_voyage_1024_llama_q8       # phase 1 (code RAG)
project_acme_doc_voyage_1024_llama_q8        # future (doc RAG)
project_acme_task_voyage_1024_llama_q8       # future (task RAG)
project_acme_code_arctic_768_st_fp16         # alt model+backend
```

**Why `domain` is mandatory:**
- Code uses AST chunker; docs use text chunker; tasks use structured chunker.
  Different pipelines cannot share a collection.
- Mixing domains in one collection degrades recall (search "login" returns a
  doc when the user wanted code).
- Isolation per domain = higher recall per domain.

### 2.4 MCP tool registry (extensibility principle)

```go
type Tool interface {
    Name() string
    Description() string
    Handle(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}

type Registry struct {
    tools map[string]Tool
}
```

- **Phase 1**: register `CodeRAGTool` only (search code via Qdrant + llama.cpp).
- **Future**: `DocRAGTool`, `TaskTool` register without architectural refactor.

This is **not scope creep** — it is an architecture that does not lock out the
future. Only one tool is built now.

### 2.5 Data locality

- **Source code stays LOCAL** on the developer's machine. The MCP tool reads it
  via filesystem access (granted by the AI assistant host). Source code is never
  uploaded to the cloud.
- **Only vectors + metadata go to CLOUD** (Qdrant on VPS). This is the minimal
  data needed for retrieval.
- **Compliance benefit**: source code (potentially sensitive/customer IP) never
  leaves the laptop. Only anonymized vectors (not reversible to source) are stored
  cloud-side.

### 2.6 Chunker implementation

Custom chunker on `smacker/go-tree-sitter`:

```go
// Pseudocode
parser := sitter.NewParser()
parser.SetLanguage(language.ByExt(ext))
tree := parser.Parse(nil, source)

// Query function/class/method nodes (per-language query strings)
query := language.QueryFor(ext)  // e.g. "(function_definition) @func"
qc := sitter.NewQueryCursor()
qc.Exec(q, tree.RootNode())

// Each match = one chunk (function/class body)
for {
    m, ok := qc.NextMatch()
    if !ok { break }
    for _, c := range m.Captures {
        chunks = append(chunks, source[c.Node.StartByte():c.Node.EndByte()])
    }
}
// Fallback: files without functions (config, __init__.py) -> naive split
```

- Per-language query mapping (~1 hour per language): Python, Go, JS, TS in phase 1.
- Fallback for non-AST files (config, docs): naive character split.
- **Recall must be re-verified** against the 8-file + 8-query benchmark dataset
  (ported from cocoindex-code) after implementation.

---

## 3. Consequences

### 3.1 Positive

- **Single binary deploy.** No Python runtime, no subprocess, no LanceDB. One Go
  binary serves MCP + dashboard + indexer.
- **Code-aware chunking retained.** AST-aware chunker (ported concept from
  cocoindex-code) preserves recall quality without the Python dependency.
- **Multi-project + multi-domain native.** Qdrant collection naming supports
  project and domain isolation from day 1.
- **MCP extensibility.** Tool registry allows adding doc/task RAG later without
  refactor.
- **Compliance-friendly.** Source code never leaves the laptop; only vectors go
  to cloud.
- **Consistent ecosystem.** Go, same as enowx-rag and the `D:\golang\` workspace.

### 3.2 Negative

- **Chunker must be written and verified (~2 days).** Not a ready-made library.
  Mitigated: tree-sitter queries are short (~150-200 lines total), and recall
  benchmark dataset already exists for verification.
- **smacker/go-tree-sitter last updated Feb 2024.** Core API is stable, but
  grammar updates may need manual fetching. Acceptable for phase 1.
- **Q8 recall still unverified (carried from ADR-0001).** Must re-benchmark
  after llama.cpp Q8 server pooling fix is deployed.
- **No code viewer in dashboard.** Dashboard shows search results (file + score +
  metadata snippet), not full source code (which stays local). Users view source
  in their local IDE.
- **Index key is verbose.** 5-tuple collection names are long but explicit.
  Acceptable trade-off for isolation clarity.

### 3.3 Risks

| Risk | Likelihood | Impact | Mitigation |
| --- | --- | --- | --- |
| Custom chunker recall drops vs cocoindex-code | Medium | High | Re-run 8-file benchmark; tune queries if <7/8 |
| smacker/go-tree-sitter abandoned | Low | Medium | Vendor grammar files; or migrate to official `tree-sitter/go-tree-sitter` |
| Q8 recall drops vs FP16 | Medium | High | ADR-0001 fallback to ST FP16 in-proc for dev; verify before go-live |
| MCP multi-domain scope creep | Medium | Medium | Tool registry isolates; only build CodeRAGTool in phase 1 |

---

## 4. Implementation phases

| Phase | Scope | Output |
| --- | --- | --- |
| 1 | Project skeleton Go + config + auth middleware + llama.go embedder | Runnable binary with health + embed endpoint |
| 2 | Dashboard UI (HTMX: sidebar + result viewer) | Browser-accessible dashboard |
| 3 | Code-aware chunker (smacker + custom chunker) | AST chunking for Python/Go/JS/TS |
| 4 | Search API + MCP tool (query Qdrant + llama.cpp embed) | End-to-end search via MCP + HTTP |
| 5 | Benchmark runner per backend | Recall + latency verification |

Prerequisite (blocks go-live): verify Q8 recall (deploy Dockerfile pooling fix +
re-run benchmark).

---

## 5. References

- ADR-0001: Embedding Model Selection and Index Isolation (voyage_nano_1024, llama.cpp Q8)
- ADR-0002: Dashboard and Index Isolation (multi-project RAG explorer)
- ADR-0003: Dashboard Authentication via API Key
- `smacker/go-tree-sitter`: https://github.com/smacker/go-tree-sitter
- cocoindex-code benchmark: `poc-cocoindex/BENCHMARK.md`
- enowx-rag MCP server: `enowx-rag/mcp-server/`
