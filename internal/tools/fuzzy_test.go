package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
// wire capture that motivated issue #39: a textDocument/definition at a call
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

	got, ok := identifierAtFilePosition(uri, 2, 6)
	if !ok || got != "Second" {
		t.Errorf("identifierAtFilePosition(line 2, col 6) = (%q, %v), want (Second, true)", got, ok)
	}
	if _, ok := identifierAtFilePosition(uri, 99, 1); ok {
		t.Error("identifierAtFilePosition out-of-range line should return false")
	}
	if _, ok := identifierAtFilePosition("not-a-uri", 1, 1); ok {
		t.Error("identifierAtFilePosition invalid URI should return false")
	}
}

// --- provenance hint ---

// TestFuzzyFallbackProvenanceHint_Qualified mirrors TestReferencesEmptyHint_Qualified:
// the hint must name the mechanism so an agent can distrust the result. (issue #39)
func TestFuzzyFallbackProvenanceHint_Qualified(t *testing.T) {
	if !strings.Contains(fuzzyFallbackProvenanceHint, "fuzzy position fallback") {
		t.Errorf("fuzzyFallbackProvenanceHint should name the fuzzy position fallback, got %q", fuzzyFallbackProvenanceHint)
	}
}
