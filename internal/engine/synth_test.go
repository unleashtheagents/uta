package engine

import (
	"strings"
	"testing"
)

func TestJoinOutcomesWithWorker(t *testing.T) {
	outcomes := []SubtaskOutcome{
		{ID: "s1", Title: "Find bugs", Worker: "claude", Result: "no bugs"},
		{ID: "s2", Title: "Lint code", Worker: "gemini", Result: "looks good"},
	}
	got := joinOutcomes(outcomes)
	wantSubs := []string{
		"## Find bugs (s1) — worker: claude",
		"no bugs",
		"## Lint code (s2) — worker: gemini",
		"looks good",
	}
	for _, s := range wantSubs {
		if !strings.Contains(got, s) {
			t.Errorf("joinOutcomes output missing %q\nfull output:\n%s", s, got)
		}
	}
	// Result should be trimmed of trailing whitespace.
	if strings.HasSuffix(got, "\n") {
		t.Errorf("joinOutcomes output should be trimmed, got trailing newline")
	}
}

func TestJoinOutcomesWithoutWorker(t *testing.T) {
	outcomes := []SubtaskOutcome{
		{ID: "s1", Title: "Alpha", Result: "alpha body"},
	}
	got := joinOutcomes(outcomes)
	if !strings.Contains(got, "## Alpha (s1)\n") {
		t.Errorf("expected header without worker attribution, got:\n%s", got)
	}
	if strings.Contains(got, "worker:") {
		t.Errorf("did not expect worker attribution in header, got:\n%s", got)
	}
	if !strings.Contains(got, "alpha body") {
		t.Errorf("expected result body in output, got:\n%s", got)
	}
}

func TestJoinOutcomesEmpty(t *testing.T) {
	if got := joinOutcomes(nil); got != "" {
		t.Errorf("expected empty output for nil, got %q", got)
	}
	if got := joinOutcomes([]SubtaskOutcome{}); got != "" {
		t.Errorf("expected empty output for empty slice, got %q", got)
	}
}

func TestRenderSynthPromptIncludesHeaderAndGoal(t *testing.T) {
	got := RenderSynthPrompt("  Build a thing  ", nil)
	if !strings.HasPrefix(got, synthPromptHeader) {
		t.Errorf("expected output to start with synthPromptHeader")
	}
	if !strings.Contains(got, "Original goal:\nBuild a thing") {
		t.Errorf("expected trimmed goal in output, got:\n%s", got)
	}
	if !strings.HasSuffix(got, "Now write the final answer.") {
		t.Errorf("expected trailing instruction, got tail:\n%q", got[len(got)-50:])
	}
}

func TestRenderSynthPromptMarkers(t *testing.T) {
	outcomes := []SubtaskOutcome{
		{ID: "s1", Title: "OK task", Result: "  result one  "},
		{ID: "s2", Title: "Bad task", Result: "boom", Failed: true},
		{ID: "s3", Title: "Skip task", Result: "n/a", Skipped: true},
		// Skipped takes precedence over Failed if both are set.
		{ID: "s4", Title: "Both", Result: "x", Failed: true, Skipped: true},
	}
	got := RenderSynthPrompt("goal", outcomes)

	wantSubs := []string{
		"--- [s1] OK task (ok) ---",
		"result one", // result is trimmed
		"--- [s2] Bad task (FAILED) ---",
		"boom",
		"--- [s3] Skip task (SKIPPED) ---",
		"n/a",
		"--- [s4] Both (SKIPPED) ---",
	}
	for _, s := range wantSubs {
		if !strings.Contains(got, s) {
			t.Errorf("RenderSynthPrompt output missing %q\nfull output:\n%s", s, got)
		}
	}
	// Result should be trimmed inside the section, so the leading spaces should not appear.
	if strings.Contains(got, "  result one  ") {
		t.Errorf("expected result to be trimmed inside section, got:\n%s", got)
	}
}

func TestRenderSynthPromptOrderingPreserved(t *testing.T) {
	outcomes := []SubtaskOutcome{
		{ID: "a", Title: "First", Result: "1"},
		{ID: "b", Title: "Second", Result: "2"},
		{ID: "c", Title: "Third", Result: "3"},
	}
	got := RenderSynthPrompt("goal", outcomes)
	iA := strings.Index(got, "[a]")
	iB := strings.Index(got, "[b]")
	iC := strings.Index(got, "[c]")
	if iA < 0 || iB < 0 || iC < 0 {
		t.Fatalf("missing one of the section markers: a=%d b=%d c=%d", iA, iB, iC)
	}
	if !(iA < iB && iB < iC) {
		t.Errorf("expected subtasks in input order; got positions a=%d b=%d c=%d", iA, iB, iC)
	}
}
