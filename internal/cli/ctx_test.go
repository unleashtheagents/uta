package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/config"
)

func TestSanitizeCtxName(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"simple", "abis.json", "abis.json", false},
		{"with-dash", "phase-1.json", "phase-1.json", false},
		{"with-dot", "config.v2.yaml", "config.v2.yaml", false},
		{"empty", "", "", true},
		{"dot", ".", "", true},
		{"dotdot", "..", "", true},
		{"slash-traversal", "../etc/passwd", "", true},
		{"absolute", "/etc/passwd", "", true},
		{"nested", "sub/file", "", true},
		{"trailing-slash", "name/", "", true},
		{"backslash-as-name-on-unix", "a\\b", "a\\b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sanitizeCtxName(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("sanitizeCtxName(%q) = %q, nil; want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("sanitizeCtxName(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("sanitizeCtxName(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// setupCtxProject creates an isolated HOME, an isolated UTA global home, and
// an initialized project rooted at a fresh temp dir. It chdir's into the
// project for the duration of the test and restores the prior cwd on cleanup.
// Returns the absolute project root so tests can poke at ContextDir directly.
func setupCtxProject(t *testing.T) string {
	t.Helper()
	// Force a clean HOME so the project walker can't reach the real home.
	t.Setenv("HOME", t.TempDir())
	// Empty UTA_HOME so newApp() performs project auto-detection.
	t.Setenv("UTA_HOME", "")

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := config.SaveProject(root, &config.Project{Name: "ctx-test"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	// Pre-create the context dir so commands that rely on it don't have to.
	if err := os.MkdirAll(filepath.Join(root, ".uta", "context"), 0o755); err != nil {
		t.Fatalf("mkdir context: %v", err)
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

// runCtx builds a fresh ctx command tree and executes it with the given args
// and stdin. Returns stdout, stderr, and the executor error.
func runCtx(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	cmd := newCtxCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(args)
	// SilenceUsage avoids polluting stderr with help text on error paths.
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

func TestCtxPutAndGet_Roundtrip(t *testing.T) {
	root := setupCtxProject(t)

	src := filepath.Join(t.TempDir(), "input.json")
	payload := []byte(`{"hello":"world"}`)
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatalf("seed input: %v", err)
	}

	if _, _, err := runCtx(t, "", "put", "greeting.json", src); err != nil {
		t.Fatalf("put: %v", err)
	}
	stored, err := os.ReadFile(filepath.Join(root, ".uta", "context", "greeting.json"))
	if err != nil {
		t.Fatalf("read stored blob: %v", err)
	}
	if !bytes.Equal(stored, payload) {
		t.Fatalf("stored blob mismatch: got %q want %q", stored, payload)
	}

	out, _, err := runCtx(t, "", "get", "greeting.json")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out != string(payload) {
		t.Fatalf("get output mismatch: got %q want %q", out, string(payload))
	}
}

func TestCtxPut_FromStdin(t *testing.T) {
	root := setupCtxProject(t)

	payload := "streamed-bytes\n"
	if _, _, err := runCtx(t, payload, "put", "from-stdin.txt", "-"); err != nil {
		t.Fatalf("put -: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(root, ".uta", "context", "from-stdin.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("stdin blob mismatch: got %q want %q", got, payload)
	}
}

func TestCtxPut_RejectsPathTraversal(t *testing.T) {
	root := setupCtxProject(t)

	// Seed a file outside the context dir; the traversal attempt must not
	// reach it (we don't actually need to read it, but having it there makes
	// the threat realistic).
	outside := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(outside, []byte("nope"), 0o644); err != nil {
		t.Fatalf("seed outside: %v", err)
	}

	src := filepath.Join(t.TempDir(), "in.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed input: %v", err)
	}

	_, _, err := runCtx(t, "", "put", "../escaped.txt", src)
	if err == nil {
		t.Fatalf("put with traversal name: want error, got nil")
	}
	// The blob must not have been written next to the project root.
	if _, statErr := os.Stat(filepath.Join(root, "escaped.txt")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("traversal succeeded: %v", statErr)
	}
}

func TestCtxGet_MissingReportsNotFound(t *testing.T) {
	setupCtxProject(t)
	_, _, err := runCtx(t, "", "get", "nope.json")
	if err == nil {
		t.Fatalf("get missing: want error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found: %v", err)
	}
}

func TestCtxRm_RemovesBlob(t *testing.T) {
	root := setupCtxProject(t)

	target := filepath.Join(root, ".uta", "context", "doomed.txt")
	if err := os.WriteFile(target, []byte("bye"), 0o644); err != nil {
		t.Fatalf("seed blob: %v", err)
	}

	if _, _, err := runCtx(t, "", "rm", "doomed.txt"); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("rm did not remove: %v", statErr)
	}
}

func TestCtxRm_MissingReportsNotFound(t *testing.T) {
	setupCtxProject(t)
	_, _, err := runCtx(t, "", "rm", "ghost.txt")
	if err == nil {
		t.Fatalf("rm missing: want error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention not found: %v", err)
	}
}

func TestCtxList_TextEmpty(t *testing.T) {
	setupCtxProject(t)
	out, _, err := runCtx(t, "", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "(empty)") {
		t.Errorf("empty list output should say (empty): %q", out)
	}
}

func TestCtxList_TextLists(t *testing.T) {
	root := setupCtxProject(t)
	for _, n := range []string{"alpha.txt", "beta.json"} {
		if err := os.WriteFile(filepath.Join(root, ".uta", "context", n), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed %s: %v", n, err)
		}
	}
	out, _, err := runCtx(t, "", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, n := range []string{"alpha.txt", "beta.json", "NAME", "SIZE", "MODIFIED"} {
		if !strings.Contains(out, n) {
			t.Errorf("list output missing %q:\n%s", n, out)
		}
	}
}

func TestCtxList_JSON(t *testing.T) {
	root := setupCtxProject(t)
	payload := []byte("hello")
	if err := os.WriteFile(filepath.Join(root, ".uta", "context", "one.txt"), payload, 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	out, _, err := runCtx(t, "", "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var rows []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode json: %v\nraw=%s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("rows: got %d want 1: %+v", len(rows), rows)
	}
	if rows[0].Name != "one.txt" || rows[0].Size != int64(len(payload)) {
		t.Errorf("row mismatch: %+v", rows[0])
	}
	if !strings.HasSuffix(rows[0].Path, filepath.Join(".uta", "context", "one.txt")) {
		t.Errorf("row path unexpected: %q", rows[0].Path)
	}
}

func TestCtxCommands_RejectInvalidNames(t *testing.T) {
	setupCtxProject(t)
	src := filepath.Join(t.TempDir(), "src.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, bad := range []string{"..", ".", "a/b", "/abs"} {
		if _, _, err := runCtx(t, "", "put", bad, src); err == nil {
			t.Errorf("put %q: want error, got nil", bad)
		}
		if _, _, err := runCtx(t, "", "get", bad); err == nil {
			t.Errorf("get %q: want error, got nil", bad)
		}
		if _, _, err := runCtx(t, "", "rm", bad); err == nil {
			t.Errorf("rm %q: want error, got nil", bad)
		}
	}
}
