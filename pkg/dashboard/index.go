package dashboard

import (
	"errors"
	"fmt"
	"html/template"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/indexer"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/jobs"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/store"
)

// indexReq is the body for POST /api/v1/index.
type indexReq struct {
	Project  string `json:"project"`   // project name (must exist in store)
	RootPath string `json:"root_path"` // optional override; defaults to stored root
}

// indexAPI enqueues an indexing run and returns 202 + job descriptor as JSON.
//
// Asynchronous semantics (ADR-0005): unlike the legacy synchronous variant,
// this returns immediately even for very large repos. Callers poll
// GET /api/v1/index/status?project=X (or by job_id) for progress.
func (s *Server) indexAPI(c echo.Context) error {
	var req indexReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(400, map[string]string{"error": "invalid request body"})
	}
	if req.Project == "" {
		return c.JSON(400, map[string]string{"error": "project is required"})
	}
	rootPath, err := s.resolveRootPath(c, req.Project, req.RootPath)
	if err != nil {
		return c.JSON(404, map[string]string{"error": err.Error()})
	}
	if s.indexer == nil || s.jobs == nil {
		return c.JSON(503, map[string]string{"error": "indexer unavailable (embedder/qdrant offline)"})
	}
	pid := s.resolveProjectID(c, req.Project)
	job, err := s.jobs.Enqueue(req.Project, rootPath, pid)
	if err != nil && !errors.Is(err, jobs.ErrAlreadyRunning) {
		return c.JSON(500, map[string]string{"error": err.Error()})
	}
	// 202 = accepted, work in progress. Same response whether brand-new or
	// already running — caller idempotent.
	return c.JSON(202, jobToJSON(job))
}

// indexStatusAPI returns the current status of the latest job for a project.
// GET /api/v1/index/status?project=X
func (s *Server) indexStatusAPI(c echo.Context) error {
	project := c.QueryParam("project")
	if project == "" {
		return c.JSON(400, map[string]string{"error": "project query param is required"})
	}
	if s.jobs == nil {
		return c.JSON(503, map[string]string{"error": "jobs manager unavailable"})
	}
	job, err := s.jobs.GetLatest(c.Request().Context(), project)
	if err != nil {
		return c.JSON(500, map[string]string{"error": err.Error()})
	}
	if job == nil {
		return c.JSON(404, map[string]string{"error": "no jobs for project"})
	}
	return c.JSON(200, jobToJSON(job))
}

// indexCancelAPI cancels the active indexing job for a project.
// POST /api/v1/index/cancel  body: {"project":"name"}
func (s *Server) indexCancelAPI(c echo.Context) error {
	var req indexReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(400, map[string]string{"error": "invalid request body"})
	}
	if req.Project == "" {
		return c.JSON(400, map[string]string{"error": "project is required"})
	}
	if s.jobs == nil {
		return c.JSON(503, map[string]string{"error": "jobs manager unavailable"})
	}
	if !s.jobs.Cancel(req.Project) {
		return c.JSON(409, map[string]string{"error": "no active job for project"})
	}
	return c.JSON(200, map[string]string{"status": "cancel signalled"})
}

// indexUploadReq is the body for POST /api/v1/index/upload.
// The MCP client sends pre-chunked documents (already read + chunked locally).
// The dashboard only does embed + store — no filesystem access needed.
type indexUploadReq struct {
	Project string `json:"project"` // project name
	Docs    []struct {
		ID      string            `json:"id"`
		Content string            `json:"content"`
		Meta    map[string]string `json:"meta"`
	} `json:"docs"`
}

// indexUploadAPI accepts pre-chunked documents from the MCP client,
// embeds them, and stores in Qdrant. This is the "remote index" path:
// MCP reads + chunks locally, sends chunks here for embed + store.
// No filesystem access needed on the dashboard side (no mounting).
func (s *Server) indexUploadAPI(c echo.Context) error {
	var req indexUploadReq
	if err := c.Bind(&req); err != nil {
		return c.JSON(400, map[string]string{"error": "invalid request body"})
	}
	if req.Project == "" {
		return c.JSON(400, map[string]string{"error": "project is required"})
	}
	if len(req.Docs) == 0 {
		return c.JSON(400, map[string]string{"error": "docs is empty"})
	}
	if s.indexer == nil || s.embed == nil {
		return c.JSON(503, map[string]string{"error": "indexer/embedder unavailable"})
	}

	ctx := c.Request().Context()

	// Per-project lock: serialize concurrent uploads to the same project.
	// If 2 MCP clients index the same project simultaneously, the second
	// waits for the first to finish (prevents Qdrant upsert races).
	s.indexMu.lockFor(req.Project).Lock()
	defer s.indexMu.lockFor(req.Project).Unlock()

	// Build collection key from active model config.
	key := s.collectionKeyFor(req.Project)
	if err := s.rag.CreateCollection(ctx, key); err != nil {
		return c.JSON(500, map[string]string{"error": fmt.Sprintf("create collection: %v", err)})
	}

	// Convert to rag.Documents.
	docs := make([]rag.Document, len(req.Docs))
	for i, d := range req.Docs {
		docs[i] = rag.Document{
			ID:      d.ID,
			Content: d.Content,
			Meta:    d.Meta,
		}
	}

	// Split oversized (in case MCP sent chunks that exceed embedder limit).
	pid := s.resolveProjectID(c, req.Project)
	docs = indexer.SplitOversizedPublic(req.Project, docs, s.indexer.MaxTokensPerChunk)

	// Embed batch.
	texts := make([]string, len(docs))
	for i, d := range docs {
		texts[i] = d.Content
	}
	vecs, err := s.embed.Embed(ctx, texts)
	if err != nil {
		return c.JSON(500, map[string]string{"error": fmt.Sprintf("embed: %v", err)})
	}

	// Store (hybrid or dense-only).
	if s.indexer.HybridEnabled {
		if qc, ok := s.rag.(*rag.QdrantClient); ok {
			sparseVecs := make([]*rag.SparseVector, len(docs))
			for i, d := range docs {
				sym := d.Meta["symbol"]
				nm := d.Meta["name"]
				f := d.Meta["source_file"]
				sparseVecs[i] = s.bm25.BuildDocSparse(sym, nm, f, d.Content)
			}
			if err := qc.IndexWithSparse(ctx, key, docs, vecs, sparseVecs); err != nil {
				// Fallback to dense-only.
				if err := s.rag.Index(ctx, key, docs, vecs); err != nil {
					return c.JSON(500, map[string]string{"error": fmt.Sprintf("index: %v", err)})
				}
			}
		} else {
			if err := s.rag.Index(ctx, key, docs, vecs); err != nil {
				return c.JSON(500, map[string]string{"error": fmt.Sprintf("index: %v", err)})
			}
		}
	} else {
		if err := s.rag.Index(ctx, key, docs, vecs); err != nil {
			return c.JSON(500, map[string]string{"error": fmt.Sprintf("index: %v", err)})
		}
	}

	_ = pid // fingerprints tracked by MCP client (local)

	return c.JSON(200, map[string]any{
		"status":   "indexed",
		"project":  req.Project,
		"embedded": len(vecs),
		"indexed":  len(docs),
	})
}

// uiRunIndex enqueues an indexing job via the HTMX UI and returns the live
// status partial. The partial itself self-polls until terminal — no further
// JS is needed.
func (s *Server) uiRunIndex(c echo.Context) error {
	project := c.FormValue("project")
	if project == "" {
		return c.HTML(400, "<p class='error'>Project is required</p>")
	}
	rootPath, err := s.resolveRootPath(c, project, "")
	if err != nil {
		return c.HTML(404, fmt.Sprintf("<p class='error'>%s</p>", template.HTMLEscapeString(err.Error())))
	}
	if s.indexer == nil || s.jobs == nil {
		return c.HTML(503, "<p class='error'>Indexer unavailable (Qdrant/embedder offline).</p>")
	}
	pid := s.resolveProjectID(c, project)
	job, err := s.jobs.Enqueue(project, rootPath, pid)
	if err != nil && !errors.Is(err, jobs.ErrAlreadyRunning) {
		return c.HTML(500, fmt.Sprintf("<p class='error'>%s</p>", template.HTMLEscapeString(err.Error())))
	}
	return c.HTML(202, s.renderJobStatus(job))
}

// uiJobStatus is the HTMX polling endpoint. Returns the latest status for the
// given project. While the job is non-terminal, the partial includes an
// `hx-trigger="every 2s"` attribute so the browser keeps polling.
// GET /ui/index/status?project=X
func (s *Server) uiJobStatus(c echo.Context) error {
	project := c.QueryParam("project")
	if project == "" {
		return c.HTML(200, emptyIndexStats())
	}
	if s.jobs == nil {
		return c.HTML(200, emptyIndexStats())
	}
	job, err := s.jobs.GetLatest(c.Request().Context(), project)
	if err != nil {
		return c.HTML(500, fmt.Sprintf("<p class='error'>%s</p>", template.HTMLEscapeString(err.Error())))
	}
	if job == nil {
		return c.HTML(200, emptyIndexStats())
	}
	return c.HTML(200, s.renderJobStatus(job))
}

// uiCancelIndex cancels the active job and returns the updated status partial.
func (s *Server) uiCancelIndex(c echo.Context) error {
	project := c.FormValue("project")
	if project == "" {
		return c.HTML(400, "<p class='error'>Project is required</p>")
	}
	if s.jobs == nil || !s.jobs.Cancel(project) {
		return c.HTML(409, "<p class='error small'>No active job to cancel.</p>")
	}
	// Return the latest snapshot — the goroutine may not have written its
	// terminal state yet, so this could still show "running"; that's fine,
	// the next 2s poll will pick up "cancelled".
	job, _ := s.jobs.GetLatest(c.Request().Context(), project)
	if job == nil {
		return c.HTML(200, emptyIndexStats())
	}
	return c.HTML(200, s.renderJobStatus(job))
}

// resolveRootPath looks up the project's root path from the store, or uses
// the override if provided.
func (s *Server) resolveRootPath(c echo.Context, project, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	p, err := s.resolveProject(c, project)
	if err != nil {
		return "", err
	}
	return p.RootPath, nil
}

// resolveProject looks up a project by name from the SQLite store.
func (s *Server) resolveProject(c echo.Context, project string) (*store.Project, error) {
	projects, err := s.store.ListProjects(c.Request().Context())
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	for _, p := range projects {
		if p.Name == project {
			return &p, nil
		}
	}
	return nil, fmt.Errorf("project %q not found", project)
}

// resolveProjectID returns the numeric project ID for fingerprint tracking.
func (s *Server) resolveProjectID(c echo.Context, project string) int64 {
	p, err := s.resolveProject(c, project)
	if err != nil {
		return 0
	}
	return p.ID
}

// jobToJSON projects a Job into a JSON-friendly map. Used by both /api/v1
// endpoints. We avoid exposing internal fields (cancel func).
func jobToJSON(j *jobs.Job) map[string]any {
	if j == nil {
		return nil
	}
	out := map[string]any{
		"id":            j.ID,
		"project":       j.Project,
		"status":        string(j.Status),
		"started_at":    j.StartedAt,
		"files_total":   j.FilesTotal,
		"files_done":    j.FilesDone,
		"chunks_done":   j.ChunksDone,
		"embedded_done": j.EmbeddedDone,
		"indexed_done":  j.IndexedDone,
		"current_file":  j.CurrentFile,
	}
	if !j.FinishedAt.IsZero() {
		out["finished_at"] = j.FinishedAt
	}
	if j.Error != "" {
		out["error"] = j.Error
	}
	if j.Stats != nil {
		out["duration_ms"] = j.Stats.Duration.Milliseconds()
		out["errors"] = j.Stats.Errors
	}
	return out
}

// emptyIndexStats is the placeholder rendered when a project has never been
// indexed (no job rows). It is still inside the #index-stats div so HTMX
// outerHTML swaps work cleanly.
func emptyIndexStats() string {
	return "<div id='index-stats' class='small muted'>No index run yet — click <strong>Re-index project</strong> to start.</div>"
}

// renderJobStatus is the HTML partial that drives the UI's live indexing
// panel. It contains its own polling trigger while the job is non-terminal,
// so HTMX automatically refreshes the panel every 2s without any JS.
//
// The wrapping <div id='index-stats'> is always present so handlers can
// target it with hx-swap=outerHTML uniformly.
func (s *Server) renderJobStatus(j *jobs.Job) string {
	var sb strings.Builder
	sb.WriteString("<div id='index-stats' class='index-stats'")
	if !j.Status.IsTerminal() {
		// Continue polling every 2 seconds. hx-swap=outerHTML replaces this
		// whole div, including the (possibly removed) hx-trigger, so polling
		// stops the moment we hit a terminal status.
		fmt.Fprintf(&sb,
			" hx-get='/ui/index/status?project=%s' hx-trigger='every 2s' hx-swap='outerHTML'",
			template.URLQueryEscaper(j.Project),
		)
	}
	sb.WriteString(">")

	switch j.Status {
	case jobs.StatusQueued:
		sb.WriteString("<h3 style='color:#d29922'>Queued…</h3>")
		sb.WriteString("<p class='small muted'>Waiting for worker slot.</p>")
		sb.WriteString(cancelForm(j.Project))

	case jobs.StatusRunning:
		sb.WriteString("<h3 style='color:#58a6ff'>Indexing in progress…</h3>")
		if j.FilesTotal > 0 {
			fmt.Fprintf(&sb,
				"<p class='small'>File %d / %d: <code>%s</code></p>",
				j.FilesDone+1, j.FilesTotal, template.HTMLEscapeString(j.CurrentFile),
			)
			pct := float64(j.FilesDone) * 100.0 / float64(j.FilesTotal)
			fmt.Fprintf(&sb,
				"<div style='background:#0d1117;border:1px solid #30363d;border-radius:6px;height:8px;overflow:hidden;margin:6px 0'>"+
					"<div style='width:%.1f%%;height:100%%;background:#1f6feb;transition:width 200ms ease-out'></div></div>",
				pct,
			)
		} else if j.CurrentFile != "" {
			fmt.Fprintf(&sb, "<p class='small'>Scanning: <code>%s</code></p>", template.HTMLEscapeString(j.CurrentFile))
		} else {
			sb.WriteString("<p class='small muted'>Walking source tree…</p>")
		}
		fmt.Fprintf(&sb,
			"<p class='small muted'>Chunks: %d · Embedded: %d · Indexed: %d</p>",
			j.ChunksDone, j.EmbeddedDone, j.IndexedDone,
		)
		sb.WriteString(cancelForm(j.Project))

	case jobs.StatusSucceeded:
		sb.WriteString("<h3 style='color:#3fb950'>Indexing complete ✓</h3>")
		if j.Stats != nil {
			sb.WriteString(renderStatsTable(*j.Stats))
		}

	case jobs.StatusFailed:
		sb.WriteString("<h3 class='error'>Indexing failed</h3>")
		if j.Error != "" {
			fmt.Fprintf(&sb, "<pre class='error small' style='white-space:pre-wrap'>%s</pre>", template.HTMLEscapeString(j.Error))
		}
		if j.Stats != nil && len(j.Stats.Errors) > 0 {
			sb.WriteString(renderStatsTable(*j.Stats))
		}

	case jobs.StatusCancelled:
		sb.WriteString("<h3 class='muted'>Cancelled by user</h3>")
		if j.Stats != nil {
			sb.WriteString(renderStatsTable(*j.Stats))
		}

	case jobs.StatusInterrupted:
		sb.WriteString("<h3 class='muted'>Interrupted (process restarted)</h3>")
		sb.WriteString("<p class='small muted'>The previous indexing run was orphaned by a container restart. Click Re-index to retry.</p>")
	}

	sb.WriteString("</div>")
	return sb.String()
}

// cancelForm renders the inline Cancel button form used inside running/queued
// status partials. Posts to /ui/index/cancel and swaps the whole stats div.
func cancelForm(project string) string {
	return fmt.Sprintf(
		`<form hx-post='/ui/index/cancel' hx-target='#index-stats' hx-swap='outerHTML' style='margin-top:10px'>`+
			`<input type='hidden' name='project' value='%s'>`+
			`<button type='submit' style='background:#da3633'>Cancel</button>`+
			`</form>`,
		template.HTMLEscapeString(project),
	)
}

// renderStatsTable renders the success/failure stats summary. Extracted so
// the renderJobStatus switch stays readable.
func renderStatsTable(st indexer.IndexStats) string {
	var sb strings.Builder
	sb.WriteString("<table style='font-size:12px;line-height:1.6;margin-top:8px'>")
	row := func(k string, v interface{}) {
		sb.WriteString(fmt.Sprintf("<tr><td style='color:#8b949e;padding-right:12px'>%s</td><td>%v</td></tr>",
			template.HTMLEscapeString(k), v))
	}
	row("Files scanned", st.FilesScanned)
	row("Chunks", st.Chunks)
	row("Embedded", st.Embedded)
	row("Indexed (points written)", st.Indexed)
	row("Skipped", st.Skipped)
	row("Duration", st.Duration.Round(1e6))
	row("Errors", len(st.Errors))
	sb.WriteString("</table>")
	if len(st.Errors) > 0 {
		sb.WriteString("<details style='margin-top:8px'><summary class='error small' style='cursor:pointer'>")
		sb.WriteString(fmt.Sprintf("%d errors (click to expand)</summary><pre style='font-size:11px;color:#f85149;margin-top:6px;white-space:pre-wrap'>", len(st.Errors)))
		for _, e := range st.Errors {
			sb.WriteString(template.HTMLEscapeString(e))
			sb.WriteString("\n")
		}
		sb.WriteString("</pre></details>")
	}
	return sb.String()
}
