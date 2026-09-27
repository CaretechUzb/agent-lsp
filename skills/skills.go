// Package skills embeds all SKILL.md files for use at runtime.
//
// This package exists at the skills/ directory level so that //go:embed
// can access the SKILL.md files (Go's embed directive can only reference
// files at or below the package directory).
package skills

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed */SKILL.md */references/*.md
var Files embed.FS

// Install writes the embedded skill trees (SKILL.md plus supporting files)
// into dest. Existing files are overwritten so managed skills stay in sync
// with the installed binary. Returns the number of skills written.
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
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(dest)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() {
			n++
		}
	}
	return n, nil
}
