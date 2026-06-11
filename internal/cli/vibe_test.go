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

// setupVibeProject mirrors setupCtxProject but creates only the minimum
// project layout `uta vibe` needs (project root + .uta/project.yaml).
func setupVibeProject(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("UTA_HOME", "")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := config.SaveProject(root, &config.Project{Name: "vibe-test"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return root
}

func runVibe(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newVibeCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

// writeProjectProfile drops a minimal valid MissionProfile YAML at
// <root>/.uta/profiles/<name>.yaml so warnIfUnknownMode treats <name> as a
// known mode.
func writeProjectProfile(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, ".uta", "profiles")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir profiles: %v", err)
	}
	body := "name: " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
}

func TestVibe_AppendWritesFile(t *testing.T) {
	root := setupVibeProject(t)
	writeProjectProfile(t, root, "audit")

	_, stderr, err := runVibe(t, "audit", "be more aggressive")
	if err != nil {
		t.Fatalf("vibe append: %v", err)
	}
	path := filepath.Join(root, ".uta", "profiles", "audit.vibe.md")
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("read vibe file: %v", rerr)
	}
	if !strings.Contains(string(data), "be more aggressive") {
		t.Errorf("vibe file missing directive: %q", data)
	}
	if strings.Contains(stderr, "warn: no profile named") {
		t.Errorf("unexpected unknown-mode warning for known mode: %q", stderr)
	}
}

func TestVibe_WarnsOnUnknownMode(t *testing.T) {
	setupVibeProject(t)
	// No profile written — `audity` is a typo for the (absent) `audit` mode.
	_, stderr, err := runVibe(t, "audity", "be aggressive")
	if err != nil {
		t.Fatalf("vibe on unknown mode should not error, got: %v", err)
	}
	if !strings.Contains(stderr, "warn: no profile named \"audity\"") {
		t.Errorf("expected unknown-mode warning, got stderr: %q", stderr)
	}
}

func TestVibe_KnownModeNoWarning(t *testing.T) {
	root := setupVibeProject(t)
	writeProjectProfile(t, root, "audit")

	_, stderr, err := runVibe(t, "audit", "be aggressive")
	if err != nil {
		t.Fatalf("vibe: %v", err)
	}
	if strings.Contains(stderr, "warn: no profile named") {
		t.Errorf("known mode should not warn, got: %q", stderr)
	}
}

func TestVibe_DefaultModeIsAlwaysKnown(t *testing.T) {
	// The embedded "default" profile must always be recognized — no warning
	// even if the user has not authored any YAML.
	setupVibeProject(t)
	_, stderr, err := runVibe(t, "default", "be aggressive")
	if err != nil {
		t.Fatalf("vibe default: %v", err)
	}
	if strings.Contains(stderr, "warn: no profile named") {
		t.Errorf("default mode should be known, got stderr: %q", stderr)
	}
}

func TestVibe_ClearRemovesFileEntirely(t *testing.T) {
	root := setupVibeProject(t)
	writeProjectProfile(t, root, "audit")
	if _, _, err := runVibe(t, "audit", "x"); err != nil {
		t.Fatalf("seed vibe: %v", err)
	}
	path := filepath.Join(root, ".uta", "profiles", "audit.vibe.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("vibe file should exist before clear: %v", err)
	}
	if _, _, err := runVibe(t, "audit", "--clear"); err != nil {
		t.Fatalf("vibe --clear: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("vibe file should be removed entirely (not truncated), stat err = %v", err)
	}
}

func TestVibe_ShowOnEmptyEmitsHint(t *testing.T) {
	root := setupVibeProject(t)
	writeProjectProfile(t, root, "audit")
	_, stderr, err := runVibe(t, "audit", "--show")
	if err != nil {
		t.Fatalf("vibe --show: %v", err)
	}
	if !strings.Contains(stderr, "no vibe set") {
		t.Errorf("--show on empty should print hint, got stderr: %q", stderr)
	}
}

func TestVibe_ShowPrintsExistingBody(t *testing.T) {
	root := setupVibeProject(t)
	writeProjectProfile(t, root, "audit")
	if _, _, err := runVibe(t, "audit", "be more aggressive"); err != nil {
		t.Fatalf("seed vibe: %v", err)
	}
	stdout, _, err := runVibe(t, "audit", "--show")
	if err != nil {
		t.Fatalf("vibe --show: %v", err)
	}
	if !strings.Contains(stdout, "be more aggressive") {
		t.Errorf("--show should print the stored directive, got stdout: %q", stdout)
	}
	if !strings.Contains(stdout, "## ") {
		t.Errorf("--show should print the entry heading, got stdout: %q", stdout)
	}
}

func TestVibe_ShowAndClearMutuallyExclusive(t *testing.T) {
	setupVibeProject(t)
	_, _, err := runVibe(t, "audit", "--show", "--clear")
	if err == nil {
		t.Fatal("expected error when --show and --clear are passed together, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention mutual exclusion, got: %v", err)
	}
}

func TestVibe_MultiLineDirectivePreservedThroughCLI(t *testing.T) {
	root := setupVibeProject(t)
	writeProjectProfile(t, root, "audit")
	directive := "be aggressive\nflag style nits too\n- bullet one"
	if _, _, err := runVibe(t, "audit", directive); err != nil {
		t.Fatalf("append multi-line: %v", err)
	}
	if _, _, err := runVibe(t, "audit", "follow-up"); err != nil {
		t.Fatalf("append follow-up: %v", err)
	}
	data, rerr := os.ReadFile(filepath.Join(root, ".uta", "profiles", "audit.vibe.md"))
	if rerr != nil {
		t.Fatalf("read vibe file: %v", rerr)
	}
	body := string(data)
	for _, want := range []string{"be aggressive", "flag style nits too", "- bullet one", "follow-up"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %q", want, body)
		}
	}
	// Two entries → exactly two headings — multi-line body must not bleed into the second entry's heading.
	if got := strings.Count(body, "## "); got != 2 {
		t.Errorf("expected exactly 2 entry headings, got %d: %q", got, body)
	}
}
