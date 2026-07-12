package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/steer"
)

const missionTestSrc = `#!/usr/bin/env -S uta mission run
agent fn greet(name: Text) -> Text
  worker any(claude, gemini)
  costs <= 4k tokens
  prompt """
  Say "hello, ${name}" back.
  """

mission hello_world {
  budget 20k tokens, $1, 5min
  let greeting = greet("world")
  emit greeting
}
`

func writeSteer(t *testing.T, src string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.steer")
	if err := os.WriteFile(path, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadSteerProgram_OK(t *testing.T) {
	var errw strings.Builder
	prog, _, ok := loadSteerProgram(writeSteer(t, missionTestSrc), &errw)
	if !ok {
		t.Fatalf("load failed: %s", errw.String())
	}
	if prog.Mission.Name != "hello_world" {
		t.Errorf("mission = %q", prog.Mission.Name)
	}
	if errw.Len() != 0 {
		t.Errorf("clean program should print no diagnostics, got %q", errw.String())
	}
}

func TestLoadSteerProgram_CheckErrorsRendered(t *testing.T) {
	src := strings.Replace(missionTestSrc, "greet(\"world\")", "gret(\"world\")", 1)
	var errw strings.Builder
	_, _, ok := loadSteerProgram(writeSteer(t, src), &errw)
	if ok {
		t.Fatal("bad program must not load")
	}
	out := errw.String()
	if !strings.Contains(out, `unknown function "gret"`) || !strings.Contains(out, "^") {
		t.Errorf("diagnostics should carry suggestion + caret, got:\n%s", out)
	}
}

func TestLoadSteerProgram_MissingFile(t *testing.T) {
	var errw strings.Builder
	_, _, ok := loadSteerProgram(filepath.Join(t.TempDir(), "nope.steer"), &errw)
	if ok || errw.Len() == 0 {
		t.Fatal("missing file should fail with a printed error")
	}
}

func TestRenderMissionPlan(t *testing.T) {
	prog, d := steer.Parse("hello.steer", missionTestSrc)
	if d != nil {
		t.Fatal(d.Render())
	}
	plan := renderMissionPlan(prog)
	for _, want := range []string{
		"mission hello_world (hello.steer)",
		"budget: 20k tokens, $1.00, 5m0s",
		"c1  greet(…)  worker any(claude, gemini)  costs <= 4k tokens",
		"emits: 1",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan missing %q:\n%s", want, plan)
		}
	}
}

func TestRenderMissionPlan_NestedCallsInOrder(t *testing.T) {
	src := `
agent fn a(x: Text) -> Text
  prompt """${x}"""
agent fn b(x: Text) -> Text
  prompt """${x}"""
mission m {
  budget 10k tokens
  emit b(a("hi"))
}
`
	prog, d := steer.Parse("m.steer", src)
	if d != nil {
		t.Fatal(d.Render())
	}
	plan := renderMissionPlan(prog)
	ai, bi := strings.Index(plan, "c1  a("), strings.Index(plan, "c2  b(")
	if ai < 0 || bi < 0 || ai > bi {
		t.Errorf("nested call a() should be planned before b():\n%s", plan)
	}
}

func TestFormatBudget(t *testing.T) {
	if got := formatBudget(nil); got != "(none)" {
		t.Errorf("nil budget = %q", got)
	}
	if got := formatBudget(&steer.BudgetDecl{Tokens: 500}); got != "500 tokens" {
		t.Errorf("got %q", got)
	}
}

func TestCountMissionCalls(t *testing.T) {
	prog, d := steer.Parse("hello.steer", missionTestSrc)
	if d != nil {
		t.Fatal(d.Render())
	}
	if n := countMissionCalls(prog); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
}
