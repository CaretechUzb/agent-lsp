package lsp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/blackwell-systems/agent-lsp/internal/config"
)

func clientPID(t *testing.T, c *LSPClient) int {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.cmd.Process == nil {
		t.Fatal("client has no server process")
	}
	return c.cmd.Process.Pid
}

// TestStartAll_RestartShutsDownPreviousClients guards the start_lsp leak: in
// multi-server mode every repeated start_lsp called StartAll, which replaced
// each entry's client without shutting the old one down. The old server
// processes, their pipes and their auto-watchers stayed alive, so each call
// added a full set of servers (and, with the kqueue watcher, a full tree of fds).
func TestStartAll_RestartShutsDownPreviousClients(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_LSP_TEST_INITIALIZE_CAPTURE", filepath.Join(root, "initialize.json"))
	t.Setenv("AGENT_LSP_DISABLE_WATCHER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	server := []string{executable, "-test.run=^TestCodeActionInitializeServer$"}
	m := NewMultiServerManager([]config.ServerEntry{
		{Extensions: []string{"py"}, Command: server, LanguageID: "python"},
		{Extensions: []string{"js"}, Command: server, LanguageID: "javascript"},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })

	if err := m.StartAll(ctx, root); err != nil {
		t.Fatal(err)
	}
	var oldPIDs []int
	for _, c := range m.AllClients() {
		oldPIDs = append(oldPIDs, clientPID(t, c))
	}

	if err := m.StartAll(ctx, root); err != nil {
		t.Fatal(err)
	}
	if n := len(m.AllClients()); n != 2 {
		t.Fatalf("AllClients = %d after restart, want 2", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range oldPIDs {
		for processAlive(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("server pid %d from the first StartAll is still running after restart", pid)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
