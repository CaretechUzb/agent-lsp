//go:build darwin && !ios

package lsp

import (
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

// fsevRoot is a watched root and its symlink-resolved form: FSEvents reports
// real paths (e.g. /private/var for /var), which are mapped back to root.
type fsevRoot struct {
	root, real string
}

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
	return w, nil
}

func (w *fseventsWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *fseventsWatcher) Errors() <-chan error          { return w.errors }
func (w *fseventsWatcher) WatchNewDir(string)            {}

func (w *fseventsWatcher) AddTree(root string) {
	if err := w.addStream(root); err != nil {
		logging.Log(logging.LevelWarning, fmt.Sprintf("auto-watcher: FSEvents cannot watch %s: %v", root, err))
		return
	}
	logging.Log(logging.LevelDebug, "auto-watcher: FSEvents watching "+root)
}

func (w *fseventsWatcher) addStream(root string) error {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("watcher closed")
	}
	// Register the root before the stream starts so the first events map back.
	w.roots = append(w.roots, fsevRoot{root: filepath.Clean(root), real: real})

	path := cfStringCreateWithCString(0, real, cfStringEncodingUTF8)
	if path == 0 {
		return errors.New("CFStringCreateWithCString failed")
	}
	defer cfRelease(path)
	paths := cfArrayCreate(0, &path, 1, cfTypeArrayCallBacks)
	if paths == 0 {
		return errors.New("CFArrayCreate failed")
	}
	defer cfRelease(paths)

	ctx := fsEventStreamContext{info: w.id}
	stream := fsEventStreamCreate(0, fsevCallback, &ctx, paths, fsevEventIDSinceNow, fsevLatency,
		fsevCreateFlagFileEvents|fsevCreateFlagNoDefer|fsevCreateFlagWatchRoot)
	if stream == 0 {
		return errors.New("FSEventStreamCreate failed")
	}
	fsEventStreamSetDispatchQueue(stream, w.queue)
	if !fsEventStreamStart(stream) {
		fsEventStreamInvalidate(stream)
		fsEventStreamRelease(stream)
		return errors.New("FSEventStreamStart failed")
	}
	w.streams = append(w.streams, stream)
	return nil
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

	// Unblock any callback waiting to send, then stop delivery and drain the
	// serial queue so no callback still reads stream memory when it is released.
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
// char** array.
func fseventsCallback(stream, info, n uintptr, eventPaths, eventFlags, eventIDs unsafe.Pointer) {
	fsevRegistryMu.Lock()
	w := fsevRegistry[info]
	fsevRegistryMu.Unlock()
	if w == nil || n == 0 {
		return
	}
	paths := unsafe.Slice((**byte)(eventPaths), n)
	flags := unsafe.Slice((*uint32)(eventFlags), n)
	for i := range paths {
		w.handle(cString(paths[i]), flags[i])
	}
}

func (w *fseventsWatcher) handle(path string, flags uint32) {
	name, root, ok := w.mapPath(path)
	if !ok || watchPathExcluded(root, name) {
		return
	}
	if flags&(fsevMustScanSubDirs|fsevUserDropped|fsevKernelDropped) != 0 {
		select {
		case w.errors <- fmt.Errorf("FSEvents dropped events under %s (flags 0x%x)", name, flags):
		default:
		}
		// The server must rescan; report the directory as changed.
		w.send(fsnotify.Event{Name: name, Op: fsnotify.Write})
		return
	}
	if op, ok := fseventsOp(flags, pathExists(name)); ok {
		w.send(fsnotify.Event{Name: name, Op: op})
	}
}

func (w *fseventsWatcher) send(ev fsnotify.Event) {
	select {
	case w.events <- ev:
	case <-w.done:
	}
}

// mapPath maps a reported real path back under the root it was watched as.
func (w *fseventsWatcher) mapPath(path string) (name, root string, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range w.roots {
		if path == r.real || strings.HasPrefix(path, r.real+string(filepath.Separator)) {
			return r.root + strings.TrimPrefix(path, r.real), r.root, true
		}
	}
	return "", "", false
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
	case flags&fsevItemRenamed != 0,
		flags&fsevItemCreated != 0 && flags&(fsevItemModified|fsevItemRemoved) == 0:
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
