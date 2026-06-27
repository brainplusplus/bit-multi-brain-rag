---
name: bit-rag
description: Semantic code search across indexed projects via bit-multi-brain-rag MCP. Use this skill when the user asks to find code, understand patterns, or locate implementations in a remote-indexed project.
triggers:
  - "find code"
  - "search code"
  - "find implementation"
  - "find function"
  - "find class"
  - "where is"
  - "rag search"
  - "bit-rag"
  - "semantic search"
tools:
  - rag_search_code  # provided by bit-rag MCP server
---

# Skill: bit-rag — Semantic Code Search

This skill enables semantic search across source code that has been pre-indexed
into a remote bit-multi-brain-rag deployment. Use it when you need to **locate**
or **understand** code without having the full repository locally.

## When to use

✅ **Good fit:**
- "Find the function that handles user authentication"
- "Show me where rate limiting is implemented"
- "Find code similar to X pattern"
- "Locate the database migration logic"
- Cross-file pattern discovery in large codebases

❌ **Wrong tool:**
- Reading a specific file at a known path → use `Read` / `Edit`
- Exact-string search (`func MyFn`) → use `Grep`
- Listing files / directory exploration → use `Glob` / `LS`
- Querying code that is NOT indexed in bit-rag → won't work

If unsure whether a project is indexed, ask the user or list projects via the
dashboard UI first.

## Tool: `rag_search_code`

Inputs:
- `project` (string, required): exact project name as registered in the dashboard
- `query` (string, required): natural-language description of what you want to find
- `limit` (integer, optional, default 5): max results

Returns: ranked code chunks with file path, symbol name, line range, and the
chunk content as a fenced code block.

## How to formulate good queries

bit-rag uses voyage-4-nano embeddings (1024-dim, cosine similarity). The
embedding model is trained on code — natural-language descriptions of behavior
work BETTER than literal token matching.

**Good queries (intent-driven):**
- "function that validates JWT tokens and returns user ID"
- "middleware that throttles requests per IP address"
- "code that reads config from environment variables"

**Bad queries (token-matching — use Grep instead):**
- "func ValidateJWT"
- "import jwt"
- "rateLimiter"

## Workflow

1. **Confirm the project name** — list available projects if needed (via
   dashboard UI). Project names are case-sensitive and used as collection keys.
2. **Run the search** with a clear intent-driven query.
3. **Inspect results** — each result includes file path + line range. If you
   need the full file, the user must provide local access (e.g. `Read` if they
   have the repo cloned).
4. **Refine** — if results are off-topic, try a more specific query or narrower
   intent. The model rewards specificity.
5. **Cite results** — when reporting findings, always include file path and
   line numbers from the result metadata so the user can verify.

## Privacy note

Only the query text and project name leave the user's machine. Source code is
NEVER sent — it lives on the dashboard's internal Qdrant. Results contain
pre-indexed chunks; if a chunk is sensitive, that is a pre-indexing concern,
not a per-query one.

## Failure modes & recovery

| Symptom | Cause | Recovery |
|---|---|---|
| "dashboard healthz failed at boot" | DASHBOARD_URL wrong or server down | Ask user to verify deployment status |
| "search backend unavailable" (503) | Qdrant or embedder offline server-side | Ask user to check Easypanel logs |
| "no matches" with valid query | Project not indexed / wrong name / index empty | Confirm project name; ask user to run dashboard's Index action |
| "401 unauthorized" | DASHBOARD_API_KEY wrong | Ask user to verify key matches DASHBOARD_API_KEYS on server |
