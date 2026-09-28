// Terminal-state guard tests (Task 19). The 8-case table pins down the rule
// that Rejected and RolledBack reject every further action:
//
//   - Rejected  × {Approve, Reject, Request, Apply}    (4 cases)
//   - RolledBack × {Approve, Reject, Request, Apply}   (4 cases)
//
// For each case the contract is:
//
//  1. errors.As picks up the typed error documented for that (status,
//     action) pair. The named slots ErrRejectedReApprove / ErrRejectedApply
//     / ErrRolledBackAny are preserved; the new typedErrorFor terminal-
//     state guard surfaces (Rejected, Reject) and (Rejected, Request) as
//     TerminalStateError rather than the generic InvalidActionError.
//  2. Plan status in BOTH the engine's in-memory cache AND the mock
//     tStore projection is UNCHANGED. A refused transition must never
//     leave a half-applied state behind.
//  3. AuditEvent count in the mock store is UNCHANGED. TxInsertAuditEvent
//     is never called during a refused transition; the transactional path
//     raises the typed error BEFORE any event row is written.
//  4. error.Error() embeds both the status name AND the action name so
//     fly-inspect / log-scan scripts can locate the refused action via
//     "status=X" or "action=Y" filters.
//
// The ReviewIssue public API surface accepts Approve / Reject / Request;
// Apply is its own public method. The 8 cases therefore split as:
//
//   - 6 cases via ReviewIssue (Rejected × {Approve, Reject, Request}
//     + RolledBack × {Approve, Reject, Request})
//   - 2 cases via Apply (Rejected × Apply + RolledBack × Apply)
//
// Driving each plan to the requested terminal state goes through
// drivePlanTo (defined in contracts_test.go) which exercises the same
// public-API surface a real operator would use.
package workflow

import (
	"errors"
	"strings"
	"testing"
)

// terminalCase encodes one row of the 8-case terminal-state table. The
// invoke closure calls either Engine.ReviewIssue or Engine.Apply depending
// on which method the action goes through; the typed-error target is the
// concrete error type the contract expects.
type terminalCase struct {
	name       string
	driveTo    PlanStatus
	invoke     func(eng *Engine, planID string) error
	errTarget  any
	actionName string
}

// TestTerminal_AllActionsRejected runs the 8 terminal-state guard cases
// through the tStore (transactional) code path so the mock store's event
// list doubles as the "AuditEvent 不增" witness.
func TestTerminal_AllActionsRejected(t *testing.T) {
	cases := []terminalCase{
		{
			name:    "rejected_approve",
			driveTo: StatusRejected,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.ReviewIssue(planID, ActionApprove, "yang_jb", "approve after reject")
				return err
			},
			errTarget:  &ErrRejectedReApprove{},
			actionName: string(ActionApprove),
		},
		{
			name:    "rejected_reject",
			driveTo: StatusRejected,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.ReviewIssue(planID, ActionReject, "yang_jb", "reject after reject")
				return err
			},
			errTarget:  &TerminalStateError{},
			actionName: string(ActionReject),
		},
		{
			name:    "rejected_request",
			driveTo: StatusRejected,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.ReviewIssue(planID, ActionRequest, "yang_jb", "request after reject")
				return err
			},
			errTarget:  &TerminalStateError{},
			actionName: string(ActionRequest),
		},
		{
			name:    "rejected_apply",
			driveTo: StatusRejected,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.Apply(planID, "yang_jb", "apply after reject")
				return err
			},
			errTarget:  &ErrRejectedApply{},
			actionName: string(ActionApply),
		},
		{
			name:    "rolled_back_approve",
			driveTo: StatusRolledBack,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.ReviewIssue(planID, ActionApprove, "yang_jb", "approve after rolled back")
				return err
			},
			errTarget:  &ErrRolledBackAny{},
			actionName: string(ActionApprove),
		},
		{
			name:    "rolled_back_reject",
			driveTo: StatusRolledBack,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.ReviewIssue(planID, ActionReject, "yang_jb", "reject after rolled back")
				return err
			},
			errTarget:  &ErrRolledBackAny{},
			actionName: string(ActionReject),
		},
		{
			name:    "rolled_back_request",
			driveTo: StatusRolledBack,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.ReviewIssue(planID, ActionRequest, "yang_jb", "request after rolled back")
				return err
			},
			errTarget:  &ErrRolledBackAny{},
			actionName: string(ActionRequest),
		},
		{
			name:    "rolled_back_apply",
			driveTo: StatusRolledBack,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.Apply(planID, "yang_jb", "apply after rolled back")
				return err
			},
			errTarget:  &ErrRolledBackAny{},
			actionName: string(ActionApply),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newMockTxStore()
			eng := NewEngine()
			eng.SetTxStore(st)

			plan, baseline := drivePlanTo(t, eng, st, tc.driveTo)

			// --- Pre-state snapshots: in-memory + mock store + audit baseline.
			preMem, err := eng.GetPlan(plan.PlanID)
			if err != nil {
				t.Fatalf("[%s] pre-state GetPlan: %v", tc.name, err)
			}
			if preMem.Status != tc.driveTo {
				t.Fatalf("[%s] pre-state in-memory status mismatch: want %s, got %s",
					tc.name, tc.driveTo, preMem.Status)
			}
			prePlans, _, preEvents, _, _ := st.snapshot()
			preMock, ok := prePlans[plan.PlanID]
			if !ok {
				t.Fatalf("[%s] pre-state: plan %q missing from mock store", tc.name, plan.PlanID)
			}
			if preMock.Status != tc.driveTo {
				t.Fatalf("[%s] pre-state mock-store status mismatch: want %s, got %s",
					tc.name, tc.driveTo, preMock.Status)
			}
			if len(preEvents) != baseline {
				t.Fatalf("[%s] pre-state audit baseline mismatch: want %d events, got %d",
					tc.name, baseline, len(preEvents))
			}

			// --- Invoke the refused action.
			err = tc.invoke(eng, plan.PlanID)
			if err == nil {
				t.Fatalf("[%s] expected error from refused terminal-state action, got nil",
					tc.name)
			}

			// --- 1. typed-error assertion (errors.As picks up concrete type).
			var target any = tc.errTarget
			if !errors.As(err, &target) {
				t.Fatalf("[%s] want error type %T, got %T (%v)",
					tc.name, tc.errTarget, err, err)
			}

			// --- 4. error message embeds status + action names.
			msg := err.Error()
			if !strings.Contains(msg, string(tc.driveTo)) {
				t.Fatalf("[%s] error message missing status %q: %s",
					tc.name, tc.driveTo, msg)
			}
			if !strings.Contains(msg, tc.actionName) {
				t.Fatalf("[%s] error message missing action %q: %s",
					tc.name, tc.actionName, msg)
			}

			// --- 2. Plan status UNCHANGED in BOTH in-memory and mock store.
			postMem, err := eng.GetPlan(plan.PlanID)
			if err != nil {
				t.Fatalf("[%s] post-state GetPlan: %v", tc.name, err)
			}
			if postMem.Status != tc.driveTo {
				t.Fatalf("[%s] in-memory Plan status mutated: was %s, now %s",
					tc.name, tc.driveTo, postMem.Status)
			}
			postPlans, _, postEvents, _, _ := st.snapshot()
			postMock := postPlans[plan.PlanID]
			if postMock.Status != tc.driveTo {
				t.Fatalf("[%s] mock-store Plan status mutated: was %s, now %s",
					tc.name, tc.driveTo, postMock.Status)
			}

			// --- 3. AuditEvent count UNCHANGED.
			if len(postEvents) != baseline {
				t.Fatalf("[%s] audit event count grew: was %d, now %d",
					tc.name, baseline, len(postEvents))
			}
		})
	}
}

// TestTerminal_PerCallPath verifies the per-call (no tStore) engine path
// also rejects terminal-state actions with the documented typed errors.
// This is the code path cmd/demo + early Task 2 tests exercise; it must
// keep matching the tStore path's typed-error contract.
func TestTerminal_PerCallPath(t *testing.T) {
	cases := []terminalCase{
		{
			name:    "per_call_rejected_approve",
			driveTo: StatusRejected,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.ReviewIssue(planID, ActionApprove, "yang_jb", "per-call approve after reject")
				return err
			},
			errTarget:  &ErrRejectedReApprove{},
			actionName: string(ActionApprove),
		},
		{
			name:    "per_call_rejected_reject",
			driveTo: StatusRejected,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.ReviewIssue(planID, ActionReject, "yang_jb", "per-call reject after reject")
				return err
			},
			errTarget:  &TerminalStateError{},
			actionName: string(ActionReject),
		},
		{
			name:    "per_call_rejected_request",
			driveTo: StatusRejected,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.ReviewIssue(planID, ActionRequest, "yang_jb", "per-call request after reject")
				return err
			},
			errTarget:  &TerminalStateError{},
			actionName: string(ActionRequest),
		},
		{
			name:    "per_call_rejected_apply",
			driveTo: StatusRejected,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.Apply(planID, "yang_jb", "per-call apply after reject")
				return err
			},
			errTarget:  &ErrRejectedApply{},
			actionName: string(ActionApply),
		},
		{
			name:    "per_call_rolled_back_approve",
			driveTo: StatusRolledBack,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.ReviewIssue(planID, ActionApprove, "yang_jb", "per-call approve after rolled back")
				return err
			},
			errTarget:  &ErrRolledBackAny{},
			actionName: string(ActionApprove),
		},
		{
			name:    "per_call_rolled_back_reject",
			driveTo: StatusRolledBack,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.ReviewIssue(planID, ActionReject, "yang_jb", "per-call reject after rolled back")
				return err
			},
			errTarget:  &ErrRolledBackAny{},
			actionName: string(ActionReject),
		},
		{
			name:    "per_call_rolled_back_request",
			driveTo: StatusRolledBack,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.ReviewIssue(planID, ActionRequest, "yang_jb", "per-call request after rolled back")
				return err
			},
			errTarget:  &ErrRolledBackAny{},
			actionName: string(ActionRequest),
		},
		{
			name:    "per_call_rolled_back_apply",
			driveTo: StatusRolledBack,
			invoke: func(eng *Engine, planID string) error {
				_, err := eng.Apply(planID, "yang_jb", "per-call apply after rolled back")
				return err
			},
			errTarget:  &ErrRolledBackAny{},
			actionName: string(ActionApply),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewEngine() // no tStore, no AuditWriter — pure in-memory path

			plan, err := eng.CreatePlanWithDraftReview(specFixture(), "yang_jb")
			if err != nil {
				t.Fatalf("[%s] create: %v", tc.name, err)
			}
			if _, err := eng.SubmitDraftReview(plan.PlanID); err != nil {
				t.Fatalf("[%s] submit: %v", tc.name, err)
			}
			if tc.driveTo == StatusRejected {
				if _, err := eng.ReviewIssue(plan.PlanID, ActionReject, "li_yw", "setup reject"); err != nil {
					t.Fatalf("[%s] reject setup: %v", tc.name, err)
				}
			} else { // StatusRolledBack
				if _, err := eng.ReviewIssue(plan.PlanID, ActionApprove, "yang_jb", "setup approve"); err != nil {
					t.Fatalf("[%s] approve setup: %v", tc.name, err)
				}
				if _, err := eng.Apply(plan.PlanID, "wang_xx", "setup apply"); err != nil {
					t.Fatalf("[%s] apply setup: %v", tc.name, err)
				}
				if _, err := eng.Rollback(plan.PlanID, "wang_xx", "setup rollback"); err != nil {
					t.Fatalf("[%s] rollback setup: %v", tc.name, err)
				}
			}

			preMem, err := eng.GetPlan(plan.PlanID)
			if err != nil {
				t.Fatalf("[%s] pre-state GetPlan: %v", tc.name, err)
			}
			if preMem.Status != tc.driveTo {
				t.Fatalf("[%s] pre-state status mismatch: want %s, got %s",
					tc.name, tc.driveTo, preMem.Status)
			}

			err = tc.invoke(eng, plan.PlanID)
			if err == nil {
				t.Fatalf("[%s] expected error from refused terminal-state action, got nil",
					tc.name)
			}

			var target any = tc.errTarget
			if !errors.As(err, &target) {
				t.Fatalf("[%s] want error type %T, got %T (%v)",
					tc.name, tc.errTarget, err, err)
			}

			msg := err.Error()
			if !strings.Contains(msg, string(tc.driveTo)) {
				t.Fatalf("[%s] error message missing status %q: %s",
					tc.name, tc.driveTo, msg)
			}
			if !strings.Contains(msg, tc.actionName) {
				t.Fatalf("[%s] error message missing action %q: %s",
					tc.name, tc.actionName, msg)
			}

			postMem, err := eng.GetPlan(plan.PlanID)
			if err != nil {
				t.Fatalf("[%s] post-state GetPlan: %v", tc.name, err)
			}
			if postMem.Status != tc.driveTo {
				t.Fatalf("[%s] in-memory Plan status mutated: was %s, now %s",
					tc.name, tc.driveTo, postMem.Status)
			}
		})
	}
}