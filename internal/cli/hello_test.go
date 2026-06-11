package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
)

// stubHelloProvider implements provider.AgentProvider with caller-supplied
// detection metadata, so the hello tour can be exercised without anything
// real on PATH.
type stubHelloProvider struct {
	name string
	det  provider.Detection
}

func (s *stubHelloProvider) Name() string                              { return s.name }
func (s *stubHelloProvider) Detect(context.Context) provider.Detection { return s.det }
func (s *stubHelloProvider) RunHeadless(context.Context, string, provider.RunOptions, chan<- provider.Event) (provider.RunResult, error) {
	return provider.RunResult{}, nil
}

// newHelloTestApp wires a minimal App rooted at a tempdir and seeds the
// registry with the supplied provider stubs. Returns the App and a
// cleanup hook.
func newHelloTestApp(t *testing.T, stubs ...*stubHelloProvider) *App {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	reg := provider.NewRegistry()
	for _, s := range stubs {
		if err := reg.Register(s, true); err != nil {
			t.Fatalf("Register(%s): %v", s.name, err)
		}
	}
	app := &App{
		GlobalHome: dir,
		StateDir:   dir,
		Store:      st,
		Registry:   reg,
	}
	return app
}

// TestRunHello_NoProvidersTellsTheUserWhatToInstall covers the
// zero-providers branch: the tour must call out the missing dependency
// and point at install docs rather than printing a useless next-step.
func TestRunHello_NoProvidersTellsTheUserWhatToInstall(t *testing.T) {
	app := newHelloTestApp(t)
	var buf bytes.Buffer
	cmd := newHelloCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	if err := runHello(cmd, app); err != nil {
		t.Fatalf("runHello: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"uta — unleash the agents",
		"Detected providers",
		"(none on PATH)",
		"docs.claude.com",
		"gemini-cli",
		"After installing a provider",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("hello output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestRunHello_WithProvidersGivesAConcreteCommand covers the happy path:
// when at least one provider is available, the tour must print a
// ready-to-run `uta run ... --worker <name>` line so the user has no
// guesswork in front of them.
func TestRunHello_WithProvidersGivesAConcreteCommand(t *testing.T) {
	stub := &stubHelloProvider{
		name: "claude",
		det: provider.Detection{
			Available: true, Version: "9.9.9", BinaryPath: "/fake/claude",
		},
	}
	app := newHelloTestApp(t, stub)
	var buf bytes.Buffer
	cmd := newHelloCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	if err := runHello(cmd, app); err != nil {
		t.Fatalf("runHello: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "claude") || !strings.Contains(out, "9.9.9") {
		t.Errorf("provider section missing claude/9.9.9:\n%s", out)
	}
	if !strings.Contains(out, "uta run -g") || !strings.Contains(out, "--worker claude") {
		t.Errorf("expected a `uta run ... --worker claude` line, got:\n%s", out)
	}
}

// TestRunHello_MentionsMCPServerMode guards a thing the docs sell hard:
// the tour points users at `uta serve --print-mcp-config` so they
// discover MCP server mode without having to read the README first.
func TestRunHello_MentionsMCPServerMode(t *testing.T) {
	app := newHelloTestApp(t)
	var buf bytes.Buffer
	cmd := newHelloCmd()
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	if err := runHello(cmd, app); err != nil {
		t.Fatalf("runHello: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "uta serve --print-mcp-config") {
		t.Errorf("expected --print-mcp-config hint:\n%s", out)
	}
}
