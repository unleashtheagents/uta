// Package export reads uta's SQLite database (and the blob store alongside
// it) into a single self-describing JSON value, and reverses the operation.
//
// Export shape:
//
//	{
//	  "header": {
//	    "uta_version": "...", "schema_version": "...",
//	    "exported_at": "...", "scope": "session" | "all",
//	    "session_ids": [...], "row_counts": {...},
//	    "body_sha256": "..."     // hash of the canonical body (excluding this field)
//	  },
//	  "sessions": [...],
//	  "subtasks": [...],
//	  "events": [...],
//	  "blobs": { "<basename>": "<base64>", ... }    // omitted when --no-blobs
//	}
//
// Blob references in rows are canonicalized to "blob:<basename>" so the export
// is portable across machines.
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
	"time"

	"github.com/unleashtheagents/uta/internal/store"
	"github.com/unleashtheagents/uta/internal/version"
)

// FormatVersion is bumped if Export's on-disk shape changes incompatibly.
const FormatVersion = 1

// Header is the export's self-describing envelope.
type Header struct {
	FormatVersion int            `json:"format_version"`
	UtaVersion    string         `json:"uta_version"`
	SchemaVersion string         `json:"schema_version"`
	ExportedAt    time.Time      `json:"exported_at"`
	Scope         string         `json:"scope"`
	SessionIDs    []string       `json:"session_ids"`
	IncludeBlobs  bool           `json:"include_blobs"`
	RowCounts     map[string]int `json:"row_counts"`
	BodySHA256    string         `json:"body_sha256,omitempty"`
}

// Export is the root of the JSON document.
type Export struct {
	Header   Header           `json:"header"`
	Sessions []SessionRow     `json:"sessions"`
	Subtasks []SubtaskRow     `json:"subtasks"`
	Events   []EventRow       `json:"events"`
	Blobs    map[string]string `json:"blobs,omitempty"`
}

// SessionRow is the serializable session record.
type SessionRow struct {
	ID             string     `json:"id"`
	Goal           string     `json:"goal"`
	Worker         string     `json:"worker"`
	Planner        string     `json:"planner,omitempty"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	FinalAnswerRef string     `json:"final_answer_ref,omitempty"`
	WorkflowPath   string     `json:"workflow_path,omitempty"`
	MetaJSON       string     `json:"meta_json,omitempty"`
}

// SubtaskRow is the serializable subtask record.
type SubtaskRow struct {
	ID                string     `json:"id"`
	SessionID         string     `json:"session_id"`
	Ord               int        `json:"ord"`
	SpecID            string     `json:"spec_id,omitempty"`
	Title             string     `json:"title"`
	PromptRef         string     `json:"prompt_ref,omitempty"`
	Worker            string     `json:"worker"`
	ProviderSessionID string     `json:"provider_session_id,omitempty"`
	Status            string     `json:"status"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	ResultText        string     `json:"result_text,omitempty"`
	RawOutputRef      string     `json:"raw_output_ref,omitempty"`
	Error             string     `json:"error,omitempty"`
	ErrorKind         string     `json:"error_kind,omitempty"`
	MetaJSON          string     `json:"meta_json,omitempty"`
}

// EventRow is the serializable trajectory event record.
type EventRow struct {
	SessionID string          `json:"session_id"`
	SubtaskID string          `json:"subtask_id,omitempty"`
	Seq       int64           `json:"seq"`
	Ts        time.Time       `json:"ts"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
}

// Options controls Run.
type Options struct {
	// SessionID, if non-empty, restricts the export to that session.
	SessionID string
	// All=true exports every session in the store. Mutually exclusive with SessionID.
	All bool
	// IncludeBlobs=true reads referenced blob files and inlines them as base64.
	IncludeBlobs bool
}

// Run produces an Export from a live store. The caller is responsible for
// JSON-marshaling the result and writing it wherever they want.
func Run(s *store.Store, blobsDir string, opts Options) (*Export, error) {
	if opts.SessionID == "" && !opts.All {
		return nil, errors.New("export: either SessionID or All must be set")
	}
	if opts.SessionID != "" && opts.All {
		return nil, errors.New("export: SessionID and All are mutually exclusive")
	}

	schemaHead, err := store.SchemaHead(s.DB)
	if err != nil {
		return nil, fmt.Errorf("read schema head: %w", err)
	}

	var sessionIDs []string
	if opts.SessionID != "" {
		// Verify it exists; surface a clean error if not.
		if _, err := s.GetSession(opts.SessionID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("session not found: %s", opts.SessionID)
			}
			return nil, err
		}
		sessionIDs = []string{opts.SessionID}
	} else {
		rows, err := s.ListSessions(0, 0, "")
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			sessionIDs = append(sessionIDs, r.ID)
		}
	}

	// Track basename → original-path mapping so we can read blob contents once.
	blobPaths := map[string]string{}
	refToBlob := func(ref string) string {
		if ref == "" {
			return ""
		}
		base := filepath.Base(ref)
		blobPaths[base] = ref
		return "blob:" + base
	}

	out := &Export{}
	for _, sid := range sessionIDs {
		sess, err := s.GetSession(sid)
		if err != nil {
			return nil, fmt.Errorf("read session %s: %w", sid, err)
		}
		out.Sessions = append(out.Sessions, SessionRow{
			ID:             sess.ID,
			Goal:           sess.Goal,
			Worker:         sess.Worker,
			Planner:        sess.Planner,
			Status:         sess.Status,
			CreatedAt:      sess.CreatedAt,
			CompletedAt:    sess.CompletedAt,
			FinalAnswerRef: refToBlob(sess.FinalAnswerRef),
			WorkflowPath:   sess.WorkflowPath,
			MetaJSON:       sess.MetaJSON,
		})

		subs, err := s.SubtaskListBySession(sid, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("read subtasks for %s: %w", sid, err)
		}
		for _, sub := range subs {
			out.Subtasks = append(out.Subtasks, SubtaskRow{
				ID:                sub.ID,
				SessionID:         sub.SessionID,
				Ord:               sub.Ord,
				SpecID:            sub.SpecID,
				Title:             sub.Title,
				PromptRef:         refToBlob(sub.PromptRef),
				Worker:            sub.Worker,
				ProviderSessionID: sub.ProviderSessionID,
				Status:            sub.Status,
				StartedAt:         sub.StartedAt,
				CompletedAt:       sub.CompletedAt,
				ResultText:        sub.ResultText,
				RawOutputRef:      refToBlob(sub.RawOutputRef),
				Error:             sub.Error,
				ErrorKind:         sub.ErrorKind,
				MetaJSON:          sub.MetaJSON,
			})
		}

		events, err := s.ListEvents(sid, 0, 0)
		if err != nil {
			return nil, fmt.Errorf("read events for %s: %w", sid, err)
		}
		for _, ev := range events {
			out.Events = append(out.Events, EventRow{
				SessionID: ev.SessionID,
				SubtaskID: ev.SubtaskID,
				Seq:       ev.Seq,
				Ts:        ev.Ts,
				Kind:      ev.Kind,
				Payload:   ev.Payload,
			})
		}
	}

	if opts.IncludeBlobs {
		out.Blobs = make(map[string]string, len(blobPaths))
		for base, path := range blobPaths {
			data, err := os.ReadFile(path)
			if err != nil {
				if os.IsNotExist(err) {
					// Blob referenced by a row but missing on disk — record as empty
					// rather than fail; importer will surface this in its report.
					out.Blobs[base] = ""
					continue
				}
				return nil, fmt.Errorf("read blob %s: %w", path, err)
			}
			out.Blobs[base] = base64.StdEncoding.EncodeToString(data)
		}
	}

	scope := "session"
	if opts.All {
		scope = "all"
	}
	out.Header = Header{
		FormatVersion: FormatVersion,
		UtaVersion:    version.Version,
		SchemaVersion: schemaHead,
		ExportedAt:    time.Now().UTC(),
		Scope:         scope,
		SessionIDs:    sessionIDs,
		IncludeBlobs:  opts.IncludeBlobs,
		RowCounts: map[string]int{
			"sessions": len(out.Sessions),
			"subtasks": len(out.Subtasks),
			"events":   len(out.Events),
			"blobs":    len(out.Blobs),
		},
	}

	// Compute body hash (everything except header.body_sha256) so importers can
	// verify integrity. Marshaling with BodySHA256 empty gives us the canonical
	// pre-hash representation.
	out.Header.BodySHA256 = ""
	canonical, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("canonicalize: %w", err)
	}
	sum := sha256.Sum256(canonical)
	out.Header.BodySHA256 = hex.EncodeToString(sum[:])

	return out, nil
}
