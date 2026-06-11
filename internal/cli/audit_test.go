package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/config"
	"github.com/unleashtheagents/uta/internal/engine"
	"github.com/unleashtheagents/uta/internal/profile"
)

// TestBuildToolSpecs_BuiltinsResolveToTheirAdapters verifies that every
// name returned by engine.KnownBuiltinTools (plus the "forge" alias)
// flows through buildToolSpecs + ResolveToolSpec to its dedicated
// adapter — not the generic "findings" shell-tool fallback.
//
// This is the regression guard for the bug where buildToolSpecs's
// switch listed slither/mythril/forge-test/forge but not aderyn, so
// `uta audit --tool aderyn` (and the audit MissionProfile that lists
// aderyn) silently degraded to a raw shell invocation with the
// generic findings parser instead of the aderyn JSON adapter.
func TestBuildToolSpecs_BuiltinsResolveToTheirAdapters(t *testing.T) {
	names := append([]string{"forge"}, engine.KnownBuiltinTools()...)
	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			specs := buildToolSpecs([]string{name}, time.Minute)
			if len(specs) != 1 {
				t.Fatalf("expected 1 spec, got %d", len(specs))
			}
			got := engine.ResolveToolSpec(specs[0])
			// The dedicated adapter MUST be set — never "findings"
			// (the generic shell-tool fallback) or empty.
			if got.Adapter == "findings" || got.Adapter == "" {
				t.Fatalf("built-in %q resolved to adapter %q; expected its dedicated adapter (not the shell-tool fallback)",
					name, got.Adapter)
			}
			if got.ShellMode {
				t.Errorf("built-in %q resolved with ShellMode=true; built-ins should exec directly", name)
			}
			if got.Cmd == "" {
				t.Errorf("built-in %q resolved with empty Cmd; ResolveToolSpec should have filled it", name)
			}
		})
	}
}

func TestBuildToolSpecs_AderynUsesAderynAdapter(t *testing.T) {
	specs := buildToolSpecs([]string{"aderyn"}, time.Minute)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	got := engine.ResolveToolSpec(specs[0])
	if got.Adapter != "aderyn" {
		t.Fatalf("aderyn must resolve to the 'aderyn' adapter, got %q", got.Adapter)
	}
	if got.Cmd != "aderyn" {
		t.Errorf("aderyn must resolve to the 'aderyn' binary, got %q", got.Cmd)
	}
	if len(got.Args) < 3 || got.Args[0] != "." || got.Args[1] != "--output" || got.Args[2] != "-" {
		t.Errorf("aderyn must resolve with `. --output -` default args, got %v", got.Args)
	}
}

func TestBuildToolSpecs_ExplicitIDEqualsCmdFallsThroughToShell(t *testing.T) {
	// `lint=npm run lint` — an explicit id= prefix MUST drop into the
	// shell-tool path even when the cmd happens to share a name with a
	// built-in (we shouldn't override the user's choice of id).
	specs := buildToolSpecs([]string{"my-aderyn=aderyn /custom/path"}, time.Minute)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	got := specs[0]
	if got.ID != "my-aderyn" {
		t.Errorf("explicit id should win; got ID=%q", got.ID)
	}
	if !got.ShellMode {
		t.Errorf("explicit id=cmd form should run via shell; ShellMode=%v", got.ShellMode)
	}
	if got.Adapter != "findings" {
		t.Errorf("explicit id=cmd form should use the generic findings adapter; got %q", got.Adapter)
	}
}

func TestBuildToolSpecs_UnknownNameFallsThroughToShell(t *testing.T) {
	specs := buildToolSpecs([]string{"some-random-tool --flag"}, time.Minute)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec, got %d", len(specs))
	}
	got := specs[0]
	if !got.ShellMode {
		t.Errorf("unknown tool should run via shell")
	}
	if got.ID != "some-random-tool" {
		t.Errorf("ID should be derived from first field; got %q", got.ID)
	}
	if got.Adapter != "findings" {
		t.Errorf("unknown tool should default to 'findings' adapter; got %q", got.Adapter)
	}
}

func TestBuildToolSpecs_EmptyAndWhitespaceSkipped(t *testing.T) {
	specs := buildToolSpecs([]string{"", "   ", "slither"}, time.Minute)
	if len(specs) != 1 {
		t.Fatalf("expected 1 spec after filtering empties, got %d", len(specs))
	}
	if specs[0].ID != "slither" {
		t.Errorf("expected slither to be the surviving spec, got %q", specs[0].ID)
	}
}

// TestApplyVibeToCritics_AppendsSectionToEveryPrompt is the integration
// guard for the file→critic prompt wiring. It writes a vibe file via the
// profile package, runs the audit-side glue, and verifies each critic's
// Prompt now ends with the `## Current vibe` section. Without this test
// a refactor of the prompt-assembly path could silently drop vibes.
func TestApplyVibeToCritics_AppendsSectionToEveryPrompt(t *testing.T) {
	dir := t.TempDir()
	if err := profile.AppendVibe(dir, "audit", "be more aggressive"); err != nil {
		t.Fatalf("seed vibe: %v", err)
	}
	in := []engine.CriticSpec{
		{ID: "a", Title: "A", Prompt: "alpha prompt"},
		{ID: "b", Title: "B", Prompt: "beta prompt\n"},
	}
	out, err := applyVibeToCritics(dir, "audit", in)
	if err != nil {
		t.Fatalf("applyVibeToCritics: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("critic count changed: want %d, got %d", len(in), len(out))
	}
	for i, c := range out {
		if !strings.Contains(c.Prompt, "## Current vibe") {
			t.Errorf("critic[%d] missing vibe header: %q", i, c.Prompt)
		}
		if !strings.Contains(c.Prompt, "be more aggressive") {
			t.Errorf("critic[%d] missing vibe body: %q", i, c.Prompt)
		}
	}
	// Original prefix must be preserved verbatim — vibe is appended, not replaced.
	if !strings.HasPrefix(out[0].Prompt, "alpha prompt") {
		t.Errorf("critic[0] prefix mutated: %q", out[0].Prompt)
	}
	if !strings.HasPrefix(out[1].Prompt, "beta prompt") {
		t.Errorf("critic[1] prefix mutated: %q", out[1].Prompt)
	}
}

// TestApplyVibeToCritics_NoVibeIsNoOp ensures critic prompts are
// untouched when no vibe file exists for the mode (the common case).
func TestApplyVibeToCritics_NoVibeIsNoOp(t *testing.T) {
	dir := t.TempDir()
	in := []engine.CriticSpec{{ID: "a", Prompt: "alpha"}}
	out, err := applyVibeToCritics(dir, "audit", in)
	if err != nil {
		t.Fatalf("applyVibeToCritics: %v", err)
	}
	if len(out) != 1 || out[0].Prompt != "alpha" {
		t.Errorf("missing vibe should be a no-op; got %+v", out)
	}
}

// TestApplyVibeToCritics_EmptyInputs guards the early-return paths so a
// caller that passes a nil/empty slice or unset identifiers can't trip a
// read of a bogus path.
func TestApplyVibeToCritics_EmptyInputs(t *testing.T) {
	dir := t.TempDir()
	if out, err := applyVibeToCritics(dir, "audit", nil); err != nil || out != nil {
		t.Errorf("nil critics: want (nil, nil), got (%v, %v)", out, err)
	}
	in := []engine.CriticSpec{{ID: "a", Prompt: "alpha"}}
	if out, err := applyVibeToCritics("", "audit", in); err != nil || len(out) != 1 || out[0].Prompt != "alpha" {
		t.Errorf("empty projectRoot should be a no-op; got (%v, %v)", out, err)
	}
	if out, err := applyVibeToCritics(dir, "", in); err != nil || len(out) != 1 || out[0].Prompt != "alpha" {
		t.Errorf("empty modeName should be a no-op; got (%v, %v)", out, err)
	}
}

// TestParseStopCondition_AllAliases pins every documented spelling of the
// --stop-when flag to its engine constant. A typo in the switch (e.g. an
// underscore vs. dash) would silently fall through to the unknown-error
// branch and abort the run with a confusing message, so each alias is
// exercised explicitly.
func TestParseStopCondition_AllAliases(t *testing.T) {
	cases := []struct {
		in   string
		want engine.StopCondition
	}{
		{"", engine.StopAtNoHigh},
		{"no_high_findings", engine.StopAtNoHigh},
		{"no-high", engine.StopAtNoHigh},
		{"  NO-HIGH  ", engine.StopAtNoHigh}, // case + whitespace tolerant
		{"no_med_or_above", engine.StopAtNoMedOrAbove},
		{"no-med", engine.StopAtNoMedOrAbove},
		{"no_findings", engine.StopAtNoFindings},
		{"none", engine.StopAtNoFindings},
		{"max_iterations", engine.StopAtMaxIterations},
		{"always", engine.StopAtMaxIterations},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseStopCondition(tc.in)
			if err != nil {
				t.Fatalf("parseStopCondition(%q) returned error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("parseStopCondition(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseStopCondition_UnknownValueReturnsError(t *testing.T) {
	_, err := parseStopCondition("bogus")
	if err == nil {
		t.Fatal("parseStopCondition(\"bogus\") should return an error")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should include offending value; got %q", err.Error())
	}
}

// TestResolvePersonas_BuiltinsAlwaysPresent verifies the merge always
// surfaces every built-in critic even when no user personas exist.
func TestResolvePersonas_BuiltinsAlwaysPresent(t *testing.T) {
	app := &App{}
	out := resolvePersonas(app)
	for _, p := range engine.BuiltinPersonas {
		got, ok := out[p.ID]
		if !ok {
			t.Errorf("built-in persona %q missing from resolvePersonas output", p.ID)
			continue
		}
		if got.Prompt != p.Prompt {
			t.Errorf("built-in %q prompt mutated by resolver", p.ID)
		}
	}
}

// TestResolvePersonas_UserAddsWithoutClobber: a user persona with a new ID
// is added alongside built-ins; a same-ID user persona without Force does
// NOT replace the built-in (silent shadowing would be a footgun).
func TestResolvePersonas_UserAddsWithoutClobber(t *testing.T) {
	app := &App{UserPersonas: []*config.Persona{
		{ID: "fuzzer", Title: "Fuzzer", Prompt: "fuzz"},
		// Same id as a built-in but Force=false → must NOT override.
		{ID: "trail-of-bits", Title: "FAKE", Prompt: "fake prompt"},
	}}
	out := resolvePersonas(app)
	if got, ok := out["fuzzer"]; !ok || got.Prompt != "fuzz" {
		t.Errorf("user persona 'fuzzer' missing or wrong: %+v", got)
	}
	tob := out["trail-of-bits"]
	if tob.Title == "FAKE" || tob.Prompt == "fake prompt" {
		t.Errorf("user persona without Force=true silently replaced a built-in: %+v", tob)
	}
}

// TestResolvePersonas_UserForceOverridesBuiltin: Force=true is the
// documented opt-in for overriding a built-in persona.
func TestResolvePersonas_UserForceOverridesBuiltin(t *testing.T) {
	app := &App{UserPersonas: []*config.Persona{
		{ID: "trail-of-bits", Title: "OVERRIDDEN", Prompt: "new prompt", Force: true},
	}}
	out := resolvePersonas(app)
	tob, ok := out["trail-of-bits"]
	if !ok {
		t.Fatal("trail-of-bits missing after override")
	}
	if tob.Title != "OVERRIDDEN" || tob.Prompt != "new prompt" {
		t.Errorf("Force=true should override built-in; got %+v", tob)
	}
}

// TestResolveCritics_DefaultsToTrailOfBitsPlusOpenZeppelin pins the
// flag-less default: --critic unset must select exactly those two
// built-ins, in order. This matches the help text and the v0.4 behavior.
func TestResolveCritics_DefaultsToTrailOfBitsPlusOpenZeppelin(t *testing.T) {
	app := &App{}
	out, err := resolveCritics(app, nil)
	if err != nil {
		t.Fatalf("resolveCritics(nil): %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 default critics, got %d", len(out))
	}
	if out[0].ID != "trail-of-bits" || out[1].ID != "openzeppelin-style" {
		t.Errorf("default critic order = %q, %q; want trail-of-bits, openzeppelin-style", out[0].ID, out[1].ID)
	}
}

func TestResolveCritics_UnknownIDReturnsError(t *testing.T) {
	app := &App{}
	_, err := resolveCritics(app, []string{"trail-of-bits", "does-not-exist"})
	if err == nil {
		t.Fatal("resolveCritics with unknown id should return error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "does-not-exist") {
		t.Errorf("error message should name the missing critic; got %q", msg)
	}
	if !strings.Contains(msg, "Available:") {
		t.Errorf("error message should list available critics; got %q", msg)
	}
}

func TestResolveCritics_SelectsRequestedIDsInOrder(t *testing.T) {
	app := &App{}
	out, err := resolveCritics(app, []string{"openzeppelin-style", "trail-of-bits"})
	if err != nil {
		t.Fatalf("resolveCritics: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 critics, got %d", len(out))
	}
	if out[0].ID != "openzeppelin-style" || out[1].ID != "trail-of-bits" {
		t.Errorf("critic order not preserved; got %q, %q", out[0].ID, out[1].ID)
	}
}

// TestFindingsHighest_NilSafe and TestFindingsCounts_NilSafe pin the
// nil-safety contracts for the final-summary printers. A nil report shows
// up whenever an audit short-circuits before any iteration completes
// (e.g. tool-only with no findings), and the summary line is one of the
// few signals the user gets on that path.
func TestFindingsHighest_NilSafe(t *testing.T) {
	if got := findingsHighest(nil); got != "n/a" {
		t.Errorf("findingsHighest(nil) = %q, want \"n/a\"", got)
	}
	r := &engine.FindingsReport{Findings: []engine.Finding{
		{Severity: engine.SevMedium}, {Severity: engine.SevHigh}, {Severity: engine.SevLow},
	}}
	if got := findingsHighest(r); got != "high" {
		t.Errorf("findingsHighest with mixed = %q, want \"high\"", got)
	}
}

func TestFindingsCounts_NilSafe(t *testing.T) {
	got := findingsCounts(nil)
	if got == nil || len(got) != 0 {
		t.Errorf("findingsCounts(nil) should return non-nil empty map, got %v", got)
	}
	r := &engine.FindingsReport{Stats: map[string]int{"high": 2, "low": 1}}
	got = findingsCounts(r)
	if got["high"] != 2 || got["low"] != 1 {
		t.Errorf("findingsCounts(report) = %v, want {high:2, low:1}", got)
	}
}

// TestPrintFindings_FormatsHeaderSeverityAndLocation pins the user-facing
// summary format. The format is consumed by humans on every audit run and
// also referenced from the README; a regression here would degrade the
// default UX significantly.
func TestPrintFindings_FormatsHeaderSeverityAndLocation(t *testing.T) {
	var buf bytes.Buffer
	r := &engine.FindingsReport{
		Iteration: 2,
		Findings: []engine.Finding{
			{
				Critic:   "trail-of-bits",
				Severity: engine.SevHigh,
				Title:    "Reentrancy in withdraw",
				File:     "contracts/Vault.sol",
				Line:     42,
				Body:     "Cross-function reentrancy possible because state writes follow the external call.",
			},
		},
	}
	printFindings(&buf, r)
	out := buf.String()
	if !strings.Contains(out, "iteration 2") {
		t.Errorf("missing iteration header: %q", out)
	}
	if !strings.Contains(out, "1 finding") {
		t.Errorf("missing finding count: %q", out)
	}
	if !strings.Contains(out, "[HIGH]") {
		t.Errorf("severity not uppercased in header: %q", out)
	}
	if !strings.Contains(out, "trail-of-bits") {
		t.Errorf("missing critic id: %q", out)
	}
	if !strings.Contains(out, "Reentrancy in withdraw") {
		t.Errorf("missing finding title: %q", out)
	}
	if !strings.Contains(out, "at contracts/Vault.sol:42") {
		t.Errorf("missing file:line location: %q", out)
	}
}

// TestPrintFindings_LongBodyTruncated guards the 400-char truncation:
// dumping a multi-page LLM essay into the terminal would bury the next
// finding under a wall of text.
func TestPrintFindings_LongBodyTruncated(t *testing.T) {
	var buf bytes.Buffer
	r := &engine.FindingsReport{
		Iteration: 1,
		Findings: []engine.Finding{
			{Critic: "x", Severity: engine.SevLow, Title: "t", Body: strings.Repeat("a", 1000)},
		},
	}
	printFindings(&buf, r)
	out := buf.String()
	if !strings.Contains(out, "…") {
		t.Errorf("long body should be truncated with an ellipsis; got %q", out)
	}
}

// TestIsBuiltinToolID_ForgeAliasAndCaseInsensitivity exercises the two
// nontrivial behaviors: the "forge" alias maps to forge-test, and matching
// is case-insensitive so a user typing "Slither" still hits the built-in
// adapter rather than falling through to a shell invocation.
func TestIsBuiltinToolID_ForgeAliasAndCaseInsensitivity(t *testing.T) {
	if !isBuiltinToolID("forge") {
		t.Error("forge alias should be recognized as a built-in tool id")
	}
	if !isBuiltinToolID("  Slither  ") {
		t.Error("isBuiltinToolID should be case- and whitespace-insensitive")
	}
	if isBuiltinToolID("") {
		t.Error("empty string is not a built-in tool id")
	}
	if isBuiltinToolID("unknown-fuzzer") {
		t.Error("unknown name should not be flagged as built-in")
	}
}

// TestPrintPersonaList_SortsAndLabelsSources pins the persona-listing
// output: ids appear alphabetically, the source column reads "[built-in]"
// or "[user]", and user personas land in the listing too.
func TestPrintPersonaList_SortsAndLabelsSources(t *testing.T) {
	app := &App{UserPersonas: []*config.Persona{
		{ID: "fuzzer", Title: "Fuzzer", Prompt: "fuzz"},
	}}
	var buf bytes.Buffer
	printPersonaList(&buf, app)
	out := buf.String()

	if !strings.Contains(out, "fuzzer") || !strings.Contains(out, "[user]") {
		t.Errorf("user persona not listed with [user] label: %q", out)
	}
	if !strings.Contains(out, "trail-of-bits") || !strings.Contains(out, "[built-in]") {
		t.Errorf("built-in persona not listed with [built-in] label: %q", out)
	}
	// Ensure ids are emitted in sorted order by checking a couple of
	// known relative positions among built-ins.
	tobIdx := strings.Index(out, "trail-of-bits")
	ozIdx := strings.Index(out, "openzeppelin-style")
	if tobIdx < 0 || ozIdx < 0 {
		t.Fatal("expected ids missing from listing")
	}
	if ozIdx >= tobIdx {
		t.Errorf("personas not sorted alphabetically: openzeppelin-style at %d, trail-of-bits at %d", ozIdx, tobIdx)
	}
}

// TestAuditCmd_FlagsRegistered guards the public flag surface. The CLI
// help/docs and downstream scripts treat each of these flag names as a
// contract; a silent rename would break automation without any test
// firing. Iterating with cmd.Flags().Lookup catches typos at compile +
// test time rather than at runtime in user shells.
func TestAuditCmd_FlagsRegistered(t *testing.T) {
	cmd := newAuditCmd()
	wantFlags := []string{
		"critic", "iter", "stop-when", "fix", "tool", "post-gate",
		"tools-only", "worker", "file", "workdir",
		"critic-timeout", "tool-timeout", "revise-timeout", "timeout",
		"pre-approve", "print-jsonl", "output-json", "output-sarif",
		"budget-time", "list-personas",
	}
	for _, name := range wantFlags {
		if f := cmd.Flags().Lookup(name); f == nil {
			t.Errorf("audit command missing --%s flag", name)
		}
	}
	// Spot-check defaults that downstream behavior depends on.
	if f := cmd.Flags().Lookup("iter"); f != nil && f.DefValue != "3" {
		t.Errorf("--iter default = %q, want 3", f.DefValue)
	}
	if f := cmd.Flags().Lookup("stop-when"); f != nil && f.DefValue != "no_high_findings" {
		t.Errorf("--stop-when default = %q, want no_high_findings", f.DefValue)
	}
}
