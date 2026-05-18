package engine

import (
	"errors"
	"testing"
)

func TestParseFindings_RawArray(t *testing.T) {
	in := `[{"id":"f1","severity":"high","title":"SQLi","file":"a.go","line":12}]`
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d want 1", len(got))
	}
	if got[0].ID != "f1" || got[0].Severity != SevHigh || got[0].Title != "SQLi" ||
		got[0].File != "a.go" || got[0].Line != 12 {
		t.Fatalf("unexpected finding: %+v", got[0])
	}
}

func TestParseFindings_ObjectWithFindingsKey(t *testing.T) {
	in := `{"findings":[{"id":"x","severity":"MEDIUM","title":"t","body":"b"}]}`
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d want 1", len(got))
	}
	if got[0].Severity != SevMedium {
		t.Fatalf("severity not normalized: got %q want %q", got[0].Severity, SevMedium)
	}
}

func TestParseFindings_MarkdownWrappedObject(t *testing.T) {
	in := "Here are my findings:\n```json\n{\"findings\":[{\"id\":\"m1\",\"severity\":\"high\",\"title\":\"t\"}]}\n```\nDone."
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParseFindings_MarkdownWrappedArray(t *testing.T) {
	in := "Findings:\n```json\n[{\"id\":\"a1\",\"severity\":\"low\",\"title\":\"t\"}]\n```"
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a1" || got[0].Severity != SevLow {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParseFindings_EmptyFindingsArray(t *testing.T) {
	got, err := ParseFindings(`{"findings":[]}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
}

func TestParseFindings_BareEmptyArray(t *testing.T) {
	got, err := ParseFindings(`[]`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
}

func TestParseFindings_PreambleAndTrailer(t *testing.T) {
	in := `I reviewed the code and found these issues. {"findings":[{"id":"p1","severity":"high","title":"t"}]} Hope this helps.`
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "p1" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParseFindings_PreambleWithBraces(t *testing.T) {
	// LLM mentions code snippets containing braces in its preamble before
	// emitting the actual JSON. First-`{`-to-last-`}` extraction would capture
	// the stray braces and produce invalid JSON.
	in := "I noticed the body `if err != nil { return err }` is missing a wrap. " +
		`{"findings":[{"id":"p1","severity":"high","title":"wrap error"}]}` +
		" Some trailing prose mentioning {another} placeholder."
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "p1" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParseFindings_PreambleWithBracesAndTrailerBraces(t *testing.T) {
	// Several braced spans in prose surrounding the real JSON; the real object
	// also contains a body with literal braces inside a JSON string literal
	// (testing string-literal-aware brace matching).
	in := "Looking at `func() { ... }` I think this needs work. " +
		`{"findings":[{"id":"x","severity":"medium","title":"t","body":"the function returns { wrong type }"}]}` +
		" Then later I saw {trailing} too."
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "x" || got[0].Body != "the function returns { wrong type }" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParseFindings_DropsEmptyFindings(t *testing.T) {
	// First has neither title nor body — must be dropped; second kept.
	in := `[{"id":"empty","severity":"low"},{"id":"real","severity":"high","title":"keeper"}]`
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "real" {
		t.Fatalf("expected only 'real', got %+v", got)
	}
}

func TestParseFindings_UnknownSeverity(t *testing.T) {
	in := `[{"id":"u","severity":"catastrophic","title":"t"}]`
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Severity != SevUnknown {
		t.Fatalf("expected unknown severity, got %+v", got)
	}
}

func TestParseFindings_SeverityAliases(t *testing.T) {
	cases := map[string]Severity{
		"critical": SevHigh,
		"crit":     SevHigh,
		"severe":   SevHigh,
		"H":        SevHigh,
		"moderate": SevMedium,
		"med":      SevMedium,
		"M":        SevMedium,
		"minor":    SevLow,
		"L":        SevLow,
		"note":     SevInfo,
		"nit":      SevInfo,
		"I":        SevInfo,
	}
	for in, want := range cases {
		got := NormalizeSeverity(in)
		if got != want {
			t.Errorf("NormalizeSeverity(%q): got %q want %q", in, got, want)
		}
	}
}

// TestNormalizeSeverity exhaustively covers every alias the function recognizes,
// plus the canonical tiers, casing/whitespace handling, and the unknown path.
// Adding new aliases to NormalizeSeverity should be paired with an entry here.
func TestNormalizeSeverity(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Severity
	}{
		// Canonical tiers.
		{"canonical high", "high", SevHigh},
		{"canonical medium", "medium", SevMedium},
		{"canonical low", "low", SevLow},
		{"canonical info", "info", SevInfo},

		// High aliases.
		{"alias critical", "critical", SevHigh},
		{"alias crit", "crit", SevHigh},
		{"alias severe", "severe", SevHigh},
		{"alias h", "h", SevHigh},

		// Medium aliases.
		{"alias med", "med", SevMedium},
		{"alias moderate", "moderate", SevMedium},
		{"alias m", "m", SevMedium},

		// Low aliases.
		{"alias minor", "minor", SevLow},
		{"alias l", "l", SevLow},

		// Info aliases.
		{"alias informational", "informational", SevInfo},
		{"alias note", "note", SevInfo},
		{"alias nit", "nit", SevInfo},
		{"alias low-info", "low-info", SevInfo},
		{"alias i", "i", SevInfo},

		// Casing — NormalizeSeverity lowercases before matching.
		{"uppercase HIGH", "HIGH", SevHigh},
		{"mixed Critical", "Critical", SevHigh},
		{"uppercase MEDIUM", "MEDIUM", SevMedium},
		{"mixed Moderate", "Moderate", SevMedium},
		{"uppercase LOW", "LOW", SevLow},
		{"mixed Minor", "Minor", SevLow},
		{"uppercase INFO", "INFO", SevInfo},
		{"mixed Nit", "Nit", SevInfo},
		{"mixed LOW-INFO", "LOW-INFO", SevInfo},

		// Whitespace — NormalizeSeverity trims before matching.
		{"leading space", "  high", SevHigh},
		{"trailing space", "high  ", SevHigh},
		{"surrounding space", "  high  ", SevHigh},
		{"tabs and spaces around alias", "\tcrit \n", SevHigh},
		{"surrounding whitespace mixed-case", "  Critical  ", SevHigh},

		// Unknown / empty inputs collapse to SevUnknown.
		{"empty string", "", SevUnknown},
		{"whitespace only", "   \t\n", SevUnknown},
		{"unknown word", "catastrophic", SevUnknown},
		{"unknown abbrev", "xx", SevUnknown},
		{"numeric", "5", SevUnknown},
		{"partial match prefix", "highish", SevUnknown},
		{"partial match suffix", "ultrahigh", SevUnknown},
		{"inner-whitespace breaks match", "h igh", SevUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeSeverity(tc.in)
			if got != tc.want {
				t.Errorf("NormalizeSeverity(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseFindings_NoJSON(t *testing.T) {
	_, err := ParseFindings("the model refused to answer")
	if !errors.Is(err, ErrFindingsUnparseable) {
		t.Fatalf("expected ErrFindingsUnparseable, got %v", err)
	}
}

func TestParseFindings_OpenBracketNoClose(t *testing.T) {
	_, err := ParseFindings(`[{"id":"x","title":"t"`)
	if !errors.Is(err, ErrFindingsUnparseable) {
		t.Fatalf("expected ErrFindingsUnparseable, got %v", err)
	}
}

func TestParseFindings_OpenBraceNoClose(t *testing.T) {
	_, err := ParseFindings(`{"findings":[`)
	if !errors.Is(err, ErrFindingsUnparseable) {
		t.Fatalf("expected ErrFindingsUnparseable, got %v", err)
	}
}

func TestParseFindings_MalformedJSONInsideBraces(t *testing.T) {
	// Has { and } but contents are not valid JSON.
	_, err := ParseFindings(`{ this is not json at all }`)
	if !errors.Is(err, ErrFindingsUnparseable) {
		t.Fatalf("expected ErrFindingsUnparseable, got %v", err)
	}
}

func TestParseFindings_MalformedJSONInsideBrackets(t *testing.T) {
	_, err := ParseFindings(`[not, json, here]`)
	if !errors.Is(err, ErrFindingsUnparseable) {
		t.Fatalf("expected ErrFindingsUnparseable, got %v", err)
	}
}

func TestParseFindings_ArrayBeforeObjectPrefersArray(t *testing.T) {
	// `[` appears before `{` — parser prefers the array path. The array
	// contains an object element, which is fine.
	in := `[{"id":"a","severity":"high","title":"t"}]`
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParseFindings_ObjectBeforeArrayPrefersObject(t *testing.T) {
	// `{` appears before `[` — parser prefers the object path. The findings
	// array is nested inside the object.
	in := `{"findings":[{"id":"b","severity":"medium","title":"t"}]}`
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "b" || got[0].Severity != SevMedium {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParseFindings_ObjectMissingFindingsKey(t *testing.T) {
	// Valid JSON object but no "findings" key — yields empty list, not error.
	got, err := ParseFindings(`{"summary":"all good"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %+v", got)
	}
}

func TestParseFindings_TitleOnlyKept(t *testing.T) {
	// Body empty but title present — should be kept.
	in := `[{"id":"t","severity":"info","title":"just a title"}]`
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1, got %+v", got)
	}
}

func TestParseFindings_BodyOnlyKept(t *testing.T) {
	// Title empty but body present — should be kept.
	in := `[{"id":"b","severity":"info","body":"just a body"}]`
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1, got %+v", got)
	}
}

func TestParseFindings_WhitespaceOnlyTitleAndBodyDropped(t *testing.T) {
	in := `[{"id":"w","severity":"high","title":"   ","body":"\t\n "}]`
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0, got %+v", got)
	}
}

func TestAggregate_WithinCriticDedupByID(t *testing.T) {
	perCritic := map[string][]Finding{
		"sec": {
			{ID: "f1", Severity: SevLow, Title: "SQLi in users.go", File: "users.go", Line: 12},
			{ID: "f1", Severity: SevHigh, Title: "SQLi in users.go", File: "users.go", Line: 12},
			{ID: "f2", Severity: SevMedium, Title: "weak crypto", File: "auth.go", Line: 7},
		},
	}
	rep := Aggregate(1, perCritic)
	if len(rep.Findings) != 2 {
		t.Fatalf("expected 2 findings after within-critic dedup, got %d: %+v", len(rep.Findings), rep.Findings)
	}
	// The duplicate f1 should have collapsed to the high-severity version.
	for _, f := range rep.Findings {
		if f.ID == "f1" && f.Severity != SevHigh {
			t.Fatalf("expected merged f1 to keep high severity, got %q", f.Severity)
		}
	}
	if rep.Stats[string(SevHigh)] != 1 || rep.Stats[string(SevMedium)] != 1 {
		t.Fatalf("unexpected stats: %+v", rep.Stats)
	}
}

func TestAggregate_CrossCriticDedupByFileLineTitle(t *testing.T) {
	perCritic := map[string][]Finding{
		"alice": {
			{ID: "a1", Severity: SevMedium, Title: "Missing nil check", File: "x.go", Line: 42},
		},
		"bob": {
			{ID: "b1", Severity: SevHigh, Title: "missing nil check", File: "x.go", Line: 42},
		},
	}
	rep := Aggregate(1, perCritic)
	if len(rep.Findings) != 1 {
		t.Fatalf("expected 1 finding after cross-critic dedup, got %d: %+v", len(rep.Findings), rep.Findings)
	}
	if rep.Findings[0].Severity != SevHigh {
		t.Fatalf("expected the high-severity version to win, got %q (critic=%s id=%s)",
			rep.Findings[0].Severity, rep.Findings[0].Critic, rep.Findings[0].ID)
	}
	if rep.Findings[0].Critic != "bob" {
		t.Fatalf("expected bob's version (higher severity), got critic=%s", rep.Findings[0].Critic)
	}
	// Both critics still listed at the report level.
	if len(rep.Critics) != 2 || rep.Critics[0] != "alice" || rep.Critics[1] != "bob" {
		t.Fatalf("expected both critics listed, got %+v", rep.Critics)
	}
}

func TestAggregate_CrossCriticDedupTieGoesToFirstCritic(t *testing.T) {
	perCritic := map[string][]Finding{
		"zeta": {
			{ID: "z1", Severity: SevMedium, Title: "Suspicious cast", File: "p.go", Line: 9},
		},
		"alpha": {
			{ID: "a1", Severity: SevMedium, Title: "suspicious cast", File: "p.go", Line: 9},
		},
	}
	rep := Aggregate(1, perCritic)
	if len(rep.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(rep.Findings))
	}
	if rep.Findings[0].Critic != "alpha" {
		t.Fatalf("expected alphabetically-first critic to win on tie, got %q", rep.Findings[0].Critic)
	}
}

func TestAggregate_DoesNotDedupDifferentLocations(t *testing.T) {
	perCritic := map[string][]Finding{
		"sec": {
			{ID: "f1", Severity: SevHigh, Title: "Use after free", File: "a.go", Line: 10},
			{ID: "f2", Severity: SevHigh, Title: "Use after free", File: "a.go", Line: 99},
			{ID: "f3", Severity: SevHigh, Title: "Use after free", File: "b.go", Line: 10},
		},
	}
	rep := Aggregate(1, perCritic)
	if len(rep.Findings) != 3 {
		t.Fatalf("expected 3 distinct findings, got %d: %+v", len(rep.Findings), rep.Findings)
	}
}

func TestAggregate_KeepsFindingsWithoutDedupKey(t *testing.T) {
	// No ID, no file — can't form a key, so each finding survives.
	perCritic := map[string][]Finding{
		"sec": {
			{Severity: SevLow, Title: "general note"},
			{Severity: SevLow, Title: "general note"},
		},
	}
	rep := Aggregate(1, perCritic)
	if len(rep.Findings) != 2 {
		t.Fatalf("expected 2 (no dedup key), got %d: %+v", len(rep.Findings), rep.Findings)
	}
}

func TestAggregate_PreservesSubtaskID(t *testing.T) {
	// Findings carry the SubtaskID of the critic/tool subtask that produced
	// them, so downstream consumers can link back to the trajectory event and
	// raw provider output. Within-critic and cross-critic dedup must preserve
	// the winner's SubtaskID, not silently drop it.
	perCritic := map[string][]Finding{
		"alice": {
			{SubtaskID: "sub-A", ID: "a1", Severity: SevMedium, Title: "shared issue", File: "x.go", Line: 1},
		},
		"bob": {
			{SubtaskID: "sub-B", ID: "b1", Severity: SevHigh, Title: "shared issue", File: "x.go", Line: 1},
			{SubtaskID: "sub-B", ID: "b2", Severity: SevLow, Title: "bob-only", File: "y.go", Line: 2},
		},
	}
	rep := Aggregate(1, perCritic)
	bySub := map[string]string{}
	for _, f := range rep.Findings {
		bySub[f.ID] = f.SubtaskID
	}
	// bob's high-severity finding won the cross-critic dedup; its SubtaskID
	// should survive on the merged finding.
	if bySub["b1"] != "sub-B" {
		t.Fatalf("cross-critic dedup winner lost SubtaskID: got %q want %q (findings=%+v)", bySub["b1"], "sub-B", rep.Findings)
	}
	if bySub["b2"] != "sub-B" {
		t.Fatalf("untouched finding lost SubtaskID: got %q want %q", bySub["b2"], "sub-B")
	}
}

func TestParseFindings_SubtaskIDRoundTrip(t *testing.T) {
	in := `{"findings":[{"id":"f1","subtask_id":"sub-123","severity":"high","title":"t"}]}`
	got, err := ParseFindings(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].SubtaskID != "sub-123" {
		t.Fatalf("expected subtask_id round-trip, got %+v", got)
	}
}

func TestAggregate_WithinCriticFallbackToFileLineTitle(t *testing.T) {
	// No IDs — within-critic dedup falls back to (file, line, title).
	perCritic := map[string][]Finding{
		"sec": {
			{Severity: SevLow, Title: "Boundary check", File: "x.go", Line: 5},
			{Severity: SevHigh, Title: "boundary check", File: "x.go", Line: 5},
		},
	}
	rep := Aggregate(1, perCritic)
	if len(rep.Findings) != 1 {
		t.Fatalf("expected within-critic fallback dedup to collapse, got %d: %+v", len(rep.Findings), rep.Findings)
	}
	if rep.Findings[0].Severity != SevHigh {
		t.Fatalf("expected high severity winner, got %q", rep.Findings[0].Severity)
	}
}
