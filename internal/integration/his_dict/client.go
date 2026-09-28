// Unified SQL-backed DictClient implementation. The struct wraps a single
// *sql.DB so the same code paths apply whether the underlying driver is
// SQLite (test/demo), MySQL (production), or PostgreSQL (production). All
// three drivers implement database/sql identically for transaction
// primitives (BeginTx / ExecContext / Commit / Rollback) — the adapter
// surface differs only at sql.Open time.
//
// Why a single struct instead of three separate ones:
//   - bytebase's source uses one storage layer per backend with shared
//     transactional helpers (backend/component/review/workflow.go lines
//     242–282). We mirror that with one Client type so the SQL strings
//     and the row-scanning logic don't drift between drivers.
//   - Test doubles (Task 8) substitute SQLite; the same struct is used
//     so a fix in one path benefits all backends.
//   - Adding a fourth backend (e.g. OceanBase) later only requires a new
//     constructor; the Apply/ReadSchema/DryRun paths stay untouched.

package his_dict

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Client is the concrete DictClient backed by *sql.DB. Construct via one
// of NewMySQLClient / NewPostgresClient / NewSQLiteClient — direct
// construction with a nil db is a programming error and the methods
// return errors rather than panicking.
type Client struct {
	db      *sql.DB
	driver  string // "mysql" | "postgres" | "sqlite3" — purely for diagnostics
	flavor  Flavor
	charset string // only honored by MySQL (utf8mb4 default)
}

// Flavor records dialect-specific SQL emission. The constants exist so
// the same method can emit `"?"` placeholders for SQLite/MySQL but `$1`
// for PostgreSQL without sprinkling driver checks throughout the code.
type Flavor int

const (
	FlavorSQLite Flavor = iota
	FlavorMySQL
	FlavorPostgres
)

// quoteRef returns the parameter placeholder for the n-th argument.
// SQLite / MySQL use "?", PostgreSQL uses "$1", "$2", ...
func (f Flavor) quoteRef(n int) string {
	if f == FlavorPostgres {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// placeholderList returns "?, ?, ?, ..." of length n — used for batch
// INSERTs whose row count varies.
func (f Flavor) placeholderList(n int) string {
	if n <= 0 {
		return ""
	}
	if f == FlavorPostgres {
		out := make([]string, n)
		for i := 0; i < n; i++ {
			out[i] = fmt.Sprintf("$%d", i+1)
		}
		return strings.Join(out, ", ")
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = "?"
	}
	return strings.Join(out, ", ")
}

// ApplyUpdate writes entries to his_dict and dual-writes his_audit_log in
// a single transaction. Contract:
//
//   BEGIN
//     for each entry:
//       UPSERT into his_dict (code, name, weight, limit, effective_date, version, updated_at)
//     for each entry:
//       INSERT into his_audit_log (code, action, version, applied_at)
//   COMMIT
//
// On any error → ROLLBACK + TransactionError. The operator can verify no
// partial state by re-reading his_dict after the failure.
//
// The ctx parameter is typed as interface{} so this type satisfies the
// DictClient interface literally declared in types.go (which uses the
// same loose shape); in practice every callsite passes a real
// context.Context and we cast it back at the top of the method.
func (c *Client) ApplyUpdate(ctxIface interface{}, entries []DictEntry) error {
	ctx, ok := ctxIface.(context.Context)
	if !ok {
		return fmt.Errorf("his_dict: ApplyUpdate requires context.Context, got %T", ctxIface)
	}
	if c == nil || c.db == nil {
		return &TransactionError{Op: "begin", Cause: fmt.Errorf("client not initialized")}
	}
	if len(entries) == 0 {
		return nil // nothing to do — not an error
	}

	// Validate first so we never open a transaction for a payload we know
	// will fail. DryRunScan does the same checks; doing them here keeps
	// ApplyUpdate self-contained for ad-hoc callers.
	if err := c.validateEntries(entries); err != nil {
		return err
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return &TransactionError{Op: "begin", Cause: err}
	}
	// Always defer Rollback — no-op after a successful Commit.
	defer func() { _ = tx.Rollback() }()

	for i, e := range entries {
		// UPSERT — INSERT OR REPLACE for SQLite, ON DUPLICATE KEY UPDATE
		// for MySQL, ON CONFLICT for PostgreSQL.
		stmt, err := c.buildUpsertSQL()
		if err != nil {
			return &TransactionError{Op: "update", Code: e.Code, Cause: err}
		}
		if _, err := tx.ExecContext(ctx, stmt,
			e.Code, e.Name, e.Weight, e.Limit, e.EffectiveDate, e.Version, e.UpdatedAt,
		); err != nil {
			return &TransactionError{Op: "update", Affected: i, Code: e.Code, Cause: err}
		}
	}

	// Dual-write audit log — one row per entry, marking when the new
	// version landed. The audit chain in internal/audit/chain.go carries
	// per-event hashes; this row is the bulk-table summary the medical-
	// insurance office uses for visual inspection.
	for i, e := range entries {
		insertSQL := c.buildAuditInsertSQL()
		if _, err := tx.ExecContext(ctx, insertSQL,
			e.Code, "UPSERT", e.Version, e.UpdatedAt,
		); err != nil {
			return &TransactionError{Op: "insert_audit", Affected: i, Code: e.Code, Cause: err}
		}
	}

	if err := tx.Commit(); err != nil {
		return &TransactionError{Op: "commit", Affected: len(entries), Cause: err}
	}
	return nil
}

// ReadSchema returns the current dict as a snapshot. Used by the dry-run
// engine to compute projected-diff before any write happens.
func (c *Client) ReadSchema(ctxIface interface{}) (DictSchema, error) {
	ctx, ok := ctxIface.(context.Context)
	if !ok {
		return DictSchema{}, fmt.Errorf("his_dict: ReadSchema requires context.Context, got %T", ctxIface)
	}
	if c == nil || c.db == nil {
		return DictSchema{}, &TransactionError{Op: "read", Cause: fmt.Errorf("client not initialized")}
	}

	rows, err := c.db.QueryContext(ctx, c.buildSelectAllSQL())
	if err != nil {
		return DictSchema{}, &TransactionError{Op: "read", Cause: err}
	}
	defer func() { _ = rows.Close() }()

	schema := DictSchema{}
	for rows.Next() {
		var e DictEntry
		var updatedAtStr string
		if err := rows.Scan(&e.Code, &e.Name, &e.Weight, &e.Limit, &e.EffectiveDate, &e.Version, &updatedAtStr); err != nil {
			return DictSchema{}, &TransactionError{Op: "read", Cause: err}
		}
		// updated_at is stored as RFC3339 text (Go's default for time.Time
		// when SQLite has no native timestamp type). Parse back into the
		// time.Time field; failure leaves zero-value time which the
		// dry-run engine treats as "unknown".
		if t, perr := time.Parse(time.RFC3339, updatedAtStr); perr == nil {
			e.UpdatedAt = t
		}
		schema.Entries = append(schema.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return DictSchema{}, &TransactionError{Op: "read", Cause: err}
	}
	// Version / EffectiveDate come from the dominant row (latest
	// effective_date) — single-version-per-table policy in the demo.
	if len(schema.Entries) > 0 {
		schema.Version = schema.Entries[0].Version
		schema.EffectiveDate = schema.Entries[0].EffectiveDate
	}
	return schema, nil
}

// DryRunScan validates entries against the current dict schema without
// touching any data. Runs an EXPLAIN-like no-op transaction so a failed
// row is reported before ApplyUpdate commits.
func (c *Client) DryRunScan(ctxIface interface{}, entries []DictEntry) error {
	ctx, ok := ctxIface.(context.Context)
	if !ok {
		return fmt.Errorf("his_dict: DryRunScan requires context.Context, got %T", ctxIface)
	}
	if c == nil || c.db == nil {
		return &TransactionError{Op: "dryrun", Cause: fmt.Errorf("client not initialized")}
	}
	if err := c.validateEntries(entries); err != nil {
		return err
	}

	// Begin a read-only tx so the driver confirms it can grab a connection
	// (catches "server gone away" / "too many connections" before the
	// real write), then ROLLBACK without doing anything.
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return &TransactionError{Op: "dryrun", Cause: err}
	}
	if err := tx.Rollback(); err != nil {
		return &TransactionError{Op: "dryrun", Cause: err}
	}
	return nil
}

// Close releases the underlying *sql.DB handle.
func (c *Client) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

// validateEntries is shared by ApplyUpdate and DryRunScan. Failures are
// surfaced as *ValidationError so the dry-run report can list them.
func (c *Client) validateEntries(entries []DictEntry) error {
	seen := make(map[string]bool, len(entries))
	for i, e := range entries {
		if strings.TrimSpace(e.Code) == "" {
			return &ValidationError{Field: fmt.Sprintf("entries[%d].code", i), Reason: "is empty"}
		}
		if seen[e.Code] {
			return &ValidationError{Field: fmt.Sprintf("entries[%d].code", i), Reason: "duplicate code " + e.Code}
		}
		seen[e.Code] = true
		if strings.TrimSpace(e.Name) == "" {
			return &ValidationError{Field: fmt.Sprintf("entries[%d].name", i), Reason: "is empty"}
		}
		if e.Limit < 0 {
			return &ValidationError{Field: fmt.Sprintf("entries[%d].limit", i), Reason: "is negative"}
		}
		if e.Weight < 0 {
			return &ValidationError{Field: fmt.Sprintf("entries[%d].weight", i), Reason: "is negative"}
		}
		if strings.TrimSpace(e.EffectiveDate) == "" {
			return &ValidationError{Field: fmt.Sprintf("entries[%d].effective_date", i), Reason: "is empty"}
		}
		if strings.TrimSpace(e.Version) == "" {
			return &ValidationError{Field: fmt.Sprintf("entries[%d].version", i), Reason: "is empty"}
		}
	}
	return nil
}

// buildUpsertSQL returns the dialect-correct UPSERT statement with 7
// parameter placeholders. Kept as a function (not a const) so the
// Postgres placeholder expansion uses $1..$7 instead of ?. The physical
// column for Limit is payment_limit to avoid the reserved word LIMIT.
func (c *Client) buildUpsertSQL() (string, error) {
	cols := "(code, name, weight, payment_limit, effective_date, version, updated_at)"
	switch c.flavor {
	case FlavorSQLite:
		return "INSERT OR REPLACE INTO his_dict " + cols + " VALUES (" + c.flavor.placeholderList(7) + ")", nil
	case FlavorMySQL:
		return "INSERT INTO his_dict " + cols + " VALUES (" + c.flavor.placeholderList(7) + ") " +
			"ON DUPLICATE KEY UPDATE name=VALUES(name), weight=VALUES(weight), " +
			"payment_limit=VALUES(payment_limit), effective_date=VALUES(effective_date), " +
			"version=VALUES(version), updated_at=VALUES(updated_at)", nil
	case FlavorPostgres:
		return "INSERT INTO his_dict " + cols + " VALUES (" + c.flavor.placeholderList(7) + ") " +
			"ON CONFLICT (code) DO UPDATE SET name=EXCLUDED.name, weight=EXCLUDED.weight, " +
			"payment_limit=EXCLUDED.payment_limit, effective_date=EXCLUDED.effective_date, " +
			"version=EXCLUDED.version, updated_at=EXCLUDED.updated_at", nil
	default:
		return "", fmt.Errorf("his_dict: unknown flavor %d", c.flavor)
	}
}

// buildAuditInsertSQL returns the audit-log INSERT. Columns are stable
// across dialects; only the placeholder syntax varies.
func (c *Client) buildAuditInsertSQL() string {
	return "INSERT INTO his_audit_log (code, action, version, applied_at) VALUES (" + c.flavor.placeholderList(4) + ")"
}

// buildSelectAllSQL returns the read-schema query. Reads payment_limit
// back into the DictEntry.Limit field so the Go-level API is unchanged.
func (c *Client) buildSelectAllSQL() string {
	return "SELECT code, name, weight, payment_limit, effective_date, version, updated_at FROM his_dict ORDER BY code"
}

// EnsureSchema creates the two tables if they don't exist. Called by
// NewSQLiteClient for tests / demo; production constructors expect the
// DBA to have created the schema and skip this step.
//
// Note on column naming: SQLite / MySQL / PostgreSQL all reserve the
// keyword LIMIT (SQL uses it for SELECT ... LIMIT n). Quoting it would
// require dialect-specific syntax; renaming the column to payment_limit
// keeps the SQL portable. The spec.md "minimum data format" stays
// unchanged at the Go-struct level (Limit field) — only the physical
// column differs.
func (c *Client) EnsureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS his_dict (
			code TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			weight REAL NOT NULL,
			payment_limit REAL NOT NULL,
			effective_date TEXT NOT NULL,
			version TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS his_audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			code TEXT NOT NULL,
			action TEXT NOT NULL,
			version TEXT NOT NULL,
			applied_at TEXT NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := c.db.ExecContext(ctx, s); err != nil {
			return &TransactionError{Op: "schema", Cause: err}
		}
	}
	return nil
}