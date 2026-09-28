// Package insurance_platform is the HTTP client the medical-insurance office
// uses to pull the monthly DRG/DIP rule bundle from the platform operator's
// API. It is the live-side counterpart of the static fixture in testdata/
// platform_response.json — that file is what cmd/demo serves when real
// credentials are absent.
//
// Protocol (interface spec — see spec.md):
//
//   - Method:   GET /v1/rules/{version_id}
//   - Headers:  Authorization: Bearer <INS_PLATFORM_API_TOKEN>
//   - Response: 200 application/json — RuleSet JSON (this file)
//   - Errors:   401/403 → Unauthorized (no retry)
//               404     → NotFound    (no retry)
//               5xx     → Retryable   (exponential backoff, 3 attempts)
//               timeout → Retryable
//
// Source: bytebase backend/component/review/workflow.go classifies
// workflowError into ErrorNotFound / ErrorPermissionDenied / ErrorInternal —
// we follow the same shape but with HTTP transport semantics layered on top.
package insurance_platform

import "time"

// RuleSet is the JSON payload returned by GET /v1/rules/{version_id}.
// Field names are locked to spec.md's RulePlan Spec shape so the result can
// be passed straight into workflow.Engine.CreatePlanWithDraftReview.
type RuleSet struct {
	VersionID     string    `json:"version_id"`
	RuleSet       string    `json:"rule_set"`
	EffectiveDate string    `json:"effective_date"` // ISO date YYYY-MM-DD
	Rules         []Rule    `json:"rules"`
	PrevVersionID string    `json:"prev_version_id"`
	RollbackSQL   string    `json:"rollback_sql"`
	IssuedAt      time.Time `json:"issued_at,omitempty"`
}

// Rule is one DRG/DIP rule. Production rules from the platform do NOT carry
// an ICD10Prefix (the platform returns DRG → payment group, not DRG →
// diagnosis). The prefix is supplied by the dry-run fixture or a future
// mapping table — see internal/dryrun.Rule.ICD10Prefix docs.
//
// The wire-level optional `prev_limit` is kept here so the dry-run engine
// can compute the limit drop percentage (Task 21) without a second HTTP
// call. When absent, projected-diff calculation falls back to a 0 delta.
type Rule struct {
	Code      string  `json:"code"`
	Name      string  `json:"name"`
	Weight    float64 `json:"weight"`
	Limit     float64 `json:"limit"`
	PrevLimit float64 `json:"prev_limit,omitempty"`
}

// ValidationError is returned when the response body parses but the
// payload is semantically incomplete (empty version_id, empty rules, etc.).
// Distinct from Unauthorized / NotFound / Retryable so cmd/demo can branch
// on it (e.g. operator should re-fetch the bundle from a different channel
// when a 200 comes back empty).
type ValidationError struct {
	Field   string
	Reason  string
	Payload []byte // first 200 bytes of body for log diagnostics
}

func (e *ValidationError) Error() string {
	return "insurance_platform: invalid response — " + e.Field + ": " + e.Reason
}