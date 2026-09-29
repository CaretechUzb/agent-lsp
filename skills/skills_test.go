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

// TestInstall_RejectsSymlinkedSkillDirectory verifies that a symlinked skill
// DIRECTORY is not followed either. Checking only the final file path is not
// enough: a project can ship .agents/skills/<skill> as a link to any
// user-writable directory, and the staged write would land SKILL.md there.
func TestInstall_RejectsSymlinkedSkillDirectory(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("expected embedded skill names")
	}
	outside := t.TempDir()
	dest := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dest, names[0])); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := Install(dest); err == nil {
		t.Fatal("expected Install to fail when a managed skill directory is a symlink")
	}

	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		t.Errorf("file written through symlinked directory: %s", filepath.Join(outside, e.Name()))
	}
}

// TestInstall_AllowsSymlinkedDestination verifies the destination root itself
// may be a symlink (users commonly symlink ~/.claude/skills to a dotfiles
// repo); only directories below it are refused.
func TestInstall_AllowsSymlinkedDestination(t *testing.T) {
	targetDir := t.TempDir()
	dest := filepath.Join(t.TempDir(), "skills")
	if err := os.Symlink(targetDir, dest); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Install(dest); err != nil {
		t.Fatalf("Install into symlinked destination: %v", err)
	}
	if _, err := os.Stat(filepath.Join(targetDir, Names()[0], "SKILL.md")); err != nil {
		t.Errorf("expected skills installed under the destination's target: %v", err)
	}
}

// TestInstall_FilesAreWorldReadable verifies installed skill files use 0644, not
// the 0600 os.CreateTemp default: the project target is committed and read by
// other tools.
func TestInstall_FilesAreWorldReadable(t *testing.T) {
	dest := t.TempDir()
	if _, err := Install(dest); err != nil {
		t.Fatalf("Install: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dest, Names()[0], "SKILL.md"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o644 {
		t.Errorf("SKILL.md mode = %o, want 644", got)
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
