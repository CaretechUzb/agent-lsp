package tools

import (
	"context"
	"strings"
	"testing"
)

// TestDiagnosticsHint_DeadChannel verifies that an empty result from a document
// with no publishDiagnostics notification is reported honestly rather than as
// "safe to proceed". (issue #44)
func TestDiagnosticsHint_DeadChannel(t *testing.T) {
	hint := diagnosticsHint(false, []string{"file:///dead.go"}, 0)
	if !strings.Contains(hint, "No diagnostics received") {
		t.Errorf("expected 'No diagnostics received', got: %s", hint)
	}
	if strings.Contains(hint, "Safe to proceed") {
		t.Errorf("must not claim safety for a dead channel, got: %s", hint)
	}
	if !strings.Contains(hint, "does not confirm the file is clean") {
		t.Errorf("expected 'does not confirm the file is clean', got: %s", hint)
	}
}

// TestDiagnosticsHint_LiveEmptyPublish verifies that a published empty array
// (live channel, nothing found) still yields the safe hint. (issue #44)
func TestDiagnosticsHint_LiveEmptyPublish(t *testing.T) {
	hint := diagnosticsHint(false, nil, 1)
	if hint != "No errors. Safe to proceed." {
		t.Errorf("expected safe hint, got: %s", hint)
	}
}

// TestDiagnosticsHint_ErrorsPresent verifies that actual errors keep the
// suggest_fixes hint, even on a dead channel. (issue #44)
func TestDiagnosticsHint_ErrorsPresent(t *testing.T) {
	hint := diagnosticsHint(true, nil, 0)
	if !strings.Contains(hint, "suggest_fixes") {
		t.Errorf("expected fixes hint, got: %s", hint)
	}
}

// TestDiagnosticsHint_ErrorsWithDeadDocuments verifies that an all-documents
// query with errors in one file still reports the documents whose diagnostics
// channel never published, instead of hiding them behind the fixes hint.
// (issue #44 review follow-up)
func TestDiagnosticsHint_ErrorsWithDeadDocuments(t *testing.T) {
	hint := diagnosticsHint(true, []string{"file:///b_dead.go", "file:///a_dead.go"}, 1)
	if !strings.Contains(hint, "suggest_fixes") {
		t.Errorf("expected fixes hint, got: %s", hint)
	}
	if !strings.Contains(hint, "file:///a_dead.go, file:///b_dead.go") {
		t.Errorf("expected sorted dead-URI note, got: %s", hint)
	}
	if !strings.Contains(hint, "not confirmed clean") {
		t.Errorf("expected dead-file caveat, got: %s", hint)
	}
}

// TestDiagnosticsHint_MixedLiveAndDead verifies that live files keep the safe
// hint while dead files are listed as unconfirmed, in deterministic order.
// (issue #44)
func TestDiagnosticsHint_MixedLiveAndDead(t *testing.T) {
	hint := diagnosticsHint(false, []string{"file:///b_dead.go", "file:///a_dead.go"}, 2)
	if !strings.Contains(hint, "No errors. Safe to proceed.") {
		t.Errorf("expected safe hint for the live files, got: %s", hint)
	}
	if !strings.Contains(hint, "file:///a_dead.go, file:///b_dead.go") {
		t.Errorf("expected sorted dead-URI note, got: %s", hint)
	}
	if !strings.Contains(hint, "those files are not confirmed clean") {
		t.Errorf("expected dead-file caveat, got: %s", hint)
	}
}

// TestDiagnosticsHint_NoQueriedDocuments verifies the empty-query case preserves
// the historical hint (no file_path and no open documents). (issue #44)
func TestDiagnosticsHint_NoQueriedDocuments(t *testing.T) {
	if hint := diagnosticsHint(false, nil, 0); hint != "No errors. Safe to proceed." {
		t.Errorf("expected safe hint for empty query, got: %s", hint)
	}
}

// TestPostEditDiagnosticsHint_Verified verifies the unchanged hint shape when
// the server published diagnostics for the document. (issue #44)
func TestPostEditDiagnosticsHint_Verified(t *testing.T) {
	hint := postEditDiagnosticsHint(0, 0, true)
	want := "errors_after: 0, warnings_after: 0. Run get_diagnostics for details."
	if hint != want {
		t.Errorf("got %q, want %q", hint, want)
	}
}

// TestPostEditDiagnosticsHint_Unverified verifies that a dead channel annotates
// the post-edit report as unverified rather than asserting a clean edit.
// (issue #44)
func TestPostEditDiagnosticsHint_Unverified(t *testing.T) {
	hint := postEditDiagnosticsHint(0, 0, false)
	if !strings.Contains(hint, "errors_after: 0, warnings_after: 0") {
		t.Errorf("expected counts in hint, got: %s", hint)
	}
	if !strings.Contains(hint, "unverified") {
		t.Errorf("expected 'unverified' annotation, got: %s", hint)
	}
	if !strings.Contains(hint, "channel may be dead") {
		t.Errorf("expected dead-channel note, got: %s", hint)
	}
}

// TestGetDiagnosticsForFileStatus_NilClient verifies the nil-client guard also
// reports the channel as unverified. (issue #44)
func TestGetDiagnosticsForFileStatus_NilClient(t *testing.T) {
	errs, warns, verified := getDiagnosticsForFileStatus(context.Background(), nil, "/tmp/test.go")
	if errs != 0 || warns != 0 || verified {
		t.Errorf("expected (0, 0, false), got (%d, %d, %v)", errs, warns, verified)
	}
}
