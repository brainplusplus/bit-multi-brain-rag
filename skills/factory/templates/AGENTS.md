# Project agent instructions

This project uses `bit-rag` MCP server for semantic code search.

## Before you start coding

1. Call `rag_retrieve_context` with the user's task/query and the project name `PROJECT_NAME`.
2. Read the returned context. If it is empty or irrelevant, continue as normal.
3. If the context changes how you would approach the task, explain the relevant insights briefly.

## After you finish coding

1. Summarize what you changed and why.
2. Call `rag_index_project` with the project name `PROJECT_NAME` to refresh the index.
3. Wait ~30s for background indexing, then new search results will reflect your changes.

## Project name

Use project name: `PROJECT_NAME`

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
- Each project has its own collection. Do not search the wrong project.
