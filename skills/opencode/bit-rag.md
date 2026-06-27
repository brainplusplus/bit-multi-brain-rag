---
name: bit-rag
description: Use the bit-multi-brain-rag MCP server to perform semantic search across remote-indexed source code. Activate when the user asks to find, locate, or understand code in a project that has been indexed.
keywords: [rag, search, semantic, find code, bit-rag, locate, where is]
---

# bit-rag — Semantic Code Search (OpenCode skill)

You have access to the `rag_search_code` tool provided by the local `bit-rag`
MCP server. It proxies semantic search to a remote bit-multi-brain-rag
dashboard over HTTPS.

## Decision tree

```
User asks about code?
├── Knows the exact file path?              → Read / Edit (skip this skill)
├── Knows exact function/class name?        → Grep (skip this skill)
├── Describes behavior or pattern?          → rag_search_code ★
└── Project not yet indexed in bit-rag?     → Ask user to index via dashboard
```

## Tool call template

```json
{
  "name": "rag_search_code",
  "arguments": {
    "project": "<exact-project-name>",
    "query": "<intent-driven natural language>",
    "limit": 5
  }
}
```

## Query writing rules

- **Describe behavior**, not tokens. "function that retries with exponential
  backoff" beats "retry function with backoff".
- **Be specific about the domain**. "JWT verification middleware in HTTP
  router" beats "JWT code".
- **One concept per query**. If you need two unrelated things, run two queries.

## After getting results

1. Each result has `File`, `Symbol`, `Lines`, and a code fence.
2. Cite `File:Lines` in your reply so the user can verify locally.
3. If results are off-topic, REFINE the query — do NOT request a higher
   `limit` to "see more". Higher limit returns more noise, not more signal.
4. If the project name is wrong you'll get an empty result set, not an error.
   Double-check the name with the user.

## Privacy

Only `{project, query, limit}` leaves the machine. No source code is uploaded
per-query — the index was built once on the server side. Treat queries as
sensitive when working on confidential codebases.

## Connection issues

The MCP boot performs a `/healthz` probe. If MCP fails to start:
- Check `DASHBOARD_URL` in the MCP env block
- Check `DASHBOARD_API_KEY` matches one in the server's `DASHBOARD_API_KEYS`
- Check the dashboard is actually deployed and reachable (curl the URL)

See `docs/INSTALL-MCP-LOCAL.md` for setup instructions.
