package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// These subcommands write to or delete from the user's config files, so every
// argument must be either recognized or rejected. Before, both ignored unknown
// arguments, and "agent-lsp uninstall --help" performed a real uninstall.

func TestParseInitArgs(t *testing.T) {
	tests := []struct {
		args     []string
		want     initOptions
		wantHelp bool
		wantErr  bool
	}{
		{args: nil, want: initOptions{}},
		{args: []string{"--non-interactive"}, want: initOptions{nonInteractive: true}},
		{args: []string{"--with-skills", "--non-interactive"}, want: initOptions{nonInteractive: true, withSkills: true}},
		{args: []string{"--help"}, wantHelp: true},
		{args: []string{"-h"}, wantHelp: true},
		{args: []string{"help"}, wantHelp: true},
		{args: []string{"--non-interactive", "--help"}, wantHelp: true},
		{args: []string{"--dry-run"}, wantErr: true},
		{args: []string{"--with-skill"}, wantErr: true},
	}
	for _, tt := range tests {
		got, err := parseInitArgs(tt.args)
		switch {
		case tt.wantHelp:
			if !errors.Is(err, errHelpRequested) {
				t.Errorf("parseInitArgs(%q) err = %v, want help requested", tt.args, err)
			}
		case tt.wantErr:
			if err == nil || errors.Is(err, errHelpRequested) {
				t.Errorf("parseInitArgs(%q) err = %v, want unknown-argument error", tt.args, err)
			}
		default:
			if err != nil || got != tt.want {
				t.Errorf("parseInitArgs(%q) = %+v, %v; want %+v, nil", tt.args, got, err, tt.want)
			}
		}
	}
}

func TestParseUninstallArgs(t *testing.T) {
	tests := []struct {
		args     []string
		wantDry  bool
		wantHelp bool
		wantErr  bool
	}{
		{args: nil},
		{args: []string{"--dry-run"}, wantDry: true},
		{args: []string{"--help"}, wantHelp: true},
		{args: []string{"-h"}, wantHelp: true},
		{args: []string{"help"}, wantHelp: true},
		{args: []string{"--dry-run", "--help"}, wantHelp: true},
		{args: []string{"--dryrun"}, wantErr: true},
		{args: []string{"--force"}, wantErr: true},
	}
	for _, tt := range tests {
		dry, err := parseUninstallArgs(tt.args)
		switch {
		case tt.wantHelp:
			if !errors.Is(err, errHelpRequested) {
				t.Errorf("parseUninstallArgs(%q) err = %v, want help requested", tt.args, err)
			}
		case tt.wantErr:
			if err == nil || errors.Is(err, errHelpRequested) {
				t.Errorf("parseUninstallArgs(%q) err = %v, want unknown-argument error", tt.args, err)
			}
		default:
			if err != nil || dry != tt.wantDry {
				t.Errorf("parseUninstallArgs(%q) = %v, %v; want %v, nil", tt.args, dry, err, tt.wantDry)
			}
		}
	}
}

// TestManagedRulesPaths_CoversEveryRulesFileInitWrites pins that uninstall cleans
// every rules file init can write, not a hand-maintained subset.
func TestManagedRulesPaths_CoversEveryRulesFileInitWrites(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	t.Chdir(cwd)
	// Resolve through the same calls uninstall makes (symlinked temp dirs on
	// macOS make cwd differ from os.Getwd's view otherwise).
	wd, _ := os.Getwd()

	got := map[string]bool{}
	for _, p := range managedRulesPaths() {
		got[p] = true
	}
	for _, want := range []string{
		filepath.Join(wd, "CLAUDE.md"),
		filepath.Join(home, ".claude", "CLAUDE.md"),
		filepath.Join(wd, ".cursor", "rules", "agent-lsp.mdc"),
		filepath.Join(wd, ".clinerules"),
		filepath.Join(home, ".windsurfrules"),
		filepath.Join(wd, "GEMINI.md"),
		filepath.Join(wd, "AGENTS.md"),
		filepath.Join(home, ".pi", "agent", "AGENTS.md"),
	} {
		if !got[want] {
			t.Errorf("uninstall does not clean %s", want)
		}
	}
}
