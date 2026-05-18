package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTempWorkflow writes the YAML to a temp file and returns its path.
func writeTempWorkflow(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "uta.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp workflow: %v", err)
	}
	return path
}

func TestLoadWorkflow_Minimal(t *testing.T) {
	path := writeTempWorkflow(t, `
version: 2
goal: do the thing
defaults:
  worker: claude
`)
	wf, err := LoadWorkflow(path)
	if err != nil {
		t.Fatalf("LoadWorkflow: %v", err)
	}
	if wf.Version != 2 {
		t.Errorf("version: got %d, want 2", wf.Version)
	}
	if wf.Goal != "do the thing" {
		t.Errorf("goal: got %q", wf.Goal)
	}
	if wf.Defaults.Worker != "claude" {
		t.Errorf("defaults.worker: got %q", wf.Defaults.Worker)
	}
	// Absent fields stay at zero values — LoadWorkflow does not inject
	// defaults itself.
	if wf.Defaults.Planner != "" {
		t.Errorf("defaults.planner: want zero value, got %q", wf.Defaults.Planner)
	}
	if wf.Defaults.SubtaskTimeout != 0 {
		t.Errorf("defaults.subtask_timeout: want zero, got %v", wf.Defaults.SubtaskTimeout)
	}
	if wf.Defaults.Timeout != 0 {
		t.Errorf("defaults.timeout: want zero, got %v", wf.Defaults.Timeout)
	}
}

func TestLoadWorkflow_FullDefaults(t *testing.T) {
	path := writeTempWorkflow(t, `
version: 2
goal: deep work
defaults:
  worker: claude
  planner: gemini
  max_parallel: 8
  max_subtasks: 12
  subtask_timeout: 5m
  timeout: 1h
  workdir: ./sub
  pre_approve:
    - go test ./...
    - go build ./...
synthesis:
  worker: gemini
  mode: merge
budget:
  max_wall_seconds: 600
`)
	wf, err := LoadWorkflow(path)
	if err != nil {
		t.Fatalf("LoadWorkflow: %v", err)
	}
	d := wf.Defaults
	if d.Planner != "gemini" {
		t.Errorf("planner: %q", d.Planner)
	}
	if d.MaxParallel != 8 {
		t.Errorf("max_parallel: %d", d.MaxParallel)
	}
	if d.MaxSubtasks != 12 {
		t.Errorf("max_subtasks: %d", d.MaxSubtasks)
	}
	if d.SubtaskTimeout != 5*time.Minute {
		t.Errorf("subtask_timeout: %v", d.SubtaskTimeout)
	}
	if d.Timeout != time.Hour {
		t.Errorf("timeout: %v", d.Timeout)
	}
	if d.Workdir != "./sub" {
		t.Errorf("workdir: %q", d.Workdir)
	}
	if len(d.PreApprove) != 2 || d.PreApprove[0] != "go test ./..." {
		t.Errorf("pre_approve: %v", d.PreApprove)
	}
	if wf.Synthesis.Worker != "gemini" || wf.Synthesis.Mode != "merge" {
		t.Errorf("synthesis: %+v", wf.Synthesis)
	}
	if wf.Budget.MaxWallSeconds != 600 {
		t.Errorf("budget.max_wall_seconds: %d", wf.Budget.MaxWallSeconds)
	}
}

func TestLoadWorkflow_GateShorthandString(t *testing.T) {
	path := writeTempWorkflow(t, `
version: 2
goal: build it
defaults:
  worker: claude
subtasks:
  - id: build
    prompt: implement contracts
    gate: forge build && forge test
`)
	wf, err := LoadWorkflow(path)
	if err != nil {
		t.Fatalf("LoadWorkflow: %v", err)
	}
	if len(wf.Subtasks) != 1 {
		t.Fatalf("subtasks: got %d", len(wf.Subtasks))
	}
	g := wf.Subtasks[0].Gate
	if g == nil {
		t.Fatal("gate: want non-nil for shorthand string")
	}
	if g.Cmd != "forge build && forge test" {
		t.Errorf("gate.cmd: %q", g.Cmd)
	}
	if g.Timeout != 0 || g.RetryProducer || g.MaxRetries != 0 {
		t.Errorf("gate: shorthand should leave other fields zero, got %+v", *g)
	}
}

func TestLoadWorkflow_GateFullObject(t *testing.T) {
	path := writeTempWorkflow(t, `
version: 2
goal: build it
defaults:
  worker: claude
subtasks:
  - id: frontend
    prompt: build the app
    gate:
      cmd: npm run typecheck && npm run build
      timeout: 10m
      retry_producer: true
      max_retries: 2
`)
	wf, err := LoadWorkflow(path)
	if err != nil {
		t.Fatalf("LoadWorkflow: %v", err)
	}
	g := wf.Subtasks[0].Gate
	if g == nil {
		t.Fatal("gate: want non-nil")
	}
	if g.Cmd != "npm run typecheck && npm run build" {
		t.Errorf("gate.cmd: %q", g.Cmd)
	}
	if g.Timeout != 10*time.Minute {
		t.Errorf("gate.timeout: %v", g.Timeout)
	}
	if !g.RetryProducer {
		t.Errorf("gate.retry_producer: want true")
	}
	if g.MaxRetries != 2 {
		t.Errorf("gate.max_retries: %d", g.MaxRetries)
	}
}

func TestLoadWorkflow_GateAbsentLeavesNil(t *testing.T) {
	path := writeTempWorkflow(t, `
version: 2
goal: g
defaults:
  worker: claude
subtasks:
  - id: t1
    prompt: do it
`)
	wf, err := LoadWorkflow(path)
	if err != nil {
		t.Fatalf("LoadWorkflow: %v", err)
	}
	if wf.Subtasks[0].Gate != nil {
		t.Errorf("gate: want nil when absent, got %+v", *wf.Subtasks[0].Gate)
	}
}

func TestLoadWorkflow_FileNotFound(t *testing.T) {
	_, err := LoadWorkflow(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "read workflow") {
		t.Errorf("expected 'read workflow' wrap, got: %v", err)
	}
}

func TestLoadWorkflow_BadYAML(t *testing.T) {
	path := writeTempWorkflow(t, "::not valid yaml:::\n  - [\n")
	_, err := LoadWorkflow(path)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "parse workflow") {
		t.Errorf("expected 'parse workflow' wrap, got: %v", err)
	}
}

func TestLoadWorkflow_ValidationFailureWraps(t *testing.T) {
	path := writeTempWorkflow(t, `
version: 2
goal: ""
defaults:
  worker: claude
`)
	_, err := LoadWorkflow(path)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "workflow ") {
		t.Errorf("expected 'workflow' wrap, got: %v", err)
	}
	if !strings.Contains(err.Error(), "goal is required") {
		t.Errorf("expected 'goal is required' inside wrapped error, got: %v", err)
	}
}

func TestValidate_VersionRange(t *testing.T) {
	cases := []struct {
		version int
		wantErr bool
	}{
		{0, false}, // unset is allowed
		{1, false},
		{2, false},
		{3, true},
		{99, true},
	}
	for _, c := range cases {
		wf := &Workflow{
			Version:  c.version,
			Goal:     "g",
			Defaults: WorkflowDefaults{Worker: "claude"},
		}
		err := wf.Validate()
		if c.wantErr && err == nil {
			t.Errorf("version=%d: want error, got nil", c.version)
		}
		if !c.wantErr && err != nil {
			t.Errorf("version=%d: unexpected error: %v", c.version, err)
		}
	}
}

func TestValidate_GoalRequired(t *testing.T) {
	wf := &Workflow{Defaults: WorkflowDefaults{Worker: "claude"}}
	err := wf.Validate()
	if err == nil || !strings.Contains(err.Error(), "goal is required") {
		t.Errorf("expected 'goal is required', got: %v", err)
	}
	// Whitespace-only goal is also rejected.
	wf.Goal = "   \n\t  "
	if err := wf.Validate(); err == nil || !strings.Contains(err.Error(), "goal is required") {
		t.Errorf("whitespace goal: expected 'goal is required', got: %v", err)
	}
}

func TestValidate_DefaultsWorkerRequired(t *testing.T) {
	wf := &Workflow{Goal: "g"}
	err := wf.Validate()
	if err == nil || !strings.Contains(err.Error(), "defaults.worker is required") {
		t.Errorf("expected 'defaults.worker is required', got: %v", err)
	}
}

func TestValidate_StrategyValues(t *testing.T) {
	cases := []struct {
		strategy string
		wantErr  bool
	}{
		{"", false},
		{"fanout", false},
		{"dag", false},
		{"sequential", true},
		{"DAG", true}, // case-sensitive
	}
	for _, c := range cases {
		wf := &Workflow{
			Goal:     "g",
			Defaults: WorkflowDefaults{Worker: "claude"},
			Strategy: c.strategy,
		}
		err := wf.Validate()
		if c.wantErr && err == nil {
			t.Errorf("strategy=%q: want error, got nil", c.strategy)
		}
		if !c.wantErr && err != nil {
			t.Errorf("strategy=%q: unexpected error: %v", c.strategy, err)
		}
	}
}

func TestValidate_SubtaskPromptRequired(t *testing.T) {
	wf := &Workflow{
		Goal:     "g",
		Defaults: WorkflowDefaults{Worker: "claude"},
		Subtasks: []WorkflowSubtask{{ID: "a", Prompt: ""}},
	}
	err := wf.Validate()
	if err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Errorf("expected 'prompt is required', got: %v", err)
	}
}

func TestValidate_DuplicateSubtaskID(t *testing.T) {
	wf := &Workflow{
		Goal:     "g",
		Defaults: WorkflowDefaults{Worker: "claude"},
		Subtasks: []WorkflowSubtask{
			{ID: "x", Prompt: "p1"},
			{ID: "x", Prompt: "p2"},
		},
	}
	err := wf.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate id") {
		t.Errorf("expected duplicate-id error, got: %v", err)
	}
}

func TestValidate_GateCmdRequiredWhenGateSet(t *testing.T) {
	wf := &Workflow{
		Goal:     "g",
		Defaults: WorkflowDefaults{Worker: "claude"},
		Subtasks: []WorkflowSubtask{{
			ID:     "a",
			Prompt: "p",
			Gate:   &WorkflowGate{Cmd: ""},
		}},
	}
	err := wf.Validate()
	if err == nil || !strings.Contains(err.Error(), "gate.cmd is required") {
		t.Errorf("expected 'gate.cmd is required', got: %v", err)
	}
}

func TestValidate_NeedsMustExist(t *testing.T) {
	wf := &Workflow{
		Goal:     "g",
		Defaults: WorkflowDefaults{Worker: "claude"},
		Subtasks: []WorkflowSubtask{
			{ID: "a", Prompt: "p", Needs: []string{"ghost"}},
		},
	}
	err := wf.Validate()
	if err == nil || !strings.Contains(err.Error(), `needs "ghost"`) {
		t.Errorf("expected dangling-needs error, got: %v", err)
	}
}

func TestValidate_NeedsForwardReferenceOK(t *testing.T) {
	// Validate scans all IDs first, then checks needs — a forward reference
	// (subtask listed before its dependency) is fine.
	wf := &Workflow{
		Goal:     "g",
		Defaults: WorkflowDefaults{Worker: "claude"},
		Subtasks: []WorkflowSubtask{
			{ID: "a", Prompt: "p", Needs: []string{"b"}},
			{ID: "b", Prompt: "p"},
		},
	}
	if err := wf.Validate(); err != nil {
		t.Errorf("forward-reference needs should be ok, got: %v", err)
	}
}

func TestInferStrategy_Explicit(t *testing.T) {
	wf := &Workflow{Strategy: "dag"}
	if got := wf.InferStrategy(); got != "dag" {
		t.Errorf("got %q, want dag", got)
	}
	wf.Strategy = "fanout"
	if got := wf.InferStrategy(); got != "fanout" {
		t.Errorf("got %q, want fanout", got)
	}
}

func TestInferStrategy_AutoDagFromNeeds(t *testing.T) {
	wf := &Workflow{Subtasks: []WorkflowSubtask{
		{ID: "a", Prompt: "p"},
		{ID: "b", Prompt: "p", Needs: []string{"a"}},
	}}
	if got := wf.InferStrategy(); got != "dag" {
		t.Errorf("got %q, want dag", got)
	}
}

func TestInferStrategy_AutoDagFromGate(t *testing.T) {
	wf := &Workflow{Subtasks: []WorkflowSubtask{
		{ID: "a", Prompt: "p", Gate: &WorkflowGate{Cmd: "make"}},
	}}
	if got := wf.InferStrategy(); got != "dag" {
		t.Errorf("got %q, want dag", got)
	}
}

func TestInferStrategy_AutoFanout(t *testing.T) {
	wf := &Workflow{Subtasks: []WorkflowSubtask{
		{ID: "a", Prompt: "p"},
		{ID: "b", Prompt: "p"},
	}}
	if got := wf.InferStrategy(); got != "fanout" {
		t.Errorf("got %q, want fanout", got)
	}
}

func TestExampleWorkflowYAML_LoadsCleanly(t *testing.T) {
	path := writeTempWorkflow(t, ExampleWorkflowYAML)
	wf, err := LoadWorkflow(path)
	if err != nil {
		t.Fatalf("ExampleWorkflowYAML should load, got: %v", err)
	}
	if wf.Version != 2 {
		t.Errorf("version: %d", wf.Version)
	}
	if wf.Defaults.Worker != "claude" {
		t.Errorf("defaults.worker: %q", wf.Defaults.Worker)
	}
}
