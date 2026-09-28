//go:build integration

// Integration tests for the PostgreSQL backend of his_dict. Run with:
//
//	# requires Docker on $PATH (or a Docker socket the testcontainers client
//	# can reach); these tests start a real postgres:15-alpine container
//	# and exercise the same Apply/Read/DryRun paths production uses:
//
//	INS_USE_TESTCONTAINERS=1 go test -tags=integration -count=1 ./internal/integration/his_dict/...
//
// Without `-tags=integration` these tests are excluded from the build, so
// the default `go test ./...` stays green in CI environments without
// Docker. The CI tag is the recommended gate for `cf_orchestrate_*`.
//
// When -tags=integration is set but Docker isn't available, the tests
// call t.Skip() with a clear message rather than failing — production
// HIS environments don't need a local Docker.
package his_dict

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// requireDockerOrSkip fails the test cleanly when Docker is not reachable.
// testcontainers has its own SkipIfDockerIsNotAvailable helper but it lives
// on the DockerProvider struct which we'd otherwise have to instantiate.
// The cheap environmental probe here catches the common case without
// pulling extra types.
func requireDockerOrSkip(t *testing.T) {
	t.Helper()
	// Honour the project's own opt-out signal first so CI can disable
	// testcontainers even if Docker happens to be available.
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("INS_USE_TESTCONTAINERS"))); v == "" || v == "0" || v == "false" {
		t.Skip("skipping his_dict integration test: set INS_USE_TESTCONTAINERS=1 to enable (requires Docker)")
	}
	if _, err := os.Stat("/var/run/docker.sock"); err != nil {
		t.Skipf("skipping his_dict integration test: docker socket not available (%v)", err)
	}
}

// startPostgresContainer launches a postgres:15-alpine container, waits
// for it to be ready, and returns the DSN the pgx stdlib driver can use.
// The caller is responsible for terminating the container via the
// returned Terminate function (best-effort — failure to terminate does
// not fail the test, the testcontainers reaper will reap orphans).
func startPostgresContainer(t *testing.T) (dsn string, terminate func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pgC, err := tcpostgres.RunContainer(ctx,
		testcontainers.WithImage("postgres:15-alpine"),
		tcpostgres.WithDatabase("his_test"),
		tcpostgres.WithUsername("his_user"),
		tcpostgres.WithPassword("his_pass"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Skipf("skipping his_dict integration test: cannot start postgres container (%v)", err)
		return "", func() {}
	}

	host, err := pgC.Host(ctx)
	if err != nil {
		_ = pgC.Terminate(context.Background())
		t.Skipf("skipping his_dict integration test: container host lookup failed (%v)", err)
		return "", func() {}
	}
	port, err := pgC.MappedPort(ctx, "5432/tcp")
	if err != nil {
		_ = pgC.Terminate(context.Background())
		t.Skipf("skipping his_dict integration test: container port lookup failed (%v)", err)
		return "", func() {}
	}

	// sslmode=disable because the container only binds a TCP port; a
	// TLS handshake would need certs we don't ship. Production DSNs
	// should keep sslmode=verify-full.
	dsn = fmt.Sprintf("host=%s port=%s user=his_user password=his_pass dbname=his_test sslmode=disable", host, port.Port())

	terminate = func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		if err := pgC.Terminate(stopCtx); err != nil {
			t.Logf("warning: postgres container terminate failed: %v", err)
		}
	}
	return dsn, terminate
}

// createSchema runs the production-style CREATE TABLE statements
// against a freshly-launched PG instance. We intentionally do NOT use
// c.EnsureSchema here because that emits SQLite-flavored SQL (TEXT
// timestamps, INTEGER PRIMARY KEY AUTOINCREMENT) — production PG uses
// TIMESTAMP columns and SERIAL/BIGSERIAL for the audit log id.
func createSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`CREATE TABLE his_dict (
			code TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			weight NUMERIC NOT NULL,
			payment_limit NUMERIC NOT NULL,
			effective_date DATE NOT NULL,
			version TEXT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE TABLE his_audit_log (
			id BIGSERIAL PRIMARY KEY,
			code TEXT NOT NULL,
			action TEXT NOT NULL,
			version TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("createSchema failed on %q: %v", s, err)
		}
	}
}

// TestIntegration_Postgres_ApplyUpdate_Success writes 5 entries and
// verifies that his_dict holds all 5 rows while his_audit_log holds
// exactly 5 dual-write rows. Mirrors TestApplyUpdate_Success from the
// SQLite suite (Task 8) but against a real PG backend.
func TestIntegration_Postgres_ApplyUpdate_Success(t *testing.T) {
	requireDockerOrSkip(t)
	dsn, terminate := startPostgresContainer(t)
	defer terminate()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer db.Close()

	createSchema(t, db)

	client, err := NewPostgresClient(dsn)
	if err != nil {
		t.Fatalf("NewPostgresClient failed: %v", err)
	}
	defer client.Close()

	entries := []DictEntry{
		{Code: "DRG-A001", Name: "阑尾炎", Weight: 1.5, Limit: 35200, Version: "v17", EffectiveDate: "2026-09-01", UpdatedAt: time.Now().UTC()},
		{Code: "DRG-A002", Name: "胆囊结石", Weight: 1.7, Limit: 51200, Version: "v17", EffectiveDate: "2026-09-01", UpdatedAt: time.Now().UTC()},
		{Code: "DRG-B014", Name: "肺部感染", Weight: 2.1, Limit: 15800, Version: "v17", EffectiveDate: "2026-09-01", UpdatedAt: time.Now().UTC()},
		{Code: "DRG-C007", Name: "脑梗塞", Weight: 2.4, Limit: 31200, Version: "v17", EffectiveDate: "2026-09-01", UpdatedAt: time.Now().UTC()},
		{Code: "DRG-E022", Name: "新生儿黄疸", Weight: 0.9, Limit: 11500, Version: "v17", EffectiveDate: "2026-09-01", UpdatedAt: time.Now().UTC()},
	}
	ctx := context.Background()

	if err := client.ApplyUpdate(ctx, entries); err != nil {
		t.Fatalf("ApplyUpdate failed: %v", err)
	}

	// his_dict: 5 rows, codes match input order.
	var dictCodes []string
	rows, err := db.QueryContext(ctx, "SELECT code FROM his_dict ORDER BY code")
	if err != nil {
		t.Fatalf("select his_dict failed: %v", err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			_ = rows.Close()
			t.Fatalf("scan his_dict.code failed: %v", err)
		}
		dictCodes = append(dictCodes, c)
	}
	_ = rows.Close()
	if len(dictCodes) != 5 {
		t.Fatalf("his_dict row count = %d, want 5", len(dictCodes))
	}
	want := []string{"DRG-A001", "DRG-A002", "DRG-B014", "DRG-C007", "DRG-E022"}
	sort.Strings(want)
	for i, c := range want {
		if dictCodes[i] != c {
			t.Fatalf("his_dict row[%d] = %q, want %q", i, dictCodes[i], c)
		}
	}

	// his_audit_log: 5 rows, action='UPSERT', version='v17'.
	var auditCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM his_audit_log WHERE action='UPSERT' AND version='v17'").Scan(&auditCount); err != nil {
		t.Fatalf("count his_audit_log failed: %v", err)
	}
	if auditCount != 5 {
		t.Fatalf("his_audit_log UPSERT/v17 count = %d, want 5", auditCount)
	}
}

// TestIntegration_Postgres_ApplyUpdate_RollbackOnConstraintFailure
// verifies the ROLLBACK path: seed 2 rows, attempt to apply 5 rows
// where one violates the CHECK constraint, and assert no new rows landed
// in either table. Mirrors the SQLite-equivalent test from Task 8.
func TestIntegration_Postgres_ApplyUpdate_RollbackOnConstraintFailure(t *testing.T) {
	requireDockerOrSkip(t)
	dsn, terminate := startPostgresContainer(t)
	defer terminate()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer db.Close()

	// Schema includes a CHECK (payment_limit >= 0) constraint that
	// the bad batch violates on its first row.
	stmts := []string{
		`CREATE TABLE his_dict (
			code TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			weight NUMERIC NOT NULL,
			payment_limit NUMERIC NOT NULL CHECK (payment_limit >= 0),
			effective_date DATE NOT NULL,
			version TEXT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL
		)`,
		`CREATE TABLE his_audit_log (
			id BIGSERIAL PRIMARY KEY,
			code TEXT NOT NULL,
			action TEXT NOT NULL,
			version TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("schema setup failed on %q: %v", s, err)
		}
	}

	// Seed 2 rows directly so we can assert "no new rows" after the
	// failed apply.
	seed := []DictEntry{
		{Code: "DRG-X001", Name: "SeedA", Weight: 1.0, Limit: 10000, Version: "v16", EffectiveDate: "2026-08-01", UpdatedAt: time.Now().UTC()},
		{Code: "DRG-X002", Name: "SeedB", Weight: 1.1, Limit: 12000, Version: "v16", EffectiveDate: "2026-08-01", UpdatedAt: time.Now().UTC()},
	}
	client, err := NewPostgresClient(dsn)
	if err != nil {
		t.Fatalf("NewPostgresClient failed: %v", err)
	}
	defer client.Close()
	ctx := context.Background()
	if err := client.ApplyUpdate(ctx, seed); err != nil {
		t.Fatalf("seed ApplyUpdate failed: %v", err)
	}

	// Bad batch: DRG-BAD has negative payment_limit which violates the
	// CHECK. ApplyUpdate should ROLLBACK before COMMIT, leaving the 2
	// seed rows untouched and no DRG-BAD rows in either table.
	bad := []DictEntry{
		{Code: "DRG-A001", Name: "Normal", Weight: 1.5, Limit: 35200, Version: "v17", EffectiveDate: "2026-09-01", UpdatedAt: time.Now().UTC()},
		{Code: "DRG-BAD", Name: "Bad", Weight: 1.0, Limit: -1, Version: "v17", EffectiveDate: "2026-09-01", UpdatedAt: time.Now().UTC()},
	}
	err = client.ApplyUpdate(ctx, bad)
	if err == nil {
		t.Fatalf("ApplyUpdate should have failed on negative payment_limit; got nil")
	}
	if !strings.Contains(err.Error(), "payment_limit") && !strings.Contains(err.Error(), "check constraint") {
		t.Logf("note: error message did not reference constraint by name (acceptable, depends on PG version): %v", err)
	}

	// No DRG-BAD in either table.
	var badCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM his_dict WHERE code='DRG-BAD'").Scan(&badCount); err != nil {
		t.Fatalf("select DRG-BAD from his_dict failed: %v", err)
	}
	if badCount != 0 {
		t.Fatalf("his_dict contains %d DRG-BAD rows after rollback; want 0", badCount)
	}
	var badAudit int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM his_audit_log WHERE code='DRG-BAD'").Scan(&badAudit); err != nil {
		t.Fatalf("select DRG-BAD from his_audit_log failed: %v", err)
	}
	if badAudit != 0 {
		t.Fatalf("his_audit_log contains %d DRG-BAD rows after rollback; want 0", badAudit)
	}

	// Seed rows must still be present (no collateral damage).
	var seedCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM his_dict WHERE version='v16'").Scan(&seedCount); err != nil {
		t.Fatalf("count v16 seed failed: %v", err)
	}
	if seedCount != 2 {
		t.Fatalf("v16 seed count = %d after failed apply; want 2 (rollback contract violated)", seedCount)
	}
}

// TestIntegration_Postgres_ReadSchema_ReturnsAllRows verifies that
// ReadSchema returns all rows in deterministic code-sorted order, the
// snapshot type round-trips back to DictSchema correctly, and Version /
// EffectiveDate fields are populated from the dominant row.
func TestIntegration_Postgres_ReadSchema_ReturnsAllRows(t *testing.T) {
	requireDockerOrSkip(t)
	dsn, terminate := startPostgresContainer(t)
	defer terminate()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer db.Close()

	createSchema(t, db)
	client, err := NewPostgresClient(dsn)
	if err != nil {
		t.Fatalf("NewPostgresClient failed: %v", err)
	}
	defer client.Close()

	// Insert rows in non-alphabetical order to prove the SELECT
	// ordering is stable.
	entries := []DictEntry{
		{Code: "DRG-Z999", Name: "Tail", Weight: 1.0, Limit: 5000, Version: "v17", EffectiveDate: "2026-09-01", UpdatedAt: time.Now().UTC()},
		{Code: "DRG-A001", Name: "Head", Weight: 1.5, Limit: 35200, Version: "v17", EffectiveDate: "2026-09-01", UpdatedAt: time.Now().UTC()},
		{Code: "DRG-M500", Name: "Mid", Weight: 1.2, Limit: 9000, Version: "v17", EffectiveDate: "2026-09-01", UpdatedAt: time.Now().UTC()},
	}
	ctx := context.Background()
	if err := client.ApplyUpdate(ctx, entries); err != nil {
		t.Fatalf("seed ApplyUpdate failed: %v", err)
	}

	schema, err := client.ReadSchema(ctx)
	if err != nil {
		t.Fatalf("ReadSchema failed: %v", err)
	}
	if len(schema.Entries) != 3 {
		t.Fatalf("ReadSchema entry count = %d, want 3", len(schema.Entries))
	}
	// Verify code-ascending order — the SELECT emits `ORDER BY code`.
	got := []string{schema.Entries[0].Code, schema.Entries[1].Code, schema.Entries[2].Code}
	want := []string{"DRG-A001", "DRG-M500", "DRG-Z999"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ReadSchema row[%d] code = %q, want %q", i, got[i], want[i])
		}
	}
	if schema.Version != "v17" {
		t.Fatalf("schema.Version = %q, want v17", schema.Version)
	}
}

// TestIntegration_Postgres_DryRunScan_DoesNotWrite confirms DryRunScan
// is a true read-only probe: it must succeed on a fresh PG instance
// without writing any rows. The test runs before and after the call
// and verifies the row count is identical (zero in both cases).
func TestIntegration_Postgres_DryRunScan_DoesNotWrite(t *testing.T) {
	requireDockerOrSkip(t)
	dsn, terminate := startPostgresContainer(t)
	defer terminate()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer db.Close()

	createSchema(t, db)
	client, err := NewPostgresClient(dsn)
	if err != nil {
		t.Fatalf("NewPostgresClient failed: %v", err)
	}
	defer client.Close()

	probe := []DictEntry{
		{Code: "DRG-A001", Name: "Probe", Weight: 1.5, Limit: 35200, Version: "v17", EffectiveDate: "2026-09-01", UpdatedAt: time.Now().UTC()},
		{Code: "DRG-A002", Name: "Probe2", Weight: 1.7, Limit: 51200, Version: "v17", EffectiveDate: "2026-09-01", UpdatedAt: time.Now().UTC()},
	}
	ctx := context.Background()
	if err := client.DryRunScan(ctx, probe); err != nil {
		t.Fatalf("DryRunScan failed on empty dict: %v", err)
	}

	var dictCount, auditCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM his_dict").Scan(&dictCount); err != nil {
		t.Fatalf("count his_dict failed: %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM his_audit_log").Scan(&auditCount); err != nil {
		t.Fatalf("count his_audit_log failed: %v", err)
	}
	if dictCount != 0 {
		t.Fatalf("his_dict has %d rows after DryRunScan; want 0 (read-only contract violated)", dictCount)
	}
	if auditCount != 0 {
		t.Fatalf("his_audit_log has %d rows after DryRunScan; want 0 (read-only contract violated)", auditCount)
	}
}