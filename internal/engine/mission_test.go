package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/budget"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/steer"
)

const missionHelloSrc = `
agent fn greet(name: Text) -> Text
  worker any(claude, gemini)
  costs <= 4k tokens
  prompt """
  Say "hello, ${name}" back.
  """

mission hello_world {
  budget 20k tokens, 5min
  let greeting = greet("world")
  emit greeting
}
`

func parseMission(t *testing.T, src string) *steer.Program {
	t.Helper()
	prog, d := steer.Parse("test.steer", src)
	if d != nil {
		t.Fatalf("parse: %s", d.Render())
	}
	if diags := steer.Check(prog, src); steer.HasErrors(diags) {
		t.Fatalf("check: %v", diags)
	}
	return prog
}

func TestRunMission_HelloWorld(t *testing.T) {
	deps := newTestDeps(t)
	var gotPrompt string
	prov := &fakeProvider{name: "gemini", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		gotPrompt = prompt
		return provider.RunResult{FinalText: "hello, world — glad you're here.", TokensIn: 10, TokensOut: 12}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program:    parseMission(t, missionHelloSrc),
		SourceFile: "hello.steer",
		Available:  []string{"gemini"}, // claude undetected → any(claude, gemini) resolves to gemini
	})
	if err != nil {
		t.Fatalf("RunMission: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("status = %q", res.Status)
	}
	if res.FinalAnswer != "hello, world — glad you're here." {
		t.Errorf("final answer = %q", res.FinalAnswer)
	}
	if !strings.Contains(gotPrompt, `Say "hello, world" back.`) {
		t.Errorf("prompt = %q, want ${name} interpolated", gotPrompt)
	}

	// The mission is a real session with a real subtask.
	sess, err := deps.Store.GetSession(res.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !strings.Contains(sess.Goal, "mission hello_world") || !strings.Contains(sess.MetaJSON, `"steer"`) {
		t.Errorf("session = %+v", sess)
	}
	if len(res.Subtasks) != 1 {
		t.Fatalf("subtasks = %d, want 1", len(res.Subtasks))
	}
	st := res.Subtasks[0]
	if st.Worker != "gemini" || st.Status != "completed" || st.SpecID != "c1-greet" {
		t.Errorf("subtask = %+v", st)
	}
	if !strings.Contains(st.MetaJSON, `"tokens_out":12`) {
		t.Errorf("usage meta = %q", st.MetaJSON)
	}
}

func TestRunMission_ChainedCallsThreadValues(t *testing.T) {
	deps := newTestDeps(t)
	var prompts []string
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		prompts = append(prompts, prompt)
		if len(prompts) == 1 {
			return provider.RunResult{FinalText: "bonjour"}, nil
		}
		return provider.RunResult{FinalText: "BONJOUR!"}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	src := `
agent fn translate(text: Text) -> Text
  prompt """Translate to French: ${text}"""

agent fn shout(text: Text) -> Text
  prompt """Uppercase this: ${text}"""

mission chain {
  budget 10k tokens
  let fr = translate("hello")
  let loud = shout(fr)
  emit loud
}
`
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, src), SourceFile: "chain.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("RunMission: %v", err)
	}
	if len(prompts) != 2 || !strings.Contains(prompts[1], "Uppercase this: bonjour") {
		t.Errorf("prompts = %q — second call should see the first call's result", prompts)
	}
	if res.FinalAnswer != "BONJOUR!" {
		t.Errorf("final = %q", res.FinalAnswer)
	}
}

func TestRunMission_BudgetExhaustionIsTyped(t *testing.T) {
	deps := newTestDeps(t)
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		return provider.RunResult{FinalText: "expensive", TokensIn: 900, TokensOut: 900}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	src := `
agent fn think(q: Text) -> Text
  prompt """think about ${q}"""

mission spendy {
  budget 1k tokens
  let a = think("a")
  let b = think("b")
  emit b
}
`
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, src), SourceFile: "spendy.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want budget.ErrBudgetExceeded", err)
	}
	if res.Status != "budget_exhausted" {
		t.Errorf("status = %q", res.Status)
	}
}

func TestRunMission_PerCallCostCeiling(t *testing.T) {
	deps := newTestDeps(t)
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		return provider.RunResult{FinalText: "way too big", TokensIn: 5000, TokensOut: 5000}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	src := `
agent fn tiny(q: Text) -> Text
  costs <= 1k tokens
  prompt """${q}"""

mission capped {
  budget 100k tokens
  emit tiny("hi")
}
`
	_, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, src), SourceFile: "capped.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want per-call ceiling to trip as budget.ErrBudgetExceeded", err)
	}
}

func TestRunMission_NoWorkerAvailable(t *testing.T) {
	deps := newTestDeps(t)
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, missionHelloSrc), SourceFile: "hello.steer",
		Available: nil, // nothing detected
	})
	if err == nil {
		t.Fatal("expected worker resolution failure")
	}
	if !strings.Contains(err.Error(), "greet() at test.steer:") {
		t.Errorf("error should carry the call site, got %v", err)
	}
	if !strings.Contains(err.Error(), "claude, gemini") {
		t.Errorf("error should list the declared workers, got %v", err)
	}
	if res.Status != "failed" {
		t.Errorf("status = %q", res.Status)
	}
}

func TestRunMission_DurationBudgetIsDeadline(t *testing.T) {
	deps := newTestDeps(t)
	prov := &fakeProvider{name: "claude", run: func(ctx context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		<-ctx.Done() // simulate a call that outlives the mission deadline
		return provider.RunResult{}, ctx.Err()
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	prog := parseMission(t, missionHelloSrc)
	prog.Mission.Budget.Deadline = 30 * time.Millisecond

	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: prog, SourceFile: "hello.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if err == nil || !strings.Contains(err.Error(), "duration budget exceeded") {
		t.Fatalf("err = %v, want duration-budget message", err)
	}
	if res.Status != "failed" {
		t.Errorf("status = %q", res.Status)
	}
}
