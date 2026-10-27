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
  recall "past bugs"
}`
	_, d := Parse("t.steer", src)
	if d == nil {
		t.Fatal("expected a diagnostic for 'recall'")
	}
	if !strings.Contains(d.Msg, "not implemented in the v0 interpreter") || !strings.Contains(d.Msg, "memory queries") {
		t.Errorf("diag = %q, want roadmap pointer", d.Msg)
	}
}

const retrySrc = `agent fn fix(plan: Text) -> Text
  prompt """Fix ${plan}."""

mission handled {
  budget 20k tokens
  let fixes = fix("the plan")
      retry 2 on SchemaMismatch
  emit fixes
}`

func TestParse_RetryClause(t *testing.T) {
	prog := parseOK(t, retrySrc)
	if diags := Check(prog, retrySrc); HasErrors(diags) {
		t.Fatalf("check: %s", renderAll(diags))
	}
	let := prog.Mission.Stmts[0].(*LetStmt)
	r, ok := let.Expr.(*RetryExpr)
	if !ok {
		t.Fatalf("expr = %#v, want RetryExpr", let.Expr)
	}
	if r.N != 2 || r.ErrName != "SchemaMismatch" {
		t.Errorf("retry = %d on %q, want 2 on SchemaMismatch", r.N, r.ErrName)
	}
	if _, ok := r.Expr.(*CallExpr); !ok {
		t.Errorf("wrapped expr = %#v, want CallExpr", r.Expr)
	}
	// The wrapped call is on the static plan, once.
	if calls := prog.Mission.Calls(); len(calls) != 1 || calls[0].Name != "fix" {
		t.Errorf("Calls() = %v, want the wrapped fix call", calls)
	}
}

func TestParse_RetryZeroAttemptsRejected(t *testing.T) {
	src := strings.Replace(retrySrc, "retry 2 on", "retry 0 on", 1)
	_, d := Parse("t.steer", src)
	if d == nil || !strings.Contains(d.Msg, "at least one attempt") {
		t.Fatalf("diag = %v, want attempt-count error", d)
	}
}

func TestCheck_RetryUnknownErrorNameSuggests(t *testing.T) {
	src := strings.Replace(retrySrc, "on SchemaMismatch", "on SchemaMismatchh", 1)
	prog := parseOK(t, src)
	joined := renderAll(Check(prog, src))
	if !strings.Contains(joined, `unknown error name "SchemaMismatchh"`) || !strings.Contains(joined, `did you mean "SchemaMismatch"`) {
		t.Errorf("diags = %s", joined)
	}
}

func TestCheck_RetryOnBudgetExceededRejected(t *testing.T) {
	src := strings.Replace(retrySrc, "on SchemaMismatch", "on BudgetExceeded", 1)
	prog := parseOK(t, src)
	joined := renderAll(Check(prog, src))
	if !strings.Contains(joined, "BudgetExceeded is not allowed") || !strings.Contains(joined, "already spent") {
		t.Errorf("diags = %s", joined)
	}
}

const gateSrc = `
agent fn draft(topic: Text) -> Text
  prompt """Draft ${topic}."""

mission gated {
  budget 10k tokens
  let d = draft("x")
  gate human "Continue?"
  emit d
}
`

func TestParse_GateHuman(t *testing.T) {
	prog := parseOK(t, gateSrc)
	if diags := Check(prog, gateSrc); HasErrors(diags) {
		t.Fatalf("check: %v", diags)
	}
	if len(prog.Mission.Stmts) != 3 {
		t.Fatalf("stmts = %d, want 3", len(prog.Mission.Stmts))
	}
	g, ok := prog.Mission.Stmts[1].(*GateStmt)
	if !ok {
		t.Fatalf("stmt[1] = %#v, want GateStmt", prog.Mission.Stmts[1])
	}
	if lit, ok := g.Message.(*StringLit); !ok || lit.Value != "Continue?" {
		t.Errorf("gate message = %#v", g.Message)
	}
}

func TestParse_GateRequiresHumanKeyword(t *testing.T) {
	_, d := Parse("t.steer", strings.Replace(gateSrc, "gate human", "gate robot", 1))
	if d == nil || !strings.Contains(d.Msg, `"human"`) {
		t.Fatalf("diag = %v, want a 'human' expectation", d)
	}
}

func TestCheck_GateMessageMustResolve(t *testing.T) {
	src := strings.Replace(gateSrc, `gate human "Continue?"`, "gate human nonexistent", 1)
	prog := parseOK(t, src)
	if diags := Check(prog, src); !HasErrors(diags) {
		t.Fatal("unknown name in a gate message should be a check error")
	}
}

func TestParse_GateWithinDeadline(t *testing.T) {
	src := strings.Replace(gateSrc, `gate human "Continue?"`, `gate human "Continue?" within 24h`, 1)
	prog := parseOK(t, src)
	if diags := Check(prog, src); HasErrors(diags) {
		t.Fatalf("check: %v", diags)
	}
	g, ok := prog.Mission.Stmts[1].(*GateStmt)
	if !ok {
		t.Fatalf("stmt[1] = %#v, want GateStmt", prog.Mission.Stmts[1])
	}
	if g.Within != 24*time.Hour {
		t.Errorf("Within = %v, want 24h", g.Within)
	}
}

func TestParse_GateWithoutDeadlineHasZeroWithin(t *testing.T) {
	prog := parseOK(t, gateSrc)
	if g := prog.Mission.Stmts[1].(*GateStmt); g.Within != 0 {
		t.Errorf("Within = %v, want 0 for a gate with no within clause", g.Within)
	}
}

func TestParse_GateWithinRejectsUnknownUnit(t *testing.T) {
	src := strings.Replace(gateSrc, `gate human "Continue?"`, `gate human "Continue?" within 3days`, 1)
	if _, d := Parse("t.steer", src); d == nil || !strings.Contains(d.Msg, "deadline unit") {
		t.Fatalf("diag = %v, want an unknown-deadline-unit error", d)
	}
}

func TestCheck_OnGateTimedOutHandlerAccepted(t *testing.T) {
	src := strings.Replace(gateSrc,
		`gate human "Continue?"`, `gate human "Continue?" within 1h`, 1)
	src = strings.Replace(src, "emit d\n}", "emit d\n  on GateTimedOut -> emit d; halt\n}", 1)
	prog := parseOK(t, src)
	if diags := Check(prog, src); HasErrors(diags) {
		t.Fatalf("on GateTimedOut should be a known handler name: %v", diags)
	}
}

const policySrc = `
agent fn draft(topic: Text) -> Text
  prompt """Draft ${topic}."""

mission bound {
  policy dev
  budget 10k tokens
  emit draft("x")
}
`

func TestParse_PolicyBindsProfileName(t *testing.T) {
	prog := parseOK(t, policySrc)
	if diags := Check(prog, policySrc); HasErrors(diags) {
		t.Fatalf("check: %s", renderAll(diags))
	}
	pol := prog.Mission.Policy
	if pol == nil || pol.Name != "dev" {
		t.Fatalf("policy = %+v, want dev", pol)
	}
	if pol.Pos.Line != 6 {
		t.Errorf("policy pos line = %d, want 6", pol.Pos.Line)
	}
}

func TestParse_DuplicatePolicyRejected(t *testing.T) {
	src := strings.Replace(policySrc, "policy dev", "policy dev\n  policy ops", 1)
	_, d := Parse("t.steer", src)
	if d == nil || !strings.Contains(d.Msg, "duplicate policy") {
		t.Fatalf("diag = %v, want duplicate-policy error", d)
	}
}

func TestCheckPolicy_ResolvesAndSuggests(t *testing.T) {
	prog := parseOK(t, policySrc)
	if d := CheckPolicy(prog, policySrc, []string{"default", "dev", "ops"}); d != nil {
		t.Fatalf("known policy should resolve, got: %s", d.Render())
	}
	d := CheckPolicy(prog, policySrc, []string{"default", "deva"})
	if d == nil {
		t.Fatal("unknown policy must be a diagnostic")
	}
	if !strings.Contains(d.Msg, `unknown policy "dev"`) || !strings.Contains(d.Msg, `did you mean "deva"`) {
		t.Errorf("diag = %q, want unknown-policy message with suggestion", d.Msg)
	}
	if d.Pos.Line != 6 || !strings.Contains(d.Render(), "policy dev") {
		t.Errorf("diag should carry the policy line excerpt:\n%s", d.Render())
	}
	// No policy declared → nothing to check.
	plain := parseOK(t, helloSrc)
	if d := CheckPolicy(plain, helloSrc, nil); d != nil {
		t.Errorf("mission without policy should pass, got: %s", d.Render())
	}
}

const withCapSrc = `
agent fn deploy(plan: Text) -> Text
  prompt """Deploy ${plan}."""

mission raise {
  policy dev
  budget 10k tokens
  with cap "Bash(git push *)" from policy release {
    let out = deploy("v2")
  }
  emit out
}
`

func TestParse_WithCapScopedRaise(t *testing.T) {
	prog := parseOK(t, withCapSrc)
	if diags := Check(prog, withCapSrc); HasErrors(diags) {
		t.Fatalf("check: %s", renderAll(diags))
	}
	if len(prog.Mission.Stmts) != 2 {
		t.Fatalf("stmts = %d, want 2 (with + emit)", len(prog.Mission.Stmts))
	}
	ws, ok := prog.Mission.Stmts[0].(*WithStmt)
	if !ok {
		t.Fatalf("stmt[0] = %#v, want WithStmt", prog.Mission.Stmts[0])
	}
	if ws.Cap != "Bash(git push *)" || ws.Policy != "release" {
		t.Errorf("with = cap %q from policy %q", ws.Cap, ws.Policy)
	}
	if len(ws.Body) != 1 {
		t.Fatalf("with body = %d stmt(s), want 1", len(ws.Body))
	}
	if _, ok := ws.Body[0].(*LetStmt); !ok {
		t.Errorf("with body stmt = %#v, want LetStmt", ws.Body[0])
	}
	// The nested call is on the static plan.
	calls := prog.Mission.Calls()
	if len(calls) != 1 || calls[0].Name != "deploy" {
		t.Errorf("calls = %+v, want the deploy call inside the with block", calls)
	}
	if raises := prog.Mission.CapRaises(); len(raises) != 1 || raises[0] != ws {
		t.Errorf("CapRaises = %+v, want the one block", raises)
	}
}

func TestParse_WithCapEmptyBlockRejected(t *testing.T) {
	src := strings.Replace(withCapSrc, "    let out = deploy(\"v2\")\n", "", 1)
	src = strings.Replace(src, "emit out", `emit deploy("v2")`, 1)
	_, d := Parse("t.steer", src)
	if d == nil || !strings.Contains(d.Msg, "with block is empty") {
		t.Fatalf("diag = %v, want empty-with-block error", d)
	}
}

func TestCheck_WithCapWithoutPolicyWarns(t *testing.T) {
	src := strings.Replace(withCapSrc, "  policy dev\n", "", 1)
	prog := parseOK(t, src)
	diags := Check(prog, src)
	if HasErrors(diags) {
		t.Fatalf("should be a warning, not an error: %s", renderAll(diags))
	}
	var warned bool
	for _, d := range diags {
		if d.Warning && strings.Contains(d.Msg, "binds no policy") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("want a no-gate-to-raise warning, got: %s", renderAll(diags))
	}
}

func TestCheckCapRaises_VerifiesTheGrant(t *testing.T) {
	prog := parseOK(t, withCapSrc)

	// The named policy grants the cap → clean.
	grants := map[string][]string{
		"dev":     {"Read"},
		"release": {"Read", "Bash(git push *)"},
	}
	if ds := CheckCapRaises(prog, withCapSrc, grants, nil); len(ds) > 0 {
		t.Fatalf("granted raise should pass, got: %s", renderAll(ds))
	}

	// M3 accept criterion: a raise borrowing a cap its source policy does
	// not grant fails at check time, not at run time.
	src := strings.Replace(withCapSrc, "from policy release", "from policy dev", 1)
	prog = parseOK(t, src)
	ds := CheckCapRaises(prog, src, grants, nil)
	if len(ds) == 0 {
		t.Fatal("ungranted raise must be a diagnostic")
	}
	if !strings.Contains(ds[0].Msg, `policy "dev" does not grant capability "Bash(git push *)"`) {
		t.Errorf("diag = %q, want does-not-grant message", ds[0].Msg)
	}
	if !strings.Contains(ds[0].Render(), "with cap") {
		t.Errorf("diag should carry the with-cap line excerpt:\n%s", ds[0].Render())
	}

	// Unknown source policy → diagnostic with a suggestion.
	src = strings.Replace(withCapSrc, "from policy release", "from policy relaese", 1)
	prog = parseOK(t, src)
	ds = CheckCapRaises(prog, src, grants, nil)
	if len(ds) == 0 || !strings.Contains(ds[0].Msg, `unknown policy "relaese"`) || !strings.Contains(ds[0].Msg, `did you mean "release"`) {
		t.Errorf("diags = %v, want unknown-policy message with suggestion", ds)
	}

	// No raises → nothing to check.
	plain := parseOK(t, helloSrc)
	if ds := CheckCapRaises(plain, helloSrc, grants, nil); len(ds) > 0 {
		t.Errorf("mission without raises should pass, got: %s", renderAll(ds))
	}
}

// With no MissionProfiles installed the diagnostics must point at where
// profiles come from instead of rendering a bare "(available: )".
func TestCheckPolicyDiags_NoProfilesInstalledWording(t *testing.T) {
	tests := []struct {
		name string
		diag func(t *testing.T) *Diag
	}{
		{"CheckPolicy", func(t *testing.T) *Diag {
			prog := parseOK(t, policySrc)
			return CheckPolicy(prog, policySrc, nil)
		}},
		{"CheckCapRaises", func(t *testing.T) *Diag {
			prog := parseOK(t, withCapSrc)
			ds := CheckCapRaises(prog, withCapSrc, nil, nil)
			if len(ds) != 1 {
				t.Fatalf("diags = %d, want 1: %s", len(ds), renderAll(ds))
			}
			return &ds[0]
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := tc.diag(t)
			if d == nil {
				t.Fatal("unknown policy with no profiles installed must be a diagnostic")
			}
			if strings.Contains(d.Msg, "available:") {
				t.Errorf("diag = %q, must not render an empty available list", d.Msg)
			}
			if !strings.Contains(d.Msg, "no MissionProfiles installed — create one under ~/.uta/profiles/") {
				t.Errorf("diag = %q, want no-profiles guidance", d.Msg)
			}
		})
	}
}

func TestCheckCapRaises_ReportsEveryBadRaise(t *testing.T) {
	// Two independent problems — an ungranted raise and an unknown source
	// policy — must both surface in one check run, not one per run.
	src := strings.Replace(withCapSrc,
		`  with cap "Bash(git push *)" from policy release {
    let out = deploy("v2")
  }`,
		`  with cap "Bash(git push *)" from policy dev {
    let out = deploy("v2")
  }
  with cap "Write" from policy relaese {
    let out2 = deploy("v3")
  }`, 1)
	src = strings.Replace(src, "emit out\n", "emit out\n  emit out2\n", 1)
	prog := parseOK(t, src)
	grants := map[string][]string{
		"dev":     {"Read"},
		"release": {"Read", "Bash(git push *)"},
	}
	ds := CheckCapRaises(prog, src, grants, nil)
	if len(ds) != 2 {
		t.Fatalf("diags = %d, want both bad raises reported:\n%s", len(ds), renderAll(ds))
	}
	if !strings.Contains(ds[0].Msg, `policy "dev" does not grant capability "Bash(git push *)"`) {
		t.Errorf("diag[0] = %q, want does-not-grant message", ds[0].Msg)
	}
	if !strings.Contains(ds[1].Msg, `unknown policy "relaese"`) {
		t.Errorf("diag[1] = %q, want unknown-policy message", ds[1].Msg)
	}
}

func TestCheckCapRaises_MatcherWidensGrants(t *testing.T) {
	// A raise narrower than the granted pattern: exact equality rejects it,
	// a subsumption matcher (what the CLI passes) accepts it.
	src := strings.Replace(withCapSrc, `"Bash(git push *)"`, `"Bash(git push origin main)"`, 1)
	prog := parseOK(t, src)
	grants := map[string][]string{
		"dev":     {"Read"},
		"release": {"Bash(git push *)"},
	}

	if ds := CheckCapRaises(prog, src, grants, nil); len(ds) == 0 {
		t.Fatal("exact-equality default should reject the narrower raise")
	}

	prefixCovers := func(grant, cap string) bool {
		return grant == cap || (strings.HasSuffix(grant, "*)") && strings.HasPrefix(cap, strings.TrimSuffix(grant, "*)")))
	}
	if ds := CheckCapRaises(prog, src, grants, prefixCovers); len(ds) > 0 {
		t.Fatalf("matcher-covered raise should pass, got: %s", renderAll(ds))
	}

	// An unrelated grant is still rejected, with the did-you-mean hint
	// (raise the original cap so the near-miss grant is within edit
	// distance of the suggestion helper).
	prog = parseOK(t, withCapSrc)
	grants["release"] = []string{"Bash(git pusk *)"}
	ds := CheckCapRaises(prog, withCapSrc, grants, prefixCovers)
	if len(ds) == 0 {
		t.Fatal("unrelated grant must be a diagnostic")
	}
	if !strings.Contains(ds[0].Msg, "does not grant capability") || !strings.Contains(ds[0].Msg, `did you mean "Bash(git pusk *)"`) {
		t.Errorf("diag = %q, want does-not-grant message with suggestion", ds[0].Msg)
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

func TestRenderPrompt_NoReExpansionOfBoundData(t *testing.T) {
	src := `agent fn f(a: Text, b: Text) -> Text
  worker any(claude)
  costs <= 1k tokens
  prompt """
  A=${a} B=${b}
  """
`
	prog := parseOK(t, src)
	out, err := prog.Agents[0].RenderPrompt(map[string]string{"a": "${b}", "b": "SECRET"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "A=${b}") {
		t.Errorf("binding data was re-expanded: rendered = %q", out)
	}
	if !strings.Contains(out, "B=SECRET") {
		t.Errorf("slot ${b} not substituted: rendered = %q", out)
	}
}

func TestParse_DecimalDollarBudget(t *testing.T) {
	src := `agent fn f() -> Text
  prompt """hi"""
mission m {
  budget $1.50, 10k tokens
  emit f()
}`
	prog := parseOK(t, src)
	if got := prog.Mission.Budget.USDCents; got != 150 {
		t.Errorf("USDCents = %d, want 150", got)
	}

	src = strings.Replace(src, "$1.50", "$0.05", 1)
	prog = parseOK(t, src)
	if got := prog.Mission.Budget.USDCents; got != 5 {
		t.Errorf("USDCents = %d, want 5 ($0.05 must keep its leading zero)", got)
	}
}

func TestParse_DecimalDollarBudget_RejectsOneDigitCents(t *testing.T) {
	src := `mission m {
  budget $1.5
}`
	_, d := Parse("t.steer", src)
	if d == nil || !strings.Contains(d.Msg, "exactly two cent digits") {
		t.Fatalf("diag = %v, want two-cent-digits error", d)
	}
}

func TestMissionCalls_ExecutionOrder(t *testing.T) {
	src := `agent fn a(x: Text) -> Text
  prompt """${x}"""
agent fn b(x: Text) -> Text
  prompt """${x}"""
mission m {
  budget 10k tokens
  let v = b(a("hi"))
  emit a(v)
}`
	prog := parseOK(t, src)
	calls := prog.Mission.Calls()
	var names []string
	for _, c := range calls {
		names = append(names, c.Name)
	}
	want := []string{"a", "b", "a"} // nested arg runs before its consumer
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v", names, want)
	}
}

const parSrc = `agent fn scan(lens: Text) -> Text
  prompt """Scan through the ${lens} lens."""

mission sweep {
  budget 50k tokens
  let raw = par for lens in ["security", "perf", "style"] { scan(lens) }
  emit raw
}`

func TestParse_ParFor(t *testing.T) {
	prog := parseOK(t, parSrc)
	let := prog.Mission.Stmts[0].(*LetStmt)
	pf, ok := let.Expr.(*ParForExpr)
	if !ok {
		t.Fatalf("expr = %#v, want ParForExpr", let.Expr)
	}
	if pf.Var != "lens" || len(pf.Items) != 3 {
		t.Errorf("pf = var %q items %d", pf.Var, len(pf.Items))
	}
	if _, ok := pf.Body.(*CallExpr); !ok {
		t.Errorf("body = %#v", pf.Body)
	}
	if diags := Check(prog, parSrc); HasErrors(diags) {
		t.Fatalf("check: %s", renderAll(diags))
	}
	if calls := prog.Mission.Calls(); len(calls) != 1 || calls[0].Name != "scan" {
		t.Errorf("Calls() = %v", calls)
	}
}

func TestParse_ParForEmptyListRejected(t *testing.T) {
	src := `mission m {
  budget 1k tokens
  emit par for x in [] { f(x) }
}`
	_, d := Parse("t.steer", src)
	if d == nil || !strings.Contains(d.Msg, "at least one item") {
		t.Fatalf("diag = %v", d)
	}
}

func TestCheck_ParForVarScoping(t *testing.T) {
	// The loop variable resolves inside the body...
	prog := parseOK(t, parSrc)
	if diags := Check(prog, parSrc); HasErrors(diags) {
		t.Fatalf("var must be visible in body: %s", renderAll(diags))
	}
	// ...but not outside it.
	src := `agent fn scan(lens: Text) -> Text
  prompt """${lens}"""
mission m {
  budget 1k tokens
  let raw = par for lens in ["a"] { scan(lens) }
  emit scan(lens)
}`
	prog = parseOK(t, src)
	joined := renderAll(Check(prog, src))
	if !strings.Contains(joined, `unknown name "lens"`) {
		t.Errorf("loop var must not leak, got:\n%s", joined)
	}
}

const untilSrc = `agent fn find(q: Text) -> Text
  prompt """FIND ${q}"""

mission discover {
  budget 50k tokens
  until dry(2) {
    let found = find("bugs")
  }
  emit found
}`

func TestParse_UntilDry(t *testing.T) {
	prog := parseOK(t, untilSrc)
	if diags := Check(prog, untilSrc); HasErrors(diags) {
		t.Fatalf("check: %s", renderAll(diags))
	}
	if len(prog.Mission.Stmts) != 2 {
		t.Fatalf("stmts = %d, want 2 (until + emit)", len(prog.Mission.Stmts))
	}
	us, ok := prog.Mission.Stmts[0].(*UntilStmt)
	if !ok {
		t.Fatalf("stmt[0] = %#v, want UntilStmt", prog.Mission.Stmts[0])
	}
	if us.N != 2 {
		t.Errorf("N = %d, want 2", us.N)
	}
	if len(us.Body) != 1 {
		t.Fatalf("until body = %d stmt(s), want 1", len(us.Body))
	}
	if _, ok := us.Body[0].(*LetStmt); !ok {
		t.Errorf("body stmt = %#v, want LetStmt", us.Body[0])
	}
	// The body call is on the static plan (it appears once; it runs per round).
	if calls := prog.Mission.Calls(); len(calls) != 1 || calls[0].Name != "find" {
		t.Errorf("Calls() = %v", calls)
	}
}

func TestParse_UntilEmptyBodyRejected(t *testing.T) {
	src := `mission m {
  budget 10k tokens
  until dry(2) { }
}`
	_, d := Parse("t.steer", src)
	if d == nil || !strings.Contains(d.Msg, "until body is empty") {
		t.Fatalf("diag = %v, want empty-until-body error", d)
	}
}

func TestParse_UntilZeroDryRoundsRejected(t *testing.T) {
	src := strings.Replace(untilSrc, "dry(2)", "dry(0)", 1)
	_, d := Parse("t.steer", src)
	if d == nil || !strings.Contains(d.Msg, "at least one dry round") {
		t.Fatalf("diag = %v, want dry-round-count error", d)
	}
}

func TestCheck_UntilWithoutBudgetGuardIsError(t *testing.T) {
	// A duration bound alone does not guard a discovery loop.
	src := strings.Replace(untilSrc, "budget 50k tokens", "budget 5min", 1)
	prog := parseOK(t, src)
	diags := Check(prog, src)
	if !HasErrors(diags) {
		t.Fatal("guardless until loop must not compile")
	}
	if joined := renderAll(diags); !strings.Contains(joined, "no budget guard") {
		t.Errorf("diags = %s", joined)
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

const judgeSrc = `agent fn propose(topic: Text) -> Text
  prompt """Propose a claim about ${topic}."""

agent fn refute(lens: Text, claim: Text) -> Text
  prompt """Refute through the ${lens} lens: ${claim}"""

mission verified {
  budget 50k tokens
  let claim = propose("caching")
  let real = judge claim by refute("correctness"), refute("perf") require 2 of 2
  emit real
}`

func TestParse_Judge(t *testing.T) {
	prog := parseOK(t, judgeSrc)
	if diags := Check(prog, judgeSrc); HasErrors(diags) {
		t.Fatalf("check: %s", renderAll(diags))
	}
	let := prog.Mission.Stmts[1].(*LetStmt)
	j, ok := let.Expr.(*JudgeExpr)
	if !ok {
		t.Fatalf("expr = %#v", let.Expr)
	}
	if j.K != 2 || j.N != 2 || len(j.By) != 2 {
		t.Errorf("judge = K%d N%d by%d", j.K, j.N, len(j.By))
	}
	if len(j.By[0].Args) != 1 {
		t.Errorf("by args = %d, want 1 (value appended at runtime)", len(j.By[0].Args))
	}
	// Calls(): propose + 2 refuters = 3 call sites.
	if n := len(prog.Mission.Calls()); n != 3 {
		t.Errorf("Calls() = %d, want 3", n)
	}
}

func TestCheck_JudgeArityAndCounts(t *testing.T) {
	// N mismatch
	src := strings.Replace(judgeSrc, "require 2 of 2", "require 2 of 3", 1)
	prog := parseOK(t, src)
	if joined := renderAll(Check(prog, src)); !strings.Contains(joined, "N must match") {
		t.Errorf("diags:\n%s", joined)
	}
	// Verifier written with full arity (forgot the appended value)
	src = strings.Replace(judgeSrc, `refute("correctness"), refute("perf")`, `refute("correctness", claim), refute("perf")`, 1)
	prog = parseOK(t, src)
	if joined := renderAll(Check(prog, src)); !strings.Contains(joined, "receives the judged value") {
		t.Errorf("diags:\n%s", joined)
	}
}

func TestParseVerdict(t *testing.T) {
	cases := map[string]Verdict{
		"Analysis...\nSTANDS":                    VerdictStands,
		"it stands to reason, but...\nREFUTED":   VerdictRefuted,
		"The claim REFUTED my doubts.\n\nSTANDS": VerdictStands,
		"stands":                                 VerdictStands,
		"I cannot decide.":                       VerdictUnclear,
		// Substrings must never read as a verdict — a contract-violating
		// verifier counts as unclear (refuted), not STANDS.
		"The author clearly understands the tradeoffs.": VerdictUnclear,
		"the claim withstands scrutiny":                 VerdictUnclear,
		"It withstands my analysis.\nSTANDS":            VerdictStands,
		// Both keywords in analysis prose, final-line verdict wins.
		"Does it STANDS or is it REFUTED? Hard to say.\nREFUTED": VerdictRefuted,
		// Word-bounded prose verdict is tolerated as a fallback.
		"I conclude the claim stands.": VerdictStands,
		"  STANDS  ":                   VerdictStands,
		"":                             VerdictUnclear,
	}
	for in, want := range cases {
		if got := ParseVerdict(in); got != want {
			t.Errorf("ParseVerdict(%q) = %v, want %v", in, got, want)
		}
	}
}

const onHandlerSrc = `agent fn think(q: Text) -> Text
  prompt """Think about ${q}."""

mission compensated {
  budget 20k tokens
  let real = think("hard")
  emit real
  on BudgetExceeded -> emit real; halt
}`

func TestParse_OnHandler(t *testing.T) {
	prog := parseOK(t, onHandlerSrc)
	if diags := Check(prog, onHandlerSrc); HasErrors(diags) {
		t.Fatalf("check: %s", renderAll(diags))
	}
	m := prog.Mission
	if len(m.Handlers) != 1 {
		t.Fatalf("handlers = %d, want 1", len(m.Handlers))
	}
	h := m.Handlers[0]
	if h.ErrName != "BudgetExceeded" {
		t.Errorf("handler on %q, want BudgetExceeded", h.ErrName)
	}
	if len(h.Body) != 2 {
		t.Fatalf("handler body = %d stmt(s), want emit + halt", len(h.Body))
	}
	if _, ok := h.Body[0].(*EmitStmt); !ok {
		t.Errorf("body[0] = %#v, want EmitStmt", h.Body[0])
	}
	if _, ok := h.Body[1].(*HaltStmt); !ok {
		t.Errorf("body[1] = %#v, want HaltStmt", h.Body[1])
	}
	// This handler makes no calls, so the plan is the main body's one call.
	if calls := m.Calls(); len(calls) != 1 {
		t.Errorf("Calls() = %d, want the main-body call only", len(calls))
	}
}

func TestMissionCalls_IncludesHandlerCompensationCalls(t *testing.T) {
	src := `agent fn think(q: Text) -> Text
  prompt """Think about ${q}."""

agent fn summarize(q: Text) -> Text
  prompt """Summarize ${q}."""

mission compensated {
  budget 20k tokens
  let real = think("hard")
  emit real
  on BudgetExceeded -> let partial = summarize("what we have"); emit partial; halt
}`
	prog := parseOK(t, src)
	if diags := Check(prog, src); HasErrors(diags) {
		t.Fatalf("check: %s", renderAll(diags))
	}
	var names []string
	for _, c := range prog.Mission.Calls() {
		names = append(names, c.Name)
	}
	// Compensation calls are on the static plan too, listed after the body.
	if got, want := strings.Join(names, ","), "think,summarize"; got != want {
		t.Errorf("Calls() = %s, want %s", got, want)
	}
}

func TestParse_HaltOutsideHandlerRejected(t *testing.T) {
	src := strings.Replace(onHandlerSrc, "emit real\n  on BudgetExceeded -> emit real; halt", "halt", 1)
	_, d := Parse("t.steer", src)
	if d == nil || !strings.Contains(d.Msg, `"halt"`) {
		t.Fatalf("diag = %v, want halt rejected outside an on-handler", d)
	}
}

func TestCheck_OnHandlerUnknownErrorNameSuggests(t *testing.T) {
	src := strings.Replace(onHandlerSrc, "on BudgetExceeded", "on BudgetExceded", 1)
	prog := parseOK(t, src)
	joined := renderAll(Check(prog, src))
	if !strings.Contains(joined, `unknown error name "BudgetExceded"`) || !strings.Contains(joined, `did you mean "BudgetExceeded"`) {
		t.Errorf("diags = %s", joined)
	}
}

func TestCheck_DuplicateOnHandlerRejected(t *testing.T) {
	src := strings.Replace(onHandlerSrc, "on BudgetExceeded -> emit real; halt",
		"on BudgetExceeded -> emit real; halt\n  on BudgetExceeded -> halt", 1)
	prog := parseOK(t, src)
	joined := renderAll(Check(prog, src))
	if !strings.Contains(joined, "duplicate handler for BudgetExceeded") {
		t.Errorf("diags = %s", joined)
	}
}

// refineSrc is the M5 slice: apply demands [Fix@2] — findings that passed
// a `judge ... require 2 of n` — and the mission earns that level with a
// two-verifier judge before the call.
const refineSrc = `
type Fix { title: Text, file: Text }

agent fn find(dim: Text) -> [Fix]
  prompt """Find fixes for ${dim}."""

agent fn refute(lens: Text, finding: Text) -> Text
  prompt """Via the ${lens} lens, try to refute: ${finding}"""

agent fn apply(fixes: [Fix@2]) -> Text
  prompt """Apply ${fixes}."""

mission refined {
  budget 50k tokens
  let raw = find("security")
  let judged = judge raw by refute("correctness"), refute("repro") require 2 of 2
  emit apply(judged)
}
`

func TestParse_RefinementRoundTrip(t *testing.T) {
	prog := parseOK(t, refineSrc)
	p := prog.Agent("apply").Params[0]
	if p.Type != "[Fix]" || p.Refine != 2 {
		t.Errorf("param = type %q refine @%d, want [Fix] @2", p.Type, p.Refine)
	}
	if got := p.TypeString(); got != "[Fix@2]" {
		t.Errorf("TypeString() = %q, want [Fix@2]", got)
	}
	if diags := Check(prog, refineSrc); HasErrors(diags) {
		t.Fatalf("@2 into @2 must check clean, got: %s", renderAll(diags))
	}

	// The suffix also parses on a bare (non-list) type.
	bare := strings.Replace(refineSrc, "[Fix@2]", "Fix@2", 1)
	p = parseOK(t, bare).Agent("apply").Params[0]
	if p.Type != "Fix" || p.Refine != 2 || p.TypeString() != "Fix@2" {
		t.Errorf("bare param = type %q refine @%d (%s), want Fix @2", p.Type, p.Refine, p.TypeString())
	}
}

func TestCheck_RefinementSubtypeAccepted(t *testing.T) {
	// @2 <: @1 — a value judged harder than the parameter demands passes.
	src := strings.Replace(refineSrc, "[Fix@2]", "[Fix@1]", 1)
	prog := parseOK(t, src)
	if diags := Check(prog, src); HasErrors(diags) {
		t.Fatalf("@2 into @1 must check clean, got: %s", renderAll(diags))
	}
}

func TestCheck_RefinementUnjudgedRejectedWithHint(t *testing.T) {
	// @0 into @2 — passing the raw findings straight to apply is the M5
	// accept criterion: a check-time diagnostic with a fix-it hint naming
	// the missing judge step.
	src := strings.Replace(refineSrc, "emit apply(judged)", "emit apply(raw)", 1)
	prog := parseOK(t, src)
	diags := Check(prog, src)
	if !HasErrors(diags) {
		t.Fatal("unjudged @0 into @2 must be a check-time error")
	}
	joined := renderAll(diags)
	if !strings.Contains(joined, `apply expects fixes: [Fix@2] but "raw" is @0 (unjudged)`) {
		t.Errorf("diags = %s, want the @0-into-@2 mismatch", joined)
	}
	if !strings.Contains(joined, "add the missing judge step first") ||
		!strings.Contains(joined, "judge raw by <verifier>, ... require 2 of 2") {
		t.Errorf("diags = %s, want the fix-it hint naming the missing judge step", joined)
	}
}

func TestCheck_JudgeVerifierLastParamRefinementRejected(t *testing.T) {
	// The verifier's last parameter receives the judged value at run time,
	// so a declared @k on it constrains the value entering the judge — an
	// unjudged @0 value must not silently satisfy a Text@1 parameter.
	src := strings.Replace(refineSrc, "finding: Text", "finding: Text@1", 1)
	prog := parseOK(t, src)
	diags := Check(prog, src)
	if !HasErrors(diags) {
		t.Fatal("judging an unjudged @0 value with a verifier demanding @1 must be a check-time error")
	}
	joined := renderAll(diags)
	if !strings.Contains(joined, `refute expects finding: Text@1 but "raw" is @0 (unjudged)`) {
		t.Errorf("diags = %s, want the @0-into-@1 mismatch on the judged value", joined)
	}
}

func TestCheck_JudgeVerifierLastParamRefinementSatisfied(t *testing.T) {
	// A value that already carries @1 flows cleanly into a verifier whose
	// last parameter demands Text@1.
	src := strings.Replace(refineSrc, "finding: Text", "finding: Text@1", 1)
	src = strings.Replace(src, "mission refined {", `agent fn screen(lens: Text, finding: Text) -> Text
  prompt """Screen via ${lens}: ${finding}"""

mission refined {`, 1)
	src = strings.Replace(src,
		`let judged = judge raw by refute("correctness"), refute("repro") require 2 of 2`,
		`let once = judge raw by screen("triage") require 1 of 1
  let judged = judge once by refute("correctness"), refute("repro") require 2 of 2`, 1)
	prog := parseOK(t, src)
	if diags := Check(prog, src); HasErrors(diags) {
		t.Fatalf("@1 into a verifier demanding @1 must check clean, got: %s", renderAll(diags))
	}
}

func TestCheck_ParForJudgeBodyPropagatesRefinement(t *testing.T) {
	// A par-for whose body is a judge yields a join of values that each
	// passed `require 2 of 2`, so the binding satisfies an @2 parameter
	// without re-judging the join — the static prediction stays in
	// lockstep with the runtime tag.
	src := strings.Replace(refineSrc,
		`let judged = judge raw by refute("correctness"), refute("repro") require 2 of 2`,
		`let judged = par for f in [raw] { judge f by refute("correctness"), refute("repro") require 2 of 2 }`, 1)
	prog := parseOK(t, src)
	if diags := Check(prog, src); HasErrors(diags) {
		t.Fatalf("par-for-over-judge @2 into @2 must check clean, got: %s", renderAll(diags))
	}

	// The loop variable itself stays raw inside the body: a par-for that
	// just echoes the item earns no level, even when the variable shadows
	// a judged outer binding.
	echo := strings.Replace(src,
		`{ judge f by refute("correctness"), refute("repro") require 2 of 2 }`,
		`{ f }`, 1)
	prog = parseOK(t, echo)
	diags := Check(prog, echo)
	if !HasErrors(diags) {
		t.Fatal("par-for echoing the raw loop variable into @2 must be a check-time error")
	}
	if joined := renderAll(diags); !strings.Contains(joined, "@0 (unjudged)") {
		t.Errorf("diags = %s, want the @0-into-@2 mismatch", joined)
	}
}

func TestParse_RefinementOnReturnTypeRejected(t *testing.T) {
	src := strings.Replace(refineSrc, "-> [Fix]", "-> [Fix@2]", 1)
	_, d := Parse("t.steer", src)
	if d == nil || !strings.Contains(d.Msg, "return type cannot carry") {
		t.Fatalf("diag = %v, want return-type refinement rejection", d)
	}
}

func TestParse_LensSetRoundTrip(t *testing.T) {
	// The provenance-lattice annotation: named lenses instead of a bare
	// count. The set implies its count (Refine == len(Lenses)) and the
	// checker enforces the named half on top: the judge's 2-of-2 lens set
	// must cover the demanded names for the mission to check clean.
	src := strings.Replace(refineSrc, "[Fix@2]", "[Fix @ {Security, Repro}]", 1)
	src = strings.Replace(src, `refute("correctness"), refute("repro")`, `refute("Security"), refute("Repro")`, 1)
	prog := parseOK(t, src)
	p := prog.Agent("apply").Params[0]
	if p.Type != "[Fix]" || p.Refine != 2 {
		t.Errorf("param = type %q refine @%d, want [Fix] @2", p.Type, p.Refine)
	}
	if got := strings.Join(p.Lenses, ","); got != "Security,Repro" {
		t.Errorf("lenses = %q, want Security,Repro", got)
	}
	if got := p.TypeString(); got != "[Fix@{Security, Repro}]" {
		t.Errorf("TypeString() = %q, want [Fix@{Security, Repro}]", got)
	}
	if diags := Check(prog, src); HasErrors(diags) {
		t.Fatalf("a matching 2-of-2 lens set into a 2-lens demand must check clean, got: %s", renderAll(diags))
	}

	// The canonical TypeString rendering re-parses to the same annotation.
	again := strings.Replace(refineSrc, "[Fix@2]", "[Fix@{Security, Repro}]", 1)
	p = parseOK(t, again).Agent("apply").Params[0]
	if strings.Join(p.Lenses, ",") != "Security,Repro" {
		t.Errorf("re-parsed lenses = %q, want Security,Repro", strings.Join(p.Lenses, ","))
	}
}

func TestCheck_LensSetCountProjectionEnforced(t *testing.T) {
	// A 3-lens set demands at least @3; the mission only earns @2, so the
	// existing count check fires and names the lens-set annotation.
	src := strings.Replace(refineSrc, "[Fix@2]", "[Fix @ {Security, Repro, Correctness}]", 1)
	prog := parseOK(t, src)
	diags := Check(prog, src)
	if !HasErrors(diags) {
		t.Fatal("@2 into a 3-lens set must fail its count projection at check time")
	}
	if joined := renderAll(diags); !strings.Contains(joined, "[Fix@{Security, Repro, Correctness}]") {
		t.Errorf("diags = %s, want the lens-set annotation named", joined)
	}
}

func TestCheck_LensSubsumptionMissingLensRejected(t *testing.T) {
	// The named half of the lattice: the count projection passes (2 of 2
	// into a 2-lens demand) but the judge's standing set does not cover
	// every demanded lens, so the checker names exactly what is missing.
	src := strings.Replace(refineSrc, "[Fix@2]", "[Fix @ {Security, Repro}]", 1)
	src = strings.Replace(src, `refute("correctness"), refute("repro")`, `refute("Security"), refute("perf")`, 1)
	prog := parseOK(t, src)
	diags := Check(prog, src)
	if !HasErrors(diags) {
		t.Fatal("a lens set not covering the demanded lenses must be a check-time error")
	}
	joined := renderAll(diags)
	if !strings.Contains(joined, `"judged" carries lens set {Security, perf}`) ||
		!strings.Contains(joined, "missing {Repro}") {
		t.Errorf("diags = %s, want the carried set and the missing lens named", joined)
	}
}

func TestCheck_LensSubsumptionRequiresKEqualsN(t *testing.T) {
	// With K < N no specific lens is guaranteed to have stood — only the
	// count. Even though every demanded lens appears among the verifiers
	// and the count projection passes, the demand must be rejected with
	// the K == N requirement explained.
	src := strings.Replace(refineSrc, "[Fix@2]", "[Fix @ {Security, Repro}]", 1)
	src = strings.Replace(src,
		`judge raw by refute("correctness"), refute("repro") require 2 of 2`,
		`judge raw by refute("Security"), refute("Repro"), refute("perf") require 2 of 3`, 1)
	prog := parseOK(t, src)
	diags := Check(prog, src)
	if !HasErrors(diags) {
		t.Fatal("a K-of-N judge with K < N must not satisfy a named-lens demand")
	}
	joined := renderAll(diags)
	if !strings.Contains(joined, `"judged" guarantees no named lens`) ||
		!strings.Contains(joined, "require N of N") {
		t.Errorf("diags = %s, want the K == N requirement explained", joined)
	}
}

func TestCheck_LensSubsumptionSupersetThroughRetryAccepted(t *testing.T) {
	// A 3-of-3 judge whose lens set is a strict superset of the demand
	// checks clean, and the guarantee survives a retry wrapper the same
	// way the @k count does (a value only arrives if some attempt passed).
	src := strings.Replace(refineSrc, "[Fix@2]", "[Fix @ {Security, Repro}]", 1)
	src = strings.Replace(src,
		`judge raw by refute("correctness"), refute("repro") require 2 of 2`,
		`judge raw by refute("Security"), refute("Repro"), refute("perf") require 3 of 3
              retry 1 on JudgeRejected`, 1)
	prog := parseOK(t, src)
	if diags := Check(prog, src); HasErrors(diags) {
		t.Fatalf("a superset lens set through retry must check clean, got: %s", renderAll(diags))
	}
}

func TestParse_LensSetOnReturnTypeRejected(t *testing.T) {
	src := strings.Replace(refineSrc, "-> [Fix]", "-> [Fix @ {Security}]", 1)
	_, d := Parse("t.steer", src)
	if d == nil || !strings.Contains(d.Msg, "return type cannot carry an @{Security} refinement") {
		t.Fatalf("diag = %v, want return-type lens-set rejection", d)
	}
}

func TestParse_LensSetRejectsEmptyAndDuplicate(t *testing.T) {
	empty := strings.Replace(refineSrc, "[Fix@2]", "[Fix @ {}]", 1)
	if _, d := Parse("t.steer", empty); d == nil {
		t.Error("empty lens set must not parse")
	}
	dup := strings.Replace(refineSrc, "[Fix@2]", "[Fix @ {Security, Security}]", 1)
	if _, d := Parse("t.steer", dup); d == nil || !strings.Contains(d.Msg, `duplicate lens "Security"`) {
		t.Errorf("diag = %v, want duplicate-lens rejection", d)
	}
}

func TestCheck_ParForJudgeBodyPropagatesLensSet(t *testing.T) {
	src := strings.Replace(refineSrc, "[Fix@2]", "[Fix @ {Security, Repro}]", 1)
	src = strings.Replace(src,
		`let judged = judge raw by refute("correctness"), refute("repro") require 2 of 2`,
		`let judged = par for f in [raw] { judge f by refute("Security"), refute("Repro") require 2 of 2 }`, 1)
	prog := parseOK(t, src)
	if diags := Check(prog, src); HasErrors(diags) {
		t.Fatalf("par-for-over-judge lens set into @{Security, Repro} must check clean, got: %s", renderAll(diags))
	}
}
