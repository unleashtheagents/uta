package improve

import (
	"strings"
	"testing"
)

func TestParseIdeas_StrictJSON(t *testing.T) {
	resp := `{
		"ideas": [
			{"title": "first", "body": "do x", "severity": "HIGH", "tags": ["go","tests"]},
			{"title": "second", "body": "do y", "severity": "low"}
		]
	}`
	ideas, err := parseIdeas(resp, "gemini", nil)
	if err != nil {
		t.Fatalf("parseIdeas: %v", err)
	}
	if len(ideas) != 2 {
		t.Fatalf("expected 2 ideas, got %d", len(ideas))
	}
	if ideas[0].Title != "first" || ideas[0].Body != "do x" {
		t.Fatalf("idea[0]: %+v", ideas[0])
	}
	if ideas[0].Severity != SevHigh {
		t.Fatalf("idea[0].Severity: got %q want %q", ideas[0].Severity, SevHigh)
	}
	if ideas[0].Source != "gather:gemini" {
		t.Fatalf("idea[0].Source: got %q want %q", ideas[0].Source, "gather:gemini")
	}
	if ideas[0].Status != StatusProposed {
		t.Fatalf("idea[0].Status: got %q want %q", ideas[0].Status, StatusProposed)
	}
	if len(ideas[0].Tags) != 2 || ideas[0].Tags[0] != "go" || ideas[0].Tags[1] != "tests" {
		t.Fatalf("idea[0].Tags: %v", ideas[0].Tags)
	}
	if ideas[1].Severity != SevLow {
		t.Fatalf("idea[1].Severity: got %q want %q", ideas[1].Severity, SevLow)
	}
}

func TestParseIdeas_ToleratesProseAround(t *testing.T) {
	// Permissive extractor: prose / markdown before & after the JSON object.
	resp := "Here are my ideas:\n\n```json\n{\n  \"ideas\": [{\"title\":\"t\",\"body\":\"b\",\"severity\":\"medium\"}]\n}\n```\nHope that helps!"
	ideas, err := parseIdeas(resp, "gemini", nil)
	if err != nil {
		t.Fatalf("parseIdeas: %v", err)
	}
	if len(ideas) != 1 || ideas[0].Title != "t" {
		t.Fatalf("got %+v", ideas)
	}
}

func TestParseIdeas_AppliesCallerTags(t *testing.T) {
	resp := `{"ideas":[{"title":"x","body":"y","severity":"medium","tags":["from-model"]}]}`
	ideas, err := parseIdeas(resp, "gemini", []string{"from-caller"})
	if err != nil {
		t.Fatalf("parseIdeas: %v", err)
	}
	if len(ideas) != 1 {
		t.Fatalf("expected 1 idea, got %d", len(ideas))
	}
	tags := ideas[0].Tags
	if len(tags) != 2 || tags[0] != "from-caller" || tags[1] != "from-model" {
		t.Fatalf("tags merge: got %v want [from-caller from-model]", tags)
	}
}

func TestParseIdeas_DropsEmptyAndWhitespaceOnlyTags(t *testing.T) {
	resp := `{"ideas":[{"title":"x","body":"y","severity":"medium","tags":["keep","","   "]}]}`
	ideas, err := parseIdeas(resp, "gemini", nil)
	if err != nil {
		t.Fatalf("parseIdeas: %v", err)
	}
	if len(ideas[0].Tags) != 1 || ideas[0].Tags[0] != "keep" {
		t.Fatalf("expected only the non-empty tag, got %v", ideas[0].Tags)
	}
}

func TestParseIdeas_DropsCompletelyEmptyEntries(t *testing.T) {
	resp := `{"ideas":[{"title":"","body":""},{"title":"keep","body":"yes"}]}`
	ideas, err := parseIdeas(resp, "claude", nil)
	if err != nil {
		t.Fatalf("parseIdeas: %v", err)
	}
	if len(ideas) != 1 || ideas[0].Title != "keep" {
		t.Fatalf("expected only the non-empty idea, got %+v", ideas)
	}
}

func TestParseIdeas_TrimsWhitespace(t *testing.T) {
	resp := `{"ideas":[{"title":"  spaced  ","body":"\n\nbody\t\n","severity":"medium"}]}`
	ideas, err := parseIdeas(resp, "claude", nil)
	if err != nil {
		t.Fatalf("parseIdeas: %v", err)
	}
	if ideas[0].Title != "spaced" {
		t.Fatalf("title trim: got %q want %q", ideas[0].Title, "spaced")
	}
	if ideas[0].Body != "body" {
		t.Fatalf("body trim: got %q want %q", ideas[0].Body, "body")
	}
}

func TestParseIdeas_NoJSONObject(t *testing.T) {
	_, err := parseIdeas("just prose, no JSON here", "gemini", nil)
	if err == nil {
		t.Fatalf("expected error when there is no JSON object")
	}
}

func TestParseIdeas_MalformedJSON(t *testing.T) {
	_, err := parseIdeas(`{"ideas": [bad}`, "gemini", nil)
	if err == nil {
		t.Fatalf("expected error on malformed JSON")
	}
}

func TestParseIdeas_EmptyIdeasArray(t *testing.T) {
	ideas, err := parseIdeas(`{"ideas": []}`, "gemini", nil)
	if err != nil {
		t.Fatalf("parseIdeas: %v", err)
	}
	if len(ideas) != 0 {
		t.Fatalf("expected zero ideas, got %d", len(ideas))
	}
}

func TestNormalizeSeverity(t *testing.T) {
	cases := []struct {
		in   string
		want Severity
	}{
		{"HIGH", SevHigh},
		{"high", SevHigh},
		{"critical", SevHigh},
		{"crit", SevHigh},
		{"h", SevHigh},
		{"medium", SevMedium},
		{"MED", SevMedium},
		{"moderate", SevMedium},
		{"m", SevMedium},
		{"low", SevLow},
		{"MINOR", SevLow},
		{"l", SevLow},
		{"info", SevInfo},
		{"informational", SevInfo},
		{"note", SevInfo},
		{"nit", SevInfo},
		{"i", SevInfo},
		{"  HIGH  ", SevHigh},        // surrounding whitespace
		{"", SevMedium},              // empty falls back to medium
		{"garbage", SevMedium},       // unknown falls back to medium
		{"P0", SevMedium},            // unknown taxonomy falls back to medium
	}
	for _, tc := range cases {
		got := normalizeSeverity(tc.in)
		if got != tc.want {
			t.Errorf("normalizeSeverity(%q): got %q want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderExisting_EmptyShowsPlaceholder(t *testing.T) {
	got := renderExisting(nil)
	if got != "(backlog is empty)" {
		t.Fatalf("empty placeholder: got %q", got)
	}
}

func TestRenderExisting_FormatsEntries(t *testing.T) {
	ideas := []*Idea{
		{Title: "first", Severity: SevHigh, Status: StatusProposed},
		{Title: "second", Severity: SevLow, Status: StatusDone},
	}
	got := renderExisting(ideas)
	if !strings.Contains(got, "[high · proposed] first") {
		t.Errorf("missing first entry: %q", got)
	}
	if !strings.Contains(got, "[low · done] second") {
		t.Errorf("missing second entry: %q", got)
	}
}

func TestSafeStrings_NilBecomesEmpty(t *testing.T) {
	out := safeStrings(nil)
	if out == nil {
		t.Fatalf("safeStrings(nil) returned nil; want empty slice")
	}
	if len(out) != 0 {
		t.Fatalf("safeStrings(nil) returned %v; want empty slice", out)
	}
}

func TestSafeMap_NilBecomesEmpty(t *testing.T) {
	out := safeMap(nil)
	if out == nil {
		t.Fatalf("safeMap(nil) returned nil; want empty map")
	}
	if len(out) != 0 {
		t.Fatalf("safeMap(nil) returned %v; want empty map", out)
	}
}
