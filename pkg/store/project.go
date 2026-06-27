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
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
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
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    root_path   TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    domains     TEXT    NOT NULL DEFAULT 'code',
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);`
	_, err := s.db.ExecContext(ctx, ddl)
	return err
}

// CreateProject inserts a new project. Returns the inserted row with ID set.
func (s *Store) CreateProject(ctx context.Context, p Project) (Project, error) {
	if p.Domains == "" {
		p.Domains = "code"
	}
	const q = `INSERT INTO projects (name, root_path, description, domains) VALUES (?, ?, ?, ?)
	RETURNING id, created_at, updated_at`
	err := s.db.QueryRowContext(ctx, q, p.Name, p.RootPath, p.Description, p.Domains).
		Scan(&p.ID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return p, nil
}

// ListProjects returns all projects, ordered by name.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	const q = `SELECT id, name, root_path, description, domains, created_at, updated_at
	FROM projects ORDER BY name ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Name, &p.RootPath, &p.Description, &p.Domains, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetProject returns a single project by ID.
func (s *Store) GetProject(ctx context.Context, id int64) (Project, error) {
	const q = `SELECT id, name, root_path, description, domains, created_at, updated_at
	FROM projects WHERE id = ?`
	var p Project
	err := s.db.QueryRowContext(ctx, q, id).
		Scan(&p.ID, &p.Name, &p.RootPath, &p.Description, &p.Domains, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return Project{}, fmt.Errorf("get project %d: %w", id, err)
	}
	return p, nil
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
