package engine

import (
	"strings"
	"testing"
)

func TestBuiltinPersonas_NonEmpty(t *testing.T) {
	if len(BuiltinPersonas) == 0 {
		t.Fatal("BuiltinPersonas is empty; expected at least one starter persona")
	}
}

func TestBuiltinPersonas_NoIDCollisions(t *testing.T) {
	seen := make(map[string]int, len(BuiltinPersonas))
	for i, p := range BuiltinPersonas {
		if prev, dup := seen[p.ID]; dup {
			t.Errorf("duplicate persona ID %q at indexes %d and %d", p.ID, prev, i)
		}
		seen[p.ID] = i
	}
}

func TestBuiltinPersonas_FieldInvariants(t *testing.T) {
	for i, p := range BuiltinPersonas {
		if strings.TrimSpace(p.ID) == "" {
			t.Errorf("persona[%d] has empty ID", i)
		}
		if p.ID != strings.TrimSpace(p.ID) {
			t.Errorf("persona[%d] ID %q has leading/trailing whitespace", i, p.ID)
		}
		if strings.ContainsAny(p.ID, " \t\n") {
			t.Errorf("persona[%d] ID %q contains whitespace", i, p.ID)
		}
		if strings.TrimSpace(p.Title) == "" {
			t.Errorf("persona[%q] has empty Title", p.ID)
		}
		if strings.TrimSpace(p.Prompt) == "" {
			t.Errorf("persona[%q] has empty Prompt", p.ID)
		}
		// Built-in personas should ship without a Worker pin so they pick up
		// the reflector's DefaultWorker — confirm we haven't hard-coded one.
		if p.Worker != "" {
			t.Errorf("persona[%q] pins Worker=%q; built-ins should leave it empty so the reflector default applies", p.ID, p.Worker)
		}
	}
}

func TestBuiltinPersonas_PromptMentionsSeverity(t *testing.T) {
	// Every starter critic should give the model some guidance on severity
	// so its findings line up with the audit pipeline's HIGH/MEDIUM/LOW/INFO
	// rubric. Accept either the literal word "severity" or any of the rubric
	// tags themselves, since some prompts only enumerate the tags.
	for _, p := range BuiltinPersonas {
		lower := strings.ToLower(p.Prompt)
		if strings.Contains(lower, "severity") {
			continue
		}
		if strings.Contains(p.Prompt, "HIGH") || strings.Contains(p.Prompt, "MEDIUM") ||
			strings.Contains(p.Prompt, "LOW") || strings.Contains(p.Prompt, "INFO") {
			continue
		}
		t.Errorf("persona[%q] Prompt does not mention severity guidance", p.ID)
	}
}
