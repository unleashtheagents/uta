package whiteboard

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSet_GetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	wb := New(s.DB)

	id, err := wb.Set(Entry{
		Key:           "blocker:auth-rewrite",
		ValueJSON:     `{"note":"don't merge until legal signs off"}`,
		AuthorMode:    "ops",
		AuthorSession: "sess-1",
	})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if id <= 0 {
		t.Fatalf("expected positive id, got %d", id)
	}

	got, found, err := wb.Get("blocker:auth-rewrite")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found {
		t.Fatal("expected entry to be found")
	}
	if got.Key != "blocker:auth-rewrite" {
		t.Fatalf("Key: got %q", got.Key)
	}
	if got.AuthorMode != "ops" {
		t.Fatalf("AuthorMode: got %q", got.AuthorMode)
	}
	if got.AuthorSession != "sess-1" {
		t.Fatalf("AuthorSession: got %q", got.AuthorSession)
	}
	if got.ValueJSON == "" {
		t.Fatal("ValueJSON empty")
	}
}

func TestGet_MissingKey(t *testing.T) {
	s := newTestStore(t)
	wb := New(s.DB)
	_, found, err := wb.Get("nope")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if found {
		t.Fatal("expected found=false for missing key")
	}
}

func TestSet_RejectsInvalidJSON(t *testing.T) {
	s := newTestStore(t)
	wb := New(s.DB)
	if _, err := wb.Set(Entry{Key: "k", ValueJSON: "not json"}); err == nil {
		t.Fatal("expected invalid-JSON error")
	}
}

func TestSet_RequiresKeyAndValue(t *testing.T) {
	s := newTestStore(t)
	wb := New(s.DB)
	if _, err := wb.Set(Entry{Key: "", ValueJSON: `"v"`}); err == nil {
		t.Fatal("expected key-required error")
	}
	if _, err := wb.Set(Entry{Key: "k", ValueJSON: ""}); err == nil {
		t.Fatal("expected value-required error")
	}
}

func TestSet_AppendOnlyLatestWins(t *testing.T) {
	s := newTestStore(t)
	wb := New(s.DB)
	t0 := time.Unix(1_700_000_000, 0)
	if _, err := wb.Set(Entry{Key: "k", ValueJSON: `"v1"`, AuthorMode: "ops", Ts: t0}); err != nil {
		t.Fatalf("Set v1: %v", err)
	}
	if _, err := wb.Set(Entry{Key: "k", ValueJSON: `"v2"`, AuthorMode: "dev", Ts: t0.Add(time.Second)}); err != nil {
		t.Fatalf("Set v2: %v", err)
	}
	got, _, err := wb.Get("k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ValueJSON != `"v2"` || got.AuthorMode != "dev" {
		t.Fatalf("expected latest entry v2/dev, got %q/%q", got.ValueJSON, got.AuthorMode)
	}
	hist, err := wb.History("k")
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("History len: got %d want 2", len(hist))
	}
	if hist[0].ValueJSON != `"v1"` || hist[1].ValueJSON != `"v2"` {
		t.Fatalf("History order: got %+v", hist)
	}
}

func TestList_LatestPerKey(t *testing.T) {
	s := newTestStore(t)
	wb := New(s.DB)
	t0 := time.Unix(1_700_000_000, 0)
	if _, err := wb.Set(Entry{Key: "a", ValueJSON: `1`, AuthorMode: "ops", Ts: t0}); err != nil {
		t.Fatal(err)
	}
	if _, err := wb.Set(Entry{Key: "b", ValueJSON: `2`, AuthorMode: "ops", Ts: t0.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := wb.Set(Entry{Key: "a", ValueJSON: `3`, AuthorMode: "dev", Ts: t0.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	entries, err := wb.List(0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("List len: got %d want 2", len(entries))
	}
	// newest first: a@t0+2 (value 3) then b@t0+1 (value 2).
	if entries[0].Key != "a" || entries[0].ValueJSON != `3` || entries[0].AuthorMode != "dev" {
		t.Fatalf("entries[0]: got %+v", entries[0])
	}
	if entries[1].Key != "b" || entries[1].ValueJSON != `2` {
		t.Fatalf("entries[1]: got %+v", entries[1])
	}
}

func TestList_LimitCap(t *testing.T) {
	s := newTestStore(t)
	wb := New(s.DB)
	t0 := time.Unix(1_700_000_000, 0)
	for i, k := range []string{"a", "b", "c"} {
		if _, err := wb.Set(Entry{Key: k, ValueJSON: `1`, Ts: t0.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := wb.List(2)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected limit cap of 2, got %d", len(entries))
	}
}

func TestStore_NilDBSafeReads(t *testing.T) {
	var wb *Store
	if _, found, err := wb.Get("k"); err != nil || found {
		t.Fatalf("nil Store Get: err=%v found=%v", err, found)
	}
	entries, err := wb.List(0)
	if err != nil || entries != nil {
		t.Fatalf("nil Store List: err=%v entries=%v", err, entries)
	}
}
