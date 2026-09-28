//go:build darwin && !ios

package lsp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func newTestFileWatcher(t *testing.T) (fileWatcher, string) {
	t.Helper()
	w, backend, err := newFileWatcher(loadWatcherLimits())
	if err != nil {
		t.Fatalf("newFileWatcher: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, backend
}

// waitEvent returns the first event for path, or fails after a timeout.
func waitEvent(t *testing.T, w fileWatcher, path string) fsnotify.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-w.Events():
			if ev.Name == path {
				return ev
			}
		case <-deadline:
			t.Fatalf("no event for %s", path)
		}
	}
}

// TestFileWatcher_NoPerFileFDs is the regression test for the macOS fd leak: the
// kqueue backend opens one fd per file in every watched directory, so a
// 2000-file tree cost 2000+ fds per client (36k per process with two servers on
// an Odoo tree). The default darwin backend must watch it with a constant
// number of fds. Run with AGENT_LSP_WATCH_BACKEND=kqueue to see the old cost.
func TestFileWatcher_NoPerFileFDs(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 4; i++ {
		mkDirWithFiles(t, filepath.Join(root, "pkg", string(rune('a'+i))), 500)
	}

	before := openFDCount()
	w, backend := newTestFileWatcher(t)
	w.AddTree(root)
	after := openFDCount()

	if delta := after - before; delta > 50 {
		t.Fatalf("%s backend opened %d fds to watch 2000 files; want a constant handful", backend, delta)
	}
}

func TestFSEvents_EventsMapToWatchedRoot(t *testing.T) {
	t.Setenv("AGENT_LSP_WATCH_BACKEND", "")
	root := t.TempDir() // /var/folders/... is a symlink to /private/var/...
	w, backend := newTestFileWatcher(t)
	if backend != "fsevents" {
		t.Fatalf("backend = %q, want fsevents", backend)
	}
	w.AddTree(root)

	file := filepath.Join(root, "sub", "a.py")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ev := waitEvent(t, w, file); ev.Op != fsnotify.Create && ev.Op != fsnotify.Write {
		t.Errorf("create: op = %v, want Create or Write", ev.Op)
	}

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if ev := waitEvent(t, w, file); ev.Op != fsnotify.Remove {
		t.Errorf("remove: op = %v, want Remove", ev.Op)
	}
}

func TestFSEvents_SkipsExcludedDirs(t *testing.T) {
	t.Setenv("AGENT_LSP_WATCH_BACKEND", "")
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	w, _ := newTestFileWatcher(t)
	w.AddTree(root)

	excluded := filepath.Join(root, "node_modules", "x", "index.js")
	if err := os.WriteFile(excluded, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A later event on a watched file proves the excluded one was dropped, not delayed.
	marker := filepath.Join(root, "marker.py")
	if err := os.WriteFile(marker, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-w.Events():
			if ev.Name == excluded {
				t.Fatalf("event for excluded path %s", excluded)
			}
			if ev.Name == marker {
				return
			}
		case <-deadline:
			t.Fatal("no event for marker file")
		}
	}
}

func TestFSEvents_CloseIsIdempotentAndStopsAddTree(t *testing.T) {
	t.Setenv("AGENT_LSP_WATCH_BACKEND", "")
	w, _, err := newFileWatcher(loadWatcherLimits())
	if err != nil {
		t.Fatal(err)
	}
	w.AddTree(t.TempDir())
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w.AddTree(t.TempDir()) // must not start a stream on a closed watcher
}

func TestFSEventsOp(t *testing.T) {
	cases := []struct {
		name   string
		flags  uint32
		exists bool
		want   fsnotify.Op
		ok     bool
	}{
		{"created", fsevItemCreated, true, fsnotify.Create, true},
		{"created then written", fsevItemCreated | fsevItemModified, true, fsnotify.Create, true},
		{"modified", fsevItemModified, true, fsnotify.Write, true},
		{"touched", fsevItemInodeMeta, true, fsnotify.Write, true},
		{"removed", fsevItemRemoved, false, fsnotify.Remove, true},
		{"created then removed", fsevItemCreated | fsevItemRemoved, false, fsnotify.Remove, true},
		{"renamed away", fsevItemRenamed, false, fsnotify.Remove, true},
		{"renamed into place", fsevItemRenamed, true, fsnotify.Create, true},
		{"xattr only", 0x8000, true, 0, false},
	}
	for _, c := range cases {
		got, ok := fseventsOp(c.flags, c.exists)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: fseventsOp(0x%x, %v) = %v, %v; want %v, %v", c.name, c.flags, c.exists, got, ok, c.want, c.ok)
		}
	}
}

// A folder added inside an excluded directory of another root (a vendored
// module, a .claude worktree) must be filtered relative to itself, not dropped
// because the enclosing root excludes "vendor".
func TestFSEvents_NestedRootInsideExcludedDir(t *testing.T) {
	t.Setenv("AGENT_LSP_WATCH_BACKEND", "")
	root := t.TempDir()
	lib := filepath.Join(root, "vendor", "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	w, _ := newTestFileWatcher(t)
	w.AddTree(root)
	w.AddTree(lib)

	file := filepath.Join(lib, "mod.py")
	if err := os.WriteFile(file, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, file)
}

// On case-insensitive APFS a root spelled in another case must still map
// events: FSEvents reports the stored spelling, EvalSymlinks keeps the caller's.
func TestFSEvents_RootSpelledInOtherCase(t *testing.T) {
	t.Setenv("AGENT_LSP_WATCH_BACKEND", "")
	parent := t.TempDir()
	real := filepath.Join(parent, "CaseDir")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	lower := filepath.Join(parent, "casedir")
	if _, err := os.Stat(lower); err != nil {
		t.Skip("case-sensitive volume")
	}
	w, _ := newTestFileWatcher(t)
	if err := w.AddTree(lower); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "a.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, w, filepath.Join(lower, "a.py"))
}

// A dropped-events flag on a directory must turn into per-file events: a
// Changed event for a directory URI is ignored by most servers.
func TestFSEvents_DropRescansFiles(t *testing.T) {
	t.Setenv("AGENT_LSP_WATCH_BACKEND", "")
	root := t.TempDir()
	sub := filepath.Join(root, "pkg")
	mkDirWithFiles(t, sub, 3)
	mkDirWithFiles(t, filepath.Join(sub, "node_modules"), 2) // excluded
	w, _ := newTestFileWatcher(t)
	if err := w.AddTree(root); err != nil {
		t.Fatal(err)
	}
	fw := w.(*fseventsWatcher)
	real, _ := canonicalDir(sub)
	go fw.handle(real, fsevMustScanSubDirs)

	for i := 0; i < 3; i++ {
		ev := waitEvent(t, w, filepath.Join(sub, fmt.Sprintf("f%d", i)))
		if ev.Op != fsnotify.Write {
			t.Errorf("rescan op = %v, want Write", ev.Op)
		}
	}
	select {
	case ev := <-w.Events():
		if strings.Contains(ev.Name, "node_modules") {
			t.Fatalf("rescan reported excluded file %s", ev.Name)
		}
	case <-time.After(200 * time.Millisecond):
	}
}

// Re-adding a folder (remove + add workspace folder) must not stack streams.
func TestFSEvents_AddTreeDeduplicatesRoots(t *testing.T) {
	t.Setenv("AGENT_LSP_WATCH_BACKEND", "")
	root := t.TempDir()
	w, _ := newTestFileWatcher(t)
	for i := 0; i < 3; i++ {
		if err := w.AddTree(root); err != nil {
			t.Fatal(err)
		}
	}
	fw := w.(*fseventsWatcher)
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if len(fw.streams) != 1 || len(fw.roots) != 1 {
		t.Fatalf("streams=%d roots=%d, want 1 and 1", len(fw.streams), len(fw.roots))
	}
}

// A root FSEvents cannot stream must report an error, so startWatcher falls
// back to kqueue instead of running with no streams.
func TestFSEvents_AddTreeReportsFailure(t *testing.T) {
	t.Setenv("AGENT_LSP_WATCH_BACKEND", "")
	w, _ := newTestFileWatcher(t)
	if err := w.AddTree(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("AddTree on a missing root returned nil")
	}
	fw := w.(*fseventsWatcher)
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if len(fw.roots) != 0 {
		t.Fatalf("failed root left registered: %v", fw.roots)
	}
}

func TestNewFileWatcher_ForcedKqueue(t *testing.T) {
	t.Setenv("AGENT_LSP_WATCH_BACKEND", "kqueue")
	w, backend := newTestFileWatcher(t)
	if _, ok := w.(*kqueueWatcher); !ok || backend != "fsnotify" {
		t.Fatalf("got %T / %q, want *kqueueWatcher / fsnotify", w, backend)
	}
}
