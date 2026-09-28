// Table-driven tests for the workflow state machine. Task 14 calls for 19
// table-driven cases organised as:
//
//   - 5 legal transitions (Draft+SubmitDraft, Pending+Approve, Pending+Reject,
//     Approved+Apply, Applied+Rollback)
//   - 9 named illegal-transition slots (per Task 13 catalogue), including the
//     second-Request self-loop slot which is tested via NextStatusForPlan
//   - 2 terminal-state guards (Rejected rejects all, RolledBack rejects all)
//   - 3 action-state mappings (each action's reachability per state)
//
// Plus: LegalTransitions slice ↔ legalTransitions map consistency check,
// NextStatusForPlan wiring, and engine-level happy/rejection/rollback paths.
package workflow

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// --- Transition-table pure tests ---------------------------------------------

// TestCanTransition_Legal covers the 5 canonical legal transitions.
func TestCanTransition_Legal(t *testing.T) {
	cases := []struct {
		name   string
		from   PlanStatus
		action IssueAction
		want   PlanStatus
	}{
		{"draft+submit_draft", StatusDraft, ActionSubmitDraft, StatusPending},
		{"pending+approve", StatusPending, ActionApprove, StatusApproved},
		{"pending+reject", StatusPending, ActionReject, StatusRejected},
		{"approved+apply", StatusApproved, ActionApply, StatusApplied},
		{"applied+rollback", StatusApplied, ActionRollback, StatusRolledBack},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CanTransition(tc.from, tc.action)
			if !ok {
				t.Fatalf("expected legal transition, got miss")
			}
			if got != tc.want {
				t.Fatalf("want %s, got %s", tc.want, got)
			}
		})
	}
}

// TestCanTransition_SelfLoop covers the (Pending, Request) self-loop. The
// first Request is legal at the table layer; NextStatusForPlan layers the
// second-Request guard on top via Plan.LastAction.
func TestCanTransition_SelfLoop(t *testing.T) {
	got, ok := CanTransition(StatusPending, ActionRequest)
	if !ok {
		t.Fatalf("expected self-loop legal at table layer")
	}
	if got != StatusPending {
		t.Fatalf("want self-loop to Pending, got %s", got)
	}
}

// TestCanTransition_LegalTransitionsMatchesMap enforces the slice ↔ map
// invariant: LegalTransitions (slice) and legalTransitions (map) must agree on
// every (from, action) pair. If they ever drift, fail loudly.
func TestCanTransition_LegalTransitionsMatchesMap(t *testing.T) {
	// Every row in LegalTransitions must be reachable via the map.
	for _, r := range LegalTransitions {
		to, ok := CanTransition(r.From, r.Action)
		if !ok {
			t.Errorf("LegalTransitions row (%s, %s) missing from runtime map", r.From, r.Action)
			continue
		}
		if to != r.To {
			t.Errorf("LegalTransitions row (%s, %s, %s) but map returns %s", r.From, r.Action, r.To, to)
		}
	}
	// SelfLoop must also be present.
	to, ok := CanTransition(SelfLoopTransition.From, SelfLoopTransition.Action)
	if !ok || to != SelfLoopTransition.To {
		t.Errorf("SelfLoopTransition (%s, %s) missing from runtime map", SelfLoopTransition.From, SelfLoopTransition.Action)
	}
}

func TestNextStatus_NineIllegalTransitions(t *testing.T) {
	// Each (from, action, errorType) triple pins a specific IllegalTransitionError.
	// errTarget uses `any` (not `error`) so errors.As can use &tc.errTarget as *any.
	cases := []struct {
		name      string
		from      PlanStatus
		action    IssueAction
		errTarget any
	}{
		{
			name:      "draft_approve",
			from:      StatusDraft,
			action:    ActionApprove,
			errTarget: &ErrDraftApprove{},
		},
		{
			name:      "approved_approve_reapprove",
			from:      StatusApproved,
			action:    ActionApprove,
			errTarget: &ErrPendingReApprove{},
		},
		{
			name:      "rejected_approve",
			from:      StatusRejected,
			action:    ActionApprove,
			errTarget: &ErrRejectedReApprove{},
		},
		{
			name:      "approved_request",
			from:      StatusApproved,
			action:    ActionRequest,
			errTarget: &ErrApprovedRequest{},
		},
		{
			name:      "applied_reject",
			from:      StatusApplied,
			action:    ActionReject,
			errTarget: &ErrAppliedReject{},
		},
		{
			name:      "rolled_back_approve",
			from:      StatusRolledBack,
			action:    ActionApprove,
			errTarget: &ErrRolledBackAny{},
		},
		{
			name:      "rolled_back_reject",
			from:      StatusRolledBack,
			action:    ActionReject,
			errTarget: &ErrRolledBackAny{},
		},
		{
			name:      "applied_reapprove",
			from:      StatusApplied,
			action:    ActionApprove,
			errTarget: &ErrAppliedReApprove{},
		},
		{
			name:      "rejected_apply",
			from:      StatusRejected,
			action:    ActionApply,
			errTarget: &ErrRejectedApply{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NextStatus(tc.from, tc.action)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.As(err, &tc.errTarget) {
				t.Fatalf("want error type %T, got %T (%v)", tc.errTarget, err, err)
			}
			// Error message must mention status name + action name.
			msg := err.Error()
			if !contains(msg, string(tc.from)) {
				t.Fatalf("error message missing status %q: %s", tc.from, msg)
			}
			if !contains(msg, string(tc.action)) {
				t.Fatalf("error message missing action %q: %s", tc.action, msg)
			}
		})
	}
}

// TestNextStatus_PendingSelfLoopGuard verifies the engine-level self-loop guard:
// the (Pending, Request) second call must fail with ErrPendingSelfLoop.
func TestNextStatus_PendingSelfLoopGuard(t *testing.T) {
	// First Request on Pending is legal; the engine-level guard (Plan.
	// LastAction == ActionRequest) should kick in on the second.
	p := Plan{
		Status:       StatusPending,
		LastAction:   ActionRequest,
		LastActionAt: time.Date(2026, 9, 28, 10, 30, 0, 0, time.UTC),
	}
	_, err := NextStatusForPlan(p, ActionRequest)
	var target *ErrPendingSelfLoop
	if !errors.As(err, &target) {
		t.Fatalf("expected ErrPendingSelfLoop, got %T (%v)", err, err)
	}
	if !contains(err.Error(), "Pending plans may Request only once") {
		t.Fatalf("expected self-loop guard message, got: %s", err.Error())
	}
}

// TestNextStatus_TerminalState_Rejected asserts that Rejected rejects every
// action (Approve / Reject / Request / Apply / SubmitDraft / Rollback). This is
// one of the two terminal-state guards required by Task 14.
func TestNextStatus_TerminalState_Rejected(t *testing.T) {
	actions := []IssueAction{
		ActionApprove, ActionReject, ActionRequest,
		ActionApply, ActionSubmitDraft, ActionRollback,
	}
	for _, act := range actions {
		t.Run(string(act), func(t *testing.T) {
			_, err := NextStatus(StatusRejected, act)
			if err == nil {
				t.Fatalf("Rejected should reject %s, got nil", act)
			}
		})
	}
}

// TestNextStatus_TerminalState_RolledBack asserts that RolledBack rejects
// every action. This is the second terminal-state guard required by Task 14.
func TestNextStatus_TerminalState_RolledBack(t *testing.T) {
	actions := []IssueAction{
		ActionApprove, ActionReject, ActionRequest,
		ActionApply, ActionSubmitDraft, ActionRollback,
	}
	for _, act := range actions {
		t.Run(string(act), func(t *testing.T) {
			_, err := NextStatus(StatusRolledBack, act)
			if err == nil {
				t.Fatalf("RolledBack should reject %s, got nil", act)
			}
		})
	}
}

// TestNextStatus_ActionStateMapping_SubmitDraft verifies SubmitDraft is only
// valid from Draft. One of the three action-state-mapping tests.
func TestNextStatus_ActionStateMapping_SubmitDraft(t *testing.T) {
	// Legal: Draft → Pending via SubmitDraft.
	to, err := NextStatus(StatusDraft, ActionSubmitDraft)
	if err != nil || to != StatusPending {
		t.Fatalf("expected (Draft, SubmitDraft) → Pending, got (%s, %v)", to, err)
	}
	// Illegal: every other state rejects SubmitDraft.
	other := []PlanStatus{StatusPending, StatusApproved, StatusApplied, StatusRejected, StatusRolledBack}
	for _, s := range other {
		if _, err := NextStatus(s, ActionSubmitDraft); err == nil {
			t.Errorf("expected (%s, SubmitDraft) to error, got nil", s)
		}
	}
}

// TestNextStatus_ActionStateMapping_Approve verifies Approve is only valid
// from Pending. Second action-state-mapping test.
func TestNextStatus_ActionStateMapping_Approve(t *testing.T) {
	// Legal: Pending → Approved via Approve.
	to, err := NextStatus(StatusPending, ActionApprove)
	if err != nil || to != StatusApproved {
		t.Fatalf("expected (Pending, Approve) → Approved, got (%s, %v)", to, err)
	}
	// Already-Approved re-Approve surfaces ErrPendingReApprove (named slot).
	_, err = NextStatus(StatusApproved, ActionApprove)
	var target *ErrPendingReApprove
	if !errors.As(err, &target) {
		t.Fatalf("expected (Approved, Approve) → ErrPendingReApprove, got %T (%v)", err, err)
	}
	// Other states reject Approve.
	other := []PlanStatus{StatusDraft, StatusApplied, StatusRejected, StatusRolledBack}
	for _, s := range other {
		if _, err := NextStatus(s, ActionApprove); err == nil {
			t.Errorf("expected (%s, Approve) to error, got nil", s)
		}
	}
}

// TestNextStatus_ActionStateMapping_Apply verifies Apply is only valid from
// Approved. Third action-state-mapping test.
func TestNextStatus_ActionStateMapping_Apply(t *testing.T) {
	// Legal: Approved → Applied via Apply.
	to, err := NextStatus(StatusApproved, ActionApply)
	if err != nil || to != StatusApplied {
		t.Fatalf("expected (Approved, Apply) → Applied, got (%s, %v)", to, err)
	}
	// Other states reject Apply (Rejection Apply specifically → ErrRejectedApply).
	_, err = NextStatus(StatusRejected, ActionApply)
	var rejectedTarget *ErrRejectedApply
	if !errors.As(err, &rejectedTarget) {
		t.Fatalf("expected (Rejected, Apply) → ErrRejectedApply, got %T (%v)", err, err)
	}
	// Pending/Draft/Applied/RolledBack reject Apply with InvalidActionError or
	// terminal-state errors.
	other := []PlanStatus{StatusDraft, StatusPending, StatusApplied, StatusRolledBack}
	for _, s := range other {
		if _, err := NextStatus(s, ActionApply); err == nil {
			t.Errorf("expected (%s, Apply) to error, got nil", s)
		}
	}
}

func TestNextStatus_InvalidActionFallback(t *testing.T) {
	// (Draft, Reject) is not one of the 9 named illegal slots — verify the
	// generic InvalidActionError catch-all still surfaces.
	_, err := NextStatus(StatusDraft, ActionReject)
	var target *InvalidActionError
	if !errors.As(err, &target) {
		t.Fatalf("expected InvalidActionError, got %T (%v)", err, err)
	}
}

func TestPlanStatus_IsTerminal(t *testing.T) {
	if !(StatusRejected.IsTerminal() && StatusRolledBack.IsTerminal()) {
		t.Fatalf("expected Rejected/RolledBack terminal")
	}
	for _, s := range []PlanStatus{StatusDraft, StatusPending, StatusApproved, StatusApplied} {
		if s.IsTerminal() {
			t.Fatalf("expected %s to be non-terminal", s)
		}
	}
}

// --- Engine skeleton tests --------------------------------------------------

func TestEngine_CreateAndSubmitDraftReview(t *testing.T) {
	e := NewEngine()
	p, err := e.CreatePlanWithDraftReview(specFixture(), "yang_jb")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.Status != StatusDraft {
		t.Fatalf("want Draft, got %s", p.Status)
	}
	if !p.Draft {
		t.Fatalf("want Draft=true after create")
	}

	// SubmitDraftReview advances Status Draft → Pending and flips the bool flag.
	p2, err := e.SubmitDraftReview(p.PlanID)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if p2.Draft {
		t.Fatalf("want Draft=false after submit")
	}
	if p2.Status != StatusPending {
		t.Fatalf("want Status=Pending after submit, got %s", p2.Status)
	}

	// Submitting a Plan already in Pending — second SubmitDraftReview — surfaces
	// InvalidActionError because (Pending, SubmitDraft) is not in the table.
	if _, err := e.SubmitDraftReview(p.PlanID); err == nil {
		t.Fatalf("second submit should error (Pending rejects SubmitDraft)")
	}
}

func TestEngine_HappyPath(t *testing.T) {
	e := NewEngine()
	p := mustCreate(t, e)

	mustSubmitDraft(t, e, p.PlanID)
	mustReview(t, e, p.PlanID, ActionApprove, "yang_jb", "已核对影响面")
	mustApply(t, e, p.PlanID, "wang_xx", "切换 v17")

	final, err := e.GetPlan(p.PlanID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.Status != StatusApplied {
		t.Fatalf("want Applied, got %s", final.Status)
	}
}

func TestEngine_RejectionPath(t *testing.T) {
	e := NewEngine()
	p := mustCreate(t, e)
	mustSubmitDraft(t, e, p.PlanID)
	mustReview(t, e, p.PlanID, ActionReject, "li_yw", "影响面过大，回退")

	final, err := e.GetPlan(p.PlanID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.Status != StatusRejected {
		t.Fatalf("want Rejected, got %s", final.Status)
	}
	if !final.Status.IsTerminal() {
		t.Fatalf("Rejected must be terminal")
	}

	// Calling Approve on terminal Rejected must return ErrRejectedReApprove.
	_, err = e.ReviewIssue(p.PlanID, ActionApprove, "yang_jb", "再核")
	var target *ErrRejectedReApprove
	if !errors.As(err, &target) {
		t.Fatalf("want ErrRejectedReApprove, got %T (%v)", err, err)
	}
}

func TestEngine_RollbackPath(t *testing.T) {
	e := NewEngine()
	p := mustCreate(t, e)
	mustSubmitDraft(t, e, p.PlanID)
	mustReview(t, e, p.PlanID, ActionApprove, "yang_jb", "ok")
	mustApply(t, e, p.PlanID, "wang_xx", "")
	if _, err := e.Rollback(p.PlanID, "wang_xx", "回滚 v16"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	final, err := e.GetPlan(p.PlanID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if final.Status != StatusRolledBack {
		t.Fatalf("want RolledBack, got %s", final.Status)
	}
}

func TestEngine_NotFound(t *testing.T) {
	e := NewEngine()
	if _, err := e.GetPlan("missing-plan"); err == nil {
		t.Fatalf("expected NotFoundError")
	} else {
		var nf *NotFoundError
		if !errors.As(err, &nf) {
			t.Fatalf("want NotFoundError, got %T", err)
		}
	}
}

func TestEngine_InvalidAction(t *testing.T) {
	e := NewEngine()
	p := mustCreate(t, e)
	// ReviewIssue with unsupported action (e.g. Rollback) returns InvalidActionError.
	_, err := e.ReviewIssue(p.PlanID, ActionRollback, "yang_jb", "")
	if err == nil {
		t.Fatalf("expected error for Rollback on Draft")
	}
	var ia *InvalidActionError
	if !errors.As(err, &ia) {
		t.Fatalf("want InvalidActionError, got %T (%v)", err, ia)
	}
}

// --- helpers ---------------------------------------------------------------

func specFixture() RulePlanSpec {
	return RulePlanSpec{
		VersionID:     "r-2026-09-DRG-v17",
		RuleSet:       "DRG",
		EffectiveDate: "2026-10-01",
		Rules: []RuleEntry{
			{Code: "DRG-A001", Name: "阑尾炎", Weight: 0.85, Limit: 12000.00},
		},
		PrevVersionID: "r-2026-06-DRG-v16",
		RollbackSQL:   "UPDATE his_dict SET version='r-2026-06-DRG-v16' WHERE version='r-2026-09-DRG-v17';",
	}
}

func mustCreate(t *testing.T, e *Engine) Plan {
	t.Helper()
	p, err := e.CreatePlanWithDraftReview(specFixture(), "yang_jb")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return p
}

func mustSubmitDraft(t *testing.T, e *Engine, planID string) {
	t.Helper()
	if _, err := e.SubmitDraftReview(planID); err != nil {
		t.Fatalf("submit: %v", err)
	}
}

func mustReview(t *testing.T, e *Engine, planID string, action IssueAction, actor, comment string) {
	t.Helper()
	if _, err := e.ReviewIssue(planID, action, actor, comment); err != nil {
		t.Fatalf("review %s: %v", action, err)
	}
}

func mustApply(t *testing.T, e *Engine, planID, actor, comment string) {
	t.Helper()
	if _, err := e.Apply(planID, actor, comment); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// silence unused import lints if test helpers drift
var _ = fmt.Sprintf