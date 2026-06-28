# Claude instructions for this project

This project has a `bit-rag` MCP server installed for semantic code search.

## Session start (auto-onboard)

When you start working on this project:

1. Call `rag_create_project` with `root_path` = the local source code path
   (where the MCP client runs — NOT the dashboard server path).
   - If already registered → returns existing `project_id`
   - If new → creates project, scans files locally, uploads chunks for embedding, returns `project_id`
2. Save the `project_id` for all subsequent calls.
3. Indexing runs during the call (files scanned locally + uploaded). No waiting needed.

**Do not ask the user to manually create or look up IDs.** `rag_create_project`
is idempotent by root_path.

## Project identity: use `project_id`

**Always use `project_id` (numeric) in tool calls.** It is guaranteed unique.
The `project` (name) parameter is a fallback only.

## RAG workflow

### Before making significant changes

1. Call `rag_retrieve_context` with `project_id` and a natural-language
   description of the task.
2. Read the returned context. If it is empty or irrelevant, continue as normal.
3. If the context reveals existing patterns or constraints, factor them into
   your approach.

### After completing work

1. Summarize what you changed and why.
2. Call `rag_index_project` with `project_id` to refresh the RAG index.
3. Indexing scans files locally + uploads — results available immediately.

## Search guidelines

bit-rag uses **hybrid search** (dense embeddings + BM25 keyword + RRF):

- **Semantic queries** work well: "database connection pooling logic"
- **Exact identifiers** work well: "parseConfig" or "RateLimiter"
- **Mixed queries** work well: "parseConfig error handling retry"
- Avoid overly broad queries: "config" or "function"

## When to retrieve context

- Before answering questions about existing code or architecture
- Before planning new features that overlap with existing systems
- Before debugging (the issue may have been solved before)
- When exploring an unfamiliar part of the codebase

## When to index

- After creating new files, endpoints, or major functions
- After refactoring or renaming modules
- At session start if the index might be stale
- When search results seem outdated

## Rules

- Cite `File:Lines` from search results in your replies so the user can verify.
- If search results are irrelevant, refine the query rather than increasing `limit`.
- Each project has its own isolated collection — use the correct `project_id`.
- Do not index more often than necessary (indexing re-embeds all changed files).
