// Package jobs implements the background indexing job manager.
//
// Responsibilities (ADR-0005):
//   - Per-project locking: only one active indexing job per project at a time.
//     Different projects may run in parallel.
//   - Live progress: in-memory map updated by the indexer's ProgressFn so the
//     UI can poll a snapshot every 2s without touching SQLite per tick.
//   - Persistence: each job is written to the index_jobs SQLite table on
//     enqueue and on completion (2 writes per job, regardless of duration).
//   - Cancellation: each job owns a context.CancelFunc; Cancel(project) signals
//     the running indexer to stop at the next file boundary.
//   - Restart recovery: at startup the manager flips any leftover queued/
//     running rows to "interrupted" so the UI doesn't show a stale spinner.
//
// This package depends on indexer (for the actual work) and store (for the
// audit trail). It does not depend on dashboard — the dashboard package wires
// HTTP handlers around it.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/indexer"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/store"
)

// Status mirrors store.JobStatus but is repeated here so callers can switch
// on it without importing store. Keep the string values in sync.
type Status string

const (
	StatusQueued      Status = "queued"
	StatusRunning     Status = "running"
	StatusSucceeded   Status = "succeeded"
	StatusFailed      Status = "failed"
	StatusCancelled   Status = "cancelled"
	StatusInterrupted Status = "interrupted"
)

// IsTerminal reports whether the status is final (no more transitions).
// Polling loops use this to know when to stop refreshing.
func (s Status) IsTerminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusInterrupted:
		return true
	}
	return false
}

// ErrAlreadyRunning is returned by Enqueue when a job for the given project
// is already active. Callers should treat this as idempotent: the existing
// Job pointer is also returned (via *Manager.Get) so the UI can show the
// in-flight state instead of duplicating work.
var ErrAlreadyRunning = errors.New("indexing already in progress for this project")

// Job is the in-memory live view of an indexing run. The DB row carries the
// same fields for audit/recovery — see store.JobRow.
//
// All field reads/writes go through Manager methods so the mutex is honored.
type Job struct {
	ID         string
	Project    string
	RootPath   string
	ProjectID  int64  // for fingerprint-based incremental indexing (0 = disabled)
	Status     Status
	StartedAt  time.Time
	FinishedAt time.Time

	// Live progress (updated by indexer callback while Status=running):
	FilesTotal   int
	FilesDone    int
	ChunksDone   int
	EmbeddedDone int
	IndexedDone  int
	CurrentFile  string

	// Set when Status becomes terminal:
	Stats *indexer.IndexStats
	Error string

	// cancel signals the indexer's context to stop. Nil for jobs loaded from
	// DB (those are not running in this process).
	cancel context.CancelFunc
}

// Snapshot returns a copy of the job safe to consume outside the manager lock.
// It is the standard way for handlers to read state.
func (j *Job) Snapshot() Job {
	cp := *j
	cp.cancel = nil // do not leak cancel to callers
	if j.Stats != nil {
		s := *j.Stats
		cp.Stats = &s
	}
	return cp
}

// Manager coordinates the lifecycle of indexing jobs.
type Manager struct {
	mu       sync.Mutex
	active   map[string]*Job // key: project name (per-project lock)
	recent   []*Job          // bounded ring of the last N finished jobs (any project)
	store    *store.Store
	indexer  *indexer.Indexer
	logger   *slog.Logger
	recentN  int
}

// NewManager constructs a Manager and runs startup recovery (interrupted jobs).
// The caller must keep the manager alive for the lifetime of the process — it
// owns goroutines via Enqueue.
func NewManager(st *store.Store, ix *indexer.Indexer, logger *slog.Logger) *Manager {
	m := &Manager{
		active:  make(map[string]*Job),
		store:   st,
		indexer: ix,
		logger:  logger.With("component", "jobs"),
		recentN: 50,
	}
	if n, err := st.MarkRunningAsInterrupted(context.Background()); err != nil {
		m.logger.Warn("startup recovery failed", "error", err)
	} else if n > 0 {
		m.logger.Info("startup recovery: marked orphaned jobs as interrupted", "count", n)
	}
	return m
}

// Enqueue starts a new indexing job for the given project. If one is already
// active for the same project, returns the existing Job and ErrAlreadyRunning
// (caller should treat as idempotent — same project, same work).
func (m *Manager) Enqueue(project, rootPath string, projectID int64) (*Job, error) {
	m.mu.Lock()
	if existing, ok := m.active[project]; ok {
		m.mu.Unlock()
		return existing, ErrAlreadyRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	job := &Job{
		ID:        uuid.NewString(),
		Project:   project,
		RootPath:  rootPath,
		ProjectID: projectID,
		Status:    StatusQueued,
		StartedAt: time.Now().UTC(),
		cancel:    cancel,
	}
	m.active[project] = job
	m.mu.Unlock()

	// Persist the queued row. Errors here are non-fatal (audit trail only —
	// the in-memory live view is what the UI sees first).
	if err := m.store.InsertJob(context.Background(), store.JobRow{
		ID:        job.ID,
		Project:   project,
		Status:    store.JobStatusQueued,
		StartedAt: job.StartedAt,
	}); err != nil {
		m.logger.Warn("insert job row failed", "id", job.ID, "error", err)
	}

	go m.run(ctx, job)
	return job, nil
}

// run is the goroutine that actually drives the indexer. It transitions the
// job through running → terminal and persists the final state on completion.
func (m *Manager) run(ctx context.Context, job *Job) {
	m.transition(job, StatusRunning)
	m.logger.Info("job started", "id", job.ID, "project", job.Project, "root", job.RootPath)

	progress := func(ev indexer.ProgressEvent) {
		m.mu.Lock()
		job.FilesTotal = ev.FilesTotal
		job.FilesDone = ev.FilesDone
		job.ChunksDone = ev.ChunksDone
		job.EmbeddedDone = ev.EmbeddedDone
		job.IndexedDone = ev.IndexedDone
		if ev.CurrentFile != "" {
			job.CurrentFile = ev.CurrentFile
		}
		m.mu.Unlock()
	}

	stats, err := m.indexer.IndexProjectWithProgress(ctx, job.Project, job.RootPath, job.ProjectID, progress)

	// Decide terminal status. Order matters: ctx.Err() trumps a returned
	// IndexStats error because cancellation is the user-initiated case.
	m.mu.Lock()
	job.Stats = &stats
	job.FilesTotal = stats.FilesScanned
	job.FilesDone = stats.FilesScanned
	job.ChunksDone = stats.Chunks
	job.EmbeddedDone = stats.Embedded
	job.IndexedDone = stats.Indexed
	job.FinishedAt = time.Now().UTC()
	switch {
	case ctx.Err() != nil:
		job.Status = StatusCancelled
		job.Error = "cancelled by user"
	case err != nil:
		job.Status = StatusFailed
		job.Error = err.Error()
	default:
		job.Status = StatusSucceeded
	}
	finalStatus := job.Status
	finalErr := job.Error
	finalStats := stats
	m.mu.Unlock()

	// Persist final state.
	errorsJSON := ""
	if len(finalStats.Errors) > 0 {
		if b, jerr := json.Marshal(finalStats.Errors); jerr == nil {
			errorsJSON = string(b)
		}
	}
	if err := m.store.UpdateJob(context.Background(), store.JobRow{
		ID:         job.ID,
		Project:    job.Project,
		Status:     store.JobStatus(finalStatus),
		StartedAt:  job.StartedAt,
		FilesTotal: finalStats.FilesScanned,
		FilesDone:  finalStats.FilesScanned,
		Chunks:     finalStats.Chunks,
		Embedded:   finalStats.Embedded,
		Indexed:    finalStats.Indexed,
		DurationMs: finalStats.Duration.Milliseconds(),
		Error:      finalErr,
		ErrorsJSON: errorsJSON,
	}); err != nil {
		m.logger.Warn("update job row failed", "id", job.ID, "error", err)
	}

	// Move from active → recent ring, freeing the per-project lock.
	m.mu.Lock()
	delete(m.active, job.Project)
	m.recent = append(m.recent, job)
	if len(m.recent) > m.recentN {
		m.recent = m.recent[len(m.recent)-m.recentN:]
	}
	m.mu.Unlock()

	m.logger.Info("job done",
		"id", job.ID, "project", job.Project, "status", string(finalStatus),
		"duration", finalStats.Duration, "indexed", finalStats.Indexed, "errors", len(finalStats.Errors),
	)
}

// transition flips a job's status under lock and is used for the running edge
// (other terminal transitions happen inline in run for cohesion).
func (m *Manager) transition(job *Job, st Status) {
	m.mu.Lock()
	job.Status = st
	m.mu.Unlock()
}

// Cancel signals the active job for the given project (if any) to stop.
// Returns true if a job was found and cancelled, false otherwise.
func (m *Manager) Cancel(project string) bool {
	m.mu.Lock()
	job, ok := m.active[project]
	m.mu.Unlock()
	if !ok || job.cancel == nil {
		return false
	}
	m.logger.Info("cancel requested", "id", job.ID, "project", project)
	job.cancel()
	return true
}

// GetActive returns the in-flight job for a project, or nil if none is active.
func (m *Manager) GetActive(project string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.active[project]; ok {
		s := j.Snapshot()
		return &s
	}
	return nil
}

// GetLatest returns either the active job (if any) or the most recent finished
// job for a project (from in-memory recent ring; falls back to SQLite if not
// in ring). Useful for "what's the state of indexing for X" queries.
func (m *Manager) GetLatest(ctx context.Context, project string) (*Job, error) {
	if j := m.GetActive(project); j != nil {
		return j, nil
	}
	// Scan recent ring newest first.
	m.mu.Lock()
	for i := len(m.recent) - 1; i >= 0; i-- {
		if m.recent[i].Project == project {
			s := m.recent[i].Snapshot()
			m.mu.Unlock()
			return &s, nil
		}
	}
	m.mu.Unlock()
	// Fall back to SQLite for jobs that pre-date this process.
	rows, err := m.store.ListJobsByProject(ctx, project, 1)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rowToJob(rows[0]), nil
}

// GetByID returns a job by ID (active → recent → DB lookup).
func (m *Manager) GetByID(ctx context.Context, id string) (*Job, error) {
	m.mu.Lock()
	for _, j := range m.active {
		if j.ID == id {
			s := j.Snapshot()
			m.mu.Unlock()
			return &s, nil
		}
	}
	for _, j := range m.recent {
		if j.ID == id {
			s := j.Snapshot()
			m.mu.Unlock()
			return &s, nil
		}
	}
	m.mu.Unlock()
	row, err := m.store.GetJob(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get job %s: %w", id, err)
	}
	return rowToJob(row), nil
}

// rowToJob projects a persisted store.JobRow into a Job (for jobs not active
// in this process). cancel is nil because the goroutine no longer exists.
func rowToJob(r store.JobRow) *Job {
	j := &Job{
		ID:           r.ID,
		Project:      r.Project,
		Status:       Status(r.Status),
		StartedAt:    r.StartedAt,
		FilesTotal:   r.FilesTotal,
		FilesDone:    r.FilesDone,
		ChunksDone:   r.Chunks,
		EmbeddedDone: r.Embedded,
		IndexedDone:  r.Indexed,
		Error:        r.Error,
	}
	if r.FinishedAt.Valid {
		j.FinishedAt = r.FinishedAt.Time
	}
	if r.DurationMs > 0 {
		stats := indexer.IndexStats{
			FilesScanned: r.FilesTotal,
			Chunks:       r.Chunks,
			Embedded:     r.Embedded,
			Indexed:      r.Indexed,
			Duration:     time.Duration(r.DurationMs) * time.Millisecond,
		}
		if r.ErrorsJSON != "" {
			_ = json.Unmarshal([]byte(r.ErrorsJSON), &stats.Errors)
		}
		j.Stats = &stats
	}
	return j
}
