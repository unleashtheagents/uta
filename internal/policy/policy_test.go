package policy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func args(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDecide_Defaults(t *testing.T) {
	var p *Policy
	if v, _ := p.Decide("read_file", args(t, map[string]any{"path": "a"})); v != Allow {
		t.Fatalf("read_file default = %v, want allow", v)
	}
	if v, _ := p.Decide("run_command", args(t, map[string]any{"command": "ls"})); v != Ask {
		t.Fatalf("run_command default = %v, want ask", v)
	}
}

func TestDecide_Precedence(t *testing.T) {
	p := New(
		[]string{"run_command(go test *)", "write_file"},
		[]string{"run_command(go test -race*)"},
		[]string{"run_command(*rm -rf*)", "write_file(.env*)"},
	)
	cases := []struct {
		tool string
		a    map[string]any
		want Verdict
	}{
		{"run_command", map[string]any{"command": "go test ./..."}, Allow},
		{"run_command", map[string]any{"command": "go test -race ./..."}, Ask},
		{"run_command", map[string]any{"command": "go test ./... && rm -rf /"}, Deny},
		{"run_command", map[string]any{"command": "make"}, Ask},
		{"write_file", map[string]any{"path": "main.go"}, Allow},
		{"write_file", map[string]any{"path": ".env.local"}, Deny},
		{"glob", map[string]any{"pattern": "**/*.go"}, Allow},
	}
	for _, c := range cases {
		got, rule := p.Decide(c.tool, args(t, c.a))
		if got != c.want {
			t.Errorf("%s %v = %v (rule %q), want %v", c.tool, c.a, got, rule, c.want)
		}
	}
}

func TestAlwaysPattern(t *testing.T) {
	if got := AlwaysPattern("run_command", "go test ./..."); got != "run_command(go test *)" {
		t.Fatalf("got %q", got)
	}
	if got := AlwaysPattern("run_command", "make"); got != "run_command(make *)" {
		t.Fatalf("got %q", got)
	}
	if got := AlwaysPattern("write_file", "x.go"); got != "write_file" {
		t.Fatalf("got %q", got)
	}
}

func TestLoadMerged_AddRule_RoundTrip(t *testing.T) {
	home := t.TempDir()
	work := t.TempDir()
	if err := AddRule(GlobalFile(home), Deny, "run_command(*sudo*)"); err != nil {
		t.Fatal(err)
	}
	if err := AddRule(ProjectFile(work), Allow, "run_command(go test *)"); err != nil {
		t.Fatal(err)
	}
	// Duplicate is a no-op.
	if err := AddRule(ProjectFile(work), Allow, "run_command(go test *)"); err != nil {
		t.Fatal(err)
	}
	p, err := LoadMerged(home, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Sources) != 2 || len(p.Allow) != 1 || len(p.Deny) != 1 {
		t.Fatalf("merged = %+v", p)
	}
	if v, _ := p.Decide("run_command", args(t, map[string]any{"command": "sudo go test ./..."})); v != Deny {
		t.Fatalf("deny should win, got %v", v)
	}
	if v, _ := p.Decide("run_command", args(t, map[string]any{"command": "go test ./..."})); v != Allow {
		t.Fatalf("project allow missing, got %v", v)
	}
	raw, _ := os.ReadFile(ProjectFile(work))
	if !strings.Contains(string(raw), "run_command(go test *)") {
		t.Fatalf("file content: %s", raw)
	}
}

func TestLoad_BadYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "permissions.yaml")
	os.WriteFile(path, []byte("allow: [unclosed"), 0o644)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error")
	}
}

func TestHooks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh hooks")
	}
	p := &Policy{Hooks: Hooks{
		PreTool: []Hook{
			{Match: "run_command(*push*)", Run: `echo "no pushes from agents ($UTA_TOOL_COMMAND)"; exit 2`},
			{Match: "write_file", Run: `exit 1`},
		},
		PostTool: []Hook{
			{Match: "edit_file(*.go)", Run: `echo "gofmt wants $UTA_TOOL_PATH"; exit 2`},
			{Run: `test "$UTA_TOOL_ERROR" = "0"`},
		},
	}}
	p.compile()
	ctx := context.Background()
	block, warn := p.RunPreHooks(ctx, HookEnv{Tool: "run_command", Args: `{"command":"git push origin main"}`})
	if block == nil || !strings.Contains(block.Output, "no pushes from agents (git push origin main)") || len(warn) != 0 {
		t.Fatalf("block=%+v warn=%+v", block, warn)
	}
	block, warn = p.RunPreHooks(ctx, HookEnv{Tool: "write_file", Args: `{"path":"a.go"}`})
	if block != nil || len(warn) != 1 || warn[0].Exit != 1 {
		t.Fatalf("block=%+v warn=%+v", block, warn)
	}
	fb, warn := p.RunPostHooks(ctx, HookEnv{Tool: "edit_file", Args: `{"path":"x.go"}`, IsErr: true})
	if len(fb) != 1 || !strings.Contains(fb[0].Output, "gofmt wants x.go") || len(warn) != 1 {
		t.Fatalf("fb=%+v warn=%+v", fb, warn)
	}
}

func TestTemplateParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.yaml")
	os.WriteFile(path, []byte(Template), 0o644)
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Allow) == 0 || len(f.Deny) == 0 {
		t.Fatalf("template rules missing: %+v", f)
	}
}
