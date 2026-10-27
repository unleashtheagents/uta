package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/budget"
	"github.com/unleashtheagents/uta/internal/hitl"
	"github.com/unleashtheagents/uta/internal/profile"
	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/steer"
	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/trajectory"
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

// The M1 contract: a malformed response retries twice — three provider
// calls in all — then fails with the catchable typed error.
func TestRunMission_SchemaMismatchAfterRetriesIsTyped(t *testing.T) {
	deps := newTestDeps(t)
	var prompts []string
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		prompts = append(prompts, prompt)
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
	if len(prompts) != 3 {
		t.Fatalf("provider calls = %d, want 3 (original + two retries)", len(prompts))
	}
	for _, p := range prompts[1:] {
		if !strings.Contains(p, "was rejected: response is not valid JSON") {
			t.Errorf("retry prompt should carry the rejection reason:\n%s", p)
		}
	}
}

const retryMissionSrc = `
type Verdict { summary: Text, score: Int, ship: Bool }

agent fn assess(topic: Text) -> Verdict
  prompt """Assess ${topic}."""

mission typed_retry {
  budget 20k tokens
  let v = assess("the release")
      retry 2 on SchemaMismatch
  emit v
}
`

// schemaFlakyProvider answers invalid JSON for the first `badCalls` provider
// calls, then a valid Verdict. Each mission-level attempt burns three provider
// calls when every response is invalid (callOnce's internal bounded schema
// retries, schemaRetries=2), so badCalls=3 fails exactly one attempt.
func schemaFlakyProvider(badCalls int, calls *int64) *fakeProvider {
	return &fakeProvider{name: "claude", run: func(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		n := atomic.AddInt64(calls, 1)
		if n <= int64(badCalls) {
			return provider.RunResult{FinalText: "not json, sorry"}, nil
		}
		return provider.RunResult{FinalText: `{"summary":"solid","score":8,"ship":true}`}, nil
	}}
}

func TestRunMission_RetrySucceedsOnSecondAttempt(t *testing.T) {
	deps := newTestDeps(t)
	var calls int64
	if err := deps.Registry.Register(schemaFlakyProvider(3, &calls), false); err != nil {
		t.Fatal(err)
	}
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, retryMissionSrc), SourceFile: "retry.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("RunMission: %v", err)
	}
	if res.FinalAnswer != `{"score":8,"ship":true,"summary":"solid"}` {
		t.Errorf("final = %q", res.FinalAnswer)
	}
	if atomic.LoadInt64(&calls) != 4 {
		t.Errorf("provider calls = %d, want 4 (failed attempt = 3, second attempt = 1)", calls)
	}
	// Each attempt journals as its own subtask under its own structural scope.
	status := map[string]string{}
	for _, st := range res.Subtasks {
		status[st.SpecID] = st.Status
	}
	if status["t1.a1.c1-assess"] != "failed" || status["t1.a2.c1-assess"] != "completed" {
		t.Errorf("attempt subtasks = %v, want t1.a1 failed and t1.a2 completed", status)
	}
}

func TestRunMission_RetryExhaustedPropagatesTypedError(t *testing.T) {
	deps := newTestDeps(t)
	var calls int64
	if err := deps.Registry.Register(schemaFlakyProvider(1_000_000, &calls), false); err != nil {
		t.Fatal(err)
	}
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, retryMissionSrc), SourceFile: "retry.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("err = %v, want the original ErrSchemaMismatch after exhaustion", err)
	}
	if res.Status != "failed" {
		t.Errorf("status = %q", res.Status)
	}
	if atomic.LoadInt64(&calls) != 9 {
		t.Errorf("provider calls = %d, want 9 (3 attempts x 3 schema-bounded calls)", calls)
	}
}

func TestRunMission_RetryDoesNotCatchOtherErrors(t *testing.T) {
	deps := newTestDeps(t)
	var calls int64
	if err := deps.Registry.Register(schemaFlakyProvider(1_000_000, &calls), false); err != nil {
		t.Fatal(err)
	}
	src := strings.Replace(retryMissionSrc, "on SchemaMismatch", "on JudgeRejected", 1)
	_, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, src), SourceFile: "retry.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("err = %v, want ErrSchemaMismatch to propagate uncaught", err)
	}
	if atomic.LoadInt64(&calls) != 3 {
		t.Errorf("provider calls = %d, want 3 — a non-matching error must not trigger another attempt", calls)
	}
}

func TestRunMission_RetryResumeRecoversCompletedAttempt(t *testing.T) {
	deps := newTestDeps(t)
	var calls int64
	if err := deps.Registry.Register(schemaFlakyProvider(3, &calls), false); err != nil {
		t.Fatal(err)
	}
	sup := New(deps)
	req := MissionRequest{
		Program: parseMission(t, retryMissionSrc), SourceFile: "retry.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	}
	res1, err := sup.RunMission(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt64(&calls) != 4 {
		t.Fatalf("first run calls = %d", calls)
	}

	// The resumed walk restarts at the completed attempt — the failed first
	// attempt is never re-paid.
	req.ResumeSessionID = res1.SessionID
	res2, err := sup.RunMission(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&calls); got != 4 {
		t.Errorf("resume made %d extra provider calls, want 0", got-4)
	}
	if res2.FinalAnswer != res1.FinalAnswer {
		t.Errorf("replayed answer differs:\n%q\nvs\n%q", res2.FinalAnswer, res1.FinalAnswer)
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

// TestRunMission_JudgePassTagsRefinement is the M2 runtime-tagging accept
// criterion: a value that passes `judge … require K of N` carries its @k
// refinement level in the journal — the JudgeCompleted event names the
// binding and the level. This runtime tag is the truth the M5 static
// checker will merely predict.
func TestRunMission_JudgePassTagsRefinement(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		if strings.Contains(prompt, "Propose a claim") {
			return provider.RunResult{FinalText: "caching is hard"}, nil
		}
		if strings.Contains(prompt, "LENS=perf") {
			return provider.RunResult{FinalText: "Weak on latency.\nREFUTED"}, nil
		}
		return provider.RunResult{FinalText: "Solid.\nSTANDS"}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	if _, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, judgeMissionSrc), SourceFile: "verified.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	}); err != nil {
		t.Fatalf("RunMission: %v", err)
	}

	deps.Bus.Shutdown()
	var judged int
	for ev := range collected {
		if ev.Kind != trajectory.JudgeCompleted {
			continue
		}
		judged++
		p := string(ev.Payload)
		if !strings.Contains(p, `"refinement":2`) {
			t.Errorf("passed judge should carry the @k tag, payload: %s", p)
		}
		// The provenance-lattice groundwork: the lenses whose verifiers let
		// the value stand ("perf" refuted, so it is not in the set).
		if !strings.Contains(p, `"lenses":["correctness","repro"]`) {
			t.Errorf("passed judge should carry the standing lens set, payload: %s", p)
		}
		if !strings.Contains(p, `"binding":"real"`) {
			t.Errorf("judge event should name the tagged binding, payload: %s", p)
		}
	}
	if judged != 1 {
		t.Fatalf("JudgeCompleted events: got %d want 1", judged)
	}
}

// A par-for whose body is a judge produces a binding whose every item
// passed `require K of N`, so the @k refinement propagates through the
// fan-out: each branch's JudgeCompleted event journals the level and names
// the binding — the runtime truth the checker's refinementOf predicts for
// the same shape.
func TestRunMission_ParForJudgeBodyTagsRefinement(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	prov := &fakeProvider{name: "claude", run: func(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		return provider.RunResult{FinalText: "Solid.\nSTANDS"}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	src := `
agent fn refute(lens: Text, claim: Text) -> Text
  prompt """LENS=${lens} Refute: ${claim}"""

mission verified {
  budget 50k tokens
  let judged = par for c in ["claim one", "claim two"] { judge c by refute("sec"), refute("repro") require 2 of 2 }
  emit judged
}
`
	if _, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, src), SourceFile: "verified.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	}); err != nil {
		t.Fatalf("RunMission: %v", err)
	}

	deps.Bus.Shutdown()
	var judged int
	for ev := range collected {
		if ev.Kind != trajectory.JudgeCompleted {
			continue
		}
		judged++
		p := string(ev.Payload)
		if !strings.Contains(p, `"refinement":2`) {
			t.Errorf("passed branch judge should carry the @k tag, payload: %s", p)
		}
		if !strings.Contains(p, `"binding":"judged"`) {
			t.Errorf("branch judge event should name the par-for's binding, payload: %s", p)
		}
	}
	if judged != 2 {
		t.Fatalf("JudgeCompleted events: got %d want 2", judged)
	}
}

// A rejected judge produces no value, so its journal event must not carry
// a refinement tag — @k marks judge-passed values only.
func TestRunMission_JudgeRejectionCarriesNoRefinement(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		if strings.Contains(prompt, "Propose a claim") {
			return provider.RunResult{FinalText: "a bold claim"}, nil
		}
		return provider.RunResult{FinalText: "No.\nREFUTED"}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	_, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, judgeMissionSrc), SourceFile: "verified.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if !errors.Is(err, ErrJudgeRejected) {
		t.Fatalf("err = %v, want ErrJudgeRejected", err)
	}

	deps.Bus.Shutdown()
	for ev := range collected {
		if ev.Kind == trajectory.JudgeCompleted &&
			(strings.Contains(string(ev.Payload), "refinement") || strings.Contains(string(ev.Payload), "lenses")) {
			t.Errorf("rejected judge must not carry a refinement or lens tag, payload: %s", string(ev.Payload))
		}
	}
}

// Each judge verifier walk must get its own copy of the bindings, exactly
// like par-for branches do — the walks run concurrently, and this test has
// every verifier read a let-bound variable from inside its branch goroutine
// so `go test -race` trips if evalJudge ever goes back to sharing the map.
func TestRunMission_JudgeVerifierBindingsAreIsolated(t *testing.T) {
	src := `
agent fn propose(topic: Text) -> Text
  prompt """Propose a claim about ${topic}."""

agent fn refute(lens: Text, claim: Text) -> Text
  prompt """LENS=${lens} Refute: ${claim}"""

mission verified_bindings {
  budget 50k tokens
  let lens = "strict"
  let claim = propose("caching")
  let real = judge claim by refute(lens), refute(lens), refute(lens) require 2 of 3
  emit real
}
`
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
		return provider.RunResult{FinalText: "Solid.\nSTANDS"}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, src), SourceFile: "verified_bindings.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("RunMission: %v", err)
	}
	if res.FinalAnswer != "caching is hard" {
		t.Errorf("final = %q", res.FinalAnswer)
	}
	if len(verifierPrompts) != 3 {
		t.Fatalf("verifier calls = %d, want 3", len(verifierPrompts))
	}
	for _, p := range verifierPrompts {
		if !strings.Contains(p, "LENS=strict") {
			t.Errorf("let-bound lens missing from verifier prompt:\n%s", p)
		}
	}
}

// A dead verifier — its provider errored — counts toward neither K nor the
// STANDS tally (RFC open question Q3, resolved conservatively). With two of
// three verifiers standing and the third dead, `require 2 of 3` still passes.
func TestRunMission_JudgeDeadVerifierStillPassesWhenKReached(t *testing.T) {
	deps := newTestDeps(t)
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		if strings.Contains(prompt, "Propose a claim") {
			return provider.RunResult{FinalText: "caching is hard"}, nil
		}
		if strings.Contains(prompt, "LENS=perf") {
			return provider.RunResult{}, errors.New("verifier process crashed")
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
		t.Fatalf("RunMission: %v — a dead verifier must not fail a judge that reaches K", err)
	}
	if res.FinalAnswer != "caching is hard" {
		t.Errorf("final = %q", res.FinalAnswer)
	}
}

// The flip side: a dead verifier does not count toward K either. One STANDS
// plus one dead plus one REFUTED misses `require 2 of 3`, the mission fails
// with ErrJudgeRejected, and the JudgeCompleted event records the dead
// verifier under verifier_errors.
func TestRunMission_JudgeDeadVerifierDoesNotCountTowardK(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		switch {
		case strings.Contains(prompt, "Propose a claim"):
			return provider.RunResult{FinalText: "a bold claim"}, nil
		case strings.Contains(prompt, "LENS=correctness"):
			return provider.RunResult{FinalText: "Fine.\nSTANDS"}, nil
		case strings.Contains(prompt, "LENS=perf"):
			return provider.RunResult{}, errors.New("verifier process crashed")
		default:
			return provider.RunResult{FinalText: "No.\nREFUTED"}, nil
		}
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, judgeMissionSrc), SourceFile: "verified.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if !errors.Is(err, ErrJudgeRejected) {
		t.Fatalf("err = %v, want ErrJudgeRejected — a dead verifier must not count as STANDS", err)
	}
	if !strings.Contains(err.Error(), "errored=1") {
		t.Errorf("error should carry the dead-verifier tally: %v", err)
	}
	if res.Status != "failed" {
		t.Errorf("status = %q", res.Status)
	}

	deps.Bus.Shutdown()
	var judged int
	for ev := range collected {
		if ev.Kind != trajectory.JudgeCompleted {
			continue
		}
		judged++
		p := string(ev.Payload)
		if !strings.Contains(p, `"verifier_errors":1`) {
			t.Errorf("JudgeCompleted should record the dead verifier, payload: %s", p)
		}
		if !strings.Contains(p, `"stands":1`) {
			t.Errorf("JudgeCompleted should tally exactly one STANDS, payload: %s", p)
		}
	}
	if judged != 1 {
		t.Fatalf("JudgeCompleted events: got %d want 1", judged)
	}
}

// Budget exhaustion inside a verifier outranks the judge verdict: it must
// reach failMission as budget.ErrBudgetExceeded (not ErrJudgeRejected) so
// the session closes as budget_exhausted.
func TestRunMission_JudgeVerifierBudgetExhaustionPropagates(t *testing.T) {
	deps := newTestDeps(t)
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		if strings.Contains(prompt, "Propose a claim") {
			return provider.RunResult{FinalText: "caching is hard"}, nil
		}
		if strings.Contains(prompt, "LENS=perf") {
			// Blows the mission's 50k-token budget in one call.
			return provider.RunResult{FinalText: "Solid.\nSTANDS", TokensIn: 40_000, TokensOut: 20_000}, nil
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
	if !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want budget.ErrBudgetExceeded to outrank the judge verdict", err)
	}
	if errors.Is(err, ErrJudgeRejected) {
		t.Errorf("budget exhaustion must not read as a judge rejection: %v", err)
	}
	if res.Status != "budget_exhausted" {
		t.Errorf("status = %q, want budget_exhausted", res.Status)
	}
}

const failoverMissionSrc = `
agent fn ask(q: Text) -> Text
  worker any(flaky, steady)
  prompt """Q: ${q}"""

mission failover {
  budget 10k tokens
  let a = ask("first")
  let b = ask("second")
  emit b
}
`

func TestRunMission_AnyFailsOverOnInfraErrors(t *testing.T) {
	deps := newTestDeps(t)
	var flakyCalls, steadyCalls int
	flaky := &fakeProvider{name: "flaky", run: func(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		flakyCalls++
		return provider.RunResult{}, provider.ErrAuth
	}}
	steady := &fakeProvider{name: "steady", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		steadyCalls++
		return provider.RunResult{FinalText: "ok: " + prompt}, nil
	}}
	if err := deps.Registry.Register(flaky, false); err != nil {
		t.Fatal(err)
	}
	if err := deps.Registry.Register(steady, false); err != nil {
		t.Fatal(err)
	}

	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, failoverMissionSrc), SourceFile: "failover.steer",
		Available: []string{"flaky", "steady"},
	})
	if err != nil {
		t.Fatalf("RunMission: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("status = %q", res.Status)
	}
	// Call 1: flaky fails auth → failover to steady. Call 2: flaky is
	// benched, steady goes first — flaky is hit exactly once all mission.
	if flakyCalls != 1 {
		t.Errorf("flaky calls = %d, want 1 (benched after the auth failure)", flakyCalls)
	}
	if steadyCalls != 2 {
		t.Errorf("steady calls = %d, want 2", steadyCalls)
	}
	// Both attempts are on the journal: a failed row and completed rows.
	var failed, completed int
	for _, st := range res.Subtasks {
		switch st.Status {
		case "failed":
			failed++
		case "completed":
			completed++
		}
	}
	if failed != 1 || completed != 2 {
		t.Errorf("journal shape = %d failed / %d completed, want 1/2", failed, completed)
	}
}

func TestRunMission_NoFailoverOnSemanticFailure(t *testing.T) {
	deps := newTestDeps(t)
	var steadyCalls int
	flaky := &fakeProvider{name: "flaky", run: func(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		return provider.RunResult{FinalText: "I tried and failed"}, provider.ErrWorkerFailed
	}}
	steady := &fakeProvider{name: "steady", run: func(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		steadyCalls++
		return provider.RunResult{FinalText: "ok"}, nil
	}}
	if err := deps.Registry.Register(flaky, false); err != nil {
		t.Fatal(err)
	}
	if err := deps.Registry.Register(steady, false); err != nil {
		t.Fatal(err)
	}

	_, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, failoverMissionSrc), SourceFile: "failover.steer",
		Available: []string{"flaky", "steady"},
	})
	if err == nil {
		t.Fatal("semantic failure should fail the mission")
	}
	if steadyCalls != 0 {
		t.Errorf("semantic failures must not burn a second provider, steady calls = %d", steadyCalls)
	}
}

const policyMissionSrc = `
agent fn work(q: Text) -> Text
  prompt """WORK ${q}"""

mission bound {
  policy dev
  budget 10k tokens
  emit work("x")
}
`

// TestRunMission_PolicyGateDeniesAndJournals is the acceptance criterion for
// `policy P`: the bound profile's capability grants gate every tool
// invocation the mission's workers make — a denied pattern is rewritten to a
// capability_gate_denied journal event and never leaks as a SubtaskToolCall,
// exactly like mode profiles on `uta run`.
func TestRunMission_PolicyGateDeniesAndJournals(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	prov := &fakeProvider{name: "claude", run: func(_ context.Context, _ string, _ provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
		events <- provider.Event{
			Kind:    provider.EventToolCall,
			Payload: []byte(`{"name":"Bash","input":{"command":"git push origin main"}}`),
		}
		events <- provider.Event{
			Kind:    provider.EventToolCall,
			Payload: []byte(`{"name":"Read","input":{"file_path":"/tmp/x"}}`),
		}
		return provider.RunResult{FinalText: "done"}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, policyMissionSrc), SourceFile: "bound.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
		Policy: &profile.MissionProfile{Name: "dev", DeniedTools: []string{"Bash(git push*)"}},
	})
	if err != nil {
		t.Fatalf("RunMission: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("status = %q", res.Status)
	}

	deps.Bus.Shutdown()
	var denied, toolCalls int
	for ev := range collected {
		switch ev.Kind {
		case trajectory.CapabilityGateDenied:
			denied++
			if !strings.Contains(string(ev.Payload), "Bash(git push*)") {
				t.Errorf("denial payload missing matched pattern: %s", string(ev.Payload))
			}
		case trajectory.SubtaskToolCall:
			toolCalls++
			if strings.Contains(string(ev.Payload), "git push") {
				t.Errorf("denied tool call leaked through as SubtaskToolCall: %s", string(ev.Payload))
			}
		}
	}
	if denied != 1 {
		t.Fatalf("CapabilityGateDenied events: got %d want 1", denied)
	}
	if toolCalls != 1 {
		t.Fatalf("SubtaskToolCall (allowed Read) events: got %d want 1", toolCalls)
	}

	// The binding itself is on the session record.
	sess, err := deps.Store.GetSession(res.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !strings.Contains(sess.MetaJSON, `"policy":"dev"`) {
		t.Errorf("session meta should record the policy binding: %s", sess.MetaJSON)
	}
}

const withCapMissionSrc = `
agent fn work(q: Text) -> Text
  prompt """WORK ${q}"""

mission raised {
  policy dev
  budget 10k tokens
  with cap "Bash(git push*)" from policy release {
    let inside = work("inside")
  }
  emit work("outside")
}
`

// TestRunMission_WithCapRaiseIsScopedAndJournaled is the acceptance test for
// `with cap`: both calls attempt the same `git push` tool invocation against
// a policy that only allows Read. Inside the block the raise lets it through;
// after the block the same invocation is denied — and the trajectory carries
// the cap_raised / cap_dropped pair bracketing the elevated call.
func TestRunMission_WithCapRaiseIsScopedAndJournaled(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()

	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
		events <- provider.Event{
			Kind:    provider.EventToolCall,
			Payload: []byte(`{"name":"Bash","input":{"command":"git push origin main"}}`),
		}
		return provider.RunResult{FinalText: "done: " + prompt}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, withCapMissionSrc), SourceFile: "raised.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
		Policy: &profile.MissionProfile{Name: "dev", AllowedTools: []string{"Read"}},
	})
	if err != nil {
		t.Fatalf("RunMission: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("status = %q", res.Status)
	}

	deps.Bus.Shutdown()
	var raised, dropped, allowedPush, denied int
	for ev := range collected {
		switch ev.Kind {
		case trajectory.CapRaised:
			raised++
			if !strings.Contains(string(ev.Payload), "Bash(git push*)") || !strings.Contains(string(ev.Payload), "release") {
				t.Errorf("cap_raised payload missing cap/policy: %s", string(ev.Payload))
			}
		case trajectory.CapDropped:
			dropped++
			if !strings.Contains(string(ev.Payload), "Bash(git push*)") {
				t.Errorf("cap_dropped payload missing cap: %s", string(ev.Payload))
			}
		case trajectory.SubtaskToolCall:
			if strings.Contains(string(ev.Payload), "git push") {
				allowedPush++
			}
		case trajectory.CapabilityGateDenied:
			denied++
		}
	}
	if raised != 1 || dropped != 1 {
		t.Errorf("raise/drop pair = %d/%d, want 1/1", raised, dropped)
	}
	// Scoping: the push inside the block passes the gate exactly once; the
	// identical push after the block is denied exactly once.
	if allowedPush != 1 {
		t.Errorf("allowed git-push tool calls = %d, want 1 (inside the with block only)", allowedPush)
	}
	if denied != 1 {
		t.Errorf("CapabilityGateDenied events = %d, want 1 (the call after the block)", denied)
	}
}

// A program that binds a policy must not run ungated: forgetting to resolve
// the profile is a wiring bug, surfaced before any session is created.
func TestRunMission_PolicyDeclaredButUnresolvedFails(t *testing.T) {
	deps := newTestDeps(t)
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, policyMissionSrc), SourceFile: "bound.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if err == nil || !strings.Contains(err.Error(), "policy dev") {
		t.Fatalf("err = %v, want unresolved-policy error", err)
	}
	if res.SessionID != "" {
		t.Errorf("no session should be created, got %s", res.SessionID)
	}
}

const gatedMissionSrc = `
agent fn draft(q: Text) -> Text
  prompt """DRAFT ${q}"""
agent fn publish(text: Text) -> Text
  prompt """PUBLISH ${text}"""

mission gated {
  budget 10k tokens
  let d = draft("x")
  gate human "Publish this draft?"
  emit publish(d)
}
`

// gatedProvider answers DRAFT/PUBLISH prompts and counts calls per kind.
func gatedProvider(drafts, publishes *int64) *fakeProvider {
	return &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		if strings.Contains(prompt, "DRAFT") {
			atomic.AddInt64(drafts, 1)
			return provider.RunResult{FinalText: "the draft", TokensIn: 5, TokensOut: 5}, nil
		}
		atomic.AddInt64(publishes, 1)
		return provider.RunResult{FinalText: "published: " + prompt, TokensIn: 5, TokensOut: 5}, nil
	}}
}

func TestRunMission_GateApprovedByStaticApprover(t *testing.T) {
	deps := newTestDeps(t)
	var drafts, publishes int64
	if err := deps.Registry.Register(gatedProvider(&drafts, &publishes), false); err != nil {
		t.Fatal(err)
	}
	approver := &hitl.StaticApprover{Approve: true}
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, gatedMissionSrc), SourceFile: "gated.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
		HITL: approver,
	})
	if err != nil {
		t.Fatalf("RunMission: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("status = %q", res.Status)
	}
	if approver.Calls() != 1 {
		t.Errorf("approver consulted %d times, want 1", approver.Calls())
	}
	if drafts != 1 || publishes != 1 {
		t.Errorf("calls = %d drafts / %d publishes, want 1/1", drafts, publishes)
	}
	var gate bool
	for _, st := range res.Subtasks {
		if st.SpecID == "g1-gate" && st.Status == "completed" && st.Worker == "human" {
			gate = true
		}
	}
	if !gate {
		t.Errorf("no completed g1-gate subtask on the journal: %+v", res.Subtasks)
	}
}

func TestRunMission_GateDeniedByStaticApproverIsTyped(t *testing.T) {
	deps := newTestDeps(t)
	var drafts, publishes int64
	if err := deps.Registry.Register(gatedProvider(&drafts, &publishes), false); err != nil {
		t.Fatal(err)
	}
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, gatedMissionSrc), SourceFile: "gated.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
		HITL: &hitl.StaticApprover{Approve: false, Reason: "not today"},
	})
	if !errors.Is(err, ErrGateDenied) {
		t.Fatalf("err = %v, want ErrGateDenied", err)
	}
	if !strings.Contains(err.Error(), "not today") {
		t.Errorf("error should carry the denial reason: %v", err)
	}
	if res.Status != "failed" {
		t.Errorf("status = %q", res.Status)
	}
	if publishes != 0 {
		t.Errorf("publish ran %d times past a denied gate, want 0", publishes)
	}
}

// The golden path: a mission suspends at the gate, the process "restarts"
// (fresh Supervisor over the same store), a human approves out-of-band, and
// --resume-session recovers the paid call and continues past the gate.
func TestRunMission_GateSuspendsThenResumesAfterApproval(t *testing.T) {
	deps := newTestDeps(t)
	var drafts, publishes int64
	if err := deps.Registry.Register(gatedProvider(&drafts, &publishes), false); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	req := MissionRequest{
		Program: parseMission(t, gatedMissionSrc), SourceFile: "gated.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
		HITLStateDir: stateDir,
	}

	res1, err := New(deps).RunMission(context.Background(), req)
	if !errors.Is(err, ErrMissionSuspended) {
		t.Fatalf("err = %v, want ErrMissionSuspended", err)
	}
	if res1.Status != "awaiting_signature" {
		t.Errorf("status = %q, want awaiting_signature", res1.Status)
	}
	sess, err := deps.Store.GetSession(res1.SessionID)
	if err != nil || sess.Status != "awaiting_signature" {
		t.Errorf("session status = %q (%v)", sess.Status, err)
	}
	if drafts != 1 || publishes != 0 {
		t.Fatalf("calls before the gate = %d drafts / %d publishes, want 1/0", drafts, publishes)
	}

	// The pending request is on disk where `uta hitl approve` looks.
	dir := hitl.SessionDir(stateDir, res1.SessionID)
	pending, err := hitl.ReadPending(dir)
	if err != nil || pending == nil {
		t.Fatalf("ReadPending: %v %+v", err, pending)
	}
	if pending.ActionID != "g1-gate" || pending.Action != "gate_human" {
		t.Errorf("pending = %+v", pending)
	}
	if _, err := os.Stat(hitl.PendingPath(dir, pending.ActionID)); err != nil {
		t.Errorf("per-action pending file missing: %v", err)
	}

	// Human signs; a fresh Supervisor (process restart) resumes the journal.
	if err := hitl.WriteApproval(dir, pending.ActionID); err != nil {
		t.Fatal(err)
	}
	req.ResumeSessionID = res1.SessionID
	res2, err := New(deps).RunMission(context.Background(), req)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res2.Status != "completed" {
		t.Errorf("status = %q", res2.Status)
	}
	if drafts != 1 {
		t.Errorf("draft re-ran on resume (%d calls), want the journal to recover it", drafts)
	}
	if publishes != 1 {
		t.Errorf("publish calls = %d, want 1", publishes)
	}
	if !strings.Contains(res2.FinalAnswer, "published:") || !strings.Contains(res2.FinalAnswer, "the draft") {
		t.Errorf("final = %q", res2.FinalAnswer)
	}
	// The approval marker is consumed — re-resuming would ask again.
	if _, err := os.Stat(hitl.ApprovedPath(dir, pending.ActionID)); err == nil {
		t.Error("approval marker should be consumed on resume")
	}
}

const untilMissionSrc = `
agent fn find(q: Text) -> Text
  prompt """FIND ${q}"""

mission discover {
  budget 100k tokens
  until dry(2) {
    let found = find("bugs")
  }
  emit found
}
`

func TestRunMission_UntilExitsOnDryRounds(t *testing.T) {
	deps := newTestDeps(t)
	var calls int
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		calls++
		switch calls {
		case 1:
			return provider.RunResult{FinalText: "bug A", TokensIn: 5, TokensOut: 5}, nil
		case 2:
			return provider.RunResult{FinalText: "bug B", TokensIn: 5, TokensOut: 5}, nil
		case 3: // verbatim repeat of the previous round = dry
			return provider.RunResult{FinalText: "bug B", TokensIn: 5, TokensOut: 5}, nil
		default: // blank = dry
			return provider.RunResult{FinalText: "", TokensIn: 5, TokensOut: 5}, nil
		}
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, untilMissionSrc), SourceFile: "discover.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("RunMission: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("status = %q", res.Status)
	}
	// Rounds: yield, yield, repeat (dry 1), blank (dry 2) → exit.
	if calls != 4 {
		t.Errorf("provider calls = %d, want 4", calls)
	}
	// The emit after the loop sees the accumulated yield, repeats deduped.
	if res.FinalAnswer != "bug A\n\n---\n\nbug B" {
		t.Errorf("final = %q, want the two novel rounds joined", res.FinalAnswer)
	}
	// Spec ids are structural: one round scope per iteration.
	specs := map[string]bool{}
	for _, st := range res.Subtasks {
		specs[st.SpecID] = true
	}
	for _, want := range []string{"u1.r1.c1-find", "u1.r2.c1-find", "u1.r3.c1-find", "u1.r4.c1-find"} {
		if !specs[want] {
			t.Errorf("missing spec id %s in %v", want, specs)
		}
	}
}

// TestRunMission_UntilExitsOnBudgetWithPartialResults is the M4 accept
// criterion's budget half: exhaustion terminates the loop — not the
// mission — and the yield gathered before the cap tripped stays bound, so
// the following emit publishes partial results.
func TestRunMission_UntilExitsOnBudgetWithPartialResults(t *testing.T) {
	deps := newTestDeps(t)
	var calls int
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		calls++
		return provider.RunResult{FinalText: fmt.Sprintf("finding-%d", calls), TokensIn: 200, TokensOut: 200}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	src := strings.Replace(untilMissionSrc, "budget 100k tokens", "budget 1k tokens", 1)
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, src), SourceFile: "discover.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("budget exhaustion inside until must end the loop, not the mission: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("status = %q", res.Status)
	}
	// Rounds 1–2 land under the 1k cap (400 tokens each); round 3's charge
	// trips it and the loop stops there.
	if calls != 3 {
		t.Errorf("provider calls = %d, want 3", calls)
	}
	if res.FinalAnswer != "finding-1\n\n---\n\nfinding-2" {
		t.Errorf("final = %q, want the pre-cap rounds only", res.FinalAnswer)
	}
}

func TestRunMission_GateDeniedOnResumeIsTyped(t *testing.T) {
	deps := newTestDeps(t)
	var drafts, publishes int64
	if err := deps.Registry.Register(gatedProvider(&drafts, &publishes), false); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	req := MissionRequest{
		Program: parseMission(t, gatedMissionSrc), SourceFile: "gated.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
		HITLStateDir: stateDir,
	}
	res1, err := New(deps).RunMission(context.Background(), req)
	if !errors.Is(err, ErrMissionSuspended) {
		t.Fatalf("err = %v, want ErrMissionSuspended", err)
	}

	dir := hitl.SessionDir(stateDir, res1.SessionID)
	if err := hitl.WriteDenial(dir, "g1-gate", "nope, redo the draft"); err != nil {
		t.Fatal(err)
	}
	req.ResumeSessionID = res1.SessionID
	res2, err := New(deps).RunMission(context.Background(), req)
	if !errors.Is(err, ErrGateDenied) {
		t.Fatalf("err = %v, want ErrGateDenied", err)
	}
	if !strings.Contains(err.Error(), "nope, redo the draft") {
		t.Errorf("error should carry the denial reason: %v", err)
	}
	if res2.Status != "failed" {
		t.Errorf("status = %q", res2.Status)
	}
	if publishes != 0 {
		t.Errorf("publish ran %d times past a denied gate, want 0", publishes)
	}
}

// gatedDeadlineMissionSrc is the gated mission with an answer deadline on
// the gate — the M8-6 `gate human "…" within D` slice.
var gatedDeadlineMissionSrc = strings.Replace(gatedMissionSrc,
	`gate human "Publish this draft?"`, `gate human "Publish this draft?" within 1h`, 1)

// expireGateDeadline rewrites a suspended gate's pending file so its
// recorded deadline is already in the past — simulating the clock running
// out between suspension and resume without sleeping in the test.
func expireGateDeadline(t *testing.T, dir, actionID string) {
	t.Helper()
	path := hitl.PendingPath(dir, actionID)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var p hitl.Pending
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if p.Deadline == "" {
		t.Fatal("suspended gate recorded no deadline in its pending file")
	}
	p.Deadline = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Approval landing before the deadline carries the mission past the gate
// exactly like an undated gate would.
func TestRunMission_GateDeadlineApprovedBeforeExpiry(t *testing.T) {
	deps := newTestDeps(t)
	var drafts, publishes int64
	if err := deps.Registry.Register(gatedProvider(&drafts, &publishes), false); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	req := MissionRequest{
		Program: parseMission(t, gatedDeadlineMissionSrc), SourceFile: "gated.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
		HITLStateDir: stateDir,
	}
	res1, err := New(deps).RunMission(context.Background(), req)
	if !errors.Is(err, ErrMissionSuspended) {
		t.Fatalf("err = %v, want ErrMissionSuspended", err)
	}

	// The pending file journals the absolute deadline for resumers to read.
	dir := hitl.SessionDir(stateDir, res1.SessionID)
	pending, err := hitl.ReadPending(dir)
	if err != nil || pending == nil {
		t.Fatalf("ReadPending: %v %+v", err, pending)
	}
	exp, perr := time.Parse(time.RFC3339, pending.Deadline)
	if perr != nil {
		t.Fatalf("pending.Deadline = %q, want RFC3339: %v", pending.Deadline, perr)
	}
	if until := time.Until(exp); until <= 50*time.Minute || until > time.Hour {
		t.Errorf("deadline %v from now, want about 1h", until)
	}

	if err := hitl.WriteApproval(dir, "g1-gate"); err != nil {
		t.Fatal(err)
	}
	req.ResumeSessionID = res1.SessionID
	res2, err := New(deps).RunMission(context.Background(), req)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res2.Status != "completed" {
		t.Errorf("status = %q", res2.Status)
	}
	if drafts != 1 || publishes != 1 {
		t.Errorf("calls = %d drafts / %d publishes, want 1/1", drafts, publishes)
	}
}

// Resuming an unanswered gate past its deadline fails typed — GateTimedOut,
// not a re-suspension — and never runs the statements past the gate.
func TestRunMission_GateExpiredOnResumeIsTyped(t *testing.T) {
	deps := newTestDeps(t)
	var drafts, publishes int64
	if err := deps.Registry.Register(gatedProvider(&drafts, &publishes), false); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	req := MissionRequest{
		Program: parseMission(t, gatedDeadlineMissionSrc), SourceFile: "gated.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
		HITLStateDir: stateDir,
	}
	res1, err := New(deps).RunMission(context.Background(), req)
	if !errors.Is(err, ErrMissionSuspended) {
		t.Fatalf("err = %v, want ErrMissionSuspended", err)
	}

	dir := hitl.SessionDir(stateDir, res1.SessionID)
	expireGateDeadline(t, dir, "g1-gate")
	req.ResumeSessionID = res1.SessionID
	res2, err := New(deps).RunMission(context.Background(), req)
	if !errors.Is(err, ErrGateTimedOut) {
		t.Fatalf("err = %v, want ErrGateTimedOut", err)
	}
	if res2.Status != "failed" {
		t.Errorf("status = %q", res2.Status)
	}
	if publishes != 0 {
		t.Errorf("publish ran %d times past an expired gate, want 0", publishes)
	}
}

// `on GateTimedOut -> emit d; halt` compensates the expiry: the mission
// terminates cleanly with the pre-gate binding as its final answer.
func TestRunMission_HaltOnGateTimedOutClosesClean(t *testing.T) {
	deps := newTestDeps(t)
	var drafts, publishes int64
	if err := deps.Registry.Register(gatedProvider(&drafts, &publishes), false); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	src := strings.Replace(gatedDeadlineMissionSrc, "emit publish(d)",
		"emit publish(d)\n  on GateTimedOut -> emit d; halt", 1)
	req := MissionRequest{
		Program: parseMission(t, src), SourceFile: "gated.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
		HITLStateDir: stateDir,
	}
	res1, err := New(deps).RunMission(context.Background(), req)
	if !errors.Is(err, ErrMissionSuspended) {
		t.Fatalf("err = %v, want ErrMissionSuspended", err)
	}

	dir := hitl.SessionDir(stateDir, res1.SessionID)
	expireGateDeadline(t, dir, "g1-gate")
	req.ResumeSessionID = res1.SessionID
	res2, err := New(deps).RunMission(context.Background(), req)
	if err != nil {
		t.Fatalf("halt must consume the handled timeout, got %v", err)
	}
	if res2.Status != "halted" {
		t.Errorf("status = %q, want halted", res2.Status)
	}
	if res2.FinalAnswer != "the draft" {
		t.Errorf("final = %q, want the compensation emit of the bound draft", res2.FinalAnswer)
	}
	if publishes != 0 {
		t.Errorf("publish ran %d times past an expired gate, want 0", publishes)
	}
}

const compensatedMissionSrc = `
agent fn think(q: Text) -> Text
  prompt """think about ${q}"""

mission compensated {
  budget 2k tokens
  let a = think("a")
  let b = think("b")
  emit b
  on BudgetExceeded -> emit a; halt
}
`

// TestRunMission_OnBudgetExceededHandlerEmitsPartialAndHalts is the M4
// acceptance test: exhaustion triggers the handler, the compensation emit
// publishes the already-bound partial value, and halt makes the termination
// clean (nil error) while the budget_exhausted close reason is preserved.
func TestRunMission_OnBudgetExceededHandlerEmitsPartialAndHalts(t *testing.T) {
	deps := newTestDeps(t)
	busSub := deps.Bus.Subscribe(64)
	collected := make(chan trajectory.Event, 64)
	go func() {
		for ev := range busSub {
			collected <- ev
		}
		close(collected)
	}()
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		return provider.RunResult{FinalText: "partial insight", TokensIn: 900, TokensOut: 900}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, compensatedMissionSrc), SourceFile: "compensated.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("halt must consume the handled failure, got %v", err)
	}
	if res.Status != "budget_exhausted" {
		t.Errorf("status = %q, want the budget_exhausted close reason preserved", res.Status)
	}
	if res.FinalAnswer != "partial insight" {
		t.Errorf("final = %q, want the compensation emit of the bound partial value", res.FinalAnswer)
	}
	sess, gerr := deps.Store.GetSession(res.SessionID)
	if gerr != nil {
		t.Fatalf("GetSession: %v", gerr)
	}
	if sess.Status != "budget_exhausted" {
		t.Errorf("session status = %q, want budget_exhausted", sess.Status)
	}

	deps.Bus.Shutdown()
	var triggered, emits, haltedEvents int
	for ev := range collected {
		switch ev.Kind {
		case trajectory.HandlerTriggered:
			triggered++
			if !strings.Contains(string(ev.Payload), "BudgetExceeded") {
				t.Errorf("handler_triggered payload = %s", string(ev.Payload))
			}
		case trajectory.MissionEmit:
			emits++
		case trajectory.MissionHalted:
			haltedEvents++
			if !strings.Contains(string(ev.Payload), "budget_exhausted") {
				t.Errorf("mission_halted payload = %s", string(ev.Payload))
			}
		}
	}
	if triggered != 1 || haltedEvents != 1 {
		t.Errorf("handler_triggered/mission_halted = %d/%d, want 1/1", triggered, haltedEvents)
	}
	if emits != 1 {
		t.Errorf("mission_emit events = %d, want the compensation emit on the journal", emits)
	}
}

// A handler without halt compensates but does not consume the failure: the
// walk error still closes the mission, typed, with the emit surfaced.
func TestRunMission_OnHandlerWithoutHaltStillFails(t *testing.T) {
	deps := newTestDeps(t)
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		return provider.RunResult{FinalText: "partial insight", TokensIn: 900, TokensOut: 900}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	src := strings.Replace(compensatedMissionSrc, "emit a; halt", "emit a", 1)
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, src), SourceFile: "compensated.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if !errors.Is(err, budget.ErrBudgetExceeded) {
		t.Fatalf("err = %v, want the unconsumed budget failure", err)
	}
	if res.Status != "budget_exhausted" {
		t.Errorf("status = %q", res.Status)
	}
	if res.FinalAnswer != "partial insight" {
		t.Errorf("final = %q, want the compensation emit", res.FinalAnswer)
	}
}

// Halt on a non-budget failure closes the session as halted — a clean typed
// termination distinct from failed.
func TestRunMission_HaltOnGateDeniedClosesClean(t *testing.T) {
	deps := newTestDeps(t)
	var drafts, publishes int64
	if err := deps.Registry.Register(gatedProvider(&drafts, &publishes), false); err != nil {
		t.Fatal(err)
	}

	src := strings.Replace(gatedMissionSrc, "emit publish(d)",
		"emit publish(d)\n  on GateDenied -> emit d; halt", 1)
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, src), SourceFile: "gated.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
		HITL: &hitl.StaticApprover{Approve: false, Reason: "not today"},
	})
	if err != nil {
		t.Fatalf("halt must consume the handled denial, got %v", err)
	}
	if res.Status != "halted" {
		t.Errorf("status = %q, want halted", res.Status)
	}
	if res.FinalAnswer != "the draft" {
		t.Errorf("final = %q, want the compensation emit of the bound draft", res.FinalAnswer)
	}
	if publishes != 0 {
		t.Errorf("publish ran %d times past a denied gate, want 0", publishes)
	}
	sess, gerr := deps.Store.GetSession(res.SessionID)
	if gerr != nil {
		t.Fatalf("GetSession: %v", gerr)
	}
	if sess.Status != "halted" {
		t.Errorf("session status = %q, want halted", sess.Status)
	}
}

// A handler for an error that never fires is inert: the mission completes
// normally and the handler's statements never run.
func TestRunMission_OnHandlerIsInertWhenErrorNeverFires(t *testing.T) {
	deps := newTestDeps(t)
	var calls int64
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, _ string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		atomic.AddInt64(&calls, 1)
		return provider.RunResult{FinalText: "cheap thought", TokensIn: 5, TokensOut: 5}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}

	src := strings.Replace(compensatedMissionSrc, "budget 2k tokens", "budget 100k tokens", 1)
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program: parseMission(t, src), SourceFile: "compensated.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
	})
	if err != nil {
		t.Fatalf("RunMission: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("status = %q", res.Status)
	}
	if res.FinalAnswer != "cheap thought" {
		t.Errorf("final = %q, want the mission's own emit, not the handler's", res.FinalAnswer)
	}
	if calls != 2 {
		t.Errorf("provider calls = %d, want the 2 planned calls only", calls)
	}
}

// recoverResult's fallback path (no result_ref blob) must refuse a stored
// ResultText at exactly missionResultDisplayLimit — it may have been
// display-truncated — while accepting one character under, which is
// definitely the full text.
func TestRecoverResult_TruncationGuard(t *testing.T) {
	deps := newTestDeps(t)
	w := &missionWalk{sup: New(deps)}

	prompt := "the rendered prompt"
	promptRef, err := deps.Blobs.Put([]byte(prompt), "txt")
	if err != nil {
		t.Fatalf("Blobs.Put: %v", err)
	}

	atLimit := strings.Repeat("x", missionResultDisplayLimit)
	if _, ok := w.recoverResult(store.Subtask{PromptRef: promptRef, ResultText: atLimit}, prompt); ok {
		t.Error("recoverResult accepted ResultText at exactly the display limit with no blob")
	}

	underLimit := strings.Repeat("x", missionResultDisplayLimit-1)
	text, ok := w.recoverResult(store.Subtask{PromptRef: promptRef, ResultText: underLimit}, prompt)
	if !ok {
		t.Fatal("recoverResult refused untruncated ResultText one under the limit")
	}
	if text != underLimit {
		t.Errorf("recovered text length = %d, want %d", len(text), len(underLimit))
	}
}

func TestRunMission_GateChooseSuspendsAndValidatesOptionOnResume(t *testing.T) {
	deps := newTestDeps(t)
	var drafts, publishes int64
	if err := deps.Registry.Register(gatedProvider(&drafts, &publishes), false); err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	src := strings.Replace(gatedMissionSrc,
		`gate human "Publish this draft?"`,
		`gate choose "Ship or hold?" of ["ship", "hold"]`, 1)
	req := MissionRequest{
		Program: parseMission(t, src), SourceFile: "gated.steer",
		DefaultWorker: "claude", Available: []string{"claude"},
		HITLStateDir: stateDir,
	}

	res1, err := New(deps).RunMission(context.Background(), req)
	if !errors.Is(err, ErrMissionSuspended) {
		t.Fatalf("err = %v, want ErrMissionSuspended", err)
	}
	dir := hitl.SessionDir(stateDir, res1.SessionID)
	pending, err := hitl.ReadPending(dir)
	if err != nil || pending == nil {
		t.Fatalf("ReadPending: %v %+v", err, pending)
	}
	if pending.Action != "gate_choose" || len(pending.Options) != 2 || pending.Options[0] != "ship" {
		t.Fatalf("pending = %+v, want gate_choose with [ship, hold]", pending)
	}

	// Approving with an option not in the list fails with ErrGateDenied.
	if err := hitl.WriteApprovalChoice(dir, pending.ActionID, "abort"); err != nil {
		t.Fatal(err)
	}
	req.ResumeSessionID = res1.SessionID
	if _, err := New(deps).RunMission(context.Background(), req); !errors.Is(err, ErrGateDenied) {
		t.Fatalf("invalid choice err = %v, want ErrGateDenied", err)
	}

	// Re-suspend and approve with a valid choice ("ship").
	req.ResumeSessionID = ""
	res2, err := New(deps).RunMission(context.Background(), req)
	if !errors.Is(err, ErrMissionSuspended) {
		t.Fatalf("err = %v, want ErrMissionSuspended", err)
	}
	dir2 := hitl.SessionDir(stateDir, res2.SessionID)
	if err := hitl.WriteApprovalChoice(dir2, "g1-gate", "ship"); err != nil {
		t.Fatal(err)
	}
	req.ResumeSessionID = res2.SessionID
	res3, err := New(deps).RunMission(context.Background(), req)
	if err != nil {
		t.Fatalf("resume with valid option: %v", err)
	}
	if res3.Status != "completed" {
		t.Errorf("status = %q, want completed", res3.Status)
	}
}
