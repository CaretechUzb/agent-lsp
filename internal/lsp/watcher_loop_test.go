package lsp

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/blackwell-systems/agent-lsp/internal/types"
	"github.com/fsnotify/fsnotify"
)

// fakeWatcher feeds runWatcherLoop without touching the filesystem.
type fakeWatcher struct {
	events chan fsnotify.Event
	errs   chan error
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{events: make(chan fsnotify.Event), errs: make(chan error)}
}

func (f *fakeWatcher) Events() <-chan fsnotify.Event { return f.events }
func (f *fakeWatcher) Errors() <-chan error          { return f.errs }
func (f *fakeWatcher) AddTree(string) error          { return nil }
func (f *fakeWatcher) WatchNewDir(string)            {}
func (f *fakeWatcher) Close() error                  { return nil }

// TestRunWatcherLoop_FlushIsNotConcurrent drives the loop with a tiny debounce
// so flushes constantly overlap incoming events. Before the loop ran flush on
// its own goroutine, flush ran on a time.AfterFunc goroutine and raced the
// loop's writes to pending: "concurrent map iteration and map write", fatal
// and unrecoverable. Run with -race.
func TestRunWatcherLoop_FlushIsNotConcurrent(t *testing.T) {
	c := NewLSPClient("unused", nil)
	var mu sync.Mutex
	seen := map[string]bool{}
	c.fileChangeCbs = append(c.fileChangeCbs, func(changes []types.FileChangeEvent) {
		mu.Lock()
		for _, ch := range changes {
			seen[ch.URI] = true
		}
		mu.Unlock()
	})

	fw := newFakeWatcher()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		c.runWatcherLoop(fw, stop, time.Microsecond)
		close(done)
	}()

	const n = 20000
	for i := 0; i < n; i++ {
		fw.events <- fsnotify.Event{Name: fmt.Sprintf("/ws/f%d.py", i%500), Op: fsnotify.Write}
	}
	close(stop)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 500 {
		t.Fatalf("flushed %d distinct paths, want 500", len(seen))
	}
}

// TestRunWatcherLoop_BatchCap flushes before the debounce once the batch is
// full, so an event storm cannot grow one batch without bound.
func TestRunWatcherLoop_BatchCap(t *testing.T) {
	c := NewLSPClient("unused", nil)
	var batches atomic.Int32
	c.fileChangeCbs = append(c.fileChangeCbs, func(changes []types.FileChangeEvent) {
		if len(changes) > watcherMaxBatch {
			t.Errorf("batch of %d exceeds cap %d", len(changes), watcherMaxBatch)
		}
		batches.Add(1)
	})

	fw := newFakeWatcher()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		c.runWatcherLoop(fw, stop, time.Hour) // only the cap or stop can flush
		close(done)
	}()
	for i := 0; i < watcherMaxBatch+1; i++ {
		fw.events <- fsnotify.Event{Name: fmt.Sprintf("/ws/f%d.py", i), Op: fsnotify.Write}
	}
	close(stop)
	<-done
	if got := batches.Load(); got != 2 {
		t.Fatalf("got %d batches, want 2 (one at the cap, one on stop)", got)
	}
}
