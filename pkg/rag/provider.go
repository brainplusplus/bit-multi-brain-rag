// Package rag abstracts vector stores and embedding backends.
//
// Design (ADR-0004): one project = N collections, where N = domains × models.
// Collection key = {project}_{domain}_{model}_{dim}_{backend}.
// Phase 1 only implements the `code` domain with the `llama_q8` backend.
package rag

import "context"

// Domain is the content category of a collection.
type Domain string

const (
	DomainCode Domain = "code"
	DomainDoc  Domain = "doc"  // Phase 2 (future)
	DomainTask Domain = "task" // Phase 3 (future)
)

// Document is a unit of content stored in a RAG collection.
type Document struct {
	ID      string
	Content string
	Meta    map[string]string
}

// Result is a retrieved chunk with its similarity score.
type Result struct {
	ID      string
	Content string
	Score   float64
	Meta    map[string]string
}

// PointInfo is a stored point's ID + source_file metadata (for delta sync).
type PointInfo struct {
	ID         string
	SourceFile string
}

// CollectionKey identifies a single vector collection.
// Convention: {project}_{domain}_{model}_{dim}_{backend}
type CollectionKey struct {
	Project string
	Domain  Domain
	Model   string
	Dim     int
	Backend string
}

// String returns the Qdrant collection name.
func (k CollectionKey) String() string {
	return k.Project + "_" + string(k.Domain) + "_" + k.Model + "_" +
		itoa(k.Dim) + "_" + k.Backend
}

// Provider describes a RAG backend capable of per-collection vector storage.
// One collection = one (project, domain, model, dim, backend) tuple.
type Provider interface {
	// CreateCollection creates the collection if it doesn't exist.
	CreateCollection(ctx context.Context, key CollectionKey) error

	// DeleteCollection removes the collection and all its points.
	DeleteCollection(ctx context.Context, key CollectionKey) error

	// Index inserts pre-embedded documents into the collection.
	// The caller is responsible for embedding (via EmbeddingClient);
	// this method only stores vectors + payload.
	Index(ctx context.Context, key CollectionKey, docs []Document, vectors [][]float32) error

	// SemanticSearch returns the k most relevant chunks for a query vector.
	// The caller embeds the query first (via EmbeddingClient), then passes
	// the vector here. This keeps embedding and storage concerns separate.
	SemanticSearch(ctx context.Context, key CollectionKey, queryVec []float32, limit int) ([]Result, error)

	// DeletePoints removes specific points by ID.
	DeletePoints(ctx context.Context, key CollectionKey, pointIDs []string) error

	// ListPoints returns all points (ID + source_file) in the collection,
	// optionally filtered by metadata. Used for delta sync (stale detection).
	ListPoints(ctx context.Context, key CollectionKey, metaFilter map[string]string) ([]PointInfo, error)

	// Close releases resources.
	Close() error
}

// EmbeddingClient abstracts text embedding models (llama.cpp Q8, ST FP16, etc.).
// One project may use multiple EmbeddingClients (multi-model switching, ADR-0002).
type EmbeddingClient interface {
	// Embed returns vectors for the input texts. Batch for throughput.
	Embed(ctx context.Context, texts []string) ([][]float32, error)

	// VectorSize returns the embedding dimension (e.g. 1024 for voyage_nano_1024).
	VectorSize() int

	// Backend returns the backend identifier (e.g. "llama_q8", "st_fp16").
	Backend() string

	// Model returns the model identifier (e.g. "voyage_nano").
	Model() string
}

// itoa is a dependency-free int→string converter (avoid strconv import in interface file).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
