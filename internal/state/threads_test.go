package state

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoad_MissingFileReturnsEmptyIndex(t *testing.T) {
	dir := t.TempDir()
	idx, err := Load(dir)
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if idx == nil {
		t.Fatal("Load missing returned nil index")
	}
	if idx.Version != currentVersion {
		t.Errorf("Version = %d, want %d", idx.Version, currentVersion)
	}
	if len(idx.Threads) != 0 {
		t.Errorf("expected zero threads, got %d", len(idx.Threads))
	}
	if idx.ActiveThread != "" {
		t.Errorf("expected no active thread, got %q", idx.ActiveThread)
	}
}

func TestNewSwitchListRoundtrip(t *testing.T) {
	dir := t.TempDir()
	idx, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dev, err := idx.New("dev-feature-x", "dev")
	if err != nil {
		t.Fatalf("New dev: %v", err)
	}
	ops, err := idx.New("ops-monthly-recap", "comms")
	if err != nil {
		t.Fatalf("New ops: %v", err)
	}
	if _, err := idx.SetActive(dev.Name); err != nil {
		t.Fatalf("SetActive dev: %v", err)
	}
	if err := Save(dir, idx); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	if got.ActiveThread != dev.ID {
		t.Errorf("ActiveThread = %q, want %q", got.ActiveThread, dev.ID)
	}
	if len(got.Threads) != 2 {
		t.Fatalf("len(Threads) = %d, want 2", len(got.Threads))
	}
	if got.Find("ops-monthly-recap") == nil {
		t.Error("ops thread missing after reload")
	}
	if got.Find(ops.ID) == nil {
		t.Error("ops thread not findable by id")
	}
	if act := got.Active(); act == nil || act.Mode != "dev" {
		t.Errorf("Active() mode = %v, want dev", act)
	}
}

func TestNew_DuplicateNameErrors(t *testing.T) {
	idx := &Index{}
	if _, err := idx.New("dev", ""); err != nil {
		t.Fatalf("first New: %v", err)
	}
	if _, err := idx.New("dev", ""); err == nil {
		t.Error("expected duplicate-name error, got nil")
	}
}

func TestNew_EmptyNameErrors(t *testing.T) {
	idx := &Index{}
	if _, err := idx.New("  ", ""); err == nil {
		t.Error("expected empty-name error, got nil")
	}
}

func TestSetActive_UnknownErrors(t *testing.T) {
	idx := &Index{}
	if _, err := idx.SetActive("nope"); err == nil {
		t.Error("expected unknown-thread error, got nil")
	}
}

func TestSetActive_TouchesLastUsed(t *testing.T) {
	idx := &Index{}
	t1, _ := idx.New("dev", "dev")
	old := time.Now().Add(-time.Hour)
	t1.LastUsedAt = old
	if _, err := idx.SetActive(t1.ID); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if !t1.LastUsedAt.After(old) {
		t.Errorf("SetActive did not touch LastUsedAt (still %v)", t1.LastUsedAt)
	}
}

func TestTouchActiveSession_UpdatesActive(t *testing.T) {
	dir := t.TempDir()
	idx, _ := Load(dir)
	dev, _ := idx.New("dev", "dev")
	if _, err := idx.SetActive(dev.ID); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if err := Save(dir, idx); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := TouchActiveSession(dir, "sess-123"); err != nil {
		t.Fatalf("TouchActiveSession: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	act := got.Active()
	if act == nil {
		t.Fatal("no active thread after touch")
	}
	if act.ActiveSessionID != "sess-123" {
		t.Errorf("ActiveSessionID = %q, want %q", act.ActiveSessionID, "sess-123")
	}
}

func TestTouchActiveSession_NoActiveIsNoop(t *testing.T) {
	dir := t.TempDir()
	// No threads, no active. Should not error, should not create a
	// state.json (nothing to record).
	if err := TouchActiveSession(dir, "sess-xyz"); err != nil {
		t.Fatalf("TouchActiveSession on empty: %v", err)
	}
	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Errorf("state.json was created even though no active thread was set: stat err=%v", err)
	}
}

func TestTouchActiveSession_EmptyArgsAreNoop(t *testing.T) {
	if err := TouchActiveSession("", "sess"); err != nil {
		t.Errorf("empty stateDir: %v", err)
	}
	dir := t.TempDir()
	if err := TouchActiveSession(dir, ""); err != nil {
		t.Errorf("empty sessionID: %v", err)
	}
}

func TestSave_AtomicNoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	idx, _ := Load(dir)
	if _, err := idx.New("t1", ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := Save(dir, idx); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestLoad_MalformedErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, Filename), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("expected parse error on malformed state.json, got nil")
	}
}

func TestPath_Format(t *testing.T) {
	if got := Path("/tmp/x"); got != "/tmp/x/"+Filename {
		t.Errorf("Path = %q", got)
	}
}

// TestSave_OrphanTmpDoesNotCorruptLoad simulates a crash mid-write: an
// orphaned .tmp-* file sits alongside the real state.json. Atomic-rename
// means the real file is untouched, so the next Load must succeed and
// return the pre-crash state. The orphan persists (cleanup is a separate
// concern) but it must not be picked up as if it were state.json.
func TestSave_OrphanTmpDoesNotCorruptLoad(t *testing.T) {
	dir := t.TempDir()
	idx, _ := Load(dir)
	dev, err := idx.New("dev", "dev")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := idx.SetActive(dev.ID); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	if err := Save(dir, idx); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Simulate a process killed *between* writing the tmp file and
	// renaming it into place: leave a malformed sibling tmp file.
	orphan := filepath.Join(dir, Filename+".tmp-orphan")
	if err := os.WriteFile(orphan, []byte("{partial"), 0o644); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load after orphan: %v", err)
	}
	if act := got.Active(); act == nil || act.Name != "dev" {
		t.Errorf("active thread lost after orphan tmp: %+v", act)
	}

	// And a fresh Save still succeeds.
	if _, err := got.New("ops", "comms"); err != nil {
		t.Fatalf("New ops: %v", err)
	}
	if err := Save(dir, got); err != nil {
		t.Fatalf("re-Save: %v", err)
	}
	again, err := Load(dir)
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	if len(again.Threads) != 2 {
		t.Errorf("after re-Save expected 2 threads, got %d", len(again.Threads))
	}
}

// TestSave_ConcurrentWritersNeverCorrupt exercises the documented
// concurrency contract for state.json: two uta processes saving at the
// same time will race for the final rename, but atomic-rename means the
// resulting file is always a complete, parseable Index — never a torn
// write. Last-writer-wins is a known limitation (the loser's update is
// lost), but the file is never garbage.
func TestSave_ConcurrentWritersNeverCorrupt(t *testing.T) {
	dir := t.TempDir()

	// Seed with a known starting index so both writers start from the
	// same baseline.
	base, _ := Load(dir)
	if _, err := base.New("seed", "dev"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Save(dir, base); err != nil {
		t.Fatalf("Save seed: %v", err)
	}

	const writers = 8
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			idx, err := Load(dir)
			if err != nil {
				t.Errorf("worker %d Load: %v", i, err)
				return
			}
			// Each worker tries to add its own thread. Duplicate-name
			// errors are expected when two workers Load the same view
			// and one already added; that's part of the last-writer-
			// wins race and not a corruption signal.
			_, _ = idx.New("worker-"+string(rune('a'+i)), "dev")
			if err := Save(dir, idx); err != nil {
				t.Errorf("worker %d Save: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// The file must always be parseable and contain at least the seed.
	final, err := Load(dir)
	if err != nil {
		t.Fatalf("Load after concurrent writes: %v", err)
	}
	if final.Find("seed") == nil {
		t.Error("seed thread lost — concurrent writers corrupted file")
	}

	// No leftover .tmp-* sidecars (the atomic-write contract).
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp file left behind after concurrent writes: %s", e.Name())
		}
	}
}
