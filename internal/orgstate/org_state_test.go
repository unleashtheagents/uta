package orgstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoad_MissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if got == nil {
		t.Fatal("Load missing returned nil state")
	}
	if !got.Frontmatter.LastUpdated.IsZero() {
		t.Errorf("missing-file state should have zero LastUpdated, got %v", got.Frontmatter.LastUpdated)
	}
	if got.Body != "" {
		t.Errorf("missing-file state body = %q, want empty", got.Body)
	}
}

func TestWriteThenLoad_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	want := &State{
		Frontmatter: Frontmatter{
			SourceEmails:     []string{"msg-001", "msg-002"},
			PendingDecisions: []string{"approve Q3 hire", "sign Acme MSA"},
		},
		Body: "## Commitments\n- Ship v1 by EOQ\n- Land 3 design partners\n",
	}
	if err := Write(dir, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if want.Frontmatter.LastUpdated.IsZero() {
		t.Fatal("Write did not stamp LastUpdated")
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Frontmatter.LastUpdated.IsZero() {
		t.Fatal("Load returned zero LastUpdated after roundtrip")
	}
	if !equalStrings(got.Frontmatter.SourceEmails, want.Frontmatter.SourceEmails) {
		t.Errorf("source_emails = %v, want %v", got.Frontmatter.SourceEmails, want.Frontmatter.SourceEmails)
	}
	if !equalStrings(got.Frontmatter.PendingDecisions, want.Frontmatter.PendingDecisions) {
		t.Errorf("pending_decisions = %v, want %v", got.Frontmatter.PendingDecisions, want.Frontmatter.PendingDecisions)
	}
	if strings.TrimSpace(got.Body) != strings.TrimSpace(want.Body) {
		t.Errorf("body = %q, want %q", got.Body, want.Body)
	}
}

func TestWrite_RespectsCallerLastUpdated(t *testing.T) {
	dir := t.TempDir()
	stamp := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	st := &State{Frontmatter: Frontmatter{LastUpdated: stamp}, Body: "hello"}
	if err := Write(dir, st); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Frontmatter.LastUpdated.Equal(stamp) {
		t.Errorf("LastUpdated = %v, want %v", got.Frontmatter.LastUpdated, stamp)
	}
}

func TestParse_NoFrontmatterTreatedAsBody(t *testing.T) {
	dir := t.TempDir()
	raw := "just some notes, no frontmatter\n"
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Frontmatter.LastUpdated.IsZero() {
		t.Errorf("frontmatterless doc should have zero LastUpdated, got %v", got.Frontmatter.LastUpdated)
	}
	if got.Body != raw {
		t.Errorf("Body = %q, want %q", got.Body, raw)
	}
}

func TestParse_MalformedFrontmatterErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "unterminated flow sequence",
			raw:  "---\nlast_updated: not-a-timestamp\nsource_emails: [a, b\n---\nbody\n",
		},
		{
			name: "wrong type for source_emails",
			// source_emails declared as []string; a scalar fails yaml decode.
			raw: "---\nsource_emails: 42\n---\nbody\n",
		},
		{
			name: "non-timestamp last_updated",
			// time.Time decode rejects free-form text.
			raw: "---\nlast_updated: yesterday\n---\nbody\n",
		},
		{
			name: "tab-indented mapping",
			// YAML 1.2 forbids tabs in indentation; go-yaml surfaces this as a parse error.
			raw: "---\nsource_emails:\n\t- a\n---\nbody\n",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, Filename), []byte(tc.raw), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(dir); err == nil {
				t.Errorf("expected malformed frontmatter %q to error, got nil", tc.name)
			}
		})
	}
}

func TestParse_UnclosedFenceTreatedAsBody(t *testing.T) {
	// A leading "---\n" with no closing fence is the "agent forgot how to
	// close frontmatter" case. parse() chooses to treat the whole input as
	// body rather than reject it — keep that contract pinned so a future
	// refactor doesn't silently start erroring on partially-written files.
	dir := t.TempDir()
	raw := "---\nsource_emails:\n  - a\nnotes go here without a closing fence\n"
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Frontmatter.LastUpdated.IsZero() || len(got.Frontmatter.SourceEmails) != 0 {
		t.Errorf("unclosed fence should yield zero frontmatter, got %+v", got.Frontmatter)
	}
	if got.Body != raw {
		t.Errorf("Body = %q, want raw input", got.Body)
	}
}

func TestWrite_IsAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	first := &State{Body: "first\n"}
	if err := Write(dir, first); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	second := &State{Body: "second\n"}
	if err := Write(dir, second); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.TrimSpace(got.Body) != "second" {
		t.Errorf("Body = %q, want %q", got.Body, "second")
	}
	// No stray .tmp-* siblings left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestPath_Format(t *testing.T) {
	got := Path("/tmp/ctx")
	want := "/tmp/ctx/" + Filename
	if got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}

func TestWrite_NilStateErrors(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, nil); err == nil {
		t.Error("expected nil-state Write to error")
	}
}

func TestLoad_EmptyDirErrors(t *testing.T) {
	if _, err := Load(""); err == nil {
		t.Error("expected empty contextDir to error")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
