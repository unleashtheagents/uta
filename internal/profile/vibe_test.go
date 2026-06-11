package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVibePath_RequiresProjectAndMode(t *testing.T) {
	if got := VibePath("", "audit"); got != "" {
		t.Errorf("empty project root should yield empty path, got %q", got)
	}
	if got := VibePath("/tmp/proj", ""); got != "" {
		t.Errorf("empty mode name should yield empty path, got %q", got)
	}
	got := VibePath("/tmp/proj", "audit")
	want := filepath.Join("/tmp/proj", ".uta", "profiles", "audit.vibe.md")
	if got != want {
		t.Errorf("VibePath = %q, want %q", got, want)
	}
}

func TestReadVibe_MissingFileIsNotError(t *testing.T) {
	dir := t.TempDir()
	body, err := ReadVibe(dir, "audit")
	if err != nil {
		t.Fatalf("ReadVibe on missing file: %v", err)
	}
	if body != "" {
		t.Errorf("missing file should return empty string, got %q", body)
	}
}

func TestAppendVibe_CreatesFileAndAppends(t *testing.T) {
	dir := t.TempDir()
	if err := AppendVibe(dir, "audit", "be more aggressive"); err != nil {
		t.Fatalf("AppendVibe first call: %v", err)
	}
	if err := AppendVibe(dir, "audit", "flag style issues too"); err != nil {
		t.Fatalf("AppendVibe second call: %v", err)
	}
	body, err := ReadVibe(dir, "audit")
	if err != nil {
		t.Fatalf("ReadVibe: %v", err)
	}
	if !strings.Contains(body, "be more aggressive") {
		t.Errorf("body missing first directive: %q", body)
	}
	if !strings.Contains(body, "flag style issues too") {
		t.Errorf("body missing second directive: %q", body)
	}
	// Second entry should appear after the first.
	if strings.Index(body, "flag style issues too") < strings.Index(body, "be more aggressive") {
		t.Errorf("appends out of order: %q", body)
	}
	// Each entry carries a `## <timestamp>` heading.
	if strings.Count(body, "## ") < 2 {
		t.Errorf("expected two timestamped headings, got: %q", body)
	}
}

func TestAppendVibe_MultiLineDirectivePreserved(t *testing.T) {
	dir := t.TempDir()
	directive := "be aggressive\nalso flag style nits\n- bullet one\n- bullet two"
	if err := AppendVibe(dir, "audit", directive); err != nil {
		t.Fatalf("AppendVibe multi-line: %v", err)
	}
	// A second entry confirms the entry separator survives multi-line bodies.
	if err := AppendVibe(dir, "audit", "follow-up directive"); err != nil {
		t.Fatalf("AppendVibe second: %v", err)
	}
	body, err := ReadVibe(dir, "audit")
	if err != nil {
		t.Fatalf("ReadVibe: %v", err)
	}
	// All four directive lines must be present, in order.
	for _, want := range []string{"be aggressive", "also flag style nits", "- bullet one", "- bullet two", "follow-up directive"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %q", want, body)
		}
	}
	// Both entries must have their own `## <ts>` heading — the multi-line
	// body of the first must not have absorbed the second's heading.
	if got := strings.Count(body, "## "); got != 2 {
		t.Errorf("expected exactly 2 entry headings, got %d: %q", got, body)
	}
	// The file must end in a single trailing newline (no run-on, no stack).
	if !strings.HasSuffix(body, "\n") || strings.HasSuffix(body, "\n\n\n") {
		t.Errorf("trailing newline shape wrong: %q", body)
	}
}

func TestAppendVibe_RejectsBlank(t *testing.T) {
	dir := t.TempDir()
	if err := AppendVibe(dir, "audit", "   "); err == nil {
		t.Error("blank directive: want error, got nil")
	}
	if err := AppendVibe("", "audit", "x"); err == nil {
		t.Error("empty project root: want error, got nil")
	}
	if err := AppendVibe(dir, "", "x"); err == nil {
		t.Error("empty mode: want error, got nil")
	}
}

func TestClearVibe_RemovesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := AppendVibe(dir, "audit", "x"); err != nil {
		t.Fatalf("AppendVibe: %v", err)
	}
	path := VibePath(dir, "audit")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("vibe file should exist before clear: %v", err)
	}
	if err := ClearVibe(dir, "audit"); err != nil {
		t.Fatalf("ClearVibe: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("vibe file should be gone, stat err = %v", err)
	}
	// Second call must not error.
	if err := ClearVibe(dir, "audit"); err != nil {
		t.Errorf("ClearVibe (idempotent) returned: %v", err)
	}
}

func TestVibePromptSection_EmptyOrBlank(t *testing.T) {
	if got := VibePromptSection(""); got != "" {
		t.Errorf("empty vibe: got %q", got)
	}
	if got := VibePromptSection("\n\n  \n"); got != "" {
		t.Errorf("whitespace-only vibe: got %q", got)
	}
}

func TestVibePromptSection_Wraps(t *testing.T) {
	got := VibePromptSection("be more aggressive\n")
	if !strings.HasPrefix(got, "## Current vibe\n") {
		t.Errorf("section missing header: %q", got)
	}
	if !strings.Contains(got, "be more aggressive") {
		t.Errorf("section missing body: %q", got)
	}
	// Trailing newlines should not stack up.
	if strings.HasSuffix(got, "\n\n") {
		t.Errorf("section should not double-trail newlines: %q", got)
	}
}
