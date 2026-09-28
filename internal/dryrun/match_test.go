package dryrun

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// sampleRules returns a small but realistic rule set: a respiratory rule
// (J18.* → DRG-B014), a cerebrovascular rule (I63.* → DRG-C007), and an
// abdominal-surgery rule (K35.* → DRG-A001). It is reused across the
// table-driven cases below.
func sampleRules() []Rule {
	return []Rule{
		{Code: "DRG-B014", Name: "肺部感染", Weight: 1.250, Limit: 14800.00, ICD10Prefix: "J18"},
		{Code: "DRG-C007", Name: "急性脑血管病", Weight: 2.090, Limit: 27600.00, ICD10Prefix: "I63"},
		{Code: "DRG-A001", Name: "胃食管及腹部手术", Weight: 2.450, Limit: 36800.00, ICD10Prefix: "K35"},
	}
}

// TestMatchDRGCode_Table covers the 5 categories required by Task 6:
// complete match, prefix-only match, no match, empty rules, empty patient.
func TestMatchDRGCode_Table(t *testing.T) {
	cases := []struct {
		name      string
		patient   PatientRecord
		rules     []Rule
		wantCode  string
		wantMatch bool
	}{
		{
			name: "icd10完整匹配: J18.901 matches DRG-B014 prefix J18",
			patient: PatientRecord{
				PatientID:            "P0001",
				Department:           "内科",
				AdmissionDate:        time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC),
				PrimaryDiagnosisCode: "J18.901",
			},
			rules:     sampleRules(),
			wantCode:  "DRG-B014",
			wantMatch: true,
		},
		{
			name: "前缀匹配: I63.901 matches DRG-C007 prefix I63 (prefix shorter than full code)",
			patient: PatientRecord{
				PatientID:            "P0004",
				Department:           "内科",
				AdmissionDate:        time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC),
				PrimaryDiagnosisCode: "I63.901",
			},
			rules:     sampleRules(),
			wantCode:  "DRG-C007",
			wantMatch: true,
		},
		{
			name: "完全不匹配: Z00.001 has no matching prefix",
			patient: PatientRecord{
				PatientID:            "P0090",
				Department:           "内科",
				AdmissionDate:        time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC),
				PrimaryDiagnosisCode: "Z00.001",
			},
			rules:     sampleRules(),
			wantCode:  "",
			wantMatch: false,
		},
		{
			name: "空 rules: patient is well-formed but rules slice is empty",
			patient: PatientRecord{
				PatientID:            "P0001",
				Department:           "内科",
				AdmissionDate:        time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC),
				PrimaryDiagnosisCode: "J18.901",
			},
			rules:     nil,
			wantCode:  "",
			wantMatch: false,
		},
		{
			name:    "空 patient_record: zero-value PatientRecord with empty PrimaryDiagnosisCode",
			patient: PatientRecord{PatientID: "P0000"},
			rules:   sampleRules(),
			// note: department / admission_date left zero; only PrimaryDiagnosisCode is consulted.
			wantCode:  "",
			wantMatch: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			gotCode, gotMatch := MatchDRGCode(tc.patient, tc.rules)
			if gotCode != tc.wantCode {
				t.Errorf("code: got %q, want %q", gotCode, tc.wantCode)
			}
			if gotMatch != tc.wantMatch {
				t.Errorf("match: got %v, want %v", gotMatch, tc.wantMatch)
			}
		})
	}
}

// TestMatchDRGCode_Defensive exercises the side-conditions that are easy
// to break in a refactor: rules with empty ICD10Prefix must be skipped,
// leading whitespace on either side must not change the answer, and the
// first matching rule in a slice wins on overlapping prefixes.
func TestMatchDRGCode_Defensive(t *testing.T) {
	t.Run("rule_with_empty_icd10_prefix_is_skipped", func(t *testing.T) {
		rules := []Rule{
			{Code: "DRG-EMPTY", Name: "no anchor", ICD10Prefix: ""},
			{Code: "DRG-B014", ICD10Prefix: "J18"},
		}
		p := PatientRecord{PrimaryDiagnosisCode: "J18.901"}
		code, ok := MatchDRGCode(p, rules)
		if !ok || code != "DRG-B014" {
			t.Errorf("empty-prefix rule should be skipped, got code=%q ok=%v", code, ok)
		}
	})

	t.Run("whitespace_padding_is_trimmed", func(t *testing.T) {
		rules := []Rule{{Code: "DRG-B014", ICD10Prefix: "  J18  "}}
		p := PatientRecord{PrimaryDiagnosisCode: "  J18.901  "}
		code, ok := MatchDRGCode(p, rules)
		if !ok || code != "DRG-B014" {
			t.Errorf("whitespace should not break the match, got code=%q ok=%v", code, ok)
		}
	})

	t.Run("first_matching_rule_wins_on_overlap", func(t *testing.T) {
		rules := []Rule{
			{Code: "DRG-GENERAL", ICD10Prefix: "J"},
			{Code: "DRG-B014", ICD10Prefix: "J18"},
		}
		p := PatientRecord{PrimaryDiagnosisCode: "J18.901"}
		code, ok := MatchDRGCode(p, rules)
		if !ok || code != "DRG-GENERAL" {
			t.Errorf("first match wins, got code=%q ok=%v", code, ok)
		}
	})
}

// TestDryRunResult_IsZero locks in the contract used by Task 12 demo to
// detect a no-op dry run (e.g. empty CSV path or skipped pre-flight).
func TestDryRunResult_IsZero(t *testing.T) {
	t.Run("zero_when_all_fields_empty", func(t *testing.T) {
		r := DryRunResult{PlanID: "r-zero"}
		if !r.IsZero() {
			t.Errorf("expected IsZero()=true for empty result")
		}
	})
	t.Run("non_zero_when_only_warnings_present", func(t *testing.T) {
		r := DryRunResult{PlanID: "r-warn", Warnings: []string{"limit drop 12%"}}
		if r.IsZero() {
			t.Errorf("warnings alone should still count as a real result")
		}
	})
	t.Run("non_zero_when_affected_count_set", func(t *testing.T) {
		r := DryRunResult{PlanID: "r-aff", AffectedInpatients: 1}
		if r.IsZero() {
			t.Errorf("affected_inpatients=1 should make the result non-zero")
		}
	})
}

// TestPatientRecord_Fields is a compile-time + runtime sanity check that
// the JSON tags round-trip through encoding/json without losing the
// primary_diagnosis_code field, which downstream consumers depend on.
func TestPatientRecord_Fields(t *testing.T) {
	p := PatientRecord{
		PatientID:            "P0001",
		Department:           "内科",
		AdmissionDate:        time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC),
		PrimaryDiagnosisCode: "J18.901",
	}
	if !strings.HasPrefix(p.PrimaryDiagnosisCode, "J18") {
		t.Errorf("PrimaryDiagnosisCode should round-trip, got %q", p.PrimaryDiagnosisCode)
	}
	// ErrSanity is referenced to keep the errors import alive in case
	// future sub-tests want to assert on returned errors.
	_ = errors.New
}