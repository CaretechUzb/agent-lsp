//go:build !windows

package lsp

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockDaemonSpawn takes an exclusive flock on the sibling file "<dir>.lock",
// blocking until it is free, and returns the release func. The lock lives
// beside dir, not in it: CleanupStaleDaemons wipes a dir without daemon.pid.
func lockDaemonSpawn(dir string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
