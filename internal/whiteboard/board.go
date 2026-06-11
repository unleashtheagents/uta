// Package whiteboard is uta's inter-agent shared scratchpad — a per-project
// SQLite-backed key/value store with append-only history that active
// threads or modes use to leave notes for each other. Ops mode can post a
// "blocker" note; the next dev-mode run sees it injected into its planner
// context and reflects it in the plan.
//
// History is the source of truth: every Set call inserts a new row stamped
// with the author mode + session + timestamp. The Get / List read paths
// return the latest entry per key. Polling on load is the only refresh
// mechanism — real-time pub/sub is intentionally out of scope.
package whiteboard

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Entry is one row in the whiteboard. ID is assigned by SQLite on insert;
// callers populate everything else. ValueJSON is the raw JSON text the
// writer supplied (validity is enforced on Set), so readers that want
// structure unmarshal it themselves.
type Entry struct {
	ID            int64
	Key           string
	ValueJSON     string
	AuthorMode    string
	AuthorSession string
	Ts            time.Time
}

// Store wraps the shared SQLite handle. Like the memory package, this is
// intentionally tiny — callers own the *sql.DB and pass it in so the
// whiteboard layer doesn't have to know how the database was opened.
type Store struct {
	DB *sql.DB
}

// New wraps an open *sql.DB. The caller is responsible for keeping the
// database alive.
func New(db *sql.DB) *Store { return &Store{DB: db} }

// Set inserts a new entry for key. ValueJSON must be valid JSON — the
// store rejects malformed payloads so readers can rely on json.Unmarshal
// succeeding. Ts is set to time.Now() when zero. Returns the inserted
// row id.
func (s *Store) Set(e Entry) (int64, error) {
	if s == nil || s.DB == nil {
		return 0, fmt.Errorf("whiteboard: store not initialised")
	}
	key := strings.TrimSpace(e.Key)
	if key == "" {
		return 0, fmt.Errorf("whiteboard: key is required")
	}
	value := strings.TrimSpace(e.ValueJSON)
	if value == "" {
		return 0, fmt.Errorf("whiteboard: value_json is required")
	}
	if !json.Valid([]byte(value)) {
		return 0, fmt.Errorf("whiteboard: value_json is not valid JSON")
	}
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}
	res, err := s.DB.Exec(
		`INSERT INTO whiteboard (key, value_json, author_mode, author_session, ts)
		 VALUES (?, ?, ?, ?, ?)`,
		key, value, e.AuthorMode, nullable(e.AuthorSession), e.Ts.UnixNano(),
	)
	if err != nil {
		return 0, fmt.Errorf("whiteboard: insert: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// Get returns the latest entry for key. The bool is false when no entry
// exists for the key — callers should treat that as "not yet set" rather
// than an error.
func (s *Store) Get(key string) (Entry, bool, error) {
	if s == nil || s.DB == nil {
		return Entry{}, false, nil
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return Entry{}, false, fmt.Errorf("whiteboard: key is required")
	}
	var (
		e             Entry
		tsNs          int64
		authorSession sql.NullString
	)
	err := s.DB.QueryRow(
		`SELECT id, key, value_json, author_mode, author_session, ts
		   FROM whiteboard WHERE key = ? ORDER BY ts DESC, id DESC LIMIT 1`,
		key,
	).Scan(&e.ID, &e.Key, &e.ValueJSON, &e.AuthorMode, &authorSession, &tsNs)
	if err == sql.ErrNoRows {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("whiteboard: get: %w", err)
	}
	e.Ts = time.Unix(0, tsNs)
	if authorSession.Valid {
		e.AuthorSession = authorSession.String
	}
	return e, true, nil
}

// List returns the latest entry for every distinct key, newest-first. The
// optional limit caps the number of keys returned (<=0 means no cap).
// Use History instead when the caller wants every version of a single key.
func (s *Store) List(limit int) ([]Entry, error) {
	if s == nil || s.DB == nil {
		return nil, nil
	}
	rows, err := s.DB.Query(
		`SELECT w.id, w.key, w.value_json, w.author_mode, w.author_session, w.ts
		   FROM whiteboard w
		   JOIN (
		     SELECT key, MAX(ts) AS max_ts FROM whiteboard GROUP BY key
		   ) latest ON latest.key = w.key AND latest.max_ts = w.ts
		   ORDER BY w.ts DESC, w.id DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("whiteboard: list: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var (
			e             Entry
			tsNs          int64
			authorSession sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.Key, &e.ValueJSON, &e.AuthorMode, &authorSession, &tsNs); err != nil {
			return nil, err
		}
		e.Ts = time.Unix(0, tsNs)
		if authorSession.Valid {
			e.AuthorSession = authorSession.String
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Defensive secondary sort: if two keys share the same ts the SQL
	// ordering above is stable but driver-dependent.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Ts.Equal(out[j].Ts) {
			return out[i].Ts.After(out[j].Ts)
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// History returns every entry recorded for key, oldest-first. Used by
// observability surfaces (CLI inspector, future TUI) that want to see how
// a note evolved across modes.
func (s *Store) History(key string) ([]Entry, error) {
	if s == nil || s.DB == nil {
		return nil, nil
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, fmt.Errorf("whiteboard: key is required")
	}
	rows, err := s.DB.Query(
		`SELECT id, key, value_json, author_mode, author_session, ts
		   FROM whiteboard WHERE key = ? ORDER BY ts ASC, id ASC`, key,
	)
	if err != nil {
		return nil, fmt.Errorf("whiteboard: history: %w", err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var (
			e             Entry
			tsNs          int64
			authorSession sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.Key, &e.ValueJSON, &e.AuthorMode, &authorSession, &tsNs); err != nil {
			return nil, err
		}
		e.Ts = time.Unix(0, tsNs)
		if authorSession.Valid {
			e.AuthorSession = authorSession.String
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
