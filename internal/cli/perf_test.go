package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/unleashtheagents/uta/internal/store"
)

// TestPickPerfSessions covers the time-window and mode filter behavior
// behind `uta perf --since <duration> --mode <name>`. The audit flagged
// this as untested — the filter is plain enough but the in-place
// reslicing pattern (out := all[:0]) is the kind of code that breaks
// silently if the order assumption changes.
func TestPickPerfSessions(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "perf.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer s.Close()

	now := time.Now()
	mkSession := func(id string, mode string, age time.Duration) {
		t.Helper()
		err := s.CreateSession(store.Session{
			ID:        id,
			Goal:      "test",
			Status:    "completed",
			CreatedAt: now.Add(-age),
			ModeName:  mode,
		})
		if err != nil {
			t.Fatalf("CreateSession(%s): %v", id, err)
		}
	}

	mkSession("recent-dev", "dev", 1*time.Hour)
	mkSession("recent-ops", "ops", 2*time.Hour)
	mkSession("old-dev", "dev", 48*time.Hour)
	mkSession("recent-nomode", "", 30*time.Minute)

	// Window = 24h. Should exclude "old-dev" only.
	cutoff := now.Add(-24 * time.Hour)
	all, err := pickPerfSessions(s, "", cutoff, 100)
	if err != nil {
		t.Fatalf("pickPerfSessions: %v", err)
	}
	if got, want := len(all), 3; got != want {
		t.Errorf("len(all sessions within 24h): got %d want %d (%+v)", got, want, sessionIDs(all))
	}
	for _, sess := range all {
		if sess.ID == "old-dev" {
			t.Errorf("expected old-dev to be filtered out, got it back")
		}
	}

	// Mode filter "dev" + window. Should yield only "recent-dev".
	devOnly, err := pickPerfSessions(s, "dev", cutoff, 100)
	if err != nil {
		t.Fatalf("pickPerfSessions(dev): %v", err)
	}
	if got, want := len(devOnly), 1; got != want {
		t.Fatalf("len(dev within 24h): got %d want %d (%+v)", got, want, sessionIDs(devOnly))
	}
	if devOnly[0].ID != "recent-dev" {
		t.Errorf("dev[0]: got %q want recent-dev", devOnly[0].ID)
	}

	// Empty window — cutoff in the future — should match nothing.
	none, err := pickPerfSessions(s, "", now.Add(1*time.Hour), 100)
	if err != nil {
		t.Fatalf("pickPerfSessions(future cutoff): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("future cutoff should return 0 sessions, got %d (%+v)", len(none), sessionIDs(none))
	}

	// Mode that doesn't exist must return empty, not error.
	missing, err := pickPerfSessions(s, "no-such-mode", cutoff, 100)
	if err != nil {
		t.Fatalf("pickPerfSessions(no-such-mode): %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("unknown mode should return 0 sessions, got %d", len(missing))
	}
}

func sessionIDs(ss []store.Session) []string {
	ids := make([]string, len(ss))
	for i, s := range ss {
		ids[i] = s.ID
	}
	return ids
}

func TestFormatUSDCents(t *testing.T) {
	cases := []struct {
		cents int64
		want  string
	}{
		{0, "$0.00"},
		{5, "$0.05"},
		{99, "$0.99"},
		{100, "$1.00"},
		{12345, "$123.45"},
		{-50, "-$0.50"},
	}
	for _, c := range cases {
		if got := formatUSDCents(c.cents); got != c.want {
			t.Errorf("formatUSDCents(%d) = %q, want %q", c.cents, got, c.want)
		}
	}
}
