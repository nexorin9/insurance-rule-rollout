// Package dryrun implements the dry-run pre-flight scan that the medical-
// insurance office runs BEFORE the five-state review machine actually switches
// the HIS dictionary table. The scan produces a DryRunResult: how many
// inpatients will be affected, how they distribute across departments, the
// projected settlement diff, and any warnings raised by the rule change
// detector.
//
// This package owns its own Rule and PatientRecord types so it does not
// import internal/workflow and create a cycle (workflow depends on store,
// store depends on workflow's row types but not on dryrun).
//
// Source mapping: bytebase has no equivalent dry-run scanner; this is a new
// module that lives on the Plan / Issue path introduced by bytebase, but the
// shape (aggregate by department, prefix-match ICD-10 → DRG, diff against the
// current dictionary table) is the medical-insurance office's own concern.
package dryrun

import "time"

// DryRunResult is the JSON payload printed in `go run ./cmd/demo` and
// consumed by the medical-insurance office's review meeting. Schema:
//
//	{
//	  "plan_id":               "r-2026-09-DRG-v17",
//	  "affected_inpatients":   87,
//	  "departments":           {"内科": 31, "外科": 24, ...},
//	  "projected_diff_amount": -12400.00,
//	  "warnings":              ["DRG-B014 limit 降幅 6.3%"],
//	  "dry_run_at":            "2026-09-28T07:42:00Z"
//	}
type DryRunResult struct {
	PlanID              string         `json:"plan_id"`
	AffectedInpatients  int            `json:"affected_inpatients"`
	Departments         map[string]int `json:"departments"`
	ProjectedDiffAmount float64        `json:"projected_diff_amount"`
	Warnings            []string       `json:"warnings"`
	DryRunAt            time.Time      `json:"dry_run_at"`
}

// IsZero reports whether the result carries no signal — used by the engine
// to decide whether the dry-run was actually run or skipped (e.g. empty CSV
// or empty rules). It is intentionally strict: zero warnings still counts as
// a real result, so Warnings == nil is NOT enough.
func (r DryRunResult) IsZero() bool {
	return r.AffectedInpatients == 0 &&
		len(r.Departments) == 0 &&
		r.ProjectedDiffAmount == 0 &&
		len(r.Warnings) == 0
}

// PatientRecord is one row from testdata/inpatients_87.csv. The DRGCode
// field is left empty by the loader and filled in by MatchDRGCode when a
// rule matches; downstream aggregation only counts patients whose
// PrimaryDiagnosisCode hits at least one rule.
type PatientRecord struct {
	PatientID            string    `json:"patient_id"`
	Department           string    `json:"department"`
	AdmissionDate        time.Time `json:"admission_date"`
	PrimaryDiagnosisCode string    `json:"primary_diagnosis_code"`
	DRGCode              string    `json:"drg_code,omitempty"`
}

// Rule is the dry-run-side view of a DRG/DIP rule. It carries an
// ICD10Prefix that the matcher uses to decide whether a PatientRecord's
// primary diagnosis falls under this rule's payment group.
//
// workflow.RuleEntry does NOT carry ICD10Prefix because the production
// rule set is fetched from the insurance-platform HTTP endpoint, which is
// out of scope for the dry-run skeleton (Task 7 wires the real fetcher).
// The prefix is supplied by the dry-run fixture (testdata) or a future
// mapping table that links DRG codes to ICD-10 chapter ranges.
type Rule struct {
	Code        string  `json:"code"`
	Name        string  `json:"name"`
	Weight      float64 `json:"weight"`
	Limit       float64 `json:"limit"`
	ICD10Prefix string  `json:"icd10_prefix"`
}

// DictEntry is one row of the HIS dictionary table snapshot used by the
// projected-diff computation in dryrun.Engine.Run. The field layout is
// locked in here so the integration with internal/integration/his_dict
// compiles without further edits. Defined as its own type (rather than
// aliasing his_dict.DictEntry) so dryrun remains decoupled from the
// persistence packages — the integration layer maps between the two at
// the call site.
type DictEntry struct {
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	Weight        float64   `json:"weight"`
	Limit         float64   `json:"limit"`
	EffectiveDate time.Time `json:"effective_date"`
}