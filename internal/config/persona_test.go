package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempPersona(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp persona: %v", err)
	}
	return path
}

func TestLoadPersona_Valid(t *testing.T) {
	path := writeTempPersona(t, "p.yaml", `
id: critic
title: Critical Reviewer
prompt: Be tough but fair.
worker: claude
tags: [security, review]
force: true
`)
	p, err := LoadPersona(path)
	if err != nil {
		t.Fatalf("LoadPersona: %v", err)
	}
	if p.ID != "critic" {
		t.Errorf("ID: got %q, want %q", p.ID, "critic")
	}
	if p.Title != "Critical Reviewer" {
		t.Errorf("Title: got %q", p.Title)
	}
	if p.Prompt != "Be tough but fair." {
		t.Errorf("Prompt: got %q", p.Prompt)
	}
	if p.Worker != "claude" {
		t.Errorf("Worker: got %q", p.Worker)
	}
	if len(p.Tags) != 2 || p.Tags[0] != "security" || p.Tags[1] != "review" {
		t.Errorf("Tags: got %v", p.Tags)
	}
	if !p.Force {
		t.Errorf("Force: got false, want true")
	}
	if p.Source != path {
		t.Errorf("Source: got %q, want %q", p.Source, path)
	}
}

func TestLoadPersona_MinimalRequiredFields(t *testing.T) {
	path := writeTempPersona(t, "min.yaml", `
id: tiny
prompt: short
`)
	p, err := LoadPersona(path)
	if err != nil {
		t.Fatalf("LoadPersona: %v", err)
	}
	if p.ID != "tiny" || p.Prompt != "short" {
		t.Errorf("got id=%q prompt=%q", p.ID, p.Prompt)
	}
	if p.Title != "" || p.Worker != "" || len(p.Tags) != 0 || p.Force {
		t.Errorf("optional fields should be zero-valued: %+v", p)
	}
}

func TestLoadPersona_FileNotFound(t *testing.T) {
	_, err := LoadPersona(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil {
		t.Fatalf("expected error for missing file, got nil")
	}
}

func TestLoadPersona_MalformedYAML(t *testing.T) {
	path := writeTempPersona(t, "bad.yaml", "id: [unterminated\nprompt: x\n")
	_, err := LoadPersona(path)
	if err == nil {
		t.Fatalf("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse failure, got: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should include path %q, got: %v", path, err)
	}
}

func TestLoadPersona_MissingID(t *testing.T) {
	path := writeTempPersona(t, "noid.yaml", `
prompt: only prompt
`)
	_, err := LoadPersona(path)
	if err == nil {
		t.Fatalf("expected validation error for missing id, got nil")
	}
	if !strings.Contains(err.Error(), "id") {
		t.Errorf("error should mention id, got: %v", err)
	}
}

func TestLoadPersona_MissingPrompt(t *testing.T) {
	path := writeTempPersona(t, "noprompt.yaml", `
id: ghost
`)
	_, err := LoadPersona(path)
	if err == nil {
		t.Fatalf("expected validation error for missing prompt, got nil")
	}
	if !strings.Contains(err.Error(), "prompt") {
		t.Errorf("error should mention prompt, got: %v", err)
	}
}

func TestLoadPersona_BlankFieldsFailValidation(t *testing.T) {
	path := writeTempPersona(t, "blank.yaml", `
id: "   "
prompt: "   "
`)
	_, err := LoadPersona(path)
	if err == nil {
		t.Fatalf("expected validation error for whitespace-only fields, got nil")
	}
}

func TestPersona_Validate(t *testing.T) {
	cases := []struct {
		name    string
		p       Persona
		wantErr bool
	}{
		{"ok", Persona{ID: "x", Prompt: "y"}, false},
		{"missing id", Persona{Prompt: "y"}, true},
		{"missing prompt", Persona{ID: "x"}, true},
		{"blank id", Persona{ID: "  ", Prompt: "y"}, true},
		{"blank prompt", Persona{ID: "x", Prompt: "\t\n"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadPersonasDir_MixedFiles(t *testing.T) {
	dir := t.TempDir()
	// Two valid personas.
	mustWrite(t, filepath.Join(dir, "a.yaml"), "id: a\nprompt: aaa\n")
	mustWrite(t, filepath.Join(dir, "b.yml"), "id: b\nprompt: bbb\n")
	// One malformed.
	mustWrite(t, filepath.Join(dir, "bad.yaml"), "id: [oops\n")
	// One missing required field.
	mustWrite(t, filepath.Join(dir, "no-prompt.yaml"), "id: c\n")
	// Files that should be ignored.
	mustWrite(t, filepath.Join(dir, "README.md"), "not yaml")
	mustWrite(t, filepath.Join(dir, "notes.txt"), "ignore me")
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// File inside subdir should not be picked up (no recursion).
	mustWrite(t, filepath.Join(dir, "sub", "nested.yaml"), "id: nested\nprompt: x\n")

	personas, errs := LoadPersonasDir(dir)
	if len(personas) != 2 {
		t.Errorf("personas: got %d, want 2 (%+v)", len(personas), personas)
	}
	if len(errs) != 2 {
		t.Errorf("errs: got %d, want 2 (%v)", len(errs), errs)
	}

	gotIDs := map[string]bool{}
	for _, p := range personas {
		gotIDs[p.ID] = true
	}
	if !gotIDs["a"] || !gotIDs["b"] {
		t.Errorf("expected personas a and b, got %v", gotIDs)
	}
}

func TestLoadPersonasDir_NonExistent(t *testing.T) {
	personas, errs := LoadPersonasDir(filepath.Join(t.TempDir(), "does-not-exist"))
	if personas != nil {
		t.Errorf("personas: got %v, want nil", personas)
	}
	if errs != nil {
		t.Errorf("errs: got %v, want nil", errs)
	}
}

func TestLoadPersonasDir_Empty(t *testing.T) {
	personas, errs := LoadPersonasDir(t.TempDir())
	if len(personas) != 0 {
		t.Errorf("personas: got %v, want empty", personas)
	}
	if len(errs) != 0 {
		t.Errorf("errs: got %v, want empty", errs)
	}
}

func TestPersonasDir_CreatesDirectory(t *testing.T) {
	home := t.TempDir()
	want := filepath.Join(home, "personas")
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("precondition: %s should not exist yet (err=%v)", want, err)
	}
	got, err := PersonasDir(home)
	if err != nil {
		t.Fatalf("PersonasDir: %v", err)
	}
	if got != want {
		t.Errorf("path: got %q, want %q", got, want)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat %s: %v", got, err)
	}
	if !info.IsDir() {
		t.Errorf("%s is not a directory", got)
	}
}

func TestPersonasDir_IdempotentWhenExists(t *testing.T) {
	home := t.TempDir()
	if _, err := PersonasDir(home); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Calling again on an existing directory should not error.
	if _, err := PersonasDir(home); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
