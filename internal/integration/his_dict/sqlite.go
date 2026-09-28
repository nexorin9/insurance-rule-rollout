// SQLite adapter for the HIS dict table. The SQLite path serves two
// roles:
//
//  1. Demo / local development — the user runs cmd/demo without a real
//     HIS to verify the workflow surface.
//  2. Test double for production backends — the same transaction
//     semantics (BEGIN / UPDATE / INSERT / COMMIT / ROLLBACK) hold, so
//     tests that exercise ApplyUpdate against SQLite give high
//     confidence the MySQL / PostgreSQL paths behave identically.
//
// DSN shape (mattn/go-sqlite3):
//
//	":memory:"                                       — private in-memory
//	"file::memory:?cache=shared"                    — shared in-memory
//	"file:/path/to/his.db?_busy_timeout=5000"       — on-disk
//
// The shared in-memory form is what cmd/demo's integration test uses.
package his_dict

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3" // SQLite driver — already in go.mod
)

// SQLiteClient is the test/demo adapter. It calls EnsureSchema on
// construction so callers do not have to manage CREATE TABLE statements
// themselves.
type SQLiteClient struct {
	*Client
}

// NewSQLiteClient opens a SQLite database pointed at by dsn, creates the
// his_dict / his_audit_log tables if missing, and returns a SQLiteClient.
// Empty DSN is an error.
func NewSQLiteClient(dsn string) (*SQLiteClient, error) {
	if dsn == "" {
		return nil, fmt.Errorf("his_dict: SQLite DSN is empty")
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, &TransactionError{Op: "open", Cause: err}
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, &TransactionError{Op: "ping", Cause: err}
	}
	// SQLite is single-writer; cap pool to 1 to avoid "database is
	// locked" errors during parallel ApplyUpdate calls.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	client := &Client{db: db, driver: "sqlite3", flavor: FlavorSQLite}
	if err := client.EnsureSchema(sqlCtx()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &SQLiteClient{Client: client}, nil
}