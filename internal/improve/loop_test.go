package improve

import (
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/profile"
)

func TestRenderImplementPrompt_IncludesAllFields(t *testing.T) {
	idea := &Idea{
		ID:       "idea-123",
		Title:    "Improve thing",
		Body:     "Specifically, do X.",
		Severity: SevHigh,
		Source:   "gather:gemini",
	}
	got := renderImplementPrompt(idea, "/tmp/work", "go test ./...")

	for _, want := range []string{
		"Improve thing",
		"high",
		"gather:gemini",
		"Specifically, do X.",
		"/tmp/work",
		"go test ./...",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, got)
		}
	}
}

func TestRenderImplementPrompt_FallsBackToDefaults(t *testing.T) {
	idea := &Idea{
		Title:    "Bare",
		Severity: SevMedium,
		Source:   "manual",
	}
	got := renderImplementPrompt(idea, "", "")
	// Empty workdir becomes ".".
	if !strings.Contains(got, "Working directory: .") {
		t.Errorf("expected default workdir '.', got:\n%s", got)
	}
	// Empty verify falls back to the dry-run sentinel.
	if !strings.Contains(got, "(none — dry-run mode)") {
		t.Errorf("expected dry-run sentinel, got:\n%s", got)
	}
	// Empty body falls back to the empty-body sentinel.
	if !strings.Contains(got, "(no body — work from title alone)") {
		t.Errorf("expected empty-body sentinel, got:\n%s", got)
	}
}

func TestRenderImplementPrompt_TrimsBody(t *testing.T) {
	idea := &Idea{Title: "x", Severity: SevMedium, Body: "\n\n  real body  \n\n"}
	got := renderImplementPrompt(idea, ".", "make test")
	if !strings.Contains(got, "real body") {
		t.Errorf("expected trimmed body content in prompt, got:\n%s", got)
	}
	if strings.Contains(got, "\n\n\n\n") {
		t.Errorf("body whitespace not trimmed, got:\n%s", got)
	}
}

func TestStringifyDuration_NilArgs(t *testing.T) {
	if got := stringifyDuration(nil, nil); got != "?" {
		t.Errorf("nil-nil: got %q want %q", got, "?")
	}
}

func TestStringifyDuration_NonTimeTypes(t *testing.T) {
	if got := stringifyDuration("not-a-time", "neither"); got != "?" {
		t.Errorf("non-time types: got %q want %q", got, "?")
	}
}

func TestStringifyDuration_HappyPath(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	end := start.Add(45 * time.Second)
	got := stringifyDuration(&end, &start)
	if got != "45s" {
		t.Errorf("duration: got %q want %q", got, "45s")
	}
}

func TestStringifyDuration_RoundsToSeconds(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	end := start.Add(1*time.Minute + 23*time.Second + 400*time.Millisecond)
	got := stringifyDuration(&end, &start)
	// time.Duration.String() formats <60s as Ns; >=60s as XmYs.
	if got != "1m23s" {
		t.Errorf("rounded duration: got %q want %q", got, "1m23s")
	}
}

func TestMin(t *testing.T) {
	if got := min(3, 5); got != 3 {
		t.Errorf("min(3,5)=%d want 3", got)
	}
	if got := min(7, 2); got != 2 {
		t.Errorf("min(7,2)=%d want 2", got)
	}
	if got := min(4, 4); got != 4 {
		t.Errorf("min(4,4)=%d want 4", got)
	}
}

func TestApplyProfile_PopulatesLoopFields(t *testing.T) {
	p := &profile.MissionProfile{
		Name:         "audit",
		Env:          map[string]string{"AUDITOR": "trail-of-bits"},
		AllowedTools: []string{"Read", "Bash(slither *)"},
		DeniedTools:  []string{"Bash(rm *)"},
		Policies: profile.Policies{
			TokenBudget:        100_000,
			PerCallBudget:      10_000,
			DollarBudgetCents:  500,
			HITLTriggers:       []string{"Bash(* push *)"},
			HITLTokenThreshold: 80,
			HITLSeverity:       "high",
		},
	}
	req := &LoopRequest{PreApproveTools: []string{"Read", "Bash(rm *)"}}
	ApplyProfile(req, p)

	if req.ModeName != "audit" {
		t.Errorf("ModeName = %q, want audit", req.ModeName)
	}
	if len(req.AllowedTools) == 0 {
		t.Errorf("AllowedTools should be copied from profile")
	}
	if req.MaxTokens != 100_000 || req.MaxUSDCents != 500 {
		t.Errorf("budget caps not copied: tokens=%d cents=%d", req.MaxTokens, req.MaxUSDCents)
	}
	if len(req.HITLTriggers) != 1 || req.HITLTriggers[0] != "Bash(* push *)" {
		t.Errorf("HITLTriggers = %v", req.HITLTriggers)
	}
	// Pre-approve must drop denied entries — Bash(rm *) is in DeniedTools
	// exactly, so SubtractDeniedTools removes it from PreApproveTools.
	for _, tool := range req.PreApproveTools {
		if tool == "Bash(rm *)" {
			t.Errorf("DeniedTools entry leaked into PreApproveTools: %v", req.PreApproveTools)
		}
	}
}

func TestApplyProfile_NilSafe(t *testing.T) {
	ApplyProfile(nil, nil)
	ApplyProfile(&LoopRequest{}, nil)
}
