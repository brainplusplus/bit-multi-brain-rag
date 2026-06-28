package indexer

import "testing"

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		name    string
		want    bool
	}{
		{"*.log", "app.log", true},
		{"*.log", "app.txt", false},
		{"*.go", "main.go", true},
		{"test*", "test_file.go", true},
		{"test*", "prod_file.go", false},
		{"*", "anything", true},
		{"abc", "abc", true},
		{"abc", "abcd", false},
		{"a*c", "abc", true},
		{"a*c", "axxxc", true},
		{"a*c", "abd", false},
	}
	for _, tt := range tests {
		got := globMatch(tt.pattern, tt.name)
		if got != tt.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
		}
	}
}

func TestMatchGitignorePattern(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		isDir   bool
		want    bool
	}{
		// Simple glob
		{"*.log", "app.log", false, true},
		{"*.log", "logs/app.log", false, true},
		{"*.log", "app.go", false, false},

		// Directory-only (trailing /)
		{"node_modules/", "node_modules", true, true},
		{"node_modules/", "app.go", false, false},

		// Negation
		{"!important.log", "important.log", false, false},

		// Exact name
		{".env", ".env", false, true},
		{".env", "config/.env", false, true},

		// Path patterns
		{"build/", "build", true, true},
		{"build/", "dist", true, false},
	}
	for _, tt := range tests {
		// matchGitignorePattern returns the negation logic, so we check
		// that it returns true when the file should be ignored.
		got := matchGitignorePattern(tt.pattern, tt.path, tt.isDir)
		if got != tt.want {
			t.Errorf("matchGitignorePattern(%q, %q, dir=%v) = %v, want %v",
				tt.pattern, tt.path, tt.isDir, got, tt.want)
		}
	}
}

func TestGitignoreMatcherBasic(t *testing.T) {
	gi := newGitignoreMatcher()

	// Simulate loading a .gitignore
	gi.patterns = append(gi.patterns, gitignoreLevel{
		depth:    0,
		patterns: []string{"*.log", ".env", "secrets/"},
	})

	tests := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{"app.log", false, true},
		{"debug/app.log", false, true},
		{".env", false, true},
		{"config/.env", false, true},
		{"app.go", false, false},
		{"secrets", true, true},
		{"src", true, false},
	}
	for _, tt := range tests {
		got := gi.match(tt.path, tt.isDir)
		if got != tt.want {
			t.Errorf("match(%q, dir=%v) = %v, want %v", tt.path, tt.isDir, got, tt.want)
		}
	}
}

func TestGitignoreMatcherNested(t *testing.T) {
	gi := newGitignoreMatcher()

	// Root .gitignore
	gi.patterns = append(gi.patterns, gitignoreLevel{
		depth:    0,
		patterns: []string{"*.tmp"},
	})
	// Nested .gitignore at depth 1
	gi.patterns = append(gi.patterns, gitignoreLevel{
		depth:    1,
		patterns: []string{"local.go"},
	})

	// Root patterns should match at any depth
	if !gi.match("file.tmp", false) {
		t.Error("root *.tmp should match")
	}
	// Nested patterns should match files under that directory
	if !gi.match("pkg/local.go", false) {
		t.Error("nested local.go should match")
	}
	// Files not matching any pattern
	if gi.match("pkg/main.go", false) {
		t.Error("pkg/main.go should not match")
	}
}

func TestShouldSkipDir(t *testing.T) {
	skip := []string{".git", "node_modules", "vendor", "__pycache__", ".venv", "dist", "build"}
	keep := []string{"src", "pkg", "cmd", "internal"}

	for _, name := range skip {
		if !shouldSkipDir(name) {
			t.Errorf("shouldSkipDir(%q) = false, want true", name)
		}
	}
	for _, name := range keep {
		if shouldSkipDir(name) {
			t.Errorf("shouldSkipDir(%q) = true, want false", name)
		}
	}
	// Hidden dirs
	if !shouldSkipDir(".hidden") {
		t.Error("hidden dirs should be skipped")
	}
}

func TestIsSourceFile(t *testing.T) {
	index := []string{"main.go", "app.py", "index.js", "component.tsx", "main.rs",
		"Dockerfile", "Makefile", "schema.sql", "config.yaml", "style.css"}
	skip := []string{"image.png", "video.mp4", "binary.exe", "data.bin"}

	for _, f := range index {
		if !isSourceFile(f) {
			t.Errorf("isSourceFile(%q) = false, want true", f)
		}
	}
	for _, f := range skip {
		if isSourceFile(f) {
			t.Errorf("isSourceFile(%q) = true, want false", f)
		}
	}
}

func TestSHA256Hex(t *testing.T) {
	// Known SHA-256 of empty string
	got := sha256Hex([]byte{})
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("sha256Hex(empty) = %q, want %q", got, want)
	}

	// Known SHA-256 of "hello"
	got = sha256Hex([]byte("hello"))
	want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Errorf("sha256Hex(hello) = %q, want %q", got, want)
	}
}
