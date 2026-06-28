package rag

import (
	"math"
	"sort"
	"strings"
	"unicode"
)

// BM25 vectorizer for code search. Generates sparse vectors from text
// using BM25-style term weighting with code-specific tokenization.
//
// Design (ADR-0008 §2.2):
//   - Code-aware tokenizer: split camelCase, snake_case, kebab-case, dot-separated
//   - Stop words filtered (programming + natural language)
//   - Symbol/name fields weighted higher than content (exact identifier match)
//   - Term hash → uint32 index (deterministic, fixed vocabulary size)
//   - BM25 weight → float32 value
//
// The sparse vector format matches Qdrant's sparse vector schema:
//   {indices: [uint32], values: [float32]}

const (
	// SparseVectorMaxDim is the virtual vocabulary size. Term hashes are
	// taken modulo this value. 65536 (2^16) balances collisions vs memory.
	SparseVectorMaxDim = 65536

	// BM25 params. k1 controls term frequency saturation, b controls
	// document length normalization. Standard values from Robertson/Sparck-Jones.
	bm25K1 = 1.2
	bm25B  = 0.75

	// Weight multipliers for different text fields. Symbol/name fields
	// get higher weight because exact identifier match is the primary
	// use case for code search hybrid. Integer repeat counts.
	weightContent = 1
	weightSymbol  = 3
	weightName    = 3
	weightFile    = 2
)

// codeStopWords are terms that carry little meaning in code search.
// Includes natural language stop words + programming keywords.
var codeStopWords = map[string]bool{
	// Natural language
	"the": true, "a": true, "an": true, "is": true, "are": true,
	"was": true, "were": true, "be": true, "been": true, "being": true,
	"have": true, "has": true, "had": true, "do": true, "does": true,
	"did": true, "will": true, "would": true, "could": true, "should": true,
	"may": true, "might": true, "shall": true, "can": true, "need": true,
	"dare": true, "ought": true, "used": true, "if": true, "then": true,
	"else": true, "when": true, "where": true, "why": true, "how": true,
	"all": true, "any": true, "both": true, "each": true, "few": true,
	"more": true, "most": true, "other": true, "some": true, "such": true,
	"no": true, "nor": true, "not": true, "only": true, "own": true,
	"same": true, "so": true, "than": true, "too": true, "very": true,
	"just": true, "but": true, "for": true, "and": true, "or": true,
	"this": true, "that": true, "these": true, "those": true,
	"i": true, "me": true, "my": true, "we": true, "our": true,
	"you": true, "your": true, "it": true, "its": true, "they": true,
	"them": true, "their": true, "what": true, "which": true, "who": true,
	"of": true, "in": true, "on": true, "at": true, "to": true,
	"from": true, "by": true, "with": true, "about": true, "as": true,
	"into": true, "through": true, "during": true, "before": true,
	"after": true, "above": true, "below": true, "up": true, "down": true,
	"out": true, "off": true, "over": true, "under": true, "again": true,
	// Programming keywords (low semantic value in code search)
	"func": true, "def": true, "var": true, "let": true, "const": true,
	"type": true, "struct": true, "class": true, "interface": true,
	"enum": true, "return": true, "import": true, "package": true,
	"public": true, "private": true, "protected": true, "static": true,
	"void": true, "int": true, "str": true, "string": true, "bool": true,
	"true": true, "false": true, "nil": true, "null": true, "none": true,
	"new": true, "make": true, "self": true, "super": true,
	"case": true, "switch": true, "break": true, "continue": true,
	"goto": true, "try": true, "catch": true, "throw": true, "raise": true,
	"err": true, "error": true, "ctx": true, "context": true,
}

// SparseVector is Qdrant's sparse vector format.
type SparseVector struct {
	Indices []uint32  `json:"indices"`
	Values  []float32 `json:"values"`
}

// BM25Vectorizer generates sparse vectors from text using BM25 weighting.
// It must be fit on a corpus to compute IDF (inverse document frequency).
type BM25Vectorizer struct {
	// docFreq counts how many documents contain each term (for IDF).
	docFreq map[uint32]int
	// docCount is the total number of documents indexed.
	docCount int
	// avgDocLen is the average document length (in tokens).
	avgDocLen float64
}

// NewBM25Vectorizer creates an unfitted vectorizer. Call Fit() before
// Vectorize() to compute IDF statistics.
func NewBM25Vectorizer() *BM25Vectorizer {
	return &BM25Vectorizer{
		docFreq: make(map[uint32]int, 4096),
	}
}

// AvgDocLen returns the average document length (in tokens) computed during
// Fit(). Used for logging/diagnostics.
func (b *BM25Vectorizer) AvgDocLen() float64 {
	return b.avgDocLen
}

// Fit computes IDF statistics from a set of documents. Must be called
// before Vectorize(). Each doc is the concatenation of weighted fields.
func (b *BM25Vectorizer) Fit(docs []string) {
	b.docCount = len(docs)
	totalLen := 0
	for _, doc := range docs {
		tokens := TokenizeCode(doc)
		totalLen += len(tokens)
		seen := make(map[uint32]bool, len(tokens))
		for _, tok := range tokens {
			h := termHash(tok)
			if !seen[h] {
				b.docFreq[h]++
				seen[h] = true
			}
		}
	}
	if b.docCount > 0 {
		b.avgDocLen = float64(totalLen) / float64(b.docCount)
	}
}

// Vectorize generates a BM25-weighted sparse vector from text.
// The text should be the same weighted concatenation used in Fit().
// Returns nil if text produces no valid terms.
func (b *BM25Vectorizer) Vectorize(text string) *SparseVector {
	tokens := TokenizeCode(text)
	if len(tokens) == 0 {
		return nil
	}

	// Term frequency in this document.
	tf := make(map[uint32]float64, len(tokens))
	docLen := float64(len(tokens))
	for _, tok := range tokens {
		h := termHash(tok)
		tf[h]++
	}

	// BM25 weighting.
	indices := make([]uint32, 0, len(tf))
	values := make([]float32, 0, len(tf))
	for h, freq := range tf {
		idf := b.idf(h)
		if idf <= 0 {
			continue // term not in fitted vocab or appears in all docs
		}
		// BM25 term weight: IDF * (tf*(k1+1)) / (tf + k1*(1-b+b*dl/avgdl))
		numerator := freq * (bm25K1 + 1)
		denominator := freq + bm25K1*(1-bm25B+bm25B*docLen/b.avgDocLen)
		weight := idf * numerator / denominator
		if weight > 0 {
			indices = append(indices, h)
			values = append(values, float32(weight))
		}
	}

	if len(indices) == 0 {
		return nil
	}

	// Sort by index (Qdrant expects sorted indices).
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	// Reorder values to match sorted indices.
	sortedValues := make([]float32, len(values))
	for i := range indices {
		sortedValues[i] = values[i]
	}

	return &SparseVector{
		Indices: indices,
		Values:  sortedValues,
	}
}

// idf computes inverse document frequency for a term hash.
// Uses the BM25+ variant: idf = ln((N - df + 0.5) / (df + 0.5) + 1)
// The +1 ensures non-negative IDF (BM25 standard smoothing).
func (b *BM25Vectorizer) idf(termHash uint32) float64 {
	df := b.docFreq[termHash]
	if df == 0 || b.docCount == 0 {
		return 0
	}
	n := float64(b.docCount)
	return math.Log((n - float64(df) + 0.5) / (float64(df) + 0.5) + 1)
}

// BuildSearchSparse generates a sparse vector for a search query.
// Uses the same tokenizer + IDF as indexing, but with uniform term
// frequency (each query term appears once).
func (b *BM25Vectorizer) BuildSearchSparse(query string) *SparseVector {
	tokens := TokenizeCode(query)
	if len(tokens) == 0 {
		return nil
	}

	indices := make([]uint32, 0, len(tokens))
	values := make([]float32, 0, len(tokens))
	for _, tok := range tokens {
		h := termHash(tok)
		idf := b.idf(h)
		if idf <= 0 {
			// Term not in fitted vocab — still index it (query expansion).
			// Give it a small weight so it doesn't dominate.
			idf = 0.1
		}
		indices = append(indices, h)
		values = append(values, float32(idf))
	}

	if len(indices) == 0 {
		return nil
	}

	// Deduplicate (multiple tokens may hash to same index).
	merged := make(map[uint32]float32, len(indices))
	for i, h := range indices {
		merged[h] += values[i]
	}

	sortedIndices := make([]uint32, 0, len(merged))
	for h := range merged {
		sortedIndices = append(sortedIndices, h)
	}
	sort.Slice(sortedIndices, func(i, j int) bool { return sortedIndices[i] < sortedIndices[j] })

	sortedValues := make([]float32, len(sortedIndices))
	for i, h := range sortedIndices {
		sortedValues[i] = merged[h]
	}

	return &SparseVector{
		Indices: sortedIndices,
		Values:  sortedValues,
	}
}

// BuildDocSparse generates a sparse vector for an indexed document,
// combining weighted fields (symbol, name, file, content).
// Returns nil if no valid terms.
func (b *BM25Vectorizer) BuildDocSparse(symbol, name, file, content string) *SparseVector {
	// Weighted concatenation: repeat symbol/name terms to amplify weight.
	// Use integer-rounded weights (repeat count).
	var sb strings.Builder
	if symbol != "" {
		for i := 0; i < int(weightSymbol); i++ {
			sb.WriteString(symbol)
			sb.WriteString(" ")
		}
	}
	if name != "" && name != symbol {
		for i := 0; i < int(weightName); i++ {
			sb.WriteString(name)
			sb.WriteString(" ")
		}
	}
	if file != "" {
		for i := 0; i < int(weightFile); i++ {
			sb.WriteString(file)
			sb.WriteString(" ")
		}
	}
	if content != "" {
		sb.WriteString(content)
	}
	return b.Vectorize(sb.String())
}

// termHash converts a term string to a uint32 index in [0, SparseVectorMaxDim).
// Uses FNV-1a for deterministic, fast, well-distributed hashing.
func termHash(term string) uint32 {
	const (
		offsetBasis uint32 = 2166136261
		prime       uint32 = 16777619
	)
	h := offsetBasis
	for i := 0; i < len(term); i++ {
		h ^= uint32(term[i])
		h *= prime
	}
	return h % SparseVectorMaxDim
}

// TokenizeCode splits text into code-aware tokens.
// Rules (ADR-0008 §2.2):
//   - Split camelCase: parseConfig → parse, config
//   - Split snake_case: load_config → load, config
//   - Split kebab-case: bit-rag → bit, rag
//   - Split on non-alphanumeric: foo.bar() → foo, bar
//   - Lowercase all terms
//   - Filter stop words
//   - Min term length: 2 chars
func TokenizeCode(text string) []string {
	// Phase 1: split on non-alphanumeric (but keep camelCase intact).
	rawTokens := splitOnNonAlnum(text)

	// Phase 2: split camelCase within each raw token.
	var tokens []string
	for _, rt := range rawTokens {
		for _, ct := range splitCamelCase(rt) {
			ct = strings.ToLower(ct)
			if len(ct) >= 2 && !codeStopWords[ct] {
				tokens = append(tokens, ct)
			}
		}
	}
	return tokens
}

// splitOnNonAlnum splits text on any non-alphanumeric character.
// "foo.bar(baz)" → ["foo", "bar", "baz"]
func splitOnNonAlnum(text string) []string {
	var tokens []string
	var current strings.Builder
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(r)
		} else {
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

// splitCamelCase splits a camelCase or PascalCase token into parts.
// "parseConfig" → ["parse", "config"]
// "HTTPServer" → ["http", "server"]
// "parseURLPath" → ["parse", "url", "path"]
// "bit_rag_test" → ["bit", "rag", "test"] (snake_case also split here)
func splitCamelCase(token string) []string {
	// First split on underscore (snake_case).
	parts := strings.Split(token, "_")

	var result []string
	for _, part := range parts {
		if part == "" {
			continue
		}
		result = append(result, splitPascalCamel(part)...)
	}
	return result
}

// splitPascalCamel splits a pure camelCase/PascalCase string.
// "parseConfig" → ["parse", "Config"]
// "HTTPServer" → ["HTTP", "Server"]
// "parseURLPath" → ["parse", "URL", "Path"]
func splitPascalCamel(s string) []string {
	var parts []string
	var current strings.Builder
	prevLower := false

	for i, r := range s {
		isUpper := unicode.IsUpper(r)
		if i > 0 && isUpper && prevLower {
			// Transition lower→upper: split here.
			parts = append(parts, current.String())
			current.Reset()
		}
		current.WriteRune(r)
		prevLower = !isUpper
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}
