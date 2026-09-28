package lsp

import (
	"encoding/json"
	"net"
	"sync"
	"time"

	"github.com/blackwell-systems/agent-lsp/internal/logging"
	"github.com/blackwell-systems/agent-lsp/internal/types"
)

// diagFanout forwards the broker's textDocument/publishDiagnostics
// notifications to every connected socket client. Without it the daemon
// client's diagnostics cache stays empty forever: handleBrokerConnection only
// relays request/response pairs, and servers push diagnostics unprompted.
//
// The broker registers fanout.publish as its single diagnostics subscriber.
// One subscriber per connection would not work: UnsubscribeFromDiagnostics
// compares closures by code pointer, so removing one connection's closure
// would remove every connection's.
type diagFanout struct {
	mu    sync.Mutex
	conns map[*brokerConn]struct{}
}

func newDiagFanout() *diagFanout {
	return &diagFanout{conns: make(map[*brokerConn]struct{})}
}

func (f *diagFanout) add(bc *brokerConn) {
	f.mu.Lock()
	f.conns[bc] = struct{}{}
	f.mu.Unlock()
}

func (f *diagFanout) remove(bc *brokerConn) {
	f.mu.Lock()
	delete(f.conns, bc)
	f.mu.Unlock()
}

// publish is the broker client's DiagnosticUpdateCallback.
func (f *diagFanout) publish(uri string, diags []types.LSPDiagnostic) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for bc := range f.conns {
		bc.enqueue(uri, diags, true)
	}
}

// brokerConn is one socket client of the broker. Responses and forwarded
// notifications share the socket, so every write goes through write().
//
// Forwarded diagnostics are coalesced per URI: a slow reader receives the
// latest set for each URI rather than an ever-growing backlog, and a write
// never blocks the broker's LSP read loop.
type brokerConn struct {
	conn    net.Conn
	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[string][]types.LSPDiagnostic
	order   []string
	// touched marks URIs updated live since the connection registered, so the
	// cache replay that follows registration cannot overwrite them with an
	// older snapshot.
	touched map[string]bool
	wake    chan struct{}
	done    chan struct{}
}

func newBrokerConn(conn net.Conn) *brokerConn {
	return &brokerConn{
		conn:    conn,
		pending: make(map[string][]types.LSPDiagnostic),
		touched: make(map[string]bool),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// brokerWriteTimeout bounds one socket write. A client that stops reading
// would otherwise block the write, and with it every response on the shared
// write lock, forever. A var so tests can shorten it.
var brokerWriteTimeout = 10 * time.Second

func (bc *brokerConn) write(msg []byte) error {
	bc.writeMu.Lock()
	defer bc.writeMu.Unlock()
	if err := bc.conn.SetWriteDeadline(time.Now().Add(brokerWriteTimeout)); err != nil {
		return err
	}
	return writeFramedMessage(bc.conn, msg)
}

// enqueue queues uri's diagnostics for forwarding. live=false is the cache
// replay, which yields to anything already delivered live.
func (bc *brokerConn) enqueue(uri string, diags []types.LSPDiagnostic, live bool) {
	// Live callbacks carry the server's raw URI while the replay snapshot is
	// keyed by NormalizeFileURI; key both the same way so precedence holds.
	uri = NormalizeFileURI(uri)
	bc.mu.Lock()
	if live {
		if bc.touched != nil {
			bc.touched[uri] = true
		}
	} else if bc.touched[uri] {
		bc.mu.Unlock()
		return
	}
	if _, queued := bc.pending[uri]; !queued {
		bc.order = append(bc.order, uri)
	}
	bc.pending[uri] = diags
	bc.mu.Unlock()
	select {
	case bc.wake <- struct{}{}:
	default:
	}
}

// replay forwards the broker's current cache, so a client that connects
// after the server published (e.g. during initial indexing) still sees it.
func (bc *brokerConn) replay(snapshot map[string][]types.LSPDiagnostic) {
	for uri, diags := range snapshot {
		bc.enqueue(uri, diags, false)
	}
	// touched only arbitrates this replay; stop growing it afterwards.
	bc.mu.Lock()
	bc.touched = nil
	bc.mu.Unlock()
}

// writeLoop drains queued diagnostics onto the socket until close() or a
// write error.
func (bc *brokerConn) writeLoop() {
	for {
		select {
		case <-bc.done:
			return
		case <-bc.wake:
		}
		bc.mu.Lock()
		order, pending := bc.order, bc.pending
		bc.order, bc.pending = nil, make(map[string][]types.LSPDiagnostic)
		bc.mu.Unlock()
		for _, uri := range order {
			diags := pending[uri]
			if diags == nil {
				diags = []types.LSPDiagnostic{}
			}
			msg, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"method":  "textDocument/publishDiagnostics",
				"params":  map[string]any{"uri": uri, "diagnostics": diags},
			})
			if err != nil {
				continue
			}
			if err := bc.write(msg); err != nil {
				// Close so the request side fails too, instead of serving a
				// client that silently stops receiving diagnostics.
				logging.Log(logging.LevelWarning, "broker: failed to forward diagnostics, closing connection: "+err.Error())
				_ = bc.conn.Close()
				return
			}
		}
	}
}

func (bc *brokerConn) close() {
	close(bc.done)
}
