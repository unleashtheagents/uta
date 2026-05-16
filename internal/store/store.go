// Package store is the SQLite persistence layer for sessions, subtasks, and
// trajectory events. The driver is the pure-Go modernc.org/sqlite so the
// binary stays static (CGO_ENABLED=0).
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

type Store struct {
	DB   *sql.DB
	Path string
}

// Open opens (or creates) the SQLite database at path, enables WAL, applies
// any pending migrations, and returns the wrapped handle. Caller owns Close.
func Open(path string) (*Store, error) {
	// modernc.org/sqlite uses driver name "sqlite". Pragmas via DSN.
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := applyMigrations(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: db, Path: path}, nil
}

func (s *Store) Close() error { return s.DB.Close() }
