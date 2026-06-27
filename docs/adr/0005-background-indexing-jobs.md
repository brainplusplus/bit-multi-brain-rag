# ADR-0005: Background indexing jobs (async API + HTMX polling)

| Field    | Value                            |
| -------- | -------------------------------- |
| Status   | Accepted                         |
| Date     | 2026-06-27                       |
| Decision | Move indexing off the request path; HTMX polling for UI feedback |
| Supersedes | Synchronous /api/v1/index handler (Phase 1) |

## Context

Phase 1 (ADR-0002) used a **synchronous** indexing handler:
`POST /api/v1/index` blocked the HTTP connection while the indexer walked
the source tree, chunked, embedded, and upserted. For our self-test
(`pkg/`, 20 Go files, ~190 chunks) this took ~34 seconds.

Two problems surfaced during local Docker testing:

1. **HTTP timeouts.** Go's `http.Server.WriteTimeout` defaults short
   (we ran with 30s). The 34s+ indexing run breached it and clients
   received `curl: (52) empty reply from server` — no JSON body, no
   error message, no progress. Raising `WriteTimeout` to 10 minutes was
   a band-aid: real codebases would still blow past it (Easypanel reverse
   proxies typically cap at 5 min anyway), and the UI gave no progress.

2. **No feedback in the UI.** The HTMX "Re-index" button submitted a
   form that simply hung for the entire duration. There was no spinner,
   no progress, no way to cancel, no way to know if the server was alive
   or hung. Users reported "dashboard gak nongol" — they assumed
   nothing was happening.

The user also asked: what happens when two clients hit Re-index, or a
single client hits the button then refreshes the page mid-flight?
Synchronous handlers make this a UX cliff: the second click waits for
the first, the refreshed page shows a fresh "click Re-index" panel,
multi-tab tabs see different states.

We considered four alternatives before picking the chosen path.

### Option A — Raise timeouts and ship

Keep the handler synchronous, raise `WriteTimeout` to 30 minutes, hope
no Easypanel proxy in front trims connections.

Rejected: still no progress feedback; still vulnerable to NAT/proxy
idle-timeout middleboxes; concurrent requests still serialize per
project _and_ across projects; multi-tab refresh still broken.

### Option B — SSE (Server-Sent Events) streaming

`GET /api/v1/index/stream?project=X` keeps the connection open and
emits `event: progress` lines until completion.

Rejected: significant new complexity (reconnect logic, event IDs,
last-event-id resumption). HTMX has `hx-sse` but it's still extension
territory. The simpler "poll every 2s" gives us 95% of the UX with 5%
of the code. We can revisit SSE if real-time-per-token feedback is
ever needed.

### Option C — External job queue (Redis / NATS / asynq)

Use a real queue: enqueue → worker pool → ack on completion.

Rejected for Phase 1: we're a single-container deploy (Easypanel one
service). Adding Redis doubles infra surface for one workload. We can
graduate to a real queue when we deploy multiple replicas (the in-
memory map limits us to one node), and the manager interface is small
enough that swapping the backend is straightforward.

### Option D — Move to React/Vite frontend

User explicitly raised this as an option when the HTMX UI looked
empty.

Rejected: the HTMX UI is fine — the missing piece was the **status
endpoint**, not the rendering layer. A polling div solves it. Moving
to React would mean two deploys (Go API + Vite static bundle), more
build chain, lost server-render simplicity. We keep HTMX and revisit
only if the dashboard grows into a multi-page app.

## Decision

Introduce a **background job manager** (`pkg/jobs.Manager`) that:

- Owns an in-memory `map[project]*Job` for live progress.
- Persists each job lifecycle event to a new `index_jobs` SQLite table
  (insert on enqueue, update on terminal transition) — 2 writes per
  job total, regardless of duration.
- Spawns one goroutine per active job via `Indexer.IndexProjectWithProgress`,
  passing a `context.CancelFunc` so we can stop work.
- Locks per-project (`map[project]*Job`): if a job exists for project X,
  a second Enqueue(X) returns the same `*Job` and `ErrAlreadyRunning`.
  Different projects run concurrently.
- On startup, calls `MarkRunningAsInterrupted()` to flip any orphaned
  `queued`/`running` rows (left by a previous container instance) to
  `interrupted` so the UI doesn't show a stale spinner.

The HTTP surface changes:

| Endpoint                              | Before          | After                              |
| ------------------------------------- | --------------- | ---------------------------------- |
| `POST /api/v1/index`                  | 200 + stats     | **202 + `{job_id, status}`**       |
| `GET  /api/v1/index/status?project=X` | (did not exist) | 200 + live `Job` JSON              |
| `POST /api/v1/index/cancel`           | (did not exist) | 200 + `{status: cancel signalled}` |
| `POST /ui/index`                      | block + stats   | **202 + polling partial**          |
| `GET  /ui/index/status?project=X`     | (did not exist) | live status partial (self-polls)   |
| `POST /ui/index/cancel`               | (did not exist) | updated status partial             |

The HTMX partial returned by `/ui/index` and `/ui/index/status` is the
key piece. It looks like:

```html
<div id='index-stats' class='index-stats'
     hx-get='/ui/index/status?project=X'
     hx-trigger='every 2s'
     hx-swap='outerHTML'>
  <h3>Indexing in progress…</h3>
  <p>File 4 / 20: chunker.go</p>
  <progress />
  <button hx-post='/ui/index/cancel'>Cancel</button>
</div>
```

When the job reaches a terminal status, the server returns the same
`<div id='index-stats'>` **without `hx-trigger`** — HTMX automatically
stops polling because the new DOM has no trigger. No JS needed.

### Refresh and multi-tab semantics

Because the panel state lives **on the server**, page refresh during
indexing re-renders the live polling partial automatically:

- `GET /ui/projects/:id` calls `jobs.GetLatest(project)`.
- If active → returns the running partial with polling trigger.
- If terminal → returns the completed/failed/cancelled summary.
- If never indexed → returns "No index run yet" placeholder.

Two browser tabs viewing the same project both poll the same endpoint
and see the same state. No race conditions, no client-side state
divergence.

### Cancellation semantics

`Cancel(project)` calls the goroutine's `context.CancelFunc`. The
indexer respects `ctx.Done()` between files and between embed batches.
Partial work is preserved: the points already upserted stay in Qdrant
(UUID v5 IDs keep them deterministic, so the next index run is
idempotent and resumes those chunks in place rather than duplicating).
The cancelled job's `Error` field reads `"cancelled by user"`.

## Consequences

### Positive

- **No more "empty reply from server".** All handlers return in <100ms.
- **Live progress in UI.** Users see "File 4/20: chunker.go", a progress
  bar, embed/index counters refreshed every 2s.
- **Refresh-safe.** Mid-indexing browser reload picks right back up.
- **Multi-tab consistent.** Server is single source of truth.
- **Idempotent submits.** Two quick clicks return the same job.
- **Cancel support.** Bad/long indexing runs can be stopped without
  process kill.
- **Audit trail.** Every run is in SQLite — when, by which job ID,
  how long, how many errors.
- **Startup recovery.** Crashed/restarted container doesn't leave a
  ghost spinner; orphan rows become "interrupted".
- **Concurrent projects.** Indexing project A doesn't block project B.

### Negative

- **Two writes per job to SQLite.** Negligible (~5µs each on modernc/sqlite).
- **In-memory map limits us to single-replica deploy.** Acceptable for
  Phase 1; ADR-0006 will revisit when we need horizontal scaling.
- **Goroutine leak risk.** Mitigated by `ctx.Done()` checks and the
  per-project lock (you can't spawn unbounded goroutines for the same
  project).
- **Polling overhead.** At 2s and ~600 byte responses, an active page
  costs ~300 bytes/s. Acceptable for a developer tool.

### Neutral

- The `/api/v1/index` response shape changed from `IndexStats` to
  `{job_id, status, ...}`. The legacy MCP CLI doesn't call this endpoint,
  so no breaking change for MCP consumers. Documented in DEPLOY-EASYPANEL.md.
- Tests for the indexer pipeline still target `IndexProject(...)`
  unchanged (it now delegates to `IndexProjectWithProgress` with a nil
  callback).

## Future work

- **Real job queue** when we deploy multi-replica (Redis-backed manager
  swapping in behind the same `*Manager` interface).
- **Live job history view** in the dashboard: list the last 20 runs per
  project, with click-through to error details.
- **Webhook on completion** for MCP integration: `POST /jobs/{id}/done`
  hooks so Factory/OpenCode can re-trigger when an external indexer
  has fresh content.
- **Resumable indexing**: today a cancelled run leaves partial points;
  a future run skips already-indexed (file, line) pairs via a
  fingerprint check instead of re-upserting.
