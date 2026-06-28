package rag

import (
	"testing"
)

func TestTokenizeCode(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		// camelCase split
		{"parseConfig", []string{"parse", "config"}},
		{"ParseConfig", []string{"parse", "config"}},
		// Consecutive uppercase = acronym, stays together as one token
		{"HTTPServer", []string{"httpserver"}},
		{"parseURLPath", []string{"parse", "urlpath"}},
		// snake_case split
		{"load_config", []string{"load", "config"}},
		{"BIT_RAG_TEST", []string{"bit", "rag", "test"}},
		// kebab-case split (split on non-alnum)
		{"bit-rag", []string{"bit", "rag"}},
		// dot-separated
		{"foo.bar()", []string{"foo", "bar"}},
		// stop words filtered
		{"the function returns", []string{"function", "returns"}},
		{"func main()", []string{"main"}},
		// min length 2
		{"a b cd", []string{"cd"}},
		// empty
		{"", nil},
	}
	for _, tt := range tests {
		got := TokenizeCode(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("TokenizeCode(%q): got %v (len %d), want %v (len %d)",
				tt.input, got, len(got), tt.want, len(tt.want))
			continue
		}
		for i, w := range tt.want {
			if got[i] != w {
				t.Errorf("TokenizeCode(%q)[%d]: got %q, want %q", tt.input, i, got[i], w)
			}
		}
	}
}

func TestTermHash(t *testing.T) {
	// Same term → same hash
	h1 := termHash("config")
	h2 := termHash("config")
	if h1 != h2 {
		t.Error("same term should produce same hash")
	}
	// Different terms → likely different hash
	h3 := termHash("parser")
	if h1 == h3 {
		t.Error("different terms should likely produce different hash")
	}
	// Hash within range
	if h1 >= SparseVectorMaxDim {
		t.Errorf("hash %d >= max %d", h1, SparseVectorMaxDim)
	}
}

func TestBM25Vectorize(t *testing.T) {
	bm := NewBM25Vectorizer()

	docs := []string{
		"func add numbers calculate sum",
		"func multiply numbers product result",
		"class User name email address",
	}
	bm.Fit(docs)

	if bm.docCount != 3 {
		t.Errorf("docCount=%d, want 3", bm.docCount)
	}
	if bm.avgDocLen <= 0 {
		t.Error("avgDocLen should be > 0 after Fit")
	}

	// Vectorize a document
	sv := bm.Vectorize("add numbers calculate")
	if sv == nil {
		t.Fatal("Vectorize returned nil")
	}
	if len(sv.Indices) == 0 {
		t.Error("expected non-empty indices")
	}
	if len(sv.Indices) != len(sv.Values) {
		t.Error("indices and values length mismatch")
	}
}

func TestBM25BuildSearchSparse(t *testing.T) {
	bm := NewBM25Vectorizer()
	bm.Fit([]string{"parse config load settings"})

	sv := bm.BuildSearchSparse("parseConfig")
	if sv == nil {
		t.Fatal("BuildSearchSparse returned nil")
	}
	// camelCase should be split → at least 2 terms
	if len(sv.Indices) < 2 {
		t.Errorf("expected at least 2 terms from parseConfig, got %d", len(sv.Indices))
	}
}

func TestBM25BuildDocSparse(t *testing.T) {
	bm := NewBM25Vectorizer()
	bm.Fit([]string{"search code index"})

	sv := bm.BuildDocSparse("Search", "doSearch", "search.go", "func doSearch query embed vector")
	if sv == nil {
		t.Fatal("BuildDocSparse returned nil")
	}
	if len(sv.Indices) == 0 {
		t.Error("expected non-empty indices")
	}
}

func TestBM25SparseVectorSorted(t *testing.T) {
	bm := NewBM25Vectorizer()
	bm.Fit([]string{"alpha beta gamma delta epsilon"})

	sv := bm.Vectorize("alpha beta gamma delta epsilon")
	if sv == nil {
		t.Fatal("Vectorize returned nil")
	}
	for i := 1; i < len(sv.Indices); i++ {
		if sv.Indices[i] <= sv.Indices[i-1] {
			t.Error("indices should be sorted ascending")
		}
	}
}

func TestBM25EmptyText(t *testing.T) {
	bm := NewBM25Vectorizer()
	bm.Fit([]string{"some text"})

	sv := bm.Vectorize("")
	if sv != nil {
		t.Error("Vectorize of empty text should return nil")
	}
}

func TestBM25UnfittedVectorize(t *testing.T) {
	bm := NewBM25Vectorizer()
	// Without Fit, idf returns 0 for all terms → Vectorize should return nil
	sv := bm.Vectorize("hello world test")
	if sv != nil {
		t.Error("unfitted Vectorize should return nil (no IDF)")
	}
}
