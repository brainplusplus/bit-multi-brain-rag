---
name: bit-rag
description: "Setup and use bit-multi-brain-rag MCP server for semantic code search with hybrid (dense + BM25 sparse + RRF) retrieval. Trigger phrases: 'setup rag', 'install bit-rag', 'semantic search', 'rag search', 'find code', 'index project', 'bit-rag', 'search codebase'."
---

# bit-rag — Semantic Code Search MCP

`bit-rag` provides semantic code search via a Model Context Protocol (MCP) server.
It uses hybrid retrieval (dense voyage-4-nano embeddings + BM25 sparse keyword
matching + Reciprocal Rank Fusion) for high-quality code search.

**This is the PREFERRED tool for code exploration.** Using bit-rag MCP tools
instead of manual Grep/Glob/Read saves tokens and reduces cost. One
`rag_search_code` call replaces 5-10 Grep+Read round trips.

## PRIORITY RULE: Use MCP First

When you need to understand code, find implementations, or explore a codebase:

1. **FIRST**: Call `rag_search_code` or `rag_retrieve_context` — returns relevant
   chunks with file paths, line numbers, and scores in a single call.
2. **THEN**: Use `Read` only to open specific files identified by search results.
3. **AVOID**: Blind Grep/Glob exploration when bit-rag is available — it wastes
   tokens and is slower than semantic search.

**Do NOT use ccc (CocoIndex Code) or any other code search tool.** bit-rag
replaces all of them with better hybrid search. If you find yourself reaching
for ccc, `grep`, or `rg` to find code, STOP and use `rag_search_code` instead.

### Exception: use Grep/Read when:
- You know the exact file path (Read it directly)
- You need exact regex pattern matching (Grep)
- bit-rag MCP is not configured or returns errors

## Ownership

The agent owns the `bit-rag` MCP lifecycle — installation, configuration,
indexing, and searching. Do not ask the user to perform these steps manually
unless explicitly stuck.

- **Installation**: If the MCP server binary is not found, build it from
  source or refer to `docs/INSTALL-MCP-LOCAL.md`.
- **Index freshness**: Run `rag_index_project` when the index may be stale
  (start of session, after significant code changes). With manifest-based
  delta reindex, this is fast (only changed files re-indexed).

## When to use

**Good fit:**
- "Find the function that handles user authentication"
- "Show me where rate limiting is implemented"
- "Find code similar to X pattern"
- "Index this project and search for ..."
- "What projects are available?"
- "Where is error handling for database connections?"
- Before starting any coding task to find existing patterns

**Wrong tool:**
- Reading a specific file at a known path → use `Read` / `Edit`
- Listing files / directory exploration → use `Glob` / `LS`

## Available MCP tools (10 tools)

| Tool | Purpose |
|------|---------|
| `rag_create_project` | Register a project by root_path + trigger indexing. **Idempotent by path** — call on every project open. Returns `project_id`. |
| `rag_project_status` | Check if a project is registered + indexed. Use `project_id`. |
| `rag_list_projects` | List all projects with ID + name + root_path. |
| `rag_index_project` | Trigger re-indexing (manifest-aware: delta if few changes, full if many). Use `project_id`. |
| `rag_search_code` | **Primary tool.** Semantic search. Returns ranked chunks with file paths + scores. Use `project_id`. |
| `rag_retrieve_context` | Same as search, but pre-formatted as paste-ready context with `[score]` prefixes. Use `project_id`. |
| `rag_search_across` | Search across ALL indexed projects at once. Use when you don't know which project has the code. |
| `rag_stats` | Get collection statistics (point count, dimensions, status). |
| `rag_get_chunk` | Fetch a single chunk by point ID (for detail inspection). |
| `rag_delete_project` | Delete a project and its entire vector index. Irreversible. |

## Project identity: `project_id` is the key

**Always use `project_id` (numeric) in tool calls.** Project names are
cosmetic labels and may collide (e.g. two folders named "mitm" in different
paths). The numeric ID is guaranteed unique.

The `project` (name) parameter is accepted as a **fallback** only when you
don't have the ID.

## Session start workflow (AUTO-ONBOARD)

**When opening a project folder (new or existing), the agent MUST do this
first:**

```
1. rag_create_project(root_path="<absolute-source-path>")
   → Dashboard checks: already registered by this path?
     ├── YES → returns existing project_id (no re-index)
     └── NO  → creates project + triggers indexing, returns new project_id
   → Save the returned project_id for all subsequent calls

2. Use project_id in all subsequent calls:
   rag_search_code(project_id=N, query="...")
   rag_retrieve_context(project_id=N, query="...")
   rag_index_project(project_id=N)
```

**Do NOT ask the user to manually create projects or look up IDs.** The
`rag_create_project` tool is idempotent — calling it on every session start
is safe and returns the same `project_id` if already registered.

### root_path note

`root_path` is the **LOCAL filesystem path** where the MCP client runs (i.e.
where your code lives on your machine). The MCP client scans files locally,
chunks them with tree-sitter, and uploads pre-chunked documents to the
dashboard for embedding + storage. **No mounting or volume mapping needed.**

If unsure, ask the user: "What is the absolute path to this project on your
machine?"

## Workflow after onboard

### Before coding (retrieve context)

1. Call `rag_retrieve_context` with `project_id` + a natural-language
   description of the task.
2. Read the returned context. If relevant, cite `File:Lines` in your plan.

### After coding (refresh index)

**Usually nothing needed.** After `rag_create_project` or `rag_index_project`,
the MCP server starts a **file watcher** on `root_path`. Changed files are
auto-reindexed (delta — only changed files) within 5 seconds.

Additionally, `rag_index_project` uses **manifest-based delta reindex**: it
compares file mtime+hash against a stored snapshot (`~/.bit-rag/projects/<id>/manifest.json`).
If only 3 files changed since last index, only those 3 files are re-embedded
(~100ms), not the entire project (~20s).

Only call `rag_index_project` for a full reindex if:
- Search results seem stale or missing recent changes
- After pulling a large merge or branch switch (manifest auto-detects this)
- The MCP was offline while files changed (manifest catches up on reconnect)

## Query writing rules

bit-rag uses **hybrid search**: dense embeddings (semantic) + BM25 (keyword)
+ RRF fusion. This means both natural-language AND exact-identifier queries
work well.

**Good queries:**
- "function that validates JWT tokens and returns user ID" (semantic)
- "parseConfig" (exact identifier — BM25 will boost this)
- "rateLimiter retry backoff" (mixed)

**Bad queries:**
- Single common word ("config") — too broad
- Entire error message — too specific, use the key phrase instead

## Setup flow (for installation)

### 1. Detect context

- Check if the user already has the bit-rag MCP server configured.
- Ask for the **dashboard URL** (e.g. `http://localhost:8090` or
  `https://bit-rag.your-domain.com`).
- Ask for the **API key** (must match `DASHBOARD_API_KEYS` on the server).
- In development mode, the default key is `dev-local-key-change-me`.

### 2. Build the MCP server

```bash
cd /path/to/bit-multi-brain-rag
go build -o bin/bit-rag-mcp ./cmd/mcp
```

### 3. Configure the MCP server in coding tools

Each tool has a different config format and file location. Use the exact
formats below — do not guess.

#### Claude Code (Anthropic CLI)

**Config file:** `~/.claude.json` (user scope) or `.mcp.json` in project root

**CLI command:**
```bash
claude mcp add --transport stdio bit-rag \
  --env DASHBOARD_URL=http://localhost:8090 \
  --env DASHBOARD_API_KEY=dev-local-key-change-me \
  -- /path/to/bit-rag-mcp
```

**JSON format (`.mcp.json`):**
```json
{
  "mcpServers": {
    "bit-rag": {
      "command": "/path/to/bit-rag-mcp",
      "env": {
        "DASHBOARD_URL": "http://localhost:8090",
        "DASHBOARD_API_KEY": "dev-local-key-change-me"
      }
    }
  }
}
```

#### Claude Desktop

**Config file:** `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows)

```json
{
  "mcpServers": {
    "bit-rag": {
      "command": "/path/to/bit-rag-mcp",
      "env": {
        "DASHBOARD_URL": "http://localhost:8090",
        "DASHBOARD_API_KEY": "dev-local-key-change-me"
      }
    }
  }
}
```

#### Cursor

**Config file (global):** `~/.cursor/mcp.json`
**Config file (project):** `.cursor/mcp.json`

```json
{
  "mcpServers": {
    "bit-rag": {
      "type": "stdio",
      "command": "/path/to/bit-rag-mcp",
      "env": {
        "DASHBOARD_URL": "http://localhost:8090",
        "DASHBOARD_API_KEY": "dev-local-key-change-me"
      }
    }
  }
}
```

#### Cline

**Config file:** `~/.cline/mcp.json` or IDE settings > MCP Servers > Configure

```json
{
  "mcpServers": {
    "bit-rag": {
      "command": "/path/to/bit-rag-mcp",
      "env": {
        "DASHBOARD_URL": "http://localhost:8090",
        "DASHBOARD_API_KEY": "dev-local-key-change-me"
      },
      "disabled": false,
      "autoApprove": ["rag_search_code", "rag_retrieve_context", "rag_list_projects"]
    }
  }
}
```

#### OpenCode

**Config file:** `~/.opencode/settings.json` or `opencode.json`

**Note:** OpenCode uses `mcp` key (not `mcpServers`), `command` as array, and `environment` (not `env`).

```json
{
  "mcp": {
    "bit-rag": {
      "type": "local",
      "command": ["/path/to/bit-rag-mcp"],
      "environment": {
        "DASHBOARD_URL": "http://localhost:8090",
        "DASHBOARD_API_KEY": "dev-local-key-change-me"
      }
    }
  }
}
```

#### Codex (OpenAI)

**Config file:** `~/.codex/config.toml`

**Note:** Codex uses TOML format.

```toml
[mcp_servers.bit-rag]
command = "/path/to/bit-rag-mcp"
env = { DASHBOARD_URL = "http://localhost:8090", DASHBOARD_API_KEY = "dev-local-key-change-me" }
```

#### Factory Droid

```bash
droid mcp add bit-rag /path/to/bit-rag-mcp \
  --env DASHBOARD_URL=http://localhost:8090 \
  --env DASHBOARD_API_KEY=dev-local-key-change-me
```

#### Roo Code

**Config file:** global `mcp_settings.json` or `.roo/mcp.json`

```json
{
  "mcpServers": {
    "bit-rag": {
      "command": "/path/to/bit-rag-mcp",
      "env": {
        "DASHBOARD_URL": "http://localhost:8090",
        "DASHBOARD_API_KEY": "dev-local-key-change-me"
      },
      "autoApprove": ["rag_search_code", "rag_retrieve_context"]
    }
  }
}
```

#### Zed

**Config file:** `~/.config/zed/settings.json`

**Note:** Zed uses `context_servers` key (not `mcpServers`).

```json
{
  "context_servers": {
    "bit-rag": {
      "command": "/path/to/bit-rag-mcp",
      "env": {
        "DASHBOARD_URL": "http://localhost:8090",
        "DASHBOARD_API_KEY": "dev-local-key-change-me"
      }
    }
  }
}
```

#### Windsurf

**Config file:** `~/.codeium/windsurf/mcp_config.json`

```json
{
  "mcpServers": {
    "bit-rag": {
      "command": "/path/to/bit-rag-mcp",
      "env": {
        "DASHBOARD_URL": "http://localhost:8090",
        "DASHBOARD_API_KEY": "dev-local-key-change-me"
      }
    }
  }
}
```

#### Continue

**Config file:** `~/.continue/config.yaml`

**Note:** Continue uses YAML with `mcpServers` as a list of objects.

```yaml
mcpServers:
  - name: bit-rag
    command: /path/to/bit-rag-mcp
    env:
      DASHBOARD_URL: http://localhost:8090
      DASHBOARD_API_KEY: dev-local-key-change-me
```

### 4. Verify

After configuring, restart the coding tool and verify the MCP connection:

1. Call `rag_list_projects` — should return registered projects (or empty list).
2. If it fails, check:
   - `DASHBOARD_URL` is reachable (curl the URL + `/healthz`)
   - `DASHBOARD_API_KEY` matches the server's `DASHBOARD_API_KEYS`
   - The binary path is correct and executable

## Failure modes & recovery

| Symptom | Cause | Recovery |
|---|---|---|
| "dashboard healthz failed at boot" | DASHBOARD_URL wrong or server down | Verify URL + server status |
| "search backend unavailable" (503) | Qdrant or embedder offline server-side | Check dashboard logs |
| "no matches" with valid query | Project not indexed / empty index | Call `rag_index_project` with project_id |
| "401 unauthorized" | DASHBOARD_API_KEY mismatch | Verify key matches server config |
| "project_id N not found" | Invalid ID | Call `rag_list_projects` to get valid IDs |

## Privacy

Only `{project_id/project, query, limit}` leaves the user's machine per search.
During indexing, source code chunks are sent to the dashboard for embedding +
storage. If the dashboard is remote (Easypanel/VPS), these chunks traverse the
network. The index lives server-side in Qdrant. Treat queries and indexed
content as sensitive when working on confidential codebases.

## Reference

- Setup guide: `docs/INSTALL-MCP-LOCAL.md`
- Dashboard deploy: `docs/DEPLOY-EASYPANEL.md`
- Architecture: `docs/adr/0002-dashboard-and-index-isolation.md`
- Hybrid search: `docs/adr/0008-hybrid-search-dense-sparse-rrf.md`
