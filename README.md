# bit-multi-brain-rag

> Multi-project semantic code search via **self-hosted voyage-4-nano embeddings**
> + Qdrant + a single Go binary. Exposes results to AI agents (Claude Desktop,
> Factory, OpenCode, Codex, Cursor, Continue, Windsurf) via a local **MCP
> server** that proxies to a remote **dashboard** over HTTPS.

[![Go Version](https://img.shields.io/badge/go-1.24%2B-blue)]()
[![License](https://img.shields.io/badge/license-MIT-green)]()

---

## TL;DR

```
AI Agent
  │  MCP stdio (JSON-RPC 2.0)
  ▼
bit-rag-mcp        ← LOCAL binary (this repo, ~12 MB)
  │  HTTPS POST /api/v1/search
  ▼
bit-rag dashboard  ← REMOTE service (Easypanel / Docker), port 8081
  │  internal Docker network
  ▼
embedder (llama.cpp Q8) + Qdrant
```

**Why this architecture:**
- One public endpoint (the dashboard). Qdrant + embedder stay private.
- Single API key (`DASHBOARD_API_KEYS`) for all clients.
- **Source code never leaves your machine.** Only the query text + project
  name travel over the wire.
- Self-hosted embeddings (voyage-4-nano Q8 GGUF) — no per-call cost.

---

## Quick start

### Option A — Use the public deployment (consumer)

If someone has already deployed the dashboard for you:

```bash
# Clone for the installer scripts and skill files
git clone https://github.com/brainplusplus/bit-multi-brain-rag.git
cd bit-multi-brain-rag

# Linux/macOS
./scripts/install-mcp.sh
# Windows (PowerShell)
.\scripts\install-mcp.ps1
```

Then add the MCP entry to your AI client config — see
[**docs/INSTALL-MCP-LOCAL.md**](./docs/INSTALL-MCP-LOCAL.md) for full
copy-paste recipes for 7 clients.

### Option B — Deploy your own dashboard (operator)

```bash
git clone https://github.com/brainplusplus/bit-multi-brain-rag.git
cd bit-multi-brain-rag

# Deploy to Easypanel (recommended) or any Docker host
# Full guide: docs/DEPLOY-EASYPANEL.md
cd infra/easypanel
cp .env.example .env
# Edit .env: set DASHBOARD_API_KEYS, EMBEDDER_TOKEN, etc.
docker compose -f compose.split.yaml up -d
```

See [**docs/DEPLOY-EASYPANEL.md**](./docs/DEPLOY-EASYPANEL.md) for the full
deployment recipe (3 compose variants, Cloudflare/Caddy/Traefik, healthchecks,
volume strategy).

---

## Features

- **AST-aware chunking** via `smacker/go-tree-sitter` — preserves function /
  class / method boundaries instead of dumb line-split.
- **Self-hosted embeddings** — voyage-4-nano Q8 GGUF on llama.cpp HTTP server
  with `--pooling mean`. ~50–200 ms/query. No vendor lock-in.
- **Multi-project isolation** — each project gets its own Qdrant collection
  keyed by `{project}_{domain}_{model}_{dim}_{backend}`.
- **Dashboard UI** — HTMX + Echo, project CRUD, index trigger, search debug,
  bench harness, API-key management.
- **MCP server (stdio)** — JSON-RPC 2.0, registers `rag_search_code` tool.
  Fail-fast healthz at boot.
- **Auth** — bearer-token middleware, multiple keys via comma-separated
  `DASHBOARD_API_KEYS`.
- **Bench harness** — `cmd/bench` runs recall@k + latency on a labeled set.

---

## Architecture

See `docs/adr/` for full decision records:

| ADR | Topic |
|---|---|
| **0001** | Embedding model: voyage-4-nano 1024-dim (Q8 llama.cpp server) |
| **0002** | Dashboard scope + multi-project index isolation |
| **0003** | API-key authentication (global key, per-project scope is TODO) |
| **0004** | Hybrid architecture (Go + tree-sitter + Qdrant + MCP/dashboard) |

### Repo layout

```
bit-multi-brain-rag/
├── cmd/
│   ├── dashboard/         HTTP dashboard server (Echo, HTMX UI, REST /api/v1/*)
│   ├── mcp/               MCP stdio server — proxies to dashboard
│   └── bench/             Recall + latency benchmark runner
├── pkg/
│   ├── config/            .env loader (dashboard config)
│   ├── auth/              API-key middleware
│   ├── rag/               Vector store + embedding provider interfaces
│   │   ├── provider.go      Provider + EmbeddingClient interfaces
│   │   ├── llama.go         llama.cpp Q8 HTTP embedder (OpenAI-compatible)
│   │   └── qdrant.go        Qdrant vector store impl
│   ├── ragclient/         HTTP client to /api/v1/search (used by MCP)
│   ├── indexer/           Scan + chunk + embed + store pipeline
│   ├── chunker/           AST-aware chunker (smacker/go-tree-sitter)
│   ├── mcp/               MCP tool registry + stdio JSON-RPC loop
│   ├── dashboard/         Echo handlers (projects, search, bench, UI)
│   ├── store/             SQLite metadata (projects, jobs, api keys)
│   └── bench/             Benchmark runner
├── web/
│   ├── templates/         HTMX templates
│   └── static/            CSS/JS
├── infra/
│   └── easypanel/         Dockerfiles + 3 compose variants + .env.example
├── scripts/
│   ├── install-mcp.ps1    Windows installer (build + install + test)
│   └── install-mcp.sh     Linux/macOS installer
├── skills/
│   ├── factory/bit-rag.md OpenCode/Factory skill file (auto-injectable)
│   └── opencode/bit-rag.md
└── docs/
    ├── adr/               Architecture Decision Records
    ├── DEPLOY-EASYPANEL.md
    └── INSTALL-MCP-LOCAL.md
```

---

## Build from source

Requires Go 1.24+ and a C toolchain (tree-sitter uses CGO).

```bash
git clone https://github.com/brainplusplus/bit-multi-brain-rag.git
cd bit-multi-brain-rag

# Build all 3 binaries
go build -o bin/bit-rag-dashboard ./cmd/dashboard
go build -o bin/bit-rag-mcp       ./cmd/mcp
go build -o bin/bit-rag-bench     ./cmd/bench
```

Module path: `github.com/brainplusplus/bit-multi-brain-rag`.

---

## Run locally (dev)

```bash
cp .env.example .env
# Edit: DASHBOARD_API_KEYS, EMBEDDING_ENDPOINT, QDRANT_URL, etc.

# Start Qdrant + embedder via Docker (or use existing ones)
docker run -p 6333:6333 qdrant/qdrant
# (set up llama.cpp embedder per docs/adr/0001)

# Run dashboard
./bin/bit-rag-dashboard

# Open http://localhost:8081
```

### Smoke test the API

```bash
# Health (public)
curl http://localhost:8081/healthz

# Search (auth required)
curl -X POST http://localhost:8081/api/v1/search \
  -H "Authorization: Bearer your-key" \
  -H "Content-Type: application/json" \
  -d '{"project":"demo","query":"JWT validation","limit":5}'
```

---

## Index isolation key

Each Qdrant collection is keyed by:

```
{project_id}_{domain}_{model}_{dim}_{backend}
```

- `domain`: `code` | `doc` | `task` (phase 1: `code` only)
- `model`: `voyage_nano_1024`
- `dim`: `1024`
- `backend`: `llama_q8` | `st_fp16` (phase 1: `llama_q8`)

This allows side-by-side comparison of different embedding models on the same
data without index collision.

---

## Status

| Component | Status |
|---|---|
| Dashboard HTTP API + UI | ✅ Implemented |
| MCP server (stdio) | ✅ Implemented + refactored to HTTP-proxy mode |
| Indexer pipeline (scan→chunk→embed→Qdrant) | ✅ Implemented |
| Bench harness | ✅ Implemented |
| Docker / Easypanel compose | ✅ 3 variants (split, all-in-one, behind-proxy) |
| Installer scripts (Windows + Linux/macOS) | ✅ Implemented |
| Skill files (Factory + OpenCode) | ✅ Implemented |
| Q8 recall verification | ⏳ Blocked on Easypanel deploy |
| Per-project API-key scope | 📋 TODO (ADR-0003 revision) |

---

## Privacy & security

1. **Code stays local.** The indexer reads code from the dashboard host; the
   MCP only sends queries.
2. **HTTPS only** — terminate TLS at Caddy/Traefik in front of the dashboard.
3. **Bearer auth** — `DASHBOARD_API_KEYS` is comma-separated for per-developer
   rotation.
4. **Don't paste secrets into queries** — query text is logged server-side
   for debugging.

See `docs/INSTALL-MCP-LOCAL.md` § Security for the full checklist.

---

## License

MIT — see `LICENSE`.

---

## Acknowledgements

- [smacker/go-tree-sitter](https://github.com/smacker/go-tree-sitter) — AST chunking
- [labstack/echo](https://github.com/labstack/echo) — HTTP router
- [qdrant/qdrant](https://github.com/qdrant/qdrant) — vector DB
- [ggerganov/llama.cpp](https://github.com/ggerganov/llama.cpp) — embedder runtime
- [voyage-code-3](https://blog.voyageai.com/2024/05/30/voyage-code-3/) — embedding model
- Reference projects: `cocoindex-code` (chunker design), `enowx-rag` (MCP+dashboard design)
