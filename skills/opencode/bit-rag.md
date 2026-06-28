---
name: bit-rag
description: "Semantic code search via bit-multi-brain-rag MCP with hybrid retrieval (dense + BM25 + RRF). Use when finding code, understanding patterns, or locating implementations in indexed projects."
keywords: [rag, search, semantic, find code, bit-rag, locate, where is, index]
---

# bit-rag — Semantic Code Search (OpenCode)

You have access to the bit-multi-brain-rag MCP server with **6 tools** for
semantic code search. It uses hybrid retrieval: dense voyage-4-nano embeddings
+ BM25 sparse keyword matching + Reciprocal Rank Fusion.

## Available tools

| Tool | When to use |
|------|-------------|
| `rag_create_project` | **Call first.** Register project by root_path. Idempotent — returns `project_id`. |
| `rag_project_status` | Check if project is registered + indexed. Use `project_id`. |
| `rag_search_code` | Semantic search. Returns ranked chunks with scores. Use `project_id`. |
| `rag_retrieve_context` | Same as search, but formatted as paste-ready context. Use `project_id`. |
| `rag_index_project` | Re-index by scanning files locally + uploading to dashboard. Use `project_id`. |
| `rag_list_projects` | List all projects with ID + name + root_path. |

## Project identity: `project_id` is the key

**Always use `project_id` (numeric) in tool calls.** Project names are
cosmetic labels and may collide. The numeric ID is guaranteed unique.

The `project` (name) parameter is accepted as a **fallback** only.

## Session start (AUTO-ONBOARD)

When opening any project folder, **before accepting search queries**:

```
1. rag_create_project(root_path="<absolute-source-path>")
   → Returns project_id (existing if already registered, new otherwise)
   → Save project_id for all subsequent calls

2. Use project_id in all calls:
   rag_search_code(project_id=N, query="...")
   rag_retrieve_context(project_id=N, query="...")
```

**Do NOT ask the user to manually create or look up IDs.** `rag_create_project`
is idempotent — calling it on every session start is safe.

## Auto-reindex (file watcher)

After `rag_create_project` or `rag_index_project`, the MCP server watches
`root_path` for file changes. Changed files are auto-reindexed (delta — only
the changed files) within 5 seconds. **You do NOT need to manually reindex
after code changes.** Only call `rag_index_project` if search results seem
stale or after a large merge.

## Decision tree (after onboard)

```
User asks about code?
├── Knows the exact file path?              → Read / Edit (skip RAG)
├── Knows exact function/class name?        → Grep (skip RAG)
├── Describes behavior or pattern?          → rag_search_code ★
├── Needs context before writing code?      → rag_retrieve_context ★
├── Finished coding?                         → Auto-reindexed by watcher (do nothing)
└── Doesn't know project_id?                → rag_create_project(root_path) ★
```

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

## Tool call templates

```json
// Onboard (call first, idempotent)
{"name": "rag_create_project", "arguments": {"root_path": "/home/user/my-app"}}

// Search
{"name": "rag_search_code", "arguments": {"project_id": 1, "query": "JWT validation middleware", "limit": 5}}

// Retrieve context (pre-formatted)
{"name": "rag_retrieve_context", "arguments": {"project_id": 1, "query": "database connection pooling", "limit": 5}}

// Index project (after code changes)
{"name": "rag_index_project", "arguments": {"project_id": 1}}

// List projects
{"name": "rag_list_projects", "arguments": {}}
```

## Privacy

Only `{project_id/project, query, limit}` leaves the machine per search.
During indexing, source code chunks are sent to the dashboard for embedding +
storage. If the dashboard is remote, these chunks traverse the network.
The index lives server-side in Qdrant. Treat queries and indexed content as
sensitive when working on confidential codebases.

## Connection issues

The MCP boot performs a `/healthz` probe. If MCP fails to start:
- Check `DASHBOARD_URL` in the MCP env block
- Check `DASHBOARD_API_KEY` matches one in the server's `DASHBOARD_API_KEYS`
- Check the dashboard is deployed and reachable

See `docs/INSTALL-MCP-LOCAL.md` for setup instructions.
