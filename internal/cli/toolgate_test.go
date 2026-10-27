package cli

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/policy"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
)

func newGateTestApp(t *testing.T) (*App, string) {
	t.Helper()
	home := t.TempDir()
	st, err := store.Open(filepath.Join(home, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	blobDir := filepath.Join(home, "blobs")
	os.MkdirAll(blobDir, 0o755)
	app := &App{GlobalHome: home, StateDir: home, Store: st, Blobs: store.NewBlobs(blobDir), Registry: provider.NewRegistry()}
	work := t.TempDir()
	return app, work
}

func req(tool, args, summary string) provider.ToolRequest {
	return provider.ToolRequest{SessionID: "sess-1", SubtaskID: "sub-1", Tool: tool, Arguments: args, Summary: summary}
}

func TestToolGate_NonInteractiveDefaults(t *testing.T) {
	app, work := newGateTestApp(t)
	var errw strings.Builder
	g, err := newToolGate(app, work, nil, strings.NewReader(""), &errw)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := g.Check(context.Background(), req("read_file", `{"path":"a.go"}`, "a.go"))
	if !d.Allow || d.Source != "default" {
		t.Fatalf("read_file should be allowed by default, got %+v", d)
	}
	d, _ = g.Check(context.Background(), req("run_command", `{"command":"go test ./..."}`, "go test ./..."))
	if d.Allow || !strings.Contains(d.Reason, "uta permissions allow 'run_command(go test *)'") {
		t.Fatalf("piped run_command should be denied with a hint, got %+v", d)
	}
}

func TestToolGate_PreApproveAndPolicy(t *testing.T) {
	app, work := newGateTestApp(t)
	if err := policy.AddRule(policy.ProjectFile(work), policy.Deny, "run_command(*rm -rf*)"); err != nil {
		t.Fatal(err)
	}
	if err := policy.AddRule(policy.ProjectFile(work), policy.Allow, "run_command(go test *)"); err != nil {
		t.Fatal(err)
	}
	var errw strings.Builder
	g, err := newToolGate(app, work, []string{"*"}, strings.NewReader(""), &errw)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if d, _ := g.Check(ctx, req("run_command", `{"command":"rm -rf /"}`, "rm -rf /")); d.Allow {
		t.Fatalf("deny rule must beat --pre-approve, got %+v", d)
	}
	if d, _ := g.Check(ctx, req("run_command", `{"command":"go test ./..."}`, "go test ./...")); !d.Allow || d.Source != "policy" {
		t.Fatalf("allow rule, got %+v", d)
	}
	if d, _ := g.Check(ctx, req("run_command", `{"command":"make"}`, "make")); !d.Allow || d.Source != "pre-approve" {
		t.Fatalf("pre-approve covers the ask case, got %+v", d)
	}
}

func TestToolGate_PromptAnswers(t *testing.T) {
	app, work := newGateTestApp(t)
	os.MkdirAll(filepath.Join(work, ".uta"), 0o755)
	var errw strings.Builder
	// Answers: once, deny, this-run, always.
	in := strings.NewReader("y\nn\ns\na\n")
	g, err := newToolGate(app, work, nil, in, &errw)
	if err != nil {
		t.Fatal(err)
	}
	g.interactive = true
	ctx := context.Background()
	if d, _ := g.Check(ctx, req("run_command", `{"command":"make"}`, "make")); !d.Allow || d.Reason != "approved once" {
		t.Fatalf("y: %+v", d)
	}
	if d, _ := g.Check(ctx, req("run_command", `{"command":"make"}`, "make")); d.Allow {
		t.Fatalf("n: %+v", d)
	}
	if d, _ := g.Check(ctx, req("write_file", `{"path":"x.txt","content":"a"}`, "x.txt")); !d.Allow {
		t.Fatalf("s: %+v", d)
	}
	// Same tool again: no prompt consumed (the next answer is still "a").
	if d, _ := g.Check(ctx, req("write_file", `{"path":"y.txt","content":"a"}`, "y.txt")); !d.Allow || d.Reason != "approved earlier in this run" {
		t.Fatalf("session approval should stick: %+v", d)
	}
	if d, _ := g.Check(ctx, req("run_command", `{"command":"go vet ./..."}`, "go vet ./...")); !d.Allow || d.Reason != "approved, rule saved" {
		t.Fatalf("a: %+v", d)
	}
	raw, _ := os.ReadFile(policy.ProjectFile(work))
	if !strings.Contains(string(raw), "run_command(go vet *)") {
		t.Fatalf("always should persist to the project file, got:\n%s", raw)
	}
	if !strings.Contains(errw.String(), "wants run_command:\x1b[0m make") {
		t.Fatalf("prompt text missing: %q", errw.String())
	}
	// Checkpoints were taken for the two approved writes (files did not exist).
	cps, err := app.Store.ListFileCheckpoints("sess-1")
	if err != nil || len(cps) != 2 || cps[0].Existed || cps[0].Path != "x.txt" {
		t.Fatalf("checkpoints = %+v err=%v", cps, err)
	}
}

func TestToolGate_HooksAndCheckpointRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hooks run through sh")
	}
	app, work := newGateTestApp(t)
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("before\n"), 0o644)
	pf := policy.File{
		Allow: []string{"write_file", "edit_file", "run_command"},
		Hooks: policy.Hooks{
			PreTool:  []policy.Hook{{Match: "run_command(*push*)", Run: `echo "no pushing"; exit 2`}},
			PostTool: []policy.Hook{{Match: "edit_file", Run: `echo "lint says hi"; exit 2`}},
		},
	}
	if err := policy.Save(policy.ProjectFile(work), pf); err != nil {
		t.Fatal(err)
	}
	var errw strings.Builder
	g, err := newToolGate(app, work, nil, strings.NewReader(""), &errw)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if d, _ := g.Check(ctx, req("run_command", `{"command":"git push"}`, "git push")); d.Allow || d.Source != "hook" || d.Reason != "no pushing" {
		t.Fatalf("pre hook block: %+v", d)
	}
	if d, _ := g.Check(ctx, req("edit_file", `{"path":"a.txt","target":"before","replacement":"after"}`, "a.txt")); !d.Allow {
		t.Fatalf("edit: %+v", d)
	}
	if fb := g.After(ctx, req("edit_file", `{"path":"a.txt"}`, "a.txt"), "edited", false); !strings.Contains(fb, "[hook feedback] lint says hi") {
		t.Fatalf("post hook feedback = %q", fb)
	}
	// Simulate the tool's edit, then undo through the checkpoint.
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("after\n"), 0o644)
	changes, err := sessionFileChanges(app, "sess-1")
	if err != nil || len(changes) != 1 || changes[0].Before != "before\n" || changes[0].After != "after\n" {
		t.Fatalf("changes = %+v err=%v", changes, err)
	}
	var out strings.Builder
	printFileDiffs(&out, changes, false)
	if !strings.Contains(out.String(), "-before") || !strings.Contains(out.String(), "+after") {
		t.Fatalf("diff = %q", out.String())
	}
	out.Reset()
	if n, err := undoChanges(&out, changes, false); err != nil || n != 1 {
		t.Fatalf("undo n=%d err=%v", n, err)
	}
	got, _ := os.ReadFile(filepath.Join(work, "a.txt"))
	if string(got) != "before\n" {
		t.Fatalf("undo restored %q", got)
	}
	if sid, _ := app.Store.LatestCheckpointSession(); sid != "sess-1" {
		t.Fatalf("latest checkpoint session = %q", sid)
	}
}

func TestExpandFileRefs(t *testing.T) {
	work := t.TempDir()
	os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n"), 0o644)
	os.MkdirAll(filepath.Join(work, "pkg"), 0o755)
	os.WriteFile(filepath.Join(work, "pkg", "x.go"), []byte("package pkg\n"), 0o644)

	goal, labels := expandFileRefs("explain @main.go and @pkg/, then ping @gemini", work)
	if len(labels) != 2 || !strings.HasPrefix(labels[0], "main.go (") || !strings.HasPrefix(labels[1], "pkg/ (1 entries)") {
		t.Fatalf("labels = %v", labels)
	}
	if !strings.Contains(goal, "explain main.go and pkg/, then ping @gemini") {
		t.Fatalf("inline text not rewritten: %q", goal)
	}
	if !strings.Contains(goal, "--- file main.go ---\npackage main") || !strings.Contains(goal, "--- directory pkg/ (1 entries) ---\nx.go") {
		t.Fatalf("attachments missing: %q", goal)
	}
	if g, l := expandFileRefs("no refs here", work); g != "no refs here" || l != nil {
		t.Fatalf("plain goal changed: %q %v", g, l)
	}
	if g, l := expandFileRefs("@../etc/passwd please", work); g != "@../etc/passwd please" || l != nil {
		t.Fatalf("escaping ref must be ignored: %q %v", g, l)
	}
}
