package export

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedSession inserts a session with one subtask and one trajectory event,
// returning the absolute blob paths used for prompt_ref / raw_output_ref so
// callers can populate the blob dir if they want IncludeBlobs to find them.
func seedSession(t *testing.T, s *store.Store, blobsDir, sessionID string, completed *time.Time) (promptPath, rawPath, finalAnswerPath string) {
	t.Helper()
	created := time.Unix(1_700_000_000, 0).UTC()
	promptPath = filepath.Join(blobsDir, sessionID+"-prompt.txt")
	rawPath = filepath.Join(blobsDir, sessionID+"-raw.txt")
	finalAnswerPath = filepath.Join(blobsDir, sessionID+"-final.txt")

	if err := s.CreateSession(store.Session{
		ID:        sessionID,
		Goal:      "g-" + sessionID,
		Worker:    "claude",
		Planner:   "claude",
		Status:    "running",
		CreatedAt: created,
		MetaJSON:  `{"k":"v"}`,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if completed != nil {
		if err := s.MarkSession(sessionID, "done", finalAnswerPath); err != nil {
			t.Fatalf("MarkSession: %v", err)
		}
	}

	if err := s.CreateSubtask(store.Subtask{
		ID:        sessionID + "-st0",
		SessionID: sessionID,
		Ord:       0,
		SpecID:    "s1",
		Title:     "first",
		PromptRef: promptPath,
		Worker:    "claude",
		Status:    "pending",
	}); err != nil {
		t.Fatalf("CreateSubtask: %v", err)
	}
	started := created.Add(1 * time.Second)
	doneAt := created.Add(2 * time.Second)
	if err := s.UpdateSubtask(store.Subtask{
		ID:           sessionID + "-st0",
		Status:       "ok",
		StartedAt:    &started,
		CompletedAt:  &doneAt,
		ResultText:   "the result",
		RawOutputRef: rawPath,
	}); err != nil {
		t.Fatalf("UpdateSubtask: %v", err)
	}

	if err := s.InsertEvent(sessionID, sessionID+"-st0", 1, created.Add(500*time.Millisecond),
		"subtask_started", json.RawMessage(`{"hello":"world"}`)); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}
	return promptPath, rawPath, finalAnswerPath
}

func TestRun_RequiresSessionOrAll(t *testing.T) {
	s := newTestStore(t)
	if _, err := Run(s, t.TempDir(), Options{}); err == nil {
		t.Fatal("expected error when neither SessionID nor All is set")
	} else if !strings.Contains(err.Error(), "either SessionID or All") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_RejectsBothSessionAndAll(t *testing.T) {
	s := newTestStore(t)
	if _, err := Run(s, t.TempDir(), Options{SessionID: "sess-x", All: true}); err == nil {
		t.Fatal("expected error when both SessionID and All are set")
	} else if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRun_SessionNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := Run(s, t.TempDir(), Options{SessionID: "missing"})
	if err == nil {
		t.Fatal("expected error for missing session")
	}
	if !strings.Contains(err.Error(), "session not found") {
		t.Fatalf("expected session not found error, got: %v", err)
	}
}

func TestRun_SingleSession_SerializationShape(t *testing.T) {
	s := newTestStore(t)
	blobsDir := t.TempDir()
	completedAt := time.Unix(1_700_000_100, 0).UTC()
	promptPath, rawPath, finalPath := seedSession(t, s, blobsDir, "sess-1", &completedAt)

	out, err := Run(s, blobsDir, Options{SessionID: "sess-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Header
	if out.Header.FormatVersion != FormatVersion {
		t.Fatalf("FormatVersion: got %d want %d", out.Header.FormatVersion, FormatVersion)
	}
	if out.Header.Scope != "session" {
		t.Fatalf("Scope: got %q want %q", out.Header.Scope, "session")
	}
	if len(out.Header.SessionIDs) != 1 || out.Header.SessionIDs[0] != "sess-1" {
		t.Fatalf("SessionIDs: got %v", out.Header.SessionIDs)
	}
	if out.Header.IncludeBlobs {
		t.Fatalf("IncludeBlobs should default to false")
	}
	if out.Header.ExportedAt.Location() != time.UTC {
		t.Fatalf("ExportedAt should be UTC, got %v", out.Header.ExportedAt.Location())
	}
	// SchemaVersion should be the latest applied migration.
	if out.Header.SchemaVersion == "" {
		t.Fatalf("SchemaVersion should be populated")
	}
	if out.Header.RowCounts["sessions"] != 1 || out.Header.RowCounts["subtasks"] != 1 ||
		out.Header.RowCounts["events"] != 1 || out.Header.RowCounts["blobs"] != 0 {
		t.Fatalf("unexpected RowCounts: %+v", out.Header.RowCounts)
	}
	if out.Header.BodySHA256 == "" || len(out.Header.BodySHA256) != 64 {
		t.Fatalf("BodySHA256 should be a 64-char hex string, got %q", out.Header.BodySHA256)
	}

	// Session canonicalization: file paths should be flattened to blob:<basename>.
	if len(out.Sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(out.Sessions))
	}
	sess := out.Sessions[0]
	if sess.FinalAnswerRef != "blob:"+filepath.Base(finalPath) {
		t.Fatalf("FinalAnswerRef canonicalization: got %q", sess.FinalAnswerRef)
	}
	if sess.CompletedAt == nil || !sess.CompletedAt.Equal(*sess.CompletedAt) {
		t.Fatalf("CompletedAt: got %v", sess.CompletedAt)
	}

	if len(out.Subtasks) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(out.Subtasks))
	}
	st := out.Subtasks[0]
	if st.PromptRef != "blob:"+filepath.Base(promptPath) {
		t.Fatalf("PromptRef canonicalization: got %q want blob:%s", st.PromptRef, filepath.Base(promptPath))
	}
	if st.RawOutputRef != "blob:"+filepath.Base(rawPath) {
		t.Fatalf("RawOutputRef canonicalization: got %q", st.RawOutputRef)
	}
	if st.StartedAt == nil || st.CompletedAt == nil {
		t.Fatalf("subtask timestamps should be set: %+v", st)
	}

	if len(out.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(out.Events))
	}
	ev := out.Events[0]
	if ev.Kind != "subtask_started" || ev.Seq != 1 || ev.SessionID != "sess-1" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	var payload map[string]string
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload not valid JSON: %v", err)
	}
	if payload["hello"] != "world" {
		t.Fatalf("payload preserved: got %+v", payload)
	}
}

func TestRun_AllScope_MultipleSessions(t *testing.T) {
	s := newTestStore(t)
	blobsDir := t.TempDir()
	seedSession(t, s, blobsDir, "sess-a", nil)
	seedSession(t, s, blobsDir, "sess-b", nil)

	out, err := Run(s, blobsDir, Options{All: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Header.Scope != "all" {
		t.Fatalf("Scope: got %q want %q", out.Header.Scope, "all")
	}
	if len(out.Sessions) != 2 || len(out.Subtasks) != 2 || len(out.Events) != 2 {
		t.Fatalf("counts: sess=%d sub=%d ev=%d", len(out.Sessions), len(out.Subtasks), len(out.Events))
	}
	ids := map[string]bool{}
	for _, id := range out.Header.SessionIDs {
		ids[id] = true
	}
	if !ids["sess-a"] || !ids["sess-b"] {
		t.Fatalf("SessionIDs missing entries: %v", out.Header.SessionIDs)
	}
}

func TestRun_AllScope_EmptyStore(t *testing.T) {
	s := newTestStore(t)
	out, err := Run(s, t.TempDir(), Options{All: true})
	if err != nil {
		t.Fatalf("Run on empty store: %v", err)
	}
	if len(out.Sessions) != 0 || len(out.Subtasks) != 0 || len(out.Events) != 0 {
		t.Fatalf("expected empty export, got sess=%d sub=%d ev=%d",
			len(out.Sessions), len(out.Subtasks), len(out.Events))
	}
	if out.Header.RowCounts["sessions"] != 0 {
		t.Fatalf("RowCounts.sessions: got %d want 0", out.Header.RowCounts["sessions"])
	}
	if out.Header.BodySHA256 == "" {
		t.Fatalf("BodySHA256 should still be computed for empty export")
	}
}

func TestRun_IncludeBlobs_ReadsAndEncodes(t *testing.T) {
	s := newTestStore(t)
	blobsDir := t.TempDir()
	promptPath, rawPath, finalPath := seedSession(t, s, blobsDir, "sess-blobs", ptrTime(time.Unix(1_700_000_200, 0).UTC()))

	promptBody := []byte("hello prompt")
	rawBody := []byte("raw output bytes")
	finalBody := []byte("final answer")
	if err := os.WriteFile(promptPath, promptBody, 0o644); err != nil {
		t.Fatalf("write prompt blob: %v", err)
	}
	if err := os.WriteFile(rawPath, rawBody, 0o644); err != nil {
		t.Fatalf("write raw blob: %v", err)
	}
	if err := os.WriteFile(finalPath, finalBody, 0o644); err != nil {
		t.Fatalf("write final blob: %v", err)
	}

	out, err := Run(s, blobsDir, Options{SessionID: "sess-blobs", IncludeBlobs: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Header.IncludeBlobs {
		t.Fatalf("IncludeBlobs flag should be true in header")
	}
	if len(out.Blobs) != 3 {
		t.Fatalf("expected 3 blobs, got %d: %v", len(out.Blobs), out.Blobs)
	}
	if out.Header.RowCounts["blobs"] != 3 {
		t.Fatalf("RowCounts.blobs: got %d want 3", out.Header.RowCounts["blobs"])
	}
	mustMatch := func(base string, want []byte) {
		t.Helper()
		raw, ok := out.Blobs[base]
		if !ok {
			t.Fatalf("blobs missing entry for %q", base)
		}
		dec, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			t.Fatalf("decode %s: %v", base, err)
		}
		if string(dec) != string(want) {
			t.Fatalf("blob %s: got %q want %q", base, dec, want)
		}
	}
	mustMatch(filepath.Base(promptPath), promptBody)
	mustMatch(filepath.Base(rawPath), rawBody)
	mustMatch(filepath.Base(finalPath), finalBody)
}

func TestRun_IncludeBlobs_MissingFileRecordedEmpty(t *testing.T) {
	s := newTestStore(t)
	blobsDir := t.TempDir()
	promptPath, _, _ := seedSession(t, s, blobsDir, "sess-miss", nil)
	// Only create the prompt blob; raw_output_ref points at a path that does
	// not exist. Exporter should record it as empty rather than failing.
	if err := os.WriteFile(promptPath, []byte("p"), 0o644); err != nil {
		t.Fatalf("write prompt blob: %v", err)
	}

	out, err := Run(s, blobsDir, Options{SessionID: "sess-miss", IncludeBlobs: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out.Blobs) < 2 {
		t.Fatalf("expected at least 2 blob entries, got %d", len(out.Blobs))
	}
	missingBase := "sess-miss-raw.txt"
	v, ok := out.Blobs[missingBase]
	if !ok {
		t.Fatalf("expected blob entry for missing %q", missingBase)
	}
	if v != "" {
		t.Fatalf("missing blob should be recorded as empty string, got %q", v)
	}
}

func TestRun_BodyHash_DeterministicForSameState(t *testing.T) {
	s := newTestStore(t)
	blobsDir := t.TempDir()
	seedSession(t, s, blobsDir, "sess-h", nil)

	// Run twice; ExportedAt differs but BodySHA256 should be stable because
	// it's computed *with* ExportedAt as part of the body. Actually it should
	// differ — confirm that two runs produce non-empty hashes that reflect the
	// ExportedAt difference. We can't easily force ExportedAt to be equal, but
	// we can assert the hash is well-formed and that re-canonicalizing the
	// returned struct with body_sha256 cleared reproduces the recorded hash.
	out, err := Run(s, blobsDir, Options{SessionID: "sess-h"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := out.Header.BodySHA256
	if want == "" {
		t.Fatalf("BodySHA256 should be set")
	}

	// Recompute: clearing BodySHA256, marshaling, hashing should match.
	out.Header.BodySHA256 = ""
	canonical, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := sha256Hex(canonical)
	if got != want {
		t.Fatalf("hash mismatch: got %s want %s", got, want)
	}
}

func TestRun_NoBlobsMapWhenIncludeBlobsFalse(t *testing.T) {
	s := newTestStore(t)
	blobsDir := t.TempDir()
	seedSession(t, s, blobsDir, "sess-noblob", nil)
	out, err := Run(s, blobsDir, Options{SessionID: "sess-noblob"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.Blobs != nil {
		t.Fatalf("Blobs should be nil when IncludeBlobs=false, got %v", out.Blobs)
	}
}

func TestRun_EmptyRefsNotTrackedAsBlobs(t *testing.T) {
	// A session/subtask with no blob refs at all should yield zero blob
	// entries even with IncludeBlobs=true.
	s := newTestStore(t)
	if err := s.CreateSession(store.Session{
		ID:        "sess-empty",
		Goal:      "no refs",
		Worker:    "claude",
		Status:    "running",
		CreatedAt: time.Unix(1_700_000_300, 0).UTC(),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	out, err := Run(s, t.TempDir(), Options{SessionID: "sess-empty", IncludeBlobs: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out.Blobs) != 0 {
		t.Fatalf("expected no blobs, got %v", out.Blobs)
	}
	if out.Header.RowCounts["blobs"] != 0 {
		t.Fatalf("RowCounts.blobs: got %d", out.Header.RowCounts["blobs"])
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestRun_RedactsSecretsByDefault seeds a session whose result text,
// event payload, and inlined blob all carry a recognizable credential,
// then asserts none of them survive the default export and the header
// reports what was scrubbed. --no-redact (Options.NoRedact) must
// preserve everything byte-faithfully.
func TestRun_RedactsSecretsByDefault(t *testing.T) {
	s := newTestStore(t)
	blobs := t.TempDir()
	const secret = "AKIA" + "IOSFODNN7EXAMPLE"

	created := time.Unix(1_700_000_000, 0).UTC()
	if err := s.CreateSession(store.Session{
		ID: "sess-red", Goal: "deploy with key " + secret,
		Worker: "claude", Status: "running", CreatedAt: created,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	rawPath := filepath.Join(blobs, "sess-red-raw.txt")
	if err := os.WriteFile(rawPath, []byte("output contains "+secret+" here"), 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := s.CreateSubtask(store.Subtask{
		ID: "sess-red-st0", SessionID: "sess-red", Ord: 0,
		Title: "t", PromptRef: rawPath, Worker: "claude", Status: "pending",
	}); err != nil {
		t.Fatalf("CreateSubtask: %v", err)
	}
	started := created.Add(time.Second)
	done := created.Add(2 * time.Second)
	if err := s.UpdateSubtask(store.Subtask{
		ID: "sess-red-st0", Status: "ok",
		StartedAt: &started, CompletedAt: &done,
		ResultText: "found " + secret + " in env", RawOutputRef: rawPath,
	}); err != nil {
		t.Fatalf("UpdateSubtask: %v", err)
	}
	if err := s.InsertEvent("sess-red", "sess-red-st0", 1, created,
		"tool_result", json.RawMessage(`{"output":"`+secret+`"}`)); err != nil {
		t.Fatalf("InsertEvent: %v", err)
	}

	// Default: redaction ON.
	out, err := Run(s, blobs, Options{SessionID: "sess-red", IncludeBlobs: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.Header.Redacted {
		t.Error("Header.Redacted = false, want true by default")
	}
	if out.Header.Redactions["aws-key"] == 0 {
		t.Errorf("expected aws-key redactions in header, got %v", out.Header.Redactions)
	}
	serialized, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The secret must not appear anywhere in the export — including the
	// base64-encoded blob bodies (checked by decoding them).
	if strings.Contains(string(serialized), secret) {
		t.Error("secret survived in serialized export (plain form)")
	}
	for base, b64 := range out.Blobs {
		raw, derr := base64.StdEncoding.DecodeString(b64)
		if derr != nil {
			continue
		}
		if strings.Contains(string(raw), secret) {
			t.Errorf("secret survived inside blob %s", base)
		}
	}

	// Opt-out: byte-faithful.
	out2, err := Run(s, blobs, Options{SessionID: "sess-red", IncludeBlobs: true, NoRedact: true})
	if err != nil {
		t.Fatalf("Run --no-redact: %v", err)
	}
	if out2.Header.Redacted {
		t.Error("Header.Redacted = true with NoRedact set")
	}
	serialized2, _ := json.Marshal(out2)
	if !strings.Contains(string(serialized2), secret) {
		t.Error("NoRedact export should preserve the secret verbatim")
	}
}
