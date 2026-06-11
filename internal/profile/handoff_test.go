package profile

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestHandoffCondition_Matches_EmptyAlwaysMatches(t *testing.T) {
	var c HandoffCondition
	if !c.Matches(nil) {
		t.Error("empty condition should match an empty file list")
	}
	if !c.Matches([]string{"foo.go"}) {
		t.Error("empty condition should match any file list")
	}
}

func TestHandoffCondition_Matches_FilesChangedMin(t *testing.T) {
	c := HandoffCondition{FilesChangedMin: 2}
	if c.Matches(nil) {
		t.Error("min=2 should not match an empty list")
	}
	if c.Matches([]string{"a.go"}) {
		t.Error("min=2 should not match a single-file list")
	}
	if !c.Matches([]string{"a.go", "b.go"}) {
		t.Error("min=2 should match a two-file list")
	}
}

func TestHandoffCondition_Matches_ContainsAny(t *testing.T) {
	c := HandoffCondition{ContainsAny: []string{"*.sol", "*.go"}}
	if c.Matches([]string{"README.md"}) {
		t.Error("md file should not match *.sol/*.go")
	}
	if !c.Matches([]string{"contracts/Token.sol"}) {
		t.Error("Token.sol should match *.sol (basename)")
	}
	if !c.Matches([]string{"main.go"}) {
		t.Error("main.go should match *.go")
	}
	if !c.Matches([]string{"README.md", "go.mod", "main.go"}) {
		t.Error("at least one match is enough")
	}
}

func TestHandoffCondition_Matches_Combined(t *testing.T) {
	c := HandoffCondition{FilesChangedMin: 1, ContainsAny: []string{"*.sol"}}
	if c.Matches(nil) {
		t.Error("zero files fails min=1")
	}
	if c.Matches([]string{"main.go"}) {
		t.Error("min met but no *.sol — should not match")
	}
	if !c.Matches([]string{"Token.sol"}) {
		t.Error("min met AND *.sol — should match")
	}
}

func TestHandoff_Validate_TargetModeRequired(t *testing.T) {
	cases := []struct {
		label string
		mode  string
	}{
		{"empty", ""},
		{"whitespace", "  "},
	}
	for _, c := range cases {
		h := &Handoff{TargetMode: c.mode}
		if err := h.Validate(); err == nil {
			t.Errorf("%s: want validation error, got nil", c.label)
		}
	}
}

func TestHandoff_Validate_TargetModeNoPathSeparators(t *testing.T) {
	h := &Handoff{TargetMode: "foo/bar"}
	err := h.Validate()
	if err == nil || !strings.Contains(err.Error(), "path separators") {
		t.Errorf("want path-separators error, got %v", err)
	}
}

func TestHandoff_Validate_NegativeFilesChangedMin(t *testing.T) {
	h := &Handoff{TargetMode: "audit", Condition: HandoffCondition{FilesChangedMin: -1}}
	err := h.Validate()
	if err == nil || !strings.Contains(err.Error(), "files_changed_min") {
		t.Errorf("want files_changed_min error, got %v", err)
	}
}

func TestHandoff_Validate_EmptyContainsAnyPattern(t *testing.T) {
	h := &Handoff{TargetMode: "audit", Condition: HandoffCondition{ContainsAny: []string{""}}}
	err := h.Validate()
	if err == nil || !strings.Contains(err.Error(), "non-empty") {
		t.Errorf("want non-empty pattern error, got %v", err)
	}
}

func TestHandoff_RenderPrompt_Substitution(t *testing.T) {
	h := &Handoff{
		TargetMode:     "audit",
		PromptTemplate: "Audit the diff from session {{prior_session_id}}.",
	}
	got := h.RenderPrompt("sess-123")
	want := "Audit the diff from session sess-123."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestHandoff_RenderPrompt_EmptyTemplateFallback(t *testing.T) {
	h := &Handoff{TargetMode: "audit"}
	got := h.RenderPrompt("sess-abc")
	if !strings.Contains(got, "sess-abc") {
		t.Errorf("fallback prompt should include the session id, got %q", got)
	}
}

func TestLoad_ProfileWithOnComplete(t *testing.T) {
	dir := t.TempDir()
	path := writeProfile(t, dir, "dev.yaml", `
name: dev
description: development
on_complete:
  - target_mode: audit
    condition:
      files_changed_min: 1
      contains_any:
        - "*.sol"
        - "*.go"
    prompt: "Audit session {{prior_session_id}}."
  - target_mode: ops
`)
	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.OnComplete) != 2 {
		t.Fatalf("on_complete count = %d, want 2", len(p.OnComplete))
	}
	if p.OnComplete[0].TargetMode != "audit" {
		t.Errorf("first target_mode = %q", p.OnComplete[0].TargetMode)
	}
	if p.OnComplete[0].Condition.FilesChangedMin != 1 {
		t.Errorf("files_changed_min = %d", p.OnComplete[0].Condition.FilesChangedMin)
	}
	if len(p.OnComplete[0].Condition.ContainsAny) != 2 {
		t.Errorf("contains_any = %v", p.OnComplete[0].Condition.ContainsAny)
	}
	if p.OnComplete[1].TargetMode != "ops" {
		t.Errorf("second target_mode = %q", p.OnComplete[1].TargetMode)
	}
}

func TestLoad_ProfileWithBadHandoffReportsIndex(t *testing.T) {
	dir := t.TempDir()
	path := writeProfile(t, dir, "dev.yaml", `
name: dev
on_complete:
  - target_mode: audit
  - target_mode: ""
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load: want validation error, got nil")
	}
	if !strings.Contains(err.Error(), "on_complete[1]") {
		t.Errorf("error should pinpoint on_complete[1], got: %v", err)
	}
}

func TestValidate_MaxHandoffDepth_NonNegative(t *testing.T) {
	p := &MissionProfile{Name: "dev", MaxHandoffDepth: -1}
	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "max_handoff_depth") {
		t.Errorf("want max_handoff_depth error for negative value, got %v", err)
	}
}

func TestValidate_MaxHandoffDepth_HardLimit(t *testing.T) {
	p := &MissionProfile{Name: "dev", MaxHandoffDepth: MaxHandoffDepthHardLimit + 1}
	err := p.Validate()
	if err == nil || !strings.Contains(err.Error(), "max_handoff_depth") {
		t.Errorf("want max_handoff_depth error above hard limit, got %v", err)
	}
	// At-the-limit must be accepted.
	p.MaxHandoffDepth = MaxHandoffDepthHardLimit
	if err := p.Validate(); err != nil {
		t.Errorf("at-the-limit MaxHandoffDepth=%d should validate, got %v", MaxHandoffDepthHardLimit, err)
	}
}

func TestLoad_ProfileWithMaxHandoffDepth(t *testing.T) {
	dir := t.TempDir()
	path := writeProfile(t, dir, "dev.yaml", `
name: dev
max_handoff_depth: 6
on_complete:
  - target_mode: audit
`)
	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.MaxHandoffDepth != 6 {
		t.Errorf("MaxHandoffDepth = %d, want 6", p.MaxHandoffDepth)
	}
}

// TestDevExampleProfile_ParsesCleanly catches regressions in the bundled
// example file — if a future field rename forgets to update the example,
// this surfaces it instantly.
func TestDevExampleProfile_ParsesCleanly(t *testing.T) {
	// Walk up from this test file to the repo root, then locate the example.
	wd, err := filepath.Abs(".")
	if err != nil {
		t.Skipf("could not resolve working dir: %v", err)
	}
	// internal/profile is two levels under the repo root.
	root := filepath.Dir(filepath.Dir(wd))
	path := filepath.Join(root, "examples", "profiles", "dev.yaml")
	p, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%s): %v", path, err)
	}
	if p.Name != "dev" {
		t.Errorf("name = %q, want dev", p.Name)
	}
	// dev.yaml is the iter-2 example, intentionally minimal: it should
	// demonstrate the core --mode wiring (personas + allowed_tools +
	// policies) without dragging in later-iteration knobs. On_complete
	// coverage lives in TestLoad_ProfileWithOnComplete on inline YAML.
	if len(p.AllowedTools) == 0 {
		t.Error("dev.yaml example should declare allowed_tools to demonstrate the pre-approve restriction")
	}
	if len(p.Personas) == 0 {
		t.Error("dev.yaml example should declare personas to demonstrate persona attachment")
	}
}
