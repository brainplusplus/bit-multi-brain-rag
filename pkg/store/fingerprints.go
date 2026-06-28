package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// FileFingerprint tracks the content hash of an indexed file so the indexer
// can skip unchanged files (partial / incremental indexing, ADR-0007 Phase 8).
type FileFingerprint struct {
	ProjectID  int64     `json:"project_id"`
	FilePath   string    `json:"file_path"`   // project-relative
	SHA256     string    `json:"sha256"`      // hex-encoded content hash
	PointCount int       `json:"point_count"` // number of Qdrant points for this file
	IndexedAt  time.Time `json:"indexed_at"`
}

// migrateFingerprints creates the file_fingerprints table.
func (s *Store) migrateFingerprints(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS file_fingerprints (
	project_id  INTEGER NOT NULL,
	file_path   TEXT NOT NULL,
	sha256      TEXT NOT NULL,
	point_count INTEGER NOT NULL DEFAULT 0,
	indexed_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	PRIMARY KEY (project_id, file_path),
	FOREIGN KEY (project_id) REFERENCES projects(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_fingerprints_project ON file_fingerprints(project_id);
`)
	return err
}

// GetFingerprint returns the stored fingerprint for a file, or nil if not indexed.
func (s *Store) GetFingerprint(ctx context.Context, projectID int64, filePath string) (*FileFingerprint, error) {
	var f FileFingerprint
	err := s.db.QueryRowContext(ctx,
		`SELECT project_id, file_path, sha256, point_count, indexed_at
		 FROM file_fingerprints WHERE project_id = ? AND file_path = ?`,
		projectID, filePath,
	).Scan(&f.ProjectID, &f.FilePath, &f.SHA256, &f.PointCount, &f.IndexedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get fingerprint: %w", err)
	}
	return &f, nil
}

// AllFingerprints returns all stored fingerprints for a project.
// Used by the indexer to detect removed files (stale points).
func (s *Store) AllFingerprints(ctx context.Context, projectID int64) (map[string]*FileFingerprint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT file_path, sha256, point_count, indexed_at
		 FROM file_fingerprints WHERE project_id = ?`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query fingerprints: %w", err)
	}
	defer rows.Close()
	out := make(map[string]*FileFingerprint)
	for rows.Next() {
		var f FileFingerprint
		f.ProjectID = projectID
		if err := rows.Scan(&f.FilePath, &f.SHA256, &f.PointCount, &f.IndexedAt); err != nil {
			return nil, err
		}
		out[f.FilePath] = &f
	}
	return out, rows.Err()
}

// SetFingerprint upserts a file fingerprint.
func (s *Store) SetFingerprint(ctx context.Context, projectID int64, filePath, sha256 string, pointCount int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO file_fingerprints (project_id, file_path, sha256, point_count, indexed_at)
		 VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		 ON CONFLICT(project_id, file_path) DO UPDATE SET
		   sha256 = excluded.sha256,
		   point_count = excluded.point_count,
		   indexed_at = CURRENT_TIMESTAMP`,
		projectID, filePath, sha256, pointCount,
	)
	return err
}

// DeleteFingerprints removes fingerprints for files no longer present.
// Returns the list of removed file paths (for Qdrant point deletion).
func (s *Store) DeleteFingerprintsExcept(ctx context.Context, projectID int64, keepPaths []string) ([]string, error) {
	// Get all stored paths.
	stored, err := s.AllFingerprints(ctx, projectID)
	if err != nil {
		return nil, err
	}
	keep := make(map[string]bool, len(keepPaths))
	for _, p := range keepPaths {
		keep[p] = true
	}
	var removed []string
	for path := range stored {
		if !keep[path] {
			removed = append(removed, path)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	// Batch delete.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return removed, err
	}
	for _, path := range removed {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM file_fingerprints WHERE project_id = ? AND file_path = ?`,
			projectID, path); err != nil {
			tx.Rollback()
			return removed, err
		}
	}
	return removed, tx.Commit()
}
