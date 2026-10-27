package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/paths"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
)

func writeMissionScript(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "responses.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// runMissionTestCmd executes `uta mission test <steer> --script <yaml>` and
// returns stdout, stderr, and the command error.
func runMissionTestCmd(t *testing.T, steerPath, scriptPath string) (string, string, error) {
	t.Helper()
	cmd := newMissionTestCmd()
	var out, errw bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errw)
	cmd.SetArgs([]string{steerPath, "--script", scriptPath})
	err := cmd.Execute()
	return out.String(), errw.String(), err
}

func TestMissionTest_PassWithExpect(t *testing.T) {
	steerPath := writeSteer(t, missionTestSrc) // greet fn, worker any(claude, gemini)
	script := writeMissionScript(t, `
responses:
  greet:
    - "hello, world"
expect: "hello, world"
`)
	out, errw, err := runMissionTestCmd(t, steerPath, script)
	if err != nil {
		t.Fatalf("mission test failed: %v\nstderr: %s", err, errw)
	}
	if !strings.Contains(out, "ok: mission hello_world") || !strings.Contains(out, "1 call(s) served") {
		t.Errorf("output = %q", out)
	}
}

func TestMissionTest_ScalarResponseAndNoExpect(t *testing.T) {
	steerPath := writeSteer(t, missionTestSrc)
	script := writeMissionScript(t, "responses:\n  greet: \"hi there\"\n")
	out, errw, err := runMissionTestCmd(t, steerPath, script)
	if err != nil {
		t.Fatalf("mission test failed: %v\nstderr: %s", err, errw)
	}
	if !strings.Contains(out, "status=completed") {
		t.Errorf("output = %q", out)
	}
}

func TestMissionTest_ExpectMismatch(t *testing.T) {
	steerPath := writeSteer(t, missionTestSrc)
	script := writeMissionScript(t, `
responses:
  greet: "goodbye"
expect: "hello, world"
`)
	_, errw, err := runMissionTestCmd(t, steerPath, script)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("want exit 1 on emit mismatch, got %v", err)
	}
	if !strings.Contains(errw, "emit mismatch") || !strings.Contains(errw, `"goodbye"`) || !strings.Contains(errw, `"hello, world"`) {
		t.Errorf("stderr should show want/got: %s", errw)
	}
}

func TestMissionTest_MissingScriptEntry(t *testing.T) {
	steerPath := writeSteer(t, missionTestSrc)
	script := writeMissionScript(t, "responses: {}\n")
	_, errw, err := runMissionTestCmd(t, steerPath, script)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 4 {
		t.Fatalf("want exit 4 on unscripted call, got %v", err)
	}
	if !strings.Contains(errw, `no scripted response for agent fn "greet"`) {
		t.Errorf("stderr = %s", errw)
	}
}

func TestMissionTest_UnknownFnInScript(t *testing.T) {
	steerPath := writeSteer(t, missionTestSrc)
	script := writeMissionScript(t, "responses:\n  gret: \"hi\"\n")
	_, errw, err := runMissionTestCmd(t, steerPath, script)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("want exit 2 on unknown fn in script, got %v", err)
	}
	if !strings.Contains(errw, `no such agent fn`) || !strings.Contains(errw, "greet") {
		t.Errorf("stderr should name the declared fns: %s", errw)
	}
}

// A chained mission threads scripted values between calls, and a per-fn
// list serves successive calls in order.
func TestMissionTest_SuccessiveCallsServeListInOrder(t *testing.T) {
	src := `
agent fn step(x: Text) -> Text
  prompt """STEP ${x}"""

mission chain {
  budget 10k tokens
  let a = step("one")
  let b = step(a)
  emit b
}
`
	steerPath := writeSteer(t, src)
	script := writeMissionScript(t, `
responses:
  step:
    - "first"
    - "second"
expect: "second"
`)
	out, errw, err := runMissionTestCmd(t, steerPath, script)
	if err != nil {
		t.Fatalf("mission test failed: %v\nstderr: %s", err, errw)
	}
	if !strings.Contains(out, "2 call(s) served") {
		t.Errorf("output = %q", out)
	}
}

// A script that provisions more responses than the mission consumes fails
// the run, naming the fn and the declared/consumed counts — a leftover
// response usually means a mission edit dropped a call the script expected.
func TestMissionTest_UnconsumedResponsesFail(t *testing.T) {
	steerPath := writeSteer(t, missionTestSrc)
	script := writeMissionScript(t, `
responses:
  greet:
    - "hello, world"
    - "never served"
`)
	_, errw, err := runMissionTestCmd(t, steerPath, script)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 {
		t.Fatalf("want exit 3 on unconsumed responses, got %v", err)
	}
	if !strings.Contains(errw, `script declared 2 response(s) for fn "greet" but only 1 were consumed`) {
		t.Errorf("stderr should name the fn and counts: %s", errw)
	}
}

// A failed call-count assertion is a test failure with a want/got line.
func TestMissionTest_AssertCallCountMismatch(t *testing.T) {
	steerPath := writeSteer(t, missionTestSrc)
	script := writeMissionScript(t, `
responses:
  greet: "hello, world"
assert:
  calls:
    greet: 2
`)
	_, errw, err := runMissionTestCmd(t, steerPath, script)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("want exit 1 on assertion failure, got %v", err)
	}
	if !strings.Contains(errw, "assertion(s) failed") || !strings.Contains(errw, "calls[greet]: want 2, got 1") {
		t.Errorf("stderr should show want/got: %s", errw)
	}
}

// The shipped gated example fires its gate exactly once (auto-approved),
// which the assert block can pin alongside call counts and cost.
func TestMissionTest_AssertGatesFiredAgainstGatedExample(t *testing.T) {
	steerPath := filepath.Join("..", "..", "examples", "gated.steer")
	script := writeMissionScript(t, `
responses:
  draft: "a draft"
  polish: "the polished announcement"
expect: "the polished announcement"
assert:
  calls:
    draft: 1
    polish: 1
  gates_fired: 1
  max_cost_usd: 0.01
`)
	out, errw, err := runMissionTestCmd(t, steerPath, script)
	if err != nil {
		t.Fatalf("mission test failed: %v\nstderr: %s", err, errw)
	}
	if !strings.Contains(out, "ok: mission gated_announcement") {
		t.Errorf("output = %q", out)
	}
}

// An assert.calls entry for an undeclared fn fails at load time, like a
// typoed responses entry.
func TestMissionTest_AssertUnknownFn(t *testing.T) {
	steerPath := writeSteer(t, missionTestSrc)
	script := writeMissionScript(t, `
responses:
  greet: "hi"
assert:
  calls:
    gret: 1
`)
	_, errw, err := runMissionTestCmd(t, steerPath, script)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("want exit 2 on unknown fn in assert, got %v", err)
	}
	if !strings.Contains(errw, `asserts calls for "gret"`) {
		t.Errorf("stderr = %s", errw)
	}
}

// KnownFields(true) applies inside the assert block: a typoed key is a
// load error, not a silently-skipped assertion.
func TestMissionTest_AssertTypoFieldFailsLoudly(t *testing.T) {
	steerPath := writeSteer(t, missionTestSrc)
	script := writeMissionScript(t, `
responses:
  greet: "hi"
assert:
  gates_fried: 1
`)
	_, _, err := runMissionTestCmd(t, steerPath, script)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("want exit 2 on unknown assert field, got %v", err)
	}
}

// recordMissionSession runs a mission against scripted providers into the
// UTA_HOME store — the stand-in for a production trajectory — and returns
// the recorded session ID.
func recordMissionSession(t *testing.T, home, steerPath string, responses map[string]scriptQueue) string {
	t.Helper()
	prog, _, _, ok := loadSteerProgram(steerPath, io.Discard)
	if !ok {
		t.Fatal("program failed to load")
	}
	db, err := store.Open(paths.DB(home))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	blobsDir, err := paths.Blobs(home)
	if err != nil {
		t.Fatal(err)
	}
	state := newMissionScriptState(prog, &missionScript{Responses: responses})
	registry := provider.NewRegistry()
	workers := scriptedWorkerNames(prog)
	for _, name := range workers {
		if err := registry.Register(&scriptedProvider{name: name, state: state}, true); err != nil {
			t.Fatal(err)
		}
	}
	bus := trajectory.NewBus()
	defer bus.Shutdown()
	sup := engine.New(engine.Deps{
		Store: db, Blobs: store.NewBlobs(blobsDir),
		Recorder: trajectory.NewRecorder(db), Bus: bus, Registry: registry,
	})
	res, runErr := sup.RunMission(context.Background(), engine.MissionRequest{
		Program: prog, SourceFile: filepath.Base(steerPath),
		DefaultWorker: workers[0], Available: workers,
	})
	if runErr != nil {
		t.Fatalf("recording run failed: %v", runErr)
	}
	return res.SessionID
}

// The M6 accept criterion: a recorded production trajectory replays as a
// passing regression test — the journal answers every call and the recorded
// emit is the expectation, with zero provider calls.
func TestMissionTest_FromSessionReplay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("UTA_HOME", home)
	steerPath := writeSteer(t, missionTestSrc)
	sessionID := recordMissionSession(t, home, steerPath, map[string]scriptQueue{"greet": {"hello, world"}})

	cmd := newMissionTestCmd()
	var out, errw bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errw)
	cmd.SetArgs([]string{steerPath, "--from-session", sessionID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("replay failed: %v\nstderr: %s", err, errw.String())
	}
	if !strings.Contains(out.String(), "ok: mission hello_world") || !strings.Contains(out.String(), "1 call(s) served") {
		t.Errorf("output = %q", out.String())
	}
}

// The recorded emit is a real expectation: replaying against a program whose
// mission emits something else fails the test, not just the attribution.
func TestMissionTest_FromSessionExpectGuardsEmit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("UTA_HOME", home)
	steerPath := writeSteer(t, missionTestSrc)
	sessionID := recordMissionSession(t, home, steerPath, map[string]scriptQueue{"greet": {"hello, world"}})

	// Same agent fn templates, but the mission now emits a literal that can
	// never match the recorded final answer.
	changed := strings.Replace(missionTestSrc, "emit greeting", `emit "something else"`, 1)
	changedPath := writeSteer(t, changed)

	cmd := newMissionTestCmd()
	var out, errw bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errw)
	cmd.SetArgs([]string{changedPath, "--from-session", sessionID})
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 1 {
		t.Fatalf("want exit 1 on emit mismatch, got %v\nstderr: %s", err, errw.String())
	}
	if !strings.Contains(errw.String(), "emit mismatch") {
		t.Errorf("stderr = %s", errw.String())
	}
}

// An unknown session is a load-time failure, like a malformed script.
func TestMissionTest_FromSessionUnknownID(t *testing.T) {
	t.Setenv("UTA_HOME", t.TempDir())
	steerPath := writeSteer(t, missionTestSrc)
	cmd := newMissionTestCmd()
	var out, errw bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errw)
	cmd.SetArgs([]string{steerPath, "--from-session", "no-such-session"})
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("want exit 2 on unknown session, got %v\nstderr: %s", err, errw.String())
	}
}

// Two fns whose templates share a literal prefix must still be attributed
// correctly: the matcher prefers the template with more literal text.
func TestMissionScriptState_PrefixShadowedTemplates(t *testing.T) {
	src := `
agent fn short(x: Text) -> Text
  prompt """Summarize ${x}"""
agent fn long(x: Text) -> Text
  prompt """Summarize ${x} in exactly three bullet points"""

mission m {
  budget 10k tokens
  let a = short("alpha")
  let b = long("beta")
  emit b
}
`
	steerPath := writeSteer(t, src)
	script := writeMissionScript(t, `
responses:
  short: "S"
  long: "L"
expect: "L"
`)
	if _, errw, err := runMissionTestCmd(t, steerPath, script); err != nil {
		t.Fatalf("mission test failed: %v\nstderr: %s", err, errw)
	}
}
