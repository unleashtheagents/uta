package engine

import "testing"

func TestMatchCloseBraceSimple(t *testing.T) {
	s := `{}`
	if got := matchCloseBrace(s, 0); got != 1 {
		t.Fatalf("got %d want 1", got)
	}
}

func TestMatchCloseBraceNested(t *testing.T) {
	s := `{"a":{"b":{}}}`
	if got := matchCloseBrace(s, 0); got != len(s)-1 {
		t.Fatalf("got %d want %d", got, len(s)-1)
	}
}

func TestMatchCloseBraceIgnoresBracesInStrings(t *testing.T) {
	// A `}` inside a string literal must not close the outer object.
	s := `{"k":"a}b{c"}`
	if got := matchCloseBrace(s, 0); got != len(s)-1 {
		t.Fatalf("got %d want %d (input=%q)", got, len(s)-1, s)
	}
}

func TestMatchCloseBraceHandlesEscapedQuote(t *testing.T) {
	// The escaped quote should NOT terminate the string, so the `}` inside
	// it must not count toward depth.
	s := `{"k":"he said \"}\" then left"}`
	if got := matchCloseBrace(s, 0); got != len(s)-1 {
		t.Fatalf("got %d want %d (input=%q)", got, len(s)-1, s)
	}
}

func TestMatchCloseBraceHandlesEscapedBackslash(t *testing.T) {
	// `\\` is a literal backslash; the following `"` then DOES close the
	// string. The trailing `}` must close the object normally.
	s := `{"k":"path\\"}`
	if got := matchCloseBrace(s, 0); got != len(s)-1 {
		t.Fatalf("got %d want %d (input=%q)", got, len(s)-1, s)
	}
}

func TestMatchCloseBraceUnbalancedReturnsMinusOne(t *testing.T) {
	if got := matchCloseBrace(`{"a":1`, 0); got != -1 {
		t.Fatalf("got %d want -1 for unbalanced input", got)
	}
	if got := matchCloseBrace(`{{}`, 0); got != -1 {
		t.Fatalf("got %d want -1 for extra opener", got)
	}
}

func TestMatchCloseBraceRejectsNonBraceStart(t *testing.T) {
	if got := matchCloseBrace(`xxx{}`, 0); got != -1 {
		t.Fatalf("got %d want -1 when start is not '{'", got)
	}
	if got := matchCloseBrace(``, 0); got != -1 {
		t.Fatalf("got %d want -1 for empty input", got)
	}
	if got := matchCloseBrace(`{}`, 5); got != -1 {
		t.Fatalf("got %d want -1 when start is out of range", got)
	}
}

func TestMatchCloseBraceDeeplyNested(t *testing.T) {
	// 32 levels of nesting — well past anything LLMs realistically emit.
	depth := 32
	s := ""
	for i := 0; i < depth; i++ {
		s += "{"
	}
	for i := 0; i < depth; i++ {
		s += "}"
	}
	if got := matchCloseBrace(s, 0); got != len(s)-1 {
		t.Fatalf("got %d want %d for %d-deep nesting", got, len(s)-1, depth)
	}
}

func TestFindJSONObjectByKeySimple(t *testing.T) {
	text := `{"findings":[]}`
	got, ok := findJSONObjectByKey(text, "findings")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != text {
		t.Fatalf("got %q want %q", got, text)
	}
}

func TestFindJSONObjectByKeySkipsProsePreamble(t *testing.T) {
	// LLMs love to wrap JSON in prose with stray braces. The extractor must
	// hop over the prose `{` and anchor on the real findings object.
	text := `Here is the result {not the real one} and here's the JSON: {"findings":[{"id":"x"}]} done.`
	got, ok := findJSONObjectByKey(text, "findings")
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := `{"findings":[{"id":"x"}]}`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestFindJSONObjectByKeyToleratesWhitespace(t *testing.T) {
	// Whitespace between `{` and the first key must be tolerated.
	text := "{\n  \"subtasks\": [\n    {\"id\": \"s1\"}\n  ]\n}"
	got, ok := findJSONObjectByKey(text, "subtasks")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if got != text {
		t.Fatalf("got %q want %q", got, text)
	}
}

func TestFindJSONObjectByKeyMissingKey(t *testing.T) {
	if _, ok := findJSONObjectByKey(`{"other":[]}`, "findings"); ok {
		t.Fatal("expected ok=false when key absent")
	}
}

func TestFindJSONObjectByKeyUnbalanced(t *testing.T) {
	// Key matches but braces don't balance — must fail rather than
	// returning a truncated span.
	if _, ok := findJSONObjectByKey(`{"findings":[`, "findings"); ok {
		t.Fatal("expected ok=false for unbalanced input")
	}
}

func TestFindJSONObjectByKeyRequiresKeyToBeFirst(t *testing.T) {
	// findJSONObjectByKey only anchors when the requested key is the FIRST
	// key after the opening brace. An object that contains the key further
	// in is intentionally not matched — that's how the function avoids
	// being tricked by stray prose `{` followed by random JSON-looking text.
	if _, ok := findJSONObjectByKey(`{"note":"x","findings":[]}`, "findings"); ok {
		t.Fatal("expected ok=false when key is not the first key")
	}
}

func TestFindJSONObjectByKeyNestedBraceInsideString(t *testing.T) {
	// A `{` inside a string-valued field must not confuse the matcher:
	// it must still return the outer object span, with the string content
	// preserved verbatim.
	text := `noise {"findings":[{"msg":"see {x}"}]}`
	got, ok := findJSONObjectByKey(text, "findings")
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := `{"findings":[{"msg":"see {x}"}]}`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestExtractJSONObjectSimple(t *testing.T) {
	if got := extractJSONObject(`{"a":1}`); got != `{"a":1}` {
		t.Fatalf("got %q", got)
	}
}

func TestExtractJSONObjectStripsSurroundingProse(t *testing.T) {
	// Tool adapters call this to peel away warning banners and trailing
	// log lines.
	text := "WARN: trust prompt skipped\n{\"ok\":true}\nbye"
	if got := extractJSONObject(text); got != `{"ok":true}` {
		t.Fatalf("got %q", got)
	}
}

func TestExtractJSONObjectIgnoresBraceInString(t *testing.T) {
	// The `}` inside the string must not close the object early.
	s := `noise {"k":"a}b"} trailing`
	if got := extractJSONObject(s); got != `{"k":"a}b"}` {
		t.Fatalf("got %q", got)
	}
}

func TestExtractJSONObjectNoBraces(t *testing.T) {
	if got := extractJSONObject(`no json here`); got != "" {
		t.Fatalf("got %q want empty", got)
	}
}

func TestExtractJSONObjectUnbalanced(t *testing.T) {
	// An opener with no matching closer must yield "" rather than the
	// rest of the string.
	if got := extractJSONObject(`prefix {"a":1`); got != "" {
		t.Fatalf("got %q want empty for unbalanced input", got)
	}
}

func TestExtractJSONObjectPicksFirstBalanced(t *testing.T) {
	// Two objects in the stream — extractJSONObject is documented to
	// return the first balanced one.
	s := `{"first":1} then {"second":2}`
	if got := extractJSONObject(s); got != `{"first":1}` {
		t.Fatalf("got %q", got)
	}
}
