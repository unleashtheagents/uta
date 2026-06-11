package eval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSuite(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evals.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write suite: %v", err)
	}
	return path
}

const validSuite = `
version: 1
name: smoke
workers: [claude, gemini]
defaults:
  timeout: 5m
cases:
  - name: summarize
    goal: summarize this repo
    assert:
      contains: ["repo"]
      min_chars: 20
  - name: pinned-worker
    goal: count to three
    workers: [gemini]
    assert:
      contains: ["three"]
`

func TestLoadSuite_Valid(t *testing.T) {
	s, err := LoadSuite(writeSuite(t, validSuite))
	if err != nil {
		t.Fatalf("LoadSuite: %v", err)
	}
	if s.Name != "smoke" || len(s.Cases) != 2 {
		t.Errorf("suite = %+v", s)
	}
	if s.Cases[0].Assert.MinChars != 20 {
		t.Errorf("min_chars = %d", s.Cases[0].Assert.MinChars)
	}
}

func TestLoadSuite_Invalid(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no-cases", "version: 1\nname: x\ncases: []\n", "no cases"},
		{"missing-goal", "cases:\n  - name: a\n    assert: {min_chars: 1}\n", "goal is required"},
		{"missing-name", "cases:\n  - goal: g\n    assert: {min_chars: 1}\n", "name is required"},
		{"no-assertions", "cases:\n  - name: a\n    goal: g\n", "at least one assertion"},
		{"duplicate-names", "cases:\n  - {name: a, goal: g, assert: {min_chars: 1}}\n  - {name: a, goal: h, assert: {min_chars: 1}}\n", "duplicate"},
		{"unknown-field", "cases:\n  - name: a\n    goal: g\n    asserts: {min_chars: 1}\n", "field asserts not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadSuite(writeSuite(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRunSuite_MatrixAndGrading(t *testing.T) {
	s, err := LoadSuite(writeSuite(t, validSuite))
	if err != nil {
		t.Fatalf("LoadSuite: %v", err)
	}
	var ran []string
	run := func(_ context.Context, c Case, worker string) RunOutcome {
		ran = append(ran, c.Name+"/"+worker)
		switch c.Name {
		case "summarize":
			return RunOutcome{
				SessionID: "sess-" + worker, Status: "completed",
				FinalAnswer: "This repo orchestrates agents across providers.",
			}
		default:
			return RunOutcome{Status: "completed", FinalAnswer: "one two three"}
		}
	}
	rep := RunSuite(context.Background(), s, "", run, nil)

	// summarize × (claude, gemini) + pinned-worker × (gemini) = 3 cells.
	if len(rep.Cells) != 3 {
		t.Fatalf("cells = %d (%v), want 3", len(rep.Cells), ran)
	}
	if rep.Failed != 0 || rep.Passed != 3 {
		t.Errorf("passed=%d failed=%d, want 3/0: %+v", rep.Passed, rep.Failed, rep.Cells)
	}
	wantRan := []string{"summarize/claude", "summarize/gemini", "pinned-worker/gemini"}
	for i, w := range wantRan {
		if ran[i] != w {
			t.Errorf("ran[%d] = %s, want %s", i, ran[i], w)
		}
	}
}

func TestGrade_FailureModes(t *testing.T) {
	base := Case{
		Name: "c", Goal: "g",
		Assert: Assertions{
			Contains:    []string{"needle"},
			NotContains: []string{"forbidden"},
			MinChars:    10,
			MaxUSDCents: 50,
			MaxTokens:   1000,
		},
	}

	t.Run("all-pass", func(t *testing.T) {
		checks := grade(base, RunOutcome{
			Status: "completed", FinalAnswer: "here is the needle in plain sight",
			TokensIn: 400, TokensOut: 100, USDCents: 12,
		})
		for _, c := range checks {
			if !c.Passed {
				t.Errorf("check %s failed: %s", c.Name, c.Detail)
			}
		}
	})

	t.Run("missing-contains", func(t *testing.T) {
		checks := grade(base, RunOutcome{Status: "completed", FinalAnswer: "nothing relevant here at all"})
		if !hasFailedCheck(checks, "contains:needle") {
			t.Errorf("expected contains failure: %+v", checks)
		}
	})

	t.Run("forbidden-present", func(t *testing.T) {
		checks := grade(base, RunOutcome{Status: "completed", FinalAnswer: "the needle and the FORBIDDEN word"})
		if !hasFailedCheck(checks, "not_contains:forbidden") {
			t.Errorf("expected not_contains failure (case-insensitive): %+v", checks)
		}
	})

	t.Run("over-budget", func(t *testing.T) {
		checks := grade(base, RunOutcome{
			Status: "completed", FinalAnswer: "the needle is here today",
			USDCents: 51, TokensIn: 900, TokensOut: 200,
		})
		if !hasFailedCheck(checks, "max_usd_cents") || !hasFailedCheck(checks, "max_tokens") {
			t.Errorf("expected budget failures: %+v", checks)
		}
	})

	t.Run("run-error-short-circuits", func(t *testing.T) {
		checks := grade(base, RunOutcome{Err: errors.New("provider exploded")})
		if len(checks) != 1 || checks[0].Passed {
			t.Errorf("errored run must yield exactly one failed check: %+v", checks)
		}
	})

	t.Run("wrong-status", func(t *testing.T) {
		checks := grade(base, RunOutcome{Status: "partial", FinalAnswer: "the needle is here today"})
		if !hasFailedCheck(checks, "status") {
			t.Errorf("expected status failure: %+v", checks)
		}
	})
}

func TestGrade_GateCommand(t *testing.T) {
	c := Case{
		Name: "g", Goal: "g",
		Assert: Assertions{Gate: `grep -q "magic" || exit 1`},
	}
	checks := grade(c, RunOutcome{Status: "completed", FinalAnswer: "the magic word"})
	if !allPassed(checks) {
		t.Errorf("gate should pass on stdin match: %+v", checks)
	}
	checks = grade(c, RunOutcome{Status: "completed", FinalAnswer: "nothing here"})
	if !hasFailedCheck(checks, "gate") {
		t.Errorf("gate should fail without match: %+v", checks)
	}
}

func hasFailedCheck(checks []CheckResult, name string) bool {
	for _, c := range checks {
		if c.Name == name && !c.Passed {
			return true
		}
	}
	return false
}

func allPassed(checks []CheckResult) bool {
	for _, c := range checks {
		if !c.Passed {
			return false
		}
	}
	return true
}
