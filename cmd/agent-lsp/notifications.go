// notifications.go wires the proactive notification Hub into the MCP server
// lifecycle. It provides the concrete NotificationSender implementation and
// helper functions to connect notification channels to an LSPClient.
package main

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/blackwell-systems/agent-lsp/internal/lsp"
	"github.com/blackwell-systems/agent-lsp/internal/notify"
	"github.com/blackwell-systems/agent-lsp/internal/types"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// wiredClients records clients whose notifications are wired, so repeated
// start_lsp calls on the same client do not stack subscriptions.
var wiredClients sync.Map // *lsp.LSPClient -> struct{}

// mcpNotifySender implements notify.NotificationSender using a live MCP session.
type mcpNotifySender struct {
	ss     *mcp.ServerSession
	server *mcp.Server
}

// SendLog emits a logging/message notification to the connected MCP client.
func (s *mcpNotifySender) SendLog(level, logger, message string) error {
	data, _ := json.Marshal(message)
	return s.ss.Log(context.Background(), &mcp.LoggingMessageParams{
		Level:  mcp.LoggingLevel(level),
		Logger: logger,
		Data:   json.RawMessage(data),
	})
}

// SendResourceUpdated emits a resources/updated notification to subscribed clients.
func (s *mcpNotifySender) SendResourceUpdated(uri string) error {
	return s.server.ResourceUpdated(context.Background(), &mcp.ResourceUpdatedNotificationParams{
		URI: uri,
	})
}

// setupNotificationHub creates a Hub with no sender (sender is set later when
// the MCP client session initializes).
func setupNotificationHub() *notify.Hub {
	return notify.NewHub(nil)
}

// wireNotificationsToClient subscribes the notification hub to all channels
// provided by the given LSP client: diagnostics, workspace readiness, health
// monitoring, and file change staleness detection.
func wireNotificationsToClient(hub *notify.Hub, client *lsp.LSPClient) {
	// start_lsp and auto-init may hand over a client that is already wired
	// (single-server mode reuses one client); wire each client once.
	if _, already := wiredClients.LoadOrStore(client, struct{}{}); already {
		return
	}
	// Each stop runs when this client shuts down (a start_lsp restart must not
	// leave its health poller reporting the intentional stop as a crash) and
	// again on hub close; OnceFunc makes the second call a no-op.
	var stops []func()
	addStop := func(fn func()) {
		once := sync.OnceFunc(fn)
		stops = append(stops, once)
		hub.AddStopFunc(once)
	}
	client.OnShutdown(func() {
		for _, stop := range stops {
			stop()
		}
		wiredClients.Delete(client)
	})

	// Diagnostic notifications (debounced).
	stopDiag := notify.SubscribeDiagnostics(hub, client)
	addStop(stopDiag)

	// Diagnostic regression detection (fires when new errors appear).
	stopDiagChange := notify.SubscribeDiagnosticChanges(hub, client)
	addStop(stopDiagChange)

	// Workspace ready notification (polls until indexed).
	stopReady := notify.SubscribeWorkspaceReady(hub, client, 2*time.Second)
	addStop(stopReady)

	// Health monitoring (polls for process liveness).
	stopHealth := notify.SubscribeHealth(hub, client, 5*time.Second)
	addStop(stopHealth)

	// Stale reference detection (debounces file changes).
	stale := notify.NewStaleNotifier(hub, 3*time.Second)
	addStop(stale.Stop)

	// Bridge file watcher events to the stale notifier.
	client.SubscribeToFileChanges(func(changes []types.FileChangeEvent) {
		fileChanges := make([]notify.FileChange, len(changes))
		for i, c := range changes {
			fileChanges[i] = notify.FileChange{
				URI:        c.URI,
				ChangeType: c.Type,
			}
		}
		stale.OnFileChange(fileChanges)
	})
}
