// Package manifest implements snapshot-based change detection for incremental
// re-indexing. Inspired by git's object model: store a snapshot of file states,
// compare on demand to get exact changeset (added, modified, deleted).
//
// Manifest files are stored centrally, not in project folders:
//
//	~/.bit-rag/projects/<project_id>/manifest.json
//
// MCP reads manifest on connect, compares mtime+size vs filesystem, and
// triggers delta re-index only for changed files. No fsnotify needed —
// no background process, no missed events on restart, no platform quirks.
package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FileEntry tracks one file's indexed state.
type FileEntry struct {
	Hash string `json:"h"`   // content hash (FNV-1a, hex)
	Size int64  `json:"sz"`  // file size in bytes
	MTime int64 `json:"mt"`  // modtime unix seconds
}

// Manifest is the snapshot of all indexed files for one project.
type Manifest struct {
	ProjectID   string               `json:"project_id"`
	ProjectName string               `json:"project_name"`
	RootPath    string               `json:"root_path"`
	IndexedAt   time.Time            `json:"indexed_at"`
	Files       map[string]FileEntry `json:"files"`
}

// Diff is the changeset between manifest and current filesystem state.
type Diff struct {
	Added    []string // new files not in manifest
	Modified []string // files with different hash
	Deleted  []string // in manifest but gone from disk
}

// HasChanges returns true if any files changed.
func (d *Diff) HasChanges() bool {
	return len(d.Added) > 0 || len(d.Modified) > 0 || len(d.Deleted) > 0
}

// ChangedFiles returns all changed file paths (added + modified + deleted).
func (d *Diff) ChangedFiles() []string {
	n := len(d.Added) + len(d.Modified) + len(d.Deleted)
	if n == 0 {
		return nil
	}
	out := make([]string, 0, n)
	out = append(out, d.Added...)
	out = append(out, d.Modified...)
	out = append(out, d.Deleted...)
	return out
}

// --- Manifest store ---

// manifestDir returns the directory for a project's manifest.
// ~/.bit-rag/projects/<projectID>/
func manifestDir(projectID string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(home, ".bit-rag", "projects", projectID), nil
}

// manifestPath returns the full path to a project's manifest file.
func manifestPath(projectID string) (string, error) {
	dir, err := manifestDir(projectID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "manifest.json"), nil
}

// Load reads the manifest for a project. Returns nil + nil if no manifest
// exists yet (first index).
func Load(projectID string) (*Manifest, error) {
	p, err := manifestPath(projectID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	return &m, nil
}

// Save writes the manifest for a project. Creates directories as needed.
func Save(m *Manifest) error {
	p, err := manifestPath(m.ProjectID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create manifest dir: %w", err)
	}
	m.IndexedAt = time.Now().UTC()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(p, data, 0644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// Delete removes the manifest for a project (on project delete).
func Delete(projectID string) error {
	dir, err := manifestDir(projectID)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// --- Diff engine ---

// Compare walks rootPath and compares against the manifest's known file states.
// Uses two-tier check: mtime+size first (instant), hash only if mtime differs.
// fileList is the set of files to check (typically from the indexer walk).
func (m *Manifest) Compare(rootPath string, fileList []string) *Diff {
	d := &Diff{}

	// Track which manifest files we saw on disk (to detect deletions).
	seen := make(map[string]bool, len(fileList))

	for _, relPath := range fileList {
		seen[relPath] = true
		absPath := filepath.Join(rootPath, relPath)
		info, err := os.Stat(absPath)
		if err != nil {
			// File vanished between list and stat — treat as deleted.
			if _, existed := m.Files[relPath]; existed {
				d.Deleted = append(d.Deleted, relPath)
			}
			continue
		}

		entry, inManifest := m.Files[relPath]

		// Fast path: mtime + size unchanged → skip hashing.
		if inManifest && info.ModTime().Unix() == entry.MTime && info.Size() == entry.Size {
			continue
		}

		// Slow path: hash to confirm real change.
		hash, err := hashFile(absPath)
		if err != nil {
			continue
		}

		if !inManifest {
			d.Added = append(d.Added, relPath)
		} else if hash != entry.Hash {
			d.Modified = append(d.Modified, relPath)
		}
		// else: mtime changed but content identical (e.g. touch) → no-op
	}

	// Detect deletions: files in manifest but not seen during walk.
	for relPath := range m.Files {
		if !seen[relPath] {
			d.Deleted = append(d.Deleted, relPath)
		}
	}

	return d
}

// ApplyUpdate updates the manifest with new file states after indexing.
// Call after successful delta or full re-index.
func (m *Manifest) ApplyUpdate(rootPath string, files []string) error {
	if m.Files == nil {
		m.Files = make(map[string]FileEntry)
	}

	// Remove deleted files from manifest.
	currentFiles := make(map[string]bool, len(files))
	for _, f := range files {
		currentFiles[f] = true
	}
	for relPath := range m.Files {
		if !currentFiles[relPath] {
			delete(m.Files, relPath)
		}
	}

	// Update/add file entries.
	for _, relPath := range files {
		absPath := filepath.Join(rootPath, relPath)
		info, err := os.Stat(absPath)
		if err != nil {
			continue
		}
		hash, err := hashFile(absPath)
		if err != nil {
			continue
		}
		m.Files[relPath] = FileEntry{
			Hash:  hash,
			Size:  info.Size(),
			MTime: info.ModTime().Unix(),
		}
	}

	return Save(m)
}

// New creates a fresh empty manifest for a project.
func New(projectID, projectName, rootPath string) *Manifest {
	return &Manifest{
		ProjectID:   projectID,
		ProjectName: projectName,
		RootPath:    rootPath,
		Files:       make(map[string]FileEntry),
	}
}
