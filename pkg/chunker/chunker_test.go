package chunker

import (
	"context"
	"strings"
	"testing"
)

// TestChunkGoFile verifies AST-aware chunking on a small Go snippet.
func TestChunkGoFile(t *testing.T) {
	src := []byte(`package main

import "fmt"

// Add returns the sum.
func Add(a, b int) int {
	return a + b
}

type Point struct {
	X, Y int
}

func (p Point) Distance() int {
	return p.X + p.Y
}
`)
	ch := New()
	chunks, err := ch.ChunkFile(context.Background(), src, "main.go")
	if err != nil {
		t.Fatalf("ChunkFile: %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("expected >=3 chunks (Add, Point, Distance), got %d", len(chunks))
	}
	// Verify each chunk has required metadata.
	for _, c := range chunks {
		if c.Language != "go" {
			t.Errorf("chunk language = %q, want go", c.Language)
		}
		if c.SourceFile != "main.go" {
			t.Errorf("source file = %q, want main.go", c.SourceFile)
		}
		if c.Content == "" {
			t.Error("chunk content is empty")
		}
		if c.StartLine <= 0 || c.EndLine < c.StartLine {
			t.Errorf("bad line range: %d-%d", c.StartLine, c.EndLine)
		}
	}
	// Check that "Add" symbol was extracted.
	foundAdd := false
	for _, c := range chunks {
		if c.Name == "Add" {
			foundAdd = true
			if !strings.Contains(c.Content, "func Add") {
				t.Errorf("Add chunk content missing 'func Add': %q", c.Content)
			}
		}
	}
	if !foundAdd {
		t.Error("no chunk named 'Add' found")
	}
}

// TestChunkUnsupportedLang falls back to naive chunking.
func TestChunkUnsupportedLang(t *testing.T) {
	src := []byte(strings.Repeat("line of text\n", 150))
	ch := New()
	ch.MaxChunkLines = 50
	chunks, err := ch.ChunkFile(context.Background(), src, "readme.txt")
	if err != nil {
		t.Fatalf("ChunkFile: %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("expected >=3 naive chunks (150/50), got %d", len(chunks))
	}
	if chunks[0].Language != "txt" {
		t.Errorf("language = %q, want txt", chunks[0].Language)
	}
}
