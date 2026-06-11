package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupImproveHome isolates HOME/UTA_HOME so newApp() opens a fresh global
// store under our control, and chdir's into a non-project directory so the
// auto-detect walk doesn't find an unrelated project.
func setupImproveHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	utaHome := filepath.Join(home, ".uta")
	if err := os.MkdirAll(utaHome, 0o755); err != nil {
		t.Fatalf("mkdir uta home: %v", err)
	}
	t.Setenv("UTA_HOME", utaHome)

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
}

// runImprove builds a fresh improve command and executes it with args.
// SilenceUsage/SilenceErrors prevent cobra from dumping usage on RunE errors
// so the test gets just the underlying error to assert against.
func runImprove(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newImproveCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

func TestBudgetStr(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"zero-is-unlimited", 0, "unlimited"},
		{"sub-second", 500 * time.Millisecond, "500ms"},
		{"minutes", 5 * time.Minute, "5m0s"},
		{"hours", 2 * time.Hour, "2h0m0s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := budgetStr(tc.in); got != tc.want {
				t.Errorf("budgetStr(%v) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsTTY_NonFileReturnsFalse(t *testing.T) {
	// A bytes.Buffer is not an *os.File, so isTTY must short-circuit to false
	// rather than panicking on the type assertion.
	if isTTY(&bytes.Buffer{}) {
		t.Errorf("isTTY(*bytes.Buffer) = true; want false")
	}
	if isTTY(nil) {
		t.Errorf("isTTY(nil) = true; want false")
	}
}

func TestIsTTY_RegularFileReturnsFalse(t *testing.T) {
	// A normal on-disk file is an *os.File but not a terminal.
	f, err := os.CreateTemp(t.TempDir(), "isatty")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()
	if isTTY(f) {
		t.Errorf("isTTY(regular file) = true; want false")
	}
}

func TestImproveCmd_BasicMetadata(t *testing.T) {
	cmd := newImproveCmd()
	if cmd.Use != "improve" {
		t.Errorf("Use = %q; want improve", cmd.Use)
	}
	if cmd.Short == "" {
		t.Errorf("Short should be set")
	}
	if cmd.RunE == nil {
		t.Errorf("RunE should be wired")
	}
}

func TestImproveCmd_FlagDefaults(t *testing.T) {
	cmd := newImproveCmd()
	flags := cmd.Flags()

	cases := []struct {
		name string
		want string
	}{
		{"verify", ""},
		{"worker", "claude"},
		{"gather-worker", "gemini"},
		{"budget", "0s"},
		{"per-idea-timeout", "20m0s"},
		{"gather-timeout", "10m0s"},
		{"gather-max", "10"},
		{"gather-when-empty", "false"},
		{"gather-goal", ""},
		{"max-iter", "0"},
		{"max-retries", "1"},
		{"dry-run", "false"},
		{"pre-approve", "[]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := flags.Lookup(tc.name)
			if f == nil {
				t.Fatalf("--%s flag missing", tc.name)
			}
			if f.DefValue != tc.want {
				t.Errorf("--%s default = %q; want %q", tc.name, f.DefValue, tc.want)
			}
		})
	}
}

func TestImproveCmd_ParsesFlags(t *testing.T) {
	cmd := newImproveCmd()
	args := []string{
		"--verify", "go test ./...",
		"--worker", "gemini",
		"--gather-worker", "claude",
		"--budget", "30m",
		"--per-idea-timeout", "5m",
		"--gather-timeout", "2m",
		"--gather-max", "25",
		"--gather-when-empty",
		"--gather-goal", "explore tests",
		"--max-iter", "7",
		"--max-retries", "3",
		"--dry-run",
		"--pre-approve", "Bash",
		"--pre-approve", "Read",
	}
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	want := map[string]string{
		"verify":            "go test ./...",
		"worker":            "gemini",
		"gather-worker":     "claude",
		"budget":            "30m0s",
		"per-idea-timeout":  "5m0s",
		"gather-timeout":    "2m0s",
		"gather-max":        "25",
		"gather-when-empty": "true",
		"gather-goal":       "explore tests",
		"max-iter":          "7",
		"max-retries":       "3",
		"dry-run":           "true",
		"pre-approve":       "[Bash,Read]",
	}
	for name, expect := range want {
		got := cmd.Flags().Lookup(name).Value.String()
		if got != expect {
			t.Errorf("flag --%s = %q; want %q", name, got, expect)
		}
	}
}

// The validation gates in RunE run after newApp() succeeds. setupImproveHome
// gives newApp() an isolated, writable home so we exercise the gate logic
// itself rather than its preconditions.

func TestImproveCmd_RequiresVerifyUnlessDryRun(t *testing.T) {
	setupImproveHome(t)

	_, _, err := runImprove(t) // no --verify, no --dry-run
	if err == nil {
		t.Fatal("expected error when --verify is omitted without --dry-run")
	}
	if !strings.Contains(err.Error(), "verify") {
		t.Errorf("error should mention --verify, got: %v", err)
	}
}

func TestImproveCmd_UnknownWorkerErrors(t *testing.T) {
	setupImproveHome(t)

	_, _, err := runImprove(t,
		"--verify", "true",
		"--worker", "does-not-exist",
	)
	if err == nil {
		t.Fatal("expected error for unregistered worker")
	}
	if !strings.Contains(err.Error(), "worker") || !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the missing worker, got: %v", err)
	}
}

func TestImproveCmd_UnknownGatherWorkerErrorsOnlyWhenGatherEnabled(t *testing.T) {
	setupImproveHome(t)

	// Without --gather-when-empty, an unregistered gather-worker should NOT
	// short-circuit validation — but the command still tries to start the
	// loop, which would block. Skip that case and assert only the gated path.
	_, _, err := runImprove(t,
		"--verify", "true",
		"--gather-when-empty",
		"--gather-worker", "no-such-gatherer",
	)
	if err == nil {
		t.Fatal("expected error when --gather-when-empty references an unregistered gather-worker")
	}
	if !strings.Contains(err.Error(), "gather-worker") || !strings.Contains(err.Error(), "no-such-gatherer") {
		t.Errorf("error should name the missing gather-worker, got: %v", err)
	}
}
