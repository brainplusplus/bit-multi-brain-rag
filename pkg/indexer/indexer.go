// Package indexer orchestrates the chunk → embed → store pipeline.
//
// Given a project root path, it walks source files, chunks each via the AST
// chunker (ADR-0004), batches the texts through the embedding client, and
// upserts points into the project's Qdrant collection.
//
// Indexing is the write path; search (pkg/dashboard) is the read path.
// Both share the same CollectionKey convention: {project}_{domain}_{model}_{dim}_{backend}.
package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/chunker"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
)

// Indexer coordinates chunking, embedding, and storage.
type Indexer struct {
	chunk  *chunker.Chunker
	embed  rag.EmbeddingClient
	rag    rag.Provider
	logger *slog.Logger
	// BatchSize is the number of chunks embedded per HTTP call (throughput).
	BatchSize int
}

// New constructs an Indexer.
func New(c *chunker.Chunker, e rag.EmbeddingClient, r rag.Provider, logger *slog.Logger) *Indexer {
	return &Indexer{
		chunk:     c,
		embed:     e,
		rag:       r,
		logger:    logger,
		BatchSize: 32,
	}
}

// IndexStats reports the result of an indexing run.
type IndexStats struct {
	FilesScanned int
	Chunks       int
	Embedded     int
	Indexed      int
	Skipped      int
	Duration     time.Duration
	Errors       []string
}

// IndexProject walks rootPath, chunks all supported source files, embeds them,
// and upserts into the project's collection. Returns aggregate stats.
func (ix *Indexer) IndexProject(ctx context.Context, project, rootPath string) (IndexStats, error) {
	start := time.Now()
	stats := IndexStats{}
	key := rag.CollectionKey{
		Project: project,
		Domain:  rag.DomainCode,
		Model:   ix.embed.Model(),
		Dim:     ix.embed.VectorSize(),
		Backend: ix.embed.Backend(),
	}
	if err := ix.rag.CreateCollection(ctx, key); err != nil {
		return stats, fmt.Errorf("create collection %s: %w", key, err)
	}

	// Walk the tree, collecting source files.
	type fileBatch struct {
		path string
		data []byte
	}
	var files []fileBatch
	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("walk %s: %v", path, err))
			return nil // continue walking
		}
		if d.IsDir() {
			// Skip common non-source dirs.
			name := d.Name()
			if shouldSkipDir(name) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isSourceFile(path) {
			return nil
		}
		rel, _ := filepath.Rel(rootPath, path)
		data, rerr := readFile(path)
		if rerr != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("read %s: %v", rel, rerr))
			return nil
		}
		files = append(files, fileBatch{path: rel, data: data})
		stats.FilesScanned++
		return nil
	})
	if err != nil {
		return stats, fmt.Errorf("walk %s: %w", rootPath, err)
	}

	// Chunk all files, accumulating documents + vectors in batches.
	type pending struct {
		doc rag.Document
	}
	var batch []pending
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		texts := make([]string, len(batch))
		for i, p := range batch {
			texts[i] = p.doc.Content
		}
		vecs, err := ix.embed.Embed(ctx, texts)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("embed batch: %v", err))
			batch = batch[:0]
			return nil
		}
		docs := make([]rag.Document, len(batch))
		for i, p := range batch {
			docs[i] = p.doc
		}
		if err := ix.rag.Index(ctx, key, docs, vecs); err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("index batch: %v", err))
		} else {
			stats.Indexed += len(docs)
			stats.Embedded += len(vecs)
		}
		batch = batch[:0]
		return nil
	}

	for _, f := range files {
		chunks, err := ix.chunk.ChunkFile(ctx, f.data, f.path)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("chunk %s: %v", f.path, err))
			continue
		}
		for _, c := range chunks {
			stats.Chunks++
			doc := rag.Document{
				ID:      pointID(project, f.path, c.StartLine),
				Content: c.Content,
				Meta: map[string]string{
					"source_file": f.path,
					"language":    c.Language,
					"symbol":      c.Symbol,
					"name":        c.Name,
					"start_line":  fmt.Sprintf("%d", c.StartLine),
					"end_line":    fmt.Sprintf("%d", c.EndLine),
					"project":     project,
				},
			}
			batch = append(batch, pending{doc: doc})
			if len(batch) >= ix.BatchSize {
				_ = flush()
			}
		}
	}
	_ = flush()

	stats.Duration = time.Since(start)
	ix.logger.Info("index complete",
		"project", project, "files", stats.FilesScanned,
		"chunks", stats.Chunks, "indexed", stats.Indexed,
		"duration", stats.Duration, "errors", len(stats.Errors),
	)
	return stats, nil
}

// pointID generates a deterministic, collision-free point ID:
// sha256(project:file:line)[:16]. Deterministic IDs enable idempotent re-indexing.
func pointID(project, file string, line int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", project, file, line)))
	return hex.EncodeToString(h[:8])
}

// shouldSkipDir returns true for VCS/build/dependency directories.
func shouldSkipDir(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", "node_modules", "vendor", "dist", "build",
		"target", "__pycache__", ".venv", "venv", ".idea", ".vscode", "bin":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// sourceExts lists file extensions we attempt AST chunking on.
var sourceExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".ts": true, ".rs": true, ".java": true, ".cs": true,
	".cpp": true, ".cc": true, ".cxx": true, ".hpp": true, ".h": true, ".hh": true, ".hxx": true,
}

func isSourceFile(path string) bool {
	return sourceExts[strings.ToLower(filepath.Ext(path))]
}
