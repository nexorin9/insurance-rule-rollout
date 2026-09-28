// Package his_dict is the HIS-side adapter that applies the monthly DRG/DIP
// rule bundle to the hospital's dictionary table. The full lifecycle is:
//
//   1. workflow.Engine.ReviewIssue → Approved
//   2. dictClient.ApplyUpdate(entries)         — single transaction, dual write
//   3. workflow.Engine.Apply         → Applied  — marks the Plan as committed
//
// Transaction boundary contract:
//
//   BEGIN
//     UPDATE his_dict SET ... WHERE code = ?
//     INSERT INTO his_audit_log (...)
//   COMMIT   ← all-or-nothing; any error → ROLLBACK
//
// Both writes live or die together so that the operator inspecting
// his_audit_log can never see a record with no matching dict row, or vice
// versa. The dual-write is the "atomic switch" the medical-insurance
// officer expects.
//
// Source: bytebase backend/component/review/workflow.go wraps the same
// pattern with `BeginTx → SELECT FOR UPDATE → UPDATE → COMMIT` — we drop
// the row lock because HIS dict is single-writer by operational policy
// (only the medical-insurance office runs ApplyUpdate during a release).
package his_dict

import "time"

// DictEntry is one row of his_dict. Fields are aligned with the spec.md
// "minimum data format" so ApplyUpdate can be fed straight from a
// platform.RuleSet without re-mapping.
type DictEntry struct {
	// Code is the DRG/DIP group code, e.g. "DRG-A001". PRIMARY KEY in the
	// HIS table — unique per row.
	Code string

	// Name is the human-readable name (Chinese). Operators read this in
	// audit reports.
	Name string

	// Weight is the DRG weight (resource consumption coefficient).
	// Non-negative float.
	Weight float64

	// Limit is the payment ceiling (元). Non-negative float.
	Limit float64

	// EffectiveDate is the ISO date "YYYY-MM-DD" on which this row starts
	// being authoritative for billing.
	EffectiveDate string

	// Version tags the row with the version it belongs to
	// (e.g. "r-2026-09-DRG-v17"). his_audit_log carries the same version
	// so the operator can correlate.
	Version string

	// UpdatedAt is the wall-clock time the row was last touched by
	// ApplyUpdate. The HIS adapter sets this on every successful write.
	UpdatedAt time.Time
}

// DictSchema is the read-side shape returned by ReadSchema — used by the
// dry-run engine to compute projected-diff without writing.
type DictSchema struct {
	Version       string      `json:"version"`
	EffectiveDate string      `json:"effective_date"`
	PrevVersion   string      `json:"prev_version,omitempty"`
	Entries       []DictEntry `json:"entries"`
}

// DictClient is the surface every HIS backend (MySQL / PG / SQLite test
// stand-in) must satisfy. cmd/demo and workflow.Engine call through this
// interface so the persistence switch stays open.
//
// ApplyUpdate   — write the supplied entries atomically (dual-write to
//                  his_dict + his_audit_log). Returns *TransactionError
//                  when the transaction could not complete (no partial
//                  state is left behind).
// ReadSchema    — snapshot the current dict into a DictSchema (used by
//                  dry-run for projected-diff; does NOT mutate state).
// DryRunScan    — verify a hypothetical set of entries would apply
//                  cleanly without writing (constraint check, type check,
//                  dry-run of the audit log row insert). Used by the
//                  workflow engine before Apply is allowed to commit.
// Close         — release the underlying *sql.DB handle.
type DictClient interface {
	ApplyUpdate(ctx interface{}, entries []DictEntry) error
	ReadSchema(ctx interface{}) (DictSchema, error)
	DryRunScan(ctx interface{}, entries []DictEntry) error
	Close() error
}

// TransactionError signals that the BEGIN/UPDATE/INSERT/COMMIT sequence
// was rolled back. The operator should look at Cause for the underlying
// driver error (constraint violation, connection drop, etc.) and at the
// dict row count to confirm no partial state.
type TransactionError struct {
	Op          string // "begin" | "update" | "insert_audit" | "commit"
	Affected    int    // rows written before the failure (always 0 on rollback)
	Code        string // offending dict code, if known
	Cause       error  // underlying driver error
}

// Error renders the failure with enough context for log triage.
func (e *TransactionError) Error() string {
	if e == nil {
		return "<nil>"
	}
	msg := "his_dict: transaction " + e.Op + " failed"
	if e.Code != "" {
		msg += " (code=" + e.Code + ")"
	}
	if e.Affected > 0 {
		msg += " after " + itoa(e.Affected) + " row(s) — rolled back"
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap exposes the underlying error for errors.Is / errors.As.
func (e *TransactionError) Unwrap() error { return e.Cause }

// ValidationError is returned by DryRunScan when entries would not survive
// ApplyUpdate (negative limit, empty code, etc.). Distinct from
// TransactionError so the dry-run report can list them without false
// positives on rollback classification.
type ValidationError struct {
	Field  string
	Reason string
}

// Error renders the validation failure.
func (e *ValidationError) Error() string {
	return "his_dict: invalid entry — " + e.Field + ": " + e.Reason
}

// itoa avoids strconv.Itoa to keep this file dependency-free for the error
// types — production code paths never allocate this path on success.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}