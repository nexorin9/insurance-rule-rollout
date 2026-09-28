// MySQL adapter for the HIS dict table.
//
// Driver registration is the caller's responsibility. The expected
// pattern is:
//
//	import _ "github.com/go-sql-driver/mysql"
//	c, err := his_dict.NewMySQLClient("user:pass@tcp(host:3306)/his?parseTime=true&charset=utf8mb4")
//
// The driver name "mysql" is the one registered by go-sql-driver/mysql —
// if a different driver is registered under the same name, this
// constructor still works as long as it understands the same DSN.
//
// Why we don't import the driver here:
//
//   - The demo binary does not need a MySQL driver (it uses the SQLite
//     substitute). Importing go-sql-driver/mysql unconditionally would
//     force a `go mod tidy` against a vendor we don't ship.
//   - Production binaries wire the driver at their own build tag (e.g.
//     `-tags=his_mysql`). Keeping this file driver-free means the same
//     compile works in both layouts.
package his_dict

import (
	"database/sql"
	"fmt"
)

// MySQLClient is a thin alias over Client that pins the flavor so call
// sites can do `mysql := his_dict.NewMySQLClient(dsn)` and still get a
// DictClient back. The struct adds nothing over Client today; keep the
// type separate to leave room for MySQL-only options (multi-statements,
// charset negotiation) without disturbing the PostgreSQL / SQLite paths.
type MySQLClient struct {
	*Client
}

// NewMySQLClient opens a MySQL database and returns a MySQLClient. The
// supplied DSN should follow the go-sql-driver/mysql format. Empty DSN
// returns an error so misconfiguration fails loudly.
//
// The MySQL dict table is expected to already exist (DBA-managed).
// EnsureSchema is NOT called here — production DBA owns schema.
func NewMySQLClient(dsn string) (*MySQLClient, error) {
	return openMySQL(dsn, "mysql")
}

// openMySQL is the shared constructor body — split out so a build-tagged
// variant (cmd/demo production wiring) can supply a different driver
// name without touching this file.
func openMySQL(dsn, driverName string) (*MySQLClient, error) {
	if dsn == "" {
		return nil, fmt.Errorf("his_dict: MySQL DSN is empty")
	}
	db, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, &TransactionError{Op: "open", Cause: err}
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, &TransactionError{Op: "ping", Cause: err}
	}
	// MySQL tolerates a wider pool than SQLite; cap to a modest number so
	// we do not exhaust the hospital's connection ceiling during a hot
	// release window.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	return &MySQLClient{
		Client: &Client{db: db, driver: driverName, flavor: FlavorMySQL, charset: "utf8mb4"},
	}, nil
}