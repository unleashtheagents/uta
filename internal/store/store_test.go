package store

import (
	"path/filepath"
	"testing"
)

func TestOpen_CreatesFileAndSetsPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if s.DB == nil {
		t.Fatal("Store.DB is nil")
	}
	if s.Path != path {
		t.Fatalf("Store.Path = %q, want %q", s.Path, path)
	}
	if err := s.DB.Ping(); err != nil {
		t.Fatalf("DB.Ping: %v", err)
	}
}

func TestOpen_AppliesPragmas(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var journalMode string
	if err := s.DB.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want %q", journalMode, "wal")
	}

	var busyTimeout int
	if err := s.DB.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}

	var foreignKeys int
	if err := s.DB.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}
}

func TestOpen_AppliesMigrations(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	head, err := SchemaHead(s.DB)
	if err != nil {
		t.Fatalf("SchemaHead: %v", err)
	}
	if head == "" {
		t.Fatal("SchemaHead is empty after Open — migrations were not applied")
	}
}

func TestOpen_ReusesExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open (first): %v", err)
	}
	if _, err := s1.DB.Exec(
		`INSERT INTO sessions(id, goal, worker, planner, status, created_at, workflow_path, meta_json) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"sess-reopen", "g", "claude", "claude", "running", 1, "", "{}",
	); err != nil {
		t.Fatalf("seed sessions: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("Open (second): %v", err)
	}
	defer s2.Close()

	var n int
	if err := s2.DB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id = ?`, "sess-reopen").Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("re-opened DB lost row: count = %d, want 1", n)
	}
}

func TestOpen_InvalidPath(t *testing.T) {
	// Path under a non-existent directory — SQLite cannot create the file.
	bogus := filepath.Join(t.TempDir(), "does", "not", "exist", "test.db")
	s, err := Open(bogus)
	if err == nil {
		s.Close()
		t.Fatal("Open with bogus path: expected error, got nil")
	}
}

func TestClose_Idempotent(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}
	// Operations on a closed DB should fail, confirming Close actually closed.
	if err := s.DB.Ping(); err == nil {
		t.Fatal("DB.Ping after Close: expected error, got nil")
	}
}
