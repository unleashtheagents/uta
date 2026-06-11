package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/unleashtheagents/uta/internal/hitl"
)

// setupHITLHome isolates HOME and UTA_HOME so newApp() opens an empty
// global store, and returns the state directory the hitl command will
// write approval/denial markers under.
func setupHITLHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	utaHome := filepath.Join(home, ".uta")
	if err := os.MkdirAll(utaHome, 0o755); err != nil {
		t.Fatalf("mkdir uta home: %v", err)
	}
	t.Setenv("UTA_HOME", utaHome)

	// Chdir to a non-project directory so newApp() doesn't auto-detect a
	// stray parent project and redirect StateDir.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
	return utaHome
}

// runHITL builds a fresh hitl command tree and executes it with args.
func runHITL(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := newHITLCmd()
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errBuf.String(), err
}

// seedPending writes both <action>.pending.json and the pending.json pointer
// the gate would have written, so the CLI can resolve the action id without
// --action.
func seedPending(t *testing.T, sessionDir, actionID, sessionID string) {
	t.Helper()
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	body, err := json.MarshalIndent(hitl.Pending{
		ActionID:    actionID,
		SessionID:   sessionID,
		Action:      "tool_call",
		RequestedAt: "2026-05-18T00:00:00Z",
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal pending: %v", err)
	}
	if err := os.WriteFile(hitl.PendingPath(sessionDir, actionID), body, 0o644); err != nil {
		t.Fatalf("write per-action pending: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "pending.json"), body, 0o644); err != nil {
		t.Fatalf("write pending pointer: %v", err)
	}
}

func TestHITLApprove_WritesApprovalMarkerWithExplicitAction(t *testing.T) {
	stateDir := setupHITLHome(t)
	sessionID := "sess-approve-explicit"
	actionID := "act-1111"
	sessionDir := hitl.SessionDir(stateDir, sessionID)
	seedPending(t, sessionDir, actionID, sessionID)

	out, _, err := runHITL(t, "approve", sessionID, "--action", actionID)
	if err != nil {
		t.Fatalf("hitl approve: %v", err)
	}
	if !strings.Contains(out, "approved "+sessionID+"/"+actionID) {
		t.Errorf("stdout missing approval line: %q", out)
	}
	if _, err := os.Stat(hitl.ApprovedPath(sessionDir, actionID)); err != nil {
		t.Errorf("approved marker not written: %v", err)
	}
	if _, err := os.Stat(hitl.DeniedPath(sessionDir, actionID)); !os.IsNotExist(err) {
		t.Errorf("denied marker should not exist after approve: %v", err)
	}
}

func TestHITLApprove_DefaultsToFirstPending(t *testing.T) {
	stateDir := setupHITLHome(t)
	sessionID := "sess-default-action"
	actionID := "act-default-2222"
	sessionDir := hitl.SessionDir(stateDir, sessionID)
	seedPending(t, sessionDir, actionID, sessionID)

	// No --action flag — should resolve from pending.json pointer.
	out, _, err := runHITL(t, "approve", sessionID)
	if err != nil {
		t.Fatalf("hitl approve (default action): %v", err)
	}
	if !strings.Contains(out, actionID) {
		t.Errorf("stdout should mention resolved action id %q: %q", actionID, out)
	}
	if _, err := os.Stat(hitl.ApprovedPath(sessionDir, actionID)); err != nil {
		t.Errorf("approved marker not written: %v", err)
	}
}

func TestHITLApprove_DenyWritesDeniedMarkerWithReason(t *testing.T) {
	stateDir := setupHITLHome(t)
	sessionID := "sess-deny-reason"
	actionID := "act-deny-3333"
	sessionDir := hitl.SessionDir(stateDir, sessionID)
	seedPending(t, sessionDir, actionID, sessionID)

	reason := "policy violation"
	out, _, err := runHITL(t, "approve", sessionID, "--action", actionID, "--deny", "--reason", reason)
	if err != nil {
		t.Fatalf("hitl approve --deny: %v", err)
	}
	if !strings.Contains(out, "denied "+sessionID+"/"+actionID) || !strings.Contains(out, reason) {
		t.Errorf("stdout missing denial line with reason: %q", out)
	}
	got, err := os.ReadFile(hitl.DeniedPath(sessionDir, actionID))
	if err != nil {
		t.Fatalf("read denied marker: %v", err)
	}
	if string(got) != reason {
		t.Errorf("denied marker body = %q; want %q", string(got), reason)
	}
	if _, err := os.Stat(hitl.ApprovedPath(sessionDir, actionID)); !os.IsNotExist(err) {
		t.Errorf("approved marker should not exist after deny: %v", err)
	}
}

func TestHITLApprove_DenyWithoutReasonUsesDefault(t *testing.T) {
	stateDir := setupHITLHome(t)
	sessionID := "sess-deny-default"
	actionID := "act-deny-4444"
	sessionDir := hitl.SessionDir(stateDir, sessionID)
	seedPending(t, sessionDir, actionID, sessionID)

	_, _, err := runHITL(t, "approve", sessionID, "--action", actionID, "--deny")
	if err != nil {
		t.Fatalf("hitl approve --deny: %v", err)
	}
	got, err := os.ReadFile(hitl.DeniedPath(sessionDir, actionID))
	if err != nil {
		t.Fatalf("read denied marker: %v", err)
	}
	if !strings.Contains(string(got), "denied via") {
		t.Errorf("default denial reason missing; got %q", string(got))
	}
}

func TestHITLApprove_NoPendingRequestErrors(t *testing.T) {
	setupHITLHome(t)
	// No pending file seeded — defaulting must surface a clear error.
	_, _, err := runHITL(t, "approve", "sess-empty")
	if err == nil {
		t.Fatal("expected error when no pending request exists")
	}
	if !strings.Contains(err.Error(), "no pending HITL request") {
		t.Errorf("error should mention no pending request: %v", err)
	}
}

func TestHITLApprove_RequiresSessionArg(t *testing.T) {
	setupHITLHome(t)
	_, _, err := runHITL(t, "approve")
	if err == nil {
		t.Fatal("expected error when session arg is omitted")
	}
}

func TestHITLApprove_ReasonWithoutDenyIsRejected(t *testing.T) {
	stateDir := setupHITLHome(t)
	sessionID := "sess-reason-no-deny"
	actionID := "act-no-deny"
	sessionDir := hitl.SessionDir(stateDir, sessionID)
	seedPending(t, sessionDir, actionID, sessionID)

	_, _, err := runHITL(t, "approve", sessionID, "--action", actionID, "--reason", "looks fine")
	if err == nil {
		t.Fatal("expected error when --reason supplied without --deny")
	}
	if !strings.Contains(err.Error(), "--deny") {
		t.Errorf("error should mention --deny requirement: %v", err)
	}
	// Neither marker should have been written.
	if _, err := os.Stat(hitl.ApprovedPath(sessionDir, actionID)); !os.IsNotExist(err) {
		t.Errorf("approved marker should not exist when validation rejected the call: %v", err)
	}
	if _, err := os.Stat(hitl.DeniedPath(sessionDir, actionID)); !os.IsNotExist(err) {
		t.Errorf("denied marker should not exist when validation rejected the call: %v", err)
	}
}

func TestHITLList_EmptyStateDirReportsNothing(t *testing.T) {
	setupHITLHome(t)
	out, _, err := runHITL(t, "list")
	if err != nil {
		t.Fatalf("hitl list: %v", err)
	}
	if !strings.Contains(out, "no pending HITL requests") {
		t.Errorf("expected friendly empty-state line, got: %q", out)
	}
}

func TestHITLList_ShowsPendingAcrossSessions(t *testing.T) {
	stateDir := setupHITLHome(t)
	for _, s := range []struct{ sess, act string }{
		{"sess-A", "act-A"},
		{"sess-B", "act-B"},
	} {
		seedPending(t, hitl.SessionDir(stateDir, s.sess), s.act, s.sess)
	}

	out, _, err := runHITL(t, "list")
	if err != nil {
		t.Fatalf("hitl list: %v", err)
	}
	for _, want := range []string{"sess-A", "act-A", "sess-B", "act-B", "SESSION", "ACTION_ID"} {
		if !strings.Contains(out, want) {
			t.Errorf("hitl list output missing %q: %s", want, out)
		}
	}
}

func TestHITLList_JSONOutputIsArray(t *testing.T) {
	stateDir := setupHITLHome(t)
	seedPending(t, hitl.SessionDir(stateDir, "sess-json"), "act-json", "sess-json")

	out, _, err := runHITL(t, "list", "--json")
	if err != nil {
		t.Fatalf("hitl list --json: %v", err)
	}
	var decoded []hitl.Pending
	if jerr := json.Unmarshal([]byte(out), &decoded); jerr != nil {
		t.Fatalf("decode JSON list: %v (out=%q)", jerr, out)
	}
	if len(decoded) != 1 || decoded[0].SessionID != "sess-json" || decoded[0].ActionID != "act-json" {
		t.Errorf("unexpected list payload: %+v", decoded)
	}
}

func TestHITLList_JSONOnEmptyIsEmptyArray(t *testing.T) {
	setupHITLHome(t)
	out, _, err := runHITL(t, "list", "--json")
	if err != nil {
		t.Fatalf("hitl list --json (empty): %v", err)
	}
	// Must serialize as `[]`, not `null`, so consumers can iterate
	// without nil-checking the array.
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("expected `[]` on empty state, got %q", out)
	}
}
