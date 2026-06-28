// Package store implements the SQLite-backed project metadata store for the
// dashboard. It persists project definitions (name, root path, domains) so
// the UI can list/select projects across restarts.
//
// The vector data itself lives in Qdrant (per-project collections, ADR-0002);
// SQLite only holds the lightweight project registry.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// Project is a registered code repository tracked by the dashboard.
type Project struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`         // human-friendly, unique
	RootPath    string    `json:"root_path"`    // absolute filesystem path (local, read by MCP)
	Description string    `json:"description"`  // optional
	Domains     string    `json:"domains"`      // comma-separated: "code,doc,task"
	MachineID   string    `json:"machine_id"`   // HMAC-SHA256 of machine ID (multi-machine support)
	MachineName string    `json:"machine_name"` // hostname for display
	MachineOS   string    `json:"machine_os"`   // runtime.GOOS (windows/linux/darwin)
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Store wraps the SQLite connection for project metadata.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at dbPath and runs migrations.
func Open(dbPath string) (*Store, error) {
	// _pragma=foreign_keys(1) enforces FK constraints; _txlock=immediate avoids
	// SQLite "database is locked" under concurrent dashboard writes.
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_txlock=immediate", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single writer connection avoids lock contention for our low-traffic dashboard.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	s := &Store{db: db}
	ctx := context.Background()
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate projects: %w", err)
	}
	if err := s.migrateJobs(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate jobs: %w", err)
	}
	if err := s.migrateModels(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate models: %w", err)
	}
	if err := s.migrateFingerprints(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate fingerprints: %w", err)
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// migrate creates the projects table if it does not exist.
func (s *Store) migrate(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS projects (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL UNIQUE,
    root_path    TEXT    NOT NULL,
    description  TEXT    NOT NULL DEFAULT '',
    domains      TEXT    NOT NULL DEFAULT 'code',
    machine_id   TEXT    NOT NULL DEFAULT '',
    machine_name TEXT    NOT NULL DEFAULT '',
    machine_os   TEXT    NOT NULL DEFAULT '',
    created_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);`
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return err
	}
	// Migration: add columns for existing DBs (ALTER TABLE ... ADD COLUMN is
	// idempotent-safe via the column-not-exists check).
	for _, col := range []string{"machine_id", "machine_name", "machine_os"} {
		_, _ = s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE projects ADD COLUMN %s TEXT NOT NULL DEFAULT ''", col))
		// Ignore error if column already exists.
	}
	return nil
}

// CreateProject inserts a new project. Returns the inserted row with ID set.
func (s *Store) CreateProject(ctx context.Context, p Project) (Project, error) {
	if p.Domains == "" {
		p.Domains = "code"
	}
	const q = `INSERT INTO projects (name, root_path, description, domains, machine_id, machine_name, machine_os)
	VALUES (?, ?, ?, ?, ?, ?, ?)
	RETURNING id, created_at, updated_at`
	err := s.db.QueryRowContext(ctx, q, p.Name, p.RootPath, p.Description, p.Domains,
		p.MachineID, p.MachineName, p.MachineOS).
		Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return p, nil
}

// ListProjects returns all projects, ordered by name.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	const q = `SELECT id, name, root_path, description, domains, machine_id, machine_name, machine_os, created_at, updated_at
	FROM projects ORDER BY name ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.RootPath, &p.Description, &p.Domains,
			&p.MachineID, &p.MachineName, &p.MachineOS, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProject returns a single project by ID.
func (s *Store) GetProject(ctx context.Context, id int64) (Project, error) {
	const q = `SELECT id, name, root_path, description, domains, machine_id, machine_name, machine_os, created_at, updated_at
	FROM projects WHERE id = ?`
	var p Project
	err := s.db.QueryRowContext(ctx, q, id).
		Scan(&p.ID, &p.Name, &p.RootPath, &p.Description, &p.Domains,
			&p.MachineID, &p.MachineName, &p.MachineOS, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return Project{}, fmt.Errorf("get project %d: %w", id, err)
	}
	return p, nil
}

// GetProjectByPathAndMachine finds a project by (machine_id, root_path).
// This is the multi-machine idempotency key: same path on different machines
// = different projects. If machineID is empty, falls back to path-only match
// (backward compat for single-machine deployments).
func (s *Store) GetProjectByPathAndMachine(ctx context.Context, machineID, rootPath string) (*Project, error) {
	var q string
	var args []any
	if machineID != "" {
		q = `SELECT id, name, root_path, description, domains, machine_id, machine_name, machine_os, created_at, updated_at
		FROM projects WHERE machine_id = ? AND root_path = ? LIMIT 1`
		args = []any{machineID, rootPath}
	} else {
		q = `SELECT id, name, root_path, description, domains, machine_id, machine_name, machine_os, created_at, updated_at
		FROM projects WHERE root_path = ? LIMIT 1`
		args = []any{rootPath}
	}
	var p Project
	err := s.db.QueryRowContext(ctx, q, args...).
		Scan(&p.ID, &p.Name, &p.RootPath, &p.Description, &p.Domains,
			&p.MachineID, &p.MachineName, &p.MachineOS, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err // sql.ErrNoRows if not found
	}
	return &p, nil
}

// DeleteProject removes a project by ID.
func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	const q = `DELETE FROM projects WHERE id = ?`
	res, err := s.db.ExecContext(ctx, q, id)
	if err != nil {
		return fmt.Errorf("delete project %d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("delete project %d: not found", id)
	}
	return nil
}
