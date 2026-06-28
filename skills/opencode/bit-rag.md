---
name: bit-rag
description: "Semantic code search via bit-multi-brain-rag MCP with hybrid retrieval (dense + BM25 + RRF). Use when finding code, understanding patterns, or locating implementations in indexed projects."
keywords: [rag, search, semantic, find code, bit-rag, locate, where is, index]
---

# bit-rag — Semantic Code Search (OpenCode)

You have access to the bit-multi-brain-rag MCP server with **4 tools** for
semantic code search. It uses hybrid retrieval: dense voyage-4-nano embeddings
+ BM25 sparse keyword matching + Reciprocal Rank Fusion.

## Available tools

| Tool | When to use |
|------|-------------|
| `rag_search_code` | Semantic search across indexed code. Returns ranked chunks with scores. |
| `rag_retrieve_context` | Same as search, but formatted as paste-ready context with `[score]` prefixes. Use before coding. |
| `rag_index_project` | Trigger background re-index after significant code changes. |
| `rag_list_projects` | Discover available project names before searching. |

## Decision tree

```
User asks about code?
├── Knows the exact file path?              → Read / Edit (skip RAG)
├── Knows exact function/class name?        → Grep (skip RAG)
├── Describes behavior or pattern?          → rag_search_code ★
├── Needs context before writing code?      → rag_retrieve_context ★
├── Finished coding, index changed?         → rag_index_project ★
├── Doesn't know project name?              → rag_list_projects ★
└── Project not yet indexed?                → Ask user to index via dashboard
```

## Workflow

### Before coding

1. Call `rag_list_projects` if you don't know the project name.
2. Call `rag_retrieve_context` with the project name + task description.
3. Read returned context. Factor existing patterns into your plan.

### After coding

1. Call `rag_index_project` with the project name.
2. Wait ~30s for background indexing.
3. New searches will reflect your changes.

## Query writing rules

bit-rag uses **hybrid search** — both semantic and exact-identifier queries work:

- **Good (semantic):** "function that validates JWT tokens and returns user ID"
- **Good (identifier):** "parseConfig" (BM25 boosts exact matches)
- **Good (mixed):** "parseConfig error handling retry logic"
- **Bad (too broad):** "config" or "function"
- **Bad (too specific):** entire error message — extract the key phrase instead

### After getting results

1. Each result has `File`, `Symbol`, `Lines`, and a code fence.
2. Cite `File:Lines` in your reply so the user can verify locally.
3. If results are off-topic, **refine the query** — do NOT request a higher
   `limit`. Higher limit returns more noise, not more signal.
4. If the project name is wrong you'll get an empty result set, not an error.
   Call `rag_list_projects` to verify.

## Tool call templates

```json
// Search
{"name": "rag_search_code", "arguments": {"project": "my-project", "query": "JWT validation middleware", "limit": 5}}

// Retrieve context (pre-formatted)
{"name": "rag_retrieve_context", "arguments": {"project": "my-project", "query": "database connection pooling", "limit": 5}}

// Index project (after code changes)
{"name": "rag_index_project", "arguments": {"project": "my-project"}}

// List projects
{"name": "rag_list_projects", "arguments": {}}
```

## Privacy

Only `{project, query, limit}` leaves the machine per search. Source code
is sent only during indexing (and only if the dashboard is remote). The
index lives server-side in Qdrant. Treat queries as sensitive when working
on confidential codebases.

## Connection issues

The MCP boot performs a `/healthz` probe. If MCP fails to start:
- Check `DASHBOARD_URL` in the MCP env block
- Check `DASHBOARD_API_KEY` matches one in the server's `DASHBOARD_API_KEYS`
- Check the dashboard is deployed and reachable

See `docs/INSTALL-MCP-LOCAL.md` for setup instructions.
