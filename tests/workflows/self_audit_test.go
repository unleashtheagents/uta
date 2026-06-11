// Package workflows holds smoke tests for the example workflow YAMLs
// under examples/workflows/. The tests parse each YAML, validate its
// shape, and assert the structural invariants the workflow promises in
// its acceptance criteria — without invoking any provider. Think of
// these as the dry-run gate: a workflow that fails to parse here would
// fail at `uta run -f ...` before even reaching the planner.
package workflows

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/config"
)

// repoRoot walks up from this test file until it finds go.mod so the
// test can locate examples/workflows/ regardless of where `go test` is
// invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("walked to filesystem root without finding go.mod (started near %s)", thisFile)
		}
		dir = parent
	}
}

// TestSelfAuditWorkflow_DryRun is the smoke test for the self-audit
// workflow. It is the unit-test analogue of running
//
//	uta run -f examples/workflows/self-audit.yaml --mode audit
//
// in dry-run mode: we load the YAML, validate its shape, and assert the
// structural promises in its header (three auditor critics, a synthesis
// step that writes ROADMAP-V2.md). Real execution needs a live provider
// and is not appropriate for `go test ./...`.
func TestSelfAuditWorkflow_DryRun(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "examples", "workflows", "self-audit.yaml")

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("self-audit workflow not found at %s: %v", path, err)
	}

	wf, err := config.LoadWorkflow(path)
	if err != nil {
		t.Fatalf("LoadWorkflow(%s): %v", path, err)
	}

	if wf.Version != 2 {
		t.Errorf("version: got %d, want 2", wf.Version)
	}
	if strings.TrimSpace(wf.Goal) == "" {
		t.Error("goal: must be non-empty")
	}
	if wf.Defaults.Worker == "" {
		t.Error("defaults.worker: must be set")
	}
	if got := wf.InferStrategy(); got != "dag" {
		t.Errorf("strategy: got %q, want dag (the audit chains critics into a synth step)", got)
	}

	// The workflow's acceptance criteria require: three auditor critics
	// (architecture, completeness, regression-risk) and a synthesis step
	// that writes ROADMAP-V2.md.
	byID := map[string]config.WorkflowSubtask{}
	for _, st := range wf.Subtasks {
		byID[st.ID] = st
	}
	for _, required := range []string{
		"critic-architecture",
		"critic-completeness",
		"critic-regression",
		"roadmap",
	} {
		if _, ok := byID[required]; !ok {
			t.Errorf("missing required subtask %q (have: %v)", required, subtaskIDs(wf.Subtasks))
		}
	}

	// The roadmap synthesizer must reference ROADMAP-V2.md in its prompt
	// — that's the artifact the acceptance criteria promise.
	roadmap, ok := byID["roadmap"]
	if !ok {
		t.Fatal("roadmap subtask missing; cannot assert ROADMAP-V2.md output")
	}
	if !strings.Contains(roadmap.Prompt, "ROADMAP-V2.md") {
		t.Errorf("roadmap subtask prompt must mention ROADMAP-V2.md so the synthesizer knows what to write; got: %s", roadmap.Prompt)
	}
	if !strings.Contains(roadmap.Prompt, "20") {
		t.Error("roadmap subtask prompt must specify 20 proposed iterations")
	}
	if len(roadmap.Needs) == 0 {
		t.Error("roadmap subtask must declare 'needs' on the critic subtasks (it's the synth step)")
	}

	// The cross-mode-gap promise from the idea: the architecture critic
	// must be instructed to surface a gap that no mode has implemented.
	arch := byID["critic-architecture"]
	if !strings.Contains(arch.Prompt, "cross-mode") && !strings.Contains(arch.Prompt, "no mode has implemented") {
		t.Errorf("critic-architecture prompt must require a cross-mode gap finding; got: %s", arch.Prompt)
	}
}

func subtaskIDs(sub []config.WorkflowSubtask) []string {
	out := make([]string, len(sub))
	for i, st := range sub {
		out[i] = st.ID
	}
	return out
}

// TestSelfAuditWorkflow_E2E actually invokes the workflow against a live
// provider and asserts that ROADMAP-V2.md gets written with at least one
// numbered bullet. Gated behind UTA_E2E_PROVIDERS=1 because it costs real
// tokens and depends on a provider CLI being installed on PATH.
//
// Optional env knobs:
//
//	UTA_E2E_PROVIDERS=1        required to opt in
//	UTA_E2E_WORKER=<name>      provider to use (default: claude)
//	UTA_E2E_TIMEOUT=<duration> overall wall clock (default: 20m)
//
// The test creates a throwaway uta project in a temp dir so the worker's
// $UTA_PROJECT_ROOT resolves there and ROADMAP-V2.md lands in that
// sandbox, not in the repo tree.
func TestSelfAuditWorkflow_E2E(t *testing.T) {
	if os.Getenv("UTA_E2E_PROVIDERS") != "1" {
		t.Skip("set UTA_E2E_PROVIDERS=1 to run the live-provider self-audit e2e (costs tokens)")
	}

	worker := os.Getenv("UTA_E2E_WORKER")
	if worker == "" {
		worker = "claude"
	}
	timeout := 20 * time.Minute
	if v := os.Getenv("UTA_E2E_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("UTA_E2E_TIMEOUT=%q: %v", v, err)
		}
		timeout = d
	}

	root := repoRoot(t)
	workflow := filepath.Join(root, "examples", "workflows", "self-audit.yaml")
	if _, err := os.Stat(workflow); err != nil {
		t.Fatalf("workflow not found: %v", err)
	}

	// Sandbox project so ROADMAP-V2.md cannot pollute the repo tree.
	project := t.TempDir()

	utaBin := buildUtaBinary(t, root)

	// `uta project init` creates the .uta/ scaffold and a project.yaml,
	// which is what flips InProject() to true so the run loop exports
	// UTA_PROJECT_ROOT into subtask provider env.
	runUta(t, utaBin, project, 2*time.Minute, "project", "init", "self-audit-e2e")

	runUta(t, utaBin, project, timeout, "run", "-f", workflow, "--worker", worker, "-y")

	roadmap := filepath.Join(project, "ROADMAP-V2.md")
	data, err := os.ReadFile(roadmap)
	if err != nil {
		t.Fatalf("ROADMAP-V2.md not written to project root: %v", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		t.Fatal("ROADMAP-V2.md is empty")
	}
	// At-least-one-bullet bar: any numbered list item ("1.", "2.", ...).
	bullet := regexp.MustCompile(`(?m)^\s*\d+\.\s`)
	if !bullet.Match(data) {
		t.Fatalf("ROADMAP-V2.md has no numbered bullet; contents:\n%s", data)
	}
}

func buildUtaBinary(t *testing.T, repoRoot string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "uta")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/uta")
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/uta: %v\n%s", err, out)
	}
	return bin
}

func runUta(t *testing.T, bin, cwd string, timeout time.Duration, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("uta %s timed out after %s\n%s", strings.Join(args, " "), timeout, out)
	}
	if err != nil {
		t.Fatalf("uta %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}
