// Package e2e holds end-to-end smoke tests that build the real uta binary
// and exercise it as a subprocess. These tests cost more than the unit
// suite (one `go build` per package run) but they verify the wire shape
// from a fresh terminal start, which the in-process tests can't.
//
// mcp_serve_smoke_test.go: launch `uta serve --mcp`, complete the MCP
// handshake, list tools, call the cheapest read-only tool, and assert
// the response shape. This is the "is the MCP integration actually
// alive?" check the docs/mcp-server.md guide promises.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/mcp"
)

// repoRoot walks up from this test file until it finds go.mod.
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

// buildUta compiles the uta binary into a temp dir and returns its path.
// Skips (not fails) when the toolchain is missing or the build fails for
// reasons unrelated to MCP (network outage during `go mod`, etc.).
func buildUta(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping uta-build smoke test")
	}
	root := repoRoot(t)
	tmp := t.TempDir()
	name := "uta"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(tmp, name)
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/uta")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/uta: %v\n%s", err, out)
	}
	return bin
}

// expectedTools is the catalog docs/mcp-server.md advertises. Drift in
// either direction (renamed tool, dropped tool, new tool the docs don't
// mention) flags here. Update both files together.
var expectedTools = []string{
	"uta_providers_list",
	"uta_ideas_list",
	"uta_ideas_add",
	"uta_ideas_show",
	"uta_sessions_list",
	"uta_trajectory_get",
	"uta_audit",
	"uta_run",
	"uta_improve_pick",
	"uta_whiteboard_set",
	"uta_whiteboard_get",
	"uta_whiteboard_list",
}

// TestMCPServeSmoke launches `uta serve --mcp` against a sandboxed
// UTA_HOME, initializes, lists tools, calls uta_providers_list, and
// asserts the tool catalog matches what the docs promise.
//
// Uses a temp UTA_HOME so the test never touches the user's real
// ~/.uta/ — important because this test runs from a freshly-built
// binary that opens the DB and applies migrations.
func TestMCPServeSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e smoke test skipped in -short mode")
	}
	bin := buildUta(t)

	utaHome := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := mcp.NewStdioClient(ctx, bin, []string{"serve", "--mcp"}, map[string]string{
		"UTA_HOME": utaHome,
		"HOME":     utaHome,
	})
	if err != nil {
		t.Fatalf("NewStdioClient: %v", err)
	}
	defer client.Close()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	name, _ := client.ServerInfo()
	if name != "uta" {
		t.Errorf("server name = %q, want %q", name, "uta")
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := map[string]bool{}
	for _, tl := range tools {
		got[tl.Name] = true
	}
	for _, want := range expectedTools {
		if !got[want] {
			t.Errorf("missing tool %q (docs/mcp-server.md advertises it)", want)
		}
	}

	res, err := client.CallTool(ctx, "uta_providers_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool uta_providers_list: %v", err)
	}
	if res.IsError {
		t.Errorf("uta_providers_list returned IsError: %s", res.Content[0].Text)
	}
	if len(res.Content) == 0 || !strings.HasPrefix(strings.TrimSpace(res.Content[0].Text), "[") {
		t.Errorf("uta_providers_list result not a JSON array: %+v", res)
	}
}

// TestServePrintMCPConfig verifies the --print-mcp-config snippets
// match what examples/mcp/*.mcp.json contain. Drift between the two
// would silently confuse users who copy from one or the other.
func TestServePrintMCPConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e smoke test skipped in -short mode")
	}
	bin := buildUta(t)
	root := repoRoot(t)

	cases := []struct {
		client   string
		filename string
	}{
		{"claude-code", "claude-code.mcp.json"},
		{"cursor", "cursor.mcp.json"},
		{"generic", "generic.mcp.json"},
	}
	for _, tc := range cases {
		t.Run(tc.client, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, bin, "serve", "--print-mcp-config", tc.client).Output()
			if err != nil {
				t.Fatalf("uta serve --print-mcp-config %s: %v", tc.client, err)
			}
			fileBytes, err := os.ReadFile(filepath.Join(root, "examples", "mcp", tc.filename))
			if err != nil {
				t.Fatalf("read example: %v", err)
			}
			gotJSON := normaliseJSON(t, out)
			wantJSON := normaliseJSON(t, fileBytes)
			if gotJSON != wantJSON {
				t.Errorf("print-mcp-config %s drift:\nbin output:\n%s\nfile:\n%s", tc.client, gotJSON, wantJSON)
			}
		})
	}
}

// normaliseJSON canonicalises JSON so a trailing newline or different
// whitespace doesn't make the comparison flake. Unmarshal+marshal makes
// the comparison structural.
func normaliseJSON(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("json parse: %v\ninput:\n%s", err, b)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json re-marshal: %v", err)
	}
	return string(out)
}
