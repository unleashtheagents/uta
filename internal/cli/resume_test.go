package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/state"
)

func TestResolveResumeSession_ExplicitArgWins(t *testing.T) {
	dir := t.TempDir()
	// Seed an active thread so we can prove the explicit arg overrides it.
	idx, _ := state.Load(dir)
	thr, _ := idx.New("dev", "dev")
	thr.ActiveSessionID = "sess-from-thread"
	if _, err := idx.SetActive(thr.ID); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if err := state.Save(dir, idx); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, name, err := resolveResumeSession([]string{"sess-explicit"}, dir)
	if err != nil {
		t.Fatalf("resolveResumeSession: %v", err)
	}
	if got != "sess-explicit" {
		t.Errorf("sessionID = %q, want sess-explicit", got)
	}
	if name != "" {
		t.Errorf("threadName should be empty when explicit arg is used, got %q", name)
	}
}

func TestResolveResumeSession_FallsBackToActiveThread(t *testing.T) {
	dir := t.TempDir()
	idx, _ := state.Load(dir)
	thr, _ := idx.New("ops", "comms")
	thr.ActiveSessionID = "sess-ops-42"
	if _, err := idx.SetActive(thr.ID); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if err := state.Save(dir, idx); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, name, err := resolveResumeSession(nil, dir)
	if err != nil {
		t.Fatalf("resolveResumeSession: %v", err)
	}
	if got != "sess-ops-42" {
		t.Errorf("sessionID = %q, want sess-ops-42", got)
	}
	if name != "ops" {
		t.Errorf("threadName = %q, want ops", name)
	}
}

func TestResolveResumeSession_NoActiveErrors(t *testing.T) {
	dir := t.TempDir()
	// state.json exists but no active thread.
	idx, _ := state.Load(dir)
	if _, err := idx.New("dev", "dev"); err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := state.Save(dir, idx); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, _, err := resolveResumeSession(nil, dir)
	if err == nil {
		t.Fatal("expected error when no explicit arg and no active thread")
	}
	if !strings.Contains(err.Error(), "no session id") {
		t.Errorf("error should mention missing session: %v", err)
	}
}

func TestResolveResumeSession_ActiveThreadWithoutSessionErrors(t *testing.T) {
	dir := t.TempDir()
	idx, _ := state.Load(dir)
	thr, _ := idx.New("dev", "dev")
	if _, err := idx.SetActive(thr.ID); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	// Note: ActiveSessionID intentionally left empty.
	if err := state.Save(dir, idx); err != nil {
		t.Fatalf("Save: %v", err)
	}

	_, _, err := resolveResumeSession(nil, dir)
	if err == nil {
		t.Fatal("expected error when active thread has no session yet")
	}
}

func TestResolveResumeSession_EmptyExplicitArgErrors(t *testing.T) {
	dir := t.TempDir()
	_, _, err := resolveResumeSession([]string{"   "}, dir)
	if err == nil {
		t.Fatal("expected error for whitespace-only session arg")
	}
}

func TestResolveResumeSession_MalformedStateErrors(t *testing.T) {
	dir := t.TempDir()
	// Drop a malformed state.json — Load should error and the helper
	// should propagate it rather than silently masking with an empty
	// session id.
	if err := os.WriteFile(filepath.Join(dir, state.Filename), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := resolveResumeSession(nil, dir)
	if err == nil {
		t.Fatal("expected error from malformed state.json")
	}
	if !strings.Contains(err.Error(), "load thread state") {
		t.Errorf("error should wrap with 'load thread state': %v", err)
	}
}
