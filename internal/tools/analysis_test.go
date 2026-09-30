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
	hint := diagnosticsHint(false, []string{"file:///dead.go"}, 0, nil, nil)
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
	hint := diagnosticsHint(false, nil, 1, nil, nil)
	if hint != "No errors. Safe to proceed." {
		t.Errorf("expected safe hint, got: %s", hint)
	}
}

// TestDiagnosticsHint_ErrorsPresent verifies that actual errors keep the
// suggest_fixes hint, even on a dead channel. (issue #44)
func TestDiagnosticsHint_ErrorsPresent(t *testing.T) {
	hint := diagnosticsHint(true, []string{"file:///dead.go"}, 0, nil, nil)
	if !strings.Contains(hint, "suggest_fixes") {
		t.Errorf("expected fixes hint, got: %s", hint)
	}
}

// TestDiagnosticsHint_ErrorsWithDeadDocuments verifies that an all-documents
// query with errors in one file still reports the documents whose diagnostics
// channel never published, instead of hiding them behind the fixes hint.
// (issue #44 review follow-up)
func TestDiagnosticsHint_ErrorsWithDeadDocuments(t *testing.T) {
	hint := diagnosticsHint(true, []string{"file:///b_dead.go", "file:///a_dead.go"}, 1, nil, nil)
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
	hint := diagnosticsHint(false, []string{"file:///b_dead.go", "file:///a_dead.go"}, 2, nil, nil)
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
	if hint := diagnosticsHint(false, nil, 0, nil, nil); hint != "No errors. Safe to proceed." {
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

// TestShouldAttemptPull covers the push-first pull decision. A pull is only ever
// attempted for a document whose push channel is dead on a server that declares
// diagnosticProvider, while the opt-in gate is open. (issue #43)
func TestShouldAttemptPull(t *testing.T) {
	orig := pullDiagnosticsEnabled
	t.Cleanup(func() { pullDiagnosticsEnabled = orig })

	t.Run("gate open", func(t *testing.T) {
		pullDiagnosticsEnabled = func() bool { return true }
		cases := []struct {
			name        string
			hasProvider bool
			pushLive    bool
			want        bool
		}{
			{"provider and dead push", true, false, true},
			{"provider but live push is never pulled", true, true, false},
			{"no provider and dead push", false, false, false},
			{"no provider and live push", false, true, false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := shouldAttemptPull(tc.hasProvider, tc.pushLive); got != tc.want {
					t.Errorf("shouldAttemptPull(%v, %v) = %v, want %v", tc.hasProvider, tc.pushLive, got, tc.want)
				}
			})
		}
	})

	t.Run("gate closed blocks every pull", func(t *testing.T) {
		pullDiagnosticsEnabled = func() bool { return false }
		if shouldAttemptPull(true, false) {
			t.Error("expected no pull attempt while the opt-in gate is closed")
		}
	})
}

// TestDiagnosticsHint_PullSucceeded verifies that a document verified by a
// successful pull yields the ordinary safe hint: a pull answer is verification,
// so it must not be reported as an unverified dead channel. (issue #43)
func TestDiagnosticsHint_PullSucceeded(t *testing.T) {
	hint := diagnosticsHint(false, nil, 1, nil, nil)
	if hint != "No errors. Safe to proceed." {
		t.Errorf("expected safe hint after a successful pull, got: %s", hint)
	}
}

// TestDiagnosticsHint_PullAttemptedButFailed verifies the honest wording when
// the server declares diagnosticProvider, a pull was attempted for the dead
// document, and it did not respond. (issue #43)
func TestDiagnosticsHint_PullAttemptedButFailed(t *testing.T) {
	hint := diagnosticsHint(false, []string{"file:///dead.go"}, 0, []string{"file:///dead.go"}, nil)
	if !strings.Contains(hint, "its pull diagnostics did not respond") {
		t.Errorf("expected pull-did-not-respond wording, got: %s", hint)
	}
	if strings.Contains(hint, "Safe to proceed") {
		t.Errorf("must not claim safety after a failed pull, got: %s", hint)
	}
	if !strings.Contains(hint, "does not confirm the file is clean") {
		t.Errorf("expected clean-file caveat, got: %s", hint)
	}
}

// TestDiagnosticsHint_PullIncapableWordingUnchanged verifies that a server
// without diagnosticProvider keeps the original dead-channel wording, so the
// two failure modes (no pull model vs pull did not respond) stay distinct.
// (issue #43)
func TestDiagnosticsHint_PullIncapableWordingUnchanged(t *testing.T) {
	hint := diagnosticsHint(false, []string{"file:///dead.go"}, 0, nil, nil)
	if strings.Contains(hint, "pull diagnostics did not respond") {
		t.Errorf("pull-incapable wording must not mention a pull attempt, got: %s", hint)
	}
	if !strings.Contains(hint, "the server has not published any") {
		t.Errorf("expected original dead-channel wording, got: %s", hint)
	}
}

// TestDiagnosticsHint_MixedPullFailed verifies the mixed live/dead hint notes a
// failed pull for the unconfirmed files while keeping the safe hint for the
// files that did report. (issue #43)
func TestDiagnosticsHint_MixedPullFailed(t *testing.T) {
	hint := diagnosticsHint(false, []string{"file:///dead.go"}, 3, []string{"file:///dead.go"}, nil)
	if !strings.Contains(hint, "No errors. Safe to proceed.") {
		t.Errorf("expected safe hint for the verified files, got: %s", hint)
	}
	if !strings.Contains(hint, "its pull diagnostics did not respond") {
		t.Errorf("expected pull-did-not-respond note, got: %s", hint)
	}
	if !strings.Contains(hint, "those files are not confirmed clean") {
		t.Errorf("expected unconfirmed caveat, got: %s", hint)
	}
}

// TestDiagnosticsHint_PullAttemptedMixedWithSkipped verifies that when the pull
// budget ran out mid-way, only the documents whose pull actually went out are
// reported as "did not respond"; the skipped ones are stated as never
// attempted. (issue #43, review follow-up)
func TestDiagnosticsHint_PullAttemptedMixedWithSkipped(t *testing.T) {
	dead := []string{"file:///a_dead.go", "file:///b_dead.go"}
	hint := diagnosticsHint(false, dead, 0,
		[]string{"file:///a_dead.go"}, []string{"file:///b_dead.go"})
	if !strings.Contains(hint, "pull did not respond for file:///a_dead.go") {
		t.Errorf("expected attempted-dead note, got: %s", hint)
	}
	if !strings.Contains(hint, "no pull was attempted for file:///b_dead.go") {
		t.Errorf("expected skipped-dead note, got: %s", hint)
	}
	if strings.Contains(hint, "Safe to proceed") {
		t.Errorf("must not claim safety for unverified documents, got: %s", hint)
	}

	hint = diagnosticsHint(false, dead, 1,
		[]string{"file:///a_dead.go"}, []string{"file:///b_dead.go"})
	if !strings.Contains(hint, "No errors. Safe to proceed.") {
		t.Errorf("expected safe hint for the verified files, got: %s", hint)
	}
	if !strings.Contains(hint, "pull did not respond for file:///a_dead.go") {
		t.Errorf("expected attempted-dead note, got: %s", hint)
	}
	if !strings.Contains(hint, "no pull was attempted for file:///b_dead.go") {
		t.Errorf("expected skipped-dead note, got: %s", hint)
	}
}

// TestDiagnosticsHint_BudgetSkippedAllKeepsGenericWording verifies that when no
// pull was attempted at all (budget exhausted before the first pull), the
// wording makes no claim that a pull was tried. (issue #43, review follow-up)
func TestDiagnosticsHint_BudgetSkippedAllKeepsGenericWording(t *testing.T) {
	hint := diagnosticsHint(false, []string{"file:///dead.go"}, 0, nil,
		[]string{"file:///dead.go"})
	if strings.Contains(hint, "pull") {
		t.Errorf("skipped-only hint must not mention a pull attempt, got: %s", hint)
	}
	if !strings.Contains(hint, "does not confirm the file is clean") {
		t.Errorf("expected clean-file caveat, got: %s", hint)
	}
}
