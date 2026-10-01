// daemon.go manages persistent LSP daemon brokers for language servers that
// need sustained background indexing (pyright, tsserver). The daemon keeps the
// language server alive between agent sessions so the workspace stays indexed.
//
// Architecture:
//
//	agent-lsp (ephemeral MCP session)
//	    → connects via Unix socket
//	daemon-broker (persistent, one per root+language)
//	    → stdio pipes
//	pyright-langserver / tsserver (persistent)
package lsp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/blackwell-systems/agent-lsp/internal/logging"
)

// DaemonInfo holds metadata about a running daemon broker.
type DaemonInfo struct {
	RootDir      string    `json:"root_dir"`
	LanguageID   string    `json:"language_id"`
	Command      []string  `json:"command"`
	SocketPath   string    `json:"socket_path"`
	PID          int       `json:"pid"`
	Ready        bool      `json:"ready"`
	StartTime    time.Time `json:"start_time"`
	LastActivity time.Time `json:"last_activity"`
	SessionPID   int       `json:"session_pid,omitempty"`
}

// SessionPID returns the PID of the MCP host session this process belongs to,
// from AGENT_LSP_SESSION_PID, or 0 when unset or invalid.
//
// When set, every agent-lsp process of that session (an MCP host such as
// Codex spawns one per subagent thread) shares one daemon broker per
// root+language, and the broker exits once the session process is gone. The
// spawned broker inherits the variable, so both sides derive the same
// DaemonDir.
func SessionPID() int {
	pid, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AGENT_LSP_SESSION_PID")))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// daemonLanguages is the allowlist of languages that benefit from daemon mode.
// These servers need sustained indexing time before references work.
var daemonLanguages = map[string]bool{
	"python":          true,
	"typescript":      true,
	"typescriptreact": true,
	"javascript":      true,
	"javascriptreact": true,
}

// NeedsDaemon returns true if the given language benefits from persistent
// daemon mode due to slow workspace indexing.
func NeedsDaemon(languageID string) bool {
	return daemonLanguages[strings.ToLower(languageID)]
}

// DaemonDir returns the directory for a daemon's state files.
// Path: ~/.cache/agent-lsp/daemons/<hash>/
// A session-scoped daemon (see SessionPID) hashes the session PID in too.
func DaemonDir(rootDir, languageID string) string {
	key := rootDir + "\x00" + languageID
	if pid := SessionPID(); pid > 0 {
		key += "\x00session:" + strconv.Itoa(pid)
	}
	h := sha256.Sum256([]byte(key))
	hash := hex.EncodeToString(h[:])[:12]
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "agent-lsp", "daemons", hash)
}

// FindRunningDaemon checks if a daemon is already running for the given workspace.
// Returns nil if no daemon exists or it's stale.
func FindRunningDaemon(rootDir, languageID string) (*DaemonInfo, error) {
	dir := DaemonDir(rootDir, languageID)
	infoPath := filepath.Join(dir, "daemon.json")

	data, err := os.ReadFile(infoPath)
	if err != nil {
		return nil, nil // no daemon
	}

	var info DaemonInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, nil // corrupt state, treat as no daemon
	}

	// Verify the process is still alive. Only this check warrants
	// wiping the registry — a dead PID can never come back, so cleanup
	// is safe. CleanupStaleDaemons performs the same check periodically.
	if !processAlive(info.PID) {
		logging.Log(logging.LevelDebug, fmt.Sprintf("daemon: stale PID %d for %s, cleaning up", info.PID, languageID))
		cleanupDaemonDir(dir)
		return nil, nil
	}

	// Verify socket is reachable. A failure here can mean two things:
	//   (1) the broker is alive but its socket file is on a Windows
	//       filesystem path that AF_UNIX dial can't reach across the
	//       detached process group, or
	//   (2) the broker is actually wedged.
	// Either way, do NOT cleanup the registry — wiping daemon.json on
	// a still-alive broker leaks the broker process AND triggers a
	// fresh spawn that conflicts with the existing socket bind. If
	// (2) is the real cause, CleanupStaleDaemons (which only checks
	// processAlive) will catch it once the broker actually exits.
	conn, err := net.DialTimeout("unix", info.SocketPath, 2*time.Second)
	if err != nil {
		logging.Log(logging.LevelDebug, fmt.Sprintf("daemon: socket unreachable for live PID %d (%v); keeping registry, returning nil for now", info.PID, err))
		return nil, nil
	}
	conn.Close()

	return &info, nil
}

// WriteDaemonInfo writes the daemon metadata to disk.
func WriteDaemonInfo(info *DaemonInfo) error {
	dir := DaemonDir(info.RootDir, info.LanguageID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename: the broker rewrites this file on every connection,
	// and a reader catching an in-place write half done would see corrupt
	// JSON, take the daemon for absent and spawn a second one.
	tmp, err := os.CreateTemp(dir, "daemon.json.*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "daemon.json"))
}

// RefreshDaemonInfo re-reads daemon.json from disk to get the latest ready status.
func RefreshDaemonInfo(rootDir, languageID string) (*DaemonInfo, error) {
	dir := DaemonDir(rootDir, languageID)
	data, err := os.ReadFile(filepath.Join(dir, "daemon.json"))
	if err != nil {
		return nil, err
	}
	var info DaemonInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// CleanupStaleDaemons removes daemon state for processes that are no longer running.
func CleanupStaleDaemons() {
	home, _ := os.UserHomeDir()
	daemonsDir := filepath.Join(home, ".cache", "agent-lsp", "daemons")
	entries, err := os.ReadDir(daemonsDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(daemonsDir, entry.Name())
		pidData, err := os.ReadFile(filepath.Join(dir, "daemon.pid"))
		if err != nil {
			cleanupDaemonDir(dir)
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
		if err != nil || !processAlive(pid) {
			cleanupDaemonDir(dir)
		}
	}
}

// StopDaemon asks the platform's process-termination mechanism to stop
// the daemon broker — SIGTERM on POSIX, TerminateProcess on Windows.
// The previous implementation always sent syscall.SIGTERM which Go's
// Windows runtime rejects ("not supported by windows"), making the
// `agent-lsp daemon-stop` CLI a no-op on Windows.
func StopDaemon(rootDir, languageID string) error {
	info, err := FindRunningDaemon(rootDir, languageID)
	if err != nil || info == nil {
		return fmt.Errorf("no running daemon found for %s at %s", languageID, rootDir)
	}
	if err := terminateProcess(info.PID); err != nil {
		return fmt.Errorf("terminating daemon PID %d: %w", info.PID, err)
	}
	return nil
}

// StopListedDaemon stops a broker as returned by ListDaemons, by its PID.
// Unlike StopDaemon it does not re-derive the DaemonDir, so it also reaches
// session-scoped brokers from a shell that has no AGENT_LSP_SESSION_PID.
func StopListedDaemon(info *DaemonInfo) error {
	if !processAlive(info.PID) {
		return fmt.Errorf("daemon PID %d is not running", info.PID)
	}
	if err := terminateProcess(info.PID); err != nil {
		return fmt.Errorf("terminating daemon PID %d: %w", info.PID, err)
	}
	return nil
}

// ListDaemons returns info about all currently running daemons.
func ListDaemons() []*DaemonInfo {
	home, _ := os.UserHomeDir()
	daemonsDir := filepath.Join(home, ".cache", "agent-lsp", "daemons")
	entries, err := os.ReadDir(daemonsDir)
	if err != nil {
		return nil
	}
	var result []*DaemonInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(daemonsDir, entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "daemon.json"))
		if err != nil {
			continue
		}
		var info DaemonInfo
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}
		if processAlive(info.PID) {
			result = append(result, &info)
		}
	}
	return result
}

// processAlive checks if a process with the given PID exists.
// processAlive is split into platform-specific files. The signal(0)
// idiom works on POSIX but is rejected on Windows ("not supported by
// windows"), which made every poll falsely conclude the daemon was
// dead and wipe its registry. See process_alive_*.go.

func cleanupDaemonDir(dir string) {
	os.RemoveAll(dir)
}
