package tools

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackwell-systems/agent-lsp/internal/lsp"
)

// --- identifierInLine ---

func TestIdentifierInLine(t *testing.T) {
	const midLine = "  x = StopLong(Bid, stopLossPips);"

	cases := []struct {
		name     string
		lineText string
		col      int
		want     string
		wantOK   bool
	}{
		{"token at line start", "Foo(1)", 1, "Foo", true},
		{"token mid-line", midLine, strings.Index(midLine, "StopLong") + 1, "StopLong", true},
		{"col on char after token", "Foo(1)", 4, "", false},
		{"col on whitespace", "foo bar", 4, "", false},
		{"empty line", "", 1, "", false},
		{"col past end of line", "Foo", 10, "", false},
		{"col zero", "Foo", 0, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := identifierInLine(tc.lineText, tc.col)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("identifierInLine(%q, %d) = (%q, %v), want (%q, %v)", tc.lineText, tc.col, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// --- fuzzyFallbackDecision ---

// The hover and line strings below are verbatim from the mql-lsp-server v2.4.2
// wire capture that motivated issue #40: a textDocument/definition at a call
// site returns null, and the hover at that position describes the *containing*
// function ("## OpenPendingOrder_Alexander") rather than the called symbol.
const (
	wireContainingHover = "## OpenPendingOrder_Alexander\n**Type:** User Defined\n**Kind:** Function\n**Detail:** Function returning int\n**File:** trading_order_utils_mq4.mqh\n"
	wireStopLongHover   = "## StopLong\n**Type:** User Defined\n**Kind:** Function\n**Detail:** Function returning double\n**File:** stop_take_utils_mq4.mqh\n"
	wireCallSite        = "               orderTicket = T4_SendOrder(Symbol(), OP_BUY, orderLots, NormalizeDouble(Ask, Digits), AlexanderSlipPage, StopLong(Bid, stopLossPips), TakeLong(Ask, takeProfitPips), orderComment+manualFlag, orderMagic, expiration, arrowColor);"
)

func TestFuzzyFallbackDecision(t *testing.T) {
	stopLongCol := strings.Index(wireCallSite, "StopLong") + 1

	cases := []struct {
		name        string
		hoverText   string
		lineText    string
		col         int
		wantSymbol  string
		wantProceed bool
	}{
		{
			name:        "containing-symbol hover is rejected",
			hoverText:   wireContainingHover,
			lineText:    wireCallSite,
			col:         stopLongCol,
			wantSymbol:  "OpenPendingOrder_Alexander",
			wantProceed: false,
		},
		{
			name:        "matching hover proceeds",
			hoverText:   wireStopLongHover,
			lineText:    wireCallSite,
			col:         stopLongCol,
			wantSymbol:  "StopLong",
			wantProceed: true,
		},
		{
			name:        "empty hover",
			hoverText:   "",
			lineText:    wireCallSite,
			col:         stopLongCol,
			wantSymbol:  "",
			wantProceed: false,
		},
		{
			name:        "go fence hover is conservative",
			hoverText:   "```go\nfunc Foo()\n```",
			lineText:    "Foo(1)",
			col:         1,
			wantSymbol:  "func",
			wantProceed: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			symbol, proceed := fuzzyFallbackDecision(tc.hoverText, tc.lineText, tc.col)
			if symbol != tc.wantSymbol || proceed != tc.wantProceed {
				t.Errorf("fuzzyFallbackDecision(...) = (%q, %v), want (%q, %v)", symbol, proceed, tc.wantSymbol, tc.wantProceed)
			}
		})
	}
}

// --- identifierAtFilePosition ---

func TestIdentifierAtFilePosition(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.mq4")
	if err := os.WriteFile(path, []byte("int first = 1;\nvoid Second() {}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	uri := CreateFileURI(path)

	got, ok := identifierAtFilePosition(uri, 2, 6, dir)
	if !ok || got != "Second" {
		t.Errorf("identifierAtFilePosition(line 2, col 6) = (%q, %v), want (Second, true)", got, ok)
	}
	if _, ok := identifierAtFilePosition(uri, 99, 1, dir); ok {
		t.Error("identifierAtFilePosition out-of-range line should return false")
	}
	if _, ok := identifierAtFilePosition("not-a-uri", 1, 1, dir); ok {
		t.Error("identifierAtFilePosition invalid URI should return false")
	}
	if _, ok := identifierAtFilePosition(uri, 2, 6, "/elsewhere"); ok {
		t.Error("identifierAtFilePosition with a rootDir that does not contain the file should return false")
	}
}

// --- UTF-16 column handling ---

// TestIdentifierInLine_UTF16Columns verifies that the tool-layer column (a
// 1-indexed UTF-16 code-unit position, as sent to and received from the LSP)
// is converted to a byte offset before indexing the line. A byte-index read
// would land mid-rune on lines with non-ASCII prefixes and reject a valid
// identifier. (issue #40 review follow-up)
func TestIdentifierInLine_UTF16Columns(t *testing.T) {
	cases := []struct {
		name     string
		lineText string
		col      int
		want     string
		wantOK   bool
	}{
		// "é" is 1 UTF-16 unit but 2 bytes: byte index 2 is the space after it.
		{"BMP non-ASCII prefix", "é StopLong()", 3, "StopLong", true},
		// "𝕏" is a surrogate pair (2 UTF-16 units) but 4 bytes.
		{"surrogate-pair prefix", "𝕏 Stop()", 4, "Stop", true},
		{"column mid-surrogate-pair is rejected", "𝕏 Stop()", 3, "", false},
		{"column past line end in units", "é Foo", 20, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := identifierInLine(tc.lineText, tc.col)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("identifierInLine(%q, %d) = (%q, %v), want (%q, %v)", tc.lineText, tc.col, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestHandleGoToDefinition_FuzzyFallbackRejectsContainingSymbolHover drives the
// full handler against a fake TCP language server: the direct definition
// lookup returns empty, the hover at the call site describes the *containing*
// function (the mql-lsp-server v2.4.2 wire behavior that motivated issue #40),
// so the fallback must reject the hover symbol and the handler must return an
// empty result WITHOUT the fuzzy provenance hint and WITHOUT querying
// workspace symbols. A handler that bypasses fuzzyFallbackDecision (or hovers
// at the wrong position) would resolve OpenPendingOrder_Alexander and leak the
// provenance hint instead. (issue #40 review follow-up)
func TestHandleGoToDefinition_FuzzyFallbackRejectsContainingSymbolHover(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	workspaceSymbolQueries := make(chan string, 1)
	go func() {
		defer close(workspaceSymbolQueries)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Register the three providers dynamically (the lazy-registration path
		// jdtls and mql-lsp-server use), so the client's capability checks pass
		// without a real initialize handshake.
		regBody, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "client/registerCapability",
			"params": map[string]any{
				"registrations": []map[string]any{
					{"id": "def", "method": "textDocument/definition"},
					{"id": "hov", "method": "textDocument/hover"},
					{"id": "wsym", "method": "workspace/symbol"},
				},
			},
		})
		if _, err := conn.Write(lsp.EncodeMessage(regBody)); err != nil {
			return
		}
		r := lsp.NewFrameReader(conn)
		for {
			raw, err := r.ReadMessage()
			if err != nil {
				return
			}
			var msg struct {
				ID     *float64        `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(raw, &msg); err != nil {
				return
			}
			if msg.ID == nil || msg.Method == "" {
				continue // notification or response to a server-initiated request
			}
			result := any(nil)
			switch msg.Method {
			case "textDocument/definition":
				result = []any{}
			case "textDocument/hover":
				result = map[string]any{
					"contents": map[string]any{"kind": "markdown", "value": wireContainingHover},
				}
			case "workspace/symbol":
				var params struct {
					Query string `json:"query"`
				}
				_ = json.Unmarshal(msg.Params, &params)
				workspaceSymbolQueries <- params.Query
				result = []any{}
			}
			body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": result})
			if _, err := conn.Write(lsp.EncodeMessage(body)); err != nil {
				return
			}
		}
	}()

	client, err := lsp.NewPassiveClient(ln.Addr().String())
	if err != nil {
		t.Fatalf("NewPassiveClient: %v", err)
	}
	defer client.Shutdown(context.Background())
	rootDir := t.TempDir()
	client.MarkInitializedForTest()
	client.SetRootDirForTest(rootDir)

	wikPath := filepath.Join(rootDir, "main.mq4")
	if err := os.WriteFile(wikPath, []byte(wireCallSite+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	args := map[string]any{
		"file_path": wikPath,
		"line":      1,
		"col":       strings.Index(wireCallSite, "StopLong") + 1,
	}
	res, err := HandleGoToDefinition(context.Background(), client, args)
	if err != nil {
		t.Fatalf("HandleGoToDefinition: %v", err)
	}
	for _, c := range res.Content {
		if strings.Contains(c.Text, "fuzzy position fallback") {
			t.Error("containing-symbol hover must be rejected: no fuzzy provenance hint expected")
		}
		if strings.Contains(c.Text, "OpenPendingOrder_Alexander") {
			t.Error("containing symbol must not be resolved as the definition")
		}
	}
	select {
	case q := <-workspaceSymbolQueries:
		t.Errorf("workspace/symbol must not be queried after the hover is rejected, got query %q", q)
	default:
	}
}

// --- provenance hint ---

// TestFuzzyFallbackProvenanceHint_Qualified mirrors TestReferencesEmptyHint_Qualified:
// the hint must name the mechanism so an agent can distrust the result. (issue #40)
func TestFuzzyFallbackProvenanceHint_Qualified(t *testing.T) {
	if !strings.Contains(fuzzyFallbackProvenanceHint, "fuzzy position fallback") {
		t.Errorf("fuzzyFallbackProvenanceHint should name the fuzzy position fallback, got %q", fuzzyFallbackProvenanceHint)
	}
}
