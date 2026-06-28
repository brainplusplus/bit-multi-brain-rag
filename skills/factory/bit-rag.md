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

bit-rag exposes **4 tools** via MCP:

| Tool | Purpose |
|------|---------|
| `rag_search_code` | Semantic search across indexed source code. Returns ranked chunks with file paths, symbols, scores. |
| `rag_retrieve_context` | Same as search, but returns results as a pre-formatted context string with `[score]` prefixes. Use before writing code. |
| `rag_index_project` | Trigger background re-indexing for a project. Use after significant code changes. |
| `rag_list_projects` | List all registered projects. Use to discover available project names. |

## Workflow

### Before coding (retrieve context)

1. Call `rag_list_projects` to see available projects.
2. Call `rag_retrieve_context` with the project name + a natural-language
   description of the task.
3. Read the returned context. If relevant, cite `File:Lines` in your plan.

### After coding (refresh index)

1. Call `rag_index_project` with the project name.
2. Wait ~30s (background job), then search will reflect new content.

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

### 5. Generate AGENTS.md / CLAUDE.md (optional)

To enable auto-use RAG workflow in a target project, create or merge
`AGENTS.md` and `CLAUDE.md` using the templates in
`skills/factory/templates/`. These tell agents to always retrieve context
before coding and index after changes.

## Failure modes & recovery

| Symptom | Cause | Recovery |
|---|---|---|
| "dashboard healthz failed at boot" | DASHBOARD_URL wrong or server down | Verify URL + server status |
| "search backend unavailable" (503) | Qdrant or embedder offline server-side | Check dashboard logs |
| "no matches" with valid query | Project not indexed / wrong name / empty index | Call `rag_index_project`, then retry |
| "401 unauthorized" | DASHBOARD_API_KEY mismatch | Verify key matches server config |
| Empty project list | No projects registered | Create a project via dashboard UI |

## Privacy

Only `{project, query, limit}` leaves the user's machine per search. Source
code is sent only during indexing (and only if the dashboard is remote).
The index lives server-side in Qdrant. Treat queries as sensitive when
working on confidential codebases.

## Reference

- Setup guide: `docs/INSTALL-MCP-LOCAL.md`
- Dashboard deploy: `docs/DEPLOY-EASYPANEL.md`
- Architecture: `docs/adr/0002-dashboard-and-index-isolation.md`
- Hybrid search: `docs/adr/0008-hybrid-search-dense-sparse-rrf.md`
