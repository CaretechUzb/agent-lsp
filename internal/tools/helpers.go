// helpers.go contains shared utilities used across all tool handlers:
//
//   - ValidateFilePath: path traversal prevention (resolves symlinks, checks
//     workspace root boundary). Used by every tool that accepts a file_path arg.
//   - WithDocument: convenience wrapper that reads a file from disk, opens it
//     in the LSP server, and calls a callback. Handles the common open-then-query
//     pattern used by navigation and analysis tools.
//   - CreateFileURI / URIToFilePath: file path <-> file:// URI conversion.
//   - CheckInitialized: guard that returns a clear error when the LSP client
//     hasn't been started yet.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	gcf "github.com/blackwell-systems/agent-lsp/internal/encoding/gcf"
	"github.com/blackwell-systems/agent-lsp/internal/lsp"
	"github.com/blackwell-systems/agent-lsp/internal/types"
	uriPkg "github.com/blackwell-systems/agent-lsp/internal/uri"
	gcfgo "github.com/blackwell-systems/gcf-go"
)

// ValidateFilePath resolves filePath to a clean absolute path and, when rootDir
// is non-empty, verifies the result is within the workspace root. This prevents
// path traversal attacks (e.g. "../../etc/passwd").
//
// Delegates to uriPkg.ValidatePath, the canonical implementation shared with
// internal/lsp (which cannot import this package without an import cycle).
func ValidateFilePath(filePath, rootDir string) (string, error) {
	return uriPkg.ValidatePath(filePath, rootDir)
}

// WithDocument reads filePath from disk, opens it on the LSP client, then calls cb.
// T is the callback return type. On error, returns zero value of T and the error.
func WithDocument[T any](
	ctx context.Context,
	client *lsp.LSPClient,
	filePath string,
	languageID string,
	cb func(fileURI string) (T, error),
) (T, error) {
	var zero T

	clean, err := ValidateFilePath(filePath, client.RootDir())
	if err != nil {
		return zero, err
	}
	filePath = clean

	content, err := os.ReadFile(filePath)
	if err != nil {
		return zero, fmt.Errorf("reading file %s: %w", filePath, err)
	}

	fileURI := CreateFileURI(filePath)

	// Record before opening: OpenDocument tracks the URI as open even when
	// the didOpen send fails, so it must still be closed afterwards.
	if t, ok := ctx.Value(openedDocsKey{}).(*openedDocs); ok {
		t.record(fileURI)
	}
	if err := client.OpenDocument(ctx, fileURI, string(content), languageID); err != nil {
		return zero, fmt.Errorf("opening document %s: %w", filePath, err)
	}

	return cb(fileURI)
}

// openedDocsKey is the context key under which WithClosingDocuments stores
// its *openedDocs.
type openedDocsKey struct{}

// openedDocs records the URIs WithDocument opened under one
// WithClosingDocuments context. Safe for concurrent use.
type openedDocs struct {
	mu   sync.Mutex
	uris map[string]struct{}
}

func (o *openedDocs) record(uri string) {
	o.mu.Lock()
	o.uris[uri] = struct{}{}
	o.mu.Unlock()
}

// WithClosingDocuments returns a context under which every document opened
// through WithDocument is recorded, and a function that sends didClose for
// each recorded document that was not already open when
// WithClosingDocuments was called. Documents the user or agent opened
// before stay open. Tools that open many files to answer one call (e.g.
// blast_radius) use it so those files do not stay open on the language
// server. Call the returned function once the work is done, typically with
// defer; CloseDocument ignores ctx, so a cancelled context is fine.
func WithClosingDocuments(ctx context.Context, client *lsp.LSPClient) (context.Context, func()) {
	before := map[string]struct{}{}
	for _, uri := range client.GetOpenDocuments() {
		before[uri] = struct{}{}
	}
	t := &openedDocs{uris: map[string]struct{}{}}
	closeOpened := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		for uri := range t.uris {
			if _, wasOpen := before[uri]; !wasOpen {
				_ = client.CloseDocument(ctx, uri)
			}
		}
		t.uris = map[string]struct{}{}
	}
	return context.WithValue(ctx, openedDocsKey{}, t), closeOpened
}

// CreateFileURI converts an absolute file path to a file:// URI.
//
// Delegates to lsp.PathToFileURI — the canonical helper that handles
// both POSIX and Windows correctly. The previous implementation here
// did `url.URL{Scheme: "file", Path: filePath}.String()` which on
// Windows produced `file://S:/Source/...` (drive letter promoted to
// the URI authority) instead of the RFC 8089 form `file:///S:/...`.
// Downstream LSP servers then either failed to resolve the URI at all
// or returned references under a totally different normalization,
// silently dropping cross-file callers in find_references and similar.
func CreateFileURI(filePath string) string {
	return lsp.PathToFileURI(filePath)
}

// URIToFilePath converts a file:// URI to an absolute path.
// Delegates to uri.URIToPath — canonical shared implementation (M3).
func URIToFilePath(rawURI string) (string, error) {
	if !strings.HasPrefix(rawURI, "file://") {
		return "", fmt.Errorf("not a file URI: %s", rawURI)
	}
	return uriPkg.URIToPath(rawURI), nil
}

// CheckInitialized returns an error if client is nil.
func CheckInitialized(client *lsp.LSPClient) error {
	if client == nil {
		return errors.New("LSP client not initialized; call start_lsp first")
	}
	return nil
}

// appendHint adds a next-step hint to a tool result's text content.
// The hint is appended as a separate line after the main content.
// Error results and empty hints/content are returned unchanged.
func appendHint(result types.ToolResult, hint string) types.ToolResult {
	if hint == "" || result.IsError || len(result.Content) == 0 || result.Content[0].Text == "" {
		return result
	}
	// Add the hint as a separate content item so it never interferes with
	// JSON parsing of the primary result. Agents see both items; parsers
	// that only read Content[0] are unaffected.
	result.Content = append(result.Content, types.ContentItem{
		Type: "text",
		Text: "Next step: " + hint,
	})
	return result
}

// outputFormatKey is a context key for the output format preference.
type outputFormatKey struct{}

// ContextWithOutputFormat returns a new context with the output format set.
func ContextWithOutputFormat(ctx context.Context, format string) context.Context {
	return context.WithValue(ctx, outputFormatKey{}, format)
}

// OutputFormatFromContext returns the output format from context, defaulting to "json".
func OutputFormatFromContext(ctx context.Context) string {
	if ctx == nil {
		return "json"
	}
	if v, ok := ctx.Value(outputFormatKey{}).(string); ok && v != "" {
		return v
	}
	return "json"
}

// EncodeResult marshals data as JSON or GCF based on the output format in ctx.
// Falls back to JSON if GCF encoding fails or format is unrecognized.
func EncodeResult(ctx context.Context, data any) (types.ToolResult, error) {
	format := OutputFormatFromContext(ctx)
	switch format {
	case "gcf":
		// Graph-profile: if data is already a *gcf.Payload, use graph encoding
		if p, ok := data.(*gcfgo.Payload); ok {
			encoded, err := gcf.EncodeGraph(p)
			if err != nil {
				raw, _ := json.Marshal(data)
				return types.TextResult(string(raw)), nil
			}
			return types.TextResult(encoded), nil
		}
		// Tabular fallback for non-Payload data
		encoded, err := gcf.Encode(data)
		if err != nil {
			// Fall back to JSON on GCF encoding failure
			raw, _ := json.Marshal(data)
			return types.TextResult(string(raw)), nil
		}
		return types.TextResult(encoded), nil
	default:
		raw, err := json.Marshal(data)
		if err != nil {
			return types.ErrorResult(err.Error()), nil
		}
		return types.TextResult(string(raw)), nil
	}
}

// EncodeResultJSON marshals data as JSON regardless of the context output format.
// Use it for any payload the caller must round-trip byte-exact, in particular a
// WorkspaceEdit that gets handed back to apply_edit. GCF's tabular form is a
// summary format: it flattens a TextEdit's range into positional path columns
// (range>end>character, range>end>line, ...) and requires the newText to be
// copied verbatim out of tool output and back into a tool argument. LLM callers
// corrupt that round-trip, transposing the range offsets and truncating a large
// newText, which overwrites the file with a mangled edit. Edits therefore always
// serialize as self-labeling JSON. See issue #12.
func EncodeResultJSON(data any) (types.ToolResult, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return types.ErrorResult(err.Error()), nil
	}
	return types.TextResult(string(raw)), nil
}
