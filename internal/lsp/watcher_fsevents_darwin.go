//go:build darwin && !ios

package lsp

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/blackwell-systems/agent-lsp/internal/logging"
	"github.com/ebitengine/purego"
	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

// FSEvents backend for the auto-watcher. One FSEventStream watches a whole
// tree recursively without opening per-file fds, unlike the kqueue backend
// (issue #18). The CoreServices API is called through purego, so the release
// stays CGO_ENABLED=0 and cross-compiles from Linux.

const (
	fsevCreateFlagNoDefer    = 0x00000002
	fsevCreateFlagWatchRoot  = 0x00000004
	fsevCreateFlagFileEvents = 0x00000010

	fsevMustScanSubDirs = 0x00000001
	fsevUserDropped     = 0x00000002
	fsevKernelDropped   = 0x00000004
	fsevRootChanged     = 0x00000020
	fsevItemCreated     = 0x00000100
	fsevItemRemoved     = 0x00000200
	fsevItemInodeMeta   = 0x00000400
	fsevItemRenamed     = 0x00000800
	fsevItemModified    = 0x00001000

	fsevEventIDSinceNow  = ^uint64(0)
	cfStringEncodingUTF8 = 0x08000100

	// fsevLatency is how long FSEvents coalesces before delivering; the
	// watcher loop debounces again, so keep it short.
	fsevLatency = 0.05
)

// fsEventStreamContext mirrors the C FSEventStreamContext struct.
type fsEventStreamContext struct {
	version         int
	info            uintptr
	retain          uintptr
	release         uintptr
	copyDescription uintptr
}

var (
	cfStringCreateWithCString     func(alloc uintptr, s string, encoding uint32) uintptr
	cfArrayCreate                 func(alloc uintptr, values *uintptr, n int, callbacks uintptr) uintptr
	cfRelease                     func(ref uintptr)
	fsEventStreamCreate           func(alloc uintptr, cb uintptr, ctx *fsEventStreamContext, paths uintptr, since uint64, latency float64, flags uint32) uintptr
	fsEventStreamSetDispatchQueue func(stream, queue uintptr)
	fsEventStreamStart            func(stream uintptr) bool
	fsEventStreamStop             func(stream uintptr)
	fsEventStreamInvalidate       func(stream uintptr)
	fsEventStreamRelease          func(stream uintptr)
	dispatchQueueCreate           func(label string, attr uintptr) uintptr
	dispatchSyncF                 func(queue, ctx, work uintptr)
	dispatchRelease               func(obj uintptr)

	cfTypeArrayCallBacks uintptr
	fsevCallback         uintptr // single purego callback: purego never frees callbacks
	fsevNoop             uintptr // drains a dispatch queue via dispatch_sync_f

	fsevLoadOnce sync.Once
	fsevLoadErr  error

	fsevRegistryMu sync.Mutex
	fsevRegistry   = map[uintptr]*fseventsWatcher{}
	fsevNextID     uintptr
)

func loadFSEvents() (err error) {
	defer func() {
		if r := recover(); r != nil { // RegisterLibFunc panics on a missing symbol
			err = fmt.Errorf("load FSEvents symbols: %v", r)
		}
	}()
	cf, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return err
	}
	cs, err := purego.Dlopen("/System/Library/Frameworks/CoreServices.framework/CoreServices", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return err
	}
	sys, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return err
	}
	purego.RegisterLibFunc(&cfStringCreateWithCString, cf, "CFStringCreateWithCString")
	purego.RegisterLibFunc(&cfArrayCreate, cf, "CFArrayCreate")
	purego.RegisterLibFunc(&cfRelease, cf, "CFRelease")
	if cfTypeArrayCallBacks, err = purego.Dlsym(cf, "kCFTypeArrayCallBacks"); err != nil {
		return err
	}
	purego.RegisterLibFunc(&fsEventStreamCreate, cs, "FSEventStreamCreate")
	purego.RegisterLibFunc(&fsEventStreamSetDispatchQueue, cs, "FSEventStreamSetDispatchQueue")
	purego.RegisterLibFunc(&fsEventStreamStart, cs, "FSEventStreamStart")
	purego.RegisterLibFunc(&fsEventStreamStop, cs, "FSEventStreamStop")
	purego.RegisterLibFunc(&fsEventStreamInvalidate, cs, "FSEventStreamInvalidate")
	purego.RegisterLibFunc(&fsEventStreamRelease, cs, "FSEventStreamRelease")
	purego.RegisterLibFunc(&dispatchQueueCreate, sys, "dispatch_queue_create")
	purego.RegisterLibFunc(&dispatchSyncF, sys, "dispatch_sync_f")
	purego.RegisterLibFunc(&dispatchRelease, sys, "dispatch_release")
	fsevCallback = purego.NewCallback(fseventsCallback)
	fsevNoop = purego.NewCallback(func(ctx uintptr) {})
	return nil
}

// fsevRoot is a watched root and its on-disk form: FSEvents reports real paths
// (e.g. /private/var for /var, the stored letter case on a case-insensitive
// volume), which are mapped back to root.
type fsevRoot struct {
	root, real string
}

// fsevRaw is one path from a callback batch, handled off the dispatch queue.
type fsevRaw struct {
	path  string
	flags uint32
}

const (
	// fsevMaxQueued bounds raw events buffered between the callback and the
	// worker; past it the worker rescans the roots instead.
	fsevMaxQueued = 100000
	// fsevMaxRescan bounds the files one rescan reports.
	fsevMaxRescan = 20000
)

type fseventsWatcher struct {
	id     uintptr
	events chan fsnotify.Event
	errors chan error
	done   chan struct{}
	queue  uintptr

	mu      sync.Mutex
	closed  bool
	streams []uintptr
	roots   []fsevRoot

	// The callback only appends here and wakes the worker, so the dispatch
	// queue never blocks on a slow consumer or a filesystem walk.
	qmu      sync.Mutex
	raw      []fsevRaw
	overflow bool
	wake     chan struct{}
}

func newPlatformWatcher() (fileWatcher, error) {
	fsevLoadOnce.Do(func() { fsevLoadErr = loadFSEvents() })
	if fsevLoadErr != nil {
		return nil, fsevLoadErr
	}
	w := &fseventsWatcher{
		events: make(chan fsnotify.Event, 4096),
		errors: make(chan error, 16),
		done:   make(chan struct{}),
		wake:   make(chan struct{}, 1),
		queue:  dispatchQueueCreate("agent-lsp.fsevents\x00", 0),
	}
	if w.queue == 0 {
		return nil, errors.New("dispatch_queue_create failed")
	}
	fsevRegistryMu.Lock()
	fsevNextID++
	w.id = fsevNextID
	fsevRegistry[w.id] = w
	fsevRegistryMu.Unlock()
	go w.work()
	return w, nil
}

func (w *fseventsWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *fseventsWatcher) Errors() <-chan error          { return w.errors }
func (w *fseventsWatcher) WatchNewDir(string)            {}

func (w *fseventsWatcher) AddTree(root string) error {
	if err := w.addStream(root); err != nil {
		return fmt.Errorf("FSEvents: %w", err)
	}
	logging.Log(logging.LevelDebug, "auto-watcher: FSEvents watching "+root)
	return nil
}

func (w *fseventsWatcher) addStream(root string) error {
	real, err := canonicalDir(root)
	if err != nil {
		return err
	}
	r := fsevRoot{root: filepath.Clean(root), real: real}

	// Hold w.mu for the whole start: Close takes it before releasing the
	// dispatch queue, so the queue stays valid while the stream attaches to
	// it. Neither the callback nor the worker blocks on w.mu while holding
	// something this path needs, so this cannot deadlock.
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("watcher closed")
	}
	for _, have := range w.roots {
		if have.real == real {
			return nil // already streamed; re-adding a folder must not stack streams
		}
	}
	// Register the root before the stream starts so the first events map back.
	w.roots = append(w.roots, r)
	stream, err := w.startStream(real)
	if err != nil {
		w.roots = w.roots[:len(w.roots)-1]
		return err
	}
	w.streams = append(w.streams, stream)
	return nil
}

func (w *fseventsWatcher) startStream(real string) (uintptr, error) {
	path := cfStringCreateWithCString(0, real, cfStringEncodingUTF8)
	if path == 0 {
		return 0, errors.New("CFStringCreateWithCString failed")
	}
	defer cfRelease(path)
	paths := cfArrayCreate(0, &path, 1, cfTypeArrayCallBacks)
	if paths == 0 {
		return 0, errors.New("CFArrayCreate failed")
	}
	defer cfRelease(paths)

	ctx := fsEventStreamContext{info: w.id}
	stream := fsEventStreamCreate(0, fsevCallback, &ctx, paths, fsevEventIDSinceNow, fsevLatency,
		fsevCreateFlagFileEvents|fsevCreateFlagNoDefer|fsevCreateFlagWatchRoot)
	if stream == 0 {
		return 0, errors.New("FSEventStreamCreate failed")
	}
	fsEventStreamSetDispatchQueue(stream, w.queue)
	if !fsEventStreamStart(stream) {
		fsEventStreamInvalidate(stream)
		fsEventStreamRelease(stream)
		return 0, errors.New("FSEventStreamStart failed")
	}
	return stream, nil
}

func (w *fseventsWatcher) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	streams := w.streams
	w.streams = nil
	w.mu.Unlock()

	// Stop the worker, then stop delivery and drain the serial queue so no
	// callback still reads stream memory when it is released.
	close(w.done)
	fsevRegistryMu.Lock()
	delete(fsevRegistry, w.id)
	fsevRegistryMu.Unlock()
	for _, s := range streams {
		fsEventStreamStop(s)
	}
	dispatchSyncF(w.queue, 0, fsevNoop)
	for _, s := range streams {
		fsEventStreamInvalidate(s)
		fsEventStreamRelease(s)
	}
	dispatchRelease(w.queue)
	return nil
}

// fseventsCallback is the FSEventStreamCallback; it runs on the watcher's
// dispatch queue. Without kFSEventStreamCreateFlagUseCFTypes, eventPaths is a
// char** array. It copies the batch and returns; the worker does the rest.
func fseventsCallback(stream, info, n uintptr, eventPaths, eventFlags, eventIDs unsafe.Pointer) {
	fsevRegistryMu.Lock()
	w := fsevRegistry[info]
	fsevRegistryMu.Unlock()
	if w == nil || n == 0 {
		return
	}
	paths := unsafe.Slice((**byte)(eventPaths), n)
	flags := unsafe.Slice((*uint32)(eventFlags), n)
	w.qmu.Lock()
	for i := range paths {
		if len(w.raw) >= fsevMaxQueued {
			w.overflow = true
			break
		}
		w.raw = append(w.raw, fsevRaw{path: cString(paths[i]), flags: flags[i]})
	}
	w.qmu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// work drains callback batches into events until Close.
func (w *fseventsWatcher) work() {
	for {
		select {
		case <-w.done:
			return
		case <-w.wake:
		}
		w.qmu.Lock()
		batch, overflow := w.raw, w.overflow
		w.raw, w.overflow = nil, false
		w.qmu.Unlock()
		if overflow {
			logging.Log(logging.LevelWarning, "auto-watcher: FSEvents backlog overflowed; rescanning watched roots")
			// A rescan only sees files that exist, so report the batch's
			// deletions first.
			for _, ev := range batch {
				if name, root, ok := w.mapPath(ev.path); ok && !watchPathExcluded(root, name) && !pathExists(name) {
					w.send(fsnotify.Event{Name: name, Op: fsnotify.Remove})
				}
			}
			w.mu.Lock()
			roots := append([]fsevRoot(nil), w.roots...)
			w.mu.Unlock()
			for _, r := range roots {
				w.rescan(r.root, r.root)
			}
			continue // the rescan covers the rest of the batch
		}
		for _, ev := range batch {
			w.handle(ev.path, ev.flags)
		}
	}
}

func (w *fseventsWatcher) handle(path string, flags uint32) {
	if flags&fsevRootChanged != 0 {
		// The watched root was moved, deleted or recreated; the stream keeps
		// watching the old inode path and may see nothing more.
		logging.Log(logging.LevelWarning, "auto-watcher: FSEvents watched root changed: "+path+"; restart the language server to re-watch it")
		return
	}
	name, root, ok := w.mapPath(path)
	// Also drop events on an excluded directory itself (node_modules being
	// created), not only on paths below it.
	if !ok || watchPathExcluded(root, name) || (name != root && watcherSkipDirs[filepath.Base(name)]) {
		return
	}
	if flags&(fsevMustScanSubDirs|fsevUserDropped|fsevKernelDropped) != 0 {
		// Events below name were coalesced or lost. A Changed event for a
		// directory is ignored by most servers, so report its files instead.
		logging.Log(logging.LevelWarning, fmt.Sprintf("auto-watcher: FSEvents dropped events under %s (flags 0x%x); rescanning", name, flags))
		w.rescan(root, name)
		return
	}
	if op, ok := fseventsOp(flags, pathExists(name)); ok {
		w.send(fsnotify.Event{Name: name, Op: op})
	}
}

// rescan reports every non-excluded file under dir as written, up to
// fsevMaxRescan files.
func (w *fseventsWatcher) rescan(root, dir string) {
	n := 0
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		select {
		case <-w.done:
			return filepath.SkipAll
		default:
		}
		if d.IsDir() {
			if path != dir && (watcherSkipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if watchPathExcluded(root, path) || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if n++; n > fsevMaxRescan {
			logging.Log(logging.LevelWarning, fmt.Sprintf("auto-watcher: rescan of %s stopped after %d files", dir, fsevMaxRescan))
			return filepath.SkipAll
		}
		w.send(fsnotify.Event{Name: path, Op: fsnotify.Write})
		return nil
	})
}

func (w *fseventsWatcher) send(ev fsnotify.Event) {
	select {
	case w.events <- ev:
	case <-w.done:
	}
}

// canonicalDir returns dir's path as stored on disk: symlinks resolved, and
// the stored letter case and Unicode form that FSEvents reports on
// case-insensitive APFS. EvalSymlinks alone keeps the caller's spelling.
func canonicalDir(dir string) (string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	defer f.Close()
	// The kernel writes into this buffer through an address passed as an int,
	// which the runtime does not track: it must not live on a goroutine stack
	// that could move during the call. A package-level array never does.
	canonMu.Lock()
	defer canonMu.Unlock()
	if _, err := unix.FcntlInt(f.Fd(), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&canonBuf[0])))); err != nil {
		return filepath.EvalSymlinks(dir)
	}
	path := canonBuf[:]
	if i := bytes.IndexByte(path, 0); i >= 0 {
		path = path[:i]
	}
	if len(path) == 0 {
		return filepath.EvalSymlinks(dir)
	}
	return filepath.Clean(string(path)), nil
}

var (
	canonMu  sync.Mutex
	canonBuf [unix.PathMax]byte
)

// mapPath maps a reported real path back under the root it was watched as.
// The most specific root wins, so a folder added inside an excluded directory
// of another root (e.g. /repo/vendor/lib) is filtered relative to itself.
func (w *fseventsWatcher) mapPath(path string) (name, root string, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	best := -1
	for i, r := range w.roots {
		if path == r.real || strings.HasPrefix(path, strings.TrimSuffix(r.real, string(filepath.Separator))+string(filepath.Separator)) {
			if best < 0 || len(r.real) > len(w.roots[best].real) {
				best = i
			}
		}
	}
	if best < 0 {
		return "", "", false
	}
	r := w.roots[best]
	return filepath.Join(r.root, strings.TrimPrefix(path, r.real)), r.root, true
}

// fseventsOp converts coalesced FSEvents item flags to an fsnotify op. FSEvents
// accumulates flags per path within a batch (a file created then written reports
// Created|Modified), so existence on disk decides between delete and create.
// Pure attribute noise (xattr, Finder info, owner) yields ok=false.
func fseventsOp(flags uint32, exists bool) (fsnotify.Op, bool) {
	if flags&(fsevItemCreated|fsevItemRemoved|fsevItemRenamed|fsevItemModified|fsevItemInodeMeta) == 0 {
		return 0, false
	}
	switch {
	case !exists:
		return fsnotify.Remove, true
	case flags&(fsevItemCreated|fsevItemRenamed) != 0:
		// A create is usually followed by a write in the same batch; report it
		// as created so servers index the new file. A stale Created flag on a
		// later write only makes the server re-read an existing file.
		return fsnotify.Create, true
	default:
		return fsnotify.Write, true
	}
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// cString copies a NUL-terminated C string into Go memory.
func cString(p *byte) string {
	if p == nil {
		return ""
	}
	n := 0
	for *(*byte)(unsafe.Add(unsafe.Pointer(p), n)) != 0 {
		n++
	}
	return string(unsafe.Slice(p, n))
}
