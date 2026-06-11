package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/config"
)

// runInit builds a fresh init command and executes it. Returns stdout,
// stderr, and the executor error. The caller is responsible for chdir'ing
// into the directory where --workflow should land uta.yaml.
func runInit(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newInitCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

// chdirTemp chdir's into a fresh temp directory and restores the prior cwd
// on cleanup. Returns the absolute path to the temp dir.
func chdirTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return dir
}

// TestInit_NoFlags_PrintsHints verifies the no-flag path: it prints the
// global uta home location, the providers dir hint, and the next-steps
// block — all sourced from paths.Home(), which honors $UTA_HOME.
func TestInit_NoFlags_PrintsHints(t *testing.T) {
	home := t.TempDir()
	t.Setenv("UTA_HOME", home)

	out, _, err := runInit(t)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(out, "uta home: "+home) {
		t.Errorf("output should announce uta home %q:\n%s", home, out)
	}
	if !strings.Contains(out, filepath.Join(home, "providers")) {
		t.Errorf("output should mention providers dir under %q:\n%s", home, out)
	}
	for _, want := range []string{"next steps:", "uta doctor", "uta init --workflow", "uta run"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestInit_Workflow_WritesExample verifies --workflow drops the canonical
// example uta.yaml in the cwd and announces the path on stdout.
func TestInit_Workflow_WritesExample(t *testing.T) {
	dir := chdirTemp(t)

	out, _, err := runInit(t, "--workflow")
	if err != nil {
		t.Fatalf("init --workflow: %v", err)
	}
	dest := filepath.Join(dir, "uta.yaml")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read uta.yaml: %v", err)
	}
	if string(got) != config.ExampleWorkflowYAML {
		t.Errorf("uta.yaml content does not match config.ExampleWorkflowYAML")
	}
	if !strings.Contains(out, "wrote uta.yaml") {
		t.Errorf("stdout should announce wrote uta.yaml:\n%s", out)
	}
}

// TestInit_Workflow_RefusesOverwrite verifies that --workflow without
// --force aborts when uta.yaml already exists, leaving the existing file
// untouched.
func TestInit_Workflow_RefusesOverwrite(t *testing.T) {
	dir := chdirTemp(t)

	existing := []byte("# pre-existing\n")
	dest := filepath.Join(dir, "uta.yaml")
	if err := os.WriteFile(dest, existing, 0o644); err != nil {
		t.Fatalf("seed uta.yaml: %v", err)
	}

	_, _, err := runInit(t, "--workflow")
	if err == nil {
		t.Fatal("init --workflow on existing file: want error, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v; want 'already exists'", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read uta.yaml: %v", err)
	}
	if !bytes.Equal(got, existing) {
		t.Errorf("existing uta.yaml was modified without --force:\n%s", got)
	}
}

// TestInit_Workflow_ForceOverwrites verifies --force lets --workflow
// replace an existing uta.yaml.
func TestInit_Workflow_ForceOverwrites(t *testing.T) {
	dir := chdirTemp(t)

	dest := filepath.Join(dir, "uta.yaml")
	if err := os.WriteFile(dest, []byte("# stale\n"), 0o644); err != nil {
		t.Fatalf("seed uta.yaml: %v", err)
	}

	if _, _, err := runInit(t, "--workflow", "--force"); err != nil {
		t.Fatalf("init --workflow --force: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read uta.yaml: %v", err)
	}
	if string(got) != config.ExampleWorkflowYAML {
		t.Errorf("uta.yaml not overwritten with example content")
	}
}
