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

func fakeServerManager(t *testing.T) *ServerManager {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	server := []string{executable, "-test.run=^TestCodeActionInitializeServer$"}
	return NewMultiServerManager([]config.ServerEntry{
		{Extensions: []string{"py"}, Command: server, LanguageID: "python"},
		{Extensions: []string{"js"}, Command: server, LanguageID: "javascript"},
	})
}

// A restart whose new servers fail to start must leave the previous, working
// set in place instead of dead or missing clients.
func TestStartAll_FailedRestartKeepsOldClients(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_LSP_TEST_INITIALIZE_CAPTURE", filepath.Join(root, "initialize.json"))
	t.Setenv("AGENT_LSP_DISABLE_WATCHER", "1")
	m := fakeServerManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })

	if err := m.StartAll(ctx, root); err != nil {
		t.Fatal(err)
	}
	before := m.AllClients()

	t.Setenv("AGENT_LSP_TEST_FAIL_INIT", "1") // inherited by the new servers
	if err := m.StartAll(ctx, root); err == nil {
		t.Fatal("StartAll succeeded although every new server fails to initialize")
	}
	after := m.AllClients()
	if len(after) != len(before) {
		t.Fatalf("AllClients = %d after failed restart, want %d", len(after), len(before))
	}
	for i := range before {
		if after[i] != before[i] {
			t.Errorf("entry %d was replaced by a failed restart", i)
		}
		if !processAlive(clientPID(t, before[i])) {
			t.Errorf("entry %d: previous server was shut down by a failed restart", i)
		}
	}
}

// A server that stopped reading stdin used to block Shutdown, and with it
// StartAll, forever: sendRequest only starts its timeout after the write.
func TestShutdown_BoundedWhenServerIsWedged(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the shutdown grace period")
	}
	root := t.TempDir()
	t.Setenv("AGENT_LSP_TEST_INITIALIZE_CAPTURE", filepath.Join(root, "initialize.json"))
	t.Setenv("AGENT_LSP_DISABLE_WATCHER", "1")
	t.Setenv("AGENT_LSP_TEST_STALL_AFTER_INIT", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := NewLSPClient(executable, []string{"-test.run=^TestCodeActionInitializeServer$"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Initialize(ctx, root); err != nil {
		t.Fatal(err)
	}
	pid := clientPID(t, c)

	start := time.Now()
	_ = c.Shutdown(context.Background())
	if d := time.Since(start); d > shutdownGrace+3*time.Second {
		t.Fatalf("Shutdown took %s, want about %s", d, shutdownGrace)
	}
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("wedged server pid %d still running after Shutdown", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestShutdown_RunsHooksOnce(t *testing.T) {
	c := NewLSPClient("unused", nil)
	calls := 0
	c.OnShutdown(func() { calls++ })
	_ = c.Shutdown(context.Background())
	_ = c.Shutdown(context.Background())
	if calls != 1 {
		t.Fatalf("shutdown hook ran %d times, want 1", calls)
	}
}

// Restart reuses the client, so its OnShutdown hooks (notification
// subscriptions) must survive it; only a final Shutdown runs them.
func TestRestart_KeepsShutdownHooks(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AGENT_LSP_TEST_INITIALIZE_CAPTURE", filepath.Join(root, "initialize.json"))
	t.Setenv("AGENT_LSP_DISABLE_WATCHER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := NewLSPClient(executable, []string{"-test.run=^TestCodeActionInitializeServer$"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Initialize(ctx, root); err != nil {
		t.Fatal(err)
	}
	calls := 0
	c.OnShutdown(func() { calls++ })
	if err := c.Restart(ctx, root); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("Restart ran the shutdown hooks %d times, want 0", calls)
	}
	_ = c.Shutdown(ctx)
	if calls != 1 {
		t.Fatalf("Shutdown ran the hooks %d times, want 1", calls)
	}
}

// Restarting a wedged server kills it; the old process's exit monitor used to
// fire after the new process started and mark the client exited, with its
// stdin nil and its requests rejected.
func TestRestart_WedgedServerLeavesWorkingClient(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the shutdown grace period")
	}
	root := t.TempDir()
	t.Setenv("AGENT_LSP_TEST_INITIALIZE_CAPTURE", filepath.Join(root, "initialize.json"))
	t.Setenv("AGENT_LSP_DISABLE_WATCHER", "1")
	t.Setenv("AGENT_LSP_TEST_STALL_AFTER_INIT", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := NewLSPClient(executable, []string{"-test.run=^TestCodeActionInitializeServer$"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Initialize(ctx, root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	if err := c.Restart(ctx, root); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	time.Sleep(200 * time.Millisecond) // let a stale exit monitor run, if any
	c.mu.Lock()
	initialized, exited, stdinNil := c.initialized, c.exited, c.stdin == nil
	c.mu.Unlock()
	if !initialized || exited || stdinNil {
		t.Fatalf("after restart: initialized=%v exited=%v stdinNil=%v, want true/false/false", initialized, exited, stdinNil)
	}
}
