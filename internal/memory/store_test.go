package memory

import (
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/store"
)

// newTestStore opens a real on-disk SQLite database in t.TempDir() and
// applies every embedded migration, then wraps the *sql.DB in a
// memory.Store. The schema lives in internal/store/migrations and is
// invoked through store.Open — that's how production wires the database
// up, so these tests exercise the same migration path (including 0006
// which creates the institutional_facts table).
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s.DB)
}

func TestNew_NilDBStillSafe(t *testing.T) {
	// New doesn't touch the DB; the nil guards live on each method.
	if New(nil) == nil {
		t.Fatal("New(nil) returned nil; expected a usable wrapper with nil-guards")
	}
}

func TestWrite_HappyPath(t *testing.T) {
	m := newTestStore(t)

	when := time.Unix(1_700_000_000, 0).UTC()
	id, err := m.Write(Fact{
		SourceMode:      "audit",
		SourceSessionID: "sess-abc",
		Kind:            "lint_rule",
		Body:            "reentrancy in withdraw",
		CreatedAt:       when,
		Tags:            []string{"Reentrancy", "Solidity"},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive insert id, got %d", id)
	}

	if n, err := m.Total(); err != nil || n != 1 {
		t.Fatalf("Total: got (%d,%v) want (1,nil)", n, err)
	}

	// TopRelevant against an overlapping tag pulls the fact back; verify
	// every scalar round-trips through SQLite as expected.
	hits, err := m.TopRelevant([]string{"reentrancy"}, 5)
	if err != nil {
		t.Fatalf("TopRelevant: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	f := hits[0]
	if f.ID != id {
		t.Fatalf("ID: got %d want %d", f.ID, id)
	}
	if f.SourceMode != "audit" || f.SourceSessionID != "sess-abc" {
		t.Fatalf("source fields: got mode=%q session=%q", f.SourceMode, f.SourceSessionID)
	}
	if f.Kind != "lint_rule" || f.Body != "reentrancy in withdraw" {
		t.Fatalf("kind/body: got %q / %q", f.Kind, f.Body)
	}
	if !f.CreatedAt.Equal(when) {
		t.Fatalf("CreatedAt: got %v want %v", f.CreatedAt, when)
	}
	// Tags should round-trip in their normalized (lowercase, sorted) form.
	if !reflect.DeepEqual(f.Tags, []string{"reentrancy", "solidity"}) {
		t.Fatalf("Tags: got %v want [reentrancy solidity]", f.Tags)
	}
}

func TestWrite_DefaultsCreatedAtWhenZero(t *testing.T) {
	m := newTestStore(t)

	before := time.Now()
	id, err := m.Write(Fact{Kind: "k", Body: "b", Tags: []string{"alpha"}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	after := time.Now()

	hits, err := m.TopRelevant([]string{"alpha"}, 1)
	if err != nil || len(hits) != 1 || hits[0].ID != id {
		t.Fatalf("readback: hits=%v err=%v", hits, err)
	}
	got := hits[0].CreatedAt
	if got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
		t.Fatalf("CreatedAt default should be ~now; got %v (before=%v after=%v)", got, before, after)
	}
}

func TestWrite_ValidationErrors(t *testing.T) {
	m := newTestStore(t)

	cases := []struct {
		name string
		f    Fact
	}{
		{"missing kind", Fact{Body: "b"}},
		{"blank kind", Fact{Kind: "   ", Body: "b"}},
		{"missing body", Fact{Kind: "k"}},
		{"blank body", Fact{Kind: "k", Body: "\t\n"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if _, err := m.Write(tc.f); err == nil {
				t.Fatalf("expected validation error, got nil")
			}
		})
	}
	if n, _ := m.Total(); n != 0 {
		t.Fatalf("no rows should have been inserted; Total=%d", n)
	}
}

func TestWrite_NilStoreAndNilDB(t *testing.T) {
	var nilStore *Store
	if _, err := nilStore.Write(Fact{Kind: "k", Body: "b"}); err == nil {
		t.Fatalf("Write on nil *Store: expected error, got nil")
	}
	empty := &Store{}
	if _, err := empty.Write(Fact{Kind: "k", Body: "b"}); err == nil {
		t.Fatalf("Write on &Store{} (nil DB): expected error, got nil")
	}
}

func TestTotalAndCountsByMode(t *testing.T) {
	m := newTestStore(t)

	if n, err := m.Total(); err != nil || n != 0 {
		t.Fatalf("Total empty: got (%d,%v) want (0,nil)", n, err)
	}
	if got, err := m.CountsByMode(); err != nil || len(got) != 0 {
		t.Fatalf("CountsByMode empty: got (%v,%v) want (empty,nil)", got, err)
	}

	seeds := []Fact{
		{SourceMode: "audit", Kind: "k", Body: "b1", Tags: []string{"a"}},
		{SourceMode: "audit", Kind: "k", Body: "b2", Tags: []string{"b"}},
		{SourceMode: "dev", Kind: "k", Body: "b3", Tags: []string{"c"}},
		{SourceMode: "", Kind: "k", Body: "b4", Tags: []string{"d"}}, // empty mode → "" key
	}
	for _, f := range seeds {
		if _, err := m.Write(f); err != nil {
			t.Fatalf("seed Write: %v", err)
		}
	}

	if n, err := m.Total(); err != nil || n != len(seeds) {
		t.Fatalf("Total: got (%d,%v) want (%d,nil)", n, err, len(seeds))
	}
	counts, err := m.CountsByMode()
	if err != nil {
		t.Fatalf("CountsByMode: %v", err)
	}
	want := map[string]int{"audit": 2, "dev": 1, "": 1}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("CountsByMode: got %v want %v", counts, want)
	}
}

func TestTotalAndCountsByMode_NilStore(t *testing.T) {
	var s *Store
	if n, err := s.Total(); err != nil || n != 0 {
		t.Fatalf("Total nil store: got (%d,%v) want (0,nil)", n, err)
	}
	got, err := s.CountsByMode()
	if err != nil {
		t.Fatalf("CountsByMode nil store err: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("CountsByMode nil store: expected empty map, got %v", got)
	}
}

func TestTopRelevant_RanksByOverlapThenRecencyThenID(t *testing.T) {
	m := newTestStore(t)

	// Two facts share 1 overlap tag, one shares 2 — the 2-overlap fact must
	// win regardless of timestamp. Among the two 1-overlap facts, the more
	// recent one wins (and on equal timestamps, the higher ID wins).
	t0 := time.Unix(1_700_000_000, 0).UTC()
	t1 := time.Unix(1_700_000_100, 0).UTC()
	t2 := time.Unix(1_700_000_200, 0).UTC()
	tEq := time.Unix(1_700_000_300, 0).UTC()

	idOld, _ := m.Write(Fact{Kind: "k", Body: "older single", CreatedAt: t0, Tags: []string{"alpha"}})
	idTop, _ := m.Write(Fact{Kind: "k", Body: "double overlap", CreatedAt: t1, Tags: []string{"alpha", "beta"}})
	idNew, _ := m.Write(Fact{Kind: "k", Body: "newer single", CreatedAt: t2, Tags: []string{"alpha"}})
	idEqA, _ := m.Write(Fact{Kind: "k", Body: "tie A", CreatedAt: tEq, Tags: []string{"alpha"}})
	idEqB, _ := m.Write(Fact{Kind: "k", Body: "tie B (later id)", CreatedAt: tEq, Tags: []string{"alpha"}})

	// Sanity on insertion order; the test reasoning relies on idEqB > idEqA.
	if !(idEqB > idEqA) {
		t.Fatalf("setup: expected idEqB > idEqA, got %d / %d", idEqB, idEqA)
	}

	hits, err := m.TopRelevant([]string{"alpha", "beta"}, 10)
	if err != nil {
		t.Fatalf("TopRelevant: %v", err)
	}
	if len(hits) != 5 {
		t.Fatalf("expected 5 hits, got %d", len(hits))
	}
	gotIDs := []int64{hits[0].ID, hits[1].ID, hits[2].ID, hits[3].ID, hits[4].ID}
	wantIDs := []int64{idTop, idEqB, idEqA, idNew, idOld}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("ranking IDs: got %v want %v", gotIDs, wantIDs)
	}
}

func TestTopRelevant_LimitApplied(t *testing.T) {
	m := newTestStore(t)
	for i := 0; i < 7; i++ {
		if _, err := m.Write(Fact{Kind: "k", Body: "x", Tags: []string{"alpha"}}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	hits, err := m.TopRelevant([]string{"alpha"}, 3)
	if err != nil {
		t.Fatalf("TopRelevant: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("limit=3: got %d hits", len(hits))
	}
}

func TestTopRelevant_DefaultLimitWhenNonPositive(t *testing.T) {
	m := newTestStore(t)
	for i := 0; i < 8; i++ {
		if _, err := m.Write(Fact{Kind: "k", Body: "x", Tags: []string{"alpha"}}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	for _, limit := range []int{0, -1, -100} {
		hits, err := m.TopRelevant([]string{"alpha"}, limit)
		if err != nil {
			t.Fatalf("TopRelevant limit=%d: %v", limit, err)
		}
		// Doc on TopRelevant: limit <= 0 falls back to 5.
		if len(hits) != 5 {
			t.Fatalf("limit=%d: got %d hits, want 5", limit, len(hits))
		}
	}
}

func TestTopRelevant_EmptyOrAllStopWordQueryReturnsNil(t *testing.T) {
	m := newTestStore(t)
	if _, err := m.Write(Fact{Kind: "k", Body: "x", Tags: []string{"alpha"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// NormalizeTags strips short/empty tokens; empty input → nil tokens →
	// nil result without scanning rows.
	for _, q := range [][]string{nil, {}, {""}, {"   "}, {"!"}, {"a"}} {
		hits, err := m.TopRelevant(q, 5)
		if err != nil {
			t.Fatalf("TopRelevant(%v): %v", q, err)
		}
		if hits != nil {
			t.Fatalf("TopRelevant(%v) expected nil, got %v", q, hits)
		}
	}
}

func TestTopRelevant_FactsWithEmptyTagsAreSkipped(t *testing.T) {
	// Round-trip safety: a fact persisted with no tags (either explicitly
	// or because every input token normalizes away) must never appear in
	// ranking results and must not derail the scan of facts that *do*
	// match. This is the "empty tags gracefully" property the audit
	// flagged.
	m := newTestStore(t)
	if _, err := m.Write(Fact{Kind: "k", Body: "tagged", Tags: []string{"reentrancy"}}); err != nil {
		t.Fatalf("seed tagged: %v", err)
	}
	if _, err := m.Write(Fact{Kind: "k", Body: "nil tags", Tags: nil}); err != nil {
		t.Fatalf("seed nil tags: %v", err)
	}
	if _, err := m.Write(Fact{Kind: "k", Body: "empty tag slice", Tags: []string{}}); err != nil {
		t.Fatalf("seed empty slice: %v", err)
	}
	// All tokens here normalize away (single-char + punctuation), so the
	// fact persists with an empty tags column.
	if _, err := m.Write(Fact{Kind: "k", Body: "drops to nothing", Tags: []string{"a", "b", "!"}}); err != nil {
		t.Fatalf("seed unnormalizable: %v", err)
	}

	hits, err := m.TopRelevant([]string{"reentrancy"}, 5)
	if err != nil {
		t.Fatalf("TopRelevant: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected only the tagged fact to match, got %d hits: %+v", len(hits), hits)
	}
	if hits[0].Body != "tagged" {
		t.Fatalf("hit body: got %q want %q", hits[0].Body, "tagged")
	}
}

func TestTopRelevant_NoOverlapReturnsEmpty(t *testing.T) {
	m := newTestStore(t)
	if _, err := m.Write(Fact{Kind: "k", Body: "x", Tags: []string{"solidity", "reentrancy"}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	hits, err := m.TopRelevant([]string{"python", "django"}, 5)
	if err != nil {
		t.Fatalf("TopRelevant: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected zero hits, got %v", hits)
	}
}

func TestTopRelevant_NilStore(t *testing.T) {
	var s *Store
	hits, err := s.TopRelevant([]string{"alpha"}, 5)
	if err != nil {
		t.Fatalf("TopRelevant nil store err: %v", err)
	}
	if hits != nil {
		t.Fatalf("TopRelevant nil store: expected nil hits, got %v", hits)
	}
}

func TestNormalizeTags(t *testing.T) {
	// The function lowercases, splits on non-alphanumeric, drops <2-char
	// tokens, dedupes, and sorts.
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, nil},
		{"single", []string{"Solidity"}, []string{"solidity"}},
		{"splits on punctuation", []string{"contracts/Vault.sol"}, []string{"contracts", "sol", "vault"}},
		{"dedupes across inputs", []string{"reentrancy", "Reentrancy", "REENTRANCY"}, []string{"reentrancy"}},
		{"drops short tokens", []string{"a b cd e"}, []string{"cd"}},
		{"sorts output", []string{"zeta", "alpha", "mu"}, []string{"alpha", "mu", "zeta"}},
		{"all dropped → nil", []string{"a", "b", "!", " "}, nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeTags(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestTokenizeGoal_DropsStopWords(t *testing.T) {
	got := TokenizeGoal("Review the auth middleware for the new release")
	// "the", "for", and "new" are in stopWords; the rest survives. Output
	// is sorted because NormalizeTags sorts before TokenizeGoal filters.
	want := []string{"auth", "middleware", "release", "review"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TokenizeGoal: got %v want %v", got, want)
	}
}

func TestTokenizeGoal_AllStopWordsYieldsEmpty(t *testing.T) {
	if got := TokenizeGoal("the and for with from"); len(got) != 0 {
		t.Fatalf("expected empty result for all-stop-words goal, got %v", got)
	}
}

// TestConcurrentWrites is the load test the upstream idea called for: a
// pile of goroutines hammering Write concurrently must produce N distinct
// rows with monotonically-increasing IDs and the right per-mode counts.
// modernc.org/sqlite serializes writes internally, so the property under
// test is "no lost writes, no corrupted rows" rather than "writes proceed
// in parallel."
func TestConcurrentWrites(t *testing.T) {
	m := newTestStore(t)

	const workers = 16
	const perWorker = 25
	const totalRows = workers * perWorker

	var wg sync.WaitGroup
	errCh := make(chan error, totalRows)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			mode := "audit"
			if worker%2 == 1 {
				mode = "dev"
			}
			for i := 0; i < perWorker; i++ {
				if _, err := m.Write(Fact{
					SourceMode: mode,
					Kind:       "lint_rule",
					Body:       "concurrent write",
					Tags:       []string{"alpha", mode},
				}); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent Write: %v", err)
		}
	}

	if n, err := m.Total(); err != nil || n != totalRows {
		t.Fatalf("Total after concurrent writes: got (%d,%v) want (%d,nil)", n, err, totalRows)
	}

	counts, err := m.CountsByMode()
	if err != nil {
		t.Fatalf("CountsByMode: %v", err)
	}
	// Half the workers wrote as "audit", half as "dev".
	if counts["audit"]+counts["dev"] != totalRows {
		t.Fatalf("CountsByMode: audit+dev should sum to %d, got %v", totalRows, counts)
	}
	if counts["audit"] != (workers/2)*perWorker || counts["dev"] != (workers/2)*perWorker {
		t.Fatalf("CountsByMode split: got %v want each mode = %d", counts, (workers/2)*perWorker)
	}

	// Verify IDs are unique and strictly increasing per the AUTOINCREMENT
	// column — a smoke test that no concurrent writer clobbered another.
	rows, err := m.DB.Query(`SELECT id FROM institutional_facts ORDER BY id`)
	if err != nil {
		t.Fatalf("select ids: %v", err)
	}
	defer rows.Close()
	seen := make(map[int64]struct{}, totalRows)
	var prev int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan id: %v", err)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id %d", id)
		}
		if id <= prev {
			t.Fatalf("non-increasing id sequence: %d after %d", id, prev)
		}
		seen[id] = struct{}{}
		prev = id
	}
	if len(seen) != totalRows {
		t.Fatalf("expected %d unique ids, got %d", totalRows, len(seen))
	}
}
