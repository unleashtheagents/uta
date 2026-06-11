// rag.go — hybrid retrieval over institutional memory.
//
// Two indexes over the same rag_documents rows:
//
//   - rag_fts: FTS5/BM25 keyword index. Always written, needs nothing.
//   - rag_vec: sqlite-vec vec0 KNN index. Written only when an Embedder
//     is configured; created lazily because its dimension comes from the
//     first embedding. rag_vec_meta pins (embedder, dim) so switching
//     models forces a rebuild instead of mixing vector spaces.
//
// Search fuses both lists with reciprocal rank fusion (RRF, k=60). With
// no embedder the vector list is empty and RRF degrades to pure BM25
// order — no special-casing needed.
package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Document is one indexed row.
type Document struct {
	ID        int64
	Kind      string // fact | final_answer | retrospective
	Ref       string // source identifier (fact id, session id, path)
	ModeName  string
	Body      string
	CreatedAt time.Time
}

// SearchResult pairs a document with its fused relevance score
// (higher = better) and which indexes contributed.
type SearchResult struct {
	Document
	Score   float64
	Matched []string // "fts", "vec"
}

// rrfK is the standard reciprocal-rank-fusion constant. 60 is the value
// from the original RRF paper and what most hybrid-search systems use.
const rrfK = 60

// perListLimit is how many candidates each index contributes before
// fusion. Wider than the caller's k so a document ranked low in one
// list can still win on the combined score.
const perListLimit = 50

// IndexDocument inserts a document into rag_documents + rag_fts, and —
// when an embedder is configured — embeds and stores its vector.
// Embedding failures are returned but the document IS already
// full-text indexed at that point; callers treat the error as a
// warning, not a rollback.
func (s *Store) IndexDocument(ctx context.Context, d Document) (int64, error) {
	if s == nil || s.DB == nil {
		return 0, errors.New("memory: store not initialised")
	}
	body := strings.TrimSpace(d.Body)
	if body == "" {
		return 0, errors.New("memory: document body is empty")
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now()
	}
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO rag_documents (kind, ref, mode_name, body, created_at) VALUES (?, ?, ?, ?, ?)`,
		d.Kind, d.Ref, d.ModeName, body, d.CreatedAt.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("memory: insert rag document: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO rag_fts(rowid, body) VALUES (?, ?)`, id, body); err != nil {
		return id, fmt.Errorf("memory: fts index: %w", err)
	}
	if s.Embedder == nil {
		return id, nil
	}
	if err := s.embedDocument(ctx, id, body); err != nil {
		return id, fmt.Errorf("memory: embed (doc %d still fts-indexed): %w", id, err)
	}
	return id, nil
}

// embedDocument computes the vector for one document and stores it in
// rag_vec, creating the table on first use.
func (s *Store) embedDocument(ctx context.Context, id int64, body string) error {
	vecs, err := s.Embedder.Embed(ctx, []string{body})
	if err != nil {
		return err
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return errors.New("embedder returned no vector")
	}
	if err := s.ensureVecTable(ctx, len(vecs[0])); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO rag_vec(rowid, embedding) VALUES (?, ?)`, id, vecJSON(vecs[0])); err != nil {
		return fmt.Errorf("insert vector: %w", err)
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE rag_documents SET embedded = 1 WHERE id = ?`, id)
	return err
}

// ensureVecTable creates rag_vec with the given dimension on first use
// and pins (embedder, dim) in rag_vec_meta. A mismatch — different
// model or different dimension than what the existing vectors were
// built with — is an error telling the operator to reindex.
func (s *Store) ensureVecTable(ctx context.Context, dim int) error {
	var gotEmbedder string
	var gotDim int
	err := s.DB.QueryRowContext(ctx, `SELECT embedder, dim FROM rag_vec_meta WHERE id = 1`).
		Scan(&gotEmbedder, &gotDim)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := s.DB.ExecContext(ctx,
			fmt.Sprintf(`CREATE VIRTUAL TABLE IF NOT EXISTS rag_vec USING vec0(embedding float[%d])`, dim)); err != nil {
			return fmt.Errorf("create rag_vec: %w", err)
		}
		_, err = s.DB.ExecContext(ctx,
			`INSERT INTO rag_vec_meta (id, embedder, dim) VALUES (1, ?, ?)`, s.Embedder.Name(), dim)
		return err
	case err != nil:
		return fmt.Errorf("read rag_vec_meta: %w", err)
	}
	if gotEmbedder != s.Embedder.Name() || gotDim != dim {
		return fmt.Errorf("embedder changed (%s/%d -> %s/%d); run `uta recall --reindex` to rebuild vectors",
			gotEmbedder, gotDim, s.Embedder.Name(), dim)
	}
	return nil
}

// SearchHybrid runs BM25 + vector KNN and fuses with RRF. Degrades
// gracefully: no embedder (or embed failure) → BM25-only; no FTS match
// → vector-only. Returns at most k results, best first.
func (s *Store) SearchHybrid(ctx context.Context, query string, k int) ([]SearchResult, error) {
	if s == nil || s.DB == nil {
		return nil, errors.New("memory: store not initialised")
	}
	query = strings.TrimSpace(query)
	if query == "" || k <= 0 {
		return nil, nil
	}

	ftsIDs, err := s.ftsCandidates(ctx, query)
	if err != nil {
		return nil, err
	}
	var vecIDs []int64
	if s.Embedder != nil && s.vecTableReady(ctx) {
		// Vector failures degrade to FTS-only — the embedder may be a
		// network service that's down; retrieval must not break.
		if ids, verr := s.vecCandidates(ctx, query); verr == nil {
			vecIDs = ids
		}
	}
	if len(ftsIDs) == 0 && len(vecIDs) == 0 {
		return nil, nil
	}

	// Reciprocal rank fusion across the two ranked lists.
	type fused struct {
		score   float64
		matched []string
	}
	scores := map[int64]*fused{}
	addList := func(ids []int64, label string) {
		for rank, id := range ids {
			f, ok := scores[id]
			if !ok {
				f = &fused{}
				scores[id] = f
			}
			f.score += 1.0 / float64(rrfK+rank+1)
			f.matched = append(f.matched, label)
		}
	}
	addList(ftsIDs, "fts")
	addList(vecIDs, "vec")

	ids := make([]int64, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return scores[ids[i]].score > scores[ids[j]].score })
	if len(ids) > k {
		ids = ids[:k]
	}

	docs, err := s.documentsByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]SearchResult, 0, len(ids))
	for _, id := range ids {
		d, ok := docs[id]
		if !ok {
			continue
		}
		out = append(out, SearchResult{
			Document: d,
			Score:    scores[id].score,
			Matched:  scores[id].matched,
		})
	}
	return out, nil
}

// ftsCandidates returns rag document ids ranked by BM25 for the query.
// The query is wrapped per-token with OR so a natural-language goal
// matches without FTS5 syntax knowledge; FTS5 operators in user text
// (AND/OR/NEAR, quotes) are neutralized by tokenizing first.
func (s *Store) ftsCandidates(ctx context.Context, query string) ([]int64, error) {
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return nil, nil
	}
	quoted := make([]string, len(tokens))
	for i, t := range tokens {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, ``) + `"`
	}
	match := strings.Join(quoted, " OR ")
	rows, err := s.DB.QueryContext(ctx,
		`SELECT rowid FROM rag_fts WHERE rag_fts MATCH ? ORDER BY bm25(rag_fts) LIMIT ?`,
		match, perListLimit)
	if err != nil {
		return nil, fmt.Errorf("memory: fts query: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// vecCandidates embeds the query and returns rag document ids ranked by
// vector distance (nearest first).
func (s *Store) vecCandidates(ctx context.Context, query string) ([]int64, error) {
	vecs, err := s.Embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vecs) != 1 {
		return nil, errors.New("embedder returned no query vector")
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT rowid FROM rag_vec WHERE embedding MATCH ? ORDER BY distance LIMIT ?`,
		vecJSON(vecs[0]), perListLimit)
	if err != nil {
		return nil, fmt.Errorf("memory: vec query: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// vecTableReady reports whether rag_vec exists (created lazily on the
// first successful embed).
func (s *Store) vecTableReady(ctx context.Context) bool {
	var one int
	err := s.DB.QueryRowContext(ctx,
		`SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'rag_vec'`).Scan(&one)
	return err == nil
}

func (s *Store) documentsByID(ctx context.Context, ids []int64) (map[int64]Document, error) {
	out := map[int64]Document{}
	if len(ids) == 0 {
		return out, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, kind, ref, mode_name, body, created_at FROM rag_documents WHERE id IN (`+placeholders+`)`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d Document
		var ns int64
		if err := rows.Scan(&d.ID, &d.Kind, &d.Ref, &d.ModeName, &d.Body, &ns); err != nil {
			return nil, err
		}
		d.CreatedAt = time.Unix(0, ns)
		out[d.ID] = d
	}
	return out, rows.Err()
}

// Reindex rebuilds rag_documents (and vectors, when an embedder is
// configured) from the institutional_facts table. Returns (indexed,
// embedded) counts. Existing rag rows are dropped first — reindexing is
// idempotent and safe to run after switching embedders.
func (s *Store) Reindex(ctx context.Context) (indexed, embedded int, err error) {
	if s == nil || s.DB == nil {
		return 0, 0, errors.New("memory: store not initialised")
	}
	for _, stmt := range []string{
		`DELETE FROM rag_documents`,
		`DELETE FROM rag_fts`,
		`DROP TABLE IF EXISTS rag_vec`,
		`DELETE FROM rag_vec_meta`,
	} {
		if _, err := s.DB.ExecContext(ctx, stmt); err != nil {
			return 0, 0, fmt.Errorf("memory: reindex reset (%s): %w", stmt, err)
		}
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, COALESCE(source_mode, ''), COALESCE(source_session_id, ''), kind, body, created_at
		   FROM institutional_facts ORDER BY id`)
	if err != nil {
		return 0, 0, fmt.Errorf("memory: read facts: %w", err)
	}
	defer rows.Close()
	type factRow struct {
		id      int64
		mode    string
		session string
		kind    string
		body    string
		ns      int64
	}
	var facts []factRow
	for rows.Next() {
		var f factRow
		if err := rows.Scan(&f.id, &f.mode, &f.session, &f.kind, &f.body, &f.ns); err != nil {
			return 0, 0, err
		}
		facts = append(facts, f)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	for _, f := range facts {
		id, ierr := s.IndexDocument(ctx, Document{
			Kind:      f.kind,
			Ref:       fmt.Sprintf("fact:%d", f.id),
			ModeName:  f.mode,
			Body:      f.body,
			CreatedAt: time.Unix(0, f.ns),
		})
		if ierr != nil && id == 0 {
			return indexed, embedded, ierr
		}
		indexed++
		if ierr == nil && s.Embedder != nil {
			embedded++
		}
	}
	return indexed, embedded, nil
}

// vecJSON serializes a vector in the JSON text form sqlite-vec accepts
// for both inserts and MATCH queries.
func vecJSON(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", f)
	}
	b.WriteByte(']')
	return b.String()
}
