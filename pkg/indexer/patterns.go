package indexer

import (
	"path/filepath"
	"strings"
)

// PatternFilter provides ccc-style include/exclude glob pattern matching.
// Patterns use ** for recursive matching (e.g. **/*.ts, **/node_modules/**).
// If include patterns are set, only files matching at least one are indexed.
// Exclude patterns are applied after includes.
type PatternFilter struct {
	includes []globPattern
	excludes []globPattern
}

type globPattern struct {
	parts    []string // split on /
	suffix   string   // for **/*.ext → ".ext" optimization
	anyDepth bool     // contains **
}

// NewPatternFilter creates a filter from include/exclude pattern lists.
// Empty includes = match all. Empty excludes = exclude none.
func NewPatternFilter(includes, excludes []string) *PatternFilter {
	pf := &PatternFilter{}
	for _, p := range includes {
		pf.includes = append(pf.includes, parseGlob(p))
	}
	for _, p := range excludes {
		pf.excludes = append(pf.excludes, parseGlob(p))
	}
	return pf
}

// Match returns true if a relative path should be indexed (passes includes + not excluded).
func (f *PatternFilter) Match(relPath string) bool {
	relPath = filepath.ToSlash(relPath) // normalize to forward slashes
	// If includes exist, file must match at least one.
	if len(f.includes) > 0 {
		matched := false
		for _, p := range f.includes {
			if matchGlob(p, relPath) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	// Check excludes.
	for _, p := range f.excludes {
		if matchGlob(p, relPath) {
			return false
		}
	}
	return true
}

func parseGlob(pattern string) globPattern {
	pattern = strings.ReplaceAll(pattern, "\\", "/")
	gp := globPattern{
		parts:    strings.Split(pattern, "/"),
		anyDepth: strings.Contains(pattern, "**"),
	}
	// Optimization: **/*.ext → just check suffix
	if len(gp.parts) == 2 && gp.parts[0] == "**" && strings.HasPrefix(gp.parts[1], "*.") {
		gp.suffix = gp.parts[1][1:] // ".ext"
	}
	return gp
}

func matchGlob(gp globPattern, path string) bool {
	// Fast path: suffix check for **/*.ext patterns
	if gp.suffix != "" {
		return strings.HasSuffix(path, gp.suffix)
	}
	// General glob matching
	pathParts := strings.Split(path, "/")
	return matchGlobParts(gp.parts, pathParts)
}

// matchGlobParts matches a glob split on / against a path split on /.
// Supports ** (matches zero or more path segments) and * (matches within a segment).
func matchGlobParts(globParts, pathParts []string) bool {
	// ** matching: try to consume zero or more segments
	for i := 0; i < len(globParts); i++ {
		if globParts[i] == "**" {
			// ** matches zero or more segments
			// Try matching the rest at every possible position
			rest := globParts[i+1:]
			for j := i; j <= len(pathParts); j++ {
				if matchGlobParts(rest, pathParts[j:]) {
					return true
				}
			}
			return false
		}
		// Non-** part: must match exactly one segment
		if i >= len(pathParts) {
			return false
		}
		if !singleSegmentMatch(globParts[i], pathParts[i]) {
			return false
		}
	}
	// All glob parts consumed. Check we consumed all path parts too.
	return len(globParts) == len(pathParts)
}

// singleSegmentMatch matches a single path segment with * wildcard.
// * matches any chars within the segment (not /).
func singleSegmentMatch(glob, segment string) bool {
	// Special case: * matches anything
	if glob == "*" {
		return true
	}
	// No wildcard: exact match
	if !strings.Contains(glob, "*") {
		return glob == segment
	}
	// Wildcard matching within segment
	return wildcardMatch(glob, segment)
}

// wildcardMatch implements * matching within a string.
func wildcardMatch(pattern, s string) bool {
	pi, si := 0, 0
	starPi, starSi := -1, -1
	for si < len(s) {
		if pi < len(pattern) {
			if pattern[pi] == '*' {
				starPi = pi
				starSi = si
				pi++
				continue
			}
			if pattern[pi] == s[si] {
				pi++
				si++
				continue
			}
		}
		if starPi != -1 {
			pi = starPi + 1
			starSi++
			si = starSi
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
