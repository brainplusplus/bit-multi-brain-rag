# Project agent instructions

This project uses `bit-rag` MCP server for semantic code search.

## Priority: Use MCP over manual search

**bit-rag MCP is the preferred tool for code exploration.** Using
`rag_search_code` or `rag_retrieve_context` saves tokens and reduces cost
compared to multiple Grep+Read round trips.

**Do NOT use ccc (CocoIndex Code) or similar tools.** bit-rag replaces them
with hybrid search (semantic + BM25 keyword + RRF fusion). One MCP call
returns ranked results with file paths and line numbers.

Use Grep/Read only when:
- You know the exact file path (Read directly)
- You need regex pattern matching (Grep)
- bit-rag returns no results

## Session start (auto-onboard)

When you start working on this project, **before accepting search queries**:

1. Call `rag_create_project` with `root_path` = the local source code path
   (where the MCP client runs — NOT the dashboard server path).
   - If already registered → returns existing `project_id`
   - If new → creates project, scans files locally, uploads chunks for embedding, returns `project_id`
2. Save the `project_id` for all subsequent calls.
3. Indexing runs during the call (files scanned locally + uploaded). No waiting needed.

**Do not ask the user to manually create or look up IDs.** `rag_create_project`
is idempotent by root_path.

## Auto-reindex (file watcher + manifest delta)

After `rag_create_project` or `rag_index_project`, the MCP server starts a
**file watcher** on `root_path`. When files change (create/edit/delete), it
auto-reindexes only the changed files within 5 seconds.

`rag_index_project` also uses **manifest-based delta reindex**: compares file
mtime+hash against `~/.bit-rag/projects/<id>/manifest.json`. Only changed files
are re-embedded (~100ms for 1 file vs ~20s full reindex).

**You do NOT need to call `rag_index_project` after every code change.** The
watcher handles it. Only call `rag_index_project` if:
- Search results seem stale or missing recent changes.
- The MCP was offline while files changed (manifest catches up on reconnect).

## Project identity: use `project_id`

**Always use `project_id` (numeric) in tool calls.** It is guaranteed unique.
The `project` (name) parameter is a fallback only.

## Before you start coding

1. Call `rag_retrieve_context` with `project_id` + the user's task/query.
2. Read the returned context. If it is empty or irrelevant, continue as normal.
3. If the context changes how you would approach the task, explain the relevant insights briefly.

## After you finish coding

1. Summarize what you changed and why.
2. The file watcher auto-reindexes changed files — no manual `rag_index_project` needed.
3. If search results seem stale, call `rag_index_project` for a full refresh.

## When to search (retrieve)

- Before answering questions about existing code or architecture.
- Before planning new features that may overlap with existing systems.
- Before debugging issues that may have been seen before.
- When the user asks "where is X implemented" or "find code that does Y".

## When to index

- After significant code changes (new files, refactors, renamed modules).
- At the start of a session if the index may be stale.
- After the user reports that search results are outdated.

## Query tips

bit-rag uses **hybrid search** (semantic + keyword + RRF fusion). Both
natural-language and exact-identifier queries work:

- Good: "function that validates JWT tokens and returns user ID"
- Good: "parseConfig" (exact name match)
- Bad: "config" (too broad)

## Rules

- Always cite `File:Lines` from search results in your replies.
- If results are off-topic, refine the query — do not increase `limit`.
- Each project has its own collection. Use the correct `project_id`.
