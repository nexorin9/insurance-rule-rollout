// Package workflow implements the five-state Plan / Issue review machine
// that the medical-insurance office uses to govern monthly / quarterly DRG/DIP
// rule version switches. It is the Go adaptation of bytebase's
// backend/component/review/workflow.go state machine, slimmed to a 5-state
// transition table for the insurance domain and stripped of workspace/iam
// semantics that are out of scope here.
//
// Five-state machine:
//
//	DRAFT  --SubmitDraft-->  PENDING  --Approve-->  APPROVED  --Apply-->  APPLIED  --Rollback-->  ROLLED_BACK
//	DRAFT  --SubmitDraft-->  PENDING  --Reject--->  REJECTED (terminal)
//
// Plan.Draft is a secondary bool flag that flips false on SubmitDraftReview
// (matches bytebase frontend/src/lib/plan/workflow.ts submitDraftReview).
package workflow

import "time"

// PlanStatus is the five-state label for a RulePlan.
type PlanStatus string

const (
	StatusDraft      PlanStatus = "draft"
	StatusPending    PlanStatus = "pending"
	StatusApproved   PlanStatus = "approved"
	StatusApplied    PlanStatus = "applied"
	StatusRolledBack PlanStatus = "rolled_back"
	StatusRejected   PlanStatus = "rejected"
)

// IsTerminal reports whether a PlanStatus rejects further ReviewIssue calls.
// Rejected and RolledBack are terminal; Applied is also terminal in practice
// (only Rollback is allowed from Applied), but the Plan must remain in Applied
// while Rollback is in flight, so it is NOT considered terminal here.
func (s PlanStatus) IsTerminal() bool {
	return s == StatusRejected || s == StatusRolledBack
}

// IssueAction is the interactive action a reviewer can take on a Plan.
type IssueAction string

const (
	ActionSubmitDraft IssueAction = "submit_draft"
	ActionApprove     IssueAction = "approve"
	ActionReject      IssueAction = "reject"
	ActionRequest     IssueAction = "request"
	ActionApply       IssueAction = "apply"
	ActionRollback    IssueAction = "rollback"
)

// RulePlanSpec is the input payload declaring a new Rule version.
type RulePlanSpec struct {
	VersionID     string         `json:"version_id"`
	RuleSet       string         `json:"rule_set"`
	EffectiveDate string         `json:"effective_date"`
	Rules         []RuleEntry    `json:"rules"`
	PrevVersionID string         `json:"prev_version_id"`
	RollbackSQL   string         `json:"rollback_sql"`
}

// RuleEntry is one row of the DRG/DIP rule table inside a Spec.
type RuleEntry struct {
	Code      string  `json:"code"`
	Name      string  `json:"name"`
	Weight    float64 `json:"weight"`
	Limit     float64 `json:"limit"`
	PrevLimit float64 `json:"prev_limit,omitempty"`
}

// Plan is one Rule version's review-bound lifecycle object.
type Plan struct {
	PlanID        string      `json:"plan_id"`
	VersionID     string      `json:"version_id"`
	RuleSet       string      `json:"rule_set"`
	EffectiveDate string      `json:"effective_date"`
	SpecJSON      string      `json:"spec_json"`
	PrevVersionID string      `json:"prev_version_id"`
	RollbackSQL   string      `json:"rollback_sql"`
	Status        PlanStatus  `json:"status"`
	Draft         bool        `json:"draft"`
	LastAction    IssueAction `json:"last_action,omitempty"`
	LastActionAt  time.Time   `json:"last_action_at,omitempty"`
	CreatedBy     string      `json:"created_by"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// IsDraft returns whether the Plan is still in draft review (draft flag, not
// state). Mirrors bytebase issue.Payload.GetDraft semantics.
func (p Plan) IsDraft() bool { return p.Draft }

// Issue is one reviewer's record of acting on a Plan.
type Issue struct {
	IssueID   string      `json:"issue_id"`
	PlanID    string      `json:"plan_id"`
	Action    IssueAction `json:"action"`
	Actor     string      `json:"actor"`
	Comment   string      `json:"comment"`
	CreatedAt time.Time   `json:"created_at"`
}

// AuditEvent is one append-only audit row. SHA256 chain linking is layered on
// top of this struct by internal/audit.Chain.
type AuditEvent struct {
	EventID   string            `json:"event_id"`
	PlanID    string            `json:"plan_id"`
	Actor     string            `json:"actor"`
	Action    IssueAction       `json:"action"`
	Payload   map[string]string `json:"payload"`
	Timestamp time.Time         `json:"timestamp"`
	PrevHash  string            `json:"prev_hash"`
	EventHash string            `json:"event_hash"`
}
