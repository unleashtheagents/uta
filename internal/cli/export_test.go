package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/export"
)

// setupExportEnv isolates UTA state under a tempdir so newApp() opens a
// fresh empty store. Setting UTA_HOME also short-circuits the project
// auto-detection branch in newApp().
func setupExportEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("UTA_HOME", home)
	t.Setenv("HOME", t.TempDir())
	return home
}

// runExportDB builds a fresh exportdb command and executes it with the
// given args. Returns stdout, stderr, and the executor error.
func runExportDB(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newExportDBCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

// TestExportDB_NoArgsAndNoAllErrors exercises the explicit RunE guard:
// without --all the command requires a session-id positional argument.
// Validation runs before newApp(), so no env setup is required.
func TestExportDB_NoArgsAndNoAllErrors(t *testing.T) {
	_, _, err := runExportDB(t)
	if err == nil {
		t.Fatal("exportdb with no args and no --all: want error, got nil")
	}
	if !strings.Contains(err.Error(), "either pass a session-id or --all") {
		t.Errorf("error = %v; want mention of session-id/--all", err)
	}
}

// TestExportDB_AllWithSessionIDErrors covers the mutual-exclusion guard:
// --all and a positional session-id are not allowed together.
func TestExportDB_AllWithSessionIDErrors(t *testing.T) {
	_, _, err := runExportDB(t, "--all", "sess-1")
	if err == nil {
		t.Fatal("exportdb --all sess-1: want error, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v; want mutual-exclusion message", err)
	}
}

// TestExportDB_TooManyPositionalArgs verifies the cobra-level
// MaximumNArgs(1) constraint surfaces an error before RunE runs. Catches
// regressions if someone changes the Args setting.
func TestExportDB_TooManyPositionalArgs(t *testing.T) {
	_, _, err := runExportDB(t, "sess-1", "sess-2")
	if err == nil {
		t.Fatal("exportdb with 2 positional args: want error, got nil")
	}
}

// TestExportDB_AllToStdout_EmptyStore is the happy path with an empty
// store. The output must be a valid Export JSON value with zero row
// counts and a populated body hash.
func TestExportDB_AllToStdout_EmptyStore(t *testing.T) {
	setupExportEnv(t)
	out, _, err := runExportDB(t, "--all")
	if err != nil {
		t.Fatalf("exportdb --all: %v", err)
	}
	var exp export.Export
	if err := json.Unmarshal([]byte(out), &exp); err != nil {
		t.Fatalf("decode export: %v\nraw=%s", err, out)
	}
	if exp.Header.Scope != "all" {
		t.Errorf("Header.Scope = %q; want all", exp.Header.Scope)
	}
	if exp.Header.FormatVersion != export.FormatVersion {
		t.Errorf("Header.FormatVersion = %d; want %d", exp.Header.FormatVersion, export.FormatVersion)
	}
	if exp.Header.RowCounts["sessions"] != 0 {
		t.Errorf("sessions count = %d; want 0", exp.Header.RowCounts["sessions"])
	}
	if exp.Header.BodySHA256 == "" {
		t.Errorf("BodySHA256 should be populated")
	}
}

// TestExportDB_AllToFile_AtomicRename verifies the temp-file path: the
// command writes to a hidden sibling and atomically renames it onto the
// final path, prints a confirmation to stderr, and leaves no .tmp
// residue behind on success.
func TestExportDB_AllToFile_AtomicRename(t *testing.T) {
	setupExportEnv(t)
	outDir := t.TempDir()
	outFile := filepath.Join(outDir, "dump.json")
	_, stderr, err := runExportDB(t, "--all", "-o", outFile)
	if err != nil {
		t.Fatalf("exportdb -o: %v", err)
	}
	if !strings.Contains(stderr, "wrote "+outFile) {
		t.Errorf("stderr should announce wrote-path: %q", stderr)
	}
	if !strings.Contains(stderr, "sessions=0") {
		t.Errorf("stderr should include row counts: %q", stderr)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read output file: %v", err)
	}
	var exp export.Export
	if err := json.Unmarshal(data, &exp); err != nil {
		t.Fatalf("decode output file: %v\nraw=%s", err, data)
	}

	// On success no .tmp sibling should remain alongside the final file.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") || strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("temp file leaked alongside output: %s", e.Name())
		}
	}
}

// TestExportDB_NoBlobs verifies the --no-blobs flag turns off blob
// inlining: Header.IncludeBlobs must be false and Blobs must be empty.
func TestExportDB_NoBlobs(t *testing.T) {
	setupExportEnv(t)
	out, _, err := runExportDB(t, "--all", "--no-blobs")
	if err != nil {
		t.Fatalf("exportdb --no-blobs: %v", err)
	}
	var exp export.Export
	if err := json.Unmarshal([]byte(out), &exp); err != nil {
		t.Fatalf("decode export: %v\nraw=%s", err, out)
	}
	if exp.Header.IncludeBlobs {
		t.Error("Header.IncludeBlobs = true; want false with --no-blobs")
	}
	if len(exp.Blobs) != 0 {
		t.Errorf("Blobs map = %v; want empty with --no-blobs", exp.Blobs)
	}
}

// TestExportDB_Indent verifies the --indent flag drives the json.Encoder
// indent setting: pretty output contains newline+two-space indentation,
// compact output does not.
func TestExportDB_Indent(t *testing.T) {
	setupExportEnv(t)
	compact, _, err := runExportDB(t, "--all")
	if err != nil {
		t.Fatalf("exportdb compact: %v", err)
	}
	pretty, _, err := runExportDB(t, "--all", "--indent")
	if err != nil {
		t.Fatalf("exportdb --indent: %v", err)
	}
	if !strings.Contains(pretty, "\n  ") {
		t.Errorf("--indent should produce pretty JSON; got:\n%s", pretty)
	}
	if strings.Contains(compact, "\n  ") {
		t.Errorf("default output should be compact (no leading-space indent); got:\n%s", compact)
	}
}

// TestExportDB_MissingSessionIDErrors verifies that asking for an
// unknown session id surfaces export.Run's "session not found" error
// through the CLI layer rather than crashing or writing partial output.
func TestExportDB_MissingSessionIDErrors(t *testing.T) {
	setupExportEnv(t)
	out, _, err := runExportDB(t, "no-such-session")
	if err == nil {
		t.Fatalf("exportdb of unknown session: want error, got nil; stdout=%q", out)
	}
	if !strings.Contains(err.Error(), "session not found") {
		t.Errorf("error = %v; want 'session not found'", err)
	}
}
