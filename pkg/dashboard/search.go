package dashboard

import (
	"context"
	"errors"
	"fmt"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/rag"
)

// errBackendUnavailable is returned when Qdrant or the embedder is unreachable.
var errBackendUnavailable = errors.New("search backend unavailable (qdrant or embedder offline)")

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
	// Build collection key: {project}_{domain}_{model}_{dim}_{backend}
	// Default domain = code (ADR-0004 phase 1 scope).
	key := rag.CollectionKey{
		Project: project,
		Domain:  rag.DomainCode,
		Model:   s.cfg.ActiveModel,
		Dim:     s.cfg.EmbeddingDim,
		Backend: s.cfg.ActiveBackend,
	}
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
