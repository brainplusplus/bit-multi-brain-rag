// Package watcher implements filesystem watching for auto-reindexing.
//
// When a project's source files change (create, modify, delete), the watcher
// fires a debounced callback so the dashboard can enqueue a delta index job
// without the user manually triggering it.
//
// Architecture (ADR-0007 Phase 10):
//   - Uses fsnotify for cross-platform file event monitoring
//   - Debounce: batches events in a 5s window, fires once (avoids per-keystroke)
//   - Respects .gitignore + shouldSkipDir (same logic as indexer)
//   - Per-project: each project gets its own watcher goroutine
package watcher

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/brainplusplus/bit-multi-brain-rag/pkg/indexer"
)

// Watcher watches a project directory and fires a debounced callback on changes.
type Watcher struct {
	fw       *fsnotify.Watcher
	root     string
	logger   *slog.Logger
	onChange func(changed []string) // debounced callback with list of changed files
	debounce time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
}

// New creates a watcher for rootPath. Call Start() to begin monitoring.
// onChange is called (at most once per debounce window) with the list of
// changed source file paths (absolute).
func New(rootPath string, onChange func(changed []string), logger *slog.Logger) (*Watcher, error) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		fw:       fw,
		root:     rootPath,
		logger:   logger,
		onChange: onChange,
		debounce: 5 * time.Second,
		done:     make(chan struct{}),
	}, nil
}

// Start begins watching. Blocks until ctx is cancelled or Stop() is called.
// Call in a goroutine: go w.Start(ctx).
func (w *Watcher) Start(ctx context.Context) {
	ctx, w.cancel = context.WithCancel(ctx)
	defer close(w.done)

	// Walk the tree and add watches for all directories.
	w.addWatches(w.root)

	var timer *time.Timer
	var timerMu sync.Mutex
	changedFiles := make(map[string]struct{}) // dedup changed paths

	fire := func() {
		timerMu.Lock()
		files := make([]string, 0, len(changedFiles))
		for f := range changedFiles {
			files = append(files, f)
		}
		changedFiles = make(map[string]struct{})
		timer = nil
		timerMu.Unlock()

		if len(files) > 0 {
			w.logger.Info("file changes detected, triggering delta re-index",
				"root", w.root, "files_changed", len(files))
			w.onChange(files)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.fw.Events:
			if !ok {
				return
			}
			// Only care about write/create/remove/rename on source files.
			if event.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			// Skip non-source files.
			if !indexer.IsSourceFilePublic(event.Name) {
				// But if it's a new directory, we need to watch it.
				if event.Op&fsnotify.Create != 0 {
					w.addWatches(event.Name)
				}
				continue
			}
			// Collect changed file, dedup.
			timerMu.Lock()
			changedFiles[event.Name] = struct{}{}
			// Debounce: reset timer on each event, fire once after quiet period.
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(w.debounce, fire)
			timerMu.Unlock()

		case err, ok := <-w.fw.Errors:
			if !ok {
				return
			}
			w.logger.Warn("watcher error", "error", err)
		}
	}
}

// Stop stops the watcher and cleans up.
func (w *Watcher) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.fw.Close()
	<-w.done
}

// addWatches recursively adds watch descriptors for all subdirectories.
func (w *Watcher) addWatches(dir string) {
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		// Skip common non-source dirs (same logic as indexer).
		name := info.Name()
		if path != dir && indexer.ShouldSkipDirPublic(name) {
			return filepath.SkipDir
		}
		if err := w.fw.Add(path); err != nil {
			w.logger.Debug("watch add failed", "path", path, "error", err)
		}
		return nil
	})
}
