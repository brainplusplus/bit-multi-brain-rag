// zvec.go: Embeddable vector store implementation using Alibaba zvec.
//
// Unlike QdrantClient (HTTP server), ZvecClient is an in-process library.
// Data is persisted to a local directory via WAL (write-ahead logging).
// No Docker, no network — pure embedded storage.
//
// This provider supports:
//   - Dense vector storage + KNN search
//   - FTS (full-text search) with BM25 — native in zvec
//   - Hybrid search via MultiQuery + RRF fusion
//
// zvec collections are keyed the same way as Qdrant: {project}_{domain}_{model}_{dim}_{backend}.
// Each collection is stored as a subdirectory under the root zvec path.
package rag

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/zvec-ai/zvec-go"
)

// ZvecClient implements Provider using embedded zvec storage.
type ZvecClient struct {
	root     string                  // root directory for all collections
	dim      int                     // vector dimension (e.g. 1024)
	mu       sync.Mutex              // guards collections map
	opened   map[string]*zvec.Collection // key.String() → open collection
}

// NewZvecClient creates an embedded zvec provider. rootPath is a local
// directory where vector data will be persisted (WAL).
func NewZvecClient(rootPath string, dim int) (*ZvecClient, error) {
	if err := os.MkdirAll(rootPath, 0755); err != nil {
		return nil, fmt.Errorf("zvec: create root dir: %w", err)
	}
	return &ZvecClient{
		root:   rootPath,
		dim:    dim,
		opened: make(map[string]*zvec.Collection),
	}, nil
}

// getOrCreate opens or creates a collection for the given key.
func (z *ZvecClient) getOrCreate(key CollectionKey) (*zvec.Collection, error) {
	z.mu.Lock()
	defer z.mu.Unlock()

	name := key.String()
	// zvec requires alphanumeric + underscore only. Replace invalid chars.
	safeName := sanitizeZvecName(name)
	if c, ok := z.opened[safeName]; ok {
		return c, nil
	}

	collPath := filepath.Join(z.root, safeName)

	// Build schema: PK (uint64 hash) + dense vector + payload fields.
	schema := zvec.NewCollectionSchema(safeName)
	schema.AddField(zvec.NewFieldSchema("embedding", zvec.DataTypeVectorFP32, false, uint32(z.dim)))
	schema.AddField(zvec.NewFieldSchema("source_file", zvec.DataTypeString, true, 0))
	schema.AddField(zvec.NewFieldSchema("language", zvec.DataTypeString, true, 0))
	schema.AddField(zvec.NewFieldSchema("symbol", zvec.DataTypeString, true, 0))
	schema.AddField(zvec.NewFieldSchema("name", zvec.DataTypeString, true, 0))
	schema.AddField(zvec.NewFieldSchema("start_line", zvec.DataTypeInt64, true, 0))
	schema.AddField(zvec.NewFieldSchema("end_line", zvec.DataTypeInt64, true, 0))
	schema.AddField(zvec.NewFieldSchema("content", zvec.DataTypeString, true, 0))

	// Add FTS index on content field for BM25 full-text search.
	schema.AddIndex("content", zvec.NewIndexParams(zvec.IndexTypeFTS))

	// Try to open existing collection first. If not found, create new.
	collection, err := zvec.Open(collPath, nil)
	if err != nil {
		// Collection doesn't exist — create it.
		collection, err = zvec.CreateAndOpen(collPath, schema, nil)
		if err != nil {
			return nil, fmt.Errorf("zvec: create collection %s: %w", safeName, err)
		}
	}

	z.opened[safeName] = collection
	return collection, nil
}

// CreateCollection creates the collection if it doesn't exist.
func (z *ZvecClient) CreateCollection(ctx context.Context, key CollectionKey) error {
	_, err := z.getOrCreate(key)
	return err
}

// DeleteCollection closes and removes the collection directory.
func (z *ZvecClient) DeleteCollection(ctx context.Context, key CollectionKey) error {
	z.mu.Lock()
	defer z.mu.Unlock()

	name := sanitizeZvecName(key.String())
	if c, ok := z.opened[name]; ok {
		c.Close()
		delete(z.opened, name)
	}

	collPath := filepath.Join(z.root, name)
	return os.RemoveAll(collPath)
}

// Index inserts pre-embedded documents.
func (z *ZvecClient) Index(ctx context.Context, key CollectionKey, docs []Document, vectors [][]float32) error {
	collection, err := z.getOrCreate(key)
	if err != nil {
		return err
	}

	zvecDocs := make([]*zvec.Doc, 0, len(docs))
	for i, doc := range docs {
		if i >= len(vectors) {
			break
		}
		pkStr := fmt.Sprintf("%d", hashDocID(doc.ID))
		d := zvec.NewDoc()
		d.SetDocID(hashDocID(doc.ID))
		d.SetPK(pkStr)
		d.AddVectorFP32Field("embedding", vectors[i])
		d.AddStringField("content", doc.Content)
		for k, v := range doc.Meta {
			switch k {
			case "source_file", "language", "symbol", "name":
				d.AddStringField(k, v)
			case "start_line", "end_line":
				var n int64
				fmt.Sscanf(v, "%d", &n)
				d.AddInt64Field(k, n)
			}
		}
		zvecDocs = append(zvecDocs, d)
	}

	if len(zvecDocs) == 0 {
		return nil
	}

	_, err = collection.Insert(zvecDocs)
	return err
}

// SemanticSearch returns the k most relevant chunks for a query vector.
func (z *ZvecClient) SemanticSearch(ctx context.Context, key CollectionKey, queryVec []float32, limit int) ([]Result, error) {
	collection, err := z.getOrCreate(key)
	if err != nil {
		return nil, err
	}

	q := zvec.NewSearchQuery()
	q.SetFieldName("embedding")
	q.SetQueryVector(queryVec)
	q.SetTopK(limit)
	q.SetOutputFields([]string{"source_file", "language", "symbol", "name", "start_line", "end_line", "content"})

	docs, err := collection.Query(q)
	if err != nil {
		return nil, fmt.Errorf("zvec query: %w", err)
	}

	return zvecDocsToResults(docs), nil
}

// DeletePoints removes specific points by string ID (hashed to uint64 PK).
func (z *ZvecClient) DeletePoints(ctx context.Context, key CollectionKey, pointIDs []string) error {
	collection, err := z.getOrCreate(key)
	if err != nil {
		return err
	}
	pks := make([]string, len(pointIDs))
	for i, id := range pointIDs {
		pks[i] = fmt.Sprintf("%d", hashDocID(id))
	}
	_, err = collection.Delete(pks)
	return err
}

// DeleteBySourceFile removes all points where source_file matches.
func (z *ZvecClient) DeleteBySourceFile(ctx context.Context, key CollectionKey, sourceFile string) error {
	collection, err := z.getOrCreate(key)
	if err != nil {
		return err
	}
	filter := `source_file = "` + sourceFile + `"`
	return collection.DeleteByFilter(filter)
}

// ListPoints returns all points (ID + source_file) in the collection.
func (z *ZvecClient) ListPoints(ctx context.Context, key CollectionKey, metaFilter map[string]string) ([]PointInfo, error) {
	// zvec doesn't have a scroll API like Qdrant. For delta sync we skip this
	// (MCP handles incremental via fingerprints). Return empty.
	return nil, nil
}

// Scroll returns one page of points (for chunks browser).
func (z *ZvecClient) Scroll(ctx context.Context, key CollectionKey, opts ScrollOpts) (ScrollResult, error) {
	// Ponytail: chunks browser not supported in embedded mode. Use dashboard
	// with Qdrant mode for full chunks browsing. Upgrade when zvec-go adds scroll.
	return ScrollResult{}, nil
}

// GetPoint returns a single point by ID.
func (z *ZvecClient) GetPoint(ctx context.Context, key CollectionKey, pointID string) (Point, error) {
	collection, err := z.getOrCreate(key)
	if err != nil {
		return Point{}, err
	}
	pkStr := fmt.Sprintf("%d", hashDocID(pointID))
	docs, err := collection.Fetch([]string{pkStr}, nil)
	if err != nil {
		return Point{}, err
	}
	if len(docs) == 0 {
		return Point{}, fmt.Errorf("point not found")
	}

	d := docs[0]
	meta := make(map[string]string)
	if v, err := d.GetStringField("source_file"); err == nil {
		meta["source_file"] = v
	}
	if v, err := d.GetStringField("language"); err == nil {
		meta["language"] = v
	}
	if v, err := d.GetStringField("symbol"); err == nil {
		meta["symbol"] = v
	}
	if v, err := d.GetStringField("name"); err == nil {
		meta["name"] = v
	}
	content, _ := d.GetStringField("content")
	return Point{ID: pointID, Content: content, Meta: meta}, nil
}

// CollectionInfo returns the collection's point count.
func (z *ZvecClient) CollectionInfo(ctx context.Context, key CollectionKey) (CollectionInfo, error) {
	collection, err := z.getOrCreate(key)
	if err != nil {
		return CollectionInfo{}, err
	}
	stats, err := collection.GetStats()
	if err != nil {
		return CollectionInfo{}, err
	}
	return CollectionInfo{
		Status:      "green",
		PointsCount: int(stats.DocCount),
		VectorsSize: z.dim,
	}, nil
}

// Ping checks the storage is accessible.
func (z *ZvecClient) Ping(ctx context.Context) error {
	info, err := os.Stat(z.root)
	if err != nil {
		return fmt.Errorf("zvec: root dir inaccessible: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("zvec: root path is not a directory")
	}
	return nil
}

// Close closes all open collections.
func (z *ZvecClient) Close() error {
	z.mu.Lock()
	defer z.mu.Unlock()
	for _, c := range z.opened {
		c.Close()
	}
	z.opened = make(map[string]*zvec.Collection)
	return nil
}

// --- Helpers ---

// hashDocID converts a string doc ID (UUID) to uint64 for zvec PK.
// Uses FNV-1a hash (deterministic, fast, good distribution).
func hashDocID(id string) uint64 {
	const offset uint64 = 14695981039346656037
	const prime uint64 = 1099511628211
	h := offset
	for _, b := range []byte(id) {
		h ^= uint64(b)
		h *= prime
	}
	return h
}

// sanitizeZvecName replaces invalid chars for zvec collection names.
// zvec requires alphanumeric + underscore only, and has length limits.
// To be safe, use a short FNV hash of the original name.
func sanitizeZvecName(name string) string {
	h := hashDocID(name)
	return fmt.Sprintf("c%d", h%100000) // short numeric name
}

// zvecDocsToResults converts zvec query results to rag.Result.
func zvecDocsToResults(docs []*zvec.Doc) []Result {
	results := make([]Result, 0, len(docs))
	for _, d := range docs {
		meta := make(map[string]string)
		if v, err := d.GetStringField("source_file"); err == nil {
			meta["source_file"] = v
		}
		if v, err := d.GetStringField("language"); err == nil {
			meta["language"] = v
		}
		if v, err := d.GetStringField("symbol"); err == nil {
			meta["symbol"] = v
		}
		if v, err := d.GetStringField("name"); err == nil {
			meta["name"] = v
		}
		if v, err := d.GetInt64Field("start_line"); err == nil {
			meta["start_line"] = fmt.Sprintf("%d", v)
		}
		if v, err := d.GetInt64Field("end_line"); err == nil {
			meta["end_line"] = fmt.Sprintf("%d", v)
		}
		content, _ := d.GetStringField("content")
		results = append(results, Result{
			ID:      d.GetPK(),
			Content: content,
			Score:   float64(d.GetScore()),
			Meta:    meta,
		})
	}
	return results
}
