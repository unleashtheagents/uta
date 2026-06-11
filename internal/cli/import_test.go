package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/export"
)

// runImport builds a fresh import command and executes it with the given args
// and stdin. Returns stdout, stderr, and the executor error.
func runImport(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newImportCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

// writeExportFile writes exp as JSON to a tempdir file and returns its path.
func writeExportFile(t *testing.T, exp *export.Export) string {
	t.Helper()
	data, err := json.Marshal(exp)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	path := filepath.Join(t.TempDir(), "dump.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write export file: %v", err)
	}
	return path
}

// makeMinimalExport returns an Export with one session and no events/blobs.
// BodySHA256 is left empty so the import will not attempt hash verification
// unless the caller explicitly sets a value.
func makeMinimalExport(sessionID string) *export.Export {
	return &export.Export{
		Header: export.Header{
			FormatVersion: export.FormatVersion,
			Scope:         "session",
			SessionIDs:    []string{sessionID},
			RowCounts:     map[string]int{"sessions": 1, "subtasks": 0, "events": 0},
		},
		Sessions: []export.SessionRow{{
			ID:        sessionID,
			Goal:      "g-" + sessionID,
			Worker:    "claude",
			Status:    "running",
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			MetaJSON:  "{}",
		}},
	}
}

// TestImport_RequiresExactlyOneArg confirms the cobra.ExactArgs(1) wiring
// surfaces a usage error before RunE runs. No env setup needed because the
// arg check happens first.
func TestImport_RequiresExactlyOneArg(t *testing.T) {
	if _, _, err := runImport(t, ""); err == nil {
		t.Fatal("import with no args: want error, got nil")
	}
	if _, _, err := runImport(t, "", "a", "b"); err == nil {
		t.Fatal("import with 2 args: want error, got nil")
	}
}

// TestImport_InvalidJSON ensures the json.Decoder failure path bubbles up
// with the "parse export" prefix from import.go.
func TestImport_InvalidJSON(t *testing.T) {
	setupExportEnv(t)
	path := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(path, []byte("not json at all"), 0o644); err != nil {
		t.Fatalf("write broken file: %v", err)
	}
	_, _, err := runImport(t, "", path)
	if err == nil {
		t.Fatal("import of garbage file: want error, got nil")
	}
	if !strings.Contains(err.Error(), "parse export") {
		t.Errorf("error = %v; want wrapper 'parse export'", err)
	}
}

// TestImport_StdinDash verifies that passing '-' as the path makes the
// command read the export from cmd.InOrStdin() rather than opening a file.
// We feed a minimal valid export and expect the success line on stdout.
func TestImport_StdinDash(t *testing.T) {
	setupExportEnv(t)
	exp := makeMinimalExport("sess-stdin")
	data, err := json.Marshal(exp)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	out, _, err := runImport(t, string(data), "-")
	if err != nil {
		t.Fatalf("import - (stdin): %v", err)
	}
	if !strings.Contains(out, "imported: sessions=1") {
		t.Errorf("stdout = %q; want sessions=1 line", out)
	}
}

// TestImport_FileHappyPath imports a minimal export from a file and checks
// the success summary line includes the row counts from ImportResult.
func TestImport_FileHappyPath(t *testing.T) {
	setupExportEnv(t)
	exp := makeMinimalExport("sess-file")
	path := writeExportFile(t, exp)
	out, _, err := runImport(t, "", path)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.Contains(out, "imported: sessions=1 subtasks=0 events=0 blobs=0") {
		t.Errorf("stdout = %q; want summary line", out)
	}
}

// TestImport_ForceFlagWired exercises the --force flag end-to-end: a second
// import of the same session id without --force must fail with the clobber
// error from export.Restore, and the same import with --force must succeed
// and report the replaced session on stdout.
func TestImport_ForceFlagWired(t *testing.T) {
	setupExportEnv(t)
	exp := makeMinimalExport("sess-collide")
	path := writeExportFile(t, exp)

	if _, _, err := runImport(t, "", path); err != nil {
		t.Fatalf("first import: %v", err)
	}

	_, _, err := runImport(t, "", path)
	if err == nil {
		t.Fatal("second import without --force: want error, got nil")
	}
	if !strings.Contains(err.Error(), "would clobber") {
		t.Errorf("error = %v; want 'would clobber'", err)
	}

	out, _, err := runImport(t, "", "--force", path)
	if err != nil {
		t.Fatalf("import --force: %v", err)
	}
	if !strings.Contains(out, "replaced existing sessions") {
		t.Errorf("stdout = %q; want 'replaced existing sessions' line", out)
	}
	if !strings.Contains(out, "sess-collide") {
		t.Errorf("stdout = %q; want replaced session id 'sess-collide'", out)
	}
}

// TestImport_SkipHashCheckFlagWired exercises --skip-hash-check: a deliberately
// bogus body_sha256 must fail the integrity check by default, and pass once
// the flag is provided.
func TestImport_SkipHashCheckFlagWired(t *testing.T) {
	setupExportEnv(t)
	exp := makeMinimalExport("sess-hash")
	// A non-empty BodySHA256 triggers verifyBodyHash; a fake value will never
	// match the canonical hash of the body.
	exp.Header.BodySHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	path := writeExportFile(t, exp)

	_, _, err := runImport(t, "", path)
	if err == nil {
		t.Fatal("import with bad sha256: want error, got nil")
	}
	if !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Errorf("error = %v; want 'sha256 mismatch'", err)
	}

	out, _, err := runImport(t, "", "--skip-hash-check", path)
	if err != nil {
		t.Fatalf("import --skip-hash-check: %v", err)
	}
	if !strings.Contains(out, "imported: sessions=1") {
		t.Errorf("stdout = %q; want sessions=1 line", out)
	}
}

// TestImport_UnsupportedFormatVersion confirms a mismatched format_version
// from the export bubbles up Restore's version-check error. Guards against
// regressions in the wiring between CLI decoding and export.Restore.
func TestImport_UnsupportedFormatVersion(t *testing.T) {
	setupExportEnv(t)
	exp := makeMinimalExport("sess-vers")
	exp.Header.FormatVersion = export.FormatVersion + 99
	path := writeExportFile(t, exp)
	_, _, err := runImport(t, "", path)
	if err == nil {
		t.Fatal("import with wrong format_version: want error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported format_version") {
		t.Errorf("error = %v; want 'unsupported format_version'", err)
	}
}
