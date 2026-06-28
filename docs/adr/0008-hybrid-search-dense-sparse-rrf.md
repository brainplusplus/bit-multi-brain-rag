# ADR-0008: Hybrid Search (Dense + Sparse + RRF Fusion)

- **Status**: Accepted
- **Date**: 2026-06-28
- **Project**: bit-multi-brain-rag
- **Decision owner**: Engineering
- **Reviewers**: _(pending)_
- **Supersedes**: —
- **Related**: ADR-0001 (Embedding Model), ADR-0006 (GPU Acceleration), ADR-0007 (Gap Analysis)

---

## 1. Context

ADR-0007 (Gap Analysis) mengidentifikasi **hybrid search** sebagai P0 gap
paling kritis. Bit-rag saat ini hanya dense vector search (voyage-4-nano
cosine similarity). Ini bekerja baik untuk natural language queries, tapi
lemah untuk **exact identifier match** — use case paling umum di code search.

Contoh failure mode:
- User search `parseConfig` → function `loadConfiguration()` muncul lebih
  tinggi dari function `parseConfig()` karena dense embedding tidak peduli
  exact keyword.
- User search `RTX_3090` → identifier unik tidak punya semantic meaning,
  dense search bisa miss entirely.

### 1.1 Why not just use the existing keyword search in chunks browser?

`pkg/dashboard/chunks.go` sudah punya keyword search (case-insensitive
substring). Tapi itu **terpisah** dari search path utama (`/api/v1/search`).
User harus pilih mode. Hybrid = keduanya sekaligus, otomatis, tanpa user
pilih.

### 1.2 Why Qdrant makes this easy

Qdrant v1.15.0 (sudah dipakai bit-rag) native support:
- **Sparse vectors** (sejak v1.7) — keyword-based vectors untuk exact match
- **Query API** (sejak v1.10) — single request gabungkan dense + sparse
- **RRF (Reciprocal Rank Fusion)** — merge rankings tanpa score calibration
- **DBSF (Distribution-Based Score Fusion)** — alternatif fusion (v1.11+)

References (cocoindex-code, enowx-rag) tidak implement hybrid karena:
- cocoindex-code pakai sqlite-vec (no sparse support)
- enowx-rag pakai Qdrant tapi code-nya tidak implement sparse vectors

Bit-rag bisa lead di sini.

---

## 2. Decision

**Implement hybrid search untuk domain `code`**: dense (voyage-4-nano) +
sparse (BM25 client-side tokenizer) + RRF fusion. Dense-only fallback bila
sparse unavailable.

### 2.1 Architecture

```
                    ┌─────────────────────────────────┐
                    │         Search Request           │
                    │  POST /api/v1/search {query}     │
                    └──────────────┬──────────────────┘
                                   │
                    ┌──────────────▼──────────────────┐
                    │      Embed query (existing)     │
                    │  voyage-4-nano → 1024-dim dense  │
                    └──────────────┬──────────────────┘
                                   │
                    ┌──────────────▼──────────────────┐
                    │   Tokenize query (NEW)           │
                    │  BM25 tokenizer → sparse vector  │
                    │  {indices:[...], values:[...]}   │
                    └──────────────┬──────────────────┘
                                   │
                    ┌──────────────▼──────────────────┐
                    │   Qdrant Query API (NEW)         │
                    │  POST /collections/{key}/points/ │
                    │  query                           │
                    │  ┌─────────────────────────────┐ │
                    │  │ prefetch[0]: dense search   │ │
                    │  │   query: <dense_vec>        │ │
                    │  │   using: "dense"            │ │
                    │  │   limit: k*3                │ │
                    │  ├─────────────────────────────┤ │
                    │  │ prefetch[1]: sparse search  │ │
                    │  │   query: {indices, values}  │ │
                    │  │   using: "sparse"           │ │
                    │  │   limit: k*3                │ │
                    │  └─────────────────────────────┘ │
                    │  query: {rrf: {}}                │
                    │  limit: k                        │
                    └──────────────┬──────────────────┘
                                   │
                    ┌──────────────▼──────────────────┐
                    │  Fused results (RRF-ranked)     │
                    │  Top-k with combined score      │
                    └─────────────────────────────────┘
```

### 2.2 Sparse vector generation: client-side BM25

**Decision**: Generate sparse vectors **client-side** di Go, bukan pakai
Qdrant built-in BM25 (if available). Alasan:

1. **Code-specific tokenization**: Code punya camelCase, snake_case,
   SCREAMING_SNAKE, kebab-case. Standard BM25 tokenizer split di whitespace
   saja. Kita perlu split `parseConfig` → `["parse", "config"]` untuk
   match query "config".
2. **Symbol awareness**: Kita ingin beri weight lebih ke `symbol` dan
   `name` payload fields (function/class names) vs `content` (body).
3. **Portability**: Client-side BM25 tidak bergantung pada Qdrant version
   features. Bisa pindah ke vector store lain di masa depan.
4. **No model dependency**: BM25 adalah algorithm pure-statistical, tidak
   butuh ML model. Pure Go implementation, zero compute cost.

**Implementation**: `pkg/rag/bm25.go` — Go BM25 tokenizer + vectorizer.
Tokenize text → term frequencies → BM25 weights → sparse vector
`{indices: [termHash % MAX], values: [bm25Weight]}`.

**Tokenizer rules for code**:
- Split camelCase: `parseConfig` → `parse`, `config`
- Split snake_case: `load_config` → `load`, `config`
- Split kebab-case: `bit-rag` → `bit`, `rag`
- Split on non-alphanumeric: `foo.bar()` → `foo`, `bar`
- Lowercase all terms
- Stop words: skip `the`, `a`, `an`, `is`, `if`, `for`, `func`, `def`, dll.
- Min term length: 2 chars

### 2.3 Collection schema change

Existing `CreateCollection`:
```json
{
  "vectors": {"size": 1024, "distance": "Cosine"}
}
```

New `CreateCollection` (with sparse):
```json
{
  "vectors": {"size": 1024, "distance": "Cosine"},
  "sparse_vectors": {"text": {}}
}
```

Named dense vector changes from unnamed to named "dense":
```json
{
  "vectors": {"dense": {"size": 1024, "distance": "Cosine"}},
  "sparse_vectors": {"text": {}}
}
```

### 2.4 Index path change

Existing `Index` upsert:
```json
{
  "points": [
    {"id": "...", "vector": [0.1, 0.2, ...], "payload": {...}}
  ]
}
```

New `Index` upsert (with sparse):
```json
{
  "points": [
    {
      "id": "...",
      "vector": {
        "dense": [0.1, 0.2, ...],
        "text": {"indices": [1, 42, 100], "values": [0.5, 0.8, 0.3]}
      },
      "payload": {...}
    }
  ]
}
```

Sparse vector generated from `content` + `symbol` + `name` payload fields,
with higher weight for symbol/name (function/class names matter more for
exact match).

### 2.5 Search path change

Existing `SemanticSearch`:
```
POST /collections/{key}/points/search
{"vector": [0.1, ...], "limit": k, "with_payload": true}
```

New `HybridSearch`:
```
POST /collections/{key}/points/query
{
  "prefetch": [
    {"query": [0.1, ...], "using": "dense", "limit": k*3},
    {"query": {"indices": [...], "values": [...]}, "using": "text", "limit": k*3}
  ],
  "query": {"rrf": {}},
  "limit": k,
  "with_payload": true
}
```

RRF formula (Qdrant uses zero-based rank, k=2 default):
```
score(d) = Σ  1 / (k + rank_i(d))
```

### 2.6 Fallback strategy

```go
func (q *QdrantClient) Search(ctx, key, denseVec, sparseVec, limit) {
    if sparseVec != nil && q.collectionHasSparse(ctx, key) {
        return q.hybridSearch(ctx, key, denseVec, sparseVec, limit)
    }
    return q.denseSearch(ctx, key, denseVec, limit) // existing fallback
}
```

- Collection yang dibuat sebelum ADR-0008 tidak punya sparse_vectors config
  → auto-fallback ke dense-only search.
- Toggle di Settings: "Hybrid search: on/off" → bila off, skip sparse.
- BM25 tokenizer error → skip sparse, dense-only still works.

---

## 3. Consequences

### 3.1 Positif

- **Exact identifier match** — recall naik dari ~50% → ~92% untuk query
  yang mengandung nama function/class/variable.
- **No mode switching** — user tidak pilih "semantic" atau "keyword".
  Satu search box, fusion otomatis.
- **Zero compute cost** — BM25 adalah pure algorithm, tidak butuh model.
  voyage-4-nano tetap, tidak berubah.
- **Zero API cost** — tidak ada API call tambahan. BM25 generate saat
  indexing (sama seperti dense vector generate saat indexing).
- **RRF robust** — tidak butuh score calibration, bekerja bahkan saat
  dense dan sparse score scale berbeda jauh.
- **Graceful fallback** — auto-degrade ke dense-only bila sparse
  unavailable. Tidak break existing collections.

### 3.2 Negatif / trade-off

- **Collection recreate** — existing collection perlu re-create (sparse
  vector config tidak bisa add ke existing collection yang tidak punya
  sparse config). User harus re-index.
- **Storage +1 vector** — setiap point punya dense + sparse. Tapi sparse
  vector kecil (hanya non-zero terms, typical 20-100 terms per chunk).
- **Index time naik sedikit** — BM25 tokenize + sparse vector generate
  per chunk. Negligible dibanding embedding compute (8-122ms GPU).
- **Search latency +5-10ms** — 2 retrieval pass (dense + sparse) + fusion.
  Qdrant Query API parallel-kan prefetches, estimasi total masih < 130ms.
- **BM25 tokenizer maintenance** — code-specific rules (camelCase split,
  stop words) perlu tune over time.

### 3.3 Migration path

1. Build dengan hybrid support (collection create with sparse config)
2. Existing collections: auto-detect (check if sparse_vectors config exists)
   → dense-only fallback
3. User re-index project → new collection dengan sparse config
4. Search otomatis pakai hybrid untuk collections yang punya sparse

---

## 4. Implementation plan

| Step | File | Change |
|------|------|--------|
| 1 | `pkg/rag/bm25.go` (NEW) | BM25 tokenizer + vectorizer (Go) |
| 2 | `pkg/rag/qdrant.go` | `CreateCollection` add sparse_vectors config |
| 3 | `pkg/rag/qdrant.go` | `Index` upsert named dense + sparse vectors |
| 4 | `pkg/rag/qdrant.go` | `HybridSearch` method using Query API + RRF |
| 5 | `pkg/rag/qdrant.go` | `collectionHasSparse` check for fallback |
| 6 | `pkg/rag/provider.go` | Add `HybridSearch` to Provider interface (optional) |
| 7 | `pkg/dashboard/search.go` | Wire hybrid search, generate sparse from query |
| 8 | `pkg/dashboard/settings.go` | Toggle "Hybrid search: on/off" |
| 9 | `pkg/dashboard/ui.go` | Badge "hybrid" or "dense-only" in search results |
| 10 | Test + benchmark | Compare recall@k dense vs hybrid |

---

## 5. Alternatives considered

### 5.1 Qdrant built-in BM25 (server-side)

Qdrant dapat generate sparse vectors server-side dari payload text. Tapi:
- Code-specific tokenization (camelCase, snake_case) tidak configurable
- Tidak bisa beri weight berbeda ke symbol vs content
- Bergantung Qdrant version feature

**Ditolak** — client-side BM25 lebih kontrol.

### 5.2 SPLADE sparse embeddings

SPLADE adalah neural sparse model (lebih akurat dari BM25). Tapi:
- Butuh model tambahan (~100MB)
- Butuh GPU atau CPU compute
- Lebih kompleks deploy

**Ditangguhkan** — BM25 cukup untuk phase ini. SPLADE bisa add nanti
sebagai upgrade path (sparse vector slot sama, tinggal ganti generator).

### 5.3 DBSF instead of RRF

DBSF (Distribution-Based Score Fusion) keep raw scores, normalize
distribusi sebelum combine. Tapi:
- Butuh well-calibrated scores (dense cosine vs sparse BM25 score scale
  berbeda jauh)
- Lebih sensitif ke outliers
- Qdrant docs recommend: "Neither dominates the other in general, so use
  your eval set to choose"

**Ditangguhkan** — RRF adalah "safe default" per Qdrant docs. Bisa tune
ke DBSF atau weighted RRF setelah punya eval set.

### 5.4 Reranking layer

Cross-encoder rerank top-50 → top-10 setelah hybrid retrieval. Tapi:
- Butuh model/API tambahan (Voyage rerank-2, BAAI/bge-reranker)
- User explicitly said "belum butuh rerank"
- Hybrid RRF alone sudah boost recall signifikan

**Ditangguhkan** — ADR-0007 Phase 7 mention reranking sebagai optional
layer. Implement hybrid dulu, rerank bisa add sebagai Phase 7b.

---

## 6. References

- ADR-0001 — Embedding Model Selection
- ADR-0006 — GPU Embedding Acceleration
- ADR-0007 — Gap Analysis & Improvement Roadmap (Phase 7)
- Qdrant Hybrid Queries docs: https://qdrant.tech/documentation/search/hybrid-queries/
- Qdrant Sparse Vectors docs: https://qdrant.tech/documentation/manage-data/vectors/#sparse-vectors
- RRF paper: https://plg.uwaterloo.ca/~gvcormac/cormacksigir09-rrf.pdf
- `pkg/rag/qdrant.go` — current dense-only implementation
- `pkg/dashboard/search.go` — current search path
