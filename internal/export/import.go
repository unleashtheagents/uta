package export

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/unleashtheagents/uta/internal/store"
)

// ImportOptions configures Restore.
type ImportOptions struct {
	// Force replaces a session that already exists (and its subtasks + events)
	// before inserting the imported one. Without Force, a collision is an error.
	Force bool
	// SkipHashCheck disables integrity verification. Default false.
	SkipHashCheck bool
}

// ImportResult is what Restore returns on success — useful for the CLI's
// success message.
type ImportResult struct {
	SessionsImported  int
	SubtasksImported  int
	EventsImported    int
	BlobsImported     int
	SessionsReplaced  []string // populated when Force=true and a session existed
	MissingBlobRefs   []string // basenames referenced by rows but absent from the export
}

// Restore reads an Export and writes its contents into the destination store
// and blob dir. Blob references in rows ("blob:<basename>") are rewritten to
// absolute paths under blobsDir before the row is inserted.
func Restore(s *store.Store, blobsDir string, exp *Export, opts ImportOptions) (*ImportResult, error) {
	if exp == nil {
		return nil, errors.New("import: nil export")
	}
	if exp.Header.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("import: unsupported format_version %d (this binary handles %d)",
			exp.Header.FormatVersion, FormatVersion)
	}

	if !opts.SkipHashCheck && exp.Header.BodySHA256 != "" {
		if err := verifyBodyHash(exp); err != nil {
			return nil, err
		}
	}

	res := &ImportResult{}

	// 1) Materialize blobs first so row refs can be rewritten to real paths.
	blobBaseToPath := map[string]string{}
	if len(exp.Blobs) > 0 {
		if err := os.MkdirAll(blobsDir, 0o755); err != nil {
			return nil, fmt.Errorf("create blobs dir: %w", err)
		}
		for basename, b64 := range exp.Blobs {
			if b64 == "" {
				continue // exporter recorded a missing blob
			}
			data, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				return nil, fmt.Errorf("decode blob %s: %w", basename, err)
			}
			dest := filepath.Join(blobsDir, basename)
			if err := os.WriteFile(dest, data, 0o644); err != nil {
				return nil, fmt.Errorf("write blob %s: %w", basename, err)
			}
			blobBaseToPath[basename] = dest
			res.BlobsImported++
		}
	}

	rewrite := func(ref string) string {
		if !strings.HasPrefix(ref, "blob:") {
			return ref
		}
		base := strings.TrimPrefix(ref, "blob:")
		if dest, ok := blobBaseToPath[base]; ok {
			return dest
		}
		// blob ref but no matching content — keep the basename so users can
		// trace it; mark it for the result report.
		res.MissingBlobRefs = append(res.MissingBlobRefs, base)
		return filepath.Join(blobsDir, base)
	}

	// 2) Handle conflicts by deleting first (Force) or erring out.
	existing := map[string]bool{}
	for _, sess := range exp.Sessions {
		_, err := s.GetSession(sess.ID)
		switch {
		case err == nil:
			existing[sess.ID] = true
		case errors.Is(err, sql.ErrNoRows):
			// fine
		default:
			return nil, fmt.Errorf("probe session %s: %w", sess.ID, err)
		}
	}
	if len(existing) > 0 && !opts.Force {
		ids := make([]string, 0, len(existing))
		for id := range existing {
			ids = append(ids, id)
		}
		return nil, fmt.Errorf("import would clobber %d existing session(s): %s (pass --force to replace)",
			len(ids), strings.Join(ids, ", "))
	}
	for id := range existing {
		if _, err := s.DB.Exec(`DELETE FROM sessions WHERE id = ?`, id); err != nil {
			return nil, fmt.Errorf("delete existing session %s: %w", id, err)
		}
		// trajectory_events and subtasks cascade via ON DELETE CASCADE.
		res.SessionsReplaced = append(res.SessionsReplaced, id)
	}

	// 3) Insert rows inside a single transaction.
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, sess := range exp.Sessions {
		_, err := tx.Exec(
			`INSERT INTO sessions (id, goal, worker, planner, status, created_at, completed_at, final_answer_ref, workflow_path, meta_json)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sess.ID, sess.Goal, sess.Worker, nullableStr(sess.Planner), sess.Status,
			sess.CreatedAt.UnixNano(), nullableTime(sess.CompletedAt),
			nullableStr(rewrite(sess.FinalAnswerRef)), nullableStr(sess.WorkflowPath),
			defaultStr(sess.MetaJSON, "{}"),
		)
		if err != nil {
			return nil, fmt.Errorf("insert session %s: %w", sess.ID, err)
		}
		res.SessionsImported++
	}

	for _, sub := range exp.Subtasks {
		_, err := tx.Exec(
			`INSERT INTO subtasks (id, session_id, ord, spec_id, title, prompt_ref, worker, provider_session_id, status, started_at, completed_at, result_text, raw_output_ref, error, error_kind, meta_json)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sub.ID, sub.SessionID, sub.Ord, nullableStr(sub.SpecID), sub.Title, rewrite(sub.PromptRef),
			sub.Worker, nullableStr(sub.ProviderSessionID), sub.Status,
			nullableTime(sub.StartedAt), nullableTime(sub.CompletedAt),
			nullableStr(sub.ResultText), nullableStr(rewrite(sub.RawOutputRef)),
			nullableStr(sub.Error), nullableStr(sub.ErrorKind),
			defaultStr(sub.MetaJSON, "{}"),
		)
		if err != nil {
			return nil, fmt.Errorf("insert subtask %s: %w", sub.ID, err)
		}
		res.SubtasksImported++
	}

	for _, ev := range exp.Events {
		payload := ev.Payload
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}
		_, err := tx.Exec(
			`INSERT INTO trajectory_events (session_id, subtask_id, seq, ts, kind, payload_json)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			ev.SessionID, nullableStr(ev.SubtaskID), ev.Seq, ev.Ts.UnixNano(), ev.Kind, string(payload),
		)
		if err != nil {
			return nil, fmt.Errorf("insert event seq=%d: %w", ev.Seq, err)
		}
		res.EventsImported++
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return res, nil
}

// verifyBodyHash re-computes the canonical hash with body_sha256 cleared and
// compares it to the header's recorded value.
func verifyBodyHash(exp *Export) error {
	want := exp.Header.BodySHA256
	exp.Header.BodySHA256 = ""
	canonical, err := json.Marshal(exp)
	exp.Header.BodySHA256 = want
	if err != nil {
		return fmt.Errorf("rehash: %w", err)
	}
	sum := sha256.Sum256(canonical)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return fmt.Errorf("import: body sha256 mismatch (got %s, expected %s)", got[:16], want[:16])
	}
	return nil
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

func defaultStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
