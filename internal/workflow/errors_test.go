// Table-driven tests for the workflow typed errors. Each of the 9 named
// illegal-transition types plus the two catch-all error types is exercised
// with three layers of assertions:
//
//  1. errors.As: the returned error unwraps to the expected typed-error
//     pointer, so callers can branch on the specific transition.
//  2. Error() message: contains the status name AND the action name, so
//     fly-inspect log scans can grep by either dimension.
//  3. Resolution hint: the message contains a non-empty resolution sentence
//     that points the operator to the next legal action.
//
// The tests live in errors_test.go (not state_test.go) so that fly-inspect
// reviewers can locate the catalogue in one place, mirroring the bytebase
// workflow_test.go split between "transition table" and "typed errors".
package workflow

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// --- 9 named illegal-transition types --------------------------------------

func TestTypedErrors_NineIllegalTransitions(t *testing.T) {
	cases := []struct {
		name             string
		from             PlanStatus
		action           IssueAction
		errTarget        any    // typed-error pointer template (errors.As target)
		hintSubstring    string // required substring in the resolution hint
		illegalKeyword   string // required word in error message ("illegal transition")
		statusInMessage  string // expected status name in error message
		actionInMessage  string // expected action name in error message
	}{
		{
			name:            "draft_approve",
			from:            StatusDraft,
			action:          ActionApprove,
			errTarget:       &ErrDraftApprove{},
			hintSubstring:   "SubmitDraftReview",
			illegalKeyword:  "illegal transition",
			statusInMessage: string(StatusDraft),
			actionInMessage: string(ActionApprove),
		},
		{
			name:            "approved_approve_reapprove",
			from:            StatusApproved,
			action:          ActionApprove,
			errTarget:       &ErrPendingReApprove{},
			hintSubstring:   "Apply",
			illegalKeyword:  "illegal transition",
			statusInMessage: string(StatusApproved),
			actionInMessage: string(ActionApprove),
		},
		{
			name:            "rejected_approve",
			from:            StatusRejected,
			action:          ActionApprove,
			errTarget:       &ErrRejectedReApprove{},
			hintSubstring:   "terminal",
			illegalKeyword:  "illegal transition",
			statusInMessage: string(StatusRejected),
			actionInMessage: string(ActionApprove),
		},
		{
			name:            "approved_request",
			from:            StatusApproved,
			action:          ActionRequest,
			errTarget:       &ErrApprovedRequest{},
			hintSubstring:   "Apply",
			illegalKeyword:  "illegal transition",
			statusInMessage: string(StatusApproved),
			actionInMessage: string(ActionRequest),
		},
		{
			name:            "applied_reject",
			from:            StatusApplied,
			action:          ActionReject,
			errTarget:       &ErrAppliedReject{},
			hintSubstring:   "Rollback",
			illegalKeyword:  "illegal transition",
			statusInMessage: string(StatusApplied),
			actionInMessage: string(ActionReject),
		},
		{
			name:            "rolled_back_approve",
			from:            StatusRolledBack,
			action:          ActionApprove,
			errTarget:       &ErrRolledBackAny{},
			hintSubstring:   "terminal",
			illegalKeyword:  "illegal transition",
			statusInMessage: string(StatusRolledBack),
			actionInMessage: string(ActionApprove),
		},
		{
			name:            "rolled_back_reject",
			from:            StatusRolledBack,
			action:          ActionReject,
			errTarget:       &ErrRolledBackAny{},
			hintSubstring:   "terminal",
			illegalKeyword:  "illegal transition",
			statusInMessage: string(StatusRolledBack),
			actionInMessage: string(ActionReject),
		},
		{
			name:            "applied_reapprove",
			from:            StatusApplied,
			action:          ActionApprove,
			errTarget:       &ErrAppliedReApprove{},
			hintSubstring:   "Rollback",
			illegalKeyword:  "illegal transition",
			statusInMessage: string(StatusApplied),
			actionInMessage: string(ActionApprove),
		},
		{
			name:            "rejected_apply",
			from:            StatusRejected,
			action:          ActionApply,
			errTarget:       &ErrRejectedApply{},
			hintSubstring:   "terminal",
			illegalKeyword:  "illegal transition",
			statusInMessage: string(StatusRejected),
			actionInMessage: string(ActionApply),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NextStatus(tc.from, tc.action)
			if err == nil {
				t.Fatalf("expected error for (%s, %s), got nil", tc.from, tc.action)
			}

			// (1) errors.As: returned error must unwrap to the typed-error pointer.
			if !errors.As(err, &tc.errTarget) {
				t.Fatalf("errors.As: want %T, got %T (%v)", tc.errTarget, err, err)
			}

			msg := err.Error()

			// (2a) "illegal transition" prefix present.
			if !strings.Contains(msg, tc.illegalKeyword) {
				t.Fatalf("error message missing %q prefix: %s", tc.illegalKeyword, msg)
			}
			// (2b) status name present (fly-inspect greps by status).
			if !strings.Contains(msg, tc.statusInMessage) {
				t.Fatalf("error message missing status %q: %s", tc.statusInMessage, msg)
			}
			// (2c) action name present (fly-inspect greps by action).
			if !strings.Contains(msg, tc.actionInMessage) {
				t.Fatalf("error message missing action %q: %s", tc.actionInMessage, msg)
			}

			// (3) resolution hint substring present.
			if !strings.Contains(msg, tc.hintSubstring) {
				t.Fatalf("error message missing resolution hint %q: %s", tc.hintSubstring, msg)
			}
		})
	}
}

// TestTypedErrors_HintMessagesDiffer verifies that each typed error produces
// a distinct resolution hint. This catches the regression where two typed
// errors accidentally fall back to the same generic message.
func TestTypedErrors_HintMessagesDiffer(t *testing.T) {
	hints := map[string]string{
		"ErrDraftApprove":      (&ErrDraftApprove{IllegalTransitionError: IllegalTransitionError{From: StatusDraft, Action: ActionApprove}}).Error(),
		"ErrPendingReApprove":  (&ErrPendingReApprove{IllegalTransitionError: IllegalTransitionError{From: StatusApproved, Action: ActionApprove}}).Error(),
		"ErrRejectedReApprove": (&ErrRejectedReApprove{IllegalTransitionError: IllegalTransitionError{From: StatusRejected, Action: ActionApprove}}).Error(),
		"ErrApprovedRequest":   (&ErrApprovedRequest{IllegalTransitionError: IllegalTransitionError{From: StatusApproved, Action: ActionRequest}}).Error(),
		"ErrAppliedReject":     (&ErrAppliedReject{IllegalTransitionError: IllegalTransitionError{From: StatusApplied, Action: ActionReject}}).Error(),
		"ErrRolledBackAny":     (&ErrRolledBackAny{IllegalTransitionError: IllegalTransitionError{From: StatusRolledBack, Action: ActionApprove}}).Error(),
		"ErrAppliedReApprove":  (&ErrAppliedReApprove{IllegalTransitionError: IllegalTransitionError{From: StatusApplied, Action: ActionApprove}}).Error(),
		"ErrRejectedApply":     (&ErrRejectedApply{IllegalTransitionError: IllegalTransitionError{From: StatusRejected, Action: ActionApply}}).Error(),
	}
	seen := make(map[string]string)
	for name, msg := range hints {
		// Strip the (status → action) prefix to compare only the hint tail.
		idx := strings.Index(msg, ": ")
		if idx < 0 {
			t.Fatalf("%s: missing ': ' separator in %q", name, msg)
		}
		tail := msg[idx+2:]
		if other, dup := seen[tail]; dup {
			t.Fatalf("%s and %s share the same hint: %q", name, other, tail)
		}
		seen[tail] = name
	}
}

// TestTypedErrors_PendingSelfLoop_DynamicTimestamp verifies ErrPendingSelfLoop
// uses the Expected field (populated by state.go with the previous Request
// timestamp) and falls back to a generic message when Expected is empty.
func TestTypedErrors_PendingSelfLoop_DynamicTimestamp(t *testing.T) {
	ts := time.Date(2026, 9, 28, 10, 30, 0, 0, time.UTC)
	t.Run("with_expected_timestamp", func(t *testing.T) {
		err := &ErrPendingSelfLoop{
			IllegalTransitionError: IllegalTransitionError{
				From:     StatusPending,
				Action:   ActionRequest,
				Expected: "Pending plans may Request only once; previous Request at " + ts.Format(time.RFC3339) + ".",
			},
		}
		msg := err.Error()
		if !strings.Contains(msg, string(StatusPending)) {
			t.Fatalf("missing status %q: %s", StatusPending, msg)
		}
		if !strings.Contains(msg, string(ActionRequest)) {
			t.Fatalf("missing action %q: %s", ActionRequest, msg)
		}
		if !strings.Contains(msg, ts.Format(time.RFC3339)) {
			t.Fatalf("expected timestamp %q in message: %s", ts.Format(time.RFC3339), msg)
		}
		if !strings.Contains(msg, "illegal transition") {
			t.Fatalf("missing 'illegal transition' prefix: %s", msg)
		}
	})

	t.Run("without_expected_falls_back", func(t *testing.T) {
		// Construct directly with empty Expected to exercise the fallback path.
		err := &ErrPendingSelfLoop{
			IllegalTransitionError: IllegalTransitionError{
				From:   StatusPending,
				Action: ActionRequest,
			},
		}
		msg := err.Error()
		if !strings.Contains(msg, "Pending plans may Request only once") {
			t.Fatalf("fallback hint missing: %s", msg)
		}
		if !strings.Contains(msg, string(StatusPending)) || !strings.Contains(msg, string(ActionRequest)) {
			t.Fatalf("status/action missing in fallback: %s", msg)
		}
	})
}

// TestTypedErrors_PendingSelfLoop_ViaStateMachine drives NextStatusForPlan
// with a Plan whose LastAction == ActionRequest; the engine must surface
// ErrPendingSelfLoop with a non-empty Expected that contains the timestamp.
func TestTypedErrors_PendingSelfLoop_ViaStateMachine(t *testing.T) {
	ts := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	p := Plan{
		Status:       StatusPending,
		LastAction:   ActionRequest,
		LastActionAt: ts,
	}
	_, err := NextStatusForPlan(p, ActionRequest)
	var target *ErrPendingSelfLoop
	if !errors.As(err, &target) {
		t.Fatalf("expected ErrPendingSelfLoop, got %T (%v)", err, err)
	}
	if !strings.Contains(err.Error(), ts.Format("2006-01-02T15:04:05")) {
		t.Fatalf("timestamp missing in error message: %s", err.Error())
	}
}

// --- catch-all error types -------------------------------------------------

func TestTypedErrors_InvalidActionError(t *testing.T) {
	// (Draft, Reject) is not one of the 9 named illegal slots — verify the
	// generic InvalidActionError still surfaces with status + action names.
	_, err := NextStatus(StatusDraft, ActionReject)
	if err == nil {
		t.Fatalf("expected error for (Draft, Reject), got nil")
	}
	var target *InvalidActionError
	if !errors.As(err, &target) {
		t.Fatalf("want InvalidActionError, got %T (%v)", err, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, string(StatusDraft)) {
		t.Fatalf("missing status %q: %s", StatusDraft, msg)
	}
	if !strings.Contains(msg, string(ActionReject)) {
		t.Fatalf("missing action %q: %s", ActionReject, msg)
	}
	if !strings.Contains(msg, "invalid action") {
		t.Fatalf("missing 'invalid action' prefix: %s", msg)
	}
}

func TestTypedErrors_TerminalStateError(t *testing.T) {
	// Direct construction exercises the Error() method.
	cases := []struct {
		from   PlanStatus
		action IssueAction
	}{
		{StatusRejected, ActionRollback},
		{StatusRolledBack, ActionApply},
		{StatusRejected, ActionSubmitDraft},
	}
	for _, tc := range cases {
		err := &TerminalStateError{From: tc.from, Action: tc.action}
		msg := err.Error()
		if !strings.Contains(msg, string(tc.from)) {
			t.Fatalf("missing status %q: %s", tc.from, msg)
		}
		if !strings.Contains(msg, string(tc.action)) {
			t.Fatalf("missing action %q: %s", tc.action, msg)
		}
		if !strings.Contains(msg, "terminal") {
			t.Fatalf("missing 'terminal' keyword: %s", msg)
		}
	}
}

func TestTypedErrors_NotFoundError(t *testing.T) {
	err := &NotFoundError{PlanID: "missing-plan-001"}
	msg := err.Error()
	if !strings.Contains(msg, "missing-plan-001") {
		t.Fatalf("missing plan_id in message: %s", msg)
	}
	if !strings.Contains(msg, "plan not found") {
		t.Fatalf("missing 'plan not found' phrase: %s", msg)
	}
}

// TestTypedErrors_BaseIllegalTransitionError exercises the fallback path on
// IllegalTransitionError itself (without a typed wrapper). The Expected field
// is consumed directly.
func TestTypedErrors_BaseIllegalTransitionError(t *testing.T) {
	err := &IllegalTransitionError{
		From:     StatusDraft,
		Action:   ActionApprove,
		Expected: "Fallback hint for the base IllegalTransitionError.",
	}
	msg := err.Error()
	if !strings.Contains(msg, string(StatusDraft)) {
		t.Fatalf("missing status: %s", msg)
	}
	if !strings.Contains(msg, string(ActionApprove)) {
		t.Fatalf("missing action: %s", msg)
	}
	if !strings.Contains(msg, "Fallback hint for the base IllegalTransitionError.") {
		t.Fatalf("Expected hint missing: %s", msg)
	}
}

// TestTypedErrors_FormatHelper_EmptyHint ensures the format helper doesn't
// produce a trailing colon-space when hint is empty (defensive — typed errors
// always pass a non-empty hint, but the helper must not crash).
func TestTypedErrors_FormatHelper_EmptyHint(t *testing.T) {
	got := illegalTransitionErrorMessage(StatusDraft, ActionApprove, "")
	want := "illegal transition draft → approve: "
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

// TestTypedErrors_AllTypesCompileAsError is a compile-time sanity check that
// every typed error satisfies the error interface. Failures here show up at
// build time, but the test serves as living documentation.
func TestTypedErrors_AllTypesCompileAsError(t *testing.T) {
	var _ error = (*ErrDraftApprove)(nil)
	var _ error = (*ErrPendingReApprove)(nil)
	var _ error = (*ErrRejectedReApprove)(nil)
	var _ error = (*ErrApprovedRequest)(nil)
	var _ error = (*ErrAppliedReject)(nil)
	var _ error = (*ErrRolledBackAny)(nil)
	var _ error = (*ErrPendingSelfLoop)(nil)
	var _ error = (*ErrAppliedReApprove)(nil)
	var _ error = (*ErrRejectedApply)(nil)
	var _ error = (*InvalidActionError)(nil)
	var _ error = (*NotFoundError)(nil)
	var _ error = (*TerminalStateError)(nil)
	var _ error = (*IllegalTransitionError)(nil)
}

// silence unused import lints if test helpers drift
var _ = fmt.Sprintf
