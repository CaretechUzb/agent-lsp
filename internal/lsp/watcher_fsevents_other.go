//go:build !darwin || ios

package lsp

// newPlatformWatcher has no recursive, fd-free backend here; inotify (linux)
// and ReadDirectoryChangesW (windows) behind fsnotify do not open per-file fds.
func newPlatformWatcher() (fileWatcher, error) {
	return nil, errNoPlatformWatcher
}
