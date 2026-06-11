// Package memory is uta's institutional-memory layer — salient takeaways
// produced by one run that future runs (possibly in another mode) should
// consider when planning. Facts live in the same SQLite database the rest
// of uta uses; this package only owns reads and writes against the
// institutional_facts table (created by migration 0006).
//
// v1 retrieval is tag-overlap with the planner's goal. Vector search /
// embeddings and LLM-based fact summarization are intentionally out of
// scope for this iteration.
package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Fact is one row in institutional_facts. ID is assigned by SQLite on
// insert; callers populate the rest. Tags is the source-of-truth slice;
// the store serializes it space-joined for persistence.
type Fact struct {
	ID              int64
	SourceMode      string
	SourceSessionID string
	Kind            string
	Body            string
	CreatedAt       time.Time
	Tags            []string
}

// Store is a thin wrapper around the shared SQLite handle. It's
// intentionally tiny — callers (engine, CLI) own the *sql.DB and pass it
// in so the memory layer doesn't have to know how the database was
// opened.
type Store struct {
	DB *sql.DB
	// Embedder, when non-nil, enables the vector half of the RAG index
	// (see rag.go). Nil keeps retrieval FTS5/BM25-only — fully
	// functional, zero external dependencies. Set by CLI wiring from
	// NewEmbedderFromEnv.
	Embedder Embedder
}

// New wraps an open *sql.DB with no embedder (FTS-only RAG). The caller
// is responsible for keeping the database alive. Tests use this so no
// environment variable can make them call a real embedding API.
func New(db *sql.DB) *Store { return &Store{DB: db} }

// NewFromEnv wraps an open *sql.DB and resolves the embedder from the
// environment (UTA_EMBEDDER / GEMINI_API_KEY / OLLAMA_HOST). This is
// the constructor CLI commands use.
func NewFromEnv(db *sql.DB) *Store { return &Store{DB: db, Embedder: NewEmbedderFromEnv()} }

// Write inserts a single fact. CreatedAt is set to time.Now() when zero.
// Tags are normalized (lowercased, whitespace-trimmed, deduped) before
// persistence so retrieval can rely on a canonical form.
func (s *Store) Write(f Fact) (int64, error) {
	if s == nil || s.DB == nil {
		return 0, fmt.Errorf("memory: store not initialised")
	}
	kind := strings.TrimSpace(f.Kind)
	if kind == "" {
		return 0, fmt.Errorf("memory: kind is required")
	}
	body := strings.TrimSpace(f.Body)
	if body == "" {
		return 0, fmt.Errorf("memory: body is required")
	}
	if f.CreatedAt.IsZero() {
		f.CreatedAt = time.Now()
	}
	tags := NormalizeTags(f.Tags)
	res, err := s.DB.Exec(
		`INSERT INTO institutional_facts (source_mode, source_session_id, kind, body, created_at, tags)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		f.SourceMode, nullable(f.SourceSessionID), kind, body, f.CreatedAt.UnixNano(), strings.Join(tags, " "),
	)
	if err != nil {
		return 0, fmt.Errorf("memory: insert fact: %w", err)
	}
	id, _ := res.LastInsertId()

	// Mirror the fact into the RAG index so hybrid retrieval sees it.
	// Document.Kind carries the fact's own kind (lint_rule,
	// session_outcome, ...) — that's what the planner block renders —
	// and Ref marks the provenance. Best-effort by design: a failed
	// embed (or even a failed FTS write) must not fail the fact write —
	// the fact row is the source of truth and `uta recall --reindex`
	// can rebuild the index any time.
	ctx, cancel := context.WithTimeout(context.Background(), embedHTTPTimeout)
	defer cancel()
	_, _ = s.IndexDocument(ctx, Document{
		Kind:      kind,
		Ref:       fmt.Sprintf("fact:%d", id),
		ModeName:  f.SourceMode,
		Body:      body,
		CreatedAt: f.CreatedAt,
	})
	return id, nil
}

// CountsByMode returns the number of facts grouped by source_mode. The
// empty-string key collects facts written without a profile attached.
// Used by `uta memory stats`.
func (s *Store) CountsByMode() (map[string]int, error) {
	if s == nil || s.DB == nil {
		return map[string]int{}, nil
	}
	rows, err := s.DB.Query(`SELECT COALESCE(source_mode, ''), COUNT(*) FROM institutional_facts GROUP BY source_mode`)
	if err != nil {
		return nil, fmt.Errorf("memory: counts by mode: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var mode string
		var n int
		if err := rows.Scan(&mode, &n); err != nil {
			return nil, err
		}
		out[mode] = n
	}
	return out, rows.Err()
}

// Total returns the total fact count. Convenience for stats / dashboard.
func (s *Store) Total() (int, error) {
	if s == nil || s.DB == nil {
		return 0, nil
	}
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM institutional_facts`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// TopRelevant returns up to limit facts whose tags overlap with the
// supplied query tokens, ranked by overlap count (ties broken by recency,
// then id descending). Returns nil when the query produces no tokens or
// when the table is empty — callers should treat that as "no relevant
// memory" and skip injection.
//
// The implementation is intentionally simple: it pulls every fact (the
// table is expected to stay small for v1) and scores in Go. When the
// table grows to a size where this becomes a problem, the indexes on
// kind + source_mode + created_at give us obvious filtering options.
func (s *Store) TopRelevant(queryTags []string, limit int) ([]Fact, error) {
	if s == nil || s.DB == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	tokens := NormalizeTags(queryTags)
	if len(tokens) == 0 {
		return nil, nil
	}
	tokenSet := make(map[string]struct{}, len(tokens))
	for _, t := range tokens {
		tokenSet[t] = struct{}{}
	}
	rows, err := s.DB.Query(
		`SELECT id, COALESCE(source_mode, ''), COALESCE(source_session_id, ''), kind, body, created_at, tags
		   FROM institutional_facts`,
	)
	if err != nil {
		return nil, fmt.Errorf("memory: scan facts: %w", err)
	}
	defer rows.Close()
	type scored struct {
		fact    Fact
		overlap int
	}
	var hits []scored
	for rows.Next() {
		var f Fact
		var createdNs int64
		var tagsStr string
		if err := rows.Scan(&f.ID, &f.SourceMode, &f.SourceSessionID, &f.Kind, &f.Body, &createdNs, &tagsStr); err != nil {
			return nil, err
		}
		f.CreatedAt = time.Unix(0, createdNs)
		f.Tags = splitTagString(tagsStr)
		overlap := 0
		for _, tag := range f.Tags {
			if _, ok := tokenSet[tag]; ok {
				overlap++
			}
		}
		if overlap == 0 {
			continue
		}
		hits = append(hits, scored{fact: f, overlap: overlap})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].overlap != hits[j].overlap {
			return hits[i].overlap > hits[j].overlap
		}
		if !hits[i].fact.CreatedAt.Equal(hits[j].fact.CreatedAt) {
			return hits[i].fact.CreatedAt.After(hits[j].fact.CreatedAt)
		}
		return hits[i].fact.ID > hits[j].fact.ID
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Fact, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.fact)
	}
	return out, nil
}

// NormalizeTags lowercases, trims, splits compound entries on whitespace
// or punctuation, drops short/empty tokens, and dedupes. The output is
// the canonical tag form used both for write-side persistence and for
// read-side scoring — keeping the two in sync is what makes tag overlap
// a stable retrieval signal.
func NormalizeTags(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, raw := range in {
		for _, tok := range tokenize(raw) {
			if _, dup := seen[tok]; dup {
				continue
			}
			seen[tok] = struct{}{}
			out = append(out, tok)
		}
	}
	sort.Strings(out)
	return out
}

// TokenizeGoal extracts memory-search tokens from a planner goal. Same
// rules as NormalizeTags plus a small stop-word filter — common English
// glue words ("the", "a", "for", ...) make for noisy overlap signals.
func TokenizeGoal(goal string) []string {
	tokens := NormalizeTags([]string{goal})
	out := tokens[:0]
	for _, t := range tokens {
		if _, drop := stopWords[t]; drop {
			continue
		}
		out = append(out, t)
	}
	return out
}

// tokenize splits a free-form string into lowercase tokens at any
// non-alphanumeric boundary. Tokens shorter than two characters are
// dropped — single-letter overlap is mostly noise.
func tokenize(raw string) []string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r))
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < 2 {
			continue
		}
		out = append(out, f)
	}
	return out
}

func splitTagString(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// stopWords is a small English glue-word filter for goal tokenization.
// Conservative — we'd rather miss filtering a borderline word than drop
// a real signal (a project named "the-bar" should still be searchable).
var stopWords = map[string]struct{}{
	"the": {}, "and": {}, "for": {}, "with": {}, "from": {}, "that": {}, "this": {},
	"into": {}, "onto": {}, "are": {}, "was": {}, "were": {}, "but": {}, "not": {},
	"any": {}, "all": {}, "use": {}, "make": {}, "your": {}, "you": {}, "have": {},
	"add": {}, "new": {}, "old": {}, "get": {}, "set": {}, "run": {}, "now": {},
}
