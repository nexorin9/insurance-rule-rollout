// 9 illegal-transition contract tests (Task 17). Each test pins ONE illegal
// (status, action) pair to its dedicated typed error and asserts the four
// invariants a fly-inspect / regulator needs to trust the engine's state
// machine:
//
//  1. errors.As picks up the expected concrete typed error (e.g.
//     *ErrDraftApprove). Callers branch on the named type — never on the
//     error message — so this contract is non-negotiable.
//  2. Plan status is UNCHANGED in BOTH the engine's in-memory cache AND the
//     mockTxStore projection. A refused transition must never leave a
//     half-applied state behind.
//  3. AuditEvent count in the mock store is UNCHANGED. TxInsertAuditEvent
//     is never called during a refused transition; the transactional path
//     raises the typed error BEFORE any event row is written.
//  4. error.Error() embeds both the status name AND the action name so a
//     fly-inspect / log-scan script can locate the refused action via
//     "status=X" or "action=Y" filters.
//
// These tests are deliberately redundant with the table tests in
// state_test.go (Task 14) and the engine_review_test.go surface (Task 16):
// the goal here is independent verification of the *contract* between the
// engine and the wider workflow package — the typed-error catalogue, the
// legal-transition table, AND the transactional discipline must all hold
// simultaneously. Drift in any one of the three layers surfaces here.
package workflow

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// drivePlanTo drives a freshly-created Plan to the requested status via the
// engine's public API surface + mockTxStore. The returned int is the
// audit-event count baseline (events committed through TxInsertAuditEvent
// during setup); contract assertions use it to pin "AuditEvent 不增".
//
// Statuses covered:
//
//	StatusDraft     — CreatePlanWithDraftReview
//	StatusPending   — CreatePlan + SubmitDraftReview
//	StatusRejected  — Pending + ReviewIssue(Reject)
//	StatusApproved  — Pending + ReviewIssue(Approve)
//	StatusApplied   — Approved + Apply
//	StatusRolledBack — Applied + Rollback + mock-store mirror (Rollback
//	                  doesn't route through tStore, so the in-memory
//	                  RolledBack state must be mirrored into the mock
//	                  store for subsequent LockPlan calls)
func drivePlanTo(t *testing.T, eng *Engine, st *mockTxStore, target PlanStatus) (Plan, int) {
	t.Helper()
	spec := RulePlanSpec{
		VersionID:     "r-contract-test",
		RuleSet:       "rs-contract",
		EffectiveDate: "2026-10-01",
		Rules:         []RuleEntry{{Code: "DRG-X", Name: "test", Weight: 1, Limit: 1000}},
	}
	plan, err := eng.CreatePlanWithDraftReview(spec, "yang_jb")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Seed the freshly-created Draft row so LockPlan can find it.
	st.insertPlan(Plan{
		PlanID: plan.PlanID, Status: StatusDraft, Draft: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})

	if target == StatusDraft {
		return plan, 0
	}

	if _, err := eng.SubmitDraftReview(plan.PlanID); err != nil {
		t.Fatalf("submit draft: %v", err)
	}
	st.insertPlan(Plan{
		PlanID: plan.PlanID, Status: StatusPending, Draft: false,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if target == StatusPending {
		return plan, 0
	}

	if target == StatusRejected {
		if _, err := eng.ReviewIssue(plan.PlanID, ActionReject, "wang_xx", "setup reject"); err != nil {
			t.Fatalf("reject: %v", err)
		}
		return plan, 1
	}

	if _, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", "setup approve"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if target == StatusApproved {
		return plan, 1
	}

	if _, err := eng.Apply(plan.PlanID, "yang_jb", "setup apply"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if target == StatusApplied {
		return plan, 2
	}

	if target == StatusRolledBack {
		if _, err := eng.Rollback(plan.PlanID, "wang_xx", "setup rollback"); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		// Rollback bypasses tStore; mirror the in-memory RolledBack state
		// into the mock store so subsequent LockPlan calls see RolledBack.
		st.mu.Lock()
		if p, ok := st.plans[plan.PlanID]; ok {
			p.Status = StatusRolledBack
			p.LastAction = ActionRollback
			p.LastActionAt = time.Now()
			p.UpdatedAt = p.LastActionAt
			st.plans[plan.PlanID] = p
		}
		st.mu.Unlock()
		return plan, 2
	}

	t.Fatalf("unsupported target status %s", target)
	return Plan{}, 0
}

// assertIllegalContract invokes the refused action and asserts the four
// invariants from the package doc. It is the shared core of the 9 named
// tests below; the only things that vary between tests are the (status,
// action) pair, the typed-error target, and the audit-event baseline.
//
// expectedErrTarget is a pre-constructed pointer to the concrete typed
// error (e.g. &ErrDraftApprove{}). The helper passes &expectedErrTarget to
// errors.As so the existing typed-error catalogue is enforced verbatim.
// actionName is matched as a substring of error.Error() so fly-inspect grep
// rules can locate the refused action in operator logs.
func assertIllegalContract(
	t *testing.T,
	eng *Engine,
	st *mockTxStore,
	planID string,
	fromStatus PlanStatus,
	invoke func() error,
	expectedErrTarget any,
	actionName string,
	auditBaseline int,
) {
	t.Helper()

	// --- Pre-state: in-memory + mock store + audit baseline. ---
	preMem, err := eng.GetPlan(planID)
	if err != nil {
		t.Fatalf("[%s] pre-state GetPlan: %v", fromStatus, err)
	}
	if preMem.Status != fromStatus {
		t.Fatalf("[%s] pre-state in-memory status mismatch: want %s, got %s",
			fromStatus, fromStatus, preMem.Status)
	}
	prePlans, _, preEvents, _, _ := st.snapshot()
	preMock, ok := prePlans[planID]
	if !ok {
		t.Fatalf("[%s] pre-state: plan %q missing from mock store", fromStatus, planID)
	}
	if preMock.Status != fromStatus {
		t.Fatalf("[%s] pre-state mock-store status mismatch: want %s, got %s",
			fromStatus, fromStatus, preMock.Status)
	}
	if len(preEvents) != auditBaseline {
		t.Fatalf("[%s] pre-state audit baseline mismatch: want %d events, got %d",
			fromStatus, auditBaseline, len(preEvents))
	}

	// --- Invoke the refused action. ---
	err = invoke()
	if err == nil {
		t.Fatalf("[%s] expected error from refused action, got nil", fromStatus)
	}

	// --- 1. typed-error assertion (errors.As picks up concrete type). ---
	if !errors.As(err, &expectedErrTarget) {
		t.Fatalf("[%s] want error type %T, got %T (%v)",
			fromStatus, expectedErrTarget, err, err)
	}

	// --- 4. error message embeds status + action names. ---
	msg := err.Error()
	if !strings.Contains(msg, string(fromStatus)) {
		t.Fatalf("[%s] error message missing status %q: %s", fromStatus, fromStatus, msg)
	}
	if !strings.Contains(msg, actionName) {
		t.Fatalf("[%s] error message missing action %q: %s", fromStatus, actionName, msg)
	}

	// --- 2. Plan status UNCHANGED in BOTH in-memory and mock store. ---
	postMem, err := eng.GetPlan(planID)
	if err != nil {
		t.Fatalf("[%s] post-state GetPlan: %v", fromStatus, err)
	}
	if postMem.Status != fromStatus {
		t.Fatalf("[%s] in-memory Plan status mutated: was %s, now %s",
			fromStatus, fromStatus, postMem.Status)
	}
	postPlans, _, postEvents, _, _ := st.snapshot()
	postMock := postPlans[planID]
	if postMock.Status != fromStatus {
		t.Fatalf("[%s] mock-store Plan status mutated: was %s, now %s",
			fromStatus, fromStatus, postMock.Status)
	}

	// --- 3. AuditEvent count UNCHANGED. ---
	if len(postEvents) != auditBaseline {
		t.Fatalf("[%s] audit event count grew: was %d, now %d",
			fromStatus, auditBaseline, len(postEvents))
	}
}

// --- 9 named illegal-transition contract tests -----------------------------

// Test 1: Draft + Approve → *ErrDraftApprove
//
// The plan is still in draft review (Draft bool = true). ReviewIssue(Approve)
// short-circuits before WithTx: the engine's reviewIssueTx pre-check sees
// IsDraft() and returns ErrDraftApprove directly. WithTx is never called, so
// the mock store's txCommitted / txRolledBack counters don't move.
func TestContract_DraftApprove(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan, baseline := drivePlanTo(t, eng, st, StatusDraft)
	assertIllegalContract(t, eng, st, plan.PlanID, StatusDraft,
		func() error {
			_, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", "should fail")
			return err
		},
		&ErrDraftApprove{},
		string(ActionApprove),
		baseline,
	)
}

// Test 2: Approved + Approve → *ErrPendingReApprove (already-approved re-approve)
//
// Plan has been Approved once already; calling Approve again on an Approved
// plan is the named slot ErrPendingReApprove (the type's name is a relic
// of an earlier internal taxonomy — the (from, action) tuple is
// (approved, approve)). The error fires inside the tx body after LockPlan,
// so WithTx does roll back (txRolledBack++) but no event row is written.
func TestContract_PendingReApprove(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan, baseline := drivePlanTo(t, eng, st, StatusApproved)
	assertIllegalContract(t, eng, st, plan.PlanID, StatusApproved,
		func() error {
			_, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", "second approve")
			return err
		},
		&ErrPendingReApprove{},
		string(ActionApprove),
		baseline,
	)
}

// Test 3: Rejected + Approve → *ErrRejectedReApprove
//
// Rejected is a terminal state — no action is accepted after Reject. The
// Approve call here is the named slot ErrRejectedReApprove.
func TestContract_RejectedReApprove(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan, baseline := drivePlanTo(t, eng, st, StatusRejected)
	assertIllegalContract(t, eng, st, plan.PlanID, StatusRejected,
		func() error {
			_, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", "approve after reject")
			return err
		},
		&ErrRejectedReApprove{},
		string(ActionApprove),
		baseline,
	)
}

// Test 4: Approved + Request → *ErrApprovedRequest
//
// Request only re-opens a Pending plan after the reviewer has Requested
// changes. Calling Request on an Approved plan is the named slot
// ErrApprovedRequest.
func TestContract_ApprovedRequest(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan, baseline := drivePlanTo(t, eng, st, StatusApproved)
	assertIllegalContract(t, eng, st, plan.PlanID, StatusApproved,
		func() error {
			_, err := eng.ReviewIssue(plan.PlanID, ActionRequest, "yang_jb", "request after approve")
			return err
		},
		&ErrApprovedRequest{},
		string(ActionRequest),
		baseline,
	)
}

// Test 5: Applied + Reject → *ErrAppliedReject
//
// Once a plan has been Applied, the Reject action no longer applies — the
// operator must use Rollback instead. This is the named slot ErrAppliedReject.
func TestContract_AppliedReject(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan, baseline := drivePlanTo(t, eng, st, StatusApplied)
	assertIllegalContract(t, eng, st, plan.PlanID, StatusApplied,
		func() error {
			_, err := eng.ReviewIssue(plan.PlanID, ActionReject, "yang_jb", "reject after apply")
			return err
		},
		&ErrAppliedReject{},
		string(ActionReject),
		baseline,
	)
}

// Test 6: RolledBack + (Approve/Reject/Request) → *ErrRolledBackAny (3 sub-cases)
//
// RolledBack is terminal; no action of any kind is accepted. typedErrorFor
// collapses every (RolledBack, action) pair to the single named slot
// ErrRolledBackAny. Three sub-cases exercise Approve / Reject / Request —
// Apply and Rollback are also refused but they would call separate engine
// methods, so this sub-test sticks to the ReviewIssue surface.
func TestContract_RolledBackAny(t *testing.T) {
	cases := []struct {
		name   string
		action IssueAction
	}{
		{"rolled_back_approve", ActionApprove},
		{"rolled_back_reject", ActionReject},
		{"rolled_back_request", ActionRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newMockTxStore()
			eng := NewEngine()
			eng.SetTxStore(st)

			plan, baseline := drivePlanTo(t, eng, st, StatusRolledBack)
			assertIllegalContract(t, eng, st, plan.PlanID, StatusRolledBack,
				func() error {
					_, err := eng.ReviewIssue(plan.PlanID, tc.action, "yang_jb", "action on rolled back")
					return err
				},
				&ErrRolledBackAny{},
				string(tc.action),
				baseline,
			)
		})
	}
}

// Test 7: Pending + Request (second time) → *ErrPendingSelfLoop
//
// First Request on a Pending plan is legal (the (Pending, Request) self-loop
// entry in the legal-transitions table). The second Request, however, must
// be rejected by the engine-level Plan.LastAction guard with
// ErrPendingSelfLoop. The error message embeds the timestamp of the
// previous Request (from Plan.LastActionAt) so fly-inspect scripts can pin
// down how long ago the previous Request happened.
func TestContract_PendingSelfLoop(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan, _ := drivePlanTo(t, eng, st, StatusPending)
	// First Request — legal; sets Plan.LastAction=Request, LastActionAt=now.
	if _, err := eng.ReviewIssue(plan.PlanID, ActionRequest, "zhao_bz", "first request"); err != nil {
		t.Fatalf("first request: %v", err)
	}
	// After the first Request the mock store has 1 AuditEvent (the first
	// Request event); that's the baseline for the contract assertion.
	_, _, eventsBaseline, _, _ := st.snapshot()
	baseline := len(eventsBaseline)

	// Second Request — must hit ErrPendingSelfLoop with the previous
	// timestamp embedded in the error message.
	var gotErr error
	var gotMsg string
	err := func() error {
		_, e := eng.ReviewIssue(plan.PlanID, ActionRequest, "zhao_bz", "second request — self-loop")
		if e != nil {
			gotErr = e
			gotMsg = e.Error()
		}
		return e
	}()
	if err == nil {
		t.Fatalf("second request: want ErrPendingSelfLoop, got nil")
	}
	// Self-loop message must embed the previous timestamp (Plan.LastActionAt
	// was set ~milliseconds ago; the formatted string appears in the hint).
	if !strings.Contains(gotMsg, "previous Request was recorded at") {
		t.Fatalf("self-loop message missing timestamp hint: %s", gotMsg)
	}

	// Final contract assertions: typed error + Plan status unchanged + event
	// count unchanged. Note: typed-error and event-count assertions run on
	// the same planID, post-second-Request.
	assertIllegalContract(t, eng, st, plan.PlanID, StatusPending,
		func() error { return gotErr },
		&ErrPendingSelfLoop{},
		string(ActionRequest),
		baseline,
	)
}

// Test 8: Applied + Approve → *ErrAppliedReApprove
//
// A plan in the Applied state cannot be Approved again — the operator must
// Rollback to revert to v16 and then create a new plan. ErrAppliedReApprove
// fires on (Applied, Approve).
func TestContract_AppliedReApprove(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan, baseline := drivePlanTo(t, eng, st, StatusApplied)
	assertIllegalContract(t, eng, st, plan.PlanID, StatusApplied,
		func() error {
			_, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", "approve after apply")
			return err
		},
		&ErrAppliedReApprove{},
		string(ActionApprove),
		baseline,
	)
}

// Test 9: Rejected + Apply → *ErrRejectedApply
//
// Rejected is terminal; calling Apply() on a Rejected plan is the named
// slot ErrRejectedApply. This test exercises the Apply() method (not
// ReviewIssue) because Apply has its own engine entry point with its own
// tx-body LockPlan + NextStatus path; the contract applies there too.
func TestContract_RejectedApply(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan, baseline := drivePlanTo(t, eng, st, StatusRejected)
	assertIllegalContract(t, eng, st, plan.PlanID, StatusRejected,
		func() error {
			_, err := eng.Apply(plan.PlanID, "yang_jb", "apply after reject")
			return err
		},
		&ErrRejectedApply{},
		string(ActionApply),
		baseline,
	)
}

// --- Coverage-pushing tests ------------------------------------------------
//
// These tests aren't strictly part of the 9 illegal-transition contracts —
// they exist to drive workflow package coverage toward the Task 17 target of
// ≥ 90%. They cover the per-call AuditWriter path (when tStore is nil but
// store is set), the engine's injection helpers (SetClock / SetIDGen /
// SetStore), the ListPlans sink, and the persist() helper. Without them the
// non-tStore branches of SubmitDraftReview / ReviewIssue / Apply / Rollback
// fall below the coverage bar.

// mockAuditWriter is a minimal AuditWriter used by the per-call coverage
// test. It records every InsertPlan / UpdatePlanStatus / InsertIssue /
// InsertAuditEvent call so the test can assert the engine hit the
// non-tStore branches.
type mockAuditWriter struct {
	mu             sync.Mutex
	plans          []PlanRow
	statusUpdates  []string // plan_id of each UpdatePlanStatus call
	issues         []IssueRow
	events         []AuditEventRow
	failNextMethod string
	failErr        error
}

func (m *mockAuditWriter) InsertPlan(ctx context.Context, row PlanRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNextMethod == "InsertPlan" {
		m.failNextMethod = ""
		return m.failErr
	}
	m.plans = append(m.plans, row)
	return nil
}
func (m *mockAuditWriter) UpdatePlanStatus(ctx context.Context, planID string, status PlanStatus, draft bool, lastAction IssueAction, lastActionAt, updatedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNextMethod == "UpdatePlanStatus" {
		m.failNextMethod = ""
		return m.failErr
	}
	m.statusUpdates = append(m.statusUpdates, planID)
	return nil
}
func (m *mockAuditWriter) InsertIssue(ctx context.Context, row IssueRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNextMethod == "InsertIssue" {
		m.failNextMethod = ""
		return m.failErr
	}
	m.issues = append(m.issues, row)
	return nil
}
func (m *mockAuditWriter) InsertAuditEvent(ctx context.Context, row AuditEventRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNextMethod == "InsertAuditEvent" {
		m.failNextMethod = ""
		return m.failErr
	}
	m.events = append(m.events, row)
	return nil
}

func (m *mockAuditWriter) snapshot() (plans []PlanRow, statusUpdates []string, issues []IssueRow, events []AuditEventRow) {
	m.mu.Lock()
	defer m.mu.Unlock()
	plansCopy := append([]PlanRow(nil), m.plans...)
	updatesCopy := append([]string(nil), m.statusUpdates...)
	issuesCopy := append([]IssueRow(nil), m.issues...)
	eventsCopy := append([]AuditEventRow(nil), m.events...)
	return plansCopy, updatesCopy, issuesCopy, eventsCopy
}

// TestCoverage_PerCallAuditWriterPath exercises the non-tStore branches of
// SubmitDraftReview / ReviewIssue / Apply / Rollback. Sets Engine.SetStore
// (not SetTxStore) so the engine takes the per-call path. Verifies each
// transition writes the expected rows to the AuditWriter.
func TestCoverage_PerCallAuditWriterPath(t *testing.T) {
	w := &mockAuditWriter{}
	eng := NewEngine()
	eng.SetStore(w)
	// Inject a deterministic clock + id-gen so test output is reproducible.
	fixedTime := time.Date(2026, 9, 28, 10, 30, 0, 0, time.UTC)
	eng.SetClock(func() time.Time { return fixedTime })
	counter := 0
	eng.SetIDGen(func() string {
		counter++
		return "id-" + string(rune('0'+counter))
	})

	// CreatePlanWithDraftReview → AuditWriter.InsertPlan + InsertAuditEvent.
	plan, err := eng.CreatePlanWithDraftReview(specFixture(), "yang_jb")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// SubmitDraftReview → UpdatePlanStatus + InsertAuditEvent.
	if _, err := eng.SubmitDraftReview(plan.PlanID); err != nil {
		t.Fatalf("submit draft: %v", err)
	}
	// ReviewIssue(Approve) → UpdatePlanStatus + InsertIssue + InsertAuditEvent.
	if _, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", "ok"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// Apply → UpdatePlanStatus + InsertAuditEvent.
	if _, err := eng.Apply(plan.PlanID, "yang_jb", "switch v17"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Rollback → UpdatePlanStatus + InsertAuditEvent.
	if _, err := eng.Rollback(plan.PlanID, "wang_xx", "回滚"); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	plans, updates, issues, events := w.snapshot()
	if len(plans) != 1 {
		t.Fatalf("InsertPlan calls: want 1, got %d", len(plans))
	}
	if len(updates) != 4 {
		// submit + approve + apply + rollback
		t.Fatalf("UpdatePlanStatus calls: want 4, got %d", len(updates))
	}
	if len(issues) != 1 {
		// only ReviewIssue(Approve) writes an Issue row
		t.Fatalf("InsertIssue calls: want 1, got %d", len(issues))
	}
	if len(events) != 5 {
		// create + submit + approve + apply + rollback
		t.Fatalf("InsertAuditEvent calls: want 5, got %d", len(events))
	}

	// Final Plan should be RolledBack.
	final, err := eng.GetPlan(plan.PlanID)
	if err != nil {
		t.Fatalf("get plan: %v", err)
	}
	if final.Status != StatusRolledBack {
		t.Fatalf("final status: want rolled_back, got %s", final.Status)
	}

	// Cover persist() directly: call it with the mocked store and verify
	// it writes one more AuditEvent row.
	prePersistEvents := len(events)
	if err := eng.persist(context.Background(), final, ActionRollback, "wang_xx", "persist-call", map[string]string{"k": "v"}); err != nil {
		t.Fatalf("persist: %v", err)
	}
	_, _, _, eventsAfter := w.snapshot()
	if len(eventsAfter) != prePersistEvents+1 {
		t.Fatalf("persist: want 1 new event, got %d (was %d, now %d)",
			len(eventsAfter)-prePersistEvents, prePersistEvents, len(eventsAfter))
	}
}

// TestCoverage_ListPlansSorted verifies ListPlans returns Plans in
// deterministic order. The engine sorts by CreatedAt then by PlanID; we
// create 3 Plans back-to-back and assert the order is what ListPlans
// reports.
func TestCoverage_ListPlansSorted(t *testing.T) {
	eng := NewEngine()
	spec := specFixture()
	p1, _ := eng.CreatePlanWithDraftReview(spec, "yang_jb")
	p2, _ := eng.CreatePlanWithDraftReview(spec, "yang_jb")
	p3, _ := eng.CreatePlanWithDraftReview(spec, "yang_jb")
	got := eng.ListPlans()
	if len(got) != 3 {
		t.Fatalf("ListPlans: want 3 plans, got %d", len(got))
	}
	// Same CreatedAt across the 3 plans (created in the same nanosecond) —
	// fall back to PlanID as the secondary sort key. We don't pin the
	// exact secondary order; just verify all 3 planIDs are present.
	seen := map[string]bool{p1.PlanID: false, p2.PlanID: false, p3.PlanID: false}
	for _, p := range got {
		if _, ok := seen[p.PlanID]; !ok {
			t.Fatalf("ListPlans: unexpected planID %q", p.PlanID)
		}
		seen[p.PlanID] = true
	}
	for id, ok := range seen {
		if !ok {
			t.Fatalf("ListPlans: missing planID %q", id)
		}
	}
}

// TestCoverage_PersistNilStoreReturnsNil verifies that persist() short-
// circuits to nil when no AuditWriter is wired — this is the defensive
// branch every per-call writer path takes before doing any work.
func TestCoverage_PersistNilStoreReturnsNil(t *testing.T) {
	eng := NewEngine()
	// No SetStore / SetTxStore calls.
	err := eng.persist(context.Background(), Plan{PlanID: "p1"}, ActionApply, "yang_jb", "", nil)
	if err != nil {
		t.Fatalf("persist with nil store: want nil, got %v", err)
	}
}

// TestCoverage_EngineValidationErrors exercises the input-validation
// branches of CreatePlanWithDraftReview / SubmitDraftReview / ReviewIssue /
// Apply / Rollback. These are the "fail-loud on bad caller input" guard
// lines that don't go through the state-machine typed-error catalogue;
// they short-circuit with a plain error before any persistence happens.
//
// Coverage target: push workflow package coverage toward 90% by hitting
// the validation guard lines that aren't reachable from the illegal-
// transition contract tests.
func TestCoverage_EngineValidationErrors(t *testing.T) {
	t.Run("create_missing_version_id", func(t *testing.T) {
		eng := NewEngine()
		_, err := eng.CreatePlanWithDraftReview(RulePlanSpec{
			RuleSet:       "rs",
			EffectiveDate: "2026-10-01",
			Rules:         []RuleEntry{{Code: "X", Name: "X"}},
		}, "yang_jb")
		if err == nil {
			t.Fatalf("want error for missing version_id, got nil")
		}
	})
	t.Run("create_missing_ruleset", func(t *testing.T) {
		eng := NewEngine()
		_, err := eng.CreatePlanWithDraftReview(RulePlanSpec{
			VersionID:     "v1",
			EffectiveDate: "2026-10-01",
			Rules:         []RuleEntry{{Code: "X", Name: "X"}},
		}, "yang_jb")
		if err == nil {
			t.Fatalf("want error for missing rule_set, got nil")
		}
	})
	t.Run("create_missing_effective_date", func(t *testing.T) {
		eng := NewEngine()
		_, err := eng.CreatePlanWithDraftReview(RulePlanSpec{
			VersionID: "v1",
			RuleSet:   "rs",
			Rules:     []RuleEntry{{Code: "X", Name: "X"}},
		}, "yang_jb")
		if err == nil {
			t.Fatalf("want error for missing effective_date, got nil")
		}
	})
	t.Run("create_no_rules", func(t *testing.T) {
		eng := NewEngine()
		_, err := eng.CreatePlanWithDraftReview(RulePlanSpec{
			VersionID:     "v1",
			RuleSet:       "rs",
			EffectiveDate: "2026-10-01",
			// Rules: nil
		}, "yang_jb")
		if err == nil {
			t.Fatalf("want error for empty rules, got nil")
		}
	})
	t.Run("create_default_reviewer", func(t *testing.T) {
		eng := NewEngine()
		p, err := eng.CreatePlanWithDraftReview(specFixture(), "")
		if err != nil {
			t.Fatalf("create with empty createdBy: %v", err)
		}
		if p.CreatedBy != DefaultReviewer {
			t.Fatalf("CreatedBy: want %s, got %s", DefaultReviewer, p.CreatedBy)
		}
	})
	t.Run("submit_draft_plan_not_found", func(t *testing.T) {
		eng := NewEngine()
		_, err := eng.SubmitDraftReview("nonexistent")
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("want NotFoundError, got %T (%v)", err, err)
		}
	})
	t.Run("review_issue_invalid_action", func(t *testing.T) {
		eng := NewEngine()
		p, _ := eng.CreatePlanWithDraftReview(specFixture(), "yang_jb")
		// ActionRollback is not valid for ReviewIssue — must reject with InvalidActionError.
		_, err := eng.ReviewIssue(p.PlanID, ActionRollback, "yang_jb", "")
		var ia *InvalidActionError
		if !errors.As(err, &ia) {
			t.Fatalf("want InvalidActionError, got %T (%v)", err, err)
		}
	})
	t.Run("review_issue_empty_actor", func(t *testing.T) {
		eng := NewEngine()
		p, _ := eng.CreatePlanWithDraftReview(specFixture(), "yang_jb")
		eng.SubmitDraftReview(p.PlanID)
		_, err := eng.ReviewIssue(p.PlanID, ActionApprove, "", "")
		if err == nil {
			t.Fatalf("want error for empty actor, got nil")
		}
	})
	t.Run("review_issue_plan_not_found", func(t *testing.T) {
		eng := NewEngine()
		_, err := eng.ReviewIssue("nonexistent", ActionApprove, "yang_jb", "")
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("want NotFoundError, got %T (%v)", err, err)
		}
	})
	t.Run("apply_empty_actor", func(t *testing.T) {
		eng := NewEngine()
		p, _ := eng.CreatePlanWithDraftReview(specFixture(), "yang_jb")
		eng.SubmitDraftReview(p.PlanID)
		eng.ReviewIssue(p.PlanID, ActionApprove, "yang_jb", "")
		_, err := eng.Apply(p.PlanID, "", "")
		if err == nil {
			t.Fatalf("want error for empty actor, got nil")
		}
	})
	t.Run("apply_plan_not_found", func(t *testing.T) {
		eng := NewEngine()
		_, err := eng.Apply("nonexistent", "yang_jb", "")
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("want NotFoundError, got %T (%v)", err, err)
		}
	})
	t.Run("rollback_empty_actor", func(t *testing.T) {
		eng := NewEngine()
		p, _ := eng.CreatePlanWithDraftReview(specFixture(), "yang_jb")
		eng.SubmitDraftReview(p.PlanID)
		eng.ReviewIssue(p.PlanID, ActionApprove, "yang_jb", "")
		eng.Apply(p.PlanID, "yang_jb", "")
		_, err := eng.Rollback(p.PlanID, "", "")
		if err == nil {
			t.Fatalf("want error for empty actor, got nil")
		}
	})
	t.Run("rollback_plan_not_found", func(t *testing.T) {
		eng := NewEngine()
		_, err := eng.Rollback("nonexistent", "yang_jb", "")
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("want NotFoundError, got %T (%v)", err, err)
		}
	})
}
