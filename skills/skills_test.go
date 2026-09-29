package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstall_RejectsSymlinkAtManagedPath verifies that a symlink planted at a
// managed skill path is never followed: Install must fail for that skill
// instead of writing through the link, and the link's target must stay
// untouched. (PR #36 security review follow-up)
func TestInstall_RejectsSymlinkAtManagedPath(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("expected embedded skill names")
	}
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	dest := t.TempDir()
	linkPath := filepath.Join(dest, names[0])
	if err := os.MkdirAll(linkPath, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(linkPath, "SKILL.md")); err != nil {
		t.Fatalf("Symlink: %v", err)
	}

	if _, err := Install(dest); err == nil {
		t.Fatal("expected Install to fail when a managed path is a symlink")
	}

	// The symlink target must not have been overwritten.
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "original" {
		t.Errorf("symlink target was overwritten: %q", data)
	}
}

// TestInstall_OverwritesRegularFiles verifies that a regular file at a managed
// path keeps the normal overwrite behavior, so managed skills stay in sync
// with the installed binary. (PR #36 security review follow-up)
func TestInstall_OverwritesRegularFiles(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("expected embedded skill names")
	}
	dest := t.TempDir()
	skillMD := filepath.Join(dest, names[0], "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillMD), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(skillMD, []byte("stale content"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	n, err := Install(dest)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if n != len(names) {
		t.Errorf("Install count = %d, want %d (embedded skill dirs, not destination contents)", n, len(names))
	}
	data, err := os.ReadFile(skillMD)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "description:") {
		t.Error("regular managed file was not overwritten with the embedded content")
	}
}

// TestNames_MatchesEmbeddedSkills verifies Names returns the embedded top-level
// skill directories. (PR #36 review follow-up)
func TestNames_MatchesEmbeddedSkills(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("expected embedded skill names")
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("Names() not sorted: %v", names)
			break
		}
	}
	for _, n := range names {
		if !strings.HasPrefix(n, "lsp-") {
			t.Errorf("unexpected non-lsp skill name %q", n)
		}
	}
}
