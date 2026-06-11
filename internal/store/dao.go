package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	// ModeName is the name of the active MissionProfile when the session
	// was created. Empty when the run had no profile attached. Surfaced by
	// `uta mode list` (last_used_at + totals) and `uta dash`.
	ModeName string
}

// Subtask is one decomposed unit dispatched to a provider.
type Subtask struct {
	ID        string
	SessionID string
	Ord       int
	// SpecID is the planner-assigned identifier (e.g. "s1") for plan-derived
	// subtasks. Empty for rows the engine creates outside a Plan (resume
	// turns, reflector critics/tools/reviser).
	SpecID            string
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
	MetaJSON          string
}

// CreateSession persists a freshly-started run row.
func (s *Store) CreateSession(sess Session) error {
	_, err := s.DB.Exec(
		`INSERT INTO sessions (id, goal, worker, planner, status, created_at, workflow_path, meta_json, mode_name)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.Goal, sess.Worker, nullable(sess.Planner), sess.Status,
		sess.CreatedAt.UnixNano(), nullable(sess.WorkflowPath), nonEmpty(sess.MetaJSON, "{}"),
		nullable(sess.ModeName),
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
		`INSERT INTO subtasks (id, session_id, ord, spec_id, title, prompt_ref, worker, status, meta_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.SessionID, t.Ord, nullable(t.SpecID), t.Title, t.PromptRef, t.Worker, t.Status,
		nonEmpty(t.MetaJSON, "{}"),
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
		 result_text = ?, raw_output_ref = ?, error = ?, error_kind = ?, meta_json = ? WHERE id = ?`,
		nullable(t.ProviderSessionID), t.Status, started, completed,
		nullable(t.ResultText), nullable(t.RawOutputRef), nullable(t.Error), nullable(t.ErrorKind),
		nonEmpty(t.MetaJSON, "{}"),
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

// ListSessions returns sessions newest-first. limit<=0 means no row cap;
// offset<=0 means start at the first row. Callers that want a default
// page size (e.g. the CLI) supply it themselves.
func (s *Store) ListSessions(limit, offset int, statusFilter string) ([]Session, error) {
	q := `SELECT id, goal, worker, COALESCE(planner, ''), status, created_at, completed_at, COALESCE(final_answer_ref, ''), COALESCE(workflow_path, ''), meta_json, COALESCE(mode_name, '') FROM sessions`
	args := []any{}
	if statusFilter != "" {
		q += ` WHERE status = ?`
		args = append(args, statusFilter)
	}
	q += ` ORDER BY created_at DESC`
	pageSQL, pageArgs := paginationSQL(limit, offset)
	q += pageSQL
	args = append(args, pageArgs...)

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
			&createdNs, &completedNs, &sess.FinalAnswerRef, &sess.WorkflowPath, &sess.MetaJSON, &sess.ModeName); err != nil {
			return nil, err
		}
		sess.CreatedAt = time.Unix(0, createdNs)
		sess.CompletedAt = nullTime(completedNs)
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
		`SELECT id, goal, worker, COALESCE(planner, ''), status, created_at, completed_at, COALESCE(final_answer_ref, ''), COALESCE(workflow_path, ''), meta_json, COALESCE(mode_name, '') FROM sessions WHERE id = ?`, id,
	).Scan(&sess.ID, &sess.Goal, &sess.Worker, &sess.Planner, &sess.Status,
		&createdNs, &completedNs, &sess.FinalAnswerRef, &sess.WorkflowPath, &sess.MetaJSON, &sess.ModeName)
	if err != nil {
		return sess, err
	}
	sess.CreatedAt = time.Unix(0, createdNs)
	sess.CompletedAt = nullTime(completedNs)
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

// ListEvents returns events for a session in seq order. limit<=0 means no
// row cap; offset<=0 means start at the first row.
func (s *Store) ListEvents(sessionID string, limit, offset int) ([]EventRow, error) {
	q := `SELECT id, session_id, COALESCE(subtask_id, ''), seq, ts, kind, payload_json FROM trajectory_events WHERE session_id = ? ORDER BY seq ASC`
	args := []any{sessionID}
	pageSQL, pageArgs := paginationSQL(limit, offset)
	q += pageSQL
	args = append(args, pageArgs...)
	rows, err := s.DB.Query(q, args...)
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
		`SELECT id, session_id, ord, COALESCE(spec_id, ''), title, prompt_ref, worker, COALESCE(provider_session_id, ''), status, started_at, completed_at, COALESCE(result_text, ''), COALESCE(raw_output_ref, ''), COALESCE(error, ''), COALESCE(error_kind, ''), meta_json FROM subtasks WHERE session_id = ? ORDER BY ord DESC LIMIT 1`, sessionID,
	).Scan(&t.ID, &t.SessionID, &t.Ord, &t.SpecID, &t.Title, &t.PromptRef, &t.Worker, &t.ProviderSessionID, &t.Status,
		&started, &completed, &t.ResultText, &t.RawOutputRef, &t.Error, &t.ErrorKind, &t.MetaJSON)
	if err != nil {
		return t, err
	}
	t.StartedAt = nullTime(started)
	t.CompletedAt = nullTime(completed)
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

// nullTime converts a nullable nanosecond timestamp into a *time.Time,
// returning nil when the column was NULL.
func nullTime(ns sql.NullInt64) *time.Time {
	if !ns.Valid {
		return nil
	}
	t := time.Unix(0, ns.Int64)
	return &t
}

// paginationSQL builds the trailing " LIMIT ?[ OFFSET ?]" fragment and the
// matching positional args. SQLite treats LIMIT -1 as no row cap, which lets
// us express "offset only, unbounded" without conditional joining.
func paginationSQL(limit, offset int) (string, []any) {
	switch {
	case limit > 0 && offset > 0:
		return " LIMIT ? OFFSET ?", []any{limit, offset}
	case limit > 0:
		return " LIMIT ?", []any{limit}
	case offset > 0:
		return " LIMIT -1 OFFSET ?", []any{offset}
	default:
		return "", nil
	}
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// ModeStat is an aggregated usage summary for one MissionProfile.
// LastUsedAt is the latest sessions.created_at where mode_name matched.
// TotalSessions counts every session ever run under the mode. SubtaskSeconds
// sums (completed_at - started_at) across every subtask of those sessions;
// subtasks missing either timestamp contribute zero. RecentSessions are the
// most recent session IDs (newest-first), capped by the caller-supplied
// limit when querying. SentinelAlerts24h counts sentinel_alert trajectory
// events for the mode's sessions in the last 24h.
type ModeStat struct {
	ModeName          string
	LastUsedAt        *time.Time
	TotalSessions     int
	SubtaskSeconds    float64
	RecentSessions    []Session
	SentinelAlerts24h int
}

// ModeStats returns aggregated stats for every distinct mode_name seen on
// sessions plus an "" entry for sessions that ran without a profile. Each
// stat carries up to recentLimit newest sessions for that mode.
func (s *Store) ModeStats(recentLimit int) (map[string]*ModeStat, error) {
	if recentLimit <= 0 {
		recentLimit = 5
	}
	out := map[string]*ModeStat{}

	rows, err := s.DB.Query(
		`SELECT COALESCE(mode_name, '') AS mn,
		        COUNT(*) AS n,
		        MAX(created_at) AS last_used
		   FROM sessions
		  GROUP BY mn`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var mn string
		var n int
		var lastNs sql.NullInt64
		if err := rows.Scan(&mn, &n, &lastNs); err != nil {
			return nil, err
		}
		ms := &ModeStat{ModeName: mn, TotalSessions: n}
		if lastNs.Valid {
			t := time.Unix(0, lastNs.Int64)
			ms.LastUsedAt = &t
		}
		out[mn] = ms
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Subtask wall-clock seconds per mode. completed_at IS NOT NULL means
	// the subtask terminated; we ignore in-flight rows.
	stRows, err := s.DB.Query(
		`SELECT COALESCE(s.mode_name, '') AS mn,
		        COALESCE(SUM(CAST(t.completed_at AS REAL) - CAST(t.started_at AS REAL)), 0) AS ns
		   FROM subtasks t
		   JOIN sessions s ON s.id = t.session_id
		  WHERE t.started_at IS NOT NULL AND t.completed_at IS NOT NULL
		  GROUP BY mn`,
	)
	if err != nil {
		return nil, err
	}
	defer stRows.Close()
	for stRows.Next() {
		var mn string
		var ns float64
		if err := stRows.Scan(&mn, &ns); err != nil {
			return nil, err
		}
		ms, ok := out[mn]
		if !ok {
			ms = &ModeStat{ModeName: mn}
			out[mn] = ms
		}
		ms.SubtaskSeconds = ns / 1e9
	}
	if err := stRows.Err(); err != nil {
		return nil, err
	}

	// Recent sessions per mode.
	for mn, ms := range out {
		recent, err := s.recentSessionsForMode(mn, recentLimit)
		if err != nil {
			return nil, err
		}
		ms.RecentSessions = recent
	}

	// Sentinel-alert counts last 24h per mode.
	since := time.Now().Add(-24 * time.Hour).UnixNano()
	alertRows, err := s.DB.Query(
		`SELECT COALESCE(s.mode_name, '') AS mn, COUNT(*) AS n
		   FROM trajectory_events e
		   JOIN sessions s ON s.id = e.session_id
		  WHERE e.kind = 'sentinel_alert' AND e.ts >= ?
		  GROUP BY mn`, since,
	)
	if err != nil {
		return nil, err
	}
	defer alertRows.Close()
	for alertRows.Next() {
		var mn string
		var n int
		if err := alertRows.Scan(&mn, &n); err != nil {
			return nil, err
		}
		ms, ok := out[mn]
		if !ok {
			ms = &ModeStat{ModeName: mn}
			out[mn] = ms
		}
		ms.SentinelAlerts24h = n
	}
	return out, alertRows.Err()
}

// CountSessionsByMode returns the total number of sessions whose mode_name
// equals modeName. Empty modeName counts the "no profile attached" bucket
// (mode_name IS NULL OR mode_name = ”).
func (s *Store) CountSessionsByMode(modeName string) (int, error) {
	var n int
	var err error
	if modeName == "" {
		err = s.DB.QueryRow(
			`SELECT COUNT(*) FROM sessions WHERE mode_name IS NULL OR mode_name = ''`,
		).Scan(&n)
	} else {
		err = s.DB.QueryRow(
			`SELECT COUNT(*) FROM sessions WHERE mode_name = ?`, modeName,
		).Scan(&n)
	}
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RecentSessionsByMode returns the newest sessions whose mode_name matches
// modeName (or the empty string for the no-profile bucket), capped at limit.
// Public wrapper around recentSessionsForMode for callers outside this
// package (e.g. the retrospective scheduler).
func (s *Store) RecentSessionsByMode(modeName string, limit int) ([]Session, error) {
	return s.recentSessionsForMode(modeName, limit)
}

// recentSessionsForMode returns the newest sessions whose mode_name
// matches modeName (or the empty string for the no-profile bucket).
func (s *Store) recentSessionsForMode(modeName string, limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 5
	}
	var rows *sql.Rows
	var err error
	if modeName == "" {
		rows, err = s.DB.Query(
			`SELECT id, goal, worker, COALESCE(planner, ''), status, created_at, completed_at,
			        COALESCE(final_answer_ref, ''), COALESCE(workflow_path, ''), meta_json,
			        COALESCE(mode_name, '')
			   FROM sessions
			  WHERE mode_name IS NULL OR mode_name = ''
			  ORDER BY created_at DESC
			  LIMIT ?`, limit)
	} else {
		rows, err = s.DB.Query(
			`SELECT id, goal, worker, COALESCE(planner, ''), status, created_at, completed_at,
			        COALESCE(final_answer_ref, ''), COALESCE(workflow_path, ''), meta_json,
			        COALESCE(mode_name, '')
			   FROM sessions
			  WHERE mode_name = ?
			  ORDER BY created_at DESC
			  LIMIT ?`, modeName, limit)
	}
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
			&createdNs, &completedNs, &sess.FinalAnswerRef, &sess.WorkflowPath, &sess.MetaJSON, &sess.ModeName); err != nil {
			return nil, err
		}
		sess.CreatedAt = time.Unix(0, createdNs)
		sess.CompletedAt = nullTime(completedNs)
		out = append(out, sess)
	}
	return out, rows.Err()
}

// SentinelAlertRow is one persisted sentinel_alert trajectory event,
// joined with the session it belongs to so dashboard renderers can
// present "rule, severity, mode, when" without extra round-trips.
type SentinelAlertRow struct {
	SessionID string
	ModeName  string
	Ts        time.Time
	Rule      string
	Severity  string
	Message   string
}

// ListSentinelAlertsSince returns persisted sentinel_alert events that fired
// at or after since, newest-first, capped by limit (<=0 means default 50).
func (s *Store) ListSentinelAlertsSince(since time.Time, limit int) ([]SentinelAlertRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.DB.Query(
		`SELECT e.session_id, COALESCE(s.mode_name, ''), e.ts, e.payload_json
		   FROM trajectory_events e
		   JOIN sessions s ON s.id = e.session_id
		  WHERE e.kind = 'sentinel_alert' AND e.ts >= ?
		  ORDER BY e.ts DESC
		  LIMIT ?`, since.UnixNano(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SentinelAlertRow
	for rows.Next() {
		var row SentinelAlertRow
		var tsNs int64
		var payload string
		if err := rows.Scan(&row.SessionID, &row.ModeName, &tsNs, &payload); err != nil {
			return nil, err
		}
		row.Ts = time.Unix(0, tsNs)
		var parsed struct {
			Rule     string `json:"rule"`
			Severity string `json:"severity"`
			Message  string `json:"message"`
		}
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			// Malformed payload: surface the raw bytes as the message so the
			// alert is still visible in dashboards instead of rendering as
			// an empty row.
			row.Message = payload
		} else {
			row.Rule = parsed.Rule
			row.Severity = parsed.Severity
			row.Message = parsed.Message
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ResolveSessionID expands a session-id prefix to the full id. The CLI
// prints 8-char short ids everywhere (`uta sessions`, dash, the hello
// tour), so every command that accepts a session id must accept what
// those surfaces display. Resolution rules:
//
//   - exact match wins immediately (full UUIDs never get prefix-scanned)
//   - a unique prefix resolves to its full id
//   - an ambiguous prefix errors and lists the candidates
//   - no match errors with "session not found"
func (s *Store) ResolveSessionID(idOrPrefix string) (string, error) {
	idOrPrefix = strings.TrimSpace(idOrPrefix)
	if idOrPrefix == "" {
		return "", errors.New("session id is empty")
	}
	var exact string
	err := s.DB.QueryRow(`SELECT id FROM sessions WHERE id = ?`, idOrPrefix).Scan(&exact)
	if err == nil {
		return exact, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	// ESCAPE so a prefix containing % or _ can't widen the scan.
	pattern := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(idOrPrefix) + "%"
	rows, err := s.DB.Query(
		`SELECT id FROM sessions WHERE id LIKE ? ESCAPE '\' ORDER BY created_at DESC LIMIT 5`, pattern)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		matches = append(matches, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("session not found: %s", idOrPrefix)
	case 1:
		return matches[0], nil
	default:
		short := make([]string, len(matches))
		for i, m := range matches {
			if len(m) > 8 {
				short[i] = m[:8]
			} else {
				short[i] = m
			}
		}
		return "", fmt.Errorf("session id prefix %q is ambiguous (matches %s…) — use more characters",
			idOrPrefix, strings.Join(short, "…, "))
	}
}

// CostBucket is one row of the cost rollup `uta perf --cost` renders.
// Mode/Provider/Day are the three rollup dimensions; counts are summed
// across every subtask that completed within the time window with a
// recorded meta_json usage stanza. Mode is empty for sessions that ran
// without a profile.
type CostBucket struct {
	Day       string // YYYY-MM-DD in UTC
	ModeName  string
	Provider  string
	Calls     int
	TokensIn  int64
	TokensOut int64
	USDCents  int64
}

// CostRollups returns per-(day, mode, provider) usage totals for every
// subtask that completed at or after cutoff. Reads tokens_in/tokens_out/
// usd_cents from subtasks.meta_json (set by the supervisor at completion
// time); subtasks without those fields contribute zero.
//
// Buckets are ordered: day DESC, mode ASC, provider ASC, so the most
// recent activity surfaces first.
func (s *Store) CostRollups(cutoff time.Time, modeFilter string) ([]CostBucket, error) {
	// json_extract reads from meta_json without a column shape change.
	// COALESCE+integer fallback handles sessions written before usage was
	// persisted (older subtasks have meta_json="{}").
	q := `
SELECT
  strftime('%Y-%m-%d', t.completed_at / 1000000000, 'unixepoch') AS day,
  COALESCE(s.mode_name, '') AS mode_name,
  t.worker AS provider,
  COUNT(*) AS calls,
  COALESCE(SUM(CAST(json_extract(t.meta_json, '$.tokens_in')  AS INTEGER)), 0) AS tokens_in,
  COALESCE(SUM(CAST(json_extract(t.meta_json, '$.tokens_out') AS INTEGER)), 0) AS tokens_out,
  COALESCE(SUM(CAST(json_extract(t.meta_json, '$.usd_cents')  AS INTEGER)), 0) AS usd_cents
FROM subtasks t
JOIN sessions s ON s.id = t.session_id
WHERE t.completed_at IS NOT NULL
  AND t.completed_at >= ?`
	args := []any{cutoff.UnixNano()}
	if modeFilter != "" {
		q += ` AND COALESCE(s.mode_name, '') = ?`
		args = append(args, modeFilter)
	}
	q += `
GROUP BY day, mode_name, provider
ORDER BY day DESC, mode_name ASC, provider ASC`
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CostBucket
	for rows.Next() {
		var b CostBucket
		if err := rows.Scan(&b.Day, &b.ModeName, &b.Provider, &b.Calls, &b.TokensIn, &b.TokensOut, &b.USDCents); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// SubtaskListBySession returns subtasks ordered by ord. limit<=0 means no
// row cap; offset<=0 means start at the first row.
func (s *Store) SubtaskListBySession(sessionID string, limit, offset int) ([]Subtask, error) {
	q := `SELECT id, session_id, ord, COALESCE(spec_id, ''), title, prompt_ref, worker, COALESCE(provider_session_id, ''), status, started_at, completed_at, COALESCE(result_text, ''), COALESCE(raw_output_ref, ''), COALESCE(error, ''), COALESCE(error_kind, ''), meta_json FROM subtasks WHERE session_id = ? ORDER BY ord ASC`
	args := []any{sessionID}
	pageSQL, pageArgs := paginationSQL(limit, offset)
	q += pageSQL
	args = append(args, pageArgs...)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subtask
	for rows.Next() {
		var t Subtask
		var started, completed sql.NullInt64
		if err := rows.Scan(&t.ID, &t.SessionID, &t.Ord, &t.SpecID, &t.Title, &t.PromptRef, &t.Worker,
			&t.ProviderSessionID, &t.Status, &started, &completed,
			&t.ResultText, &t.RawOutputRef, &t.Error, &t.ErrorKind, &t.MetaJSON); err != nil {
			return nil, err
		}
		t.StartedAt = nullTime(started)
		t.CompletedAt = nullTime(completed)
		out = append(out, t)
	}
	return out, rows.Err()
}
