// Package rag implements the embedding backends and vector store abstraction
// for bit-multi-brain-rag.
//
// The Provider interface abstracts vector stores (Qdrant, Chroma, pgvector).
// The EmbeddingClient interface abstracts embedders (llama.cpp Q8, TEI,
// Voyage cloud). Phase 1 ships a llama.cpp Q8 HTTP embedder; the Qdrant
// provider will be added in a later phase.
package rag
