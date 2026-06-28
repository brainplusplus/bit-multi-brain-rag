# Project agent instructions

This project uses `bit-rag` MCP server for semantic code search.

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

## Project identity: use `project_id`

**Always use `project_id` (numeric) in tool calls.** It is guaranteed unique.
The `project` (name) parameter is a fallback only.

## Before you start coding

1. Call `rag_retrieve_context` with `project_id` + the user's task/query.
2. Read the returned context. If it is empty or irrelevant, continue as normal.
3. If the context changes how you would approach the task, explain the relevant insights briefly.

## After you finish coding

1. Summarize what you changed and why.
2. Call `rag_index_project` with `project_id` to refresh the index.
3. Indexing scans files locally + uploads — results available immediately.

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
