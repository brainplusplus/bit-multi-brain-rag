# Claude instructions for this project

This project has a `bit-rag` MCP server installed for semantic code search.

## RAG workflow

### Before making significant changes

1. Call `rag_retrieve_context` with the project name `PROJECT_NAME` and a
   natural-language description of the task.
2. Read the returned context. If it is empty or irrelevant, continue as normal.
3. If the context reveals existing patterns or constraints, factor them into
   your approach.

### After completing work

1. Summarize what you changed and why.
2. Call `rag_index_project` with the project name `PROJECT_NAME` to refresh
   the RAG index with your changes.
3. The index update runs in background (~30s). Subsequent searches will
   reflect the new code.

## Project name

Use project name: `PROJECT_NAME`

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
- Each project has its own isolated collection — never search the wrong project.
- Do not index more often than necessary (indexing re-embeds all files).
