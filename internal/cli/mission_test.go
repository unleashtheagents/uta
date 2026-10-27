package cli

import (
	"encoding/json"
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
	prog, _, _, ok := loadSteerProgram(writeSteer(t, missionTestSrc), &errw)
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
	_, _, _, ok := loadSteerProgram(writeSteer(t, src), &errw)
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
	_, _, _, ok := loadSteerProgram(filepath.Join(t.TempDir(), "nope.steer"), &errw)
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

const policyBoundSrc = `
agent fn work(q: Text) -> Text
  prompt """WORK ${q}"""

mission bound {
  policy dev
  budget 10k tokens
  emit work("x")
}
`

// TestResolveMissionPolicy_LoadsProfileCaps covers the `policy P` resolution
// chain: a mission binding `policy dev` gets the on-disk dev profile with its
// capability grants; an unknown name is a check-style diagnostic with a
// suggestion; a mission without a policy resolves to nothing.
func TestResolveMissionPolicy_LoadsProfileCaps(t *testing.T) {
	home := t.TempDir()
	profDir := filepath.Join(home, "profiles")
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatal(err)
	}
	devYAML := "name: dev\nallowed_tools:\n  - Read\n  - Bash(git diff*)\ndenied_tools:\n  - Bash(git push*)\n"
	if err := os.WriteFile(filepath.Join(profDir, "dev.yaml"), []byte(devYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	app := &App{GlobalHome: home}
	var errw strings.Builder

	prog, d := steer.Parse("bound.steer", policyBoundSrc)
	if d != nil {
		t.Fatal(d.Render())
	}
	pol, diags := resolveMissionPolicy(app, prog, policyBoundSrc, &errw)
	if len(diags) > 0 {
		t.Fatalf("resolve: %s", diags[0].Render())
	}
	if pol == nil || pol.Name != "dev" {
		t.Fatalf("policy = %+v, want the dev profile", pol)
	}
	if len(pol.AllowedTools) != 2 || len(pol.DeniedTools) != 1 || pol.DeniedTools[0] != "Bash(git push*)" {
		t.Errorf("caps = allow %v deny %v, want the dev profile's grants", pol.AllowedTools, pol.DeniedTools)
	}

	// Unknown policy name → diagnostic with a did-you-mean suggestion.
	unknown := strings.Replace(policyBoundSrc, "policy dev", "policy dve", 1)
	prog, d = steer.Parse("bound.steer", unknown)
	if d != nil {
		t.Fatal(d.Render())
	}
	pol, diags = resolveMissionPolicy(app, prog, unknown, &errw)
	if pol != nil || len(diags) == 0 {
		t.Fatalf("unknown policy should not resolve (pol=%+v diags=%v)", pol, diags)
	}
	if !strings.Contains(diags[0].Msg, `unknown policy "dve"`) || !strings.Contains(diags[0].Msg, `did you mean "dev"`) {
		t.Errorf("diag = %q, want unknown-policy message with suggestion", diags[0].Msg)
	}

	// No policy declared → nothing to resolve, no diagnostic.
	prog, d = steer.Parse("plain.steer", missionTestSrc)
	if d != nil {
		t.Fatal(d.Render())
	}
	if pol, diags = resolveMissionPolicy(app, prog, missionTestSrc, &errw); pol != nil || len(diags) > 0 {
		t.Errorf("plain mission: pol=%+v diags=%v, want nil/nil", pol, diags)
	}
}

const capRaiseSrc = `
agent fn deploy(q: Text) -> Text
  prompt """DEPLOY ${q}"""

mission raise {
  policy dev
  budget 10k tokens
  with cap "Bash(git push*)" from policy release {
    let out = deploy("v2")
  }
  emit out
}
`

// TestResolveMissionPolicy_ChecksCapRaises is the M3 accept criterion at the
// CLI seam: `uta mission check` (which funnels through resolveMissionPolicy)
// rejects a `with cap` raise whose source policy does not actually grant the
// capability — the failure happens at check time, not at run time.
func TestResolveMissionPolicy_ChecksCapRaises(t *testing.T) {
	home := t.TempDir()
	profDir := filepath.Join(home, "profiles")
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatal(err)
	}
	devYAML := "name: dev\nallowed_tools:\n  - Read\n"
	releaseYAML := "name: release\nallowed_tools:\n  - Read\n  - Bash(git push*)\n"
	for name, body := range map[string]string{"dev.yaml": devYAML, "release.yaml": releaseYAML} {
		if err := os.WriteFile(filepath.Join(profDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app := &App{GlobalHome: home}
	var errw strings.Builder

	// The raise borrows from a policy that grants the cap → resolves clean.
	prog, d := steer.Parse("raise.steer", capRaiseSrc)
	if d != nil {
		t.Fatal(d.Render())
	}
	pol, diags := resolveMissionPolicy(app, prog, capRaiseSrc, &errw)
	if len(diags) > 0 {
		t.Fatalf("granted raise should resolve: %s", diags[0].Render())
	}
	if pol == nil || pol.Name != "dev" {
		t.Fatalf("policy = %+v, want the dev profile", pol)
	}

	// The raise borrows a cap its source policy does not grant → check-time
	// diagnostic.
	notGranted := strings.Replace(capRaiseSrc, "from policy release", "from policy dev", 1)
	prog, d = steer.Parse("raise.steer", notGranted)
	if d != nil {
		t.Fatal(d.Render())
	}
	pol, diags = resolveMissionPolicy(app, prog, notGranted, &errw)
	if pol != nil || len(diags) == 0 {
		t.Fatalf("ungranted raise must not resolve (pol=%+v diags=%v)", pol, diags)
	}
	if !strings.Contains(diags[0].Msg, `does not grant capability "Bash(git push*)"`) {
		t.Errorf("diag = %q, want does-not-grant message", diags[0].Msg)
	}

	// Unknown source policy → diagnostic with a suggestion.
	unknown := strings.Replace(capRaiseSrc, "from policy release", "from policy relaese", 1)
	prog, d = steer.Parse("raise.steer", unknown)
	if d != nil {
		t.Fatal(d.Render())
	}
	if _, diags = resolveMissionPolicy(app, prog, unknown, &errw); len(diags) == 0 ||
		!strings.Contains(diags[0].Msg, `unknown policy "relaese"`) || !strings.Contains(diags[0].Msg, `did you mean "release"`) {
		t.Errorf("diags = %v, want unknown-policy message with suggestion", diags)
	}
}

func runMissionCheck(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := newMissionCheckCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out, errb strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errb.String(), err
}

func TestMissionCheck_JSONClean(t *testing.T) {
	path := writeSteer(t, missionTestSrc)
	stdout, stderr, err := runMissionCheck(t, "--json", path)
	if err != nil {
		t.Fatalf("check failed: %v\n%s", err, stderr)
	}
	if want := `{"ok":true,"diagnostics":[]}` + "\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func TestMissionCheck_JSONTwoErrors(t *testing.T) {
	// Two check errors: an unknown function and a mission with no budget.
	src := strings.Replace(missionTestSrc, `greet("world")`, `gret("world")`, 1)
	src = strings.Replace(src, "  budget 20k tokens, $1, 5min\n", "", 1)
	path := writeSteer(t, src)
	stdout, _, err := runMissionCheck(t, "--json", path)
	if err == nil {
		t.Fatal("check must fail on a bad program")
	}
	// Golden: two errors, plus the never-called warning the misspelling
	// causes. Warnings ride along in the JSON just as they do in text mode.
	want := `{"ok":false,"diagnostics":[` +
		`{"file":"test.steer","line":9,"col":1,"severity":"error","message":"mission \"hello_world\" declares no budget — steer refuses to run an unbounded mission (add e.g. ` + "`budget 50k tokens, 10min`" + `)"},` +
		`{"file":"test.steer","line":10,"col":18,"severity":"error","message":"unknown function \"gret\" (did you mean \"greet\"?)"},` +
		`{"file":"test.steer","line":2,"col":1,"severity":"warning","message":"agent fn \"greet\" is declared but never called"}]}` + "\n"
	if stdout != want {
		t.Errorf("stdout = %s\nwant %s", stdout, want)
	}
}

func TestMissionCheck_JSONParseError(t *testing.T) {
	path := writeSteer(t, "mission broken {\n")
	stdout, _, err := runMissionCheck(t, "--json", path)
	if err == nil {
		t.Fatal("check must fail on a syntax error")
	}
	var res missionCheckResult
	if uerr := json.Unmarshal([]byte(stdout), &res); uerr != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", uerr, stdout)
	}
	if res.OK || len(res.Diagnostics) != 1 || res.Diagnostics[0].Severity != "error" {
		t.Errorf("parse failure should yield ok=false with one error diagnostic:\n%s", stdout)
	}
}

func TestLoadSteerProgram_CountsWarnings(t *testing.T) {
	src := strings.Replace(missionTestSrc, "  emit greeting\n", "", 1) // missing emit + unused let → 2 warnings
	var errw strings.Builder
	_, warnings, _, ok := loadSteerProgram(writeSteer(t, src), &errw)
	if !ok {
		t.Fatalf("warnings must not block loading: %s", errw.String())
	}
	if warnings != 2 {
		t.Errorf("warnings = %d, want 2:\n%s", warnings, errw.String())
	}
}
