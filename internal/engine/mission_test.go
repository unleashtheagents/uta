package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
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

func TestRunMission_ResumeRecoversCompletedCalls(t *testing.T) {
	deps := newTestDeps(t)
	var callsMade []string
	fail := true
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		callsMade = append(callsMade, prompt)
		if strings.Contains(prompt, "SECOND") && fail {
			return provider.RunResult{}, provider.ErrWorkerFailed
		}
		return provider.RunResult{FinalText: "result-for: " + prompt, TokensIn: 5, TokensOut: 5}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	src := `
agent fn first(q: Text) -> Text
  prompt """FIRST ${q}"""
agent fn second(q: Text) -> Text
  prompt """SECOND ${q}"""

mission twostep {
  budget 10k tokens
  let a = first("alpha")
  let b = second(a)
  emit b
}
`
	req := MissionRequest{
		Program: parseMission(t, src), SourceFile: "twostep.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	}
	sup := New(deps)

	// First run: call 1 succeeds, call 2 fails — the mission fails but
	// call 1's payment is on the journal.
	res1, err := sup.RunMission(context.Background(), req)
	if err == nil || res1.Status != "failed" {
		t.Fatalf("first run should fail at call 2: status=%q err=%v", res1.Status, err)
	}
	if len(callsMade) != 2 {
		t.Fatalf("callsMade = %d, want 2", len(callsMade))
	}

	// Resume: call 1 is recovered (no provider hit), call 2 re-runs.
	fail = false
	callsMade = nil
	req.ResumeSessionID = res1.SessionID
	res2, err := sup.RunMission(context.Background(), req)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res2.Status != "completed" {
		t.Errorf("status = %q", res2.Status)
	}
	if len(callsMade) != 1 || !strings.Contains(callsMade[0], "SECOND") {
		t.Errorf("only the frontier should re-run, got %q", callsMade)
	}
	if len(res2.Subtasks) != 2 {
		t.Fatalf("subtasks = %d, want 2 (recovered + rerun)", len(res2.Subtasks))
	}
	var recoveredTitles int
	for _, st := range res2.Subtasks {
		if strings.Contains(st.Title, "(recovered)") {
			recoveredTitles++
		}
	}
	if recoveredTitles != 1 {
		t.Errorf("recovered subtasks = %d, want 1", recoveredTitles)
	}
}

func TestRunMission_ResumeReRunsWhenPromptChanged(t *testing.T) {
	deps := newTestDeps(t)
	var calls int
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		calls++
		return provider.RunResult{FinalText: "ok"}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}
	sup := New(deps)

	src := `
agent fn f(q: Text) -> Text
  prompt """do ${q}"""
mission m {
  budget 10k tokens
  emit f("one thing")
}
`
	req := MissionRequest{
		Program: parseMission(t, src), SourceFile: "m.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	}
	res1, err := sup.RunMission(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}

	// Edit the program: same call shape, different argument → different
	// prompt → the journal entry must NOT be recovered.
	edited := strings.Replace(src, `"one thing"`, `"another thing"`, 1)
	req.Program = parseMission(t, edited)
	req.ResumeSessionID = res1.SessionID
	if _, err := sup.RunMission(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("provider calls = %d, want 2 (no false recovery)", calls)
	}
}

const parMissionSrc = `
agent fn scan(lens: Text) -> Text
  prompt """Scan through the ${lens} lens."""

mission sweep {
  budget 50k tokens
  let raw = par for lens in ["security", "perf", "style"] { scan(lens) }
  emit raw
}
`

func TestRunMission_ParForRunsBranchesConcurrently(t *testing.T) {
	deps := newTestDeps(t)
	var mu sync.Mutex
	var prompts []string
	inflight, peak := 0, 0
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		mu.Lock()
		prompts = append(prompts, prompt)
		inflight++
		if inflight > peak {
			peak = inflight
		}
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		inflight--
		mu.Unlock()
		return provider.RunResult{FinalText: "found: " + prompt}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, parMissionSrc), SourceFile: "sweep.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("RunMission: %v", err)
	}
	if len(prompts) != 3 {
		t.Fatalf("prompts = %d, want 3", len(prompts))
	}
	if peak < 2 {
		t.Errorf("peak concurrency = %d, want >= 2 (branches must overlap)", peak)
	}
	// Results join in item order regardless of completion order.
	wantOrder := []string{"security", "perf", "style"}
	last := -1
	for _, lens := range wantOrder {
		idx := strings.Index(res.FinalAnswer, lens)
		if idx < 0 || idx < last {
			t.Fatalf("answer out of item order:\n%s", res.FinalAnswer)
		}
		last = idx
	}
	// Spec ids are structural, not scheduling-dependent.
	specs := map[string]bool{}
	for _, st := range res.Subtasks {
		specs[st.SpecID] = true
	}
	for _, want := range []string{"p1.b1.c1-scan", "p1.b2.c1-scan", "p1.b3.c1-scan"} {
		if !specs[want] {
			t.Errorf("missing spec id %s in %v", want, specs)
		}
	}
}

func TestRunMission_ParForResumeRecoversAllBranches(t *testing.T) {
	deps := newTestDeps(t)
	var calls int64
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		atomic.AddInt64(&calls, 1)
		return provider.RunResult{FinalText: "found: " + prompt}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}
	sup := New(deps)
	req := MissionRequest{
		Program: parseMission(t, parMissionSrc), SourceFile: "sweep.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	}
	res1, err := sup.RunMission(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&calls) != 3 {
		t.Fatalf("first run calls = %d", calls)
	}

	req.ResumeSessionID = res1.SessionID
	res2, err := sup.RunMission(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&calls) != 3 {
		t.Errorf("resume made %d extra provider calls, want 0", calls-3)
	}
	if res2.FinalAnswer != res1.FinalAnswer {
		t.Errorf("replayed answer differs:\n%q\nvs\n%q", res2.FinalAnswer, res1.FinalAnswer)
	}
}

const typedMissionSrc = `
type Verdict { summary: Text, score: Int, ship: Bool }

agent fn assess(topic: Text) -> Verdict
  prompt """Assess ${topic}."""

mission typed {
  budget 10k tokens
  emit assess("the release")
}
`

func TestRunMission_SchemaValidatedWithRetry(t *testing.T) {
	deps := newTestDeps(t)
	var prompts []string
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		prompts = append(prompts, prompt)
		if len(prompts) == 1 {
			return provider.RunResult{FinalText: "Sure! Here you go: it looks great."}, nil
		}
		return provider.RunResult{FinalText: "```json\n{\"summary\":\"solid\",\"score\":8,\"ship\":true}\n```"}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, typedMissionSrc), SourceFile: "typed.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("RunMission: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("provider calls = %d, want 2 (original + one retry)", len(prompts))
	}
	if !strings.Contains(prompts[0], "Respond ONLY with a single JSON Verdict object") {
		t.Errorf("schema instruction missing from prompt:\n%s", prompts[0])
	}
	if !strings.Contains(prompts[1], "was rejected: response is not valid JSON") {
		t.Errorf("retry prompt should carry the rejection reason:\n%s", prompts[1])
	}
	// The value that flows onward is the normalized JSON, fences stripped.
	if res.FinalAnswer != `{"score":8,"ship":true,"summary":"solid"}` {
		t.Errorf("final = %q", res.FinalAnswer)
	}
}

func TestRunMission_SchemaMismatchAfterRetryIsTyped(t *testing.T) {
	deps := newTestDeps(t)
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		return provider.RunResult{FinalText: "I refuse to emit JSON."}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, typedMissionSrc), SourceFile: "typed.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("err = %v, want ErrSchemaMismatch", err)
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

const judgeMissionSrc = `
agent fn propose(topic: Text) -> Text
  prompt """Propose a claim about ${topic}."""

agent fn refute(lens: Text, claim: Text) -> Text
  prompt """LENS=${lens} Refute: ${claim}"""

mission verified {
  budget 50k tokens
  let claim = propose("caching")
  let real = judge claim by refute("correctness"), refute("perf"), refute("repro") require 2 of 3
  emit real
}
`

func TestRunMission_JudgePassesKofN(t *testing.T) {
	deps := newTestDeps(t)
	var mu sync.Mutex
	var verifierPrompts []string
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(prompt, "Propose a claim") {
			return provider.RunResult{FinalText: "caching is hard"}, nil
		}
		verifierPrompts = append(verifierPrompts, prompt)
		if strings.Contains(prompt, "LENS=perf") {
			return provider.RunResult{FinalText: "Weak on latency.\nREFUTED"}, nil
		}
		return provider.RunResult{FinalText: "Solid.\nSTANDS"}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, judgeMissionSrc), SourceFile: "verified.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("RunMission: %v", err)
	}
	// 2 of 3 stand → the judged value passes through unchanged.
	if res.FinalAnswer != "caching is hard" {
		t.Errorf("final = %q", res.FinalAnswer)
	}
	if len(verifierPrompts) != 3 {
		t.Fatalf("verifier calls = %d, want 3", len(verifierPrompts))
	}
	for _, p := range verifierPrompts {
		if !strings.Contains(p, "Refute: caching is hard") {
			t.Errorf("judged value missing from verifier prompt:\n%s", p)
		}
		if !strings.Contains(p, "STANDS") || !strings.Contains(p, "REFUTED") {
			t.Errorf("verdict contract missing from verifier prompt:\n%s", p)
		}
	}
}

func TestRunMission_JudgeRejectionIsTyped(t *testing.T) {
	deps := newTestDeps(t)
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		if strings.Contains(prompt, "Propose a claim") {
			return provider.RunResult{FinalText: "a bold claim"}, nil
		}
		if strings.Contains(prompt, "LENS=correctness") {
			return provider.RunResult{FinalText: "Fine.\nSTANDS"}, nil
		}
		return provider.RunResult{FinalText: "No.\nREFUTED"}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, judgeMissionSrc), SourceFile: "verified.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if !errors.Is(err, ErrJudgeRejected) {
		t.Fatalf("err = %v, want ErrJudgeRejected", err)
	}
	if !strings.Contains(err.Error(), "1 of 3") || !strings.Contains(err.Error(), "require 2") {
		t.Errorf("error should carry the tally: %v", err)
	}
	if res.Status != "failed" {
		t.Errorf("status = %q", res.Status)
	}
}
