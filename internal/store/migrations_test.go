package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// openRawDB returns a SQLite handle without running migrations, so individual
// migration tests can drive applyMigrations themselves.
func openRawDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)", filepath.Join(t.TempDir(), "test.db"))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		t.Fatalf("Ping: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// embeddedMigrationVersions returns the sorted version names (without the
// .sql suffix) of every migration shipped in the embedded FS.
func embeddedMigrationVersions(t *testing.T) []string {
	t.Helper()
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	versions := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		versions = append(versions, strings.TrimSuffix(e.Name(), ".sql"))
	}
	sort.Strings(versions)
	return versions
}

func TestApplyMigrations_AppliesAllInLexicalOrder(t *testing.T) {
	db := openRawDB(t)

	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}

	want := embeddedMigrationVersions(t)
	if len(want) == 0 {
		t.Fatalf("expected at least one embedded migration")
	}

	rows, err := db.Query(`SELECT version, applied_at FROM schema_migrations ORDER BY version ASC`)
	if err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	defer rows.Close()

	var got []string
	var prevApplied int64
	for rows.Next() {
		var v string
		var appliedAt int64
		if err := rows.Scan(&v, &appliedAt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if appliedAt <= 0 {
			t.Fatalf("applied_at for %s should be positive, got %d", v, appliedAt)
		}
		if appliedAt < prevApplied {
			t.Fatalf("applied_at for %s (%d) went backwards from previous (%d) — migrations not applied in lexical order",
				v, appliedAt, prevApplied)
		}
		prevApplied = appliedAt
		got = append(got, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("schema_migrations row count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("schema_migrations[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestApplyMigrations_Idempotent(t *testing.T) {
	db := openRawDB(t)

	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations (first): %v", err)
	}

	var firstCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&firstCount); err != nil {
		t.Fatalf("count first: %v", err)
	}

	var firstHead string
	row := db.QueryRow(`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`)
	if err := row.Scan(&firstHead); err != nil {
		t.Fatalf("head first: %v", err)
	}
	var firstAppliedAt int64
	if err := db.QueryRow(
		`SELECT applied_at FROM schema_migrations WHERE version = ?`, firstHead,
	).Scan(&firstAppliedAt); err != nil {
		t.Fatalf("first applied_at: %v", err)
	}

	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations (second): %v", err)
	}

	var secondCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&secondCount); err != nil {
		t.Fatalf("count second: %v", err)
	}
	if secondCount != firstCount {
		t.Fatalf("row count changed across re-apply: first=%d second=%d", firstCount, secondCount)
	}

	var secondAppliedAt int64
	if err := db.QueryRow(
		`SELECT applied_at FROM schema_migrations WHERE version = ?`, firstHead,
	).Scan(&secondAppliedAt); err != nil {
		t.Fatalf("second applied_at: %v", err)
	}
	if secondAppliedAt != firstAppliedAt {
		t.Fatalf("applied_at for %s rewritten on re-apply: first=%d second=%d",
			firstHead, firstAppliedAt, secondAppliedAt)
	}
}

func TestApplyMigrations_ResumesFromPartialState(t *testing.T) {
	db := openRawDB(t)

	versions := embeddedMigrationVersions(t)
	if len(versions) < 2 {
		t.Skip("need at least two migrations to test partial-state resume")
	}

	// Pre-populate schema_migrations with only the first version (and the
	// table it creates) to simulate an old database that has been partially
	// upgraded. applyMigrations should fill in the rest without retrying the
	// first one.
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	firstSQL, err := migrationsFS.ReadFile("migrations/" + versions[0] + ".sql")
	if err != nil {
		t.Fatalf("read first migration: %v", err)
	}
	if _, err := db.Exec(string(firstSQL)); err != nil {
		t.Fatalf("apply first migration manually: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO schema_migrations(version, applied_at) VALUES (?, 1)`, versions[0],
	); err != nil {
		t.Fatalf("seed schema_migrations: %v", err)
	}

	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}

	// The seeded applied_at sentinel (1) must be preserved — applyMigrations
	// must not have re-applied a version it already saw.
	var seededAppliedAt int64
	if err := db.QueryRow(
		`SELECT applied_at FROM schema_migrations WHERE version = ?`, versions[0],
	).Scan(&seededAppliedAt); err != nil {
		t.Fatalf("read seeded applied_at: %v", err)
	}
	if seededAppliedAt != 1 {
		t.Fatalf("applied_at for %s changed: got %d, want 1 (already-applied migration was re-run)",
			versions[0], seededAppliedAt)
	}

	// And every embedded version is now present.
	for _, v := range versions {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, v,
		).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", v, err)
		}
		if n != 1 {
			t.Fatalf("version %s row count = %d, want 1", v, n)
		}
	}
}

func TestSchemaHead_EmptyDB(t *testing.T) {
	db := openRawDB(t)
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}

	head, err := SchemaHead(db)
	if err != nil {
		t.Fatalf("SchemaHead: %v", err)
	}
	if head != "" {
		t.Fatalf("SchemaHead on empty table = %q, want \"\"", head)
	}
}

func TestSchemaHead_ReturnsHighestVersion(t *testing.T) {
	db := openRawDB(t)
	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}

	versions := embeddedMigrationVersions(t)
	wantHead := versions[len(versions)-1]

	head, err := SchemaHead(db)
	if err != nil {
		t.Fatalf("SchemaHead: %v", err)
	}
	if head != wantHead {
		t.Fatalf("SchemaHead = %q, want %q", head, wantHead)
	}
}

func TestApplyMigrations_CreatesExpectedTables(t *testing.T) {
	// Sanity check: after migrations, key tables referenced by other
	// packages should exist. If a future migration deletes one of these
	// without updating callers, this catches it cheaply.
	db := openRawDB(t)
	if err := applyMigrations(db); err != nil {
		t.Fatalf("applyMigrations: %v", err)
	}
	mustHave := []string{"sessions", "subtasks", "trajectory_events", "providers_seen", "ideas"}
	for _, table := range mustHave {
		var name string
		err := db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err == sql.ErrNoRows {
			t.Fatalf("expected table %q to exist after migrations", table)
		}
		if err != nil {
			t.Fatalf("query sqlite_master for %s: %v", table, err)
		}
	}
}
