package tools

import (
	"context"
	"os"
	"strings"

	"github.com/blackwell-systems/agent-lsp/internal/logging"
	"github.com/blackwell-systems/agent-lsp/internal/lsp"
	"github.com/blackwell-systems/agent-lsp/internal/types"
)

// fuzzyPositionFallback retries a position-based lookup using workspace symbol
// candidates when the direct lookup returns empty results.
//
// It extracts a symbol name from hover at (line, col), validates that the hover
// describes the identifier actually sitting at that position, searches workspace
// symbols for that name, and retries lookupFn at each candidate position. Returns
// the first non-empty result set and whether the fallback produced it.
//
// The hover/position validation exists because some servers answer a hover at a
// position they cannot resolve with the *containing* symbol (observed with
// mql-lsp-server v2.4.2). Resolving the fallback through that name would then
// answer a different question than the one asked and return a confidently wrong
// result (issue #40).
//
// rootDir is the workspace root; the fallback re-reads the source line from
// disk, so the converted path is re-validated against it at the filesystem
// boundary (see sourceLineAtFile).
//
// line and col are 1-indexed (tool convention); converted internally to 0-indexed.
func fuzzyPositionFallback(
	ctx context.Context,
	client *lsp.LSPClient,
	fileURI string,
	line, col int,
	rootDir string,
	lookupFn func(pos types.Position) ([]types.Location, error),
) (locs []types.Location, used bool, err error) {
	hoverPos := types.Position{Line: line - 1, Character: col - 1}
	hoverText, err := client.GetInfoOnLocation(ctx, fileURI, hoverPos)
	if err != nil || hoverText == "" {
		logging.Log(logging.LevelDebug, "fuzzyFallback: no hover text, skipping")
		return nil, false, nil
	}

	lineText, ok := sourceLineAtFile(fileURI, line, client.RootDir())
	if !ok {
		logging.Log(logging.LevelDebug, "fuzzyFallback: could not read source line, skipping")
		return nil, false, nil
	}

	symbolName, proceed := fuzzyFallbackDecision(hoverText, lineText, col)
	if !proceed {
		queried, _ := identifierAtFilePosition(fileURI, line, col, client.RootDir())
		logging.Log(logging.LevelDebug, "fuzzyFallback: hover symbol "+symbolName+" does not match identifier "+queried+" at position; skipping")
		return nil, false, nil
	}

	logging.Log(logging.LevelDebug, "fuzzyFallback: searching workspace symbols for "+symbolName)

	syms, symErr := client.GetWorkspaceSymbols(ctx, symbolName)
	if symErr != nil || len(syms) == 0 {
		return nil, false, nil
	}

	for _, sym := range syms {
		if sym.Location.URI == "" {
			continue
		}
		candidatePos := types.Position{
			Line:      sym.Location.Range.Start.Line,
			Character: sym.Location.Range.Start.Character,
		}
		results, lErr := lookupFn(candidatePos)
		if lErr == nil && len(results) > 0 {
			logging.Log(logging.LevelDebug, "fuzzyFallback: found results via workspace symbol candidate")
			return results, true, nil
		}
	}

	return nil, false, nil
}

// fuzzyFallbackDecision decides whether the fuzzy fallback may proceed for a
// hover text at a queried source position. The hover must describe the same
// identifier that sits at the queried position: some servers return the
// *containing* symbol when cursor resolution fails, and resolving the
// fallback through that name would answer a different question than the one
// asked (issue #40). Returns the extracted symbol name and whether the
// fallback may proceed. col is 1-indexed, consistent with the tool layer.
func fuzzyFallbackDecision(hoverText, lineText string, col int) (symbolName string, proceed bool) {
	symbolName = extractSymbolName(hoverText)
	if symbolName == "" {
		return "", false
	}
	queried, ok := identifierInLine(lineText, col)
	if !ok || queried == "" {
		return symbolName, false
	}
	if queried != symbolName {
		return symbolName, false
	}
	return symbolName, true
}

// identifierInLine extracts the identifier token at the 1-indexed UTF-16 code
// unit column col of a single source line, walking left from col to the token
// start. Tool-layer columns follow the LSP position convention (UTF-16 code
// units), so the column is converted to a byte offset before indexing the Go
// string; a byte-index read would land mid-rune for lines with non-ASCII
// prefixes and reject a valid identifier. Returns ("", false) when the line
// has no identifier at that position.
func identifierInLine(lineText string, col int) (string, bool) {
	i, ok := utf16ColToByteOffset(lineText, col)
	if !ok || i >= len(lineText) || !isIdentifierChar(lineText[i]) {
		return "", false
	}
	start := i
	for start > 0 && isIdentifierChar(lineText[start-1]) {
		start--
	}
	end := i + 1
	for end < len(lineText) && isIdentifierChar(lineText[end]) {
		end++
	}
	return lineText[start:end], true
}

// utf16ColToByteOffset converts a 1-indexed UTF-16 code-unit column (the LSP
// position convention) to a 0-indexed byte offset within s, counting surrogate
// pairs and multi-byte runes the way an LSP server does. Returns ok=false when
// the column is before the start or beyond the end of s.
func utf16ColToByteOffset(s string, col int) (int, bool) {
	if col < 1 {
		return 0, false
	}
	units := 0
	for i, r := range s {
		if units == col-1 {
			return i, true
		}
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
		if units > col-1 {
			// The column points into the middle of a rune (e.g. the second
			// half of a surrogate pair): not a valid character boundary.
			return 0, false
		}
	}
	if units == col-1 {
		return len(s), true
	}
	return 0, false
}

// isIdentifierChar reports whether b may appear in an identifier: an ASCII
// letter, digit, or underscore. Identifiers outside this set are not
// comparable via the fallback's exact-match check.
func isIdentifierChar(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_'
}

// identifierAtFilePosition reads the source line at (line, col) (both
// 1-indexed) from the file behind fileURI and extracts the identifier there.
// rootDir must be the workspace root: the converted path is re-validated
// against it immediately before the read (see sourceLineAtFile).
// Returns ("", false) on read errors or out-of-range positions.
func identifierAtFilePosition(fileURI string, line, col int, rootDir string) (string, bool) {
	lineText, ok := sourceLineAtFile(fileURI, line, rootDir)
	if !ok {
		return "", false
	}
	return identifierInLine(lineText, col)
}

// sourceLineAtFile reads the 1-indexed source line from the file behind
// fileURI. rootDir must be the workspace root: WithDocument validates the
// caller's path before opening the document, but the file may be swapped (or a
// path alias re-pointed) between that validation and this read, so the
// converted path is validated again here, at the filesystem boundary, before
// os.ReadFile. Returns ("", false) on validation or read errors and
// out-of-range line numbers.
func sourceLineAtFile(fileURI string, line int, rootDir string) (string, bool) {
	if line < 1 {
		return "", false
	}
	path, err := URIToFilePath(fileURI)
	if err != nil {
		return "", false
	}
	valid, err := ValidateFilePath(path, rootDir)
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(valid)
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(data), "\n")
	if line > len(lines) {
		return "", false
	}
	return lines[line-1], true
}

// extractSymbolName parses a short identifier from hover text.
// Hover text from gopls typically starts with the symbol signature.
// We extract the first identifier-like token.
func extractSymbolName(hover string) string {
	hover = strings.TrimSpace(hover)
	if strings.HasPrefix(hover, "```") {
		lines := strings.SplitN(hover, "\n", 3)
		if len(lines) >= 2 {
			hover = lines[1]
		}
	}
	var sb strings.Builder
	for _, r := range hover {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			sb.WriteRune(r)
		} else if sb.Len() > 0 {
			break
		}
	}
	return sb.String()
}
