package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// EmbedBackend identifies the embedding provider type.
type EmbedBackend string

const (
	BackendLlamaQ8    EmbedBackend = "llama_q8"
	BackendOpenAI     EmbedBackend = "openai"
	BackendCohere     EmbedBackend = "cohere"
	BackendVoyage     EmbedBackend = "voyage"
	BackendOpenRouter EmbedBackend = "openrouter"
	BackendOllama     EmbedBackend = "ollama"
)

// EmbeddingModel is a configured embedding model in the registry.
type EmbeddingModel struct {
	ID               int64        `json:"id"`
	Name             string       `json:"name"`       // human label, e.g. "voyage-4-nano (local Q8)"
	Backend          EmbedBackend `json:"backend"`    // one of the Backend* constants
	ModelName        string       `json:"model_name"` // wire model name sent to API (e.g. "text-embedding-3-small")
	Endpoint         string       `json:"endpoint"`   // base URL (empty = provider default)
	APIKey           string       `json:"api_key"`    // encrypted at rest; masked in JSON output
	Dim              int          `json:"dim"`        // vector dimension
	Pooling          string       `json:"pooling"`    // "mean", "cls", "" (provider default)
	MaxContextTokens int          `json:"max_context_tokens"` // model's hard ctx limit (curated)
	ChunkTokens      *int         `json:"chunk_tokens,omitempty"` // user override; nil = auto
	ChunkOverlap     int          `json:"chunk_overlap"`      // overlap tokens; default 100
	IsActive         bool         `json:"is_active"`  // true if this is the currently active model
	CreatedAt        time.Time    `json:"created_at"`
}

// EffectiveChunkTokens returns the chunk size to use for indexing with this model.
// Smart default: min(MaxContextTokens * 0.8, 2000). Capped at 2000 because RAG
// research (2026) consistently shows that retrieval precision degrades above
// ~2k tokens per chunk — one vector cannot meaningfully represent more than
// ~1-2 coherent concepts. Users can override by setting ChunkTokens.
func (m EmbeddingModel) EffectiveChunkTokens() int {
	if m.ChunkTokens != nil && *m.ChunkTokens > 0 {
		return *m.ChunkTokens
	}
	if m.MaxContextTokens <= 0 {
		return 400 // legacy conservative default for pre-migration models
	}
	auto := int(float64(m.MaxContextTokens) * 0.8)
	if auto > 2000 {
		auto = 2000
	}
	if auto < 128 {
		auto = 128
	}
	return auto
}

// migrateModels creates the embedding_models + settings tables and applies
// later schema migrations (idempotent ALTER statements).
func (s *Store) migrateModels(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS embedding_models (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL,
    backend    TEXT NOT NULL,
    model_name TEXT NOT NULL,
    endpoint   TEXT NOT NULL DEFAULT '',
    api_key    TEXT NOT NULL DEFAULT '',
    dim        INTEGER NOT NULL DEFAULT 1024,
    pooling    TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

-- Seed active_model_id if not exists
INSERT OR IGNORE INTO settings (key, value) VALUES ('active_model_id', '0');
-- Seed embedder_mode (cpu | gpu) — drives which embedder image is running.
INSERT OR IGNORE INTO settings (key, value) VALUES ('embedder_mode', 'cpu');
`
	if _, err := s.db.ExecContext(ctx, ddl); err != nil {
		return err
	}
	// Idempotent ALTERs for chunk-size + context-window fields added in 2026-06.
	// SQLite ALTER TABLE ADD COLUMN errors if the column already exists; we
	// swallow those errors by checking pragma table_info first.
	addCol := func(table, col, def string) error {
		var name string
		err := s.db.QueryRowContext(ctx,
			`SELECT name FROM pragma_table_info(?) WHERE name = ?`, table, col,
		).Scan(&name)
		if err == nil {
			return nil // column exists
		}
		_, err = s.db.ExecContext(ctx,
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, def))
		return err
	}
	if err := addCol("embedding_models", "max_context_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return fmt.Errorf("add max_context_tokens: %w", err)
	}
	if err := addCol("embedding_models", "chunk_tokens", "INTEGER"); err != nil {
		return fmt.Errorf("add chunk_tokens: %w", err)
	}
	if err := addCol("embedding_models", "chunk_overlap", "INTEGER NOT NULL DEFAULT 100"); err != nil {
		return fmt.Errorf("add chunk_overlap: %w", err)
	}
	return nil
}

// CreateModel inserts a new embedding model config.
func (s *Store) CreateModel(ctx context.Context, m EmbeddingModel) (EmbeddingModel, error) {
	const q = `INSERT INTO embedding_models
	(name, backend, model_name, endpoint, api_key, dim, pooling, max_context_tokens, chunk_tokens, chunk_overlap)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id, created_at`
	var chunkTokens any // nil for NULL
	if m.ChunkTokens != nil {
		chunkTokens = *m.ChunkTokens
	}
	if m.ChunkOverlap == 0 {
		m.ChunkOverlap = 100
	}
	err := s.db.QueryRowContext(ctx, q,
		m.Name, string(m.Backend), m.ModelName, m.Endpoint, m.APIKey, m.Dim, m.Pooling,
		m.MaxContextTokens, chunkTokens, m.ChunkOverlap,
	).Scan(&m.ID, &m.CreatedAt)
	if err != nil {
		return EmbeddingModel{}, fmt.Errorf("create model: %w", err)
	}
	return m, nil
}

// UpdateModel patches an existing model. Only mutable fields are updated.
func (s *Store) UpdateModel(ctx context.Context, m EmbeddingModel) error {
	if m.ChunkOverlap == 0 {
		m.ChunkOverlap = 100
	}
	var chunkTokens any
	if m.ChunkTokens != nil {
		chunkTokens = *m.ChunkTokens
	}
	const q = `UPDATE embedding_models SET
	name=?, model_name=?, endpoint=?, api_key=?, dim=?, pooling=?,
	max_context_tokens=?, chunk_tokens=?, chunk_overlap=?
	WHERE id=?`
	res, err := s.db.ExecContext(ctx, q,
		m.Name, m.ModelName, m.Endpoint, m.APIKey, m.Dim, m.Pooling,
		m.MaxContextTokens, chunkTokens, m.ChunkOverlap, m.ID,
	)
	if err != nil {
		return fmt.Errorf("update model %d: %w", m.ID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("model %d not found", m.ID)
	}
	return nil
}

// scanModel reads one row into a model, handling nullable chunk_tokens.
func scanModel(scanner interface {
	Scan(dest ...any) error
}) (EmbeddingModel, error) {
	var m EmbeddingModel
	var backend string
	var chunkTokens sql.NullInt64
	err := scanner.Scan(
		&m.ID, &m.Name, &backend, &m.ModelName, &m.Endpoint, &m.APIKey,
		&m.Dim, &m.Pooling, &m.MaxContextTokens, &chunkTokens, &m.ChunkOverlap,
		&m.CreatedAt,
	)
	if err != nil {
		return EmbeddingModel{}, err
	}
	m.Backend = EmbedBackend(backend)
	if chunkTokens.Valid {
		v := int(chunkTokens.Int64)
		m.ChunkTokens = &v
	}
	return m, nil
}

// ListModels returns all configured models with is_active flag set.
func (s *Store) ListModels(ctx context.Context) ([]EmbeddingModel, error) {
	activeID := s.getActiveModelID(ctx)
	const q = `SELECT id, name, backend, model_name, endpoint, api_key, dim, pooling,
	max_context_tokens, chunk_tokens, chunk_overlap, created_at
	FROM embedding_models ORDER BY name ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer rows.Close()
	var out []EmbeddingModel
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		m.IsActive = m.ID == activeID
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetModel returns a single model by ID.
func (s *Store) GetModel(ctx context.Context, id int64) (EmbeddingModel, error) {
	activeID := s.getActiveModelID(ctx)
	const q = `SELECT id, name, backend, model_name, endpoint, api_key, dim, pooling,
	max_context_tokens, chunk_tokens, chunk_overlap, created_at
	FROM embedding_models WHERE id = ?`
	m, err := scanModel(s.db.QueryRowContext(ctx, q, id))
	if err != nil {
		return EmbeddingModel{}, fmt.Errorf("get model %d: %w", id, err)
	}
	m.IsActive = m.ID == activeID
	return m, nil
}

// DeleteModel removes a model config. Cannot delete the active model.
func (s *Store) DeleteModel(ctx context.Context, id int64) error {
	activeID := s.getActiveModelID(ctx)
	if id == activeID {
		return fmt.Errorf("cannot delete the active model (id=%d); switch to another first", id)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM embedding_models WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete model %d: %w", id, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("model %d not found", id)
	}
	return nil
}

// SetActiveModel updates the active model ID in settings.
func (s *Store) SetActiveModel(ctx context.Context, id int64) error {
	// Verify model exists.
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM embedding_models WHERE id = ?`, id).Scan(&exists)
	if err != nil {
		return fmt.Errorf("model %d not found", id)
	}
	_, err = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO settings (key, value) VALUES ('active_model_id', ?)`, fmt.Sprintf("%d", id))
	if err != nil {
		return fmt.Errorf("set active model: %w", err)
	}
	return nil
}

// GetActiveModelID returns the current active model ID (0 if unset).
func (s *Store) GetActiveModelID(ctx context.Context) int64 {
	return s.getActiveModelID(ctx)
}

func (s *Store) getActiveModelID(ctx context.Context) int64 {
	var val string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'active_model_id'`).Scan(&val)
	if err != nil {
		return 0
	}
	var id int64
	fmt.Sscanf(val, "%d", &id)
	return id
}

// GetActiveModel returns the currently active model, or sql.ErrNoRows if none set.
func (s *Store) GetActiveModel(ctx context.Context) (EmbeddingModel, error) {
	id := s.getActiveModelID(ctx)
	if id == 0 {
		return EmbeddingModel{}, sql.ErrNoRows
	}
	return s.GetModel(ctx, id)
}

// GetSetting returns a settings row value, or "" if not present.
func (s *Store) GetSetting(ctx context.Context, key string) string {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return ""
	}
	return v
}

// SetSetting upserts a settings row.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings (key, value) VALUES (?, ?)`, key, value)
	return err
}

// SeedDefaultModel creates the default voyage-4-nano model if the models table
// is empty. Called once at startup to ensure existing deployments have a model
// in the registry. The endpoint and API key come from the global config.
func (s *Store) SeedDefaultModel(ctx context.Context, endpoint, apiKey string) error {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM embedding_models`).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil // already has models
	}
	m, err := s.CreateModel(ctx, EmbeddingModel{
		Name:             "voyage-4-nano (local Q8)",
		Backend:          BackendLlamaQ8,
		ModelName:        "voyage-4-nano",
		Endpoint:         endpoint,
		APIKey:           apiKey,
		Dim:              1024,
		Pooling:          "mean",
		MaxContextTokens: 32000, // voyage-4-nano supports 32K
		ChunkOverlap:     100,
		// ChunkTokens = nil -> auto = min(32000*0.8, 2000) = 2000
	})
	if err != nil {
		return fmt.Errorf("seed default model: %w", err)
	}
	return s.SetActiveModel(ctx, m.ID)
}
