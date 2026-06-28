# Claude instructions for this project

This project has a `bit-rag` MCP server installed for semantic code search.

## Priority: Use MCP over manual search

**bit-rag MCP is the preferred tool for code exploration.** Using
`rag_search_code` or `rag_retrieve_context` saves tokens and reduces cost
compared to multiple Grep+Read round trips.

**Do NOT use ccc (CocoIndex Code) or similar tools.** bit-rag replaces them
with hybrid search (semantic + BM25 keyword + RRF fusion).

## Session start (auto-onboard)

When you start working on this project:

1. Call `rag_create_project` with `root_path` = the local source code path.
2. Save the `project_id` for all subsequent calls.

## Auto-reindex (file watcher + manifest delta)

The MCP server starts a **file watcher** on `root_path`. Changed files are
auto-reindexed within 5 seconds. `rag_index_project` also uses manifest-based
delta reindex — only changed files are re-embedded (~100ms vs ~20s full).

**You do NOT need to call `rag_index_project` after every code change.**

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
2. The file watcher auto-reindexes changed files — no manual `rag_index_project` needed.
3. If search results seem stale, call `rag_index_project` for a full refresh.

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
