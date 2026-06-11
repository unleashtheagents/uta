package profile

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeProfile(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func TestEmbeddedDefault_ParsesAndValidates(t *testing.T) {
	p := EmbeddedDefault()
	if p.Name != DefaultProfileName {
		t.Errorf("name = %q, want %q", p.Name, DefaultProfileName)
	}
	if p.Source != EmbeddedSource {
		t.Errorf("source = %q, want %q", p.Source, EmbeddedSource)
	}
	if err := p.Validate(); err != nil {
		t.Errorf("embedded default failed validation: %v", err)
	}
	// Returning fresh copies each call; mutating one should not stomp
	// the next caller.
	p.Description = "mutated"
	if EmbeddedDefault().Description == "mutated" {
		t.Error("EmbeddedDefault must return a fresh copy each call")
	}
}

// TestEmbeddedDefault_NoFutureFieldLeakage pins the embedded default to the
// original MissionProfile spec (name, description, personas, allowed_tools,
// denied_tools, env, policies). Later iterations grew the struct with
// mcp_servers, on_complete, memory, retrospective_every, shadow_drift_tokens
// and friends — those belong on opt-in user profiles, not the embedded
// baseline. Anyone adding such a key to defaultProfileYAML will trip this
// test, which is the prompt to put it on an example file instead.
func TestEmbeddedDefault_NoFutureFieldLeakage(t *testing.T) {
	forbidden := []string{
		"mcp_servers",
		"on_complete",
		"memory",
		"retrospective_every",
		"retrospective_prompt",
		"shadow_drift_tokens",
		"max_handoff_depth",
		"tools",
	}
	// Match YAML keys at the start of a line (optionally indented) so we
	// don't false-fire on substrings like "tools" inside "allowed_tools".
	for _, key := range forbidden {
		pat := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `\s*:`)
		if pat.MatchString(defaultProfileYAML) {
			t.Errorf("embedded default YAML contains a %q: field — keep it to the original spec; add per-mode opts to example/user profiles instead", key)
		}
	}
	// And the embedded default's parsed form must also reflect zero values
	// for those advanced knobs.
	p := EmbeddedDefault()
	if len(p.MCPServers) != 0 || len(p.OnComplete) != 0 || p.MaxHandoffDepth != 0 ||
		p.RetrospectiveEvery != 0 || p.RetrospectivePrompt != "" || p.ShadowDriftTokens != 0 ||
		p.Memory.Consolidate || p.Memory.TopK != 0 || len(p.Tools) != 0 {
		t.Errorf("embedded default carries non-zero advanced fields: %+v", p)
	}
}

func TestLoad_FullProfile(t *testing.T) {
	dir := t.TempDir()
	path := writeProfile(t, dir, "dev.yaml", `
name: dev
description: development mode
personas:
  - architect
  - engineer
allowed_tools:
  - Read
  - Edit
  - Bash(go *)
denied_tools:
  - Bash(curl *)
env:
  GO111MODULE: "on"
  FOO: bar
policies:
  hitl_severity: high
  token_budget: 100000
  per_call_budget: 8000
`)
	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.Name != "dev" {
		t.Errorf("name = %q", p.Name)
	}
	if p.Description != "development mode" {
		t.Errorf("description = %q", p.Description)
	}
	if len(p.Personas) != 2 || p.Personas[0] != "architect" {
		t.Errorf("personas = %v", p.Personas)
	}
	if len(p.AllowedTools) != 3 {
		t.Errorf("allowed_tools = %v", p.AllowedTools)
	}
	if len(p.DeniedTools) != 1 || p.DeniedTools[0] != "Bash(curl *)" {
		t.Errorf("denied_tools = %v", p.DeniedTools)
	}
	if p.Env["GO111MODULE"] != "on" || p.Env["FOO"] != "bar" {
		t.Errorf("env = %v", p.Env)
	}
	if p.Policies.HITLSeverity != "high" {
		t.Errorf("hitl_severity = %q", p.Policies.HITLSeverity)
	}
	if p.Policies.TokenBudget != 100000 || p.Policies.PerCallBudget != 8000 {
		t.Errorf("budgets = %+v", p.Policies)
	}
	if p.Source != path {
		t.Errorf("source = %q, want %q", p.Source, path)
	}
}

func TestLoad_UnknownFieldFailsLoud(t *testing.T) {
	dir := t.TempDir()
	path := writeProfile(t, dir, "bad.yaml", `
name: bad
description: has an unknown field
mystery_field: nope
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load: want error for unknown field, got nil")
	}
	// yaml.v3 surfaces "field <name> not found" — assert on that hint.
	if !strings.Contains(err.Error(), "mystery_field") {
		t.Errorf("error should name the unknown field, got: %v", err)
	}
}

func TestLoad_MalformedYAMLRejected(t *testing.T) {
	dir := t.TempDir()
	// Indentation breakage + unterminated string — yaml.v3 should refuse
	// to decode this; the loader must wrap the error with "parse:" so
	// CLI callers can attribute it.
	path := writeProfile(t, dir, "bad.yaml", "name: bad\npersonas:\n  - architect\n   - engineer: \"unterminated\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load: want error for malformed YAML, got nil")
	}
	if !strings.Contains(err.Error(), "parse:") {
		t.Errorf("error should be tagged as a parse failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should include the source path, got: %v", err)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatal("Load: want error for missing file, got nil")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist error, got: %v", err)
	}
}

func TestValidate_NameRequired(t *testing.T) {
	cases := []struct {
		label string
		name  string
	}{
		{"empty", ""},
		{"whitespace-only", "   "},
		{"leading-space", " dev"},
		{"trailing-space", "dev "},
	}
	for _, c := range cases {
		p := &MissionProfile{Name: c.name}
		if err := p.Validate(); err == nil {
			t.Errorf("%s: want validation error, got nil", c.label)
		}
	}
}

func TestValidate_NameNoPathSeparators(t *testing.T) {
	for _, n := range []string{"foo/bar", `foo\bar`} {
		p := &MissionProfile{Name: n}
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "path separators") {
			t.Errorf("name=%q: want 'path separators' error, got %v", n, err)
		}
	}
}

func TestValidate_HITLSeverity(t *testing.T) {
	cases := []struct {
		sev     string
		wantErr bool
	}{
		{"", false},
		{"low", false},
		{"medium", false},
		{"high", false},
		{"critical", false},
		{"HIGH", true},
		{"warn", true},
		{"none", true},
	}
	for _, c := range cases {
		p := &MissionProfile{Name: "x", Policies: Policies{HITLSeverity: c.sev}}
		err := p.Validate()
		if c.wantErr && err == nil {
			t.Errorf("sev=%q: want error, got nil", c.sev)
		}
		if !c.wantErr && err != nil {
			t.Errorf("sev=%q: unexpected error: %v", c.sev, err)
		}
	}
}

func TestValidate_HITLTokenThresholdRange(t *testing.T) {
	cases := []struct {
		pct     int
		wantErr bool
	}{
		{0, false}, {1, false}, {50, false}, {100, false},
		{-1, true}, {101, true},
	}
	for _, c := range cases {
		p := &MissionProfile{Name: "x", Policies: Policies{HITLTokenThreshold: c.pct}}
		err := p.Validate()
		if c.wantErr && err == nil {
			t.Errorf("pct=%d: want error, got nil", c.pct)
		}
		if !c.wantErr && err != nil {
			t.Errorf("pct=%d: unexpected error %v", c.pct, err)
		}
	}
}

func TestLoad_HITLTriggers(t *testing.T) {
	dir := t.TempDir()
	path := writeProfile(t, dir, "dev.yaml", `
name: dev
policies:
  hitl_severity: high
  hitl_triggers:
    - "Bash(* push *)"
    - "Bash(* deploy *)"
  hitl_token_threshold: 80
  token_budget: 100000
`)
	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.Policies.HITLTriggers) != 2 {
		t.Fatalf("hitl_triggers len = %d", len(p.Policies.HITLTriggers))
	}
	if p.Policies.HITLTriggers[1] != "Bash(* deploy *)" {
		t.Errorf("hitl_triggers[1] = %q", p.Policies.HITLTriggers[1])
	}
	if p.Policies.HITLTokenThreshold != 80 {
		t.Errorf("hitl_token_threshold = %d, want 80", p.Policies.HITLTokenThreshold)
	}
}

func TestValidate_BudgetsNonNegative(t *testing.T) {
	p := &MissionProfile{Name: "x", Policies: Policies{TokenBudget: -1}}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "token_budget") {
		t.Errorf("negative token_budget: want error, got %v", err)
	}
	p = &MissionProfile{Name: "x", Policies: Policies{PerCallBudget: -5}}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "per_call_budget") {
		t.Errorf("negative per_call_budget: want error, got %v", err)
	}
	p = &MissionProfile{Name: "x", Policies: Policies{DollarBudgetCents: -1}}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "dollar_budget_cents") {
		t.Errorf("negative dollar_budget_cents: want error, got %v", err)
	}
}

func TestValidate_RetrospectiveEveryNonNegative(t *testing.T) {
	p := &MissionProfile{Name: "x", RetrospectiveEvery: -1}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "retrospective_every") {
		t.Errorf("negative retrospective_every: want error, got %v", err)
	}
	p = &MissionProfile{Name: "x", RetrospectiveEvery: 0}
	if err := p.Validate(); err != nil {
		t.Errorf("zero retrospective_every should be valid (means 'disabled'), got %v", err)
	}
	p = &MissionProfile{Name: "x", RetrospectiveEvery: 10}
	if err := p.Validate(); err != nil {
		t.Errorf("positive retrospective_every should be valid, got %v", err)
	}
}

func TestValidate_ShadowDriftTokensNonNegative(t *testing.T) {
	p := &MissionProfile{Name: "x", ShadowDriftTokens: -1}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "shadow_drift_tokens") {
		t.Errorf("negative shadow_drift_tokens: want error, got %v", err)
	}
	p = &MissionProfile{Name: "x", ShadowDriftTokens: 0}
	if err := p.Validate(); err != nil {
		t.Errorf("zero shadow_drift_tokens should be valid (means 'use default'), got %v", err)
	}
	p = &MissionProfile{Name: "x", ShadowDriftTokens: 25}
	if err := p.Validate(); err != nil {
		t.Errorf("positive shadow_drift_tokens should be valid, got %v", err)
	}
}

func TestValidate_EnvKeys(t *testing.T) {
	p := &MissionProfile{Name: "x", Env: map[string]string{"": "v"}}
	if err := p.Validate(); err == nil {
		t.Error("empty env key: want error, got nil")
	}
	p = &MissionProfile{Name: "x", Env: map[string]string{"FOO=BAR": "v"}}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "'='") {
		t.Errorf("env key with '=': want format error, got %v", err)
	}
}

func TestLoadDir_SkipsMissing(t *testing.T) {
	out, errs := LoadDir(filepath.Join(t.TempDir(), "nope"))
	if out != nil || errs != nil {
		t.Errorf("missing dir should be silent, got out=%v errs=%v", out, errs)
	}
}

func TestLoadDir_SkipsNonYAML(t *testing.T) {
	dir := t.TempDir()
	writeProfile(t, dir, "good.yaml", "name: a\n")
	writeProfile(t, dir, "readme.md", "# nope\n")
	writeProfile(t, dir, "stub.txt", "ignored\n")
	out, errs := LoadDir(dir)
	if len(errs) != 0 {
		t.Errorf("unexpected errs: %v", errs)
	}
	if len(out) != 1 || out[0].Name != "a" {
		t.Errorf("expected one profile named 'a', got %+v", out)
	}
}

func TestLoadAll_OverrideChain(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()

	globalDir := GlobalProfilesDir(home)
	projectDir := ProjectProfilesDir(project)

	// Global "dev" — should be shadowed by the project copy.
	writeProfile(t, globalDir, "dev.yaml", `
name: dev
description: global dev
`)
	// Global-only profile — survives unchanged.
	writeProfile(t, globalDir, "audit.yaml", `
name: audit
description: global audit
`)
	// Project "dev" — wins over the global one.
	writeProfile(t, projectDir, "dev.yaml", `
name: dev
description: project dev
`)
	// Project-only profile.
	writeProfile(t, projectDir, "ops.yaml", `
name: ops
description: project ops
`)

	profiles, errs := LoadAll(home, project)
	if len(errs) != 0 {
		t.Fatalf("LoadAll errors: %v", errs)
	}

	got := map[string]string{}
	for _, p := range profiles {
		got[p.Name] = p.Description
	}

	// Expect: embedded default + audit (global) + dev (project) + ops (project).
	if len(got) != 4 {
		t.Errorf("expected 4 profiles, got %d: %v", len(got), got)
	}
	if _, ok := got[DefaultProfileName]; !ok {
		t.Errorf("missing embedded default; got: %v", got)
	}
	if got["dev"] != "project dev" {
		t.Errorf("dev: project-local should win, got %q", got["dev"])
	}
	if got["audit"] != "global audit" {
		t.Errorf("audit: global should survive, got %q", got["audit"])
	}
	if got["ops"] != "project ops" {
		t.Errorf("ops: project-only should be present, got %q", got["ops"])
	}

	// Verify Source paths are populated correctly for the YAML-loaded ones.
	dev, _ := Find(profiles, "dev")
	if !strings.HasPrefix(dev.Source, projectDir) {
		t.Errorf("dev.Source should live under project dir, got %q (project=%s)", dev.Source, projectDir)
	}
	audit, _ := Find(profiles, "audit")
	if !strings.HasPrefix(audit.Source, globalDir) {
		t.Errorf("audit.Source should live under global dir, got %q (global=%s)", audit.Source, globalDir)
	}
	def, _ := Find(profiles, DefaultProfileName)
	if def.Source != EmbeddedSource {
		t.Errorf("default.Source = %q, want %q", def.Source, EmbeddedSource)
	}
}

func TestLoadAll_OverrideIsWholesaleReplacement(t *testing.T) {
	// The doc comment on LoadAll promises that override is wholesale —
	// a project profile with fewer fields than the global does NOT
	// inherit anything from the global. This test pins that contract
	// across diverse fields (personas, tool lists, env, policies).
	home := t.TempDir()
	project := t.TempDir()

	writeProfile(t, GlobalProfilesDir(home), "dev.yaml", `
name: dev
description: global dev
personas:
  - architect
  - engineer
allowed_tools:
  - Read
  - Edit
denied_tools:
  - Bash(curl *)
env:
  GO111MODULE: "on"
  ONLY_GLOBAL: "1"
policies:
  hitl_severity: high
  token_budget: 50000
`)
	writeProfile(t, ProjectProfilesDir(project), "dev.yaml", `
name: dev
description: project dev
personas:
  - reviewer
`)

	profiles, errs := LoadAll(home, project)
	if len(errs) != 0 {
		t.Fatalf("LoadAll errors: %v", errs)
	}
	dev, err := Find(profiles, "dev")
	if err != nil {
		t.Fatalf("Find dev: %v", err)
	}

	if dev.Description != "project dev" {
		t.Errorf("description: want %q, got %q", "project dev", dev.Description)
	}
	if len(dev.Personas) != 1 || dev.Personas[0] != "reviewer" {
		t.Errorf("personas: project should replace global, got %v", dev.Personas)
	}
	if len(dev.AllowedTools) != 0 {
		t.Errorf("allowed_tools: project omitted them, should be empty, got %v", dev.AllowedTools)
	}
	if len(dev.DeniedTools) != 0 {
		t.Errorf("denied_tools: project omitted them, should be empty, got %v", dev.DeniedTools)
	}
	if _, ok := dev.Env["GO111MODULE"]; ok {
		t.Errorf("env: project omitted env, no global keys should leak, got %v", dev.Env)
	}
	if _, ok := dev.Env["ONLY_GLOBAL"]; ok {
		t.Errorf("env: ONLY_GLOBAL must not leak from global, got %v", dev.Env)
	}
	if dev.Policies.HITLSeverity != "" {
		t.Errorf("policies.hitl_severity: project omitted, want empty, got %q", dev.Policies.HITLSeverity)
	}
	if dev.Policies.TokenBudget != 0 {
		t.Errorf("policies.token_budget: project omitted, want 0, got %d", dev.Policies.TokenBudget)
	}
}

func TestLoadAll_UserCanOverrideDefault(t *testing.T) {
	home := t.TempDir()
	writeProfile(t, GlobalProfilesDir(home), "default.yaml", `
name: default
description: user-overridden default
`)
	profiles, errs := LoadAll(home, "")
	if len(errs) != 0 {
		t.Fatalf("LoadAll errors: %v", errs)
	}
	def, err := Find(profiles, DefaultProfileName)
	if err != nil {
		t.Fatalf("Find default: %v", err)
	}
	if def.Description != "user-overridden default" {
		t.Errorf("user override ignored, got description=%q source=%q", def.Description, def.Source)
	}
	if def.Source == EmbeddedSource {
		t.Errorf("source should not be %q after override", EmbeddedSource)
	}
}

func TestLoadAll_NoProjectRoot(t *testing.T) {
	home := t.TempDir()
	writeProfile(t, GlobalProfilesDir(home), "audit.yaml", "name: audit\n")
	profiles, errs := LoadAll(home, "")
	if len(errs) != 0 {
		t.Fatalf("errs: %v", errs)
	}
	// Should include embedded default + audit.
	if _, err := Find(profiles, DefaultProfileName); err != nil {
		t.Errorf("default missing: %v", err)
	}
	if _, err := Find(profiles, "audit"); err != nil {
		t.Errorf("audit missing: %v", err)
	}
}

func TestLoadAll_BadProfileReportedButOthersLoad(t *testing.T) {
	home := t.TempDir()
	writeProfile(t, GlobalProfilesDir(home), "good.yaml", "name: good\n")
	// Validation failure: empty name.
	writeProfile(t, GlobalProfilesDir(home), "bad.yaml", "description: nameless\n")

	profiles, errs := LoadAll(home, "")
	if len(errs) != 1 {
		t.Fatalf("expected one error, got %d: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "bad.yaml") {
		t.Errorf("error should reference bad.yaml, got %v", errs[0])
	}
	if _, err := Find(profiles, "good"); err != nil {
		t.Errorf("good profile should still load: %v", err)
	}
}

func TestLoad_MCPServers(t *testing.T) {
	dir := t.TempDir()
	path := writeProfile(t, dir, "ops.yaml", `
name: ops
description: ops mode
mcp_servers:
  - name: gmail
    command: npx
    args: ["-y", "@modelcontextprotocol/server-gmail"]
    env:
      GMAIL_OAUTH_PATH: /tmp/oauth.json
  - name: calendar
    command: uvx
    args: ["mcp-server-calendar"]
`)
	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.MCPServers) != 2 {
		t.Fatalf("mcp_servers count = %d, want 2", len(p.MCPServers))
	}
	g := p.MCPServers[0]
	if g.Name != "gmail" || g.Command != "npx" {
		t.Errorf("gmail = %+v", g)
	}
	if len(g.Args) != 2 || g.Args[1] != "@modelcontextprotocol/server-gmail" {
		t.Errorf("gmail args = %v", g.Args)
	}
	if g.Env["GMAIL_OAUTH_PATH"] != "/tmp/oauth.json" {
		t.Errorf("gmail env = %v", g.Env)
	}
	if p.MCPServers[1].Name != "calendar" {
		t.Errorf("second server = %+v", p.MCPServers[1])
	}
}

func TestValidate_MCPServerNameRequired(t *testing.T) {
	p := &MissionProfile{
		Name:       "x",
		MCPServers: []ProfileMCPServer{{Name: "", Command: "npx"}},
	}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Errorf("want name-required error, got %v", err)
	}
}

func TestValidate_MCPServerCommandRequired(t *testing.T) {
	p := &MissionProfile{
		Name:       "x",
		MCPServers: []ProfileMCPServer{{Name: "gmail"}},
	}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "command is required") {
		t.Errorf("want command-required error, got %v", err)
	}
}

func TestValidate_MCPServerDuplicateName(t *testing.T) {
	p := &MissionProfile{
		Name: "x",
		MCPServers: []ProfileMCPServer{
			{Name: "gmail", Command: "npx"},
			{Name: "gmail", Command: "uvx"},
		},
	}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("want duplicate error, got %v", err)
	}
}

func TestFind_UnknownNameReportsAvailable(t *testing.T) {
	profiles := []*MissionProfile{{Name: "a"}, {Name: "b"}}
	_, err := Find(profiles, "z")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "a, b") {
		t.Errorf("error should list available names, got: %v", err)
	}
}
