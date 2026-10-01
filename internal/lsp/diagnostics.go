package lsp

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/blackwell-systems/agent-lsp/internal/types"
)

// Tunables for WaitForDiagnostics, read from the environment so a server with
// slower or quieter publishing can be accommodated without a rebuild.
//
//   - AGENT_LSP_DIAG_QUIET_MS: quiet window that must follow the last fresh
//     notification before diagnostics count as settled (default 500). Raise it
//     for servers that publish an empty set first and the real one later
//     (OdooLS: ~1s apart).
//   - AGENT_LSP_DIAG_CACHED_SETTLE_MS: when a URI already has cached
//     diagnostics and no fresh notification arrives within this window, accept
//     the cache (default 0 = disabled, wait until timeout). For servers that do
//     not republish when a document is reopened with unchanged content.
var (
	diagQuietWindow  = envMillis("AGENT_LSP_DIAG_QUIET_MS", 500*time.Millisecond)
	diagCachedSettle = envMillis("AGENT_LSP_DIAG_CACHED_SETTLE_MS", 0)
)

func envMillis(name string, def time.Duration) time.Duration {
	v, err := strconv.Atoi(os.Getenv(name))
	if err != nil || v < 0 {
		return def
	}
	return time.Duration(v) * time.Millisecond
}

// WaitForDiagnostics waits for diagnostic stabilisation for all uris.
// It skips the cached-replay notification for URIs that already had cached
// diagnostics (matching the TypeScript sawInitialSnapshot logic), requires one
// fresh notification per URI after that, then waits for a quiet window
// (AGENT_LSP_DIAG_QUIET_MS). A cached URI with no fresh notification counts as
// settled after AGENT_LSP_DIAG_CACHED_SETTLE_MS, when that is set.
// Resolves on timeout without error.
func WaitForDiagnostics(ctx context.Context, client *LSPClient, uris []string, timeoutMs int) error {
	if len(uris) == 0 {
		return nil
	}

	var mu sync.Mutex

	// Track which URIs have received at least one fresh notification.
	received := make(map[string]bool, len(uris))
	for _, uri := range uris {
		received[uri] = false
	}

	// pendingReplay holds URIs whose first callback will be the cached replay
	// from SubscribeToDiagnostics. Only those get a callback skipped: a URI
	// with nothing cached gets no replay, so its first callback is fresh.
	pendingReplay := make(map[string]bool, len(uris))
	cached := make(map[string]bool, len(uris))
	for _, uri := range uris {
		if client.HasPublishedDiagnostics(uri) {
			pendingReplay[uri] = true
			cached[uri] = true
		}
	}

	start := time.Now()
	lastEvent := start

	allReceived := func() bool {
		cachedSettled := diagCachedSettle > 0 && time.Since(start) >= diagCachedSettle
		for uri, ok := range received {
			if !ok && !(cachedSettled && cached[uri]) {
				return false
			}
		}
		return true
	}

	notify := make(chan struct{}, len(uris)+1)

	cb := types.DiagnosticUpdateCallback(func(uri string, _ []types.LSPDiagnostic) {
		mu.Lock()
		if _, tracked := received[uri]; tracked {
			if pendingReplay[uri] {
				delete(pendingReplay, uri)
				mu.Unlock()
				return
			}
			received[uri] = true
		}
		lastEvent = time.Now()
		mu.Unlock()
		select {
		case notify <- struct{}{}:
		default:
		}
	})

	defer client.SubscribeToDiagnostics(cb)()

	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	quietWindow := diagQuietWindow

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil
			}
			mu.Lock()
			gotAll := allReceived()
			quiet := time.Since(lastEvent) >= quietWindow
			mu.Unlock()
			if gotAll && quiet {
				return nil
			}
		case <-notify:
			// Notification received. Check the quiet window immediately to
			// avoid waiting up to 50ms for the next tick (M5 fix).
			if time.Now().After(deadline) {
				return nil
			}
			mu.Lock()
			gotAll := allReceived()
			quiet := time.Since(lastEvent) >= quietWindow
			mu.Unlock()
			if gotAll && quiet {
				return nil
			}
		}
	}
}
