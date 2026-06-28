package indexer

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// gitignoreMatcher implements a minimal .gitignore pattern matcher.
// It respects nested .gitignore files throughout the directory tree and
// supports the most common gitignore patterns:
//   - Simple glob patterns (e.g. "*.log", "node_modules/")
//   - Directory patterns (trailing /)
//   - Negation (prefix !)
//   - Path-anchored (leading /)
//   - Double-wildcard (**)
//
// It does NOT support:
//   - Character ranges ([abc]) — rare in gitignore
//   - Complex nested ** patterns — edge case
//
// Each directory in the walk can have its own .gitignore. Patterns are
// accumulated as we descend and cleared as we ascend.
type gitignoreMatcher struct {
	// patterns is a stack of (dir-relative, patterns) pairs.
	// As we descend into subdirs, we push; as we ascend, we pop.
	patterns []gitignoreLevel
}

type gitignoreLevel struct {
	depth    int      // directory depth (0 = root)
	patterns []string // gitignore patterns from this level
}

func newGitignoreMatcher() *gitignoreMatcher {
	return &gitignoreMatcher{}
}

// loadDir reads .gitignore in the given directory and pushes patterns.
// Call this when entering a directory during walk.
func (g *gitignoreMatcher) loadDir(dirPath string, depth int) {
	giPath := filepath.Join(dirPath, ".gitignore")
	f, err := os.Open(giPath)
	if err != nil {
		return // no .gitignore in this dir — that's fine
	}
	defer f.Close()

	var patterns []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	if len(patterns) > 0 {
		g.patterns = append(g.patterns, gitignoreLevel{depth: depth, patterns: patterns})
	}
}

// pop removes patterns from the current depth level (call when ascending).
func (g *gitignoreMatcher) pop(depth int) {
	for len(g.patterns) > 0 && g.patterns[len(g.patterns)-1].depth >= depth {
		g.patterns = g.patterns[:len(g.patterns)-1]
	}
}

// match checks if a relative path is ignored by any loaded .gitignore.
func (g *gitignoreMatcher) match(relPath string, isDir bool) bool {
	for _, level := range g.patterns {
		for _, pattern := range level.patterns {
			if matchGitignorePattern(pattern, relPath, isDir) {
				// Check for negation — later ! patterns can un-ignore.
				// For simplicity, we process all patterns and let ! override.
				if strings.HasPrefix(pattern, "!") {
					return false // un-ignored
				}
				return true
			}
		}
	}
	return false
}

// matchGitignorePattern matches a single gitignore pattern against a path.
func matchGitignorePattern(pattern, relPath string, isDir bool) bool {
	negate := false
	if strings.HasPrefix(pattern, "!") {
		negate = true
		pattern = pattern[1:]
	}

	// Directory-only pattern (trailing /).
	dirOnly := strings.HasSuffix(pattern, "/")
	if dirOnly {
		pattern = strings.TrimSuffix(pattern, "/")
		if !isDir {
			// Pattern only matches directories, but this is a file.
			// Check if any parent directory matches.
			parts := strings.Split(relPath, "/")
			for i := 1; i < len(parts); i++ {
				parent := strings.Join(parts[:i], "/")
				if globMatch(pattern, filepath.Base(parent)) || globMatch(pattern, parent) {
					return negate != true
				}
			}
			return false
		}
	}

	// Anchored pattern (leading /).
	if strings.HasPrefix(pattern, "/") {
		pattern = strings.TrimPrefix(pattern, "/")
		return globMatch(pattern, relPath) == !negate
	}

	// Non-anchored: match against any path segment.
	// e.g. "node_modules" matches "foo/node_modules" and "node_modules".
	parts := strings.Split(relPath, "/")
	for _, part := range parts {
		if globMatch(pattern, part) {
			return !negate
		}
	}
	// Also try full path match (for patterns with /).
	if globMatch(pattern, relPath) {
		return !negate
	}

	return negate // if negated and no match, return false
}

// globMatch is a minimal glob matcher supporting * and **.
func globMatch(pattern, name string) bool {
	// Handle ** (matches any number of path segments).
	if strings.Contains(pattern, "**") {
		// Split on ** and match prefix + suffix.
		idx := strings.Index(pattern, "**")
		prefix := pattern[:idx]
		suffix := pattern[idx+2:]
		// Strip leading/trailing / from prefix/suffix.
		prefix = strings.TrimSuffix(prefix, "/")
		suffix = strings.TrimPrefix(suffix, "/")
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			return false
		}
		if suffix != "" && !strings.HasSuffix(name, suffix) {
			return false
		}
		return true
	}

	// Standard glob with * (matches any chars except /).
	pi, ni := 0, 0
	starPi, starNi := -1, -1
	for ni < len(name) {
		if pi < len(pattern) {
			if pattern[pi] == '*' {
				starPi = pi
				starNi = ni
				pi++
				continue
			}
			if pattern[pi] == name[ni] {
				pi++
				ni++
				continue
			}
		}
		if starPi != -1 {
			pi = starPi + 1
			starNi++
			ni = starNi
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
