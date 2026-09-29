package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blackwell-systems/agent-lsp/skills"
)

func TestCleanMCPConfig_PreservesOtherServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")

	cfg := map[string]any{
		"mcpServers": map[string]any{
			"lsp": map[string]any{
				"type":    "stdio",
				"command": "agent-lsp",
				"args":    []string{"go:gopls"},
			},
			"other-server": map[string]any{
				"type":    "stdio",
				"command": "other-binary",
			},
		},
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(path, data, 0o644)

	removed, skipped := cleanMCPConfig(path, false)
	if removed != 1 {
		t.Errorf("expected removed=1, got %d", removed)
	}
	if skipped != 0 {
		t.Errorf("expected skipped=0, got %d", skipped)
	}

	// Verify other-server is preserved.
	result, _ := os.ReadFile(path)
	var parsed map[string]any
	json.Unmarshal(result, &parsed)
	servers := parsed["mcpServers"].(map[string]any)

	if _, ok := servers["lsp"]; ok {
		t.Error("lsp key should have been removed")
	}
	if _, ok := servers["other-server"]; !ok {
		t.Error("other-server key should have been preserved")
	}
}

func TestCleanMCPConfig_RemovesBothKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")

	cfg := map[string]any{
		"mcpServers": map[string]any{
			"lsp":       map[string]any{"command": "agent-lsp"},
			"agent-lsp": map[string]any{"command": "agent-lsp"},
			"keep":      map[string]any{"command": "other"},
		},
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(path, data, 0o644)

	removed, _ := cleanMCPConfig(path, false)
	if removed != 2 {
		t.Errorf("expected removed=2, got %d", removed)
	}

	result, _ := os.ReadFile(path)
	var parsed map[string]any
	json.Unmarshal(result, &parsed)
	servers := parsed["mcpServers"].(map[string]any)
	if len(servers) != 1 {
		t.Errorf("expected 1 remaining server, got %d", len(servers))
	}
}

func TestCleanMCPConfig_MissingFile(t *testing.T) {
	removed, skipped := cleanMCPConfig("/nonexistent/path/.mcp.json", false)
	if removed != 0 || skipped != 1 {
		t.Errorf("expected (0,1), got (%d,%d)", removed, skipped)
	}
}

func TestCleanClaudeMDSection_PreservesSurroundingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")

	content := `# My Config

Some content before.

<!-- agent-lsp:skills:start -->
## LSP Skills
Lots of skill documentation here.
<!-- agent-lsp:skills:end -->

Some content after.
`
	os.WriteFile(path, []byte(content), 0o644)

	removed, skipped := cleanManagedSection(path, false)
	if removed != 1 {
		t.Errorf("expected removed=1, got %d", removed)
	}
	if skipped != 0 {
		t.Errorf("expected skipped=0, got %d", skipped)
	}

	result, _ := os.ReadFile(path)
	resultStr := string(result)

	if expected := "Some content before."; !contains(resultStr, expected) {
		t.Error("content before sentinel should be preserved")
	}
	if expected := "Some content after."; !contains(resultStr, expected) {
		t.Error("content after sentinel should be preserved")
	}
	if contains(resultStr, "agent-lsp:skills:start") {
		t.Error("start sentinel should have been removed")
	}
	if contains(resultStr, "LSP Skills") {
		t.Error("managed section content should have been removed")
	}
}

func TestCleanClaudeMDSection_NoSentinels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLAUDE.md")
	os.WriteFile(path, []byte("# Just a normal file\n"), 0o644)

	removed, skipped := cleanManagedSection(path, false)
	if removed != 0 || skipped != 1 {
		t.Errorf("expected (0,1), got (%d,%d)", removed, skipped)
	}
}

func TestCleanClaudeMDSection_MissingFile(t *testing.T) {
	removed, skipped := cleanManagedSection("/nonexistent/CLAUDE.md", false)
	if removed != 0 || skipped != 1 {
		t.Errorf("expected (0,1), got (%d,%d)", removed, skipped)
	}
}

func TestUninstallDryRun_NoSideEffects(t *testing.T) {
	dir := t.TempDir()

	// Create an MCP config.
	mcpPath := filepath.Join(dir, ".mcp.json")
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"lsp": map[string]any{"command": "agent-lsp"},
		},
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(mcpPath, data, 0o644)
	originalData, _ := os.ReadFile(mcpPath)

	// Create a CLAUDE.md with sentinels.
	claudePath := filepath.Join(dir, "CLAUDE.md")
	claudeContent := "before\n<!-- agent-lsp:skills:start -->\nstuff\n<!-- agent-lsp:skills:end -->\nafter\n"
	os.WriteFile(claudePath, []byte(claudeContent), 0o644)

	// Create a skill directory.
	skillDir := filepath.Join(dir, "skills", "lsp-test")
	os.MkdirAll(skillDir, 0o755)

	// Create a cache directory.
	cacheDir := filepath.Join(dir, "cache")
	os.MkdirAll(cacheDir, 0o755)

	// Run dry-run on individual functions.
	cleanMCPConfig(mcpPath, true)
	cleanManagedSection(claudePath, true)
	cleanSkillDirs(filepath.Join(dir, "skills"), true)
	cleanPath(cacheDir, true)

	// Verify nothing was changed.
	afterData, _ := os.ReadFile(mcpPath)
	if string(afterData) != string(originalData) {
		t.Error("MCP config was modified during dry-run")
	}

	afterClaude, _ := os.ReadFile(claudePath)
	if string(afterClaude) != claudeContent {
		t.Error("CLAUDE.md was modified during dry-run")
	}

	if _, err := os.Stat(skillDir); os.IsNotExist(err) {
		t.Error("skill directory was removed during dry-run")
	}

	if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
		t.Error("cache directory was removed during dry-run")
	}
}

func TestCleanSkillDirs(t *testing.T) {
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, "skills")

	// Two managed skills (real embedded names) plus a user-owned skill that
	// shares the lsp- prefix but is not part of the embedded set: uninstall
	// must remove the managed set only. (PR #36 review follow-up)
	names := skills.Names()
	if len(names) < 2 {
		t.Fatalf("expected at least two embedded skill names, got %v", names)
	}
	for _, name := range names[:2] {
		os.MkdirAll(filepath.Join(skillsDir, name), 0o755)
	}
	os.MkdirAll(filepath.Join(skillsDir, "other-skill"), 0o755)
	os.MkdirAll(filepath.Join(skillsDir, "lsp-not-managed-by-this-binary"), 0o755)

	removed, _ := cleanSkillDirs(skillsDir, false)
	if removed != 2 {
		t.Errorf("expected removed=2, got %d", removed)
	}

	// The managed set is gone; the user-owned skills remain.
	remaining := map[string]bool{}
	entries, _ := os.ReadDir(skillsDir)
	for _, e := range entries {
		remaining[e.Name()] = true
	}
	if len(remaining) != 2 || !remaining["other-skill"] || !remaining["lsp-not-managed-by-this-binary"] {
		t.Errorf("expected only the user-owned skills to remain, got %v", remaining)
	}
}

// TestCleanManagedSection_RemovesBothSentinelPairs verifies that a file holding
// both the rules and skills sentinel pairs loses both sections in one call.
// (PR #36 review follow-up)
func TestCleanManagedSection_RemovesBothSentinelPairs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")
	content := "# Project\n\n" +
		"<!-- agent-lsp:rules:start -->\nrules-body\n<!-- agent-lsp:rules:end -->\n" +
		"middle content\n" +
		"<!-- agent-lsp:skills:start -->\nskills-body\n<!-- agent-lsp:skills:end -->\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	removed, _ := cleanManagedSection(path, false)
	if removed != 2 {
		t.Errorf("expected removed=2, got %d", removed)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "agent-lsp:") || strings.Contains(text, "rules-body") || strings.Contains(text, "skills-body") {
		t.Errorf("managed sections not fully removed, got: %q", text)
	}
	if !strings.Contains(text, "# Project") || !strings.Contains(text, "middle content") {
		t.Errorf("user content lost, got: %q", text)
	}
}

// TestCleanMCPConfig_DryRunReportsEmptyHuskDeletion verifies the dry-run
// previews the file deletion for a config that would become an empty husk
// after the managed keys are removed. (PR #36 review follow-up)
func TestCleanMCPConfig_DryRunReportsEmptyHuskDeletion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	cfg := map[string]any{"mcpServers": map[string]any{"lsp": map[string]any{"command": "agent-lsp"}}}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	removed, _ := cleanMCPConfig(path, true)
	if removed == 0 {
		t.Error("expected dry-run to report the removal")
	}
	// Dry run must not touch the file.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("dry-run must not delete the config file: %v", err)
	}

	// The real run deletes the husk.
	removed, _ = cleanMCPConfig(path, false)
	if removed == 0 {
		t.Error("expected the empty husk to be removed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("expected the empty husk config to be deleted")
	}
}

func TestCleanPath_MissingPath(t *testing.T) {
	removed, skipped := cleanPath("/nonexistent/path", false)
	if removed != 0 || skipped != 1 {
		t.Errorf("expected (0,1), got (%d,%d)", removed, skipped)
	}
}

func TestCleanPath_ExistingDir(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "cache")
	os.MkdirAll(cacheDir, 0o755)
	os.WriteFile(filepath.Join(cacheDir, "data.bin"), []byte("data"), 0o644)

	removed, skipped := cleanPath(cacheDir, false)
	if removed != 1 || skipped != 0 {
		t.Errorf("expected (1,0), got (%d,%d)", removed, skipped)
	}

	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Error("cache directory should have been removed")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestCleanManagedSection_RulesSentinels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")

	content := "before\n<!-- agent-lsp:rules:start -->\nrules\n<!-- agent-lsp:rules:end -->\nafter\n"
	os.WriteFile(path, []byte(content), 0o644)

	removed, skipped := cleanManagedSection(path, false)
	if removed != 1 || skipped != 0 {
		t.Fatalf("expected (1,0), got (%d,%d)", removed, skipped)
	}

	result, _ := os.ReadFile(path)
	resultStr := string(result)
	if contains(resultStr, "agent-lsp:rules:start") || contains(resultStr, "rules\n") {
		t.Error("rules managed section was not removed")
	}
	if !contains(resultStr, "before") || !contains(resultStr, "after") {
		t.Error("surrounding content should be preserved")
	}
}

func TestCleanMCPConfig_RemovesEmptyGeneratedConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")

	cfg := map[string]any{
		"mcpServers": map[string]any{
			"agent-lsp": map[string]any{"type": "stdio", "command": "agent-lsp"},
		},
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(path, data, 0o644)

	removed, _ := cleanMCPConfig(path, false)
	if removed < 1 {
		t.Fatalf("expected at least 1 removed item, got %d", removed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("empty generated config file should have been removed")
	}
}

func TestCleanMCPConfig_KeepsFileWithUserServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")

	cfg := map[string]any{
		"mcpServers": map[string]any{
			"agent-lsp": map[string]any{"command": "agent-lsp"},
			"other":     map[string]any{"command": "other"},
		},
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(path, data, 0o644)

	cleanMCPConfig(path, false)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("config file with user servers must be kept")
	}
	result, _ := os.ReadFile(path)
	var parsed map[string]any
	json.Unmarshal(result, &parsed)
	servers := parsed["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Error("user server entry was lost")
	}
	if _, ok := servers["agent-lsp"]; ok {
		t.Error("agent-lsp entry should have been removed")
	}
}

func TestCleanManagedSection_RemovesGeneratedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "AGENTS.md")

	content := "<!-- agent-lsp:rules:start -->\nrules\n<!-- agent-lsp:rules:end -->\n"
	os.WriteFile(path, []byte(content), 0o644)

	removed, _ := cleanManagedSection(path, false)
	if removed != 1 {
		t.Fatalf("expected removed=1, got %d", removed)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("generated rules file with no other content should have been removed")
	}
}

func TestCleanMCPConfig_RemovesAlreadyEmptyHusk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	os.WriteFile(path, []byte("{\n  \"mcpServers\": {}\n}"), 0o644)

	removed, skipped := cleanMCPConfig(path, false)
	if removed != 1 || skipped != 0 {
		t.Fatalf("expected (1,0), got (%d,%d)", removed, skipped)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("empty husk config should have been removed")
	}
}
