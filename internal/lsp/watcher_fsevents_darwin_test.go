//go:build darwin && !ios

package lsp

import (
	"os"
	"path/filepath"
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
		{"created then written", fsevItemCreated | fsevItemModified, true, fsnotify.Write, true},
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
