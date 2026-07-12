package steer

import (
	"strings"
	"testing"
	"time"
)

const helloSrc = `#!/usr/bin/env -S uta mission run
// hello.steer — the first steer program.

agent fn greet(name: Text) -> Text
  worker any(claude, gemini)
  costs <= 4k tokens
  prompt """
  Say "hello, ${name}" back — one short, warm sentence.
  """

mission hello_world {
  budget 20k tokens, $1, 5min

  let greeting = greet("world")
  emit greeting
}
`

func parseOK(t *testing.T, src string) *Program {
	t.Helper()
	prog, d := Parse("test.steer", src)
	if d != nil {
		t.Fatalf("parse: %s", d.Render())
	}
	return prog
}

func TestParse_HelloWorld(t *testing.T) {
	prog := parseOK(t, helloSrc)

	if len(prog.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(prog.Agents))
	}
	fn := prog.Agents[0]
	if fn.Name != "greet" || fn.ReturnType != "Text" {
		t.Errorf("fn = %s -> %s", fn.Name, fn.ReturnType)
	}
	if len(fn.Params) != 1 || fn.Params[0].Name != "name" || fn.Params[0].Type != "Text" {
		t.Errorf("params = %+v", fn.Params)
	}
	if len(fn.Workers) != 2 || fn.Workers[0] != "claude" || fn.Workers[1] != "gemini" {
		t.Errorf("workers = %v", fn.Workers)
	}
	if fn.CostTokens != 4000 {
		t.Errorf("costs = %d, want 4000 (4k)", fn.CostTokens)
	}
	if !strings.Contains(fn.Prompt.Text, `Say "hello, ${name}" back`) {
		t.Errorf("prompt = %q", fn.Prompt.Text)
	}
	if len(fn.Prompt.Slots) != 1 || fn.Prompt.Slots[0].Name != "name" {
		t.Errorf("slots = %+v", fn.Prompt.Slots)
	}

	m := prog.Mission
	if m == nil || m.Name != "hello_world" {
		t.Fatalf("mission = %+v", m)
	}
	if m.Budget.Tokens != 20000 || m.Budget.USDCents != 100 || m.Budget.Deadline != 5*time.Minute {
		t.Errorf("budget = %+v", m.Budget)
	}
	if len(m.Stmts) != 2 {
		t.Fatalf("stmts = %d, want 2", len(m.Stmts))
	}
	let, ok := m.Stmts[0].(*LetStmt)
	if !ok || let.Name != "greeting" {
		t.Fatalf("stmt[0] = %#v", m.Stmts[0])
	}
	call, ok := let.Expr.(*CallExpr)
	if !ok || call.Name != "greet" || len(call.Args) != 1 {
		t.Fatalf("let expr = %#v", let.Expr)
	}
	if lit, ok := call.Args[0].(*StringLit); !ok || lit.Value != "world" {
		t.Errorf("arg = %#v", call.Args[0])
	}
	if _, ok := m.Stmts[1].(*EmitStmt); !ok {
		t.Errorf("stmt[1] = %#v", m.Stmts[1])
	}
}

func TestParse_ShebangAndCommentsSkipped(t *testing.T) {
	prog := parseOK(t, helloSrc)
	if prog.Mission == nil {
		t.Fatal("mission lost behind shebang/comments")
	}
}

func TestParse_PromptDedent(t *testing.T) {
	prog := parseOK(t, helloSrc)
	p := prog.Agents[0].Prompt.Text
	if strings.HasPrefix(p, " ") || strings.HasPrefix(p, "\t") {
		t.Errorf("prompt should be dedented, got %q", p)
	}
	if strings.HasSuffix(p, "\n") {
		t.Errorf("prompt should have no trailing newline, got %q", p)
	}
}

func TestParse_ReservedConstructsGetRoadmapError(t *testing.T) {
	src := `mission m {
  budget 10k tokens
  until dry(2) { }
}`
	_, d := Parse("t.steer", src)
	if d == nil {
		t.Fatal("expected a diagnostic for 'until'")
	}
	if !strings.Contains(d.Msg, "not implemented in the v0 interpreter") || !strings.Contains(d.Msg, "discovery loops") {
		t.Errorf("diag = %q, want roadmap pointer", d.Msg)
	}
}

func TestParse_SyntaxErrorHasPositionAndExcerpt(t *testing.T) {
	src := "mission m {\n  budget 10k tokens\n  let = greet()\n}"
	_, d := Parse("t.steer", src)
	if d == nil {
		t.Fatal("expected a parse error")
	}
	if d.Pos.Line != 3 {
		t.Errorf("line = %d, want 3", d.Pos.Line)
	}
	r := d.Render()
	if !strings.Contains(r, "let = greet()") || !strings.Contains(r, "^") {
		t.Errorf("render should carry excerpt + caret:\n%s", r)
	}
}

func TestParse_MultipleMissionsRejected(t *testing.T) {
	src := "mission a { budget 1k tokens }\nmission b { budget 1k tokens }"
	_, d := Parse("t.steer", src)
	if d == nil || !strings.Contains(d.Msg, "exactly one") {
		t.Fatalf("diag = %v", d)
	}
}

func TestCheck_HelloWorldIsClean(t *testing.T) {
	prog := parseOK(t, helloSrc)
	diags := Check(prog, helloSrc)
	if HasErrors(diags) {
		for _, d := range diags {
			t.Log(d.Render())
		}
		t.Fatal("hello world should check clean")
	}
	for _, d := range diags {
		t.Errorf("unexpected warning: %s", d.Error())
	}
}

func TestCheck_MissingBudgetIsError(t *testing.T) {
	src := `agent fn f() -> Text
  prompt """hi"""
mission m {
  emit f()
}`
	prog := parseOK(t, src)
	diags := Check(prog, src)
	if !HasErrors(diags) {
		t.Fatal("mission without budget must not compile")
	}
	joined := renderAll(diags)
	if !strings.Contains(joined, "declares no budget") {
		t.Errorf("diags = %s", joined)
	}
}

func TestCheck_DurationOnlyBudgetIsError(t *testing.T) {
	src := `agent fn f() -> Text
  prompt """hi"""
mission m {
  budget 5min
  emit f()
}`
	prog := parseOK(t, src)
	if !HasErrors(Check(prog, src)) {
		t.Fatal("duration-only budget must not compile (does not bound spend)")
	}
}

func TestCheck_UnknownCallSuggests(t *testing.T) {
	src := `agent fn greet(name: Text) -> Text
  prompt """hello ${name}"""
mission m {
  budget 10k tokens
  emit gret("world")
}`
	prog := parseOK(t, src)
	diags := Check(prog, src)
	joined := renderAll(diags)
	if !strings.Contains(joined, `unknown function "gret"`) || !strings.Contains(joined, `did you mean "greet"`) {
		t.Errorf("diags = %s", joined)
	}
}

func TestCheck_UnknownPromptSlot(t *testing.T) {
	src := `agent fn greet(name: Text) -> Text
  prompt """hello ${nmae}"""
mission m {
  budget 10k tokens
  emit greet("world")
}`
	prog := parseOK(t, src)
	diags := Check(prog, src)
	joined := renderAll(diags)
	if !strings.Contains(joined, "${nmae}") || !strings.Contains(joined, `did you mean "name"`) {
		t.Errorf("diags = %s", joined)
	}
}

func TestCheck_ArityMismatch(t *testing.T) {
	src := `agent fn greet(name: Text) -> Text
  prompt """hello ${name}"""
mission m {
  budget 10k tokens
  emit greet()
}`
	prog := parseOK(t, src)
	joined := renderAll(Check(prog, src))
	if !strings.Contains(joined, "takes 1 argument(s)") {
		t.Errorf("diags = %s", joined)
	}
}

func TestCheck_UnknownIdent(t *testing.T) {
	src := `agent fn f() -> Text
  prompt """hi"""
mission m {
  budget 10k tokens
  let x = f()
  emit y
}`
	prog := parseOK(t, src)
	joined := renderAll(Check(prog, src))
	if !strings.Contains(joined, `unknown name "y"`) {
		t.Errorf("diags = %s", joined)
	}
}

func TestCheck_Warnings(t *testing.T) {
	src := `agent fn f() -> Text
  prompt """hi"""
agent fn unused() -> Text
  prompt """never called"""
mission m {
  budget 10k tokens
  let x = f()
}`
	prog := parseOK(t, src)
	diags := Check(prog, src)
	if HasErrors(diags) {
		t.Fatalf("only warnings expected: %s", renderAll(diags))
	}
	joined := renderAll(diags)
	for _, want := range []string{"never emits", `binding "x" is never used`, `agent fn "unused" is declared but never called`} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing warning %q in:\n%s", want, joined)
		}
	}
}

func TestCheck_AgentFnWithoutPrompt(t *testing.T) {
	src := `agent fn f() -> Text
  worker claude
mission m {
  budget 10k tokens
  emit f()
}`
	prog := parseOK(t, src)
	joined := renderAll(Check(prog, src))
	if !strings.Contains(joined, "no prompt block") {
		t.Errorf("diags = %s", joined)
	}
}

func TestRenderPrompt(t *testing.T) {
	prog := parseOK(t, helloSrc)
	out, err := prog.Agents[0].RenderPrompt(map[string]string{"name": "world"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `Say "hello, world" back`) {
		t.Errorf("rendered = %q", out)
	}
}

func renderAll(diags []Diag) string {
	var b strings.Builder
	for _, d := range diags {
		b.WriteString(d.Render())
		b.WriteString("\n")
	}
	return b.String()
}
