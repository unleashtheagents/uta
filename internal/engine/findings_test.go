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
