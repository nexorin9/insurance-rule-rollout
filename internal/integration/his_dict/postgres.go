// PostgreSQL adapter for the HIS dict table. Identical surface to the
// MySQL adapter (DictClient + ApplyUpdate / ReadSchema / DryRunScan) so
// the workflow engine and cmd/demo don't care which backend the hospital
// uses.
//
// Driver registration is the caller's responsibility:
//
//	import _ "github.com/jackc/pgx/v5/stdlib"
//	c, err := his_dict.NewPostgresClient("postgres://user:pass@host:5432/his?sslmode=disable")
//
// Production deployments should use sslmode=require / verify-full and
// supply a client cert; the constructor leaves TLS entirely to the DSN.
//
// As with mysql.go, the driver is not imported here so the default
// build (which uses SQLite for tests) does not force a vendor pull.
package his_dict

import (
	"database/sql"
	"fmt"
)

// PostgresClient is a thin alias over Client (same rationale as
// MySQLClient — keeps room for PG-only tuning later).
type PostgresClient struct {
	*Client
}

// NewPostgresClient opens a PostgreSQL database and returns a
// PostgresClient. Empty DSN returns an error.
//
// Production schema is DBA-managed; EnsureSchema is not called here.
// Task 25 wires testcontainers-go to spin up a real PG container for
// integration tests; this file just holds the production path.
func NewPostgresClient(dsn string) (*PostgresClient, error) {
	return openPostgres(dsn, "pgx")
}

// openPostgres is the shared constructor body — same pattern as
// openMySQL; the build-tagged variant lets the production binary wire a
// custom driver name (e.g. "postgres" for lib/pq).
func openPostgres(dsn, driverName string) (*PostgresClient, error) {
	if dsn == "" {
		return nil, fmt.Errorf("his_dict: PostgreSQL DSN is empty")
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, &TransactionError{Op: "open", Cause: err}
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, &TransactionError{Op: "ping", Cause: err}
	}
	// Postgres handles a deeper pool cleanly; cap modestly.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	return &PostgresClient{
		Client: &Client{db: db, driver: driverName, flavor: FlavorPostgres},
	}, nil
}