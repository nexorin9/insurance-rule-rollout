// Tests for the ReviewIssue + Apply public API surface (Task 16).
// Complements engine_tx_test.go (which pins down WithTx transaction
// semantics) by exercising the operator-facing behaviour: 5 legal
// ReviewIssue cases + 3 illegal ReviewIssue cases with typed-error
// assertions.
//
// What's covered (8 tests, table-driven through driveReviewTestPlan):
//
//	legal (5):
//	  TestReviewIssue_ApproveFromPending           — happy path Pending → Approved
//	  TestReviewIssue_RejectFromPending            — Pending → Rejected (terminal)
//	  TestReviewIssue_RequestFromPendingFirstTime  — Pending → Pending (self-loop, first time)
//	  TestReviewIssue_ApproveWithDetailedComment   — comment propagates to IssueRow + AuditEvent Payload
//	  TestReviewIssue_ApproveSecondPlan            — two parallel Plans, independent state transitions
//
//	illegal (3, each with errors.As typed-error assertion):
//	  TestReviewIssue_IllegalDraftApprove          — (Draft, approve) → *ErrDraftApprove
//	  TestReviewIssue_IllegalPendingRequestTwice   — (Pending, request) ×2 → *ErrPendingSelfLoop
//	  TestReviewIssue_IllegalApprovedRequest       — (Approved, request) → *ErrApprovedRequest
//
// Note: contract tests that pin Plan-state-doesn't-mutate + AuditEvent-doesn't-grow
// live in contracts_test.go (Task 17). SQLite-shared-cache + concurrent-goroutine
// integration tests live in integration_test.go (Task 18). Both are deliberately
// out of scope here.
package workflow

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// driveReviewTestPlan drives a freshly-created Plan into the requested
// status using the engine's public API. The mock store is seeded with the
// matching row at each step so subsequent ReviewIssue / Apply calls (which
// call LockPlan) can find it.
//
// Status flow:
//   - StatusDraft     — CreatePlanWithDraftReview (mock store seeded with Draft row)
//   - StatusPending   — CreatePlanWithDraftReview → SubmitDraftReview (mock store seeded with Pending row)
//   - StatusApproved  — … → ReviewIssue(Approve) (mock store updated via TxUpdatePlanStatus)
//   - StatusApplied   — … → Apply (mock store updated via TxUpdatePlanStatus)
func driveReviewTestPlan(t *testing.T, eng *Engine, st *mockTxStore, target PlanStatus) Plan {
	t.Helper()
	spec := RulePlanSpec{
		VersionID:     "r-test",
		RuleSet:       "rs-test",
		EffectiveDate: "2026-10-01",
		Rules:         []RuleEntry{{Code: "X", Name: "X", Weight: 1, Limit: 1}},
	}
	plan, err := eng.CreatePlanWithDraftReview(spec, "yang_jb")
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	// Always seed the freshly-created Draft row so LockPlan can find it.
	st.insertPlan(Plan{
		PlanID: plan.PlanID, Status: StatusDraft, Draft: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if target == StatusDraft {
		return plan
	}
	if _, err := eng.SubmitDraftReview(plan.PlanID); err != nil {
		t.Fatalf("submit draft: %v", err)
	}
	// SubmitDraftReview doesn't go through tStore — mirror manually so
	// ReviewIssue's LockPlan can read (Status: Pending, Draft: false).
	st.insertPlan(Plan{
		PlanID: plan.PlanID, Status: StatusPending, Draft: false,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	})
	if target == StatusPending {
		return plan
	}
	if _, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", "setup approve"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// After ReviewIssue(Approve), the tStore has (Status: Approved, Draft: false, LastAction: approve).
	if target == StatusApproved {
		return plan
	}
	if _, err := eng.Apply(plan.PlanID, "yang_jb", "setup apply"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return plan
}

// ----- 5 LEGAL ReviewIssue cases -----

// Case 1: Pending + Approve → Approved (basic happy path).
// Verifies Status advances, LastAction/LastActionAt are set, the IssueRow
// records actor+comment, and one AuditEvent row is committed.
func TestReviewIssue_ApproveFromPending(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan := driveReviewTestPlan(t, eng, st, StatusPending)

	got, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", "approve comment")
	if err != nil {
		t.Fatalf("review issue: %v", err)
	}
	if got.Status != StatusApproved {
		t.Fatalf("status: want approved, got %s", got.Status)
	}
	if got.LastAction != ActionApprove {
		t.Fatalf("last action: want approve, got %s", got.LastAction)
	}
	if got.LastActionAt.IsZero() {
		t.Fatalf("LastActionAt should be set, got zero time")
	}

	plans, issues, events, committed, rolledBack := st.snapshot()
	if committed != 1 || rolledBack != 0 {
		t.Fatalf("tx counter: want committed=1 rolledBack=0, got committed=%d rolledBack=%d", committed, rolledBack)
	}
	if plans[plan.PlanID].Status != StatusApproved {
		t.Fatalf("mock plan status: want approved, got %s", plans[plan.PlanID].Status)
	}
	if len(issues) != 1 {
		t.Fatalf("issues: want 1, got %d", len(issues))
	}
	if issues[0].Action != ActionApprove || issues[0].Actor != "yang_jb" || issues[0].Comment != "approve comment" {
		t.Fatalf("issue row mismatch: action=%s actor=%s comment=%q", issues[0].Action, issues[0].Actor, issues[0].Comment)
	}
	if len(events) != 1 {
		t.Fatalf("events: want 1, got %d", len(events))
	}
	if events[0].Action != ActionApprove {
		t.Fatalf("event action: want approve, got %s", events[0].Action)
	}
	if events[0].Actor != "yang_jb" {
		t.Fatalf("event actor: want yang_jb, got %s", events[0].Actor)
	}
}

// Case 2: Pending + Reject → Rejected (terminal state).
// Verifies Status flips to Rejected, the IssueRow records the rejecter's
// actor + reject comment, and the AuditEvent row is committed.
func TestReviewIssue_RejectFromPending(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan := driveReviewTestPlan(t, eng, st, StatusPending)

	got, err := eng.ReviewIssue(plan.PlanID, ActionReject, "wang_xx", "reject: missing diagnosis fields")
	if err != nil {
		t.Fatalf("review issue: %v", err)
	}
	if got.Status != StatusRejected {
		t.Fatalf("status: want rejected, got %s", got.Status)
	}
	if got.LastAction != ActionReject {
		t.Fatalf("last action: want reject, got %s", got.LastAction)
	}

	plans, _, events, committed, rolledBack := st.snapshot()
	if committed != 1 || rolledBack != 0 {
		t.Fatalf("tx counter: want committed=1 rolledBack=0, got committed=%d rolledBack=%d", committed, rolledBack)
	}
	if plans[plan.PlanID].Status != StatusRejected {
		t.Fatalf("mock plan status: want rejected, got %s", plans[plan.PlanID].Status)
	}
	if len(events) != 1 || events[0].Actor != "wang_xx" {
		t.Fatalf("audit event: want 1 row with actor wang_xx, got %d (actor=%s)", len(events), events[0].Actor)
	}
}

// Case 3: Pending + Request (first time) → Pending (self-loop).
// Engine updates LastAction=Request and Plan.LastActionAt without changing
// Status. The locked row in the tStore also reflects the updated
// LastAction/LastActionAt so a second call to Request can detect the reuse.
func TestReviewIssue_RequestFromPendingFirstTime(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan := driveReviewTestPlan(t, eng, st, StatusPending)

	got, err := eng.ReviewIssue(plan.PlanID, ActionRequest, "zhao_bz", "please add ICD-10 mapping for DRG-X999")
	if err != nil {
		t.Fatalf("review issue: %v", err)
	}
	if got.Status != StatusPending {
		t.Fatalf("status: want pending (self-loop), got %s", got.Status)
	}
	if got.LastAction != ActionRequest {
		t.Fatalf("last action: want request, got %s", got.LastAction)
	}
	if got.LastActionAt.IsZero() {
		t.Fatalf("LastActionAt should advance on first request, got zero time")
	}

	plans, issues, events, committed, rolledBack := st.snapshot()
	if committed != 1 || rolledBack != 0 {
		t.Fatalf("tx counter: want committed=1 rolledBack=0, got committed=%d rolledBack=%d", committed, rolledBack)
	}
	if plans[plan.PlanID].Status != StatusPending {
		t.Fatalf("mock plan status: want pending, got %s", plans[plan.PlanID].Status)
	}
	if len(issues) != 1 || issues[0].Action != ActionRequest {
		t.Fatalf("issue row: want 1 request issue, got %d (action=%s)", len(issues), issues[0].Action)
	}
	if len(events) != 1 {
		t.Fatalf("audit event: want 1 request event, got %d", len(events))
	}
	if events[0].Action != ActionRequest {
		t.Fatalf("event action: want request, got %s", events[0].Action)
	}
}

// Case 4: Pending + Approve with a multi-segment Chinese operator comment.
// Verifies the comment is preserved verbatim on the IssueRow AND embedded in
// the AuditEvent Payload map under the "comment" key, alongside from_status
// and to_status (these three fields are what fly-inspect reports key on).
func TestReviewIssue_ApproveWithDetailedComment(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan := driveReviewTestPlan(t, eng, st, StatusPending)

	comment := "已核对影响面：87 名在院患者，新增 DRG-A001, A002；旧科室裕度支持"
	got, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", comment)
	if err != nil {
		t.Fatalf("review issue: %v", err)
	}
	if got.Status != StatusApproved {
		t.Fatalf("status: want approved, got %s", got.Status)
	}

	_, issues, events, _, _ := st.snapshot()
	if len(issues) != 1 {
		t.Fatalf("issue row: want 1, got %d", len(issues))
	}
	if issues[0].Comment != comment {
		t.Fatalf("issue comment: want %q, got %q", comment, issues[0].Comment)
	}
	if len(events) != 1 {
		t.Fatalf("audit event: want 1, got %d", len(events))
	}
	payload := events[0].Payload
	if payload["comment"] != comment {
		t.Fatalf("audit payload comment: want %q, got %q", comment, payload["comment"])
	}
	if payload["from_status"] != string(StatusPending) {
		t.Fatalf("audit payload from_status: want pending, got %s", payload["from_status"])
	}
	if payload["to_status"] != string(StatusApproved) {
		t.Fatalf("audit payload to_status: want approved, got %s", payload["to_status"])
	}
}

// Case 5: Two parallel Plans, both Pending; approve each independently.
// Verifies the engine state isn't cross-contaminated (plan A approve doesn't
// touch plan B) and that each Plan gets its own IssueRow + AuditEvent row.
func TestReviewIssue_ApproveSecondPlan(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	planA := driveReviewTestPlan(t, eng, st, StatusPending)
	planB := driveReviewTestPlan(t, eng, st, StatusPending)

	gotA, err := eng.ReviewIssue(planA.PlanID, ActionApprove, "yang_jb", "approve A")
	if err != nil {
		t.Fatalf("review A: %v", err)
	}
	if gotA.Status != StatusApproved {
		t.Fatalf("plan A status: want approved, got %s", gotA.Status)
	}
	gotB, err := eng.ReviewIssue(planB.PlanID, ActionApprove, "wang_xx", "approve B")
	if err != nil {
		t.Fatalf("review B: %v", err)
	}
	if gotB.Status != StatusApproved {
		t.Fatalf("plan B status: want approved, got %s", gotB.Status)
	}

	plans, issues, events, committed, rolledBack := st.snapshot()
	if committed != 2 || rolledBack != 0 {
		t.Fatalf("tx counter: want committed=2 rolledBack=0, got committed=%d rolledBack=%d", committed, rolledBack)
	}
	if plans[planA.PlanID].Status != StatusApproved {
		t.Fatalf("mock plan A status: want approved, got %s", plans[planA.PlanID].Status)
	}
	if plans[planB.PlanID].Status != StatusApproved {
		t.Fatalf("mock plan B status: want approved, got %s", plans[planB.PlanID].Status)
	}
	if len(issues) != 2 {
		t.Fatalf("issues: want 2, got %d", len(issues))
	}
	if len(events) != 2 {
		t.Fatalf("events: want 2, got %d", len(events))
	}
	issueByPlan := map[string]int{}
	eventByPlan := map[string]int{}
	for _, iss := range issues {
		issueByPlan[iss.PlanID]++
	}
	for _, ev := range events {
		eventByPlan[ev.PlanID]++
	}
	if issueByPlan[planA.PlanID] != 1 || issueByPlan[planB.PlanID] != 1 {
		t.Fatalf("issue plan counts: want one each, got %v", issueByPlan)
	}
	if eventByPlan[planA.PlanID] != 1 || eventByPlan[planB.PlanID] != 1 {
		t.Fatalf("event plan counts: want one each, got %v", eventByPlan)
	}
}

// ----- 3 ILLEGAL ReviewIssue cases with typed-error assertions -----

// Illegal Case 1: (Draft, Approve) → *ErrDraftApprove.
// The IsDraft pre-tx check fires BEFORE WithTx; neither commit nor rollback
// counter increments. Plan status remains Draft; no Issue / AuditEvent rows
// are appended. The error message contains both the offending status ("draft")
// and action ("approve") so fly-inspect greps work.
func TestReviewIssue_IllegalDraftApprove(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan := driveReviewTestPlan(t, eng, st, StatusDraft)

	_, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", "should fail on draft")
	if err == nil {
		t.Fatalf("review on draft: want error, got nil")
	}
	var typedErr *ErrDraftApprove
	if !errors.As(err, &typedErr) {
		t.Fatalf("want ErrDraftApprove, got %T (%v)", err, err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "draft") {
		t.Fatalf("error message must mention 'draft', got %q", err.Error())
	}
	if !strings.Contains(strings.ToLower(err.Error()), "approve") {
		t.Fatalf("error message must mention 'approve', got %q", err.Error())
	}

	plans, issues, events, committed, rolledBack := st.snapshot()
	// The IsDraft check fires BEFORE WithTx — neither counter increments.
	if committed != 0 || rolledBack != 0 {
		t.Fatalf("tx counter: want committed=0 rolledBack=0 (pre-tx error), got committed=%d rolledBack=%d", committed, rolledBack)
	}
	if plans[plan.PlanID].Status != StatusDraft {
		t.Fatalf("plan status must be unchanged (draft), got %s", plans[plan.PlanID].Status)
	}
	if len(issues) != 0 {
		t.Fatalf("issues after illegal review: want 0, got %d", len(issues))
	}
	if len(events) != 0 {
		t.Fatalf("events after illegal review: want 0, got %d", len(events))
	}
}

// Illegal Case 2: (Pending, Request) called twice → *ErrPendingSelfLoop.
// First Request: legal self-loop, WithTx commits (committed=1).
// Second Request: NextStatusForPlan detects Plan.LastAction == ActionRequest
// and returns ErrPendingSelfLoop; WithTx rolls back (rolledBack=1).
// Net counter: committed=1, rolledBack=1. Plan status remains Pending.
func TestReviewIssue_IllegalPendingRequestTwice(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan := driveReviewTestPlan(t, eng, st, StatusPending)

	// First Request: legal self-loop.
	got, err := eng.ReviewIssue(plan.PlanID, ActionRequest, "yang_jb", "first request")
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	if got.Status != StatusPending {
		t.Fatalf("after first request: want pending, got %s", got.Status)
	}

	// Second Request: illegal self-loop.
	_, err = eng.ReviewIssue(plan.PlanID, ActionRequest, "yang_jb", "second request")
	if err == nil {
		t.Fatalf("second request: want error, got nil")
	}
	var typedErr *ErrPendingSelfLoop
	if !errors.As(err, &typedErr) {
		t.Fatalf("want ErrPendingSelfLoop, got %T (%v)", err, err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "pending") {
		t.Fatalf("error message must mention 'pending', got %q", err.Error())
	}

	plans, issues, events, committed, rolledBack := st.snapshot()
	// First Request commits (legal); second Request self-loops inside WithTx
	// and triggers Rollback. Counter: committed=1 rolledBack=1.
	if committed != 1 || rolledBack != 1 {
		t.Fatalf("tx counter: want committed=1 rolledBack=1 (first commits, second rolls back), got committed=%d rolledBack=%d", committed, rolledBack)
	}
	if plans[plan.PlanID].Status != StatusPending {
		t.Fatalf("plan status must be unchanged (pending), got %s", plans[plan.PlanID].Status)
	}
	if len(issues) != 1 {
		t.Fatalf("issues after illegal review: want 1 (only first request), got %d", len(issues))
	}
	if len(events) != 1 {
		t.Fatalf("events after illegal review: want 1 (only first request), got %d", len(events))
	}
}

// Illegal Case 3: (Approved, Request) → *ErrApprovedRequest.
// The pre-tx IsDraft check passes (Draft=false), so WithTx IS entered.
// LockPlan succeeds, locked.IsDraft()=false, NextStatusForPlan returns
// ErrApprovedRequest from typedErrorFor; fn returns this error;
// WithTx rolls back. driveReviewTestPlan(StatusApproved) commits one
// ReviewIssue(Approve) to reach Approved, then the illegal Request rolls
// back. Net counter: committed=1, rolledBack=1. Plan status remains
// Approved; no Issue / AuditEvent rows are appended from the illegal call.
func TestReviewIssue_IllegalApprovedRequest(t *testing.T) {
	st := newMockTxStore()
	eng := NewEngine()
	eng.SetTxStore(st)

	plan := driveReviewTestPlan(t, eng, st, StatusApproved)

	_, err := eng.ReviewIssue(plan.PlanID, ActionRequest, "zhao_bz", "should fail on approved")
	if err == nil {
		t.Fatalf("request on approved: want error, got nil")
	}
	var typedErr *ErrApprovedRequest
	if !errors.As(err, &typedErr) {
		t.Fatalf("want ErrApprovedRequest, got %T (%v)", err, err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "approved") {
		t.Fatalf("error message must mention 'approved', got %q", err.Error())
	}
	if !strings.Contains(strings.ToLower(err.Error()), "request") {
		t.Fatalf("error message must mention 'request', got %q", err.Error())
	}

	plans, issues, events, committed, rolledBack := st.snapshot()
	// driveReviewTestPlan committed 1 ReviewIssue(Approve); the illegal
	// Request inside NextStatusForPlan caused the WithTx fn to return
	// ErrApprovedRequest, triggering Rollback. Net: committed=1 rolledBack=1.
	if committed != 1 || rolledBack != 1 {
		t.Fatalf("tx counter: want committed=1 rolledBack=1 (setup approve commits, illegal request rolls back), got committed=%d rolledBack=%d", committed, rolledBack)
	}
	if plans[plan.PlanID].Status != StatusApproved {
		t.Fatalf("plan status must be unchanged (approved), got %s", plans[plan.PlanID].Status)
	}
	// Setup approve wrote 1 Issue + 1 AuditEvent; the illegal Request must
	// have added none. Confirm counter-by-counter stability.
	if len(issues) != 1 {
		t.Fatalf("issues after illegal review: want 1 (only setup approve), got %d", len(issues))
	}
	if len(events) != 1 {
		t.Fatalf("events after illegal review: want 1 (only setup approve), got %d", len(events))
	}
}