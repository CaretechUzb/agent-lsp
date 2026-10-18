// change_impact.go implements the blast_radius MCP tool for blast-radius
// analysis. Given a list of changed files, it:
//
//  1. Opens each file and retrieves its exported symbols (GetDocumentSymbols).
//  2. Warms the workspace with one blocking reference query.
//  3. Calls GetReferencesRaw for each exported symbol IN PARALLEL.
//  4. Partitions callers into test files vs non-test callers, dropping
//     references inside the symbol's own declaration.
//  5. Names each reference by its enclosing function (test function for test
//     references, caller for non-test references).
//
// The result tells the agent which code paths are affected by the change,
// enabling informed decisions about whether to proceed with an edit or halt
// due to excessive blast radius.
//
// Optionally, include_transitive follows one additional level of indirection:
// for each non-test caller, find its callers too.
package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gcf "github.com/blackwell-systems/agent-lsp/internal/encoding/gcf"
	"github.com/blackwell-systems/agent-lsp/internal/lsp"
	"github.com/blackwell-systems/agent-lsp/internal/types"
	gcfgo "github.com/blackwell-systems/gcf-go"
)

// isTestFile returns true if the given path looks like a test file.
func isTestFile(path string) bool {
	if strings.HasSuffix(path, "_test.go") {
		return true
	}
	if strings.Contains(path, ".test.") || strings.Contains(path, ".spec.") {
		return true
	}
	if strings.HasPrefix(filepath.Base(path), "test_") {
		return true
	}
	return false
}

// symbolRef is a reference to a named symbol at a file location.
type symbolRef struct {
	Name string `json:"name"`
	File string `json:"file"`
	Line int    `json:"line"`
}

// callerRef is a non-test reference to a blast-radius target. Name is the
// symbol enclosing the reference (or "file:line" when none is resolved), Line
// is the 1-indexed line of the reference itself, and Target names the changed
// symbol being referenced.
type callerRef struct {
	symbolRef
	Target     string `json:"target"`
	TargetFile string `json:"-"`
	// enclosed is true when Name was resolved from documentSymbol rather than
	// synthesised from the reference location.
	enclosed bool
}

// exportedSymbol holds a symbol and its position for batch reference queries.
type exportedSymbol struct {
	Name     string
	File     string
	LangID   string
	Position types.Position
	Line     int         // 1-indexed for output
	Range    types.Range // full declaration range, used to drop self-references
}

// symbolRefs holds the references found for a single symbol.
type symbolRefs struct {
	Symbol  exportedSymbol
	Locs    []types.Location
	Warning string
}

// maxConcurrentRefs is the worker pool size for parallel reference queries.
// 16 keeps the gopls request queue saturated without flooding it. gopls has
// limited internal concurrency, so values above ~24 show diminishing returns.
const maxConcurrentRefs = 16

// maxCallerSymbolFiles caps how many distinct non-test referencing files get a
// documentSymbol request to name the enclosing caller. Each request opens the
// file on the language server, so a widely used symbol would otherwise open a
// large part of the workspace. Callers in files past the cap are named by
// file:line. Test files are not capped (they were always resolved).
const maxCallerSymbolFiles = 128

// documentSymbolTimeout caps one documentSymbol request made to name callers.
const documentSymbolTimeout = 5 * time.Second

// HandleGetChangeImpact enumerates exported symbols in each changed file via
// GetDocumentSymbols, calls GetReferencesRaw in parallel for each symbol,
// partitions results into test files vs non-test callers, and extracts
// enclosing test function names for test references.
// changeImpactTimeout caps the entire blast_radius operation. Prevents
// indefinite blocking when gopls is slow to index a new workspace.
const changeImpactTimeout = 90 * time.Second

func HandleGetChangeImpact(ctx context.Context, client *lsp.LSPClient, args map[string]any) (types.ToolResult, error) {
	if err := CheckInitialized(client); err != nil {
		return types.ErrorResult(err.Error()), nil
	}

	// Apply overall timeout so the tool never blocks indefinitely.
	ctx, cancel := context.WithTimeout(ctx, changeImpactTimeout)
	defer cancel()

	// Close the changed, caller and test files this call opens (up to
	// maxCallerSymbolFiles plus every test file), keeping ones already open.
	ctx, closeOpened := WithClosingDocuments(ctx, client)
	defer closeOpened()

	// Decode changed_files (arrives as []any from JSON).
	rawFiles, ok := args["changed_files"].([]any)
	if !ok || len(rawFiles) == 0 {
		return types.ErrorResult("changed_files is required"), nil
	}
	changedFiles := make([]string, 0, len(rawFiles))
	for _, v := range rawFiles {
		s, ok := v.(string)
		if !ok || s == "" {
			continue
		}
		changedFiles = append(changedFiles, s)
	}
	if len(changedFiles) == 0 {
		return types.ErrorResult("changed_files is required"), nil
	}

	includeTransitive := false
	if v, ok := args["include_transitive"].(bool); ok {
		includeTransitive = v
	}

	scope := "exported"
	if v, ok := args["scope"].(string); ok && v != "" {
		scope = v
	}

	filter := ""
	if v, ok := args["filter"].(string); ok && v != "" {
		filter = v
	}

	// Phase 1: Collect all exported symbols from all changed files.
	// Only collects top-level exports (functions, types, variables, constants).
	// Struct fields are excluded: they aren't independently callable and their
	// references are noise that inflates the symbol count.
	var allExports []exportedSymbol
	var allDocSymbols []types.DocumentSymbol // retained for sync-guarded detection
	filesBySymbol := make(map[string]string) // symbol name -> file path
	var warnings []string

	// documentSymbol cache shared by caller and test-function resolution,
	// keyed by file path. Seeded with the changed files so references inside
	// them do not reopen (and re-sync) the document.
	symbolCache := &sync.Map{}

	for _, file := range changedFiles {
		langID := client.LanguageIDForFile(file)
		symbols, err := WithDocument[[]types.DocumentSymbol](ctx, client, file, langID, func(fURI string) ([]types.DocumentSymbol, error) {
			return client.GetDocumentSymbols(ctx, fURI)
		})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("warning: could not get symbols for %s: %s", file, err))
			continue
		}
		if clean, cerr := ValidateFilePath(file, client.RootDir()); cerr == nil {
			symbolCache.Store(clean, symbols)
		}
		allDocSymbols = append(allDocSymbols, symbols...)
		for _, sym := range symbols {
			filesBySymbol[sym.Name] = file
		}
		if scope == "all" {
			collectAllSymbols(symbols, file, langID, &allExports, true)
		} else {
			collectExportedSymbols(symbols, file, langID, &allExports, true, 0, 0)
		}
	}

	// Phase 1.5: Warmup. The first reference query on a cold workspace forces
	// the language server to complete its full package/module load. Subsequent
	// queries are fast. We do one blocking query (with full WaitForFileIndexed)
	// on the first symbol to absorb the cold-start cost. The result is kept
	// and used in Phase 2 so we don't re-query the same symbol.
	var warmupResult *symbolRefs
	if len(allExports) > 0 {
		first := allExports[0]
		locs, err := WithDocument[[]types.Location](ctx, client, first.File, first.LangID, func(fURI string) ([]types.Location, error) {
			return client.GetReferences(ctx, fURI, first.Position, false)
		})
		ref := symbolRefs{Symbol: first, Locs: locs}
		if err != nil {
			ref.Warning = fmt.Sprintf("warning: GetReferences failed for %s in %s: %s", first.Name, first.File, err)
		}
		warmupResult = &ref
	}

	// Phase 2: Query references for remaining symbols in parallel.
	// Skip the first symbol (already queried in warmup).
	remaining := allExports
	if len(remaining) > 0 {
		remaining = remaining[1:]
	}
	refResults := queryReferencesParallel(ctx, client, remaining)

	// Prepend the warmup result so all symbols are in order.
	if warmupResult != nil {
		refResults = append([]symbolRefs{*warmupResult}, refResults...)
	}

	// Phase 3: Partition results into test vs non-test callers.
	// Build a set of sync-guarded struct types by scanning the document symbols
	// already fetched in Phase 1. A struct is sync-guarded if it contains a
	// field whose type name includes "Mutex", "RWMutex", "Lock", "synchronized",
	// "Atomic", or "sync.". This is a heuristic across language families.
	syncGuardedTypes := buildSyncGuardedSet(allDocSymbols, filesBySymbol)

	// Fetch documentSymbol for the referencing files up front, in parallel,
	// so naming callers does not cost one sequential round trip per file.
	prefetchCallerSymbols(ctx, client, symbolCache, refResults)

	part := partitionReferences(ctx, client, symbolCache, refResults, syncGuardedTypes, filter)
	changedSymbols := part.changedSymbols
	symbolsWithCallers := part.symbolsWithCallers
	testFilesSet := part.testFiles
	testFunctions := part.testFunctions
	nonTestCallers := part.nonTestCallers
	refWarnings := part.warnings

	// Phase 3.5: Transitive references (if requested).
	// Batched and parallelized like Phase 2.
	if includeTransitive && len(nonTestCallers) > 0 {
		var transitiveSymbols []exportedSymbol
		for _, caller := range nonTestCallers {
			transitiveSymbols = append(transitiveSymbols, exportedSymbol{
				Name:   caller.Name,
				File:   caller.File,
				LangID: client.LanguageIDForFile(caller.File),
				Position: types.Position{
					Line:      caller.Line - 1, // convert back to 0-indexed
					Character: 0,
				},
				Line: caller.Line,
			})
		}
		transitiveResults := queryReferencesParallel(ctx, client, transitiveSymbols)
		for _, ref := range transitiveResults {
			for _, loc := range ref.Locs {
				tPath, tErr := URIToFilePath(loc.URI)
				if tErr != nil {
					continue
				}
				if isTestFile(tPath) {
					testFilesSet[tPath] = true
				}
			}
		}
	}

	// Build testFiles slice from the set.
	testFiles := make([]string, 0, len(testFilesSet))
	for f := range testFilesSet {
		testFiles = append(testFiles, f)
	}

	// Build summary.
	summary := fmt.Sprintf("Found %d changed symbols with %d test references across %d test files.",
		len(changedSymbols), len(testFunctions), len(testFiles))
	if len(warnings) > 0 {
		summary += " Warnings: " + strings.Join(warnings, "; ")
	}

	response := map[string]any{
		"changed_symbols":  changedSymbols,
		"affected_symbols": symbolsWithCallers,
		"test_files":       testFiles,
		"test_functions":   testFunctions,
		"non_test_callers": nonTestCallers,
		"summary":          summary,
		"warnings":         refWarnings,
	}

	impactHint := "Review high-callers symbols before making changes."
	if len(refWarnings) > 0 {
		impactHint = fmt.Sprintf("%d warnings encountered during analysis. %s", len(refWarnings), impactHint)
	}

	// Graph-profile: build a *gcf.Payload for structured graph output.
	if OutputFormatFromContext(ctx) == "gcf" {
		payload := buildChangeImpactPayload(changedSymbols, nonTestCallers, testFunctions)
		result, err := EncodeResult(ctx, payload)
		if err != nil {
			return types.ErrorResult(fmt.Sprintf("encoding response: %s", err)), nil
		}
		return appendHint(result, impactHint), nil
	}

	result, err := EncodeResult(ctx, response)
	if err != nil {
		return types.ErrorResult(fmt.Sprintf("encoding response: %s", err)), nil
	}
	return appendHint(result, impactHint), nil
}

// symbolWithCallers is one changed symbol with its own test_callers and
// non_test_callers lists, so agents know which tests cover which specific
// function/method.
type symbolWithCallers struct {
	symbolRef
	TestCallers    []symbolRef `json:"test_callers"`
	NonTestCallers []symbolRef `json:"non_test_callers"`
	SyncGuarded    bool        `json:"sync_guarded,omitempty"`
}

// impactPartition is the result of partitioning reference locations into
// test and non-test callers.
type impactPartition struct {
	changedSymbols     []symbolRef
	symbolsWithCallers []symbolWithCallers
	testFiles          map[string]bool
	testFunctions      []symbolRef
	nonTestCallers     []callerRef
	warnings           []string
}

// partitionReferences splits each symbol's reference locations into test
// functions and non-test callers. References inside the symbol's own
// declaration range are dropped: some servers (odoo-ls) ignore
// includeDeclaration=false and return the definition itself, and a recursive
// self-call is not blast radius either. Non-test callers are named by their
// enclosing callable from cache; prefetchCallerSymbols fills it beforehand,
// and files missing from it are named by file:line.
func partitionReferences(ctx context.Context, client *lsp.LSPClient, cache *sync.Map, refResults []symbolRefs, syncGuardedTypes map[string]bool, filter string) impactPartition {
	part := impactPartition{testFiles: map[string]bool{}}
	testFuncSet := map[string]bool{} // dedup key: "file:name"
	paths := pathNormalizer{}

	for _, ref := range refResults {
		entry := symbolWithCallers{
			symbolRef: symbolRef{
				Name: ref.Symbol.Name,
				File: ref.Symbol.File,
				Line: ref.Symbol.Line,
			},
			SyncGuarded: isSyncGuardedSymbol(ref.Symbol.Name, syncGuardedTypes),
		}

		if ref.Warning != "" {
			part.warnings = append(part.warnings, ref.Warning)
		}

		for _, loc := range ref.Locs {
			refPath, err := URIToFilePath(loc.URI)
			if err != nil {
				continue
			}
			if refInsideSymbol(paths, ref.Symbol, refPath, loc.Range.Start) {
				continue
			}
			if isTestFile(refPath) {
				part.testFiles[refPath] = true
				enclosing := findEnclosingTestFunction(ctx, client, cache, refPath, loc.Range.Start.Line)
				if enclosing != nil {
					key := fmt.Sprintf("%s:%s", refPath, enclosing.Name)
					if !testFuncSet[key] {
						testFuncSet[key] = true
						testRef := symbolRef{
							Name: enclosing.Name,
							File: refPath,
							Line: enclosing.SelectionRange.Start.Line + 1,
						}
						part.testFunctions = append(part.testFunctions, testRef)
						entry.TestCallers = append(entry.TestCallers, testRef)
					}
				}
			} else {
				caller := callerRef{
					symbolRef: symbolRef{
						Name: fmt.Sprintf("%s:%d", filepath.Base(refPath), loc.Range.Start.Line+1),
						File: refPath,
						Line: loc.Range.Start.Line + 1,
					},
					Target:     ref.Symbol.Name,
					TargetFile: ref.Symbol.File,
				}
				if name := lookupEnclosingCaller(cache, refPath, loc.Range.Start.Line); name != "" {
					caller.Name = name
					caller.enclosed = true
				}
				part.nonTestCallers = append(part.nonTestCallers, caller)
				entry.NonTestCallers = append(entry.NonTestCallers, caller.symbolRef)
			}
		}

		part.changedSymbols = append(part.changedSymbols, symbolRef{
			Name: ref.Symbol.Name,
			File: ref.Symbol.File,
			Line: ref.Symbol.Line,
		})

		// Apply filter: "untested" keeps only symbols with non-test callers
		// but zero test callers (active in production, no test coverage).
		if filter == "untested" {
			if len(entry.NonTestCallers) == 0 || len(entry.TestCallers) > 0 {
				continue
			}
		}

		part.symbolsWithCallers = append(part.symbolsWithCallers, entry)
	}
	return part
}

// prefetchCallerSymbols fetches documentSymbol for every distinct file that
// holds a reference into cache, using a bounded worker pool with a per-file
// timeout. Non-test files are capped at maxCallerSymbolFiles. Files already in
// cache (the changed files) are skipped. A failed or timed-out file is cached
// as empty so it is not retried; once ctx is done, the remaining files are
// left out and their callers fall back to file:line names.
func prefetchCallerSymbols(ctx context.Context, client *lsp.LSPClient, cache *sync.Map, refResults []symbolRefs) {
	paths := pathNormalizer{}
	seen := map[string]bool{}
	var files []string
	nonTest := 0
	for _, ref := range refResults {
		for _, loc := range ref.Locs {
			refPath, err := URIToFilePath(loc.URI)
			if err != nil || seen[refPath] {
				continue
			}
			if refInsideSymbol(paths, ref.Symbol, refPath, loc.Range.Start) {
				continue
			}
			if _, ok := cache.Load(refPath); ok {
				seen[refPath] = true
				continue
			}
			if !isTestFile(refPath) {
				if nonTest >= maxCallerSymbolFiles {
					continue
				}
				nonTest++
			}
			seen[refPath] = true
			files = append(files, refPath)
		}
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentRefs)
	for _, f := range files {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release
			if ctx.Err() != nil {
				return
			}
			fileCtx, cancel := context.WithTimeout(ctx, documentSymbolTimeout)
			defer cancel()
			syms, err := WithDocument[[]types.DocumentSymbol](fileCtx, client, path, client.LanguageIDForFile(path), func(fURI string) ([]types.DocumentSymbol, error) {
				return client.GetDocumentSymbols(fileCtx, fURI)
			})
			if err != nil {
				syms = []types.DocumentSymbol{}
			}
			cache.Store(path, syms)
		}(f)
	}
	wg.Wait()
}

// lookupEnclosingCaller returns the qualified name of the callable enclosing
// line (0-indexed) in refPath, using only the cached documentSymbol result.
// Returns "" when the file is not cached or no callable encloses the line.
func lookupEnclosingCaller(cache *sync.Map, refPath string, line int) string {
	cached, ok := cache.Load(refPath)
	if !ok {
		return ""
	}
	syms, _ := cached.([]types.DocumentSymbol)
	_, name := findEnclosingCallable(syms, line)
	return name
}

// refInsideSymbol reports whether a reference at pos in refPath lies inside
// sym's own declaration range. A zero Range means the range is unknown.
func refInsideSymbol(paths pathNormalizer, sym exportedSymbol, refPath string, pos types.Position) bool {
	if sym.Range == (types.Range{}) {
		return false
	}
	if filepath.Base(refPath) != filepath.Base(sym.File) || paths.norm(refPath) != paths.norm(sym.File) {
		return false
	}
	return positionInRange(pos, sym.Range)
}

// positionInRange reports whether pos lies within r, both ends inclusive.
func positionInRange(pos types.Position, r types.Range) bool {
	if pos.Line < r.Start.Line || (pos.Line == r.Start.Line && pos.Character < r.Start.Character) {
		return false
	}
	if pos.Line > r.End.Line || (pos.Line == r.End.Line && pos.Character > r.End.Character) {
		return false
	}
	return true
}

// pathNormalizer memoizes absolute, symlink-resolved forms of file paths so
// a changed_files argument and a server URI for the same file compare equal
// (e.g. /tmp vs /private/tmp on macOS). Not safe for concurrent use.
type pathNormalizer map[string]string

func (m pathNormalizer) norm(p string) string {
	if n, ok := m[p]; ok {
		return n
	}
	n, err := filepath.Abs(filepath.Clean(p))
	if err != nil {
		n = filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(n); err == nil {
		n = resolved
	}
	m[p] = n
	return n
}

// nestedCallableKinds is the set of LSP SymbolKind values that remain valid
// blast-radius targets when they appear NESTED (depth > 0) inside another
// symbol. Several language servers (observed with mql-lsp-server v2.4.2) nest
// parameters and local variables as children of functions in
// textDocument/documentSymbol, so scope=exported restricts nested symbols to
// container/callable kinds and leaves locals (13 Variable), fields (8 Field),
// properties (7 Property), enum members (22 EnumMember), and literal-ish kinds
// (15-21) out. Constants (14) are an exception: they only stay targetable when
// their parent is a container kind (see constantContainerKinds) — a constant
// declared inside a function is a local implementation detail.
// scope=all (collectAllSymbols) legitimately wants the excluded nested symbols
// (fields excepted, which stay excluded there too) for dead-code detection of
// internal helpers and is unaffected.
// 12 Function, 2 Module, 3 Namespace, 4 Package, 5 Class, 6 Method,
// 9 Constructor, 10 Enum, 11 Interface, 14 Constant, 23 Struct, 24 Event.
var nestedCallableKinds = map[types.SymbolKind]bool{
	2: true, 3: true, 4: true, 5: true, 6: true, 9: true,
	10: true, 11: true, 12: true, 14: true, 23: true, 24: true,
}

// constantContainerKinds is the set of parent SymbolKinds inside which a
// nested Constant (14) stays a legitimate blast-radius target: class, struct,
// enum, namespace, module, package, interface, and event bodies. A constant
// declared inside a function or method is a local detail and is excluded from
// scope=exported.
var constantContainerKinds = map[types.SymbolKind]bool{
	2: true, 3: true, 4: true, 5: true, 10: true, 11: true, 23: true, 24: true,
}

// pythonTopLevelKinds is the set of top-level (depth 0) SymbolKinds that are
// blast-radius targets for Python at scope=exported: 5 Class, 6 Method,
// 12 Function, 14 Constant. Python servers report imports and module-level
// assignments as 13 Variable (odoo-ls) or 2 Module (pylsp for imports); in an
// Odoo model file these outnumber the real API and each costs a reference
// query. Constants stay: pyright reports UPPER_CASE module names as 14, and
// those are deliberate public names. Python is singled out because its
// module-level callables are always def/class; in JS/TS an exported arrow
// function is a Variable or Constant, so the same rule would drop real API.
// scope=all is unaffected.
var pythonTopLevelKinds = map[types.SymbolKind]bool{
	5: true, 6: true, 12: true, 14: true,
}

// collectExportedSymbols walks a DocumentSymbol tree and appends exported symbols
// to the provided slice. For Go, only uppercase symbols are exported.
// If recurseIntoChildren is false, struct fields and method children are skipped
// to avoid inflating the symbol count with non-independently-callable members.
// depth is the current nesting level: top-level symbols (depth == 0) keep every
// kind except fields, while nested symbols (depth > 0) are restricted to
// nestedCallableKinds so parameters, locals, and other non-callable members do
// not become blast-radius targets.
func collectExportedSymbols(syms []types.DocumentSymbol, filePath, langID string, out *[]exportedSymbol, recurseIntoChildren bool, depth int, parentKind types.SymbolKind) {
	// Cache source lines for resolving symbol name positions.
	// gopls returns SelectionRange.Start pointing to the keyword (e.g., "func")
	// rather than the identifier name for functions and methods. We resolve the
	// actual column by finding the symbol name in the source line.
	var sourceLines []string
	if data, err := os.ReadFile(filePath); err == nil {
		sourceLines = strings.Split(string(data), "\n")
	}

	for _, sym := range syms {
		// Skip struct fields (kind 8): they're not independently callable and
		// inflate the symbol count without adding blast-radius value.
		if sym.Kind == 8 {
			continue
		}
		// Nested symbols are only legitimate blast-radius targets when they are
		// independently callable/container kinds. Skip excluded nested symbols
		// WITHOUT recursing: a parameter has no callable children, so descending
		// would only surface more noise.
		if depth > 0 && !nestedCallableKinds[sym.Kind] {
			continue
		}
		// A nested constant stays a target only when its parent is a container
		// (class constant, enum member constant, ...); a function-local constant
		// is a local implementation detail. (issue #41 review follow-up)
		if depth > 0 && sym.Kind == 14 && !constantContainerKinds[parentKind] {
			continue
		}
		// Python imports and module-level variables are not targets; skip
		// without recursing (they have no callable children).
		if depth == 0 && langID == "python" && !pythonTopLevelKinds[sym.Kind] {
			continue
		}
		// For Go, check if the symbol is exported. Method names from gopls
		// include the receiver prefix (e.g., "(*Hub).SetSender"), so extract
		// the actual name after the last dot before checking case.
		exportName := sym.Name
		if dotIdx := strings.LastIndex(exportName, "."); dotIdx >= 0 {
			exportName = exportName[dotIdx+1:]
		}
		exported := langID != "go" || (len(exportName) > 0 && exportName[0] >= 'A' && exportName[0] <= 'Z')
		if exported {
			line := sym.SelectionRange.Start.Line
			char := sym.SelectionRange.Start.Character

			// Resolve actual column of the symbol name in the source line.
			// For methods, strip the receiver prefix (e.g., "(*LSPClient).Shutdown" -> "Shutdown").
			searchName := sym.Name
			if dotIdx := strings.LastIndex(searchName, "."); dotIdx >= 0 {
				searchName = searchName[dotIdx+1:]
			}
			if line < len(sourceLines) {
				if col := strings.Index(sourceLines[line], searchName); col >= 0 {
					char = col
				}
			}

			*out = append(*out, exportedSymbol{
				Name:   sym.Name,
				File:   filePath,
				LangID: langID,
				Position: types.Position{
					Line:      line,
					Character: char,
				},
				Line:  line + 1,
				Range: sym.Range,
			})
		}
		if recurseIntoChildren {
			collectExportedSymbols(sym.Children, filePath, langID, out, true, depth+1, sym.Kind)
		}
	}
}

// collectAllSymbols walks a DocumentSymbol tree and appends ALL symbols
// (exported and unexported) to the provided slice. Used when scope="all"
// is requested for dead code detection of internal helpers.
// Struct fields (kind 8) are still excluded.
func collectAllSymbols(syms []types.DocumentSymbol, filePath, langID string, out *[]exportedSymbol, recurseIntoChildren bool) {
	var sourceLines []string
	if data, err := os.ReadFile(filePath); err == nil {
		sourceLines = strings.Split(string(data), "\n")
	}

	for _, sym := range syms {
		if sym.Kind == 8 {
			continue
		}
		line := sym.SelectionRange.Start.Line
		char := sym.SelectionRange.Start.Character

		searchName := sym.Name
		if dotIdx := strings.LastIndex(searchName, "."); dotIdx >= 0 {
			searchName = searchName[dotIdx+1:]
		}
		if line < len(sourceLines) {
			if col := strings.Index(sourceLines[line], searchName); col >= 0 {
				char = col
			}
		}

		*out = append(*out, exportedSymbol{
			Name:   sym.Name,
			File:   filePath,
			LangID: langID,
			Position: types.Position{
				Line:      line,
				Character: char,
			},
			Line:  line + 1,
			Range: sym.Range,
		})
		if recurseIntoChildren {
			collectAllSymbols(sym.Children, filePath, langID, out, true)
		}
	}
}

// perSymbolTimeout caps how long a single reference query can take.
// Prevents one slow symbol from blocking the entire batch. Symbols that
// exceed this are skipped with a warning.
const perSymbolTimeout = 15 * time.Second

// queryReferencesParallel queries GetReferencesRaw for all symbols using a
// worker pool. Checks the persistent reference cache first; only queries the
// language server for cache misses. The caller must ensure the workspace is
// warm before calling (e.g. by doing one blocking GetReferences call first).
func queryReferencesParallel(ctx context.Context, client *lsp.LSPClient, symbols []exportedSymbol) []symbolRefs {
	results := make([]symbolRefs, len(symbols))
	cache := client.RefCache()
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrentRefs)

	for i, sym := range symbols {
		wg.Add(1)
		go func(idx int, s exportedSymbol) {
			defer wg.Done()

			// Respect context cancellation.
			if ctx.Err() != nil {
				results[idx] = symbolRefs{Symbol: s, Warning: "context cancelled"}
				return
			}

			// Check cache first.
			if cached := cache.Get(s.File, s.Name, s.Line); cached != nil {
				results[idx] = symbolRefs{Symbol: s, Locs: cached.Locations}
				return
			}

			sem <- struct{}{}        // acquire
			defer func() { <-sem }() // release

			// Per-symbol timeout prevents one slow query from blocking the batch.
			symCtx, cancel := context.WithTimeout(ctx, perSymbolTimeout)
			defer cancel()

			locs, err := WithDocument[[]types.Location](symCtx, client, s.File, s.LangID, func(fURI string) ([]types.Location, error) {
				return client.GetReferencesRaw(symCtx, fURI, s.Position, false)
			})

			ref := symbolRefs{Symbol: s, Locs: locs}
			if err != nil {
				if symCtx.Err() == context.DeadlineExceeded {
					ref.Warning = fmt.Sprintf("warning: GetReferences timed out for %s in %s after %s", s.Name, s.File, perSymbolTimeout)
				} else {
					ref.Warning = fmt.Sprintf("warning: GetReferences failed for %s in %s: %s", s.Name, s.File, err)
				}
			} else {
				// Cache successful results.
				cache.Put(s.File, s.Name, s.Line, locs)
			}
			results[idx] = ref
		}(i, sym)
	}

	wg.Wait()
	return results
}

// findEnclosingTestFunction finds the enclosing test function for a reference
// in a test file, with caching to avoid redundant GetDocumentSymbols calls.
func findEnclosingTestFunction(ctx context.Context, client *lsp.LSPClient, cache *sync.Map, refPath string, line int) *types.DocumentSymbol {
	// Check cache first.
	if cached, ok := cache.Load(refPath); ok {
		if syms, ok := cached.([]types.DocumentSymbol); ok {
			sym, _ := findEnclosingCallable(syms, line)
			return sym
		}
		return nil
	}

	// Query and cache.
	syms, err := WithDocument[[]types.DocumentSymbol](ctx, client, refPath, client.LanguageIDForFile(refPath), func(fURI string) ([]types.DocumentSymbol, error) {
		return client.GetDocumentSymbols(ctx, fURI)
	})
	if err != nil {
		cache.Store(refPath, []types.DocumentSymbol{})
		return nil
	}
	cache.Store(refPath, syms)
	sym, _ := findEnclosingCallable(syms, line)
	return sym
}

// callerKinds is the set of SymbolKinds that can name the caller enclosing a
// reference: 5 Class, 6 Method, 9 Constructor, 10 Enum, 11 Interface,
// 12 Function, 23 Struct. Variables are left out because several servers
// (odoo-ls among them) report parameters and local assignment targets as
// one-line Variable children, so on `x = target()` the smallest enclosing
// symbol would be the local `x` rather than the function.
var callerKinds = map[types.SymbolKind]bool{
	5: true, 6: true, 9: true, 10: true, 11: true, 12: true, 23: true,
}

// findEnclosingCallable walks a DocumentSymbol tree and returns the smallest
// symbol of a callerKinds kind whose range contains lineNum (0-indexed),
// descending through symbols of any kind. The returned name joins the
// enclosing callerKinds ancestors with "." (e.g. "SaleOrder.action_confirm"),
// so same-named methods of different classes stay distinct. Returns nil, ""
// if none is found.
func findEnclosingCallable(syms []types.DocumentSymbol, lineNum int) (*types.DocumentSymbol, string) {
	var best *types.DocumentSymbol
	bestName := ""
	var walk func(syms []types.DocumentSymbol, prefix string)
	walk = func(syms []types.DocumentSymbol, prefix string) {
		for i := range syms {
			sym := &syms[i]
			if lineNum < sym.Range.Start.Line || lineNum > sym.Range.End.Line {
				continue
			}
			childPrefix := prefix
			if callerKinds[sym.Kind] {
				name := sym.Name
				if prefix != "" {
					name = prefix + "." + sym.Name
				}
				// <= so a nested symbol wins a tie with its parent.
				if best == nil || sym.Range.End.Line-sym.Range.Start.Line <= best.Range.End.Line-best.Range.Start.Line {
					best = sym
					bestName = name
				}
				childPrefix = name
			}
			walk(sym.Children, childPrefix)
		}
	}
	walk(syms, "")
	return best, bestName
}

// syncFieldPatterns contains substrings that indicate a field provides
// synchronization. Language-agnostic: covers Go (sync.Mutex, sync.RWMutex),
// Java/Kotlin (ReentrantLock, synchronized), Rust (Mutex), Python (Lock),
// C/C++ (pthread_mutex, std::mutex), and atomic types.
var syncFieldPatterns = []string{
	"Mutex", "RWMutex", "Lock", "Semaphore",
	"atomic", "Atomic",
	"sync.", "pthread_mutex", "std::mutex",
}

// buildSyncGuardedSet scans document symbols and returns a set of type names
// (structs, classes) that contain at least one synchronization field.
// Uses two strategies: (1) check document symbol children if the LSP provides
// them (TypeScript, Java), (2) read the struct's source lines and pattern-match
// for sync primitives (Go/gopls doesn't nest fields as children).
func buildSyncGuardedSet(symbols []types.DocumentSymbol, filesBySymbol map[string]string) map[string]bool {
	guarded := make(map[string]bool)
	for _, sym := range symbols {
		// Check structs/classes (kind 23=struct, 5=class)
		if sym.Kind == 23 || sym.Kind == 5 {
			// Strategy 1: check children (works for TSServer, jdtls, etc.)
			for _, child := range sym.Children {
				if child.Kind == 8 || child.Kind == 7 {
					for _, pattern := range syncFieldPatterns {
						if strings.Contains(child.Name, pattern) || strings.Contains(child.Detail, pattern) {
							guarded[sym.Name] = true
							break
						}
					}
				}
				if guarded[sym.Name] {
					break
				}
			}

			// Strategy 2: read source lines of the struct (for gopls which
			// doesn't nest fields as children).
			if !guarded[sym.Name] {
				if filePath, ok := filesBySymbol[sym.Name]; ok {
					if data, err := os.ReadFile(filePath); err == nil {
						lines := strings.Split(string(data), "\n")
						start := sym.Range.Start.Line
						end := sym.Range.End.Line
						if end >= len(lines) {
							end = len(lines) - 1
						}
						for i := start; i <= end; i++ {
							for _, pattern := range syncFieldPatterns {
								if strings.Contains(lines[i], pattern) {
									guarded[sym.Name] = true
									break
								}
							}
							if guarded[sym.Name] {
								break
							}
						}
					}
				}
			}
		}
		// Recurse into children (nested types)
		for k, v := range buildSyncGuardedSet(sym.Children, filesBySymbol) {
			if v {
				guarded[k] = true
			}
		}
	}
	return guarded
}

// isSyncGuardedSymbol checks if a symbol name refers to a method on a
// sync-guarded type. Go methods from gopls include the receiver prefix
// (e.g., "(*Hub).SetSender"); this extracts the type name and checks
// against the guarded set.
func isSyncGuardedSymbol(name string, guardedTypes map[string]bool) bool {
	// Direct match (the symbol IS a guarded type)
	if guardedTypes[name] {
		return true
	}
	// Method receiver: "(*TypeName).Method" or "TypeName.Method"
	if dotIdx := strings.LastIndex(name, "."); dotIdx >= 0 {
		receiver := name[:dotIdx]
		// Strip pointer receiver syntax: "(*TypeName)" -> "TypeName"
		receiver = strings.TrimPrefix(receiver, "(*")
		receiver = strings.TrimPrefix(receiver, "(")
		receiver = strings.TrimSuffix(receiver, ")")
		if guardedTypes[receiver] {
			return true
		}
	}
	return false
}

// callerQualifiedName is the GCF qname of a non-test caller. GCF graph
// symbols carry no location field, so the reference line goes into the name:
// "pkg/dir.Caller#file.go:42". A caller named by location ("file.go:42") is
// already unique and gets "pkg/dir.file.go:42".
func callerQualifiedName(c callerRef) string {
	qn := gcf.QualifiedName(c.File, c.Name)
	if !c.enclosed {
		return qn
	}
	return fmt.Sprintf("%s#%s:%d", qn, filepath.Base(c.File), c.Line)
}

// buildChangeImpactPayload constructs a *gcfgo.Payload for graph-encoded
// blast_radius output. Target symbols are distance 0, non-test callers
// distance 1 with decaying score, test functions distance 1 with score 0.7.
// Each caller is one symbol (keyed by file and enclosing name, qname carrying
// its first reference line) with one "calls" edge per target it references,
// caller as Source and target as Target, the same direction as find_callers.
func buildChangeImpactPayload(changed []symbolRef, nonTestCallers []callerRef, testFuncs []symbolRef) *gcfgo.Payload {
	var symbols []gcfgo.Symbol
	var edges []gcfgo.Edge
	paths := pathNormalizer{}
	seen := map[string]bool{} // qnames already emitted as symbols

	// Target symbols (distance 0, score 1.0)
	targetQN := map[string]string{} // file + "\x00" + name -> qname
	for _, s := range changed {
		qn := gcf.QualifiedName(s.File, s.Name)
		targetQN[paths.norm(s.File)+"\x00"+s.Name] = qn
		if seen[qn] {
			continue
		}
		seen[qn] = true
		symbols = append(symbols, gcfgo.Symbol{
			QualifiedName: qn,
			Kind:          "function",
			Score:         1.0,
			Provenance:    "lsp_resolved",
			Distance:      0,
		})
	}

	// Non-test callers (distance 1, score 0.9 decaying by caller index)
	callerQN := map[string]string{} // file + "\x00" + name -> qname
	edgeSeen := map[string]bool{}
	for _, c := range nonTestCallers {
		key := paths.norm(c.File) + "\x00" + c.Name
		// A caller that is itself a changed symbol reuses its target node.
		src, ok := targetQN[key]
		if !ok {
			src, ok = callerQN[key]
		}
		if !ok {
			src = callerQualifiedName(c)
			callerQN[key] = src
			if !seen[src] {
				seen[src] = true
				score := max(0.1, 0.9-float64(len(callerQN)-1)*0.05)
				symbols = append(symbols, gcfgo.Symbol{
					QualifiedName: src,
					Kind:          "function",
					Score:         score,
					Provenance:    "lsp_resolved",
					Distance:      1,
				})
			}
		}
		// Edge: caller -> changed symbol. QualifiedName keeps only the
		// directory, so two same-named symbols in one package share a qname;
		// never emit the resulting self-edge.
		tgt := gcf.QualifiedName(c.TargetFile, c.Target)
		if src == tgt || edgeSeen[src+"\x00"+tgt] {
			continue
		}
		edgeSeen[src+"\x00"+tgt] = true
		edges = append(edges, gcfgo.Edge{
			Source:   src,
			Target:   tgt,
			EdgeType: "calls",
		})
	}

	// Test functions (distance 1, score 0.7)
	for _, t := range testFuncs {
		qn := gcf.QualifiedName(t.File, t.Name)
		if seen[qn] {
			continue
		}
		seen[qn] = true
		symbols = append(symbols, gcfgo.Symbol{
			QualifiedName: qn,
			Kind:          "function",
			Score:         0.7,
			Provenance:    "lsp_resolved",
			Distance:      1,
		})
	}

	return gcf.BuildGraphPayload("blast_radius", symbols, edges)
}
