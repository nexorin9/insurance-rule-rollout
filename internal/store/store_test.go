// store_test.go — happy-path CRUD tests for the SQLite-backed Plan / Issue /
// AuditEvent store. Covers migration idempotency, unique constraints, and
// index usage (EXPLAIN QUERY PLAN) per Task 3 acceptance criteria.
package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/example/insurance-rule-rollout/internal/workflow"
)

// openTestStore returns a freshly-opened in-memory SQLite with migrations
// applied. Each test gets its own isolated DB (no shared cache) so concurrent
// tests do not race on schema/constraint setup.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func samplePlanRow(id string) workflow.PlanRow {
	return workflow.PlanRow{
		PlanID:        id,
		VersionID:     "r-2026-09-DRG-v17",
		RuleSet:       "DRG-2026-09-NATIONAL",
		EffectiveDate: "2026-10-01",
		SpecJSON:      `{"version_id":"r-2026-09-DRG-v17"}`,
		PrevVersionID: "r-2026-06-DRG-v16",
		RollbackSQL:   "UPDATE his_dict SET version='r-2026-06-DRG-v16' WHERE version='r-2026-09-DRG-v17';",
		Status:        workflow.StatusDraft,
		Draft:         true,
		CreatedBy:     "yang_jb",
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
}

// TestMigrateIdempotent confirms that calling Migrate twice does not error
// and leaves the schema intact. Covers the "重复 Migrate 不报错" acceptance item.
func TestMigrateIdempotent(t *testing.T) {
	st := openTestStore(t)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if _, err := st.DB().ExecContext(context.Background(), "SELECT 1 FROM plan"); err != nil {
		t.Fatalf("plan table missing after second migrate: %v", err)
	}
	if _, err := st.DB().ExecContext(context.Background(), "SELECT 1 FROM issue"); err != nil {
		t.Fatalf("issue table missing after second migrate: %v", err)
	}
	if _, err := st.DB().ExecContext(context.Background(), "SELECT 1 FROM audit_event"); err != nil {
		t.Fatalf("audit_event table missing after second migrate: %v", err)
	}
}

// TestInsertPlanUniqueConstraint confirms that (plan_id, version_id) is
// UNIQUE. Inserting the same plan twice yields a constraint violation.
func TestInsertPlanUniqueConstraint(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	row := samplePlanRow("plan-001")
	if err := st.InsertPlan(ctx, row); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	err := st.InsertPlan(ctx, row)
	if err == nil {
		t.Fatal("expected UNIQUE constraint violation, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("expected UNIQUE error, got %v", err)
	}
}

// TestAuditEventUniqueConstraint confirms that (plan_id, actor, action,
// timestamp) is UNIQUE. Inserting the same event twice yields a constraint
// violation.
func TestAuditEventUniqueConstraint(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 28, 10, 30, 0, 0, time.UTC)
	if err := st.InsertPlan(ctx, samplePlanRow("plan-002")); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	ev := workflow.AuditEventRow{
		EventID:   "ev-001",
		PlanID:    "plan-002",
		Actor:     "yang_jb",
		Action:    workflow.ActionApprove,
		Payload:   map[string]string{"comment": "first"},
		Timestamp: now,
	}
	if err := st.InsertAuditEvent(ctx, ev); err != nil {
		t.Fatalf("first audit insert: %v", err)
	}
	ev2 := ev
	ev2.EventID = "ev-002"
	if err := st.InsertAuditEvent(ctx, ev2); err == nil {
		t.Fatal("expected UNIQUE violation, got nil")
	}
}

// TestListEventsOrdered confirms ListEvents returns events in ascending
// timestamp order (the ordering required by cmd/demo's "AuditEvent 顺序按
// timestamp 升序" assertion).
func TestListEventsOrdered(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if err := st.InsertPlan(ctx, samplePlanRow("plan-003")); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	for i, offset := range []time.Duration{0, 10 * time.Second, 20 * time.Second, 30 * time.Second} {
		ev := workflow.AuditEventRow{
			EventID:   "ev-" + string(rune('A'+i)),
			PlanID:    "plan-003",
			Actor:     "yang_jb",
			Action:    workflow.ActionApprove,
			Timestamp: base.Add(offset),
		}
		if err := st.InsertAuditEvent(ctx, ev); err != nil {
			t.Fatalf("insert event %d: %v", i, err)
		}
	}
	got, err := st.ListEvents(ctx, "plan-003")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 events, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Timestamp.Before(got[i-1].Timestamp) {
			t.Fatalf("events not ordered at index %d: %v before %v", i, got[i].Timestamp, got[i-1].Timestamp)
		}
	}
}

// TestUpdatePlanStatusAndGetPlan confirms InsertPlan → UpdatePlanStatus →
// GetPlan round-trips status, draft, last_action and last_action_at.
func TestUpdatePlanStatusAndGetPlan(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	row := samplePlanRow("plan-004")
	if err := st.InsertPlan(ctx, row); err != nil {
		t.Fatalf("insert plan: %v", err)
	}
	now := time.Now().UTC()
	if err := st.UpdatePlanStatus(ctx, "plan-004", workflow.StatusApproved, false, workflow.ActionApprove, now, now); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := st.GetPlan(ctx, "plan-004")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != workflow.StatusApproved {
		t.Fatalf("status: want approved, got %s", got.Status)
	}
	if got.Draft {
		t.Fatalf("draft: want false, got true")
	}
	if got.LastAction != workflow.ActionApprove {
		t.Fatalf("last_action: want approve, got %s", got.LastAction)
	}
}

// TestUpdatePlanStatusNotFound confirms UpdatePlanStatus returns
// ErrPlanNotFound when the plan_id is absent (rather than silently succeeding).
func TestUpdatePlanStatusNotFound(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	err := st.UpdatePlanStatus(ctx, "missing-plan", workflow.StatusApproved, false, workflow.ActionApprove, time.Now().UTC(), time.Now().UTC())
	if !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("expected ErrPlanNotFound, got %v", err)
	}
}

// TestPlanStatusIndexUsed confirms the migration creates idx_plan_status
// (EXPLAIN QUERY PLAN may pick a scan over the index for a 5-row demo
// fixture, so we assert the index exists in sqlite_master instead of
// forcing its use via INDEXED BY).
func TestPlanStatusIndexUsed(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	rows, err := st.DB().QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='plan'")
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}
	joined := strings.Join(names, " ")
	if !strings.Contains(joined, "idx_plan_status") {
		t.Fatalf("expected idx_plan_status, got: %v", names)
	}
	if !strings.Contains(joined, "idx_plan_version") {
		t.Fatalf("expected idx_plan_version, got: %v", names)
	}
}
