package dashboard

import (
	"context"
	"errors"
	"fmt"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
)

// errBackendUnavailable is returned when Qdrant or the embedder is unreachable.
var errBackendUnavailable = errors.New("search backend unavailable (qdrant or embedder offline)")

// collectionKeyFor builds the CollectionKey for a project under the active
// model/backend config. Centralised so read paths (search, chunks browser)
// stay aligned with the indexer write path (which uses embed.Model() →
// cfg.EmbeddingModel). Using cfg.ActiveModel here would silently target a
// DIFFERENT collection name — see search.go history.
func (s *Server) collectionKeyFor(project string) rag.CollectionKey {
	return rag.CollectionKey{
		Project: project,
		Domain:  rag.DomainCode,
		Model:   s.cfg.EmbeddingModel,
		Dim:     s.cfg.EmbeddingDim,
		Backend: s.cfg.ActiveBackend,
	}
}

// doSearch embeds the query and searches the project's code collection.
// It constructs the CollectionKey from the active model/backend config (ADR-0001).
func (s *Server) doSearch(ctx context.Context, project, query string, limit int) ([]rag.Result, error) {
	if s.rag == nil || s.embed == nil {
		return nil, errBackendUnavailable
	}
	// Embed the query text.
	vec, err := s.embedOne(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	key := s.collectionKeyFor(project)
	// Ensure collection exists (idempotent).
	if err := s.rag.CreateCollection(ctx, key); err != nil {
		return nil, fmt.Errorf("ensure collection %s: %w", key, err)
	}
	return s.rag.SemanticSearch(ctx, key, vec, limit)
}

// embedOne embeds a single text string, returning the vector.
func (s *Server) embedOne(ctx context.Context, text string) ([]float32, error) {
	vecs, err := s.embed.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embedder returned 0 vectors")
	}
	return vecs[0], nil
}
