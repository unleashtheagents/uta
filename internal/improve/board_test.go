package improve

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/store"
)

func newTestBoard(t *testing.T) *Board {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return NewBoard(s)
}

func TestInsert_AppliesDefaults(t *testing.T) {
	b := newTestBoard(t)

	idea := &Idea{Title: "do a thing"}
	if err := b.Insert(idea); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if idea.ID == "" {
		t.Fatalf("Insert should have populated ID")
	}
	if idea.CreatedAt.IsZero() {
		t.Fatalf("Insert should have stamped CreatedAt")
	}

	got, err := b.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusProposed {
		t.Fatalf("default Status: got %q want %q", got.Status, StatusProposed)
	}
	if got.Severity != SevMedium {
		t.Fatalf("default Severity: got %q want %q", got.Severity, SevMedium)
	}
	if got.Source != "manual" {
		t.Fatalf("default Source: got %q want %q", got.Source, "manual")
	}
	if got.Meta == nil {
		t.Fatalf("Meta should be initialized to empty map, got nil")
	}
}

func TestInsert_TitleRequired(t *testing.T) {
	b := newTestBoard(t)
	err := b.Insert(&Idea{Body: "no title"})
	if err == nil {
		t.Fatalf("expected error when Title is empty")
	}
}

func TestInsert_RoundTripsTagsAndMeta(t *testing.T) {
	b := newTestBoard(t)

	idea := &Idea{
		Title:    "with metadata",
		Severity: SevHigh,
		Tags:     []string{"perf", "refactor"},
		Meta: map[string]any{
			"score":   4.0,
			"flagged": true,
			"owner":   "thomas",
		},
	}
	if err := b.Insert(idea); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := b.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "perf" || got.Tags[1] != "refactor" {
		t.Fatalf("Tags round-trip: got %v want [perf refactor]", got.Tags)
	}
	if got.Meta["score"] != 4.0 {
		t.Fatalf("Meta.score: got %v want 4.0", got.Meta["score"])
	}
	if got.Meta["flagged"] != true {
		t.Fatalf("Meta.flagged: got %v want true", got.Meta["flagged"])
	}
	if got.Meta["owner"] != "thomas" {
		t.Fatalf("Meta.owner: got %v want thomas", got.Meta["owner"])
	}
}

func TestInsert_NilTagsAndMetaPersistAsEmpty(t *testing.T) {
	b := newTestBoard(t)
	idea := &Idea{Title: "no tags or meta"}
	if err := b.Insert(idea); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var tags, meta string
	row := b.store.DB.QueryRow(`SELECT tags_json, meta_json FROM ideas WHERE id = ?`, idea.ID)
	if err := row.Scan(&tags, &meta); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if tags != "[]" {
		t.Fatalf("tags_json: got %q want %q", tags, "[]")
	}
	if meta != "{}" {
		t.Fatalf("meta_json: got %q want %q", meta, "{}")
	}
}

func TestSetStatus_TransitionsAndAttempts(t *testing.T) {
	b := newTestBoard(t)

	idea := &Idea{Title: "transition me"}
	if err := b.Insert(idea); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// proposed -> in_progress (with attempts bumped)
	if err := b.SetStatus(idea.ID, StatusInProgress, SetStatusOpts{IncrementAttempts: true, LastSession: "sess-abc"}); err != nil {
		t.Fatalf("SetStatus in_progress: %v", err)
	}
	got, err := b.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusInProgress {
		t.Fatalf("Status: got %q want %q", got.Status, StatusInProgress)
	}
	if got.Attempts != 1 {
		t.Fatalf("Attempts: got %d want 1", got.Attempts)
	}
	if got.LastSession != "sess-abc" {
		t.Fatalf("LastSession: got %q want %q", got.LastSession, "sess-abc")
	}

	// in_progress -> done (with summary)
	summary := "fixed it"
	if err := b.SetStatus(idea.ID, StatusDone, SetStatusOpts{Summary: &summary}); err != nil {
		t.Fatalf("SetStatus done: %v", err)
	}
	got, err = b.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusDone {
		t.Fatalf("Status: got %q want %q", got.Status, StatusDone)
	}
	if got.Attempts != 1 {
		t.Fatalf("Attempts unchanged: got %d want 1", got.Attempts)
	}
	if got.Summary != "fixed it" {
		t.Fatalf("Summary: got %q want %q", got.Summary, "fixed it")
	}

	// done -> failed (with error)
	errMsg := "actually broke"
	if err := b.SetStatus(idea.ID, StatusFailed, SetStatusOpts{IncrementAttempts: true, LastError: &errMsg}); err != nil {
		t.Fatalf("SetStatus failed: %v", err)
	}
	got, err = b.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusFailed {
		t.Fatalf("Status: got %q want %q", got.Status, StatusFailed)
	}
	if got.Attempts != 2 {
		t.Fatalf("Attempts: got %d want 2", got.Attempts)
	}
	if got.LastError != "actually broke" {
		t.Fatalf("LastError: got %q want %q", got.LastError, "actually broke")
	}
}

func TestSetStatus_MissingIdea(t *testing.T) {
	b := newTestBoard(t)
	err := b.SetStatus("not-a-real-id", StatusDone, SetStatusOpts{})
	if err == nil {
		t.Fatalf("expected error when updating non-existent idea")
	}
}

func TestSetStatus_UpdatesTimestamp(t *testing.T) {
	b := newTestBoard(t)
	idea := &Idea{Title: "watch the clock"}
	if err := b.Insert(idea); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	before, err := b.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	time.Sleep(2 * time.Millisecond)
	if err := b.SetStatus(idea.ID, StatusAccepted, SetStatusOpts{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	after, err := b.Get(idea.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !after.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("UpdatedAt should advance: before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}
}

func TestList_FiltersByStatus(t *testing.T) {
	b := newTestBoard(t)

	must := func(idea *Idea) string {
		if err := b.Insert(idea); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		return idea.ID
	}
	id1 := must(&Idea{Title: "a", Severity: SevHigh})
	id2 := must(&Idea{Title: "b", Severity: SevMedium})
	id3 := must(&Idea{Title: "c", Severity: SevLow})

	// Move id2 to done so we can filter on it.
	if err := b.SetStatus(id2, StatusDone, SetStatusOpts{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	proposed, err := b.List(ListOptions{Status: StatusProposed})
	if err != nil {
		t.Fatalf("List proposed: %v", err)
	}
	if len(proposed) != 2 {
		t.Fatalf("proposed count: got %d want 2", len(proposed))
	}
	for _, i := range proposed {
		if i.ID != id1 && i.ID != id3 {
			t.Fatalf("unexpected idea in proposed list: %+v", i)
		}
	}

	done, err := b.List(ListOptions{Status: StatusDone})
	if err != nil {
		t.Fatalf("List done: %v", err)
	}
	if len(done) != 1 || done[0].ID != id2 {
		t.Fatalf("done filter: got %+v want id %q", done, id2)
	}
}

func TestList_FiltersBySeverity(t *testing.T) {
	b := newTestBoard(t)

	must := func(idea *Idea) {
		if err := b.Insert(idea); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}
	must(&Idea{Title: "high1", Severity: SevHigh})
	must(&Idea{Title: "high2", Severity: SevHigh})
	must(&Idea{Title: "med1", Severity: SevMedium})
	must(&Idea{Title: "low1", Severity: SevLow})
	must(&Idea{Title: "info1", Severity: SevInfo})

	highs, err := b.List(ListOptions{Severity: SevHigh})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(highs) != 2 {
		t.Fatalf("high count: got %d want 2", len(highs))
	}
	for _, i := range highs {
		if i.Severity != SevHigh {
			t.Fatalf("non-high idea in high-filter result: %+v", i)
		}
	}

	infos, err := b.List(ListOptions{Severity: SevInfo})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("info count: got %d want 1", len(infos))
	}
}

func TestList_FiltersByStatusAndSeverity(t *testing.T) {
	b := newTestBoard(t)

	id1 := mustInsert(t, b, &Idea{Title: "high-proposed", Severity: SevHigh})
	id2 := mustInsert(t, b, &Idea{Title: "high-done", Severity: SevHigh})
	mustInsert(t, b, &Idea{Title: "low-proposed", Severity: SevLow})

	if err := b.SetStatus(id2, StatusDone, SetStatusOpts{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	got, err := b.List(ListOptions{Status: StatusProposed, Severity: SevHigh})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != id1 {
		t.Fatalf("combined filter: got %+v want id %q", got, id1)
	}
}

func TestList_DefaultOrderPriorityFirst(t *testing.T) {
	b := newTestBoard(t)

	// Insert in non-priority order to confirm the SQL ORDER BY works.
	idLow := mustInsert(t, b, &Idea{Title: "low first", Severity: SevLow})
	idHigh := mustInsert(t, b, &Idea{Title: "high second", Severity: SevHigh})
	idMed := mustInsert(t, b, &Idea{Title: "med third", Severity: SevMedium})

	got, err := b.List(ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 ideas, got %d", len(got))
	}
	// All are proposed -> ordering should be high, medium, low.
	if got[0].ID != idHigh || got[1].ID != idMed || got[2].ID != idLow {
		t.Fatalf("default order should be by severity DESC: got %v %v %v",
			got[0].Title, got[1].Title, got[2].Title)
	}
}

func TestList_NewestOrder(t *testing.T) {
	b := newTestBoard(t)

	first := mustInsert(t, b, &Idea{Title: "first"})
	time.Sleep(2 * time.Millisecond)
	second := mustInsert(t, b, &Idea{Title: "second"})
	time.Sleep(2 * time.Millisecond)
	third := mustInsert(t, b, &Idea{Title: "third"})

	got, err := b.List(ListOptions{Newest: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 ideas, got %d", len(got))
	}
	if got[0].ID != third || got[1].ID != second || got[2].ID != first {
		t.Fatalf("newest-first order wrong: %v %v %v", got[0].Title, got[1].Title, got[2].Title)
	}
}

func TestNextActionable_PicksHighestSeverityOldest(t *testing.T) {
	b := newTestBoard(t)

	// All proposed: NextActionable should pick high before medium/low.
	idLow := mustInsert(t, b, &Idea{Title: "low", Severity: SevLow})
	time.Sleep(2 * time.Millisecond)
	idHigh := mustInsert(t, b, &Idea{Title: "high", Severity: SevHigh})
	time.Sleep(2 * time.Millisecond)
	mustInsert(t, b, &Idea{Title: "high-newer", Severity: SevHigh})

	picked, err := b.NextActionable()
	if err != nil {
		t.Fatalf("NextActionable: %v", err)
	}
	if picked.ID != idHigh {
		t.Fatalf("should pick the oldest high, got %+v", picked)
	}

	// Move high ones out, low should now be picked.
	_ = b.SetStatus(idHigh, StatusDone, SetStatusOpts{})
	picked2, err := b.NextActionable()
	if err != nil {
		t.Fatalf("NextActionable second: %v", err)
	}
	// One of the two remaining proposed ideas (high-newer or low) — high wins.
	if picked2.Severity != SevHigh && picked2.ID != idLow {
		t.Fatalf("unexpected second pick: %+v", picked2)
	}
}

func TestNextActionable_SkipsTerminalStatuses(t *testing.T) {
	b := newTestBoard(t)

	id := mustInsert(t, b, &Idea{Title: "doomed", Severity: SevHigh})
	if err := b.SetStatus(id, StatusDone, SetStatusOpts{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	_, err := b.NextActionable()
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows when backlog is empty, got %v", err)
	}
}

func TestNextActionable_IncludesAccepted(t *testing.T) {
	b := newTestBoard(t)

	id := mustInsert(t, b, &Idea{Title: "accepted", Severity: SevMedium})
	if err := b.SetStatus(id, StatusAccepted, SetStatusOpts{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	picked, err := b.NextActionable()
	if err != nil {
		t.Fatalf("NextActionable: %v", err)
	}
	if picked.ID != id {
		t.Fatalf("expected accepted idea %q, got %+v", id, picked)
	}
}

func TestStats_CountsByStatus(t *testing.T) {
	b := newTestBoard(t)

	id1 := mustInsert(t, b, &Idea{Title: "a"})
	id2 := mustInsert(t, b, &Idea{Title: "b"})
	id3 := mustInsert(t, b, &Idea{Title: "c"})
	mustInsert(t, b, &Idea{Title: "d"})

	if err := b.SetStatus(id1, StatusDone, SetStatusOpts{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := b.SetStatus(id2, StatusDone, SetStatusOpts{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := b.SetStatus(id3, StatusFailed, SetStatusOpts{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	stats, err := b.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats[StatusProposed] != 1 {
		t.Fatalf("proposed: got %d want 1", stats[StatusProposed])
	}
	if stats[StatusDone] != 2 {
		t.Fatalf("done: got %d want 2", stats[StatusDone])
	}
	if stats[StatusFailed] != 1 {
		t.Fatalf("failed: got %d want 1", stats[StatusFailed])
	}
}

func TestDelete_RemovesIdea(t *testing.T) {
	b := newTestBoard(t)
	id := mustInsert(t, b, &Idea{Title: "delete me"})

	if err := b.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := b.Get(id)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("after Delete: want sql.ErrNoRows, got %v", err)
	}
}

func TestDelete_MissingIdeaReturnsError(t *testing.T) {
	b := newTestBoard(t)
	if err := b.Delete("ghost-id"); err == nil {
		t.Fatalf("expected error for missing id")
	}
}

func TestGet_NotFound(t *testing.T) {
	b := newTestBoard(t)
	_, err := b.Get("nope")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func mustInsert(t *testing.T, b *Board, idea *Idea) string {
	t.Helper()
	if err := b.Insert(idea); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	return idea.ID
}
