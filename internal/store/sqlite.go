// Package store is the SQLite-backed Plan / Issue / AuditEvent persistence
// layer for insurance-rule-rollout. Task 3 ships the happy-path CRUD surface;
// the transactional SELECT FOR UPDATE gate is layered in Task 4 alongside
// the workflow.Store interface.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed migrations/0001_init.sql
var migration0001 string

// Open opens (or creates) a SQLite database pointed at by dsn. The expected
// DSN forms are:
//
//	":memory:"                                   — private in-memory, single connection
//	"file::memory:?cache=shared"                — shared in-memory across goroutines
//	"file:/path/to/state.db?cache=shared&_busy_timeout=5000" — on-disk with concurrency
//
// Production deployments should pass the on-disk form; the demo uses
// ":memory:" because the AuditEvent JSONL path is the real durability story.
func Open(dsn string) (*Store, error) {
	if dsn == "" {
		return nil, fmt.Errorf("store: empty dsn")
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping sqlite: %w", err)
	}
	// SQLite is single-writer; bound the pool so we don't accidentally fan
	// out parallel writers from the demo and immediately lock the file.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return &Store{db: db}, nil
}

// Migrate runs the 0001_init.sql migration. CREATE TABLE / CREATE INDEX use
// IF NOT EXISTS so re-invoking Migrate is idempotent — covered by
// store_test.go's TestMigrateIdempotent.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, migration0001); err != nil {
		return fmt.Errorf("store: migrate 0001_init: %w", err)
	}
	return nil
}

// Close releases the underlying SQLite handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB exposes the raw *sql.DB for callers that need to drive a transaction
// directly (Task 4's WithTx / LockPlan land in store.go). Production code
// should prefer the typed methods on Store.
func (s *Store) DB() *sql.DB { return s.db }
