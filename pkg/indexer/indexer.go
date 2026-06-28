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
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/chunker"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
	"github.com/brainplusplus/bit-multi-brain-rag/pkg/store"
)

// charsPerToken is a conservative chars-to-tokens estimate for code.
// Observed on voyage-4-nano / BPE tokenizers:
//   - Natural language: ~4 chars/token
//   - Code (Go/Python): ~3 chars/token average, down to ~2 for symbol-dense code
// We use 3 to stay conservative — overestimating tokens ensures chunks
// stay under the embedder's ubatch-size limit.
const charsPerToken = 3

// defaultMaxTokensPerChunk is the legacy fallback when the active model has
// no curated MaxContextTokens (and thus EffectiveChunkTokens returns 400).
// New models use per-model chunk sizes via Indexer.MaxTokensPerChunk.
const defaultMaxTokensPerChunk = 400

// Indexer coordinates chunking, embedding, and storage.
type Indexer struct {
	chunk  *chunker.Chunker
	embed  rag.EmbeddingClient
	rag    rag.Provider
	logger *slog.Logger
	store  *store.Store       // optional: for fingerprint-based incremental indexing
	// BatchSize is the number of chunks embedded per HTTP call (throughput).
	BatchSize int
	// MaxTokensPerChunk is the per-model chunk size cap (in tokens). When 0,
	// falls back to defaultMaxTokensPerChunk. Set by the dashboard from the
	// active model's EffectiveChunkTokens() at hot-swap time.
	MaxTokensPerChunk int
	// HybridEnabled enables BM25 sparse vector generation for hybrid search
	// (ADR-0008). When true, indexer generates sparse vectors from
	// symbol+name+file+content and uses IndexWithSparse instead of Index.
	HybridEnabled bool
	// bm25 is the lazy-initialized BM25 vectorizer (fitted on first batch).
	bm25 *rag.BM25Vectorizer
}

// New constructs an Indexer.
func New(c *chunker.Chunker, e rag.EmbeddingClient, r rag.Provider, logger *slog.Logger) *Indexer {
	return &Indexer{
		chunk:             c,
		embed:             e,
		rag:               r,
		logger:            logger,
		BatchSize:         32,
		MaxTokensPerChunk: defaultMaxTokensPerChunk,
	}
}

// WithStore attaches a SQLite store for fingerprint-based incremental indexing.
// When set, the indexer skips files whose SHA-256 hash hasn't changed since
// the last indexing run (ADR-0007 Phase 8).
func (ix *Indexer) WithStore(s *store.Store) *Indexer {
	ix.store = s
	return ix
}

// effectiveMaxTokens returns the configured cap or the legacy default.
func (ix *Indexer) effectiveMaxTokens() int {
	if ix.MaxTokensPerChunk > 0 {
		return ix.MaxTokensPerChunk
	}
	return defaultMaxTokensPerChunk
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

// ProgressEvent is emitted by IndexProjectWithProgress as the indexer makes
// progress through the file tree. All fields are running counters except
// CurrentFile, which is the file most recently entered (relative path).
//
// Emitted at coarse boundaries (per-file enter + per-batch flush) so the
// channel is cheap; UI typically polls a snapshot rather than consuming the
// full stream.
type ProgressEvent struct {
	FilesTotal   int
	FilesDone    int
	ChunksDone   int
	EmbeddedDone int
	IndexedDone  int
	CurrentFile  string
}

// ProgressFn is the callback signature for incremental progress updates.
// It is invoked synchronously from the indexer goroutine — keep it fast
// (a non-blocking map update is the intended use).
type ProgressFn func(ProgressEvent)

// IndexProject is the legacy synchronous entry point. It runs the same
// pipeline as IndexProjectWithProgress with a no-op callback. Kept so the
// existing /api/v1/index JSON handler still compiles during the async
// rollout; new code should prefer the progress-aware variant.
func (ix *Indexer) IndexProject(ctx context.Context, project, rootPath string) (IndexStats, error) {
	return ix.IndexProjectWithProgress(ctx, project, rootPath, 0, nil)
}

// IndexProjectWithProgress walks rootPath, chunks all supported source files,
// embeds them, and upserts into the project's collection. The optional
// progress callback receives ProgressEvent values at each file boundary and
// each batch flush. Returns aggregate stats.
//
// projectID is used for fingerprint-based incremental indexing (0 = disabled).
//
// Cancellation: ctx is checked between file walks and between batches; on
// ctx.Done the function returns the partial stats and ctx.Err().
func (ix *Indexer) IndexProjectWithProgress(ctx context.Context, project, rootPath string, projectID int64, progress ProgressFn) (IndexStats, error) {
	start := time.Now()
	stats := IndexStats{}
	emit := func(current string) {
		if progress == nil {
			return
		}
		progress(ProgressEvent{
			FilesTotal:   stats.FilesScanned,
			FilesDone:    stats.FilesScanned, // updated below per real progress
			ChunksDone:   stats.Chunks,
			EmbeddedDone: stats.Embedded,
			IndexedDone:  stats.Indexed,
			CurrentFile:  current,
		})
	}
	key := rag.CollectionKey{
		Project: project,
		Domain:  rag.DomainCode,
		Model:   ix.embed.Model(),
		Dim:     ix.embed.VectorSize(),
		Backend: ix.embed.Backend(),
	}
	if ix.HybridEnabled {
		// Try hybrid collection (dense + sparse). Falls back to standard
		// if Qdrant doesn't support sparse_vectors.
		if qc, ok := ix.rag.(*rag.QdrantClient); ok {
			if err := qc.CreateCollectionWithSparse(ctx, key); err != nil {
				ix.logger.Warn("hybrid collection create failed, falling back to dense-only", "error", err)
				if err := ix.rag.CreateCollection(ctx, key); err != nil {
					return stats, fmt.Errorf("create collection %s: %w", key, err)
				}
				ix.HybridEnabled = false
			}
		} else {
			if err := ix.rag.CreateCollection(ctx, key); err != nil {
				return stats, fmt.Errorf("create collection %s: %w", key, err)
			}
			ix.HybridEnabled = false
		}
	} else {
		if err := ix.rag.CreateCollection(ctx, key); err != nil {
			return stats, fmt.Errorf("create collection %s: %w", key, err)
		}
	}

	// Walk the tree, collecting source files.
	type fileBatch struct {
		path string
		data []byte
	}
	var files []fileBatch
	gi := newGitignoreMatcher()
	depth := 0
	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("walk %s: %v", path, err))
			return nil // continue walking
		}
		rel, _ := filepath.Rel(rootPath, path)
		if d.IsDir() {
			// Track depth for gitignore pattern scoping.
			if path != rootPath {
				depth++
				gi.loadDir(path, depth)
			}
			// Skip common non-source dirs.
			name := d.Name()
			if shouldSkipDir(name) {
				gi.pop(depth)
				depth--
				return filepath.SkipDir
			}
			// Skip if .gitignore matches this directory.
			if gi.match(rel, true) {
				gi.pop(depth)
				depth--
				return filepath.SkipDir
			}
			return nil
		}
		// Skip if .gitignore matches this file.
		if gi.match(rel, false) {
			return nil
		}
		if !isSourceFile(path) {
			return nil
		}
		data, rerr := readFile(path)
		if rerr != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("read %s: %v", rel, rerr))
			return nil
		}
		// Incremental: skip unchanged files via SHA-256 fingerprint (ADR-0007 Phase 8).
		if ix.store != nil && projectID > 0 {
			hash := sha256Hex(data)
			if fp, _ := ix.store.GetFingerprint(ctx, projectID, rel); fp != nil && fp.SHA256 == hash {
				stats.Skipped++
				return nil // file unchanged, skip
			}
		}
		files = append(files, fileBatch{path: rel, data: data})
		stats.FilesScanned++
		return nil
	})
	if err != nil {
		return stats, fmt.Errorf("walk %s: %w", rootPath, err)
	}

	// Stale detection: delete Qdrant points for files no longer present.
	if ix.store != nil && projectID > 0 {
		keepPaths := make([]string, len(files))
		for i, f := range files {
			keepPaths[i] = f.path
		}
		removed, _ := ix.store.DeleteFingerprintsExcept(ctx, projectID, keepPaths)
		if len(removed) > 0 {
			ix.logger.Info("stale files detected, deleting points", "count", len(removed))
			ix.deletePointsByFiles(ctx, key, removed)
			stats.Skipped += len(removed) // report as skipped (stale removed)
		}
	}

	// Chunk all files, accumulating documents + vectors in batches.
	type pending struct {
		doc       rag.Document
		filePath  string // for fingerprint tracking
		fileHash  string // SHA-256 of file content
	}
	var batch []pending
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		// Extract docs, split oversized at line boundaries to stay below
		// the embedder's per-input token budget.
		rawDocs := make([]rag.Document, len(batch))
		for i, p := range batch {
			rawDocs[i] = p.doc
		}
		docs := splitOversized(project, rawDocs, ix.effectiveMaxTokens())
		texts := make([]string, len(docs))
		for i, d := range docs {
			texts[i] = d.Content
		}
		vecs, err := ix.embed.Embed(ctx, texts)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("embed batch: %v", err))
			batch = batch[:0]
			return nil
		}
		// Track point count per file for fingerprint storage.
		filePointCounts := make(map[string]int)
		if ix.HybridEnabled {
			if qc, ok := ix.rag.(*rag.QdrantClient); ok {
				// Generate BM25 sparse vectors for hybrid search (ADR-0008).
				// Fit BM25 on this batch (IDF from batch docs — good enough
				// approximation for relative weighting within the index).
				ix.ensureBM25(texts)
				sparseVecs := make([]*rag.SparseVector, len(docs))
				for i, d := range docs {
					sym := d.Meta["symbol"]
					nm := d.Meta["name"]
					f := d.Meta["source_file"]
					sparseVecs[i] = ix.bm25.BuildDocSparse(sym, nm, f, d.Content)
				}
				if err := qc.IndexWithSparse(ctx, key, docs, vecs, sparseVecs); err != nil {
					// Fallback to dense-only on sparse index error.
					ix.logger.Warn("hybrid index failed, falling back to dense-only", "error", err)
					if err := ix.rag.Index(ctx, key, docs, vecs); err != nil {
						stats.Errors = append(stats.Errors, fmt.Sprintf("index batch: %v", err))
					} else {
						stats.Indexed += len(docs)
						stats.Embedded += len(vecs)
					}
				} else {
					stats.Indexed += len(docs)
					stats.Embedded += len(vecs)
				}
			} else {
				if err := ix.rag.Index(ctx, key, docs, vecs); err != nil {
					stats.Errors = append(stats.Errors, fmt.Sprintf("index batch: %v", err))
				} else {
					stats.Indexed += len(docs)
					stats.Embedded += len(vecs)
				}
			}
		} else {
			if err := ix.rag.Index(ctx, key, docs, vecs); err != nil {
				stats.Errors = append(stats.Errors, fmt.Sprintf("index batch: %v", err))
			} else {
				stats.Indexed += len(docs)
				stats.Embedded += len(vecs)
			}
		}
		// Record fingerprints for successfully indexed files (incremental, ADR-0007 Phase 8).
		if ix.store != nil && projectID > 0 {
			// Count points per file from the batch.
			for _, p := range batch {
				filePointCounts[p.filePath]++
			}
			for _, p := range batch {
				if p.filePath != "" && p.fileHash != "" {
					if err := ix.store.SetFingerprint(ctx, projectID, p.filePath, p.fileHash, filePointCounts[p.filePath]); err != nil {
						ix.logger.Warn("fingerprint save failed", "file", p.filePath, "error", err)
					}
				}
			}
		}
		batch = batch[:0]
		return nil
	}

	for i, f := range files {
		// Respect cancellation between files. Embed batch + qdrant upsert are
		// short bursts; checking here avoids cancelling mid-batch (which would
		// orphan partial points but is otherwise idempotent on retry).
		if err := ctx.Err(); err != nil {
			stats.Duration = time.Since(start)
			return stats, err
		}
		// Emit "now starting file i+1/N: path" so the UI can show live status.
		if progress != nil {
			progress(ProgressEvent{
				FilesTotal:   len(files),
				FilesDone:    i,
				ChunksDone:   stats.Chunks,
				EmbeddedDone: stats.Embedded,
				IndexedDone:  stats.Indexed,
				CurrentFile:  f.path,
			})
		}
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
			batch = append(batch, pending{doc: doc, filePath: f.path, fileHash: sha256Hex(f.data)})
			if len(batch) >= ix.BatchSize {
				_ = flush()
				// After each flush, surface fresh embed/index counters.
				if progress != nil {
					progress(ProgressEvent{
						FilesTotal:   len(files),
						FilesDone:    i,
						ChunksDone:   stats.Chunks,
						EmbeddedDone: stats.Embedded,
						IndexedDone:  stats.Indexed,
						CurrentFile:  f.path,
					})
				}
			}
		}
	}
	_ = flush()

	stats.Duration = time.Since(start)
	// Final emit lets the UI render "complete" without another poll round-trip.
	emit("")
	ix.logger.Info("index complete",
		"project", project, "files", stats.FilesScanned,
		"chunks", stats.Chunks, "indexed", stats.Indexed,
		"duration", stats.Duration, "errors", len(stats.Errors),
	)
	return stats, nil
}

// pointID generates a deterministic, collision-free point ID as a UUID v5.
// Qdrant only accepts unsigned ints or RFC-4122 UUIDs as point IDs; hex
// digests without dashes (e.g. raw SHA fragments) are rejected with 400.
// UUID v5 over (project,file,line) is deterministic, so re-indexing the
// same chunk overwrites in place (idempotent).
func pointID(project, file string, line int) string {
	return uuid.NewSHA1(uuid.NameSpaceURL,
		[]byte(fmt.Sprintf("%s:%s:%d", project, file, line))).String()
}

// splitOversized splits any document whose content exceeds the embedder's
// per-input token budget into multiple sub-documents at line boundaries.
// Sub-documents preserve metadata (source_file, language, symbol, name) but
// get adjusted start_line/end_line ranges and unique IDs via the line offset.
//
// maxTokens is the per-model token cap (typically from
// store.EmbeddingModel.EffectiveChunkTokens()). charsPerToken is a
// conservative chars-to-tokens estimate.
//
// If a chunk is below threshold, it passes through unchanged.
func splitOversized(project string, docs []rag.Document, maxTokens int) []rag.Document {
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokensPerChunk
	}
	maxChars := maxTokens * charsPerToken
	out := make([]rag.Document, 0, len(docs))
	for _, d := range docs {
		if len(d.Content) <= maxChars {
			out = append(out, d)
			continue
		}
		// Walk lines; flush when adding the next line would exceed maxChars.
		startLine := atoiSafe(d.Meta["start_line"])
		curLine := startLine
		var buf strings.Builder
		lines := strings.Split(d.Content, "\n")
		flushSub := func(endLine int) {
			if buf.Len() == 0 {
				return
			}
			sub := d
			sub.Content = buf.String()
			// Override line range + regenerate ID for this sub-chunk.
			sub.Meta = cloneMeta(d.Meta)
			sub.Meta["start_line"] = fmt.Sprintf("%d", startLine)
			sub.Meta["end_line"] = fmt.Sprintf("%d", endLine)
			sub.ID = pointID(project, d.Meta["source_file"], startLine)
			out = append(out, sub)
			buf.Reset()
		}
		for _, ln := range lines {
			// Hard-split single lines that exceed maxChars on their own
			// (minified/generated code). Splits at rune boundaries.
			if len(ln) > maxChars {
				// Flush any accumulated buffer first.
				if buf.Len() > 0 {
					flushSub(curLine - 1)
					startLine = curLine
				}
				// Split the long line into maxChars-sized pieces.
				remaining := ln
				for len(remaining) > maxChars {
					// Find a safe split point (don't break multibyte runes).
					cut := maxChars
					for cut > 0 && !utf8.RuneStart(remaining[cut]) {
						cut--
					}
					piece := remaining[:cut]
					remaining = remaining[cut:]
					sub := d
					sub.Content = piece
					sub.Meta = cloneMeta(d.Meta)
					sub.Meta["start_line"] = fmt.Sprintf("%d", curLine)
					sub.Meta["end_line"] = fmt.Sprintf("%d", curLine)
					sub.ID = pointID(project, d.Meta["source_file"], curLine)
					out = append(out, sub)
					startLine = curLine
				}
				if len(remaining) > 0 {
					buf.WriteString(remaining)
					buf.WriteByte('\n')
				}
				curLine++
				continue
			}
			if buf.Len()+len(ln)+1 > maxChars && buf.Len() > 0 {
				flushSub(curLine - 1)
				startLine = curLine
			}
			buf.WriteString(ln)
			buf.WriteByte('\n')
			curLine++
		}
		flushSub(curLine - 1)
	}
	// Final safety pass: any chunk still > maxChars (e.g. pathological
	// multibyte content) gets truncated to the limit. Better to index a
	// truncated chunk than to fail the entire batch.
	for i, d := range out {
		if len(d.Content) > maxChars {
			// Truncate at rune boundary.
			cut := maxChars
			for cut > 0 && !utf8.RuneStart(d.Content[cut]) {
				cut--
			}
			out[i].Content = d.Content[:cut]
		}
	}
	return out
}

func atoiSafe(s string) int {
	var n int
	_, _ = fmt.Sscanf(s, "%d", &n)
	if n <= 0 {
		n = 1
	}
	return n
}

func cloneMeta(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
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

// sourceExts lists file extensions we attempt indexing on.
// AST-aware chunking is used for code languages; the rest fall back to
// naive line-based chunking (chunker.chunkNaive).
var sourceExts = map[string]bool{
	// AST-aware (tree-sitter):
	".go": true, ".py": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".ts": true, ".rs": true, ".java": true, ".cs": true,
	".cpp": true, ".cc": true, ".cxx": true, ".hpp": true, ".h": true, ".hh": true, ".hxx": true,
	// AST-aware (Jalur B additions):
	".rb": true, ".php": true, ".sh": true, ".bash": true, ".sql": true,
	// Naive chunking (text/config/docs):
	".md": true, ".rst": true, ".txt": true,
	".json": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".cfg": true,
	".tf": true, ".hcl": true,
	".dockerfile": true,
	".proto": true, ".graphql": true, ".gql": true,
	".html": true, ".css": true, ".scss": true, ".less": true,
	".xml": true, ".svg": true,
	".lua": true, ".r": true, ".dart": true, ".swift": true, ".kt": true, ".scala": true,
	".clj": true, ".ex": true, ".exs": true, ".erl": true, ".hs": true, ".ml": true,
	".vim": true, ".ps1": true, ".bat": true, ".cmd": true,
	".makefile": true, ".cmake": true,
	".env": true, ".gitignore": true, ".editorconfig": true,
}

// isSourceFile returns true if the file should be indexed.
// Matches by extension OR by basename (for extensionless files like Dockerfile, Makefile).
func isSourceFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if sourceExts[ext] {
		return true
	}
	// Extensionless files with known basenames.
	base := strings.ToLower(filepath.Base(path))
	switch base {
	case "dockerfile", "makefile", "rakefile", "gemfile", "brewfile", "procfile",
		".dockerignore", ".gitignore", ".npmignore", ".editorconfig", ".env":
		return true
	}
	return false
}

// ensureBM25 lazily initializes + fits the BM25 vectorizer on the first batch.
// Subsequent calls are no-ops (already fitted). IDF stats from the first batch
// are a good approximation for relative term weighting within the index.
func (ix *Indexer) ensureBM25(texts []string) {
	if ix.bm25 != nil {
		return
	}
	ix.bm25 = rag.NewBM25Vectorizer()
	ix.bm25.Fit(texts)
	ix.logger.Info("BM25 vectorizer fitted", "docs", len(texts), "avg_doc_len", ix.bm25.AvgDocLen())
}

// sha256Hex returns the hex-encoded SHA-256 hash of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// deletePointsByFiles removes Qdrant points for files no longer present.
// Uses ListPoints to find points with source_file matching removed paths.
func (ix *Indexer) deletePointsByFiles(ctx context.Context, key rag.CollectionKey, removedPaths []string) {
	// Scroll all points and find those whose source_file matches.
	points, err := ix.rag.ListPoints(ctx, key, nil)
	if err != nil {
		ix.logger.Warn("stale cleanup: ListPoints failed", "error", err)
		return
	}
	removed := make(map[string]bool, len(removedPaths))
	for _, p := range removedPaths {
		removed[p] = true
	}
	var toDelete []string
	for _, pt := range points {
		if removed[pt.Meta["source_file"]] {
			toDelete = append(toDelete, pt.ID)
		}
	}
	if len(toDelete) > 0 {
		if err := ix.rag.DeletePoints(ctx, key, toDelete); err != nil {
			ix.logger.Warn("stale cleanup: DeletePoints failed", "error", err, "count", len(toDelete))
		} else {
			ix.logger.Info("stale points deleted", "count", len(toDelete))
		}
	}
}
