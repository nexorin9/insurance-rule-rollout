// State machine table for the five-state Plan review transition.
//
// Legal transitions (from, action, to):
//
//	Draft     + SubmitDraft  → Pending
//	Pending   + Approve      → Approved
//	Pending   + Reject       → Rejected  (terminal)
//	Pending   + Request      → Pending   (self-loop, first time only; second
//	                                    Request → ErrPendingSelfLoop)
//	Approved  + Apply        → Applied
//	Applied   + Rollback     → RolledBack (terminal)
//
// All other (from, action) pairs produce typed errors matching Task 13's
// 9-illegal-transition catalogue; the (RolledBack, *) cases collapse to
// ErrRolledBackAny since they share the same fate.
package workflow

// TransitionRule is one row of the legal-transitions table. The slice
// LegalTransitions below is exposed for documentation and test pinning — the
// runtime lookup table is the legalTransitions map (built from this slice at
// package init via initLegalTransitions). The two views must stay in sync;
// TestCanTransition_LegalTransitionsMatchesMap enforces this on every test run.
type TransitionRule struct {
	From   PlanStatus
	Action IssueAction
	To     PlanStatus
}

// LegalTransitions is the canonical 5-row legal-transition table. Self-loop
// (Pending, Request) is intentionally NOT included here because the engine
// layer (Plan.LastAction) is what gates the second Request — the table alone
// cannot express that context-sensitive rule.
var LegalTransitions = []TransitionRule{
	{From: StatusDraft, Action: ActionSubmitDraft, To: StatusPending},
	{From: StatusPending, Action: ActionApprove, To: StatusApproved},
	{From: StatusPending, Action: ActionReject, To: StatusRejected},
	{From: StatusApproved, Action: ActionApply, To: StatusApplied},
	{From: StatusApplied, Action: ActionRollback, To: StatusRolledBack},
}

// SelfLoopTransition is the (Pending, Request) self-loop. Documented
// separately from LegalTransitions because the engine layer (Plan.LastAction)
// must gate it: first Request is legal; second Request on the same plan emits
// ErrPendingSelfLoop.
var SelfLoopTransition = TransitionRule{
	From: StatusPending, Action: ActionRequest, To: StatusPending,
}

// legalTransitions is the source-of-truth runtime lookup map, built from
// LegalTransitions plus SelfLoopTransition at package init. The map form is
// what CanTransition consults; the slice form is the canonical doc view.
//
// Self-loop (Pending, Request) appears in the map once; the engine.go layer
// detects the second Request by inspecting Plan.LastAction and emits
// ErrPendingSelfLoop in that case (see detectPendingSelfLoop).
var legalTransitions = map[PlanStatus]map[IssueAction]PlanStatus{}

func init() {
	for _, r := range LegalTransitions {
		initPutTransition(r)
	}
	initPutTransition(SelfLoopTransition)
}

func initPutTransition(r TransitionRule) {
	row, ok := legalTransitions[r.From]
	if !ok {
		row = map[IssueAction]PlanStatus{}
		legalTransitions[r.From] = row
	}
	row[r.Action] = r.To
}

// CanTransition looks up a (status, action) pair in the legal table.
// Returns (to, true) on hit; (from, false) on miss. The caller is expected to
// consult typedErrorFor / detectPendingSelfLoop for the precise error on miss.
//
// This function does not consult Plan.LastAction — the self-loop guard lives
// in the engine layer because it requires contextual data beyond the table.
func CanTransition(from PlanStatus, action IssueAction) (PlanStatus, bool) {
	row, ok := legalTransitions[from]
	if !ok {
		return from, false
	}
	to, ok := row[action]
	if !ok {
		return from, false
	}
	return to, true
}

// NextStatus is the user-friendly wrapper around CanTransition that returns
// the typed error for any illegal (status, action) pair. It does NOT perform
// the Plan.LastAction self-loop guard — callers that have a Plan (i.e. all
// engine paths) should call NextStatusForPlan instead.
func NextStatus(from PlanStatus, action IssueAction) (PlanStatus, error) {
	to, ok := CanTransition(from, action)
	if !ok {
		return from, typedErrorFor(from, action)
	}
	return to, nil
}

// NextStatusForPlan is the engine-level transition: it first applies the
// table lookup, then runs detectPendingSelfLoop when the table reports the
// (Pending, Request) self-loop.
func NextStatusForPlan(p Plan, action IssueAction) (PlanStatus, error) {
	to, err := NextStatus(p.Status, action)
	if err != nil {
		return to, err
	}
	// Self-loop guard for the (Pending, Request) reuse.
	if p.Status == StatusPending && action == ActionRequest && p.LastAction == ActionRequest && !p.LastActionAt.IsZero() {
		return to, &ErrPendingSelfLoop{
			IllegalTransitionError: IllegalTransitionError{
				From:     p.Status,
				Action:   action,
				Expected: "Pending plans may Request only once before an Approve or Reject; previous Request was recorded at " + p.LastActionAt.Format("2006-01-02T15:04:05Z07:00") + ".",
			},
		}
	}
	return to, nil
}

// typedErrorFor maps every illegal (status, action) pair to the dedicated
// sub-error required by the Task 13 catalogue. The 9 named illegal transitions
// plus a few catch-all invalid-action pairs (e.g. Draft+Reject) are covered.
func typedErrorFor(from PlanStatus, action IssueAction) error {
	switch {
	case from == StatusDraft && action == ActionApprove:
		return &ErrDraftApprove{
			IllegalTransitionError: IllegalTransitionError{
				From:     from,
				Action:   action,
				Expected: "", // Resolution hint lives in ErrDraftApprove.Error(); leave Expected empty.
			},
		}
	// (Approved, Approve) — already-approved re-approve.
	case from == StatusApproved && action == ActionApprove:
		return &ErrPendingReApprove{
			IllegalTransitionError: IllegalTransitionError{
				From: from, Action: action,
				Expected: "Plan is already Approved; Apply to push to Applied, or roll back from Applied.",
			},
		}
	case from == StatusRejected && action == ActionApprove:
		return &ErrRejectedReApprove{
			IllegalTransitionError: IllegalTransitionError{
				From: from, Action: action,
				Expected: "Rejected is a terminal state; create a new Plan instead of re-opening this one.",
			},
		}
	case from == StatusApproved && action == ActionRequest:
		return &ErrApprovedRequest{
			IllegalTransitionError: IllegalTransitionError{
				From: from, Action: action,
				Expected: "Request applies only to Pending plans after the reviewer has Requested changes; call Apply to push an Approved plan forward.",
			},
		}
	case from == StatusApplied && action == ActionReject:
		return &ErrAppliedReject{
			IllegalTransitionError: IllegalTransitionError{
				From: from, Action: action,
				Expected: "Plan is already Applied; use Rollback instead of Reject.",
			},
		}
	case from == StatusRolledBack:
		return &ErrRolledBackAny{
			IllegalTransitionError: IllegalTransitionError{
				From: from, Action: action,
				Expected: "RolledBack is a terminal state; create a new Plan to switch rules again.",
			},
		}
	case from == StatusApplied && action == ActionApprove:
		return &ErrAppliedReApprove{
			IllegalTransitionError: IllegalTransitionError{
				From: from, Action: action,
				Expected: "Plan is already Applied; call Rollback to roll back instead of re-approving.",
			},
		}
	case from == StatusRejected && action == ActionApply:
		return &ErrRejectedApply{
			IllegalTransitionError: IllegalTransitionError{
				From: from, Action: action,
				Expected: "Rejected is a terminal state; create a new Plan to retry.",
			},
		}
	}
	// Catch-all for (status, action) pairs that don't have a named illegal-
	// transition slot — e.g. (Draft, Reject), (Approved, SubmitDraft),
	// (Pending, Apply). Reported as InvalidActionError so callers can use
	// errors.As to branch on a generic recoverable path.
	//
	// Terminal-state guard (Task 19): Rejected / RolledBack reject every
	// action. The named slots above already cover (Rejected, Approve),
	// (Rejected, Apply), and (RolledBack, *); here we catch the remaining
	// terminal-state pairs that fall through (e.g. (Rejected, Reject),
	// (Rejected, Request), (Rejected, SubmitDraft), (Rejected, Rollback))
	// and surface them as TerminalStateError so callers can errors.As the
	// terminal-state class without having to enumerate every per-action
	// named slot.
	if from.IsTerminal() {
		return &TerminalStateError{From: from, Action: action}
	}
	return &InvalidActionError{From: from, Action: action}
}
