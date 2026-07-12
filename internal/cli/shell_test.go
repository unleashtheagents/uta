package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// fakeShellBackend records every dispatch so tests can assert routing
// without touching providers, the store, or the engine.
type fakeShellBackend struct {
	turns     []string    // ExecTurn goals
	turnsAs   [][2]string // ExecTurnAs (worker, goal)
	localCmds []string    // LocalExec command lines
	turnErr   error       // returned from ExecTurn/ExecTurnAs
	attachErr error       // returned from Attach
	workerErr error       // returned from SetWorker
	modelErr  error       // returned from SetModel

	worker   string
	session  string
	mode     string
	model    string
	tools    []string
	workdir  string
	newCalls int
}

func (f *fakeShellBackend) ExecTurn(_ context.Context, goal string) error {
	f.turns = append(f.turns, goal)
	return f.turnErr
}

func (f *fakeShellBackend) ExecTurnAs(_ context.Context, worker, goal string) error {
	f.turnsAs = append(f.turnsAs, [2]string{worker, goal})
	return f.turnErr
}

func (f *fakeShellBackend) SessionShort() string { return f.session }
func (f *fakeShellBackend) NewConversation()     { f.newCalls++; f.session = "" }

func (f *fakeShellBackend) Attach(id string) (string, error) {
	if f.attachErr != nil {
		return "", f.attachErr
	}
	f.session = shortID(id)
	return id, nil
}

func (f *fakeShellBackend) Sessions(w io.Writer, limit int) error {
	fmt.Fprintf(w, "sessions(limit=%d)\n", limit)
	return nil
}

func (f *fakeShellBackend) Agents(_ context.Context, w io.Writer) error {
	fmt.Fprintln(w, "agents-listing")
	return nil
}

func (f *fakeShellBackend) Worker() string { return f.worker }

func (f *fakeShellBackend) SetWorker(_ context.Context, name string) error {
	if f.workerErr != nil {
		return f.workerErr
	}
	f.worker = name
	return nil
}

func (f *fakeShellBackend) ModeName() string { return f.mode }

func (f *fakeShellBackend) SetMode(name string) error {
	f.mode = name
	return nil
}

func (f *fakeShellBackend) ClearMode() { f.mode = "" }

func (f *fakeShellBackend) Model() string { return f.model }

func (f *fakeShellBackend) SetModel(spec string) error {
	if f.modelErr != nil {
		return f.modelErr
	}
	f.model = spec
	return nil
}

func (f *fakeShellBackend) ClearModel() { f.model = "" }

func (f *fakeShellBackend) Tools() []string { return f.tools }

func (f *fakeShellBackend) SetTools(tools []string) { f.tools = tools }

func (f *fakeShellBackend) SetWorkdir(dir string) error {
	f.workdir = dir
	return nil
}

func (f *fakeShellBackend) Workdir() string { return f.workdir }

func (f *fakeShellBackend) LocalExec(_ context.Context, cmdline string) error {
	f.localCmds = append(f.localCmds, cmdline)
	return nil
}

func (f *fakeShellBackend) Status(w io.Writer) { fmt.Fprintln(w, "status-report") }

func dispatch(t *testing.T, f *fakeShellBackend, line string) (out, errw string, quit bool) {
	t.Helper()
	var ob, eb strings.Builder
	quit = dispatchShellLine(context.Background(), line, f, &ob, &eb)
	return ob.String(), eb.String(), quit
}

func TestDispatch_ExitWords(t *testing.T) {
	f := &fakeShellBackend{}
	for _, line := range []string{"exit", "quit", "/exit", "/quit"} {
		if _, _, quit := dispatch(t, f, line); !quit {
			t.Errorf("%q should quit the shell", line)
		}
	}
	if len(f.turns) != 0 {
		t.Errorf("exit words must not become goals, got %v", f.turns)
	}
}

func TestDispatch_FreeTextBecomesTurn(t *testing.T) {
	f := &fakeShellBackend{}
	_, _, quit := dispatch(t, f, "summarize the repo")
	if quit {
		t.Fatal("a goal must not quit the shell")
	}
	if len(f.turns) != 1 || f.turns[0] != "summarize the repo" {
		t.Errorf("turns = %v, want the goal verbatim", f.turns)
	}
}

func TestDispatch_TurnErrorIsPrintedNotFatal(t *testing.T) {
	f := &fakeShellBackend{turnErr: errors.New("provider melted")}
	_, errw, quit := dispatch(t, f, "do something")
	if quit {
		t.Fatal("a failed turn must not quit the shell")
	}
	if !strings.Contains(errw, "provider melted") {
		t.Errorf("stderr should carry the turn error, got %q", errw)
	}
}

func TestDispatch_AtRoutesToNamedAgent(t *testing.T) {
	f := &fakeShellBackend{}
	dispatch(t, f, "@gemini cross-check the last answer")
	if len(f.turnsAs) != 1 {
		t.Fatalf("turnsAs = %v, want one routed turn", f.turnsAs)
	}
	if f.turnsAs[0] != [2]string{"gemini", "cross-check the last answer"} {
		t.Errorf("routed turn = %v", f.turnsAs[0])
	}
}

func TestDispatch_AtWithoutGoalIsUsageError(t *testing.T) {
	f := &fakeShellBackend{}
	_, errw, _ := dispatch(t, f, "@gemini")
	if len(f.turnsAs) != 0 {
		t.Errorf("no turn should run, got %v", f.turnsAs)
	}
	if !strings.Contains(errw, "usage: @<agent>") {
		t.Errorf("stderr should show usage, got %q", errw)
	}
}

func TestDispatch_BangRunsLocally(t *testing.T) {
	f := &fakeShellBackend{}
	dispatch(t, f, "!git status -sb")
	if len(f.localCmds) != 1 || f.localCmds[0] != "git status -sb" {
		t.Errorf("localCmds = %v", f.localCmds)
	}
	if len(f.turns) != 0 {
		t.Errorf("a !-line must not become a goal, got %v", f.turns)
	}
}

func TestDispatch_NewAndClearResetConversation(t *testing.T) {
	for _, cmd := range []string{"/new", "/clear"} {
		f := &fakeShellBackend{session: "1a2b3c4d"}
		dispatch(t, f, cmd)
		if f.newCalls != 1 {
			t.Errorf("%s: newCalls = %d, want 1", cmd, f.newCalls)
		}
	}
}

func TestDispatch_UseAndResumeAttach(t *testing.T) {
	for _, cmd := range []string{"/use", "/resume"} {
		f := &fakeShellBackend{}
		out, _, _ := dispatch(t, f, cmd+" 1a2b3c4d")
		if f.session != "1a2b3c4d" {
			t.Errorf("%s: session = %q", cmd, f.session)
		}
		if !strings.Contains(out, "attached to session") {
			t.Errorf("%s: out = %q", cmd, out)
		}
	}
}

func TestDispatch_UseWithoutArgShowsInvokedName(t *testing.T) {
	f := &fakeShellBackend{}
	_, errw, _ := dispatch(t, f, "/resume")
	if !strings.Contains(errw, "usage: /resume <session-id>") {
		t.Errorf("usage should echo the invoked alias, got %q", errw)
	}
}

func TestDispatch_SessionsParsesLimit(t *testing.T) {
	f := &fakeShellBackend{}
	out, _, _ := dispatch(t, f, "/sessions 3")
	if !strings.Contains(out, "sessions(limit=3)") {
		t.Errorf("out = %q, want limit 3", out)
	}
	out, _, _ = dispatch(t, f, "/sessions")
	if !strings.Contains(out, "sessions(limit=10)") {
		t.Errorf("out = %q, want default limit 10", out)
	}
	_, errw, _ := dispatch(t, f, "/sessions nope")
	if !strings.Contains(errw, "usage: /sessions") {
		t.Errorf("errw = %q, want usage error", errw)
	}
}

func TestDispatch_WorkerShowAndSwitch(t *testing.T) {
	f := &fakeShellBackend{worker: "claude"}
	out, _, _ := dispatch(t, f, "/worker")
	if !strings.Contains(out, "worker: claude") {
		t.Errorf("out = %q", out)
	}
	dispatch(t, f, "/worker gemini")
	if f.worker != "gemini" {
		t.Errorf("worker = %q, want gemini", f.worker)
	}

	f = &fakeShellBackend{worker: "claude", workerErr: errors.New("not detected")}
	_, errw, _ := dispatch(t, f, "/worker ghost")
	if f.worker != "claude" || !strings.Contains(errw, "not detected") {
		t.Errorf("failed switch must keep the old worker and print the error; worker=%q errw=%q", f.worker, errw)
	}
}

func TestDispatch_ModelPinShowClear(t *testing.T) {
	f := &fakeShellBackend{}
	out, _, _ := dispatch(t, f, "/model")
	if !strings.Contains(out, "(provider default)") {
		t.Errorf("out = %q", out)
	}
	dispatch(t, f, "/model claude-fable-5")
	if f.model != "claude-fable-5" {
		t.Errorf("model = %q", f.model)
	}
	dispatch(t, f, "/model default")
	if f.model != "" {
		t.Errorf("model should be cleared, got %q", f.model)
	}
}

func TestDispatch_ToolsSetAndClear(t *testing.T) {
	f := &fakeShellBackend{}
	dispatch(t, f, "/tools Read,Bash Edit")
	want := []string{"Read", "Bash", "Edit"}
	if len(f.tools) != len(want) {
		t.Fatalf("tools = %v, want %v", f.tools, want)
	}
	for i := range want {
		if f.tools[i] != want[i] {
			t.Errorf("tools[%d] = %q, want %q", i, f.tools[i], want[i])
		}
	}
	dispatch(t, f, "/tools none")
	if len(f.tools) != 0 {
		t.Errorf("tools should be cleared, got %v", f.tools)
	}
}

func TestDispatch_ModeShowSwitchClear(t *testing.T) {
	f := &fakeShellBackend{}
	out, _, _ := dispatch(t, f, "/mode")
	if !strings.Contains(out, "(none)") {
		t.Errorf("out = %q", out)
	}
	dispatch(t, f, "/mode deep-research")
	if f.mode != "deep-research" {
		t.Errorf("mode = %q", f.mode)
	}
	dispatch(t, f, "/mode none")
	if f.mode != "" {
		t.Errorf("mode should be cleared, got %q", f.mode)
	}
}

func TestDispatch_UnknownCommandHintsHelp(t *testing.T) {
	f := &fakeShellBackend{}
	_, errw, _ := dispatch(t, f, "/frobnicate")
	if !strings.Contains(errw, "unknown command /frobnicate") || !strings.Contains(errw, "/help") {
		t.Errorf("errw = %q", errw)
	}
}

func TestDispatch_HelpMentionsEveryCommand(t *testing.T) {
	f := &fakeShellBackend{}
	out, _, _ := dispatch(t, f, "/help")
	for _, want := range []string{"@<agent>", "/new", "/use", "/sessions", "/agents", "/worker", "/model", "/tools", "/mode", "/status", "/cd", "/exit", "!<command>"} {
		if !strings.Contains(out, want) {
			t.Errorf("help is missing %q", want)
		}
	}
	if !strings.Contains(out, "one of four things") {
		t.Errorf("help should open with the abstract input-model overview, got %q", out)
	}
}

func TestDispatch_CdWithoutArgShowsWorkdir(t *testing.T) {
	f := &fakeShellBackend{workdir: "/tmp/proj"}
	out, _, _ := dispatch(t, f, "/cd")
	if !strings.Contains(out, "workdir: /tmp/proj") {
		t.Errorf("out = %q", out)
	}
	f = &fakeShellBackend{}
	out, _, _ = dispatch(t, f, "/cd")
	if !strings.Contains(out, "(current directory)") {
		t.Errorf("empty workdir should render as current directory, got %q", out)
	}
}

func TestUtaShell_StatusIncludesTools(t *testing.T) {
	s := &utaShell{app: &App{}, worker: "claude", models: map[string]string{}, preApprove: []string{"Read", "Bash"}}
	var b strings.Builder
	s.Status(&b)
	if !strings.Contains(b.String(), "tools:   Read, Bash") {
		t.Errorf("status = %q", b.String())
	}
}

func TestShellLoop_ScriptedSession(t *testing.T) {
	f := &fakeShellBackend{worker: "claude"}
	in := strings.NewReader("summarize the repo\n/worker gemini\nnow deeper\n/exit\nafter exit\n")
	var out, errw strings.Builder

	if err := shellLoop(context.Background(), in, &out, &errw, f); err != nil {
		t.Fatalf("shellLoop: %v", err)
	}
	if len(f.turns) != 2 || f.turns[0] != "summarize the repo" || f.turns[1] != "now deeper" {
		t.Errorf("turns = %v", f.turns)
	}
	if f.worker != "gemini" {
		t.Errorf("worker = %q, want gemini", f.worker)
	}
}

func TestShellLoop_EOFExitsCleanly(t *testing.T) {
	f := &fakeShellBackend{}
	var out strings.Builder
	if err := shellLoop(context.Background(), strings.NewReader(""), &out, io.Discard, f); err != nil {
		t.Fatalf("EOF should exit cleanly, got %v", err)
	}
	if !strings.Contains(out.String(), "uta> ") {
		t.Errorf("prompt should have been written, out = %q", out.String())
	}
}

func TestShellLoop_BlankLinesReprompt(t *testing.T) {
	f := &fakeShellBackend{}
	var out strings.Builder
	if err := shellLoop(context.Background(), strings.NewReader("\n   \n"), &out, io.Discard, f); err != nil {
		t.Fatalf("shellLoop: %v", err)
	}
	if len(f.turns) != 0 {
		t.Errorf("blank lines must not become goals, got %v", f.turns)
	}
	if got := strings.Count(out.String(), "uta> "); got != 3 {
		t.Errorf("prompt count = %d, want 3 (initial + one per blank line)", got)
	}
}

func TestShellPrompt_ReflectsAttachedSession(t *testing.T) {
	f := &fakeShellBackend{}
	if p := shellPrompt(f); p != "uta> " {
		t.Errorf("prompt = %q", p)
	}
	f.session = "1a2b3c4d"
	if p := shellPrompt(f); p != "uta [1a2b3c4d]> " {
		t.Errorf("prompt = %q", p)
	}
}

func TestShellLoop_CancelledContextStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fakeShellBackend{}
	err := shellLoop(ctx, strings.NewReader("should never run\n"), io.Discard, io.Discard, f)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(f.turns) != 0 {
		t.Errorf("no turn should run under a cancelled context, got %v", f.turns)
	}
}

func TestModelEnvVarFor(t *testing.T) {
	cases := map[string]string{"claude": "ANTHROPIC_MODEL", "gemini": "GEMINI_MODEL", "custom-wrapper": ""}
	for worker, want := range cases {
		if got := modelEnvVarFor(worker); got != want {
			t.Errorf("modelEnvVarFor(%q) = %q, want %q", worker, got, want)
		}
	}
}

func TestUtaShell_SetModelSpecForms(t *testing.T) {
	s := &utaShell{worker: "claude", models: map[string]string{}}
	if err := s.SetModel("claude-fable-5"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if got := s.Model(); got != "ANTHROPIC_MODEL=claude-fable-5" {
		t.Errorf("Model() = %q", got)
	}

	// Unknown provider: bare names are rejected, VAR=value passes through.
	s.worker = "custom-wrapper"
	if err := s.SetModel("some-model"); err == nil {
		t.Error("bare model name for an unknown provider should error")
	}
	if err := s.SetModel("MY_MODEL=some-model"); err != nil {
		t.Fatalf("explicit spec: %v", err)
	}
	if got := s.Model(); got != "MY_MODEL=some-model" {
		t.Errorf("Model() = %q", got)
	}

	// Pins are per worker: switching back shows the claude pin, clearing
	// removes only the active worker's.
	s.worker = "claude"
	s.ClearModel()
	if got := s.Model(); got != "" {
		t.Errorf("Model() after clear = %q", got)
	}
	s.worker = "custom-wrapper"
	if got := s.Model(); got != "MY_MODEL=some-model" {
		t.Errorf("other worker's pin should survive, got %q", got)
	}
}
