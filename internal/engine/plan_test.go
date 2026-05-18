package engine

import (
	"errors"
	"testing"
)

func TestParsePlan_Basic(t *testing.T) {
	in := `{"subtasks":[{"id":"s1","title":"a","prompt":"do a"}]}`
	p, err := ParsePlan(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.Subtasks) != 1 || p.Subtasks[0].ID != "s1" {
		t.Fatalf("unexpected plan: %+v", p)
	}
}

func TestParsePlan_PreambleAndTrailer(t *testing.T) {
	in := `Sure, here is the plan: {"subtasks":[{"id":"s1","title":"a","prompt":"do a"}]} hope that helps.`
	p, err := ParsePlan(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.Subtasks) != 1 {
		t.Fatalf("unexpected plan: %+v", p)
	}
}

func TestParsePlan_PreambleWithBraces(t *testing.T) {
	// LLM emits prose with braces (code snippet) before the JSON; old extractor
	// would grab from the first `{` in the prose and fail to unmarshal.
	in := "First I'll describe what `func() { return nil }` does. " +
		`{"subtasks":[{"id":"s1","title":"a","prompt":"do a"}]}` +
		" Trailing {x} too."
	p, err := ParsePlan(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.Subtasks) != 1 || p.Subtasks[0].ID != "s1" {
		t.Fatalf("unexpected plan: %+v", p)
	}
}

func TestParsePlan_NoJSON(t *testing.T) {
	_, err := ParsePlan("the model refused")
	if !errors.Is(err, ErrPlanUnparseable) {
		t.Fatalf("expected ErrPlanUnparseable, got %v", err)
	}
}

func TestParsePlan_EmptySubtasks(t *testing.T) {
	_, err := ParsePlan(`{"subtasks":[]}`)
	if !errors.Is(err, ErrPlanUnparseable) {
		t.Fatalf("expected ErrPlanUnparseable, got %v", err)
	}
}

func TestParsePlan_InvalidSubtaskID(t *testing.T) {
	// Planner-supplied IDs with spaces or other punctuation are rejected — they
	// could break artifact paths, JSON payloads, or `needs` references.
	cases := []string{
		`{"subtasks":[{"id":"has space","title":"t","prompt":"p"}]}`,
		`{"subtasks":[{"id":"weird/id","title":"t","prompt":"p"}]}`,
		`{"subtasks":[{"id":"with$dollar","title":"t","prompt":"p"}]}`,
		`{"subtasks":[{"id":"../traversal","title":"t","prompt":"p"}]}`,
	}
	for _, in := range cases {
		if _, err := ParsePlan(in); !errors.Is(err, ErrPlanUnparseable) {
			t.Errorf("input %q: expected ErrPlanUnparseable, got %v", in, err)
		}
	}
}

func TestParsePlan_ValidSubtaskIDs(t *testing.T) {
	// Common safe shapes the planner is likely to produce.
	cases := []string{
		`{"subtasks":[{"id":"s1","title":"t","prompt":"p"}]}`,
		`{"subtasks":[{"id":"build-frontend","title":"t","prompt":"p"}]}`,
		`{"subtasks":[{"id":"step_2","title":"t","prompt":"p"}]}`,
		`{"subtasks":[{"id":"v1.0","title":"t","prompt":"p"}]}`,
	}
	for _, in := range cases {
		if _, err := ParsePlan(in); err != nil {
			t.Errorf("input %q: unexpected error: %v", in, err)
		}
	}
}

func TestParsePlan_AutoFilledIDIsValid(t *testing.T) {
	// When the planner omits the id field, ParsePlan auto-fills "s1", which
	// must pass its own format check.
	in := `{"subtasks":[{"title":"t","prompt":"p"}]}`
	p, err := ParsePlan(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Subtasks[0].ID != "s1" {
		t.Errorf("auto-filled id: got %q, want s1", p.Subtasks[0].ID)
	}
}

func TestParsePlan_BracesInsidePromptStringLiteral(t *testing.T) {
	// The subtask prompt itself contains JSON-escaped braces. String-aware
	// matching must not be confused by them.
	in := `Plan: {"subtasks":[{"id":"s1","title":"t","prompt":"write code with { and } braces"}]}`
	p, err := ParsePlan(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.Subtasks) != 1 || p.Subtasks[0].Prompt != "write code with { and } braces" {
		t.Fatalf("unexpected plan: %+v", p)
	}
}
