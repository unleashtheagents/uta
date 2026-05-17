// Package improve houses the self-improvement primitives: the idea board
// (a persisted backlog of proposed code changes) and the long-running loop
// that pulls from it, executes one idea, records the outcome, and loops.
//
// Storage is project-scoped — ideas live in the same SQLite database as
// sessions and trajectories, so `uta project init` is a prerequisite for
// the full workflow. Without a project, ideas land in the global home and
// the loop runs the same way (just with no project context to share).
package improve

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/unleashtheagents/uta/internal/store"
)

// Severity mirrors engine.Severity but the improve package deliberately
// avoids importing engine to keep the dep direction one-way. Strings only.
type Severity = string

const (
	SevHigh   Severity = "high"
	SevMedium Severity = "medium"
	SevLow    Severity = "low"
	SevInfo   Severity = "info"
)

// Status is the lifecycle state of an idea.
type Status = string

const (
	StatusProposed   Status = "proposed"
	StatusAccepted   Status = "accepted"
	StatusInProgress Status = "in_progress"
	StatusDone       Status = "done"
	StatusFailed     Status = "failed"
	StatusRejected   Status = "rejected"
)

// Idea is one entry on the board.
type Idea struct {
	ID          string
	Title       string
	Body        string
	Source      string
	Severity    Severity
	Status      Status
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Attempts    int
	LastSession string
	LastError   string
	Summary     string
	Tags        []string
	Meta        map[string]any
}

// Board is the DAO for ideas. Reuses the supplied *store.Store; ideas live
// alongside sessions/trajectories in the same SQLite DB.
type Board struct {
	store *store.Store
}

func NewBoard(s *store.Store) *Board { return &Board{store: s} }

// Insert creates a new idea row. If idea.ID is empty, a UUID is generated.
// If idea.CreatedAt is zero, time.Now is used. Status defaults to proposed.
func (b *Board) Insert(idea *Idea) error {
	if idea.Title == "" {
		return errors.New("idea.title is required")
	}
	if idea.ID == "" {
		idea.ID = uuid.NewString()
	}
	now := time.Now()
	if idea.CreatedAt.IsZero() {
		idea.CreatedAt = now
	}
	idea.UpdatedAt = now
	if idea.Status == "" {
		idea.Status = StatusProposed
	}
	if idea.Severity == "" {
		idea.Severity = SevMedium
	}
	if idea.Source == "" {
		idea.Source = "manual"
	}
	tags, _ := json.Marshal(safeStrings(idea.Tags))
	meta, _ := json.Marshal(safeMap(idea.Meta))
	_, err := b.store.DB.Exec(
		`INSERT INTO ideas
		 (id, title, body, source, severity, status, created_at, updated_at,
		  attempts, last_session, last_error, summary, tags_json, meta_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		idea.ID, idea.Title, idea.Body, idea.Source, idea.Severity, idea.Status,
		idea.CreatedAt.UnixNano(), idea.UpdatedAt.UnixNano(),
		idea.Attempts, nullStr(idea.LastSession), nullStr(idea.LastError),
		nullStr(idea.Summary), string(tags), string(meta),
	)
	return err
}

// SetStatus moves an idea to a new status, optionally bumping attempts and
// stamping a session id / error / summary.
func (b *Board) SetStatus(id string, status Status, opts SetStatusOpts) error {
	q := `UPDATE ideas SET status = ?, updated_at = ?`
	args := []any{status, time.Now().UnixNano()}
	if opts.IncrementAttempts {
		q += `, attempts = attempts + 1`
	}
	if opts.LastSession != "" {
		q += `, last_session = ?`
		args = append(args, opts.LastSession)
	}
	if opts.LastError != nil {
		q += `, last_error = ?`
		args = append(args, nullStr(*opts.LastError))
	}
	if opts.Summary != nil {
		q += `, summary = ?`
		args = append(args, nullStr(*opts.Summary))
	}
	q += ` WHERE id = ?`
	args = append(args, id)
	res, err := b.store.DB.Exec(q, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("idea %q not found", id)
	}
	return nil
}

// SetStatusOpts carries optional updates that piggyback on SetStatus.
type SetStatusOpts struct {
	IncrementAttempts bool
	LastSession       string
	LastError         *string
	Summary           *string
}

// Get fetches one idea by id.
func (b *Board) Get(id string) (*Idea, error) {
	row := b.store.DB.QueryRow(
		`SELECT id, title, body, source, severity, status, created_at, updated_at,
		        attempts, COALESCE(last_session,''), COALESCE(last_error,''),
		        COALESCE(summary,''), tags_json, meta_json
		 FROM ideas WHERE id = ?`, id)
	return scanIdea(row)
}

// ListOptions filters the result of List.
type ListOptions struct {
	Status   Status // empty = any
	Severity Severity
	Limit    int  // 0 = no limit (capped at 500)
	Newest   bool // sort by created_at DESC vs (status, severity DESC, created_at ASC)
}

// List returns ideas matching opts. Default order: proposed-first, then
// severity DESC, then created_at ASC (so "highest-priority oldest" wins).
func (b *Board) List(opts ListOptions) ([]*Idea, error) {
	q := `SELECT id, title, body, source, severity, status, created_at, updated_at,
	             attempts, COALESCE(last_session,''), COALESCE(last_error,''),
	             COALESCE(summary,''), tags_json, meta_json
	      FROM ideas`
	var args []any
	var conds []string
	if opts.Status != "" {
		conds = append(conds, "status = ?")
		args = append(args, opts.Status)
	}
	if opts.Severity != "" {
		conds = append(conds, "severity = ?")
		args = append(args, opts.Severity)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	if opts.Newest {
		q += " ORDER BY created_at DESC"
	} else {
		q += " ORDER BY CASE status WHEN 'in_progress' THEN 0 WHEN 'proposed' THEN 1 WHEN 'accepted' THEN 2 WHEN 'failed' THEN 3 WHEN 'done' THEN 4 ELSE 5 END, " +
			"CASE severity WHEN 'high' THEN 0 WHEN 'medium' THEN 1 WHEN 'low' THEN 2 ELSE 3 END, created_at ASC"
	}
	limit := opts.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	q += " LIMIT ?"
	args = append(args, limit)

	rows, err := b.store.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Idea
	for rows.Next() {
		idea, err := scanIdea(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, idea)
	}
	return out, rows.Err()
}

// NextActionable returns the highest-priority idea in proposed or accepted
// state, or sql.ErrNoRows when the backlog is empty.
func (b *Board) NextActionable() (*Idea, error) {
	row := b.store.DB.QueryRow(
		`SELECT id, title, body, source, severity, status, created_at, updated_at,
		        attempts, COALESCE(last_session,''), COALESCE(last_error,''),
		        COALESCE(summary,''), tags_json, meta_json
		 FROM ideas
		 WHERE status IN ('proposed','accepted')
		 ORDER BY CASE severity WHEN 'high' THEN 0 WHEN 'medium' THEN 1 WHEN 'low' THEN 2 ELSE 3 END,
		          created_at ASC
		 LIMIT 1`)
	return scanIdea(row)
}

// Stats returns counts grouped by status. Useful for `uta ideas list` headers
// and the improve-loop summary.
func (b *Board) Stats() (map[Status]int, error) {
	rows, err := b.store.DB.Query(`SELECT status, COUNT(*) FROM ideas GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[Status]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// Delete removes an idea by id.
func (b *Board) Delete(id string) error {
	res, err := b.store.DB.Exec(`DELETE FROM ideas WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("idea %q not found", id)
	}
	return nil
}

// scanIdea reads one row from QueryRow or Rows.Scan.
type scanner interface {
	Scan(dest ...any) error
}

func scanIdea(s scanner) (*Idea, error) {
	var idea Idea
	var createdNs, updatedNs int64
	var tags, meta string
	err := s.Scan(&idea.ID, &idea.Title, &idea.Body, &idea.Source, &idea.Severity, &idea.Status,
		&createdNs, &updatedNs, &idea.Attempts, &idea.LastSession, &idea.LastError,
		&idea.Summary, &tags, &meta)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, err
	}
	idea.CreatedAt = time.Unix(0, createdNs)
	idea.UpdatedAt = time.Unix(0, updatedNs)
	_ = json.Unmarshal([]byte(tags), &idea.Tags)
	_ = json.Unmarshal([]byte(meta), &idea.Meta)
	if idea.Meta == nil {
		idea.Meta = map[string]any{}
	}
	return &idea, nil
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func safeStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func safeMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}
