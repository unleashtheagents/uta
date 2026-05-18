package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unleashtheagents/uta/internal/provider"
)

func TestReflector_Validation_NoCriticOrTool(t *testing.T) {
	deps := newTestDeps(t)
	_, err := New(deps).RunReflector(context.Background(), ReflectorRequest{
		InputSummary:  "code",
		DefaultWorker: "worker",
	})
	if err == nil || !strings.Contains(err.Error(), "at least one critic or tool") {
		t.Fatalf("expected critic-or-tool-required error, got %v", err)
	}
}

func TestReflector_Validation_ToolsOnlyWithoutTools(t *testing.T) {
	deps := newTestDeps(t)
	_, err := New(deps).RunReflector(context.Background(), ReflectorRequest{
		InputSummary: "code",
		ToolsOnly:    true,
	})
	if err == nil || !strings.Contains(err.Error(), "tools-only mode requires") {
		t.Fatalf("expected tools-only error, got %v", err)
	}
}

func TestReflector_Validation_MissingInputSummary(t *testing.T) {
	deps := newTestDeps(t)
	_, err := New(deps).RunReflector(context.Background(), ReflectorRequest{
		DefaultWorker: "worker",
		Critics:       []CriticSpec{{ID: "c1", Prompt: "x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "input_summary is required") {
		t.Fatalf("expected input_summary error, got %v", err)
	}
}

func TestReflector_Validation_MissingDefaultWorker(t *testing.T) {
	deps := newTestDeps(t)
	_, err := New(deps).RunReflector(context.Background(), ReflectorRequest{
		InputSummary: "code",
		Critics:      []CriticSpec{{ID: "c1", Prompt: "x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "default_worker is required") {
		t.Fatalf("expected default_worker error, got %v", err)
	}
}

func TestReflector_Validation_UnregisteredDefaultWorker(t *testing.T) {
	deps := newTestDeps(t)
	_, err := New(deps).RunReflector(context.Background(), ReflectorRequest{
		InputSummary:  "code",
		Critics:       []CriticSpec{{ID: "c1", Prompt: "x"}},
		DefaultWorker: "ghost",
	})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("expected not-registered error, got %v", err)
	}
}

func TestReflector_NoFindings_StopsAtNoFindings(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{FinalText: `{"findings":[]}`}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).RunReflector(context.Background(), ReflectorRequest{
		Goal:          "audit it",
		InputSummary:  "the code under ./contracts",
		DefaultWorker: "worker",
		Critics:       []CriticSpec{{ID: "tob", Title: "tob", Prompt: "look carefully"}},
		StopWhen:      StopAtNoFindings,
		MaxIterations: 3,
	})
	if err != nil {
		t.Fatalf("RunReflector: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status: got %q want completed", res.Status)
	}
	if res.StoppedBecause != "no_findings" {
		t.Fatalf("stopped_because: got %q want no_findings", res.StoppedBecause)
	}
	if res.Iterations != 1 {
		t.Fatalf("iterations: got %d want 1", res.Iterations)
	}
	if res.FinalFindings == nil || len(res.FinalFindings.Findings) != 0 {
		t.Fatalf("expected empty findings, got %+v", res.FinalFindings)
	}
}

func TestReflector_HighFindingNoReviser_StopsAtNoReviser(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{
				FinalText: `{"findings":[{"id":"f1","severity":"high","title":"big bug"}]}`,
			}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).RunReflector(context.Background(), ReflectorRequest{
		Goal:          "audit",
		InputSummary:  "code",
		DefaultWorker: "worker",
		Critics:       []CriticSpec{{ID: "c1", Prompt: "lens"}},
		StopWhen:      StopAtNoHigh,
		MaxIterations: 3,
	})
	if err != nil {
		t.Fatalf("RunReflector: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status: got %q want completed", res.Status)
	}
	if res.StoppedBecause != "no_reviser" {
		t.Fatalf("stopped_because: got %q want no_reviser", res.StoppedBecause)
	}
	if res.Iterations != 1 {
		t.Fatalf("iterations: got %d want 1", res.Iterations)
	}
	if res.FinalFindings == nil || len(res.FinalFindings.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %+v", res.FinalFindings)
	}
	if got := res.FinalFindings.HighestSeverity(); got != SevHigh {
		t.Fatalf("highest severity: got %q want high", got)
	}
}

func TestReflector_MaxIterationsReached(t *testing.T) {
	// With StopAtMaxIterations and a reviser that runs cleanly, the loop must
	// run the full MaxIterations. Each iteration: critic → reviser → critic …
	deps := newTestDeps(t)
	var criticCalls atomic.Int32
	var reviserCalls atomic.Int32
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			// Criticism preamble starts with "You are an auditor".
			// Reviser preamble starts with "You are the reviser".
			if strings.HasPrefix(prompt, "You are the reviser") {
				reviserCalls.Add(1)
				return provider.RunResult{FinalText: "patched"}, nil
			}
			criticCalls.Add(1)
			return provider.RunResult{
				FinalText: `{"findings":[{"id":"f1","severity":"medium","title":"still here"}]}`,
			}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).RunReflector(context.Background(), ReflectorRequest{
		Goal:          "audit",
		InputSummary:  "code",
		DefaultWorker: "worker",
		Critics:       []CriticSpec{{ID: "c1", Prompt: "lens"}},
		Reviser:       &ReviserSpec{Prompt: "fix things"},
		StopWhen:      StopAtMaxIterations,
		MaxIterations: 2,
	})
	if err != nil {
		t.Fatalf("RunReflector: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("status: got %q want completed", res.Status)
	}
	if res.StoppedBecause != "max_iterations" {
		t.Fatalf("stopped_because: got %q want max_iterations", res.StoppedBecause)
	}
	if res.Iterations != 2 {
		t.Fatalf("iterations: got %d want 2", res.Iterations)
	}
	if got := criticCalls.Load(); got != 2 {
		t.Fatalf("critic calls: got %d want 2", got)
	}
	// Reviser runs after iter 1 (before iter 2 starts); after iter 2 the loop
	// stops because iter == MaxIterations, so reviser runs exactly once.
	if got := reviserCalls.Load(); got != 1 {
		t.Fatalf("reviser calls: got %d want 1", got)
	}
}

func TestReflector_CriticParseError_RecordedAsFailure(t *testing.T) {
	deps := newTestDeps(t)
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{FinalText: "the model refused to answer"}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).RunReflector(context.Background(), ReflectorRequest{
		Goal:          "audit",
		InputSummary:  "code",
		DefaultWorker: "worker",
		Critics:       []CriticSpec{{ID: "c1", Prompt: "x"}},
		MaxIterations: 1,
	})
	if err != nil {
		t.Fatalf("RunReflector: %v", err)
	}
	// Parse failure → no findings recorded for that critic. Highest severity
	// is SevUnknown; StopAtNoHigh (default) is satisfied → completed.
	if res.Status != "completed" {
		t.Fatalf("status: got %q want completed", res.Status)
	}
	if res.FinalFindings == nil || len(res.FinalFindings.Findings) != 0 {
		t.Fatalf("expected zero findings (parse failure dropped), got %+v", res.FinalFindings)
	}

	// Verify the subtask row was marked failed with error_kind=parse.
	subs, err := deps.Store.SubtaskListBySession(res.SessionID, 0, 0)
	if err != nil {
		t.Fatalf("SubtaskListBySession: %v", err)
	}
	var sawParseFailure bool
	for _, st := range subs {
		if st.Status == "failed" && st.ErrorKind == "parse" {
			sawParseFailure = true
		}
	}
	if !sawParseFailure {
		t.Fatalf("expected a subtask row with status=failed error_kind=parse, got %+v", subs)
	}
}

func TestReflector_PersistsFindingsToContextDir(t *testing.T) {
	deps := newTestDeps(t)
	ctxDir := filepath.Join(t.TempDir(), "ctx")
	worker := &fakeProvider{
		name: "worker",
		run: func(ctx context.Context, prompt string, opts provider.RunOptions, events chan<- provider.Event) (provider.RunResult, error) {
			return provider.RunResult{
				FinalText: `{"findings":[{"id":"f1","severity":"medium","title":"saved"}]}`,
			}, nil
		},
	}
	if err := deps.Registry.Register(worker, false); err != nil {
		t.Fatalf("register: %v", err)
	}

	res, err := New(deps).RunReflector(context.Background(), ReflectorRequest{
		Goal:          "audit",
		InputSummary:  "code",
		DefaultWorker: "worker",
		Critics:       []CriticSpec{{ID: "c1", Prompt: "lens"}},
		StopWhen:      StopAtNoHigh, // stops after iter 1 since highest is medium
		MaxIterations: 1,
		ContextDir:    ctxDir,
	})
	if err != nil {
		t.Fatalf("RunReflector: %v", err)
	}
	if len(res.HistoryRefs) != 1 {
		t.Fatalf("HistoryRefs: got %d want 1", len(res.HistoryRefs))
	}
	if _, err := os.Stat(res.HistoryRefs[0]); err != nil {
		t.Fatalf("history file should exist: %v", err)
	}
	latest := filepath.Join(ctxDir, "findings.json")
	if _, err := os.Stat(latest); err != nil {
		t.Fatalf("findings.json (latest mirror) should exist: %v", err)
	}
	if res.FindingsLatestRef != latest {
		t.Fatalf("FindingsLatestRef: got %q want %q", res.FindingsLatestRef, latest)
	}
}

func TestShouldStop_MaxIterations(t *testing.T) {
	rep := &FindingsReport{}
	stop, why := shouldStop(StopAtNoHigh, rep, 3, 3)
	if !stop || why != "max_iterations" {
		t.Fatalf("at-max should stop: got stop=%v why=%q", stop, why)
	}
}

func TestShouldStop_NoFindings(t *testing.T) {
	rep := &FindingsReport{}
	stop, why := shouldStop(StopAtNoFindings, rep, 1, 5)
	if !stop || why != "no_findings" {
		t.Fatalf("empty report: got stop=%v why=%q", stop, why)
	}
}

func TestShouldStop_NoMedOrAbove(t *testing.T) {
	rep := &FindingsReport{Findings: []Finding{{Severity: SevLow}}}
	stop, why := shouldStop(StopAtNoMedOrAbove, rep, 1, 5)
	if !stop || why != "no_med_or_above" {
		t.Fatalf("low only: got stop=%v why=%q", stop, why)
	}
	// With a medium finding it should keep going.
	rep2 := &FindingsReport{Findings: []Finding{{Severity: SevMedium}}}
	stop, _ = shouldStop(StopAtNoMedOrAbove, rep2, 1, 5)
	if stop {
		t.Fatalf("medium present: should not stop")
	}
}

func TestShouldStop_NoHigh(t *testing.T) {
	rep := &FindingsReport{Findings: []Finding{{Severity: SevMedium}}}
	stop, why := shouldStop(StopAtNoHigh, rep, 1, 5)
	if !stop || why != "no_high_findings" {
		t.Fatalf("medium-only: got stop=%v why=%q", stop, why)
	}
	// With a high finding it should not stop.
	rep2 := &FindingsReport{Findings: []Finding{{Severity: SevHigh}}}
	stop, _ = shouldStop(StopAtNoHigh, rep2, 1, 5)
	if stop {
		t.Fatalf("high present: should not stop")
	}
}

func TestReflectorMeta_ShapeAndKeys(t *testing.T) {
	req := ReflectorRequest{
		Critics:       []CriticSpec{{ID: "a"}, {ID: "b"}},
		MaxIterations: 5,
		StopWhen:      StopAtNoMedOrAbove,
		Reviser:       &ReviserSpec{Prompt: "fix"},
	}
	meta := reflectorMeta(req)
	expectedSubstrings := []string{
		`"strategy":"reflector"`,
		`"critics":2`,
		`"max_iterations":5`,
		`"stop_when":"no_med_or_above"`,
		`"has_reviser":true`,
	}
	for _, want := range expectedSubstrings {
		if !strings.Contains(meta, want) {
			t.Fatalf("reflectorMeta missing %q in %s", want, meta)
		}
	}
}

// Sanity: criticPreambleTmpl + reviserPreambleTmpl format with the expected
// args. Catches accidental %s drift.
func TestPreambleTemplates_FormatCleanly(t *testing.T) {
	got := fmt.Sprintf(criticPreambleTmpl, "g", "in", "wd", "lens")
	if !strings.Contains(got, "Goal of this audit: g") ||
		!strings.Contains(got, "Material under review: in") ||
		!strings.Contains(got, "Workdir for any file inspection: wd") {
		t.Fatalf("critic preamble missing expected segments: %s", got)
	}
	got = fmt.Sprintf(reviserPreambleTmpl, "g", "wd", "FINDINGS", "charter")
	if !strings.Contains(got, "Goal: g") ||
		!strings.Contains(got, "Workdir: wd") ||
		!strings.Contains(got, "FINDINGS") ||
		!strings.Contains(got, "charter") {
		t.Fatalf("reviser preamble missing expected segments: %s", got)
	}
}
