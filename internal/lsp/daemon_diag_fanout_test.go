package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/blackwell-systems/agent-lsp/internal/types"
)

func publishDiag(t *testing.T, w interface{ Write([]byte) (int, error) }, uri string, msgs ...string) {
	t.Helper()
	diags := []any{}
	for _, m := range msgs {
		diags = append(diags, map[string]any{"message": m})
	}
	if err := writeMsg(w, map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/publishDiagnostics",
		"params":  map[string]any{"uri": uri, "diagnostics": diags},
	}); err != nil {
		t.Fatalf("write diag: %v", err)
	}
}

// A URI with nothing cached gets no replay, so its first notification is
// fresh and must count (it used to be skipped as a presumed replay).
func TestWaitForDiagnostics_UncachedFirstNotificationCounts(t *testing.T) {
	c, serverW, _ := newTestClient(t)
	uri := "file:///fresh.py"

	done := make(chan error, 1)
	go func() { done <- WaitForDiagnostics(context.Background(), c, []string{uri}, 5000) }()

	waitForDiagSubscribers(t, c, 1)
	publishDiag(t, serverW, uri, "boom")

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitForDiagnostics ignored the first fresh notification for an uncached URI")
	}
	if got := c.GetDiagnostics(uri); len(got) != 1 || got[0].Message != "boom" {
		t.Fatalf("diagnostics = %+v, want one 'boom'", got)
	}
}

// With AGENT_LSP_DIAG_CACHED_SETTLE_MS set, a cached URI that the server never
// republishes (unchanged reopen) settles on the cache instead of the timeout.
func TestWaitForDiagnostics_CachedSettle(t *testing.T) {
	c, _, _ := newTestClient(t)
	uri := "file:///unchanged.py"
	c.diagMu.Lock()
	c.diags[uri] = []types.LSPDiagnostic{{Message: "cached"}}
	c.diagMu.Unlock()

	old := diagCachedSettle
	diagCachedSettle = 200 * time.Millisecond
	defer func() { diagCachedSettle = old }()

	start := time.Now()
	if err := WaitForDiagnostics(context.Background(), c, []string{uri}, 5000); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("took %v, want ~cached-settle window, not the 5s timeout", el)
	}
}

func readForwarded(t *testing.T, r *bufio.Reader) (string, []types.LSPDiagnostic) {
	t.Helper()
	body, err := readFramedMessage(r)
	if err != nil {
		t.Fatalf("read forwarded: %v", err)
	}
	var msg struct {
		Method string `json:"method"`
		Params struct {
			URI         string                `json:"uri"`
			Diagnostics []types.LSPDiagnostic `json:"diagnostics"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.Method != "textDocument/publishDiagnostics" {
		t.Fatalf("method = %q", msg.Method)
	}
	return msg.Params.URI, msg.Params.Diagnostics
}

// The broker forwards replayed and live diagnostics, and a live update beats
// a stale replay of the same URI.
func TestBrokerConn_ForwardsDiagnostics(t *testing.T) {
	brokerSide, clientSide := net.Pipe()
	defer clientSide.Close()
	bc := newBrokerConn(brokerSide)
	defer bc.close()

	f := newDiagFanout()
	f.add(bc)
	f.publish("file:///a.py", []types.LSPDiagnostic{{Message: "live-a"}})
	bc.replay(map[string][]types.LSPDiagnostic{
		"file:///a.py": {{Message: "stale-a"}},
		"file:///b.py": {{Message: "cached-b"}},
	})
	go bc.writeLoop()

	r := bufio.NewReader(clientSide)
	got := map[string]string{}
	for i := 0; i < 2; i++ {
		uri, diags := readForwarded(t, r)
		if len(diags) != 1 {
			t.Fatalf("%s: %d diags", uri, len(diags))
		}
		got[uri] = diags[0].Message
	}
	if got["file:///a.py"] != "live-a" || got["file:///b.py"] != "cached-b" {
		t.Fatalf("forwarded = %v", got)
	}

	// Removed connections receive nothing further.
	f.remove(bc)
	f.publish("file:///c.py", nil)
	bc.mu.Lock()
	n := len(bc.pending)
	bc.mu.Unlock()
	if n != 0 {
		t.Fatalf("removed conn still queued %d updates", n)
	}
}

// End to end: a daemon-mode client reading the broker socket fills its cache
// from forwarded notifications.
func TestBrokerConn_DaemonClientReceivesDiagnostics(t *testing.T) {
	brokerSide, clientSide := net.Pipe()
	bc := newBrokerConn(brokerSide)
	defer bc.close()
	go bc.writeLoop()

	c := NewLSPClient("", nil)
	c.stdin = clientSide
	c.frameReader = NewFrameReader(clientSide)
	go c.readLoop(c.frameReader)
	defer clientSide.Close()

	uri := "file:///daemon.py"
	done := make(chan error, 1)
	go func() { done <- WaitForDiagnostics(context.Background(), c, []string{uri}, 5000) }()
	waitForDiagSubscribers(t, c, 1)

	f := newDiagFanout()
	f.add(bc)
	f.publish(uri, []types.LSPDiagnostic{{Message: "OLS03002"}})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon client never saw the forwarded diagnostics")
	}
	if got := c.GetDiagnostics(uri); len(got) != 1 || got[0].Message != "OLS03002" {
		t.Fatalf("diagnostics = %+v", got)
	}
}

// waitForDiagSubscribers blocks until c has at least n diagnostics
// subscribers, so a test publishes only after its waiter is listening.
func waitForDiagSubscribers(t *testing.T, c *LSPClient, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.diagMu.RLock()
		have := len(c.diagSubs)
		c.diagMu.RUnlock()
		if have >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("diagnostics subscribers = %d, want %d", have, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// Fan-out reaches every connection, and removing one leaves the others
// subscribed.
func TestDiagFanout_MultipleConnections(t *testing.T) {
	f := newDiagFanout()
	a, b := newBrokerConn(nil), newBrokerConn(nil)
	f.add(a)
	f.add(b)
	f.publish("file:///x.py", []types.LSPDiagnostic{{Message: "one"}})
	for name, bc := range map[string]*brokerConn{"a": a, "b": b} {
		if got := bc.pending["file:///x.py"]; len(got) != 1 || got[0].Message != "one" {
			t.Errorf("conn %s pending = %+v, want one diagnostic", name, got)
		}
	}
	f.remove(a)
	f.publish("file:///y.py", nil)
	if _, ok := a.pending["file:///y.py"]; ok {
		t.Error("removed connection still received a publish")
	}
	if _, ok := b.pending["file:///y.py"]; !ok {
		t.Error("remaining connection missed a publish after another was removed")
	}
}

// A client that stops reading must not wedge the connection's shared write
// lock forever; the write fails at the deadline instead.
func TestBrokerConn_WriteDeadline(t *testing.T) {
	old := brokerWriteTimeout
	brokerWriteTimeout = 50 * time.Millisecond
	defer func() { brokerWriteTimeout = old }()
	brokerSide, clientSide := net.Pipe() // nobody reads clientSide
	defer clientSide.Close()
	bc := newBrokerConn(brokerSide)
	start := time.Now()
	if err := bc.write([]byte(`{"jsonrpc":"2.0"}`)); err == nil {
		t.Fatal("write to a non-reading client succeeded")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("write blocked %s despite the deadline", d)
	}
}

func TestEnvMillis(t *testing.T) {
	const name = "AGENT_LSP_TEST_ENV_MILLIS"
	def := 7 * time.Millisecond
	for _, tc := range []struct {
		val  string
		want time.Duration
	}{
		{"", def},
		{"250", 250 * time.Millisecond},
		{"0", 0},
		{"-5", def},
		{"abc", def},
	} {
		t.Setenv(name, tc.val)
		if got := envMillis(name, def); got != tc.want {
			t.Errorf("envMillis(%q) = %s, want %s", tc.val, got, tc.want)
		}
	}
}

// Two subscriptions made at the same call site share a code pointer; the
// returned unsubscribe must remove only its own. UnsubscribeFromDiagnostics
// removed both, so a finished WaitForDiagnostics silenced a concurrent one.
func TestSubscribeToDiagnostics_UnsubscribeIsExact(t *testing.T) {
	c := NewLSPClient("unused", nil)
	var hits [2]int
	subscribe := func(i int) func() {
		return c.SubscribeToDiagnostics(func(string, []types.LSPDiagnostic) { hits[i]++ })
	}
	unsubA := subscribe(0)
	defer subscribe(1)()
	unsubA()
	unsubA() // idempotent

	c.handlePublishDiagnostics([]byte(`{"uri":"file:///z.py","diagnostics":[]}`))
	if hits[0] != 0 || hits[1] != 1 {
		t.Fatalf("hits = %v, want [0 1]", hits)
	}
}
