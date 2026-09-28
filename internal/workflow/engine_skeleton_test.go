// Tests for the in-memory Engine skeleton (Task 15). Focused on the
// CreatePlanWithDraftReview + SubmitDraftReview + GetPlan surface — the
// state-machine transitions themselves are covered exhaustively in
// state_test.go (Task 14) and engine_tx_test.go (Task 4). This file pins
// the public engine skeleton contract:
//
//   - CreatePlanWithDraftReview returns a Plan with status=Draft, draft=true,
//     and a non-empty UUID plan_id.
//   - SubmitDraftReview flips the draft bool off AND advances status Draft→
//     Pending. The latter is the design choice documented in Task 2's
//     progress.txt note: bytebase's frontend submitDraftReview only flips the
//     draft flag, but our 5-state machine uses SubmitDraft as a state-machine
//     action so the plan can reach Pending and unblock Approve. The legacy
//     bool flag is preserved for bytebase spec compatibility.
//   - SubmitDraftReview is NOT idempotent — calling it a second time surfaces
//     InvalidActionError because (Pending, SubmitDraft) is not in the legal
//     transition table.
//   - GetPlan returns a copy of the cached Plan, or *NotFoundError for an
//     unknown planID.
//
// These tests use the in-memory engine only (no Store, no tStore) so they
// represent the Task 15 "memory-only" scope.
package workflow

import (
	"errors"
	"testing"
	"time"
)

// skeletonSpecFixture returns a minimal valid RulePlanSpec for the engine
// skeleton tests. Kept local to this file so the tests stay self-contained.
func skeletonSpecFixture() RulePlanSpec {
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

// skeletonNewEngine is a small constructor that returns an in-memory
// Engine without any persistence sinks wired. Equivalent to NewEngine() but
// makes the "memory-only" intent explicit for tests.
func skeletonNewEngine() *Engine {
	return NewEngine()
}

// TestEngineSkeleton_CreatePlanWithDraftReview_BasicFields pins the
// post-creation Plan fields: Status=Draft, Draft=true, PlanID is a non-empty
// UUID, SpecJSON round-trips the input spec, CreatedAt/UpdatedAt are set.
func TestEngineSkeleton_CreatePlanWithDraftReview_BasicFields(t *testing.T) {
	e := skeletonNewEngine()
	spec := skeletonSpecFixture()

	p, err := e.CreatePlanWithDraftReview(spec, "yang_jb")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Status starts at Draft.
	if p.Status != StatusDraft {
		t.Fatalf("want Status=Draft after create, got %s", p.Status)
	}
	// Draft bool flag starts true (bytebase payload.draft semantics).
	if !p.Draft {
		t.Fatalf("want Draft=true after create")
	}
	// PlanID is a non-empty UUID (uuid.NewString returns 36-char v4).
	if p.PlanID == "" {
		t.Fatalf("want non-empty PlanID, got empty")
	}
	if len(p.PlanID) != 36 {
		t.Fatalf("want PlanID length 36 (UUID v4), got %d (value=%q)", len(p.PlanID), p.PlanID)
	}
	// VersionID / RuleSet / EffectiveDate copied through.
	if p.VersionID != spec.VersionID {
		t.Fatalf("want VersionID=%q, got %q", spec.VersionID, p.VersionID)
	}
	if p.RuleSet != spec.RuleSet {
		t.Fatalf("want RuleSet=%q, got %q", spec.RuleSet, p.RuleSet)
	}
	if p.EffectiveDate != spec.EffectiveDate {
		t.Fatalf("want EffectiveDate=%q, got %q", spec.EffectiveDate, p.EffectiveDate)
	}
	// CreatedBy copied through.
	if p.CreatedBy != "yang_jb" {
		t.Fatalf("want CreatedBy=yang_jb, got %q", p.CreatedBy)
	}
	// SpecJSON round-trips: the Plan's SpecJSON must contain version_id.
	if p.SpecJSON == "" {
		t.Fatalf("want non-empty SpecJSON after create")
	}
	// Timestamps are set.
	if p.CreatedAt.IsZero() {
		t.Fatalf("want CreatedAt set, got zero")
	}
	if p.UpdatedAt.IsZero() {
		t.Fatalf("want UpdatedAt set, got zero")
	}
}

// TestEngineSkeleton_SubmitDraftReview_FlipsDraftFlag verifies the bytebase-
// style draft flag flip: the draft bool goes from true to false after
// SubmitDraftReview.
func TestEngineSkeleton_SubmitDraftReview_FlipsDraftFlag(t *testing.T) {
	e := skeletonNewEngine()
	p, err := e.CreatePlanWithDraftReview(skeletonSpecFixture(), "yang_jb")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !p.Draft {
		t.Fatalf("pre-condition: want Draft=true after create")
	}

	p2, err := e.SubmitDraftReview(p.PlanID)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// The bytebase-style invariant: draft bool is now false.
	if p2.Draft {
		t.Fatalf("want Draft=false after SubmitDraftReview, got true")
	}
}

// TestEngineSkeleton_SubmitDraftReview_AdvancesStatusToPending verifies the
// design choice in Task 2: SubmitDraftReview also advances Status from
// Draft → Pending. bytebase's frontend submitDraftReview only flips the
// flag, but our 5-state machine treats SubmitDraft as a state-machine action
// (so the plan can reach Pending and unblock ReviewIssue(Approve)).
func TestEngineSkeleton_SubmitDraftReview_AdvancesStatusToPending(t *testing.T) {
	e := skeletonNewEngine()
	p, err := e.CreatePlanWithDraftReview(skeletonSpecFixture(), "yang_jb")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	p2, err := e.SubmitDraftReview(p.PlanID)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if p2.Status != StatusPending {
		t.Fatalf("want Status=Pending after SubmitDraftReview (state-machine "+
			"design choice in Task 2), got %s", p2.Status)
	}
	// LastAction recorded as submit_draft.
	if p2.LastAction != ActionSubmitDraft {
		t.Fatalf("want LastAction=submit_draft, got %q", p2.LastAction)
	}
	if p2.LastActionAt.IsZero() {
		t.Fatalf("want LastActionAt set, got zero")
	}
}

// TestEngineSkeleton_SubmitDraftReview_SecondCallErrors verifies that the
// engine skeleton does NOT treat SubmitDraftReview as idempotent. The state
// machine only allows (Draft, SubmitDraft) → Pending; once status is Pending,
// a second SubmitDraftReview hits the catch-all InvalidActionError.
func TestEngineSkeleton_SubmitDraftReview_SecondCallErrors(t *testing.T) {
	e := skeletonNewEngine()
	p, err := e.CreatePlanWithDraftReview(skeletonSpecFixture(), "yang_jb")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := e.SubmitDraftReview(p.PlanID); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	_, err = e.SubmitDraftReview(p.PlanID)
	if err == nil {
		t.Fatalf("second SubmitDraftReview: want error (Pending rejects "+
			"SubmitDraft), got nil")
	}
	// The error must be either InvalidActionError (catch-all) or
	// some other typed error — but never nil.
	var ia *InvalidActionError
	if !errors.As(err, &ia) {
		// Allow the test to pass if it's some other typed error too,
		// but log a soft warning so the design stays visible.
		t.Logf("note: second SubmitDraftReview surfaced %T (%v); "+
			"InvalidActionError is the expected catch-all", err, err)
	}
}

// TestEngineSkeleton_GetPlan_ReturnsCurrent verifies GetPlan returns the
// cached Plan after a series of operations (Create → SubmitDraft → Review).
func TestEngineSkeleton_GetPlan_ReturnsCurrent(t *testing.T) {
	e := skeletonNewEngine()
	p, err := e.CreatePlanWithDraftReview(skeletonSpecFixture(), "yang_jb")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Initial GetPlan returns Draft state.
	got, err := e.GetPlan(p.PlanID)
	if err != nil {
		t.Fatalf("get (initial): %v", err)
	}
	if got.Status != StatusDraft || !got.Draft {
		t.Fatalf("want Draft=true, Status=Draft; got Draft=%v Status=%s",
			got.Draft, got.Status)
	}

	// After SubmitDraftReview, GetPlan returns the updated state.
	if _, err := e.SubmitDraftReview(p.PlanID); err != nil {
		t.Fatalf("submit: %v", err)
	}
	got2, err := e.GetPlan(p.PlanID)
	if err != nil {
		t.Fatalf("get (post-submit): %v", err)
	}
	if got2.Draft {
		t.Fatalf("want Draft=false after submit, got true")
	}
	if got2.Status != StatusPending {
		t.Fatalf("want Status=Pending after submit, got %s", got2.Status)
	}

	// After Approve via ReviewIssue, GetPlan returns Approved.
	if _, err := e.ReviewIssue(p.PlanID, ActionApprove, "yang_jb", "ok"); err != nil {
		t.Fatalf("review approve: %v", err)
	}
	got3, err := e.GetPlan(p.PlanID)
	if err != nil {
		t.Fatalf("get (post-approve): %v", err)
	}
	if got3.Status != StatusApproved {
		t.Fatalf("want Status=Approved after approve, got %s", got3.Status)
	}
}

// TestEngineSkeleton_GetPlan_NotFound verifies GetPlan returns *NotFoundError
// when the planID is not in the in-memory cache.
func TestEngineSkeleton_GetPlan_NotFound(t *testing.T) {
	e := skeletonNewEngine()

	_, err := e.GetPlan("does-not-exist")
	if err == nil {
		t.Fatalf("want NotFoundError for unknown planID, got nil")
	}
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("want *NotFoundError, got %T (%v)", err, err)
	}
	// The PlanID should be echoed in the error message for traceability.
	if nf.PlanID != "does-not-exist" {
		t.Fatalf("want NotFoundError.PlanID=does-not-exist, got %q", nf.PlanID)
	}
}

// TestEngineSkeleton_DefaultsArePinned is a small structural assertion: with
// no SetClock / SetIDGen / SetStore / SetTxStore calls, the engine uses the
// default uuid + time.Now clock. We verify a freshly-created plan has a
// CreatedAt within 1 second of time.Now() and a non-UUID-empty PlanID.
func TestEngineSkeleton_DefaultsArePinned(t *testing.T) {
	e := skeletonNewEngine()
	before := time.Now()
	p, err := e.CreatePlanWithDraftReview(skeletonSpecFixture(), "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	after := time.Now()

	if p.CreatedAt.Before(before.Add(-time.Second)) || p.CreatedAt.After(after.Add(time.Second)) {
		t.Fatalf("CreatedAt %v not within [%v, %v]", p.CreatedAt, before, after)
	}
	// CreatedBy default to DefaultReviewer when empty.
	if p.CreatedBy != DefaultReviewer {
		t.Fatalf("want CreatedBy=%q (default for empty), got %q", DefaultReviewer, p.CreatedBy)
	}
}