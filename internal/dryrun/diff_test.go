// diff_test.go exercises Task 21's two new surfaces:
//   - ComputeProjectedDiff (free function, three warning categories)
//   - RenderDryRunJSON (JSON serialization with 中文 characters)
//
// The tests are table-driven where it makes sense (warning categories) and
// behavioural where the JSON shape matters (RenderDryRunJSON). Each
// warning category is named verbatim so a failure tells the reviewer
// exactly which contract regressed.
package dryrun

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// findWarning returns the index of the first warning containing substr,
// or -1 if none. Used to assert "at least one warning of this kind
// exists" without coupling the test to a specific wording prefix (the
// wording in ComputeProjectedDiff may evolve; the category labels are
// stable).
func findWarning(warnings []string, substr string) int {
	for i, w := range warnings {
		if strings.Contains(w, substr) {
			return i
		}
	}
	return -1
}

// hasWarning returns whether substr appears in any warning string.
func hasWarning(warnings []string, substr string) bool {
	return findWarning(warnings, substr) >= 0
}

// TestComputeProjectedDiff_LimitDrop5Pct_NoWarning covers warning
// category 1 NEGATIVE case: a 5% limit drop is BELOW the 10% threshold
// and must NOT surface as a warning. The diff is still accumulated
// because the math applies (sum of drops); only the warning is suppressed.
func TestComputeProjectedDiff_LimitDrop5Pct_NoWarning(t *testing.T) {
	currentDict := []DictEntry{
		{Code: "DRG-A001", Limit: 36800},
	}
	newRules := []Rule{
		{Code: "DRG-A001", Limit: 34960}, // 36800 - 34960 = 1840 ≈ 5% drop
	}
	diff, warnings := ComputeProjectedDiff(currentDict, newRules)
	// 5% < 10% threshold → no warning.
	if hasWarning(warnings, "limit 降幅") {
		t.Errorf("expected no limit-drop warning at 5%%; got: %v", warnings)
	}
	// But the projected diff must still reflect the numeric drop.
	if diff != 1840 {
		t.Errorf("projected diff = %v, want 1840 (36800 - 34960)", diff)
	}
}

// TestComputeProjectedDiff_LimitDrop12Pct_Warning covers warning
// category 1 POSITIVE case: a 12% drop crosses the 10% threshold and
// must emit a warning naming the code + percentage + (prev → new).
func TestComputeProjectedDiff_LimitDrop12Pct_Warning(t *testing.T) {
	currentDict := []DictEntry{
		{Code: "DRG-B014", Limit: 15800},
	}
	newRules := []Rule{
		{Code: "DRG-B014", Limit: 13904}, // 15800 - 13904 = 1896 ≈ 12%
	}
	diff, warnings := ComputeProjectedDiff(currentDict, newRules)
	if !hasWarning(warnings, "limit 降幅") {
		t.Fatalf("expected a limit-drop warning; got: %v", warnings)
	}
	if !hasWarning(warnings, "DRG-B014") {
		t.Errorf("warning should name the rule code DRG-B014; got: %v", warnings)
	}
	if !hasWarning(warnings, "12.0%") {
		t.Errorf("warning should print the percentage with 1 decimal; got: %v", warnings)
	}
	if diff != 1896 {
		t.Errorf("projected diff = %v, want 1896 (15800 - 13904)", diff)
	}
}

// TestComputeProjectedDiff_NewRule_NoPrev covers warning category 2:
// a rule present in the new rule set but ABSENT from the current
// dictionary. No prior limit to diff against → emits "新增规则 (无
// prev_version 对照)" warning.
func TestComputeProjectedDiff_NewRule_NoPrev(t *testing.T) {
	currentDict := []DictEntry{
		{Code: "DRG-A001", Limit: 36800}, // only A001 in current
	}
	newRules := []Rule{
		{Code: "DRG-A001", Limit: 36800},
		{Code: "DRG-X999", Limit: 22000}, // entirely new
	}
	diff, warnings := ComputeProjectedDiff(currentDict, newRules)
	if !hasWarning(warnings, "DRG-X999 新增规则") {
		t.Fatalf("expected a new-rule warning for DRG-X999; got: %v", warnings)
	}
	if !hasWarning(warnings, "无 prev_version 对照") {
		t.Errorf("warning should mention no prev version; got: %v", warnings)
	}
	// DRG-X999 contributes 0 to the diff (no prior limit to compare).
	if diff != 0 {
		t.Errorf("projected diff = %v, want 0 (X999 contributes 0)", diff)
	}
}

// TestComputeProjectedDiff_DeletedRule_NoMatchingPrev covers warning
// category 3: a rule present in the current dictionary but ABSENT
// from the new rule set. Emits "删除规则 (无对应 prev)" warning.
func TestComputeProjectedDiff_DeletedRule_NoMatchingPrev(t *testing.T) {
	currentDict := []DictEntry{
		{Code: "DRG-A001", Limit: 36800},
		{Code: "DRG-OLD1", Limit: 5000}, // about to be retired
	}
	newRules := []Rule{
		{Code: "DRG-A001", Limit: 36800}, // unchanged
		// DRG-OLD1 deliberately missing → must trigger deletion warning
	}
	_, warnings := ComputeProjectedDiff(currentDict, newRules)
	if !hasWarning(warnings, "DRG-OLD1 删除规则") {
		t.Fatalf("expected a deleted-rule warning for DRG-OLD1; got: %v", warnings)
	}
	if !hasWarning(warnings, "无对应 prev") {
		t.Errorf("warning should mention no matching prev; got: %v", warnings)
	}
	// No drop → diff = 0 (DRG-A001 unchanged, DRG-OLD1 contributes 0).
	if len(warnings) != 1 {
		t.Errorf("expected exactly 1 warning; got %d: %v", len(warnings), warnings)
	}
}

// TestComputeProjectedDiff_MixedScenario combines all three warning
// categories into one realistic fixture: one rule with a limit drop,
// one brand-new rule, one deleted rule, plus one unchanged rule. This
// is the closest analog to a real month-end DRG bump.
func TestComputeProjectedDiff_MixedScenario(t *testing.T) {
	currentDict := []DictEntry{
		{Code: "DRG-A001", Limit: 36800}, // unchanged
		{Code: "DRG-B014", Limit: 15800}, // will drop to 13904 (12%)
		{Code: "DRG-OLD1", Limit: 5000},  // being deleted
	}
	newRules := []Rule{
		{Code: "DRG-A001", Limit: 36800}, // unchanged
		{Code: "DRG-B014", Limit: 13904}, // drop
		{Code: "DRG-X999", Limit: 22000}, // new
		// DRG-OLD1 absent
	}
	diff, warnings := ComputeProjectedDiff(currentDict, newRules)

	if !hasWarning(warnings, "DRG-B014 limit 降幅") {
		t.Errorf("expected limit-drop warning for DRG-B014; got: %v", warnings)
	}
	if !hasWarning(warnings, "DRG-X999 新增规则") {
		t.Errorf("expected new-rule warning for DRG-X999; got: %v", warnings)
	}
	if !hasWarning(warnings, "DRG-OLD1 删除规则") {
		t.Errorf("expected deleted-rule warning for DRG-OLD1; got: %v", warnings)
	}

	// Diff = 15800 - 13904 = 1896 (only DRG-B014 contributes).
	if diff != 1896 {
		t.Errorf("projected diff = %v, want 1896 (DRG-B014 only)", diff)
	}

	// Warnings must be sorted (deterministic output).
	if !sortStringsAreEqual(warnings, sortedCopy(warnings)) {
		t.Errorf("warnings are not in sorted order: %v", warnings)
	}

	// No false-positive DRG-A001 warning.
	if findWarning(warnings, "DRG-A001") != -1 {
		t.Errorf("unchanged DRG-A001 should produce no warning; got: %v", warnings)
	}
}

// TestRenderDryRunJSON_FieldCompleteness exercises the JSON output
// path. The serialized payload must include every documented field at
// the expected JSON key (snake_case), and 中文 characters in warnings +
// department names must round-trip cleanly (no \uXXXX escapes).
func TestRenderDryRunJSON_FieldCompleteness(t *testing.T) {
	result := DryRunResult{
		PlanID:              "r-2026-09-DRG-v17",
		AffectedInpatients:  87,
		Departments:         map[string]int{"内科": 31, "外科": 24, "妇产科": 18, "儿科": 14},
		ProjectedDiffAmount: -1896,
		Warnings: []string{
			"DRG-B014 limit 降幅 12.0% (15800 → 13904)",
			"DRG-X999 新增规则（无 prev_version 对照）",
		},
		DryRunAt: mustParseTime("2026-09-28T07:42:00Z"),
	}

	out, err := RenderDryRunJSON(result)
	if err != nil {
		t.Fatalf("RenderDryRunJSON: %v", err)
	}

	// All 6 fields must appear as snake_case JSON keys.
	for _, key := range []string{
		`"plan_id"`,
		`"affected_inpatients"`,
		`"departments"`,
		`"projected_diff_amount"`,
		`"warnings"`,
		`"dry_run_at"`,
	} {
		if !strings.Contains(out, key) {
			t.Errorf("JSON output missing key %s\n---\n%s\n---", key, out)
		}
	}

	// 中文 must round-trip cleanly (raw UTF-8 bytes in the output, NO
	// ASCII-style 一 escape sequences). With SetEscapeHTML(false) the
	// encoder emits the raw UTF-8 char; with the default (true) it emits
	// the literal text "\uXXXX" (6 ASCII chars). We detect the bad path
	// by searching for the canonical escape sequence "一" (which in
	// Go source is the literal ASCII text `一`).
	if strings.Contains(out, `一`) {
		t.Errorf("JSON output contains \\u4e00 escape (SetEscapeHTML(false) should prevent this):\n%s", out)
	}
	// And confirm the raw char IS present (positive assertion).
	if !strings.Contains(out, "内科") {
		t.Errorf("JSON output is missing raw UTF-8 中文 (expected 内科 in departments):\n%s", out)
	}

	// Re-parse and confirm the Go-side values survive the round trip.
	var decoded DryRunResult
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("Re-parse JSON: %v", err)
	}
	if decoded.PlanID != result.PlanID {
		t.Errorf("PlanID round-trip = %q, want %q", decoded.PlanID, result.PlanID)
	}
	if decoded.AffectedInpatients != result.AffectedInpatients {
		t.Errorf("AffectedInpatients round-trip = %d, want %d",
			decoded.AffectedInpatients, result.AffectedInpatients)
	}
	if decoded.ProjectedDiffAmount != result.ProjectedDiffAmount {
		t.Errorf("ProjectedDiffAmount round-trip = %v, want %v",
			decoded.ProjectedDiffAmount, result.ProjectedDiffAmount)
	}
	if decoded.Departments["内科"] != 31 || decoded.Departments["外科"] != 24 {
		t.Errorf("Departments round-trip mismatch: %v", decoded.Departments)
	}
	if len(decoded.Warnings) != 2 {
		t.Fatalf("Warnings round-trip: got %d warnings, want 2", len(decoded.Warnings))
	}

	// Must be pretty-printed (not minified) — the medical-insurance
	// reviewer reads this in the terminal.
	if !strings.Contains(out, "\n  ") {
		t.Errorf("JSON output is not pretty-printed (expected leading indent on inner lines)")
	}
}

// TestRenderDryRunJSON_EmptyWarnings exercises the zero-warnings edge:
// the JSON must still include the "warnings" key (with []), and parsing
// must yield a non-nil-but-zero-length slice.
func TestRenderDryRunJSON_EmptyWarnings(t *testing.T) {
	result := DryRunResult{
		PlanID:             "plan-empty",
		AffectedInpatients: 0,
		Departments:        map[string]int{},
		Warnings:           []string{},
		DryRunAt:           mustParseTime("2026-09-28T07:42:00Z"),
	}
	out, err := RenderDryRunJSON(result)
	if err != nil {
		t.Fatalf("RenderDryRunJSON: %v", err)
	}
	if !strings.Contains(out, `"warnings"`) {
		t.Errorf("JSON must include the warnings key even when empty:\n%s", out)
	}
	var decoded DryRunResult
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("Re-parse: %v", err)
	}
	if len(decoded.Warnings) != 0 {
		t.Errorf("expected empty warnings after round-trip, got %v", decoded.Warnings)
	}
}

// TestComputeProjectedDiff_BothEmpty covers the early-return path: if
// both inputs are empty (or nil), the function returns 0 / nil without
// panicking. This is the path cmd/demo runs against a brand-new HIS
// dictionary that has not yet been primed.
func TestComputeProjectedDiff_BothEmpty(t *testing.T) {
	diff, warnings := ComputeProjectedDiff(nil, nil)
	if diff != 0 {
		t.Errorf("diff = %v, want 0", diff)
	}
	// nil or empty are both acceptable — test length, not identity.
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want empty/nil", warnings)
	}
}

// --- test helpers ---

// mustParseTime is a tiny test-side helper that fails-fast on bad input.
func mustParseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic("mustParseTime: " + err.Error())
	}
	return t
}

// sortedCopy returns a fresh sorted copy of warnings using stdlib sort
// (kept here as a thin wrapper so the test file has no other dependency
// on engine internals beyond what is exported).
func sortedCopy(warnings []string) []string {
	out := make([]string, len(warnings))
	copy(out, warnings)
	stringSliceSort(out)
	return out
}

// sortStringsAreEqual returns true if a and b contain the same elements
// in the same order.
func sortStringsAreEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// stringSliceSort sorts a string slice in place using a simple
// insertion sort (these warning lists are <10 entries).
func stringSliceSort(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
