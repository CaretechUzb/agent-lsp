//go:build windows

package lsp

// lockDaemonSpawn is a no-op on Windows: session-scoped daemons
// (AGENT_LSP_SESSION_PID) are only used on POSIX hosts.
func lockDaemonSpawn(dir string) (func(), error) {
	return func() {}, nil
}
