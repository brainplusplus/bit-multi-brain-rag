package indexer

import (
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// DeriveProjectName generates a unique, human-readable project name from a
// filesystem path. The goal is to avoid collisions when two different paths
// share the same leaf folder name (e.g. D:/NodeJs/mitm vs D:/golang/mitm).
//
// Format: {leaf}-{disambiguators} (leaf always first, disambiguators added
// only as needed to resolve collisions against existing names).
//
// Disambiguator precedence:
//   1. Parent folder (e.g. "nodejs", "golang")
//   2. Drive letter (Windows) or grandparent (Unix)
//   3. Numeric counter (-2, -3, ...)
//
// Examples:
//   D:/NodeJs/mitm     → "mitm-nodejs"     (first time)
//   D:/golang/mitm     → "mitm-golang"     (different parent, unique)
//   E:/NodeJs/mitm     → "mitm-nodejs-e"   (same parent, add drive)
//   /home/user/my-app  → "my-app-user"     (Unix)
//   /opt/user/my-app   → "my-app-user-opt" (collision resolved via grandparent)
//   /my-app            → "my-app"          (root, no parent)
func DeriveProjectName(rootPath string) string {
	return ResolveNameCollision(baseProjectName(rootPath), nil)
}

// DeriveProjectNameUnique is like DeriveProjectName but resolves collisions
// against the provided set of existing project names.
func DeriveProjectNameUnique(rootPath string, existingNames map[string]bool) string {
	return ResolveNameCollision(baseProjectName(rootPath), existingNames)
}

// baseProjectName builds the preferred name (leaf-first) without collision
// resolution. It prefers the full path hierarchy for maximum disambiguation
// on first encounter; ResolveNameCollision will simplify if there's actually
// no collision.
func baseProjectName(rootPath string) string {
	clean := filepath.Clean(rootPath)
	leaf := slugify(filepath.Base(clean))

	// Collect path segments from leaf upward (excluding leaf itself).
	var segments []string
	dir := filepath.Dir(clean)
	for {
		base := slugify(filepath.Base(dir))
		if base == "" || base == "." || base == string(filepath.Separator) {
			break
		}
		// Stop at drive letters on Windows (we'll use them as suffix).
		if runtime.GOOS == "windows" && isDriveLetter(base) {
			segments = append(segments, base)
			break
		}
		// Stop at root on Unix.
		if dir == "/" || dir == "." {
			break
		}
		segments = append(segments, base)
		dir = filepath.Dir(dir)
	}

	// Reverse segments so parent is first, grandparent second, etc.
	// segments is currently [parent, grandparent, ...], we want [parent, grandparent] appended to leaf.
	// But we only want the MEANINGFUL ones, filtering out generic names like "home", "users".
	var meaningful []string
	for _, s := range segments {
		if !isGenericSegment(s) {
			meaningful = append(meaningful, s)
		}
	}

	if len(meaningful) == 0 {
		// No meaningful parent. On Windows, check for drive letter.
		if runtime.GOOS == "windows" && len(segments) > 0 && isDriveLetter(segments[0]) {
			return leaf + "-" + segments[0]
		}
		return leaf
	}

	// Build candidate: leaf-parent[-grandparent...]
	parts := append([]string{leaf}, meaningful...)
	return strings.Join(parts, "-")
}

// isGenericSegment returns true for common path segments that don't add
// disambiguation value (but we still include them as last resort).
func isGenericSegment(name string) bool {
	switch name {
	case "home", "users", "user", "usr", "opt", "var", "etc", "tmp":
		return true
	}
	return false
}

func isDriveLetter(s string) bool {
	if len(s) != 1 {
		return false
	}
	c := s[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// slugify converts a string to a URL/project-safe slug:
// lowercase, alphanumeric + hyphens only.
var nonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.ToLower(s)
	s = nonAlnumRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

// ResolveNameCollision ensures a candidate name doesn't collide with existing
// names by appending disambiguators. It first tries the candidate as-is,
// then appends drive letters / counters as needed.
func ResolveNameCollision(candidate string, existingNames map[string]bool) string {
	if existingNames == nil || !existingNames[candidate] {
		return candidate
	}
	// Collision: try appending numeric suffix.
	for i := 2; ; i++ {
		// Strip any previous numeric suffix to avoid stacking (e.g. -2-3).
		base := stripNumericSuffix(candidate)
		cand := base + "-" + itoa(i)
		if !existingNames[cand] {
			return cand
		}
		if i > 100 {
			return candidate + "-" + itoa(i) // give up, append anyway
		}
	}
}

func stripNumericSuffix(s string) string {
	parts := strings.Split(s, "-")
	if len(parts) <= 1 {
		return s
	}
	last := parts[len(parts)-1]
	// Check if last part is purely numeric.
	isNum := true
	for _, c := range last {
		if c < '0' || c > '9' {
			isNum = false
			break
		}
	}
	if isNum {
		return strings.Join(parts[:len(parts)-1], "-")
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
