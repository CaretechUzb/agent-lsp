// Package skills embeds all SKILL.md files for use at runtime.
//
// This package exists at the skills/ directory level so that //go:embed
// can access the SKILL.md files (Go's embed directive can only reference
// files at or below the package directory).
package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

//go:embed */SKILL.md */references/*.md
var Files embed.FS

// Install writes the embedded skill trees (SKILL.md plus supporting files)
// into dest. Existing managed files are replaced through a staged
// write-and-rename so a symlink planted at a managed path is never followed:
// an existing symlink is rejected outright, and the final rename replaces it
// atomically instead of writing through it. Regular files keep normal
// overwrite behavior so managed skills stay in sync with the installed
// binary. Returns the number of skills written.
func Install(dest string) (int, error) {
	err := fs.WalkDir(Files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Dir(p) == "." {
			return nil // no top-level loose files; skills are directories
		}
		target := filepath.Join(dest, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		data, err := Files.ReadFile(p)
		if err != nil {
			return err
		}
		return writeFileManaged(target, data)
	})
	if err != nil {
		return 0, err
	}
	return len(Names()), nil
}

// writeFileManaged replaces the managed file at target with data without ever
// following a symlink planted at target. The content is staged in a temporary
// file in the destination directory and renamed into place: os.Rename
// replaces the destination entry atomically (a symlink destination is
// unlinked, not written through), so a prepared link in a project-controlled
// tree cannot redirect the write outside it. A pre-existing symlink is also
// rejected outright, so installing never adopts a planted link as a managed
// file.
func writeFileManaged(target string, data []byte) error {
	if fi, err := os.Lstat(target); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to overwrite symlink at managed skill path %s", target)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".agent-lsp-skill-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// No-op once the rename below succeeds.
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

// Names returns the sorted names of the embedded top-level skill directories:
// the canonical set of skills this binary manages and installs. Uninstall uses
// it to restrict deletion to skills agent-lsp actually ships, so a user-owned
// skill that merely shares the lsp- prefix is never removed.
func Names() []string {
	entries, err := Files.ReadDir(".")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}
