package export

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/store"
)

// runExport is a small convenience that builds an Export from a seeded store
// so import tests can exercise Restore against realistic input.
func runExport(t *testing.T, s *store.Store, blobsDir string, opts Options) *Export {
	t.Helper()
	exp, err := Run(s, blobsDir, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return exp
}

func TestRestore_NilExport(t *testing.T) {
	s := newTestStore(t)
	if _, err := Restore(s, t.TempDir(), nil, ImportOptions{}); err == nil {
		t.Fatal("expected error for nil export")
	} else if !strings.Contains(err.Error(), "nil export") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRestore_UnsupportedFormatVersion(t *testing.T) {
	s := newTestStore(t)
	exp := &Export{Header: Header{FormatVersion: FormatVersion + 99}}
	if _, err := Restore(s, t.TempDir(), exp, ImportOptions{SkipHashCheck: true}); err == nil {
		t.Fatal("expected error for unsupported format_version")
	} else if !strings.Contains(err.Error(), "unsupported format_version") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRestore_BodyHashMismatch(t *testing.T) {
	src := newTestStore(t)
	srcBlobs := t.TempDir()
	seedSession(t, src, srcBlobs, "sess-tamper", nil)
	exp := runExport(t, src, srcBlobs, Options{SessionID: "sess-tamper"})

	// Tamper with a session field after the hash was computed.
	exp.Sessions[0].Goal = "tampered"

	dst := newTestStore(t)
	if _, err := Restore(dst, t.TempDir(), exp, ImportOptions{}); err == nil {
		t.Fatal("expected body sha256 mismatch error")
	} else if !strings.Contains(err.Error(), "body sha256 mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRestore_SkipHashCheckBypassesVerification(t *testing.T) {
	src := newTestStore(t)
	srcBlobs := t.TempDir()
	seedSession(t, src, srcBlobs, "sess-skip", nil)
	exp := runExport(t, src, srcBlobs, Options{SessionID: "sess-skip"})
	exp.Sessions[0].Goal = "edited but skip-hash"

	dst := newTestStore(t)
	res, err := Restore(dst, t.TempDir(), exp, ImportOptions{SkipHashCheck: true})
	if err != nil {
		t.Fatalf("Restore with SkipHashCheck: %v", err)
	}
	if res.SessionsImported != 1 {
		t.Fatalf("expected 1 session imported, got %d", res.SessionsImported)
	}
	got, err := dst.GetSession("sess-skip")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Goal != "edited but skip-hash" {
		t.Fatalf("Goal: got %q want %q", got.Goal, "edited but skip-hash")
	}
}

func TestRestore_Roundtrip_WithBlobs(t *testing.T) {
	src := newTestStore(t)
	srcBlobs := t.TempDir()
	completedAt := time.Unix(1_700_001_000, 0).UTC()
	promptPath, rawPath, finalPath := seedSession(t, src, srcBlobs, "sess-rt", &completedAt)

	promptBody := []byte("the prompt body")
	rawBody := []byte("the raw output body")
	finalBody := []byte("the final answer body")
	for path, body := range map[string][]byte{
		promptPath: promptBody,
		rawPath:    rawBody,
		finalPath:  finalBody,
	} {
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatalf("write blob %s: %v", path, err)
		}
	}

	exp := runExport(t, src, srcBlobs, Options{SessionID: "sess-rt", IncludeBlobs: true})

	dst := newTestStore(t)
	dstBlobs := filepath.Join(t.TempDir(), "blobs")
	res, err := Restore(dst, dstBlobs, exp, ImportOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if res.SessionsImported != 1 || res.SubtasksImported != 1 || res.EventsImported != 1 {
		t.Fatalf("counts: sess=%d sub=%d ev=%d", res.SessionsImported, res.SubtasksImported, res.EventsImported)
	}
	if res.BlobsImported != 3 {
		t.Fatalf("BlobsImported: got %d want 3", res.BlobsImported)
	}
	if len(res.SessionsReplaced) != 0 || len(res.MissingBlobRefs) != 0 {
		t.Fatalf("did not expect replacements or missing refs: %+v", res)
	}

	// Verify session row, including blob path rewriting from "blob:<base>" back
	// to an absolute path under dstBlobs.
	gotSess, err := dst.GetSession("sess-rt")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if gotSess.Goal != "g-sess-rt" || gotSess.Worker != "claude" || gotSess.Status != "done" {
		t.Fatalf("session core fields: %+v", gotSess)
	}
	if gotSess.FinalAnswerRef != filepath.Join(dstBlobs, filepath.Base(finalPath)) {
		t.Fatalf("FinalAnswerRef should point under dstBlobs: got %q", gotSess.FinalAnswerRef)
	}
	if gotSess.CompletedAt == nil {
		t.Fatalf("CompletedAt should be set, got nil")
	}

	// Verify subtask row.
	subs, err := dst.SubtaskListBySession("sess-rt", 0, 0)
	if err != nil {
		t.Fatalf("SubtaskListBySession: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 subtask, got %d", len(subs))
	}
	st := subs[0]
	if st.PromptRef != filepath.Join(dstBlobs, filepath.Base(promptPath)) {
		t.Fatalf("PromptRef: got %q", st.PromptRef)
	}
	if st.RawOutputRef != filepath.Join(dstBlobs, filepath.Base(rawPath)) {
		t.Fatalf("RawOutputRef: got %q", st.RawOutputRef)
	}
	if st.ResultText != "the result" {
		t.Fatalf("ResultText: got %q", st.ResultText)
	}

	// Verify event row.
	events, err := dst.ListEvents("sess-rt", 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.Kind != "subtask_started" || ev.Seq != 1 {
		t.Fatalf("event fields: %+v", ev)
	}

	// Verify blob files were materialized with original bodies.
	for path, want := range map[string][]byte{
		filepath.Join(dstBlobs, filepath.Base(promptPath)): promptBody,
		filepath.Join(dstBlobs, filepath.Base(rawPath)):    rawBody,
		filepath.Join(dstBlobs, filepath.Base(finalPath)):  finalBody,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read materialized blob %s: %v", path, err)
		}
		if string(got) != string(want) {
			t.Fatalf("blob %s: got %q want %q", path, got, want)
		}
	}
}

func TestRestore_CollisionWithoutForce_Errors(t *testing.T) {
	src := newTestStore(t)
	srcBlobs := t.TempDir()
	seedSession(t, src, srcBlobs, "sess-clash", nil)
	exp := runExport(t, src, srcBlobs, Options{SessionID: "sess-clash"})

	// Pre-populate dst with a session of the same id.
	dst := newTestStore(t)
	if err := dst.CreateSession(store.Session{
		ID:        "sess-clash",
		Goal:      "pre-existing",
		Worker:    "claude",
		Status:    "running",
		CreatedAt: time.Unix(1_700_002_000, 0).UTC(),
	}); err != nil {
		t.Fatalf("seed dst: %v", err)
	}

	_, err := Restore(dst, t.TempDir(), exp, ImportOptions{})
	if err == nil {
		t.Fatal("expected collision error without Force")
	}
	if !strings.Contains(err.Error(), "would clobber") {
		t.Fatalf("unexpected error: %v", err)
	}

	// The pre-existing row should be untouched (no replace, no rewrite of Goal).
	got, err := dst.GetSession("sess-clash")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Goal != "pre-existing" {
		t.Fatalf("pre-existing row was mutated: %+v", got)
	}
}

func TestRestore_CollisionWithForce_Replaces(t *testing.T) {
	src := newTestStore(t)
	srcBlobs := t.TempDir()
	seedSession(t, src, srcBlobs, "sess-force", nil)
	exp := runExport(t, src, srcBlobs, Options{SessionID: "sess-force"})

	dst := newTestStore(t)
	if err := dst.CreateSession(store.Session{
		ID:        "sess-force",
		Goal:      "pre-existing",
		Worker:    "claude",
		Status:    "running",
		CreatedAt: time.Unix(1_700_003_000, 0).UTC(),
	}); err != nil {
		t.Fatalf("seed dst: %v", err)
	}

	res, err := Restore(dst, t.TempDir(), exp, ImportOptions{Force: true})
	if err != nil {
		t.Fatalf("Restore with Force: %v", err)
	}
	if len(res.SessionsReplaced) != 1 || res.SessionsReplaced[0] != "sess-force" {
		t.Fatalf("SessionsReplaced: got %v", res.SessionsReplaced)
	}
	got, err := dst.GetSession("sess-force")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.Goal != "g-sess-force" {
		t.Fatalf("Goal should be from import, got %q", got.Goal)
	}
}

func TestRestore_MissingBlobRefSurfacedInResult(t *testing.T) {
	src := newTestStore(t)
	srcBlobs := t.TempDir()
	// Only create the prompt blob; raw_output_ref will be exported with empty
	// content and Restore should mark it as missing rather than fail.
	promptPath, _, _ := seedSession(t, src, srcBlobs, "sess-miss", nil)
	if err := os.WriteFile(promptPath, []byte("p"), 0o644); err != nil {
		t.Fatalf("write prompt blob: %v", err)
	}

	exp := runExport(t, src, srcBlobs, Options{SessionID: "sess-miss", IncludeBlobs: true})

	dst := newTestStore(t)
	dstBlobs := filepath.Join(t.TempDir(), "blobs")
	res, err := Restore(dst, dstBlobs, exp, ImportOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if len(res.MissingBlobRefs) == 0 {
		t.Fatal("expected at least one missing blob ref in result")
	}
	missingBase := "sess-miss-raw.txt"
	found := false
	for _, b := range res.MissingBlobRefs {
		if b == missingBase {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("MissingBlobRefs missing %q: got %v", missingBase, res.MissingBlobRefs)
	}
}

func TestRestore_BadBase64BlobErrors(t *testing.T) {
	exp := &Export{
		Header: Header{FormatVersion: FormatVersion},
		Blobs:  map[string]string{"junk.txt": "!!!not-base64!!!"},
	}
	dst := newTestStore(t)
	_, err := Restore(dst, t.TempDir(), exp, ImportOptions{SkipHashCheck: true})
	if err == nil {
		t.Fatal("expected base64 decode error")
	}
	if !strings.Contains(err.Error(), "decode blob") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRestore_TransactionRollbackOnInsertFailure builds an export that contains
// two sessions sharing the same id. The first insert succeeds inside the
// transaction; the second collides on the PRIMARY KEY and forces a rollback.
// After the call returns, the destination store must contain no rows from the
// failed import.
func TestRestore_TransactionRollbackOnInsertFailure(t *testing.T) {
	created := time.Unix(1_700_004_000, 0).UTC()
	dupSess := SessionRow{
		ID:        "dup-id",
		Goal:      "first",
		Worker:    "claude",
		Status:    "running",
		CreatedAt: created,
	}
	exp := &Export{
		Header: Header{FormatVersion: FormatVersion},
		Sessions: []SessionRow{
			dupSess,
			{
				ID:        "dup-id", // intentional collision
				Goal:      "second",
				Worker:    "claude",
				Status:    "running",
				CreatedAt: created,
			},
		},
		Events: []EventRow{
			{
				SessionID: "dup-id",
				Seq:       1,
				Ts:        created,
				Kind:      "thought",
				Payload:   json.RawMessage(`{"ok":true}`),
			},
		},
	}

	dst := newTestStore(t)
	_, err := Restore(dst, t.TempDir(), exp, ImportOptions{SkipHashCheck: true})
	if err == nil {
		t.Fatal("expected error from duplicate session id")
	}
	if !strings.Contains(err.Error(), "insert session") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Rollback verification: the first session's insert succeeded inside the
	// transaction but must have been rolled back. GetSession should report
	// sql.ErrNoRows and no events should remain.
	if _, err := dst.GetSession("dup-id"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("session should not exist after rollback, got err=%v", err)
	}
	events, err := dst.ListEvents("dup-id", 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events should be empty after rollback, got %d", len(events))
	}
}

func TestRestore_HashVerificationOnUnmodifiedExport(t *testing.T) {
	// Sanity check: a freshly-produced export should pass hash verification
	// without SkipHashCheck.
	src := newTestStore(t)
	srcBlobs := t.TempDir()
	seedSession(t, src, srcBlobs, "sess-ok", nil)
	exp := runExport(t, src, srcBlobs, Options{SessionID: "sess-ok"})

	// Confirm the body hash recomputed here matches the recorded hash —
	// guards against accidental changes in the canonicalization shape.
	want := exp.Header.BodySHA256
	exp.Header.BodySHA256 = ""
	canonical, err := json.Marshal(exp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sum := sha256.Sum256(canonical)
	got := hex.EncodeToString(sum[:])
	exp.Header.BodySHA256 = want
	if got != want {
		t.Fatalf("canonicalization changed: got %s want %s", got, want)
	}

	dst := newTestStore(t)
	if _, err := Restore(dst, t.TempDir(), exp, ImportOptions{}); err != nil {
		t.Fatalf("Restore on unmodified export: %v", err)
	}
}

func TestRestore_EmptyBlobStringSkipped(t *testing.T) {
	// Blobs map entries with empty string values represent "referenced but
	// missing on the source side" — Restore should skip them silently rather
	// than counting them as imported.
	exp := &Export{
		Header: Header{FormatVersion: FormatVersion},
		Blobs:  map[string]string{"placeholder.txt": ""},
	}
	dst := newTestStore(t)
	dstBlobs := filepath.Join(t.TempDir(), "blobs")
	res, err := Restore(dst, dstBlobs, exp, ImportOptions{SkipHashCheck: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.BlobsImported != 0 {
		t.Fatalf("BlobsImported: got %d want 0", res.BlobsImported)
	}
	if _, err := os.Stat(filepath.Join(dstBlobs, "placeholder.txt")); !os.IsNotExist(err) {
		t.Fatalf("placeholder blob should not be written, stat err=%v", err)
	}
}

func TestRestore_BlobRefRewriting_WithoutBlobsMap(t *testing.T) {
	// When the export omits Blobs (e.g. --no-blobs at export time) the row
	// inserts should still translate "blob:<base>" references to absolute
	// paths under dstBlobs, and each unmatched basename should appear in
	// MissingBlobRefs.
	created := time.Unix(1_700_005_000, 0).UTC()
	exp := &Export{
		Header: Header{FormatVersion: FormatVersion},
		Sessions: []SessionRow{
			{
				ID:             "sess-noblob",
				Goal:           "no blobs",
				Worker:         "claude",
				Status:         "running",
				CreatedAt:      created,
				FinalAnswerRef: "blob:final.txt",
			},
		},
		Subtasks: []SubtaskRow{
			{
				ID:        "sess-noblob-st0",
				SessionID: "sess-noblob",
				Ord:       0,
				Title:     "t",
				PromptRef: "blob:prompt.txt",
				Worker:    "claude",
				Status:    "pending",
			},
		},
	}

	dst := newTestStore(t)
	dstBlobs := filepath.Join(t.TempDir(), "blobs")
	res, err := Restore(dst, dstBlobs, exp, ImportOptions{SkipHashCheck: true})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// Both refs should be flagged as missing since no blob bodies were shipped.
	missing := map[string]bool{}
	for _, b := range res.MissingBlobRefs {
		missing[b] = true
	}
	if !missing["final.txt"] || !missing["prompt.txt"] {
		t.Fatalf("MissingBlobRefs should include final.txt and prompt.txt, got %v", res.MissingBlobRefs)
	}

	gotSess, err := dst.GetSession("sess-noblob")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if gotSess.FinalAnswerRef != filepath.Join(dstBlobs, "final.txt") {
		t.Fatalf("FinalAnswerRef: got %q", gotSess.FinalAnswerRef)
	}
	subs, err := dst.SubtaskListBySession("sess-noblob", 0, 0)
	if err != nil {
		t.Fatalf("SubtaskListBySession: %v", err)
	}
	if len(subs) != 1 || subs[0].PromptRef != filepath.Join(dstBlobs, "prompt.txt") {
		t.Fatalf("subtask PromptRef rewrite: got %+v", subs)
	}
}
