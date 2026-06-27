package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// JobStatus is the persisted lifecycle status of an indexing job.
// Mirrors pkg/jobs.Status (we keep them in sync — see ADR-0005).
type JobStatus string

const (
	JobStatusQueued      JobStatus = "queued"
	JobStatusRunning     JobStatus = "running"
	JobStatusSucceeded   JobStatus = "succeeded"
	JobStatusFailed      JobStatus = "failed"
	JobStatusCancelled   JobStatus = "cancelled"
	JobStatusInterrupted JobStatus = "interrupted" // process restart caught it mid-run
)

// JobRow is the SQLite-persisted projection of an indexing job.
// pkg/jobs.Job is the in-memory live view; this is the durable record
// (audit trail + restart recovery).
type JobRow struct {
	ID          string       `json:"id"`
	Project     string       `json:"project"`
	Status      JobStatus    `json:"status"`
	StartedAt   time.Time    `json:"started_at"`
	FinishedAt  sql.NullTime `json:"finished_at,omitempty"`
	FilesTotal  int          `json:"files_total"`
	FilesDone   int          `json:"files_done"`
	Chunks      int          `json:"chunks"`
	Embedded    int          `json:"embedded"`
	Indexed     int          `json:"indexed"`
	DurationMs  int64        `json:"duration_ms"`
	Error       string       `json:"error,omitempty"`
	ErrorsJSON  string       `json:"errors_json,omitempty"`
}

// migrateJobs creates the index_jobs table on first boot. Safe to call repeatedly.
//
// We do NOT use a separate migrations runner for this single new table —
// keep it inline with the project store migration for v1. If we add a third
// table, refactor to a real migration framework.
func (s *Store) migrateJobs(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS index_jobs (
    id           TEXT PRIMARY KEY,
    project      TEXT NOT NULL,
    status       TEXT NOT NULL,
    started_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    finished_at  DATETIME,
    files_total  INTEGER NOT NULL DEFAULT 0,
    files_done   INTEGER NOT NULL DEFAULT 0,
    chunks       INTEGER NOT NULL DEFAULT 0,
    embedded     INTEGER NOT NULL DEFAULT 0,
    indexed      INTEGER NOT NULL DEFAULT 0,
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    error        TEXT NOT NULL DEFAULT '',
    errors_json  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_index_jobs_project_started
    ON index_jobs(project, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_index_jobs_status
    ON index_jobs(status);
`
	_, err := s.db.ExecContext(ctx, ddl)
	return err
}

// InsertJob persists a brand-new job row in the "queued" state.
func (s *Store) InsertJob(ctx context.Context, j JobRow) error {
	const q = `INSERT INTO index_jobs
    (id, project, status, started_at, files_total, files_done, chunks, embedded, indexed, duration_ms, error, errors_json)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if j.StartedAt.IsZero() {
		j.StartedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, q,
		j.ID, j.Project, string(j.Status), j.StartedAt,
		j.FilesTotal, j.FilesDone, j.Chunks, j.Embedded, j.Indexed,
		j.DurationMs, j.Error, j.ErrorsJSON,
	)
	if err != nil {
		return fmt.Errorf("insert job %s: %w", j.ID, err)
	}
	return nil
}

// UpdateJob persists the final state of a job (status, counters, error).
// Only called on terminal transitions — we don't write live progress here
// (that's the in-memory map's job, see pkg/jobs).
func (s *Store) UpdateJob(ctx context.Context, j JobRow) error {
	const q = `UPDATE index_jobs SET
        status       = ?,
        finished_at  = ?,
        files_total  = ?,
        files_done   = ?,
        chunks       = ?,
        embedded     = ?,
        indexed      = ?,
        duration_ms  = ?,
        error        = ?,
        errors_json  = ?
    WHERE id = ?`
	var finished sql.NullTime
	if j.FinishedAt.Valid {
		finished = j.FinishedAt
	} else {
		finished = sql.NullTime{Time: time.Now().UTC(), Valid: true}
	}
	res, err := s.db.ExecContext(ctx, q,
		string(j.Status), finished,
		j.FilesTotal, j.FilesDone, j.Chunks, j.Embedded, j.Indexed,
		j.DurationMs, j.Error, j.ErrorsJSON,
		j.ID,
	)
	if err != nil {
		return fmt.Errorf("update job %s: %w", j.ID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("update job %s: row not found", j.ID)
	}
	return nil
}

// GetJob returns a single job row by ID, or sql.ErrNoRows if missing.
func (s *Store) GetJob(ctx context.Context, id string) (JobRow, error) {
	const q = `SELECT id, project, status, started_at, finished_at,
        files_total, files_done, chunks, embedded, indexed,
        duration_ms, error, errors_json
    FROM index_jobs WHERE id = ?`
	var j JobRow
	var statusStr string
	err := s.db.QueryRowContext(ctx, q, id).Scan(
		&j.ID, &j.Project, &statusStr, &j.StartedAt, &j.FinishedAt,
		&j.FilesTotal, &j.FilesDone, &j.Chunks, &j.Embedded, &j.Indexed,
		&j.DurationMs, &j.Error, &j.ErrorsJSON,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return JobRow{}, err
		}
		return JobRow{}, fmt.Errorf("get job %s: %w", id, err)
	}
	j.Status = JobStatus(statusStr)
	return j, nil
}

// ListJobsByProject returns the most recent N jobs for a project, newest first.
func (s *Store) ListJobsByProject(ctx context.Context, project string, limit int) ([]JobRow, error) {
	if limit <= 0 {
		limit = 10
	}
	const q = `SELECT id, project, status, started_at, finished_at,
        files_total, files_done, chunks, embedded, indexed,
        duration_ms, error, errors_json
    FROM index_jobs WHERE project = ? ORDER BY started_at DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, project, limit)
	if err != nil {
		return nil, fmt.Errorf("list jobs %s: %w", project, err)
	}
	defer rows.Close()
	var out []JobRow
	for rows.Next() {
		var j JobRow
		var statusStr string
		if err := rows.Scan(
			&j.ID, &j.Project, &statusStr, &j.StartedAt, &j.FinishedAt,
			&j.FilesTotal, &j.FilesDone, &j.Chunks, &j.Embedded, &j.Indexed,
			&j.DurationMs, &j.Error, &j.ErrorsJSON,
		); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		j.Status = JobStatus(statusStr)
		out = append(out, j)
	}
	return out, rows.Err()
}

// MarkRunningAsInterrupted is called at startup. Any jobs left in queued/running
// state from a previous container instance are orphans (their goroutines died
// with the process) and must be flagged so the UI doesn't show a stale spinner.
func (s *Store) MarkRunningAsInterrupted(ctx context.Context) (int64, error) {
	const q = `UPDATE index_jobs
        SET status = 'interrupted', finished_at = CURRENT_TIMESTAMP,
            error = COALESCE(NULLIF(error,''), 'process restarted')
        WHERE status IN ('queued', 'running')`
	res, err := s.db.ExecContext(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("mark interrupted: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
