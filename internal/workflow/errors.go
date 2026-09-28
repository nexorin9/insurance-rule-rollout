// Typed errors for the workflow state machine. Each illegal (status, action)
// pair maps to a dedicated error type so fly-inspect / log scans can grep by
// error name and so call sites can recover with errors.As.
//
// Each typed error below embeds IllegalTransitionError and overrides Error()
// to produce an actionable message containing:
//
//  1. The illegal (status, action) pair — e.g. "(Draft, approve)".
//  2. A resolution hint pointing the operator to the next legal action.
//
// The 9 named illegal-transition errors match the Task 13 catalogue:
//
//	ErrDraftApprove      — (Draft, approve)
//	ErrPendingReApprove  — (Approved, approve)
//	ErrRejectedReApprove — (Rejected, approve)
//	ErrApprovedRequest   — (Approved, request)
//	ErrAppliedReject     — (Applied, reject)
//	ErrRolledBackAny     — (RolledBack, any action)
//	ErrPendingSelfLoop   — (Pending, request) called twice
//	ErrAppliedReApprove  — (Applied, approve)
//	ErrRejectedApply     — (Rejected, apply)
//
// Two catch-all error types handle edge cases that don't fit the catalogue:
//
//	InvalidActionError   — generic (status, action) not in the legal table
//	TerminalStateError   — terminal-state action with no specific named slot
package workflow

import "fmt"

// IllegalTransitionError is the base for any (from, action) the state machine
// rejects. Concrete sub-types below override Error() to embed typed-resolution
// guidance. Direct use of IllegalTransitionError (without a typed wrapper)
// renders a generic message using the Expected hint provided by the caller.
type IllegalTransitionError struct {
	From     PlanStatus
	Action   IssueAction
	Expected string // human-friendly hint; consumed by base Error().
}

func (e *IllegalTransitionError) Error() string {
	return illegalTransitionErrorMessage(e.From, e.Action, e.Expected)
}

// illegalTransitionErrorMessage is the canonical format every typed sub-error
// uses. Format:
//
//	illegal transition <status> → <action>: <resolution hint>
//
// Fly-inspect greps by status and action name; the resolution hint tells the
// operator what to do next. Both status and action render in their canonical
// lower-case string forms ("Draft → approve") rather than the Go identifier
// forms ("StatusDraft → ActionApprove") for log readability.
func illegalTransitionErrorMessage(from PlanStatus, action IssueAction, hint string) string {
	return fmt.Sprintf("illegal transition %s → %s: %s", from, action, hint)
}

// --- 9 named illegal-transition types (Task 13 catalogue) -----------------

// ErrDraftApprove: (Draft, Approve) — must SubmitDraft first.
type ErrDraftApprove struct{ IllegalTransitionError }

func (e *ErrDraftApprove) Error() string {
	return illegalTransitionErrorMessage(e.From, e.Action,
		"Draft plans cannot be approved. "+
			"Call SubmitDraftReview first to move the plan to Pending, then Approve.")
}

// ErrPendingReApprove: (Approved, Approve) — already approved, can't approve again.
type ErrPendingReApprove struct{ IllegalTransitionError }

func (e *ErrPendingReApprove) Error() string {
	return illegalTransitionErrorMessage(e.From, e.Action,
		"Plan is already Approved. "+
			"Do not call Approve again. "+
			"Call Apply to push the plan to Applied, or first Rollback from Applied to revert.")
}

// ErrRejectedReApprove: (Rejected, Approve) — Rejected is terminal.
type ErrRejectedReApprove struct{ IllegalTransitionError }

func (e *ErrRejectedReApprove) Error() string {
	return illegalTransitionErrorMessage(e.From, e.Action,
		"Rejected is a terminal state. "+
			"Do not call Approve on a Rejected plan. "+
			"Create a new Plan instead of re-opening this one.")
}

// ErrApprovedRequest: (Approved, Request) — Request only re-opens a Pending plan.
type ErrApprovedRequest struct{ IllegalTransitionError }

func (e *ErrApprovedRequest) Error() string {
	return illegalTransitionErrorMessage(e.From, e.Action,
		"Request applies only to Pending plans after the reviewer has Requested changes. "+
			"Call Apply to push an Approved plan forward.")
}

// ErrAppliedReject: (Applied, Reject) — past the Apply gate.
type ErrAppliedReject struct{ IllegalTransitionError }

func (e *ErrAppliedReject) Error() string {
	return illegalTransitionErrorMessage(e.From, e.Action,
		"Plan is already Applied. "+
			"Do not call Reject on an Applied plan. "+
			"Use Rollback instead of Reject.")
}

// ErrRolledBackAny: (RolledBack, any action) — RolledBack is terminal.
type ErrRolledBackAny struct{ IllegalTransitionError }

func (e *ErrRolledBackAny) Error() string {
	return illegalTransitionErrorMessage(e.From, e.Action,
		"RolledBack is a terminal state. "+
			"No further actions are accepted. "+
			"Create a new Plan to switch rules again.")
}

// ErrPendingSelfLoop: (Pending, Request) called twice without an intervening
// Approve or Reject. Detected via Plan.LastAction == ActionRequest at the
// engine layer (see state.go's NextStatusForPlan).
//
// The Expected field carries the timestamp of the previous Request, populated
// by state.go. If Expected is empty (e.g. a caller constructs the error
// directly), fall back to a generic self-loop message.
type ErrPendingSelfLoop struct{ IllegalTransitionError }

func (e *ErrPendingSelfLoop) Error() string {
	hint := e.Expected
	if hint == "" {
		hint = "Pending plans may Request only once before an Approve or Reject. " +
			"Call Approve or Reject to advance the plan."
	}
	return illegalTransitionErrorMessage(e.From, e.Action, hint)
}

// ErrAppliedReApprove: (Applied, Approve) — already applied.
type ErrAppliedReApprove struct{ IllegalTransitionError }

func (e *ErrAppliedReApprove) Error() string {
	return illegalTransitionErrorMessage(e.From, e.Action,
		"Plan is already Applied. "+
			"Do not call Approve again. "+
			"Call Rollback to roll back instead of re-approving.")
}

// ErrRejectedApply: (Rejected, Apply) — Rejected is terminal.
type ErrRejectedApply struct{ IllegalTransitionError }

func (e *ErrRejectedApply) Error() string {
	return illegalTransitionErrorMessage(e.From, e.Action,
		"Rejected is a terminal state. "+
			"Do not call Apply on a Rejected plan. "+
			"Create a new Plan to retry the rule switch.")
}

// --- catch-all error types --------------------------------------------------

// InvalidActionError: action not legal for the current status in general
// (e.g. (Draft, Reject), (Approved, SubmitDraft)). Independent of the 9 named
// transitions above; used for catch-all unsupported (from, action) pairs.
type InvalidActionError struct {
	From   PlanStatus
	Action IssueAction
}

func (e *InvalidActionError) Error() string {
	return fmt.Sprintf("invalid action: status=%s does not accept action=%s. "+
		"Check the workflow state machine table for the legal next action.",
		e.From, e.Action)
}

// NotFoundError: planID not present in the engine.
type NotFoundError struct {
	PlanID string
}

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("plan not found: %s", e.PlanID)
}

// TerminalStateError: action called against a terminal status when the (from,
// action) pair is not one of the 9 dedicated illegal transitions above. Kept
// for forward-compatibility (e.g. (RolledBack, Apply) cases that don't fit
// the named slots).
type TerminalStateError struct {
	From   PlanStatus
	Action IssueAction
}

func (e *TerminalStateError) Error() string {
	return fmt.Sprintf("terminal state: status=%s rejects action=%s. "+
		"Terminal plans (rejected, rolled_back) do not accept further actions.",
		e.From, e.Action)
}
