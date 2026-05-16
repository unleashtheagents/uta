package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// Session is one orchestration run.
type Session struct {
	ID             string
	Goal           string
	Worker         string
	Planner        string
	Status         string
	CreatedAt      time.Time
	CompletedAt    *time.Time
	FinalAnswerRef string
	WorkflowPath   string
	MetaJSON       string
}

// Subtask is one decomposed unit dispatched to a provider.
type Subtask struct {
	ID                string
	SessionID         string
	Ord               int
	Title             string
	PromptRef         string
	Worker            string
	ProviderSessionID string
	Status            string
	StartedAt         *time.Time
	CompletedAt       *time.Time
	ResultText        string
	RawOutputRef      string
	Error             string
	ErrorKind         string
}

// CreateSession persists a freshly-started run row.
func (s *Store) CreateSession(sess Session) error {
	_, err := s.DB.Exec(
		`INSERT INTO sessions (id, goal, worker, planner, status, created_at, workflow_path, meta_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.Goal, sess.Worker, nullable(sess.Planner), sess.Status,
		sess.CreatedAt.UnixNano(), nullable(sess.WorkflowPath), nonEmpty(sess.MetaJSON, "{}"),
	)
	return err
}

// MarkSession sets status, completed_at, and final answer ref atomically.
func (s *Store) MarkSession(id, status, finalAnswerRef string) error {
	_, err := s.DB.Exec(
		`UPDATE sessions SET status = ?, completed_at = ?, final_answer_ref = ? WHERE id = ?`,
		status, time.Now().UnixNano(), nullable(finalAnswerRef), id,
	)
	return err
}

// CreateSubtask inserts a pending subtask row.
func (s *Store) CreateSubtask(t Subtask) error {
	_, err := s.DB.Exec(
		`INSERT INTO subtasks (id, session_id, ord, title, prompt_ref, worker, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.SessionID, t.Ord, t.Title, t.PromptRef, t.Worker, t.Status,
	)
	return err
}

// UpdateSubtask applies the terminal-state fields after a subtask finishes.
func (s *Store) UpdateSubtask(t Subtask) error {
	var started, completed any
	if t.StartedAt != nil {
		started = t.StartedAt.UnixNano()
	}
	if t.CompletedAt != nil {
		completed = t.CompletedAt.UnixNano()
	}
	_, err := s.DB.Exec(
		`UPDATE subtasks SET provider_session_id = ?, status = ?, started_at = ?, completed_at = ?,
		 result_text = ?, raw_output_ref = ?, error = ?, error_kind = ? WHERE id = ?`,
		nullable(t.ProviderSessionID), t.Status, started, completed,
		nullable(t.ResultText), nullable(t.RawOutputRef), nullable(t.Error), nullable(t.ErrorKind),
		t.ID,
	)
	return err
}

// InsertEvent persists one trajectory event. The seq value must be supplied
// by the caller (the recorder maintains a monotonic counter per session).
func (s *Store) InsertEvent(sessionID, subtaskID string, seq int64, ts time.Time, kind string, payload json.RawMessage) error {
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	_, err := s.DB.Exec(
		`INSERT INTO trajectory_events (session_id, subtask_id, seq, ts, kind, payload_json)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, nullable(subtaskID), seq, ts.UnixNano(), kind, string(payload),
	)
	return err
}

// ListSessions returns recent sessions newest-first, capped at limit (default 50).
func (s *Store) ListSessions(limit int, statusFilter string) ([]Session, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, goal, worker, COALESCE(planner, ''), status, created_at, completed_at, COALESCE(final_answer_ref, ''), COALESCE(workflow_path, ''), meta_json FROM sessions`
	args := []any{}
	if statusFilter != "" {
		q += ` WHERE status = ?`
		args = append(args, statusFilter)
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		var createdNs int64
		var completedNs sql.NullInt64
		if err := rows.Scan(&sess.ID, &sess.Goal, &sess.Worker, &sess.Planner, &sess.Status,
			&createdNs, &completedNs, &sess.FinalAnswerRef, &sess.WorkflowPath, &sess.MetaJSON); err != nil {
			return nil, err
		}
		sess.CreatedAt = time.Unix(0, createdNs)
		if completedNs.Valid {
			t := time.Unix(0, completedNs.Int64)
			sess.CompletedAt = &t
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// GetSession returns one session by id, or sql.ErrNoRows.
func (s *Store) GetSession(id string) (Session, error) {
	var sess Session
	var createdNs int64
	var completedNs sql.NullInt64
	err := s.DB.QueryRow(
		`SELECT id, goal, worker, COALESCE(planner, ''), status, created_at, completed_at, COALESCE(final_answer_ref, ''), COALESCE(workflow_path, ''), meta_json FROM sessions WHERE id = ?`, id,
	).Scan(&sess.ID, &sess.Goal, &sess.Worker, &sess.Planner, &sess.Status,
		&createdNs, &completedNs, &sess.FinalAnswerRef, &sess.WorkflowPath, &sess.MetaJSON)
	if err != nil {
		return sess, err
	}
	sess.CreatedAt = time.Unix(0, createdNs)
	if completedNs.Valid {
		t := time.Unix(0, completedNs.Int64)
		sess.CompletedAt = &t
	}
	return sess, nil
}

// EventRow is one row from trajectory_events.
type EventRow struct {
	ID        int64
	SessionID string
	SubtaskID string
	Seq       int64
	Ts        time.Time
	Kind      string
	Payload   json.RawMessage
}

// ListEvents returns every event for a session in seq order.
func (s *Store) ListEvents(sessionID string) ([]EventRow, error) {
	rows, err := s.DB.Query(
		`SELECT id, session_id, COALESCE(subtask_id, ''), seq, ts, kind, payload_json FROM trajectory_events WHERE session_id = ? ORDER BY seq ASC`, sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var ev EventRow
		var tsNs int64
		var payload string
		if err := rows.Scan(&ev.ID, &ev.SessionID, &ev.SubtaskID, &ev.Seq, &tsNs, &ev.Kind, &payload); err != nil {
			return nil, err
		}
		ev.Ts = time.Unix(0, tsNs)
		ev.Payload = json.RawMessage(payload)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// LastSubtask returns the highest-ord subtask for a session — used by resume
// to recover the provider's session id.
func (s *Store) LastSubtask(sessionID string) (Subtask, error) {
	var t Subtask
	var started, completed sql.NullInt64
	err := s.DB.QueryRow(
		`SELECT id, session_id, ord, title, prompt_ref, worker, COALESCE(provider_session_id, ''), status, started_at, completed_at, COALESCE(result_text, ''), COALESCE(raw_output_ref, ''), COALESCE(error, ''), COALESCE(error_kind, '') FROM subtasks WHERE session_id = ? ORDER BY ord DESC LIMIT 1`, sessionID,
	).Scan(&t.ID, &t.SessionID, &t.Ord, &t.Title, &t.PromptRef, &t.Worker, &t.ProviderSessionID, &t.Status,
		&started, &completed, &t.ResultText, &t.RawOutputRef, &t.Error, &t.ErrorKind)
	if err != nil {
		return t, err
	}
	if started.Valid {
		v := time.Unix(0, started.Int64)
		t.StartedAt = &v
	}
	if completed.Valid {
		v := time.Unix(0, completed.Int64)
		t.CompletedAt = &v
	}
	return t, nil
}

// RecordProvidersSeen upserts current detection results so `uta doctor` and
// `uta providers --refresh` keep a persistent snapshot.
func (s *Store) RecordProvidersSeen(name, binaryPath, version string, capabilities []string, notes string) error {
	_, err := s.DB.Exec(
		`INSERT INTO providers_seen (name, binary_path, version, capabilities, last_detected, notes)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET binary_path=excluded.binary_path, version=excluded.version,
		   capabilities=excluded.capabilities, last_detected=excluded.last_detected, notes=excluded.notes`,
		name, nullable(binaryPath), nullable(version), strings.Join(capabilities, ","), time.Now().UnixNano(), nullable(notes),
	)
	return err
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// SubtaskListBySession returns subtasks ordered by ord.
func (s *Store) SubtaskListBySession(sessionID string) ([]Subtask, error) {
	rows, err := s.DB.Query(
		`SELECT id, session_id, ord, title, prompt_ref, worker, COALESCE(provider_session_id, ''), status, started_at, completed_at, COALESCE(result_text, ''), COALESCE(raw_output_ref, ''), COALESCE(error, ''), COALESCE(error_kind, '') FROM subtasks WHERE session_id = ? ORDER BY ord ASC`, sessionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subtask
	for rows.Next() {
		var t Subtask
		var started, completed sql.NullInt64
		if err := rows.Scan(&t.ID, &t.SessionID, &t.Ord, &t.Title, &t.PromptRef, &t.Worker,
			&t.ProviderSessionID, &t.Status, &started, &completed,
			&t.ResultText, &t.RawOutputRef, &t.Error, &t.ErrorKind); err != nil {
			return nil, err
		}
		if started.Valid {
			v := time.Unix(0, started.Int64)
			t.StartedAt = &v
		}
		if completed.Valid {
			v := time.Unix(0, completed.Int64)
			t.CompletedAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

