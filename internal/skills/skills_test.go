package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscover_RulesSkillsSubagentsAndPrecedence(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()

	// Create .git so ancestor walker stops at root.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("# Project Rules\nAlways run go test with CGO_ENABLED=0.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("# Antigravity Rules\nPrefer deterministic Steer missions.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Workspace skill in .agents/skills/nebius-router/SKILL.md
	wsSkillDir := filepath.Join(root, ".agents", "skills", "nebius-router")
	if err := os.MkdirAll(wsSkillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillContent := `---
name: nebius-router
description: Route 90% of subtasks to Nemotron Super and 10% to Nemotron Ultra.
---
# Nebius Router Skill
Use nvidia/nemotron-3-super-120b-a12b for parallel workers.
`
	if err := os.WriteFile(filepath.Join(wsSkillDir, "SKILL.md"), []byte(skillContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Duplicate global skill with same name should be overridden by workspace skill.
	globalSkillDir := filepath.Join(home, ".gemini", "config", "skills", "nebius-router")
	if err := os.MkdirAll(globalSkillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalSkillDir, "SKILL.md"), []byte("---\nname: nebius-router\ndescription: Global fallback\n---\nBody"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Subagent in .claude/agents/uta-devops.md
	agentsDir := filepath.Join(root, ".claude", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "uta-devops.md"), []byte("---\nname: uta-devops\ndescription: Release and build verification subagent\n---\nRun goreleaser."), 0o644); err != nil {
		t.Fatal(err)
	}

	// Discover from a nested subdirectory inside root.
	subDir := filepath.Join(root, "internal", "cli")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cat := Discover(subDir, home)
	if len(cat.Rules()) != 2 {
		t.Fatalf("expected 2 rules (AGENTS.md, CLAUDE.md), got %d", len(cat.Rules()))
	}
	if len(cat.Skills()) != 1 {
		t.Fatalf("expected 1 deduplicated skill, got %d", len(cat.Skills()))
	}
	sk := cat.Skills()[0]
	if sk.Scope != "workspace" || !strings.Contains(sk.Description, "Route 90%") {
		t.Errorf("workspace skill should win precedence over global, got %+v", sk)
	}
	if len(cat.Subagents()) != 1 {
		t.Fatalf("expected 1 subagent, got %d", len(cat.Subagents()))
	}

	progCtx := cat.BuildProgressiveContext(4096)
	for _, want := range []string{
		"<workspace_rules>",
		"CGO_ENABLED=0",
		"<available_skills>",
		"nebius-router [agy]: Route 90%",
		"<available_subagents>",
		"uta-devops [claude]",
	} {
		if !strings.Contains(progCtx, want) {
			t.Errorf("BuildProgressiveContext missing %q:\n%s", want, progCtx)
		}
	}
	// Progressive disclosure: full skill body must NOT be dumped into progressive context.
	if strings.Contains(progCtx, "# Nebius Router Skill") {
		t.Errorf("progressive context should only include skill name+description, not full body:\n%s", progCtx)
	}
}

func TestExpandFileMentions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prompt := "Please review @main.go and ask @nebius to check @../secret.txt"
	expanded, attached := ExpandFileMentions(prompt, dir)
	if len(attached) != 1 || attached[0] != "main.go" {
		t.Fatalf("expected attached=[main.go], got %v", attached)
	}
	if !strings.Contains(expanded, `<file path="main.go">`) || !strings.Contains(expanded, "package main") {
		t.Errorf("expanded prompt missing main.go content:\n%s", expanded)
	}
}
