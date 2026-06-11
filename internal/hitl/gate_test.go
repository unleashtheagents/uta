package hitl

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStaticApprover_ApprovePath(t *testing.T) {
	sa := &StaticApprover{Approve: true}
	d, err := sa.Request(context.Background(), Request{Action: "x"})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if !d.Approved {
		t.Errorf("approved=false, want true")
	}
	if d.Approver != "test" {
		t.Errorf("approver=%q, want test", d.Approver)
	}
	if d.ActionID == "" {
		t.Error("expected non-empty action id")
	}
	if sa.Calls() != 1 {
		t.Errorf("calls=%d, want 1", sa.Calls())
	}
}

func TestStaticApprover_DenyPathPropagatesReason(t *testing.T) {
	sa := &StaticApprover{Approve: false, Reason: "policy says no"}
	d, _ := sa.Request(context.Background(), Request{})
	if d.Approved {
		t.Error("approved=true, want false")
	}
	if d.Reason != "policy says no" {
		t.Errorf("reason=%q, want policy says no", d.Reason)
	}
}

// TestGate_NonTTYStdin_FallsBackToAsync asserts that a non-TTY stdin
// never reaches the synchronous read path — the gate must detect "not a
// terminal" and use the file-drop flow even when ForceAsync is false.
// Exercising the actual TTY branch would need a PTY harness, out of
// scope for this layer.
func TestGate_NonTTYStdin_FallsBackToAsync(t *testing.T) {
	dir := t.TempDir()
	g := &Gate{
		StateDir:  dir,
		Stdin:     bytes.NewBufferString(""),
		Stderr:    &bytes.Buffer{},
		AsyncPoll: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	var d Decision
	var rerr error
	go func() {
		d, rerr = g.Request(ctx, Request{SessionID: "S1", Action: "tool_call", PromptText: "X"})
		close(done)
	}()

	// Wait for the pending file to appear, then approve.
	sessionDir := SessionDir(dir, "S1")
	deadline := time.Now().Add(400 * time.Millisecond)
	var actionID string
	for time.Now().Before(deadline) {
		id, err := FirstPending(sessionDir)
		if err == nil && id != "" {
			actionID = id
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if actionID == "" {
		t.Fatalf("pending action never appeared in %s", sessionDir)
	}
	if err := WriteApproval(sessionDir, actionID); err != nil {
		t.Fatalf("WriteApproval: %v", err)
	}
	<-done
	if rerr != nil {
		t.Fatalf("Request err: %v", rerr)
	}
	if !d.Approved {
		t.Errorf("approved=false, want true; reason=%q", d.Reason)
	}
	if d.Approver != "async" {
		t.Errorf("approver=%q, want async", d.Approver)
	}
}

func TestGate_Async_DenyCarriesReason(t *testing.T) {
	dir := t.TempDir()
	g := &Gate{
		StateDir:   dir,
		Stdin:      bytes.NewBufferString(""),
		Stderr:     &bytes.Buffer{},
		ForceAsync: true,
		AsyncPoll:  20 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	var d Decision
	go func() {
		d, _ = g.Request(ctx, Request{SessionID: "S2", Action: "tool_call"})
		close(done)
	}()

	sessionDir := SessionDir(dir, "S2")
	deadline := time.Now().Add(400 * time.Millisecond)
	var actionID string
	for time.Now().Before(deadline) {
		id, _ := FirstPending(sessionDir)
		if id != "" {
			actionID = id
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if actionID == "" {
		t.Fatalf("pending action never appeared")
	}
	if err := WriteDenial(sessionDir, actionID, "operator pressed no"); err != nil {
		t.Fatalf("WriteDenial: %v", err)
	}
	<-done
	if d.Approved {
		t.Errorf("approved=true, want false")
	}
	if !strings.Contains(d.Reason, "operator pressed no") {
		t.Errorf("reason=%q should carry user message", d.Reason)
	}
}

func TestGate_Async_ContextCancelUnblocks(t *testing.T) {
	dir := t.TempDir()
	g := &Gate{
		StateDir:   dir,
		Stdin:      bytes.NewBufferString(""),
		Stderr:     &bytes.Buffer{},
		ForceAsync: true,
		AsyncPoll:  10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var d Decision
	var err error
	go func() {
		d, err = g.Request(ctx, Request{SessionID: "S3"})
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Request did not unblock after cancel")
	}
	if err == nil {
		t.Errorf("expected ctx.Err(), got nil")
	}
	if d.Approved {
		t.Errorf("approved=true on cancel, want false")
	}
}

func TestGate_Async_RequiresStateDir(t *testing.T) {
	g := &Gate{ForceAsync: true, Stdin: bytes.NewBufferString(""), Stderr: &bytes.Buffer{}}
	_, err := g.Request(context.Background(), Request{SessionID: "S"})
	if err == nil {
		t.Fatal("expected error on missing StateDir")
	}
}

func TestGate_Async_RequiresSessionID(t *testing.T) {
	g := &Gate{ForceAsync: true, StateDir: t.TempDir(), Stdin: bytes.NewBufferString(""), Stderr: &bytes.Buffer{}}
	_, err := g.Request(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected error on missing SessionID")
	}
}

func TestFirstPending_NoFileReturnsEmpty(t *testing.T) {
	id, err := FirstPending(t.TempDir())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if id != "" {
		t.Errorf("id=%q, want empty", id)
	}
}

func TestReadPending_NoFileReturnsNil(t *testing.T) {
	p, err := ReadPending(t.TempDir())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if p != nil {
		t.Errorf("p=%+v, want nil", p)
	}
}

func TestReadPending_CorruptJSONReturnsError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pending.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := ReadPending(dir); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

func TestReadPending_RoundtripsPendingSchema(t *testing.T) {
	dir := t.TempDir()
	g := &Gate{StateDir: dir, ForceAsync: true, Stdin: bytes.NewBufferString(""), Stderr: &bytes.Buffer{}, AsyncPoll: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = g.Request(ctx, Request{
			SessionID:  "S-RT",
			SubtaskID:  "sub-1",
			Action:     "tool_call",
			Severity:   "high",
			PromptText: "ship it?",
			Detail:     map[string]any{"tool": "Bash", "pattern": "git push *"},
		})
	}()
	sessionDir := SessionDir(dir, "S-RT")
	deadline := time.Now().Add(500 * time.Millisecond)
	var got *Pending
	for time.Now().Before(deadline) {
		p, err := ReadPending(sessionDir)
		if err == nil && p != nil {
			got = p
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("pending.json never landed on disk")
	}
	if got.SessionID != "S-RT" || got.SubtaskID != "sub-1" || got.Action != "tool_call" ||
		got.Severity != "high" || got.Prompt != "ship it?" || got.ActionID == "" || got.RequestedAt == "" {
		t.Errorf("Pending schema not preserved: %+v", got)
	}
	if !strings.Contains(string(got.Detail), `"tool"`) || !strings.Contains(string(got.Detail), `"Bash"`) {
		t.Errorf("Detail not roundtripped: %s", string(got.Detail))
	}
}

func TestSessionDir_PathShape(t *testing.T) {
	got := SessionDir("/tmp/state", "abc")
	want := filepath.Join("/tmp/state", "hitl", "abc")
	if got != want {
		t.Errorf("SessionDir = %q, want %q", got, want)
	}
}

func TestListPending_NoHITLDirReturnsNilNil(t *testing.T) {
	got, err := ListPending(t.TempDir())
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if got != nil {
		t.Errorf("got=%v, want nil for absent hitl/ dir", got)
	}
}

func TestListPending_SkipsCorruptAndReturnsValid(t *testing.T) {
	stateDir := t.TempDir()

	// Valid session: writes both per-action file + pending.json pointer
	// the same way the gate does.
	goodDir := SessionDir(stateDir, "good")
	if err := os.MkdirAll(goodDir, 0o755); err != nil {
		t.Fatalf("mkdir good: %v", err)
	}
	good := Pending{ActionID: "act-good", SessionID: "good", Action: "tool_call", RequestedAt: "2026-01-01T00:00:00Z"}
	body, err := json.MarshalIndent(good, "", "  ")
	if err != nil {
		t.Fatalf("marshal good: %v", err)
	}
	if err := os.WriteFile(filepath.Join(goodDir, "pending.json"), body, 0o644); err != nil {
		t.Fatalf("write good pending: %v", err)
	}

	// Corrupt session: pending.json is malformed — must be skipped, not
	// abort the whole listing.
	badDir := SessionDir(stateDir, "bad")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatalf("mkdir bad: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "pending.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write bad pending: %v", err)
	}

	// Empty session: no pending.json at all — must be skipped.
	emptyDir := SessionDir(stateDir, "empty")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}

	got, err := ListPending(stateDir)
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1: %+v", len(got), got)
	}
	if got[0].SessionID != "good" || got[0].ActionID != "act-good" {
		t.Errorf("unexpected entry: %+v", got[0])
	}
}

func TestGate_Async_WritesPendingPointer(t *testing.T) {
	dir := t.TempDir()
	g := &Gate{StateDir: dir, ForceAsync: true, Stdin: bytes.NewBufferString(""), Stderr: &bytes.Buffer{}, AsyncPoll: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = g.Request(ctx, Request{SessionID: "P1", Action: "tool_call"}) }()

	sessionDir := SessionDir(dir, "P1")
	deadline := time.Now().Add(500 * time.Millisecond)
	var ok bool
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(sessionDir, "pending.json")); err == nil {
			ok = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok {
		t.Fatalf("pending.json pointer never appeared in %s", sessionDir)
	}
}
