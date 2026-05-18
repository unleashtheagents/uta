package trajectory

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/store"
)

func TestRecorder_LastError_NilByDefault(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	r := NewRecorder(s)
	if got := r.LastError(); got != nil {
		t.Fatalf("LastError() = %v, want nil", got)
	}
}

func TestRecorder_LastError_CapturedOnInsertFailure(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	// Close the DB so any subsequent InsertEvent fails.
	s.Close()

	r := NewRecorder(s)
	r.PersistSync(Event{
		SessionID: "sess-x",
		Ts:        time.Unix(1_700_000_000, 0).UTC(),
		Kind:      Kind("test"),
	})

	if got := r.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}
	if got := r.LastError(); got == nil {
		t.Fatalf("LastError() = nil, want non-nil after InsertEvent failure")
	}
}
