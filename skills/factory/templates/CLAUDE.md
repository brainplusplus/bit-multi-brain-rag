# Claude instructions for this project

This project has a `bit-rag` MCP server installed for semantic code search.

## How to search: just pass root_path

**No setup needed. Just search:**

```
rag_search_code(root_path="D:/path/to/project", query="authentication middleware")
```

The tool auto-resolves the project by path. If not indexed yet, it auto-creates
and starts indexing in the background. No project_id needed.

## Auto-reindex (file watcher + manifest delta)

File watcher auto-reindexes changed files within 5 seconds. No manual reindex needed.

## RAG workflow

### Before making significant changes

1. Call `rag_retrieve_context` with `root_path` and a natural-language description.
2. Read the returned context. If empty or irrelevant, continue as normal.

### After completing work

The file watcher auto-reindexes. No manual action needed.

## Search guidelines

bit-rag uses **hybrid search** (dense + BM25 keyword + RRF):

- Semantic: "database connection pooling logic"
- Identifiers: "parseConfig" or "RateLimiter"
- Mixed: "parseConfig error handling retry"

## Rules

- Cite `File:Lines` from search results.
- Do NOT use ccc (CocoIndex Code). bit-rag replaces it.
- Grep fallback only when bit-rag returns irrelevant results.
