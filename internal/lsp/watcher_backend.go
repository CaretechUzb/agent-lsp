package lsp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/blackwell-systems/agent-lsp/internal/logging"
	"github.com/fsnotify/fsnotify"
)

// fileWatcher is the auto-watcher backend. The kqueue backend (fsnotify) opens
// one fd per file in every watched directory on macOS, so a multi-server config
// watching the same tree multiplies that cost per client and several agent-lsp
// processes together can exhaust the system-wide file table. The FSEvents
// backend (darwin) watches a whole tree with one stream and no per-file fds.
type fileWatcher interface {
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	// AddTree starts watching root and everything below it, honoring
	// watcherSkipDirs and the hidden-dir rule. An error means nothing under
	// root is watched.
	AddTree(root string) error
	// WatchNewDir is called for a directory created at runtime. Recursive
	// backends ignore it; the kqueue backend must add it explicitly.
	WatchNewDir(path string)
	Close() error
}

// errNoPlatformWatcher is returned by newPlatformWatcher on platforms without a
// recursive, fd-free watcher backend.
var errNoPlatformWatcher = errors.New("no platform watcher backend")

// newFileWatcher returns the best available backend: FSEvents on darwin, the
// fsnotify backend elsewhere or when FSEvents cannot start.
// AGENT_LSP_WATCH_BACKEND=kqueue forces the fsnotify backend.
func newFileWatcher(lim watcherLimits) (fileWatcher, string, error) {
	if !strings.EqualFold(strings.TrimSpace(os.Getenv("AGENT_LSP_WATCH_BACKEND")), "kqueue") {
		w, err := newPlatformWatcher()
		if err == nil {
			return w, "fsevents", nil
		}
		if !errors.Is(err, errNoPlatformWatcher) {
			logging.Log(logging.LevelWarning, "auto-watcher: FSEvents unavailable, falling back to kqueue: "+err.Error())
		}
	}
	w, err := newKqueueWatcher(lim)
	if err != nil {
		return nil, "", err
	}
	return w, "fsnotify", nil
}

func newKqueueWatcher(lim watcherLimits) (fileWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &kqueueWatcher{w: w, lim: lim}, nil
}

// kqueueWatcher adapts fsnotify.Watcher, which watches single directories, to
// fileWatcher. All roots share one entry budget (issue #18).
type kqueueWatcher struct {
	w     *fsnotify.Watcher
	lim   watcherLimits
	mu    sync.Mutex
	total int
}

func (k *kqueueWatcher) Events() <-chan fsnotify.Event { return k.w.Events }
func (k *kqueueWatcher) Errors() <-chan error          { return k.w.Errors }
func (k *kqueueWatcher) Close() error                  { return k.w.Close() }

func (k *kqueueWatcher) AddTree(root string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.total = addWatchedTree(k.w, root, k.lim, k.total)
	logging.Log(logging.LevelDebug, fmt.Sprintf("auto-watcher: watching ~%d entries under %s", k.total, root))
	return nil
}

// WatchNewDir adds a runtime-created directory unless it is oversized (a
// runtime-created cache dir would open that many kqueue fds on macOS, issue #18).
func (k *kqueueWatcher) WatchNewDir(path string) {
	if watcherSkipDirs[filepath.Base(path)] {
		return
	}
	if entries, err := os.ReadDir(path); err == nil && len(entries) <= k.lim.maxDirEntries {
		_ = k.w.Add(path)
	}
}

// watchPathExcluded reports whether path lies under an excluded directory of
// root (a watcherSkipDirs name or a hidden directory). Recursive backends
// receive events for the whole tree and filter with this, mirroring what
// addWatchedTree skips during its walk.
func watchPathExcluded(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, dir := range parts[:len(parts)-1] {
		if watcherSkipDirs[dir] || strings.HasPrefix(dir, ".") {
			return true
		}
	}
	return false
}
