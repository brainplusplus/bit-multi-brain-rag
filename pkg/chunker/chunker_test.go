package chunker

import (
	"context"
	"testing"
)

func TestLangByExt(t *testing.T) {
	tests := []struct {
		ext      string
		wantLang string
		wantOK   bool
	}{
		{".go", "go", true},
		{".py", "python", true},
		{".js", "javascript", true},
		{".ts", "typescript", true},
		{".rs", "rust", true},
		{".java", "java", true},
		{".cs", "csharp", true},
		{".cpp", "cpp", true},
		{".rb", "ruby", true},
		{".php", "php", true},
		{".sh", "bash", true},
		{".sql", "sql", true},
		{".swift", "swift", true},
		{".kt", "kotlin", true},
		{".lua", "lua", true},
		{".proto", "protobuf", true},
		{".tf", "hcl", true},
		{".yaml", "yaml", true},
		{".css", "css", true},
		{".html", "html", true},
		{".unknown", "", false},
		{".txt", "", false},
		{".json", "", false},
	}
	for _, tt := range tests {
		info, ok := langByExt(tt.ext)
		if ok != tt.wantOK {
			t.Errorf("langByExt(%q): ok=%v, want %v", tt.ext, ok, tt.wantOK)
			continue
		}
		if ok && info.name != tt.wantLang {
			t.Errorf("langByExt(%q): lang=%q, want %q", tt.ext, info.name, tt.wantLang)
		}
	}
}

func TestLangByBasename(t *testing.T) {
	info, ok := langByBasename("Dockerfile")
	if !ok || info.name != "dockerfile" {
		t.Errorf("langByBasename('Dockerfile'): ok=%v", ok)
	}
	_, ok = langByBasename("Makefile")
	if ok {
		t.Error("langByBasename('Makefile'): expected ok=false (no AST grammar for Makefile)")
	}
}

func TestChunkGoAST(t *testing.T) {
	src := []byte(`package main

import "fmt"

func add(a, b int) int {
	return a + b
}

func main() {
	fmt.Println(add(1, 2))
}
`)
	ch := New()
	chunks, err := ch.ChunkFile(context.Background(), src, "test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks (add + main), got %d", len(chunks))
	}
	if chunks[0].Name != "add" {
		t.Errorf("first chunk name=%q, want 'add'", chunks[0].Name)
	}
	if chunks[1].Name != "main" {
		t.Errorf("second chunk name=%q, want 'main'", chunks[1].Name)
	}
	if chunks[0].Language != "go" {
		t.Errorf("language=%q, want 'go'", chunks[0].Language)
	}
}

func TestChunkPythonAST(t *testing.T) {
	src := []byte(`def greet(name):
    print(f"Hello, {name}!")

class Calculator:
    def add(self, a, b):
        return a + b
`)
	ch := New()
	chunks, err := ch.ChunkFile(context.Background(), src, "test.py")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
}

func TestChunkNaiveFallback(t *testing.T) {
	src := []byte("line1\nline2\nline3\n")
	ch := New()
	ch.MaxChunkLines = 2
	chunks, err := ch.ChunkFile(context.Background(), src, "test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks (2 lines each), got %d", len(chunks))
	}
	if chunks[0].Language != "txt" {
		t.Errorf("language=%q, want 'txt'", chunks[0].Language)
	}
}

func TestChunkEmptyFile(t *testing.T) {
	ch := New()
	chunks, err := ch.ChunkFile(context.Background(), []byte(""), "empty.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk (whole-file fallback), got %d", len(chunks))
	}
}

func TestChunkRubyAST(t *testing.T) {
	src := []byte(`
class User
  def name
    @name
  end
end
`)
	ch := New()
	chunks, err := ch.ChunkFile(context.Background(), src, "user.rb")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least 1 chunk for Ruby")
	}
	if chunks[0].Language != "ruby" {
		t.Errorf("language=%q, want 'ruby'", chunks[0].Language)
	}
}

func TestChunkSQLAST(t *testing.T) {
	src := []byte(`
CREATE TABLE users (
    id INTEGER PRIMARY KEY,
    name TEXT NOT NULL
);

CREATE INDEX idx_users_name ON users(name);
`)
	ch := New()
	chunks, err := ch.ChunkFile(context.Background(), src, "schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least 1 chunk for SQL")
	}
	if chunks[0].Language != "sql" {
		t.Errorf("language=%q, want 'sql'", chunks[0].Language)
	}
}

func TestChunkDockerfile(t *testing.T) {
	src := []byte(`FROM golang:1.25-alpine
WORKDIR /app
COPY . .
RUN go build -o app .
CMD ["./app"]
`)
	ch := New()
	chunks, err := ch.ChunkFile(context.Background(), src, "Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least 1 chunk for Dockerfile")
	}
}
