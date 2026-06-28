> Multi-project semantic code search via **self-hosted voyage-4-nano embeddings**
> + Qdrant + a single Go binary. Exposes results to AI agents (Claude Desktop,
> Factory, OpenCode, Codex, Cursor, Continue, Windsurf) via a local **MCP
> server** that proxies to a remote **dashboard** over HTTPS.

[![Go Version](https://img.shields.io/badge/go-1.24%2B-blue)]()
[![License](https://img.shields.io/badge/license-MIT-green)]()

---

## TL;DR

```
AI Agent (Claude / Factory / Cursor / etc.)
  │  MCP stdio (JSON-RPC 2.0)
  ▼
bit-rag-mcp        ← LOCAL binary (scans files, chunks, uploads)
  │  HTTPS POST /api/v1/*
  ▼
bit-rag dashboard  ← REMOTE service (Docker / Easypanel), port 8081
  │  embed (GPU) + store (Qdrant)
  ▼
embedder (llama.cpp Q8) + Qdrant
```

**Architecture (refactored):**
- **MCP scans files locally** (tree-sitter AST chunking), sends pre-chunked
  docs to dashboard for embedding + storage. **No mounting needed.**
- Dashboard only does **embed (GPU) + store (Qdrant)**. Zero filesystem access.
- Single API key (`DASHBOARD_API_KEYS`) for all clients.
- Self-hosted embeddings (voyage-4-nano Q8 GGUF) — no per-call cost.
- **Multi-machine ready** — per-machine ID + per-project mutex for concurrent agents.

---

## Quick start

### Option A — Deploy your own server + install client (self-hosted)

```bash
git clone https://github.com/brainplusplus/bit-multi-brain-rag.git
cd bit-multi-brain-rag

# Windows (PowerShell)
.\scripts\setup.ps1

# Linux / macOS
./scripts/setup.sh
```

The setup wizard will:
1. Check prerequisites (Docker, Go)
2. Generate `.env` with a random API key
3. Deploy Qdrant + embedder + dashboard via Docker Compose
4. Build the MCP binary locally
5. Print ready-to-paste MCP config for your AI tool

### Option B — Use an existing dashboard (consumer)

If someone already deployed the dashboard for you:

```bash
git clone https://github.com/brainplusplus/bit-multi-brain-rag.git
cd bit-multi-brain-rag

# Windows
.\scripts\install-mcp.ps1
# Linux / macOS
./scripts/install-mcp.sh
```

Then add the MCP entry to your AI client config (see
[docs/INSTALL-MCP-LOCAL.md](./docs/INSTALL-MCP-LOCAL.md) for copy-paste
recipes for 7 clients).

---

## How it works

### Indexing (MCP → Dashboard)

```
MCP (local)                           Dashboard (Docker / VPS)
┌────────────────────────┐            ┌────────────────────┐
│ Walk project folder    │            │                    │
│ Read files locally     │            │                    │
│ Chunk (tree-sitter AST)│            │                    │
│ Compute SHA-256        │            │                    │
│                        │──HTTPS────→│ POST /api/v1/index/│
│ Send chunks in batches │  (JSON)    │      upload        │
│                        │            │ Embed (GPU)        │
│                        │←───────────│ Store (Qdrant)     │
└────────────────────────┘            └────────────────────┘
  scan + chunk LOCAL                    embed + store ONLY
  NO filesystem access on server        NO mounting needed
```

The MCP client walks the project folder **locally**, chunks files with
tree-sitter (25+ languages), and uploads pre-chunked documents to the
dashboard. The dashboard only embeds + stores — it never touches the
filesystem.

### Search (MCP → Dashboard)

```
User: "where is JWT validation?"
  │
  MCP: POST /api/v1/search
  │     {"project_id": 5, "query": "JWT validation", "limit": 5}
  │
  Dashboard: hybrid search (dense + sparse + RRF)
  │     → Qdrant Query API
  │
  ← Results with file, symbol, lines, score
```

### Multi-machine + multi-agent

- **Machine ID**: Each MCP client sends `X-Machine-ID` (platform-specific:
  Windows registry MachineGuid, Linux `/etc/machine-id`, macOS IOPlatformUUID).
  Same `root_path` on different machines = different projects.
- **Concurrent safety**: Per-project mutex on the dashboard serializes
  concurrent uploads (2 agents indexing the same project = no race condition).
- **Search is always parallel-safe** (read-only).

---

## Features

- **AST-aware chunking** via tree-sitter — 25+ languages, preserves function /
  class / method boundaries.
- **Self-hosted embeddings** — voyage-4-nano Q8 GGUF on llama.cpp, GPU-accelerated
  (RTX 3090: 52-236x faster than CPU).
- **Hybrid search** — dense embeddings (semantic) + BM25 sparse (keyword) +
  Reciprocal Rank Fusion.
- **Incremental indexing** — SHA-256 fingerprint per file, skip unchanged,
  delete stale points.
- **.gitignore respect** — nested .gitignore matcher with glob/directory/negation.
- **File watcher** — fsnotify-based, 5s debounce, auto re-index on change.
- **Multi-project isolation** — each project gets its own Qdrant collection.
- **Multi-machine** — machine ID + hostname per project, no cross-machine collision.
- **Dashboard UI** — HTMX + Echo, project CRUD, search debug, GPU management,
  model hot-swap, hybrid toggle, settings.
- **MCP server (stdio)** — 6 tools, JSON-RPC 2.0, project_id-based identity.
- **6 MCP tools**:
  - `rag_create_project` — register + index (idempotent by root_path + machine)
  - `rag_search_code` — semantic search, ranked results
  - `rag_retrieve_context` — search formatted as paste-ready context
  - `rag_index_project` — re-index after code changes
  - `rag_list_projects` — list all projects
  - `rag_project_status` — check registration + index status

---

## Architecture

See `docs/adr/` for full decision records:

| ADR | Topic |
|---|---|
| 0001 | Embedding model: voyage-4-nano 1024-dim (Q8 llama.cpp) |
| 0002 | Dashboard scope + multi-project index isolation |
| 0003 | API-key authentication |
| 0004 | Hybrid architecture (Go + tree-sitter + Qdrant + MCP) |
| 0005 | Background job manager for indexing |
| 0006 | GPU embedding acceleration (CDI, RTX 3090 benchmark) |
| 0007 | Gap analysis + improvement roadmap |
| 0008 | Hybrid search (dense + sparse + RRF) |

### Repo layout

```
bit-multi-brain-rag/
├── cmd/
│   ├── dashboard/        HTTP dashboard server
│   ├── mcp/              MCP stdio server (scans + chunks locally)
│   ├── embed-bench/      GPU vs CPU embedding benchmark
│   └── bench/            Recall + latency benchmark
├── pkg/
│   ├── chunker/          AST chunker (25+ languages, tree-sitter)
│   ├── indexer/          Chunk + embed + store pipeline
│   ├── rag/              Qdrant + embedder + BM25 hybrid search
│   ├── ragclient/        HTTP client to dashboard (used by MCP)
│   ├── mcp/              6 MCP tools (JSON-RPC over stdio)
│   ├── dashboard/        Echo HTTP handlers + UI
│   ├── store/            SQLite (projects, jobs, models, fingerprints)
│   ├── machineid/        Cross-platform machine ID (Win/Linux/Mac)
│   ├── watcher/          fsnotify file watcher
│   └── config/           .env loader
├── skills/               Skill files for AI agents
│   ├── factory/          Factory Droid skill + templates
│   └── opencode/         OpenCode skill
├── scripts/              Setup + install scripts
│   ├── setup.ps1         Windows full setup wizard
│   ├── setup.sh          Linux/macOS full setup wizard
│   ├── install-mcp.ps1   Windows MCP-only installer
│   └── install-mcp.sh    Linux/macOS MCP-only installer
├── docker-compose.yml    Dashboard + embedder
├── docker-compose.qdrant.yml  Qdrant (separate for independent lifecycle)
├── Dockerfile            Multi-stage Alpine build
└── docs/
    ├── adr/              Architecture Decision Records
    ├── DEPLOY-EASYPANEL.md
    └── INSTALL-MCP-LOCAL.md
```

---

## Privacy

- **During search**: Only `{project_id, query, limit}` leaves your machine.
- **During indexing**: Source code chunks are sent to the dashboard for
  embedding + storage. If the dashboard is remote (VPS/Easypanel), chunks
  traverse the network. The index lives server-side in Qdrant.
- **Machine ID**: Sent as HMAC-SHA256 hash (not reversible, deterministic
  per machine).
- Treat queries and indexed content as sensitive when working on
  confidential codebases.

---

## Build from source

Requires Go 1.24+ and a C toolchain (tree-sitter uses CGO).

```bash
go build -o bin/bit-rag-dashboard ./cmd/dashboard
go build -o bin/bit-rag-mcp       ./cmd/mcp
go build -o bin/bit-rag-bench     ./cmd/bench
```

---

## License

MIT — see `LICENSE`.

---

## Acknowledgements

- [smacker/go-tree-sitter](https://github.com/smacker/go-tree-sitter) — AST chunking
- [labstack/echo](https://github.com/labstack/echo) — HTTP router
- [qdrant/qdrant](https://github.com/qdrant/qdrant) — vector DB
- [ggerganov/llama.cpp](https://github.com/ggerganov/llama.cpp) — embedder runtime
- Reference: [enowx-rag](https://github.com/enowx/enowx-rag) (MCP local-scan architecture)
