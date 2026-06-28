---
name: bit-rag
description: "Setup and use bit-multi-brain-rag MCP server for semantic code search with hybrid (dense + BM25 sparse + RRF) retrieval. Trigger phrases: 'setup rag', 'install bit-rag', 'semantic search', 'rag search', 'find code', 'index project', 'bit-rag'."
---

# bit-rag — Semantic Code Search MCP

`bit-rag` provides semantic code search via a Model Context Protocol (MCP) server.
It proxies to a remote bit-multi-brain-rag dashboard over HTTPS, using hybrid
retrieval (dense voyage-4-nano embeddings + BM25 sparse keyword matching +
Reciprocal Rank Fusion) for high-quality code search.

## Ownership

The agent owns the `bit-rag` MCP lifecycle — installation, configuration,
indexing, and searching. Do not ask the user to perform these steps manually
unless explicitly stuck.

- **Installation**: If the MCP server binary is not found, build it from
  source or refer to `docs/INSTALL-MCP-LOCAL.md`.
- **Index freshness**: Run `rag_index_project` when the index may be stale
  (start of session, after significant code changes).

## When to use

✅ **Good fit:**
- "Find the function that handles user authentication"
- "Show me where rate limiting is implemented"
- "Find code similar to X pattern"
- "Index this project and search for ..."
- "What projects are available?"

❌ **Wrong tool:**
- Reading a specific file at a known path → use `Read` / `Edit`
- Exact-string search (`func MyFn`) → use `Grep`
- Listing files / directory exploration → use `Glob` / `LS`

## Available MCP tools

bit-rag exposes **6 tools** via MCP:

| Tool | Purpose |
|------|---------|
| `rag_create_project` | Register a project by root_path + trigger indexing. **Idempotent by path** — call on every project open. Returns `project_id`. |
| `rag_project_status` | Check if a project is registered + indexed. Use `project_id`. |
| `rag_list_projects` | List all projects with ID + name + root_path. |
| `rag_search_code` | Semantic search. Returns ranked chunks with file paths + scores. Use `project_id`. |
| `rag_retrieve_context` | Same as search, but pre-formatted as paste-ready context with `[score]` prefixes. Use `project_id`. |
| `rag_index_project` | Trigger background re-indexing. Use `project_id`. |

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
auto-reindexed (delta — only changed files, not full walk) within 5 seconds.

Only call `rag_index_project` for a full reindex if:
- Search results seem stale or missing recent changes
- After pulling a large merge or branch switch

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
