// Transactional Store interface for the workflow engine.
//
// The Engine uses two persistence surfaces:
//   1. AuditWriter — per-call CRUD (CreatePlan / SubmitDraftReview / Rollback
//      and the early-write paths of ReviewIssue / Apply). Set via SetStore.
//   2. Store (this file) — transactional gate wrapping LockPlan +
//      UpdatePlanStatus + InsertIssue + InsertAuditEvent in a single
//      BEGIN/COMMIT window. ReviewIssue and Apply prefer Store when set so
//      that a row-level lock + atomic write replaces the two-step
//      audit-then-update path. Set via SetTxStore.
//
// The two surfaces coexist on purpose: AuditWriter keeps the demo's create
// path simple (no extra WithTx nesting), while Store handles the concurrency-
// sensitive review / apply paths. Task 18's integration tests pin down the
// expected behaviour with file::memory:?cache=shared and concurrent goroutines.
//
// Source mapping: bytebase backend/component/review/workflow.go:ReviewIssue
// runs inside `s.db.BeginTx()` + `lockIssue FOR UPDATE` + `tx.Commit()`. The
// Store interface below is the smallest Go surface that captures that shape.
package workflow

import (
	"context"
	"time"
)

// Tx is the opaque transactional handle passed to all Store methods inside a
// WithTx block. Concrete implementations live in internal/store (SQLite) and
// future drivers (MySQL/PostgreSQL when WithTxGate runs against a real DB).
// Callers must not retain a Tx beyond the WithTx callback.
type Tx interface{}

// Store is the transactional persistence gate Engine.ReviewIssue and
// Engine.Apply consult when one is wired via SetTxStore. All four write
// methods MUST be called from within a WithTx callback; passing a Tx
// obtained outside is undefined behaviour.
//
// Concurrency contract: LockPlan takes a row-level lock equivalent to
// `SELECT ... FOR UPDATE`. Implementations that don't speak FOR UPDATE
// natively (e.g. SQLite, which serialises writes via the database lock)
// MUST emulate it by acquiring a write transaction lock (BEGIN IMMEDIATE
// in SQLite terms) so a second LockPlan on the same plan_id blocks until
// the first WithTx commits or rolls back.
//
// ROLLBACK discipline: WithTx MUST roll back when fn returns a non-nil
// error and commit when fn returns nil. Implementations must not commit
// partially-written state.
type Store interface {
	// WithTx opens a transaction, calls fn(tx), commits on nil return and
	// rolls back on any non-nil return. The error returned by fn (if any)
	// is propagated as the return value; commit / rollback errors are also
	// surfaced.
	WithTx(ctx context.Context, fn func(tx Tx) error) error

	// LockPlan loads a Plan with a row-level lock so the transaction is
	// the sole writer until commit / rollback. Returns NotFoundError when
	// the plan_id is absent.
	LockPlan(ctx context.Context, tx Tx, planID string) (Plan, error)

	// TxUpdatePlanStatus mutates plan.status, plan.draft, plan.last_action,
	// plan.last_action_at, plan.updated_at inside the transaction.
	TxUpdatePlanStatus(ctx context.Context, tx Tx, planID string, status PlanStatus, draft bool, lastAction IssueAction, lastActionAt, updatedAt time.Time) error

	// TxInsertIssue persists a reviewer's Issue row inside the transaction.
	TxInsertIssue(ctx context.Context, tx Tx, row IssueRow) error

	// TxInsertAuditEvent persists one audit chain event inside the
	// transaction. SHA256 hash chaining is layered on top by internal/audit
	// (Task 10); this surface just persists the raw row.
	TxInsertAuditEvent(ctx context.Context, tx Tx, row AuditEventRow) error
}
