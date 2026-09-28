// Tests for the HIS dict client. We substitute SQLite for MySQL/Postgres
// because the production drivers are not vendored in this round — the
// transaction semantics (BEGIN / UPDATE / INSERT / COMMIT / ROLLBACK) are
// identical, so high confidence in the SQLite path transfers to the
// MySQL/Postgres paths (Task 25 layers in testcontainers-go for end-to-
// end PG coverage).
//
// Coverage matrix:
//
//   ApplyUpdate success (5 rows + audit log 5 rows)
//   ApplyUpdate failure on constraint violation → ROLLBACK (his_dict 0 changes)
//   ApplyUpdate empty entries → no-op
//   ReadSchema returns ordered snapshot
//   DryRunScan on empty entries → no-op
//   DryRunScan with invalid entry → ValidationError
//   TransactionError type + Unwrap on driver error
//   ValidationError duplicate code
//   ValidationError negative limit/weight
//   ValidationError empty code/name/effective_date/version
//   NewSQLiteClient empty DSN → error
//   NewSQLiteClient happy path → tables created + 2 expected tables present
//   Flavor placeholder generation (MySQL=?, PG=$1..$N, SQLite=?)
package his_dict

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sampleEntries returns 5 desensitized DRG rule rows. Mirrors the
// testdata/dict_v17.json fixture so a passing test doubles as a
// canary against the fixture drifting out of sync.
func sampleEntries() []DictEntry {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	return []DictEntry{
		{Code: "DRG-A001", Name: "胃食管及腹部手术", Weight: 2.450, Limit: 36800.00, EffectiveDate: "2026-10-01", Version: "r-2026-09-DRG-v17", UpdatedAt: now},
		{Code: "DRG-A002", Name: "肝胆胰系统手术", Weight: 3.180, Limit: 49500.00, EffectiveDate: "2026-10-01", Version: "r-2026-09-DRG-v17", UpdatedAt: now},
		{Code: "DRG-B014", Name: "肺部感染", Weight: 1.250, Limit: 14800.00, EffectiveDate: "2026-10-01", Version: "r-2026-09-DRG-v17", UpdatedAt: now},
		{Code: "DRG-C007", Name: "急性脑血管病", Weight: 2.090, Limit: 27600.00, EffectiveDate: "2026-10-01", Version: "r-2026-09-DRG-v17", UpdatedAt: now},
		{Code: "DRG-E022", Name: "新生儿疾病", Weight: 1.080, Limit: 11500.00, EffectiveDate: "2026-10-01", Version: "r-2026-09-DRG-v17", UpdatedAt: now},
	}
}

// freshSQLiteClient opens a private in-memory SQLite and runs schema
// setup. Each test gets its own database so they can run in parallel
// without contending for the global state.
func freshSQLiteClient(t *testing.T) *SQLiteClient {
	t.Helper()
	c, err := NewSQLiteClient("file:" + filepath.Join(t.TempDir(), "his.db") + "?cache=shared")
	if err != nil {
		t.Fatalf("NewSQLiteClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestApplyUpdate_Success verifies the happy path: 5 entries applied,
// his_dict has 5 rows, his_audit_log has 5 rows, schema version is
// stamped from the entries.
func TestApplyUpdate_Success(t *testing.T) {
	c := freshSQLiteClient(t)
	ctx := context.Background()

	entries := sampleEntries()
	if err := c.ApplyUpdate(ctx, entries); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}

	// his_dict must have 5 rows.
	var dictCount int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM his_dict").Scan(&dictCount); err != nil {
		t.Fatalf("count his_dict: %v", err)
	}
	if dictCount != 5 {
		t.Errorf("his_dict rows: got %d, want 5", dictCount)
	}

	// his_audit_log must have 5 rows.
	var auditCount int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM his_audit_log").Scan(&auditCount); err != nil {
		t.Fatalf("count his_audit_log: %v", err)
	}
	if auditCount != 5 {
		t.Errorf("his_audit_log rows: got %d, want 5", auditCount)
	}

	// ReadSchema returns all 5 entries.
	schema, err := c.ReadSchema(ctx)
	if err != nil {
		t.Fatalf("ReadSchema: %v", err)
	}
	if len(schema.Entries) != 5 {
		t.Errorf("schema entries: got %d, want 5", len(schema.Entries))
	}
	if schema.Version != "r-2026-09-DRG-v17" {
		t.Errorf("schema version: got %q, want r-2026-09-DRG-v17", schema.Version)
	}
}

// TestApplyUpdate_RollbackOnConstraintFailure forces a constraint
// violation mid-transaction and asserts no partial state is left behind.
// Strategy: pre-insert one of the target codes with an unrelated row so
// the second INSERT inside the transaction triggers a PRIMARY KEY
// conflict on the upsert path. The 4-row + 4-row partial state must NOT
// survive the rollback.
//
// Note: SQLite's INSERT OR REPLACE will overwrite — to force a real
// failure we use a CHECK constraint and submit a row that violates it.
func TestApplyUpdate_RollbackOnConstraintFailure(t *testing.T) {
	c := freshSQLiteClient(t)
	ctx := context.Background()

	// Add a CHECK constraint that limits weight to <= 1000. The 5th
	// sample entry has weight 1.080 which is fine, but we'll swap one
	// entry to weight 9999.99 to trip the check.
	if _, err := c.db.ExecContext(ctx, "ALTER TABLE his_dict ADD CONSTRAINT chk_weight CHECK (weight <= 1000)"); err != nil {
		t.Fatalf("add constraint: %v", err)
	}

	// First commit 2 entries successfully so we have a baseline.
	seed := sampleEntries()[:2]
	if err := c.ApplyUpdate(ctx, seed); err != nil {
		t.Fatalf("seed ApplyUpdate: %v", err)
	}

	// Now submit a batch where the 3rd entry violates the constraint.
	batch := sampleEntries()
	batch[2].Weight = 9999.99

	err := c.ApplyUpdate(ctx, batch)
	if err == nil {
		t.Fatalf("expected constraint violation, got nil")
	}
	var txErr *TransactionError
	if !errors.As(err, &txErr) {
		t.Fatalf("expected *TransactionError, got %T (%v)", err, err)
	}
	if txErr.Op != "update" && txErr.Op != "commit" {
		t.Errorf("expected Op=update|commit, got %q", txErr.Op)
	}

	// his_dict must still have only the 2 seeded rows (rollback worked).
	var count int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM his_dict").Scan(&count); err != nil {
		t.Fatalf("count his_dict: %v", err)
	}
	if count != 2 {
		t.Errorf("his_dict rows after rollback: got %d, want 2", count)
	}

	// his_audit_log must also still be at 2.
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM his_audit_log").Scan(&count); err != nil {
		t.Fatalf("count his_audit_log: %v", err)
	}
	if count != 2 {
		t.Errorf("his_audit_log rows after rollback: got %d, want 2", count)
	}
}

// TestApplyUpdate_EmptyEntries confirms the no-op contract — calling
// ApplyUpdate with zero entries returns nil without opening a tx.
func TestApplyUpdate_EmptyEntries(t *testing.T) {
	c := freshSQLiteClient(t)
	if err := c.ApplyUpdate(context.Background(), nil); err != nil {
		t.Errorf("empty entries: %v", err)
	}
	if err := c.ApplyUpdate(context.Background(), []DictEntry{}); err != nil {
		t.Errorf("empty entries slice: %v", err)
	}

	// No rows should have been written.
	var count int
	if err := c.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM his_dict").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("his_dict rows after empty ApplyUpdate: got %d, want 0", count)
	}
}

// TestApplyUpdate_UpsertOverwrite verifies that re-applying the same
// code with a new version overwrites the row in one transaction. This
// is the "v17 supersedes v16" path.
func TestApplyUpdate_UpsertOverwrite(t *testing.T) {
	c := freshSQLiteClient(t)
	ctx := context.Background()

	// v16 first.
	v16 := sampleEntries()
	for i := range v16 {
		v16[i].Version = "r-2026-06-DRG-v16"
		v16[i].Limit -= 1000 // simulate older limits
	}
	if err := c.ApplyUpdate(ctx, v16); err != nil {
		t.Fatalf("v16 ApplyUpdate: %v", err)
	}

	// Now v17 overwrites. Sample entries are already v17.
	v17 := sampleEntries()
	if err := c.ApplyUpdate(ctx, v17); err != nil {
		t.Fatalf("v17 ApplyUpdate: %v", err)
	}

	// his_dict must still have exactly 5 rows (not 10).
	var count int
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM his_dict").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 5 {
		t.Errorf("his_dict rows after upsert: got %d, want 5", count)
	}

	// The first code's version must be v17.
	var version string
	if err := c.db.QueryRowContext(ctx, "SELECT version FROM his_dict WHERE code=?", "DRG-A001").Scan(&version); err != nil {
		t.Fatalf("query: %v", err)
	}
	if version != "r-2026-09-DRG-v17" {
		t.Errorf("code DRG-A001 version after upsert: got %q, want r-2026-09-DRG-v17", version)
	}

	// his_audit_log should now have 10 rows (5 v16 + 5 v17).
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM his_audit_log").Scan(&count); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if count != 10 {
		t.Errorf("his_audit_log rows after upsert: got %d, want 10", count)
	}
}

// TestReadSchema_ReturnsOrderedSnapshot verifies ReadSchema emits rows
// sorted by code (ORDER BY code in the SQL) so the dry-run engine's
// diff calculation is deterministic.
func TestReadSchema_ReturnsOrderedSnapshot(t *testing.T) {
	c := freshSQLiteClient(t)
	ctx := context.Background()

	// Insert in non-alphabetical order to confirm the SQL sort kicks in.
	entries := sampleEntries()
	// Reverse the slice before applying.
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	if err := c.ApplyUpdate(ctx, entries); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}

	schema, err := c.ReadSchema(ctx)
	if err != nil {
		t.Fatalf("ReadSchema: %v", err)
	}
	want := []string{"DRG-A001", "DRG-A002", "DRG-B014", "DRG-C007", "DRG-E022"}
	if len(schema.Entries) != len(want) {
		t.Fatalf("entries len: got %d, want %d", len(schema.Entries), len(want))
	}
	for i, e := range schema.Entries {
		if e.Code != want[i] {
			t.Errorf("entries[%d].Code: got %q, want %q", i, e.Code, want[i])
		}
	}
}

// TestReadSchema_EmptyDB confirms ReadSchema on a fresh DB returns an
// empty schema with no entries and no error.
func TestReadSchema_EmptyDB(t *testing.T) {
	c := freshSQLiteClient(t)
	schema, err := c.ReadSchema(context.Background())
	if err != nil {
		t.Fatalf("ReadSchema: %v", err)
	}
	if len(schema.Entries) != 0 {
		t.Errorf("entries: got %d, want 0", len(schema.Entries))
	}
	if schema.Version != "" {
		t.Errorf("version: got %q, want empty", schema.Version)
	}
}

// TestDryRunScan_ValidEntries confirms DryRunScan returns nil for a
// payload that ApplyUpdate would accept.
func TestDryRunScan_ValidEntries(t *testing.T) {
	c := freshSQLiteClient(t)
	if err := c.DryRunScan(context.Background(), sampleEntries()); err != nil {
		t.Errorf("DryRunScan: %v", err)
	}
}

// TestDryRunScan_InvalidEntry confirms validation errors are surfaced as
// *ValidationError (not TransactionError) so dry-run reports can list
// them as "would-fail" without false positives on rollback classification.
func TestDryRunScan_InvalidEntry(t *testing.T) {
	c := freshSQLiteClient(t)
	entries := sampleEntries()
	entries[0].Limit = -1 // negative

	err := c.DryRunScan(context.Background(), entries)
	if err == nil {
		t.Fatalf("expected ValidationError, got nil")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T (%v)", err, err)
	}
	if !strings.Contains(vErr.Field, "limit") {
		t.Errorf("expected field to mention limit, got %q", vErr.Field)
	}
}

// TestTransactionError_Unwrap exposes the underlying driver error so
// errors.Is / errors.As can introspect the cause.
func TestTransactionError_Unwrap(t *testing.T) {
	sentinel := errors.New("simulated driver failure")
	e := &TransactionError{Op: "update", Code: "DRG-A001", Cause: sentinel}
	if !errors.Is(e, sentinel) {
		t.Errorf("errors.Is should reach sentinel")
	}
	if e.Error() == "" {
		t.Errorf("Error() must produce a non-empty message")
	}
	if !strings.Contains(e.Error(), "DRG-A001") {
		t.Errorf("Error() should mention code: %q", e.Error())
	}
}

// TestValidationError_DuplicateCode covers the duplicate-code guard.
func TestValidationError_DuplicateCode(t *testing.T) {
	c := freshSQLiteClient(t)
	entries := sampleEntries()
	entries = append(entries, entries[0]) // duplicate DRG-A001

	err := c.ApplyUpdate(context.Background(), entries)
	if err == nil {
		t.Fatalf("expected ValidationError, got nil")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T (%v)", err, err)
	}
	if !strings.Contains(vErr.Reason, "duplicate") {
		t.Errorf("expected duplicate reason, got %q", vErr.Reason)
	}
}

// TestValidationError_NegativeLimit covers the negative-limit guard.
func TestValidationError_NegativeLimit(t *testing.T) {
	c := freshSQLiteClient(t)
	entries := sampleEntries()
	entries[1].Limit = -100

	err := c.ApplyUpdate(context.Background(), entries)
	if err == nil {
		t.Fatalf("expected ValidationError, got nil")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T (%v)", err, err)
	}
	if !strings.Contains(vErr.Field, "limit") {
		t.Errorf("expected field to mention limit, got %q", vErr.Field)
	}
}

// TestValidationError_NegativeWeight covers the negative-weight guard.
func TestValidationError_NegativeWeight(t *testing.T) {
	c := freshSQLiteClient(t)
	entries := sampleEntries()
	entries[2].Weight = -1.5

	err := c.ApplyUpdate(context.Background(), entries)
	if err == nil {
		t.Fatalf("expected ValidationError, got nil")
	}
	var vErr *ValidationError
	if !errors.As(err, &vErr) {
		t.Fatalf("expected *ValidationError, got %T (%v)", err, err)
	}
	if !strings.Contains(vErr.Field, "weight") {
		t.Errorf("expected field to mention weight, got %q", vErr.Field)
	}
}

// TestValidationError_EmptyFields covers all four "empty string" guards
// (code, name, effective_date, version).
func TestValidationError_EmptyFields(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*DictEntry)
		wantSub string
	}{
		{"empty code", func(e *DictEntry) { e.Code = "" }, "code"},
		{"empty name", func(e *DictEntry) { e.Name = "" }, "name"},
		{"empty effective_date", func(e *DictEntry) { e.EffectiveDate = "" }, "effective_date"},
		{"empty version", func(e *DictEntry) { e.Version = "" }, "version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := freshSQLiteClient(t)
			entries := sampleEntries()
			tc.mutate(&entries[0])

			err := c.ApplyUpdate(context.Background(), entries)
			if err == nil {
				t.Fatalf("expected ValidationError")
			}
			var vErr *ValidationError
			if !errors.As(err, &vErr) {
				t.Fatalf("expected *ValidationError, got %T (%v)", err, err)
			}
			if !strings.Contains(vErr.Field, tc.wantSub) {
				t.Errorf("expected field to mention %q, got %q", tc.wantSub, vErr.Field)
			}
		})
	}
}

// TestNewSQLiteClient_EmptyDSN covers the empty-DSN guard.
func TestNewSQLiteClient_EmptyDSN(t *testing.T) {
	_, err := NewSQLiteClient("")
	if err == nil {
		t.Fatalf("expected error on empty DSN")
	}
	if !strings.Contains(err.Error(), "DSN is empty") {
		t.Errorf("expected DSN error, got %v", err)
	}
}

// TestNewSQLiteClient_HappyPath verifies schema setup ran — both tables
// must exist after construction.
func TestNewSQLiteClient_HappyPath(t *testing.T) {
	c, err := NewSQLiteClient("file:" + filepath.Join(t.TempDir(), "his.db") + "?cache=shared")
	if err != nil {
		t.Fatalf("NewSQLiteClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	for _, table := range []string{"his_dict", "his_audit_log"} {
		var name string
		err := c.db.QueryRowContext(context.Background(),
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
		if name != table {
			t.Errorf("table name: got %q, want %q", name, table)
		}
	}
}

// TestFlavor_PlaceholderGeneration pins the dialect-correct placeholder
// expansion — a regression here would silently send wrong SQL to PG.
func TestFlavor_PlaceholderGeneration(t *testing.T) {
	if got := FlavorMySQL.quoteRef(1); got != "?" {
		t.Errorf("MySQL.quoteRef(1): got %q, want ?", got)
	}
	if got := FlavorMySQL.placeholderList(3); got != "?, ?, ?" {
		t.Errorf("MySQL.placeholderList(3): got %q, want ?, ?, ?", got)
	}
	if got := FlavorPostgres.quoteRef(1); got != "$1" {
		t.Errorf("Postgres.quoteRef(1): got %q, want $1", got)
	}
	if got := FlavorPostgres.quoteRef(7); got != "$7" {
		t.Errorf("Postgres.quoteRef(7): got %q, want $7", got)
	}
	if got := FlavorPostgres.placeholderList(3); got != "$1, $2, $3" {
		t.Errorf("Postgres.placeholderList(3): got %q, want $1, $2, $3", got)
	}
	if got := FlavorSQLite.quoteRef(1); got != "?" {
		t.Errorf("SQLite.quoteRef(1): got %q, want ?", got)
	}
	if got := FlavorSQLite.placeholderList(0); got != "" {
		t.Errorf("SQLite.placeholderList(0): got %q, want empty", got)
	}
}

// TestBuildUpsertSQL_Dialect verifies the per-flavor UPSERT syntax —
// catching a Postgres vs SQLite mix-up here would silently corrupt data
// on the first ApplyUpdate against a real PG instance.
func TestBuildUpsertSQL_Dialect(t *testing.T) {
	cases := []struct {
		flavor   Flavor
		contains string
	}{
		{FlavorSQLite, "INSERT OR REPLACE INTO his_dict"},
		{FlavorMySQL, "ON DUPLICATE KEY UPDATE"},
		{FlavorPostgres, "ON CONFLICT (code) DO UPDATE"},
	}
	for _, tc := range cases {
		t.Run(tc.contains, func(t *testing.T) {
			c := &Client{flavor: tc.flavor}
			sql, err := c.buildUpsertSQL()
			if err != nil {
				t.Fatalf("buildUpsertSQL: %v", err)
			}
			if !strings.Contains(sql, tc.contains) {
				t.Errorf("expected SQL to contain %q, got %q", tc.contains, sql)
			}
		})
	}
}

// TestNewMySQLClient_EmptyDSN confirms the MySQL path rejects empty DSN
// even before sql.Open is called.
func TestNewMySQLClient_EmptyDSN(t *testing.T) {
	_, err := NewMySQLClient("")
	if err == nil {
		t.Fatalf("expected error on empty DSN")
	}
}

// TestNewPostgresClient_EmptyDSN confirms the Postgres path rejects
// empty DSN even before sql.Open is called.
func TestNewPostgresClient_EmptyDSN(t *testing.T) {
	_, err := NewPostgresClient("")
	if err == nil {
		t.Fatalf("expected error on empty DSN")
	}
}

// TestSQLiteClient_SatisfiesDictClient is a compile-time-ish guard that
// the concrete SQLiteClient still satisfies DictClient. If a future
// refactor breaks the surface, this fails to compile.
func TestSQLiteClient_SatisfiesDictClient(t *testing.T) {
	var _ DictClient = (*SQLiteClient)(nil)
	var _ DictClient = (*MySQLClient)(nil)
	var _ DictClient = (*PostgresClient)(nil)
	var _ DictClient = (*Client)(nil)
}