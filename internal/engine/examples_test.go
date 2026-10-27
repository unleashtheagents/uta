package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/unleashtheagents/uta/internal/provider"
	"github.com/unleashtheagents/uta/internal/steer"
)

// loadExample reads, parses, and checks a shipped examples/*.steer file so
// these tests execute exactly what users download — not a copy that can
// drift from the real thing.
func loadExample(t *testing.T, name string) *steer.Program {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "examples", name))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	prog, d := steer.Parse(name, string(src))
	if d != nil {
		t.Fatalf("parse: %s", d.Render())
	}
	if diags := steer.Check(prog, string(src)); steer.HasErrors(diags) {
		t.Fatalf("check: %v", diags)
	}
	return prog
}

// scriptedDeps builds test deps with a fake claude provider whose replies
// are routed by prompt content, counting every call.
func scriptedDeps(t *testing.T, respond func(prompt string) string) (Deps, *int64) {
	t.Helper()
	deps := newTestDeps(t)
	calls := new(int64)
	prov := &fakeProvider{name: "claude", run: func(_ context.Context, prompt string, _ provider.RunOptions, _ chan<- provider.Event) (provider.RunResult, error) {
		atomic.AddInt64(calls, 1)
		return provider.RunResult{FinalText: respond(prompt), TokensIn: 10, TokensOut: 10}, nil
	}}
	if err := deps.Registry.Register(prov, false); err != nil {
		t.Fatal(err)
	}
	return deps, calls
}

// TestExamples_ExecuteAgainstScriptedProviders is the M6 accept criterion:
// every shipped examples/*.steer runs end-to-end through RunMission against
// scripted providers with zero network — executable regressions, not parse
// fixtures. gated.steer and discovery.steer are exercised separately below:
// the first's contract is suspension at the gate, the second's replies must
// vary by round.
func TestExamples_ExecuteAgainstScriptedProviders(t *testing.T) {
	cases := []struct {
		file      string
		respond   func(prompt string) string
		wantCalls int64
		wantFinal string // substring of the emitted value
	}{
		{
			file:      "hello.steer",
			respond:   func(string) string { return "hello, world — glad you're here." },
			wantCalls: 1, // one greet call
			wantFinal: "hello, world",
		},
		{
			file: "fanout.steer",
			respond: func(prompt string) string {
				if strings.Contains(prompt, "Synthesize them") {
					return "one prioritized paragraph"
				}
				return "finding from a scan branch"
			},
			wantCalls: 4, // three par scan branches + one digest
			wantFinal: "one prioritized paragraph",
		},
		{
			file: "judge.steer",
			respond: func(prompt string) string {
				if strings.Contains(prompt, "Try hard to refute") {
					return "Could not break it.\nSTANDS"
				}
				return "release is ready: tests pass and the budget holds"
			},
			wantCalls: 4, // one assess + three refute verifiers
			// 3 of 3 stand ≥ require 2 → the judged claim passes through unchanged.
			wantFinal: "release is ready",
		},
		{
			file: "records.steer",
			respond: func(prompt string) string {
				if strings.Contains(prompt, "Pick the single most urgent one") {
					return `{"title":"flaky retry","detail":"errors are swallowed","blocking":true}`
				}
				return `[{"title":"flaky retry","detail":"errors are swallowed","blocking":true},{"title":"naming","detail":"minor","blocking":false}]`
			},
			wantCalls: 2, // inspect + triage
			wantFinal: "flaky retry",
		},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			deps, calls := scriptedDeps(t, tc.respond)
			res, err := New(deps).RunMission(context.Background(), MissionRequest{
				Program:    loadExample(t, tc.file),
				SourceFile: tc.file,
				Available:  []string{"claude"}, // any(claude, gemini) resolves to claude
			})
			if err != nil {
				t.Fatalf("RunMission: %v", err)
			}
			if res.Status != "completed" {
				t.Errorf("status = %q", res.Status)
			}
			if res.FinalAnswer == "" || !strings.Contains(res.FinalAnswer, tc.wantFinal) {
				t.Errorf("final = %q, want it to contain %q", res.FinalAnswer, tc.wantFinal)
			}
			if got := atomic.LoadInt64(calls); got != tc.wantCalls {
				t.Errorf("provider calls = %d, want %d", got, tc.wantCalls)
			}
		})
	}
}

// TestExamples_GatedSuspendsAtGate covers gated.steer, whose contract is
// different: the walk must suspend at `gate human` in a re-enterable
// awaiting_signature state after paying for the draft only.
func TestExamples_GatedSuspendsAtGate(t *testing.T) {
	deps, calls := scriptedDeps(t, func(string) string { return "a three-sentence draft" })
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program:       loadExample(t, "gated.steer"),
		SourceFile:    "gated.steer",
		DefaultWorker: "claude", // gated.steer's agents declare no worker clause
		Available:     []string{"claude"},
		HITLStateDir:  t.TempDir(),
	})
	if !errors.Is(err, ErrMissionSuspended) {
		t.Fatalf("err = %v, want ErrMissionSuspended", err)
	}
	if res.Status != "awaiting_signature" {
		t.Errorf("status = %q", res.Status)
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("provider calls = %d, want 1 (draft only — polish is behind the gate)", got)
	}
}

// TestExamples_DiscoveryLoopExitsDryAndEmits covers discovery.steer, the M4
// example: the until dry(2) loop must exit on two consecutive dry rounds
// (a verbatim repeat, then a blank), the retry-wrapped summarize call must
// see the accumulated yield, and the final emit must land. The scripted
// budget is never approached, so a "completed" status plus the exact call
// count proves the loop ended dry, not on the cap.
func TestExamples_DiscoveryLoopExitsDryAndEmits(t *testing.T) {
	var scoutCalls int64
	deps, calls := scriptedDeps(t, func(prompt string) string {
		if strings.Contains(prompt, "accumulated yield") {
			return `{"headline":"two distinct gaps in error handling","count":2}`
		}
		switch atomic.AddInt64(&scoutCalls, 1) {
		case 1:
			return "errors are swallowed in the retry path"
		case 2:
			return "timeouts are not surfaced to the caller"
		case 3: // verbatim repeat of the previous round = dry round 1
			return "timeouts are not surfaced to the caller"
		default: // blank = dry round 2 → the loop exits
			return ""
		}
	})
	res, err := New(deps).RunMission(context.Background(), MissionRequest{
		Program:    loadExample(t, "discovery.steer"),
		SourceFile: "discovery.steer",
		Available:  []string{"claude"},
	})
	if err != nil {
		t.Fatalf("RunMission: %v", err)
	}
	if res.Status != "completed" {
		t.Errorf("status = %q", res.Status)
	}
	// Four scout rounds (yield, yield, repeat, blank) + one summarize.
	if got := atomic.LoadInt64(calls); got != 5 {
		t.Errorf("provider calls = %d, want 5", got)
	}
	if !strings.Contains(res.FinalAnswer, "two distinct gaps") {
		t.Errorf("final = %q, want the summarized report emitted", res.FinalAnswer)
	}
}
