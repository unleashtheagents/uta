package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDashIfEmpty(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "-"},
		{"x", "x"},
		{"  ", "  "}, // only the empty string substitutes; whitespace is preserved
	}
	for _, tc := range cases {
		if got := dashIfEmpty(tc.in); got != tc.want {
			t.Errorf("dashIfEmpty(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestProvidersCmd_FlagDefaults(t *testing.T) {
	cmd := newProvidersCmd()
	jsonFlag := cmd.Flags().Lookup("json")
	if jsonFlag == nil {
		t.Fatal("--json flag missing")
	}
	if jsonFlag.DefValue != "false" {
		t.Errorf("--json default = %q; want false", jsonFlag.DefValue)
	}
}

// setupProvidersHome isolates HOME, UTA_HOME and PATH so newApp() builds a
// registry that detects neither the claude nor the gemini binary. The
// resulting Detection rows uniformly report Available=false with the
// "not found on PATH" notes, which is exactly what the providers command
// must format as missing/'-'.
func setupProvidersHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	utaHome := filepath.Join(home, ".uta")
	if err := os.MkdirAll(utaHome, 0o755); err != nil {
		t.Fatalf("mkdir uta home: %v", err)
	}
	t.Setenv("UTA_HOME", utaHome)
	// Force exec.LookPath to fail for both builtins so Detect() returns
	// Available=false with Err set — the providers command must still
	// render that row without panicking.
	t.Setenv("PATH", "")

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

func runProviders(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newProvidersCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

func TestProvidersCmd_JSONOutput_MissingProviders(t *testing.T) {
	setupProvidersHome(t)

	out, _, err := runProviders(t, "--json")
	if err != nil {
		t.Fatalf("providers --json: %v", err)
	}

	var rows []struct {
		Name         string   `json:"name"`
		Available    bool     `json:"available"`
		BinaryPath   string   `json:"binary_path,omitempty"`
		Version      string   `json:"version,omitempty"`
		Capabilities []string `json:"capabilities,omitempty"`
		Notes        string   `json:"notes,omitempty"`
		Error        string   `json:"error,omitempty"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode JSON: %v\nraw=%s", err, out)
	}

	// Registry seeds two builtins. Names() sorts alphabetically: claude, gemini.
	if len(rows) < 2 {
		t.Fatalf("rows: got %d want >=2; raw=%s", len(rows), out)
	}
	byName := map[string]int{}
	for i, r := range rows {
		byName[r.Name] = i
	}
	for _, want := range []string{"claude", "gemini"} {
		idx, ok := byName[want]
		if !ok {
			t.Fatalf("expected provider %q in JSON: %s", want, out)
		}
		r := rows[idx]
		if r.Available {
			t.Errorf("provider %q: Available=true; want false when binary not on PATH", want)
		}
		// Detection error must be surfaced so callers can act on it.
		if r.Error == "" {
			t.Errorf("provider %q: error empty; want non-empty when detection failed (notes=%q)", want, r.Notes)
		}
		// Missing optional fields must be omitted by omitempty.
		if r.BinaryPath != "" {
			t.Errorf("provider %q: binary_path=%q; want empty when not on PATH", want, r.BinaryPath)
		}
		if r.Version != "" {
			t.Errorf("provider %q: version=%q; want empty when not on PATH", want, r.Version)
		}
		if len(r.Capabilities) != 0 {
			t.Errorf("provider %q: capabilities=%v; want empty when not on PATH", want, r.Capabilities)
		}
	}
}

func TestProvidersCmd_TableOutput_MissingFieldsRenderedAsDash(t *testing.T) {
	setupProvidersHome(t)

	out, _, err := runProviders(t)
	if err != nil {
		t.Fatalf("providers: %v", err)
	}

	// Header row carries the schema the table layout promises.
	for _, want := range []string{"NAME", "STATUS", "VERSION", "BINARY", "CAPABILITIES", "NOTES"} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing %q:\n%s", want, out)
		}
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected header + at least 2 provider rows, got %d:\n%s", len(lines), out)
	}

	for _, name := range []string{"claude", "gemini"} {
		var row string
		for _, l := range lines[1:] {
			if strings.HasPrefix(strings.TrimSpace(l), name+" ") || strings.TrimSpace(l) == name {
				row = l
				break
			}
		}
		if row == "" {
			t.Fatalf("table row for %q not found:\n%s", name, out)
		}
		if !strings.Contains(row, "missing") {
			t.Errorf("row %q should report status 'missing':\n%s", name, row)
		}
		// With no binary on PATH, version/binary/capabilities are all empty and
		// must fall back to '-' via dashIfEmpty / capsString.
		fields := strings.Fields(row)
		// Row layout: NAME STATUS VERSION BINARY CAPABILITIES NOTES
		// Notes for the not-found case is non-empty (set by Detect), so we
		// only assert dashes for the three blank-prone columns.
		if len(fields) < 5 {
			t.Fatalf("row %q has %d fields; want >=5: %q", name, len(fields), row)
		}
		if fields[2] != "-" {
			t.Errorf("row %q: VERSION = %q; want '-'", name, fields[2])
		}
		if fields[3] != "-" {
			t.Errorf("row %q: BINARY = %q; want '-'", name, fields[3])
		}
		if fields[4] != "-" {
			t.Errorf("row %q: CAPABILITIES = %q; want '-'", name, fields[4])
		}
	}
}
