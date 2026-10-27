package cli

import (
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/store"
)

func TestSlugifyGoal(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"add rate limiting middleware", "add-rate-limiting-middleware"},
		{"FIX error handling! (critical)", "fix-error-handling-critical"},
		{"   spaces   and---dashes   ", "spaces-and-dashes"},
		{"a very long goal title that exceeds forty characters and should be truncated cleanly", "a-very-long-goal-title-that-exceeds-fort"},
	}

	for _, tt := range tests {
		got := slugifyGoal(tt.input)
		if got != tt.want {
			t.Errorf("slugifyGoal(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBuildPRBody(t *testing.T) {
	res := engine.RunResult{
		SessionID: "sess-12345",
		Subtasks: []store.Subtask{
			{ID: "s1", Title: "Scaffold middleware", Worker: "claude", Status: "completed"},
			{ID: "s2", Title: "Add unit tests", Worker: "claude", Status: "completed"},
		},
	}

	body := buildPRBody("Add rate limiting", res, "go test ./...")

	if !strings.Contains(body, "Add rate limiting") {
		t.Errorf("PR body missing objective: %s", body)
	}
	if !strings.Contains(body, "Scaffold middleware") || !strings.Contains(body, "Add unit tests") {
		t.Errorf("PR body missing subtask list: %s", body)
	}
	if !strings.Contains(body, "Passed verification gate: `go test ./...`") {
		t.Errorf("PR body missing verification info: %s", body)
	}
	if !strings.Contains(body, "`sess-12345`") {
		t.Errorf("PR body missing session id: %s", body)
	}
}
