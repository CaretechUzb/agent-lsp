package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackwell-systems/agent-lsp/internal/config"
)

func TestBuildLspArgs(t *testing.T) {
	tests := []struct {
		name    string
		entries []config.ServerEntry
		want    []string
	}{
		{
			name: "go/gopls no extra args",
			entries: []config.ServerEntry{
				{LanguageID: "go", Command: []string{"gopls"}},
			},
			want: []string{"go:gopls"},
		},
		{
			name: "typescript with --stdio",
			entries: []config.ServerEntry{
				{LanguageID: "typescript", Command: []string{"typescript-language-server", "--stdio"}},
			},
			want: []string{"typescript:typescript-language-server,--stdio"},
		},
		{
			name: "ruby with stdio arg",
			entries: []config.ServerEntry{
				{LanguageID: "ruby", Command: []string{"solargraph", "stdio"}},
			},
			want: []string{"ruby:solargraph,stdio"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildLspArgs(tc.entries)
			if len(got) != len(tc.want) {
				t.Fatalf("len(got)=%d, len(want)=%d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got[%d]=%q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestWriteOrMergeConfig_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", ".mcp.json")

	err := writeOrMergeConfig(path, []string{"go:gopls"})
	if err != nil {
		t.Fatalf("writeOrMergeConfig returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read written file: %v", err)
	}

	var cfg mcpConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	entry, ok := cfg.MCPServers["agent-lsp"]
	if !ok {
		t.Fatal("MCPServers[\"agent-lsp\"] not found")
	}
	if entry.Type != "stdio" {
		t.Errorf("Type=%q, want \"stdio\"", entry.Type)
	}
	if entry.Command != "agent-lsp" {
		t.Errorf("Command=%q, want \"agent-lsp\"", entry.Command)
	}
}

func TestWriteOrMergeConfig_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")

	existing := `{"mcpServers":{"other-tool":{"type":"stdio","command":"other","args":[]}}}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("could not write seed file: %v", err)
	}

	err := writeOrMergeConfig(path, []string{"go:gopls"})
	if err != nil {
		t.Fatalf("writeOrMergeConfig returned error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read merged file: %v", err)
	}

	var cfg mcpConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if _, ok := cfg.MCPServers["other-tool"]; !ok {
		t.Error("\"other-tool\" key was lost after merge")
	}
	if _, ok := cfg.MCPServers["agent-lsp"]; !ok {
		t.Error("\"agent-lsp\" key not present after merge")
	}
}

func TestResolveTargetPath_ProjectFiles(t *testing.T) {
	tests := []struct {
		choice int
		suffix string
	}{
		{1, ".mcp.json"},
		{4, filepath.Join(".cursor", "mcp.json")},
		{5, filepath.Join(".vscode", "cline_mcp_settings.json")},
		{7, filepath.Join(".gemini", "settings.json")},
	}

	for _, tc := range tests {
		t.Run(tc.suffix, func(t *testing.T) {
			got, err := resolveTargetPath(tc.choice, "")
			if err != nil {
				t.Fatalf("resolveTargetPath(%d, \"\") error: %v", tc.choice, err)
			}
			if !strings.HasSuffix(got, tc.suffix) {
				t.Errorf("got %q, want suffix %q", got, tc.suffix)
			}
		})
	}
}

func TestResolveTargetPath_Custom(t *testing.T) {
	got, err := resolveTargetPath(8, "~/foo/bar.json")
	if err != nil {
		t.Fatalf("resolveTargetPath(8, ...) error: %v", err)
	}
	if strings.HasPrefix(got, "~") {
		t.Errorf("tilde was not expanded: got %q", got)
	}
	if !strings.HasSuffix(got, "foo/bar.json") {
		t.Errorf("expected path to end with foo/bar.json, got %q", got)
	}
}

func TestResolveTargetPath_Pi(t *testing.T) {
	project, err := resolveTargetPath(9, "")
	if err != nil {
		t.Fatalf("resolveTargetPath(9, \"\") error: %v", err)
	}
	if !strings.HasSuffix(project, filepath.Join(".mcp.json")) {
		t.Errorf("Pi project target: got %q, want suffix .mcp.json", project)
	}

	global, err := resolveTargetPath(10, "")
	if err != nil {
		t.Fatalf("resolveTargetPath(10, \"\") error: %v", err)
	}
	want := filepath.Join(".config", "mcp", "mcp.json")
	if !strings.HasSuffix(global, want) {
		t.Errorf("Pi global target: got %q, want suffix %q", global, want)
	}
}

func TestResolveRulesPath_Pi(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd error: %v", err)
	}

	project := resolveRulesPath(9)
	if want := filepath.Join(cwd, "AGENTS.md"); project != want {
		t.Errorf("Pi project rules: got %q, want %q", project, want)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir error: %v", err)
	}
	global := resolveRulesPath(10)
	if want := filepath.Join(homeDir, ".pi", "agent", "AGENTS.md"); global != want {
		t.Errorf("Pi global rules: got %q, want %q", global, want)
	}
}

func TestWriteManagedSection_PiRulesSentinels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(path, []byte("# Project\n"), 0o644); err != nil {
		t.Fatalf("could not seed AGENTS.md: %v", err)
	}

	if err := writeManagedSection(path, "managed-body"); err != nil {
		t.Fatalf("writeManagedSection error: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read AGENTS.md: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, managedSectionStart) || !strings.Contains(text, managedSectionEnd) {
		t.Error("managed section sentinels not found after write")
	}
	if !strings.Contains(text, "managed-body") {
		t.Error("managed content not found after write")
	}
	if !strings.Contains(text, "# Project") {
		t.Error("existing content was lost")
	}

	// Second write replaces the section instead of appending a duplicate.
	if err := writeManagedSection(path, "managed-body-2"); err != nil {
		t.Fatalf("second writeManagedSection error: %v", err)
	}
	data, _ = os.ReadFile(path)
	text = string(data)
	if strings.Count(text, managedSectionStart) != 1 {
		t.Errorf("expected exactly one managed section, found %d", strings.Count(text, managedSectionStart))
	}
	if !strings.Contains(text, "managed-body-2") || strings.Contains(text, "managed-body\n") {
		t.Error("managed section was not replaced on second write")
	}
}

func TestGenerateRulesContent_ProviderSkillHint(t *testing.T) {
	generic := generateRulesContent()
	if !strings.Contains(generic, "prompts/get") {
		t.Error("generic rules should mention prompts/get")
	}
	if strings.Contains(generic, "/mcp__agent-lsp__") {
		t.Error("generic rules should not mention Pi slash commands")
	}

	claude := generateRulesContent(rulesTargetClaudeCode)
	if !strings.Contains(claude, "prompts/get") {
		t.Error("Claude Code rules should mention prompts/get")
	}

	pi := generateRulesContent(rulesTargetPi)
	if !strings.Contains(pi, "/mcp__agent-lsp__") {
		t.Error("Pi rules should mention /mcp__agent-lsp__ slash commands")
	}
	if strings.Contains(pi, "prompts/get") {
		t.Error("Pi rules should not reference raw prompts/get")
	}
	if !strings.Contains(pi, "activate_skill") {
		t.Error("Pi rules should mention the activate_skill tool")
	}
}

func TestRulesTarget(t *testing.T) {
	if got := selectRulesTarget(true, false); got != rulesTargetClaudeCode {
		t.Errorf("rulesTarget(true,false) = %v, want rulesTargetClaudeCode", got)
	}
	if got := selectRulesTarget(false, true); got != rulesTargetPi {
		t.Errorf("rulesTarget(false,true) = %v, want rulesTargetPi", got)
	}
	if got := selectRulesTarget(false, false); got != rulesTargetGeneric {
		t.Errorf("rulesTarget(false,false) = %v, want rulesTargetGeneric", got)
	}
	if isPiChoice(9) != true || isPiChoice(10) != true || isPiChoice(1) != false {
		t.Error("isPiChoice returned unexpected results")
	}
}
