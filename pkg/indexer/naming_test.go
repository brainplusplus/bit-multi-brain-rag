package indexer

import (
	"runtime"
	"testing"
)

func TestDeriveProjectName(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		// Windows paths — leaf-first + parent
		{"D:/NodeJs/mitm", "mitm-nodejs"},
		{"D:/golang/mitm", "mitm-golang"},
		{"D:\\NodeJs\\mitm", "mitm-nodejs"},
		{"D:\\golang\\mitm", "mitm-golang"},

		// No meaningful parent (root/drive only)
		{"D:/app", "app"},
		{"/my-app", "my-app"},

		// Unix paths — leaf-first + parent
		{"/projects/app", "app-projects"},
		{"/repos/api-server", "api-server-repos"},

		// Special characters
		{"D:/My Project.App", "my-project-app"}, // leaf only, no meaningful parent
		{"/home/user/FooBar_Baz", "foobar-baz"}, // "user" and "home" are generic, skipped

		// Same parent and leaf — slug of "test" parent is "test"
		// Result: leaf "test" + parent "test" = "test-test", then collision
		// resolution would apply but with nil map it stays.
		{"/test/test", "test-test"},
	}
	for _, tt := range tests {
		// Skip Windows-specific tests on non-Windows (drive letter detection).
		if runtime.GOOS != "windows" {
			if isWindowsPath(tt.path) {
				t.Logf("skip %q on %s", tt.path, runtime.GOOS)
				continue
			}
		}
		got := DeriveProjectName(tt.path)
		if got != tt.want {
			t.Errorf("DeriveProjectName(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestResolveNameCollision_NoCollision(t *testing.T) {
	existing := map[string]bool{"mitm": true}
	got := ResolveNameCollision("new-project", existing)
	if got != "new-project" {
		t.Errorf("got %q, want new-project", got)
	}
}

func TestResolveNameCollision_WithExisting(t *testing.T) {
	existing := map[string]bool{
		"mitm":          true,
		"mitm-nodejs":   true,
		"mitm-golang":   true,
	}
	// "mitm-nodejs" exists → should become "mitm-nodejs-2"
	got := ResolveNameCollision("mitm-nodejs", existing)
	if got != "mitm-nodejs-2" {
		t.Errorf("got %q, want mitm-nodejs-2", got)
	}
	// "mitm-golang" exists → should become "mitm-golang-2"
	got = ResolveNameCollision("mitm-golang", existing)
	if got != "mitm-golang-2" {
		t.Errorf("got %q, want mitm-golang-2", got)
	}
}

func TestResolveNameCollision_NilExisting(t *testing.T) {
	got := ResolveNameCollision("my-app", nil)
	if got != "my-app" {
		t.Errorf("got %q, want my-app", got)
	}
}

func TestResolveNameCollision_ChainedNumerics(t *testing.T) {
	// Test that -2-3 stacking doesn't happen.
	existing := map[string]bool{
		"mitm-nodejs":   true,
		"mitm-nodejs-2": true,
	}
	got := ResolveNameCollision("mitm-nodejs", existing)
	if got != "mitm-nodejs-3" {
		t.Errorf("got %q, want mitm-nodejs-3", got)
	}
}

func TestSlugify(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"NodeJs", "nodejs"},
		{"my-app", "my-app"},
		{"My Project.App", "my-project-app"},
		{"foo_bar baz", "foo-bar-baz"},
		{"UPPER", "upper"},
		{"123abc", "123abc"},
		{"", ""},
	}
	for _, tt := range tests {
		got := slugify(tt.input)
		if got != tt.want {
			t.Errorf("slugify(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestStripNumericSuffix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"mitm-nodejs-2", "mitm-nodejs"},
		{"mitm-nodejs", "mitm-nodejs"},
		{"app-3", "app"},
		{"app", "app"},
		{"a-b-c-10", "a-b-c"},
	}
	for _, tt := range tests {
		got := stripNumericSuffix(tt.input)
		if got != tt.want {
			t.Errorf("stripNumericSuffix(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsDriveLetter(t *testing.T) {
	if !isDriveLetter("d") {
		t.Error("'d' should be a drive letter")
	}
	if !isDriveLetter("D") {
		t.Error("'D' should be a drive letter")
	}
	if isDriveLetter("de") {
		t.Error("'de' should not be a drive letter")
	}
	if isDriveLetter("") {
		t.Error("empty string should not be a drive letter")
	}
}

func isWindowsPath(p string) bool {
	return len(p) >= 2 && (p[1] == ':')
}
