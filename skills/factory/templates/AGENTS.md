# Project agent instructions

This project uses `bit-rag` MCP server for semantic code search.

## Priority: Use MCP over manual search

**bit-rag MCP is the preferred tool for code exploration.** Using
`rag_search_code` or `rag_retrieve_context` saves tokens and reduces cost
compared to multiple Grep+Read round trips.

**Do NOT use ccc (CocoIndex Code) or similar tools.** bit-rag replaces them
with hybrid search (semantic + BM25 keyword + RRF fusion).

Use Grep/Read only when bit-rag returns no results or you need regex.

## How to search: just pass root_path

**No setup needed. Just search:**

```
rag_search_code(root_path="D:/path/to/project", query="authentication middleware")
```

The tool auto-resolves the project by path. If not indexed yet, it auto-creates
and starts indexing in the background. You don't need project_id or rag_create_project.

## Auto-reindex (file watcher + manifest delta)

File watcher auto-reindexes changed files within 5 seconds. Manifest delta
reindex: only changed files are re-embedded (~100ms vs ~20s full).

**You do NOT need to call `rag_index_project` after every code change.**

## Before you start coding

1. Call `rag_retrieve_context` with `root_path` + the user's task/query.
2. Read the returned context. If it is empty or irrelevant, continue as normal.

## Query tips

bit-rag uses **hybrid search** (semantic + keyword + RRF fusion):

- Good: "function that validates JWT tokens and returns user ID"
- Good: "parseConfig" (exact identifier match)
- Bad: "config" (too broad)

## Rules

- Always cite `File:Lines` from search results in your replies.
- If results are off-topic, refine the query — do not increase `limit`.
