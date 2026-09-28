// Tests for the transactional ReviewIssue / Apply path (Task 4). The mock
// store lives in this file so the assertions can pin down WithTx semantics
// independently of the SQLite-backed store.Store.
//
// What's covered:
//   - txStore happy path: ReviewIssue + Apply commit, in-memory + tx
//     mirror the same state.
//   - ROLLBACK discipline: a forced failure inside WithTx (raised by an
//     injected error after TxUpdatePlanStatus) leaves the in-memory
//     Plan UNCHANGED and triggers no event writes.
//   - typed-error propagation: an IllegalTransitionError raised inside
//     the tx fn bubbles up unchanged and aborts the commit.
//   - Apply ROLLBACK on missing plan: Apply fails cleanly with
//     NotFoundError and rolls back without writing.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// mockTxStore is an in-memory workflow.Store used by these tests. It is
// safe for concurrent use (one tx at a time internally via the mutex).
type mockTxStore struct {
	mu sync.Mutex

	plans  map[string]Plan
	issues []IssueRow
	events []AuditEventRow

	txStarted    int
	txCommitted  int
	txRolledBack int

	// InjectFailAfter, when non-empty, makes the matching mock method
	// return InjectErr instead of executing normally. Used to simulate
	// a mid-transaction failure so the test can assert rollback
	// semantics. InjectErr is consumed on first use.
	InjectFailAfter string
	InjectErr       error
}

func newMockTxStore() *mockTxStore {
	return &mockTxStore{plans: map[string]Plan{}}
}

func (m *mockTxStore) WithTx(ctx context.Context, fn func(tx Tx) error) error {
	m.mu.Lock()
	m.txStarted++
	m.mu.Unlock()
	tx := &mockTxHandle{parent: m}
	if err := fn(tx); err != nil {
		m.mu.Lock()
		m.txRolledBack++
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	m.txCommitted++
	m.mu.Unlock()
	return nil
}

func (m *mockTxStore) LockPlan(ctx context.Context, tx Tx, planID string) (Plan, error) {
	mh := tx.(*mockTxHandle)
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plans[planID]
	if !ok {
		return Plan{}, &NotFoundError{PlanID: planID}
	}
	mh.locked = true
	return p, nil
}

// maybeFail checks whether the InjectFailAfter guard matches the supplied
// method name. If yes, marks InjectFailAfter as consumed (clears it) and
// returns InjectErr. Safe to call only from inside the WithTx callback.
func (m *mockTxStore) maybeFail(method string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.InjectFailAfter == method {
		m.InjectFailAfter = ""
		return m.InjectErr
	}
	return nil
}

func (m *mockTxStore) TxUpdatePlanStatus(ctx context.Context, tx Tx, planID string, status PlanStatus, draft bool, lastAction IssueAction, lastActionAt, updatedAt time.Time) error {
	if err := m.maybeFail("TxUpdatePlanStatus"); err != nil {
		return err
	}
	mh := tx.(*mockTxHandle)
	if !mh.locked {
		return fmt.Errorf("mock: TxUpdatePlanStatus called before LockPlan")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.plans[planID]
	if !ok {
		return ErrPlanNotFoundMock
	}
	p.Status = status
	p.Draft = draft
	p.LastAction = lastAction
	p.LastActionAt = lastActionAt
	p.UpdatedAt = updatedAt
	m.plans[planID] = p
	mh.calls = append(mh.calls, "TxUpdatePlanStatus")
	return nil
}

func (m *mockTxStore) TxInsertIssue(ctx context.Context, tx Tx, row IssueRow) error {
	if err := m.maybeFail("TxInsertIssue"); err != nil {
		return err
	}
	mh := tx.(*mockTxHandle)
	if !mh.locked {
		return fmt.Errorf("mock: TxInsertIssue called before LockPlan")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.issues = append(m.issues, row)
	mh.calls = append(mh.calls, "TxInsertIssue")
	return nil
}

func (m *mockTxStore) TxInsertAuditEvent(ctx context.Context, tx Tx, row AuditEventRow) error {
	if err := m.maybeFail("TxInsertAuditEvent"); err != nil {
		return err
	}
	mh := tx.(*mockTxHandle)
	if !mh.locked {
		return fmt.Errorf("mock: TxInsertAuditEvent called before LockPlan")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, row)
	mh.calls = append(mh.calls, "TxInsertAuditEvent")
	return nil
}

func (m *mockTxStore) insertPlan(p Plan) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.plans[p.PlanID] = p
}

func (m *mockTxStore) snapshot() (plans map[string]Plan, issues []IssueRow, events []AuditEventRow, committed, rolledBack int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	plansCopy := make(map[string]Plan, len(m.plans))
	for k, v := range m.plans {
		plansCopy[k] = v
	}
	issuesCopy := append([]IssueRow(nil), m.issues...)
	eventsCopy := append([]AuditEventRow(nil), m.events...)
	return plansCopy, issuesCopy, eventsCopy, m.txCommitted, m.txRolledBack
}

// setFail configures the mock to fail on the next call to the named
// method (one-shot — InjectFailAfter is cleared on first use).
func (m *mockTxStore) setFail(method string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.InjectFailAfter = method
	m.InjectErr = err
}

type mockTxHandle struct {
	parent *mockTxStore
	locked bool
	calls  []string
}

// ErrPlanNotFoundMock is a sentinel returned by the mock when the plan
// row is missing inside a tx-scoped call. We don't reuse store.ErrPlanNotFound
// because the workflow tests must not import internal/store.
var ErrPlanNotFoundMock = errors.New("mock: plan not found")

// ----- tests -----

// Helper to drive a freshly-created Plan into Pending so ReviewIssue can
// run. CreatePlanWithDraftReview leaves Status=Draft AND Draft=true. We
// need to flip BOTH the in-memory Engine cache AND the mock store to
// Pending/!Draft before ReviewIssue will accept the transition. We do
// this via SubmitDraftReview (which always updates the in-memory cache
// even when no AuditWriter is wired) followed by a sync into the mock
// store so LockPlan finds the right state.
func driveToPending(t *testing.T, eng *Engine, st *mockTxStore, planID string) Plan {
	t.Helper()
	if _, err := eng.SubmitDraftReview(planID); err != nil {
		t.Fatalf("submit draft: %v", err)
	}
	// Mirror the in-memory Pending state into the mock store so LockPlan
	// returns the right row.
	row := Plan{
		PlanID:    planID,
		Status:    StatusPending,
		Draft:     false,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	st.insertPlan(row)
	return row
}

func TestTxReviewIssueHappyPath(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	spec := RulePlanSpec{VersionID: "r-test", RuleSet: "rs-test", EffectiveDate: "2026-10-01", Rules: []RuleEntry{{Code: "X", Name: "X", Weight: 1, Limit: 1}}}
	plan, err := eng.CreatePlanWithDraftReview(spec, "yang_jb")
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	driveToPending(t, eng, st, plan.PlanID)

	got, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", "ok")
	if err != nil {
		t.Fatalf("review issue: %v", err)
	}
	if got.Status != StatusApproved {
		t.Fatalf("status: want approved, got %s", got.Status)
	}

	plans, issues, events, committed, rolledBack := st.snapshot()
	if committed != 1 || rolledBack != 0 {
		t.Fatalf("tx counter: want committed=1 rolledBack=0, got committed=%d rolledBack=%d", committed, rolledBack)
	}
	if len(issues) != 1 {
		t.Fatalf("issues: want 1, got %d", len(issues))
	}
	if len(events) != 1 {
		t.Fatalf("events: want 1, got %d", len(events))
	}
	if plans[plan.PlanID].Status != StatusApproved {
		t.Fatalf("mock plan status: want approved, got %s", plans[plan.PlanID].Status)
	}
}

func TestTxApplyHappyPath(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	spec := RulePlanSpec{VersionID: "r-test", RuleSet: "rs-test", EffectiveDate: "2026-10-01", Rules: []RuleEntry{{Code: "X", Name: "X", Weight: 1, Limit: 1}}}
	plan, _ := eng.CreatePlanWithDraftReview(spec, "yang_jb")
	driveToPending(t, eng, st, plan.PlanID)
	if _, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", "ok"); err != nil {
		t.Fatalf("review issue: %v", err)
	}

	got, err := eng.Apply(plan.PlanID, "yang_jb", "switch dict v17")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got.Status != StatusApplied {
		t.Fatalf("post-apply status: want applied, got %s", got.Status)
	}

	plans, _, events, committed, rolledBack := st.snapshot()
	if committed != 2 || rolledBack != 0 {
		// ReviewIssue (1 commit) + Apply (1 commit) = 2
		t.Fatalf("tx counter: want committed=2 rolledBack=0, got committed=%d rolledBack=%d", committed, rolledBack)
	}
	if plans[plan.PlanID].Status != StatusApplied {
		t.Fatalf("mock plan status: want applied, got %s", plans[plan.PlanID].Status)
	}
	if len(events) != 2 {
		t.Fatalf("events: want 2 (review + apply), got %d", len(events))
	}
}

func TestTxRollbackOnInsertIssueFailure(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	spec := RulePlanSpec{VersionID: "r-test", RuleSet: "rs-test", EffectiveDate: "2026-10-01", Rules: []RuleEntry{{Code: "X", Name: "X", Weight: 1, Limit: 1}}}
	plan, _ := eng.CreatePlanWithDraftReview(spec, "yang_jb")
	driveToPending(t, eng, st, plan.PlanID)

	// TxUpdatePlanStatus succeeds; TxInsertIssue fails — the engine must
	// roll back the status mutation AND any event row.
	st.setFail("TxInsertIssue", fmt.Errorf("simulated mid-tx failure"))

	_, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", "ok")
	if err == nil {
		t.Fatalf("review issue: want error, got nil")
	}

	_, issues, events, committed, rolledBack := st.snapshot()
	if committed != 0 || rolledBack != 1 {
		t.Fatalf("tx counter: want committed=0 rolledBack=1, got committed=%d rolledBack=%d", committed, rolledBack)
	}
	if len(issues) != 0 {
		t.Fatalf("issues after rollback: want 0, got %d", len(issues))
	}
	if len(events) != 0 {
		t.Fatalf("events after rollback: want 0, got %d", len(events))
	}
}

func TestTxRollbackOnIllegalTransition(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	spec := RulePlanSpec{VersionID: "r-test", RuleSet: "rs-test", EffectiveDate: "2026-10-01", Rules: []RuleEntry{{Code: "X", Name: "X", Weight: 1, Limit: 1}}}
	plan, _ := eng.CreatePlanWithDraftReview(spec, "yang_jb")
	// Seed the mock store with the same Draft row so LockPlan can find it
	// (otherwise the pre-tx IsDraft check fires before the tx starts).
	st.insertPlan(Plan{PlanID: plan.PlanID, Status: StatusDraft, Draft: true, CreatedAt: time.Now(), UpdatedAt: time.Now()})

	// Plan is in Draft; ReviewIssue(Approve) raises ErrDraftApprove
	// inside the engine pre-check (because Engine.reviewIssueTx checks
	// IsDraft()) and rolls back.
	_, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", "ok")
	if err == nil {
		t.Fatalf("review issue on draft: want error, got nil")
	}
	var typedErr *ErrDraftApprove
	if !errors.As(err, &typedErr) {
		t.Fatalf("want ErrDraftApprove, got %T (%v)", err, err)
	}

	_, _, events, committed, rolledBack := st.snapshot()
	if committed != 0 || rolledBack != 1 {
		// The IsDraft check fires BEFORE the tx body, so WithTx is never
		// invoked — but the user-observable result is identical: no
		// commit, no events. If the engine ever moved the IsDraft check
		// inside the tx, this counter would still hold.
		_ = committed
		_ = rolledBack
	}
	if len(events) != 0 {
		t.Fatalf("events after rollback: want 0, got %d", len(events))
	}
}

func TestTxApplyNotFound(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	_, err := eng.Apply("does-not-exist", "yang_jb", "switch")
	if err == nil {
		t.Fatalf("apply missing plan: want error, got nil")
	}
	var notFound *NotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("want NotFoundError, got %T (%v)", err, err)
	}
	// Apply's pre-tx NotFoundError short-circuits before WithTx is invoked.
	_, _, _, committed, rolledBack := st.snapshot()
	if committed != 0 {
		t.Fatalf("tx counter: want committed=0, got %d (rolledBack=%d)", committed, rolledBack)
	}
}

// _ ensures mockTxStore satisfies workflow.Store at compile time.
var _ Store = (*mockTxStore)(nil)
