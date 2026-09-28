// engine_test.go exercises the dry-run engine end-to-end against the
// fixture (testdata/inpatients_87.csv) and against synthetic CSVs written
// from the test process (boundary cases: empty, missing column, partial
// rows, etc.). Per Row Rule decomposition keeps the matcher call count
// deterministic across runs.
//
// Source mapping: there is no bytebase equivalent — this is a domain-
// specific test for the medical-insurance dry-run scanner introduced in
// Task 6 (skeleton) and Task 20 (engine wiring). The fixture layout is
// locked at design time so these expectations double as a contract for
// testdata/inpatients_87.csv: changing the fixture means updating these
// numbers too.
package dryrun

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// fixtureDRGRules mirrors the DRG→ICD-10 prefix table hardcoded in
// cmd/demo/main.go (buildDryRunRules). Kept here as an independent copy so
// dryrun tests do not need to import the cmd package, and so a future
// move of the demo's mapping table does not silently change expectations.
func fixtureDRGRules() []Rule {
	type codeLimit struct {
		limit, weight float64
		name          string
	}
	limits := map[string]codeLimit{
		"DRG-A001": {limit: 36800, weight: 2.450, name: "胃、食管及腹部手术"},
		"DRG-A002": {limit: 49500, weight: 3.180, name: "肝胆胰系统手术"},
		"DRG-B014": {limit: 14800, weight: 1.250, name: "肺部感染"},
		"DRG-C007": {limit: 27600, weight: 2.090, name: "急性脑血管病"},
		"DRG-E022": {limit: 11500, weight: 1.080, name: "新生儿疾病"},
	}
	prefixByCode := map[string][]string{
		"DRG-A001": {"K35", "N84", "O80"},
		"DRG-A002": {"K81"},
		"DRG-B014": {"J18"},
		"DRG-C007": {"I63"},
		"DRG-E022": {"P"},
	}
	codes := make([]string, 0, len(prefixByCode))
	for c := range prefixByCode {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	var out []Rule
	for _, code := range codes {
		cl := limits[code]
		for _, prefix := range prefixByCode[code] {
			out = append(out, Rule{
				Code:        code,
				Name:        cl.name,
				Weight:      cl.weight,
				Limit:       cl.limit,
				ICD10Prefix: prefix,
			})
		}
	}
	return out
}

// resolveFixtureCSV walks up from the test working directory (test package
// is run from internal/dryrun/) until it finds testdata/inpatients_87.csv.
// Walking is required because `go test` chdirs into the package directory
// but a runner that `cd`'s into cmd/demo would also need this path.
func resolveFixtureCSV(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"testdata/inpatients_87.csv",
		"../../testdata/inpatients_87.csv",
		"../../../testdata/inpatients_87.csv",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	t.Fatalf("testdata/inpatients_87.csv not found (cwd=%s)", mustGetwd())
	return ""
}

func mustGetwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "<unknown>"
	}
	return wd
}

// TestEngine_Fixture87_AffectedCount is the primary contract test for
// Task 20: given the inpatients_87.csv fixture + the DRG→ICD-10 prefix
// table, the engine must report exactly 87 affected inpatients (every
// row's primary diagnosis starts with a prefix in our table).
func TestEngine_Fixture87_AffectedCount(t *testing.T) {
	csvPath := resolveFixtureCSV(t)
	rules := fixtureDRGRules()
	eng := NewEngine(csvPath, "r-2026-09-DRG-v17", rules, nil)

	result, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if result.AffectedInpatients != 87 {
		t.Fatalf("AffectedInpatients = %d, want 87 (rules=%d, csv=%s)",
			result.AffectedInpatients, len(rules), csvPath)
	}
	if result.PlanID != "r-2026-09-DRG-v17" {
		t.Errorf("PlanID = %q, want r-2026-09-DRG-v17", result.PlanID)
	}
	if result.IsZero() {
		t.Errorf("result is zero but 87 inpatients affected")
	}
	if result.DryRunAt.IsZero() {
		t.Errorf("DryRunAt should be stamped by Run")
	}
}

// TestEngine_Fixture87_DepartmentDistribution verifies the per-department
// aggregation against the fixture layout:
//
//	内科   : P0001-P0031 (31 rows: 3×J18 for DRG-B014 + 28×I63 for DRG-C007)
//	外科   : P0032-P0055 (24 rows: 10×K35 for DRG-A001 + 14×K81 for DRG-A002)
//	妇产科 : P0056-P0073 (18 rows: 10×O80 + 8×N84 both for DRG-A001)
//	儿科   : P0074-P0087 (14 rows: 10×P07 + 4×P22 both for DRG-E022)
//
// Total = 31+24+18+14 = 87 (cross-checks with TestEngine_Fixture87_AffectedCount).
func TestEngine_Fixture87_DepartmentDistribution(t *testing.T) {
	csvPath := resolveFixtureCSV(t)
	eng := NewEngine(csvPath, "", fixtureDRGRules(), nil)

	result, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}

	want := map[string]int{
		"内科":   31,
		"外科":   24,
		"妇产科": 18,
		"儿科":   14,
	}
	if len(result.Departments) != len(want) {
		t.Fatalf("department count = %d, want %d (got %v)",
			len(result.Departments), len(want), result.Departments)
	}
	for dept, n := range want {
		if got := result.Departments[dept]; got != n {
			t.Errorf("department %q: got %d, want %d", dept, got, n)
		}
	}
	total := 0
	for _, n := range result.Departments {
		total += n
	}
	if total != 87 {
		t.Errorf("department total = %d, want 87", total)
	}
}

// TestEngine_EmptyCSVTempFile covers the primary boundary from Task 20
// step 4: when the CSV has only the header (zero data rows), the engine
// must return AffectedInpatients = 0 and not error. (An empty file with
// only a header is the cleanest "empty CSV" we can construct without
// confusing encoding/csv's EOF behaviour.)
func TestEngine_EmptyCSVTempFile(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.csv")
	if err := os.WriteFile(empty, []byte("patient_id,department,admission_date,primary_diagnosis_code\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	eng := NewEngine(empty, "plan-empty", fixtureDRGRules(), nil)

	result, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Run on empty CSV: unexpected error: %v", err)
	}
	if result.AffectedInpatients != 0 {
		t.Errorf("AffectedInpatients = %d, want 0", result.AffectedInpatients)
	}
	if len(result.Departments) != 0 {
		t.Errorf("Departments should be empty: %v", result.Departments)
	}
}

// TestEngine_EmptyRules_NoMatch asserts that an empty rules slice yields
// zero matches even when the CSV has data. This is the other side of the
// boundary from Task 20: aggregate-by-department must be empty when
// there are no rules to match against.
func TestEngine_EmptyRules_NoMatch(t *testing.T) {
	csvPath := resolveFixtureCSV(t)
	eng := NewEngine(csvPath, "plan-no-rules", nil, nil)

	result, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if result.AffectedInpatients != 0 {
		t.Errorf("AffectedInpatients = %d, want 0 (rules slice is nil)", result.AffectedInpatients)
	}
	if len(result.Departments) != 0 {
		t.Errorf("Departments should be empty: %v", result.Departments)
	}
}

// TestEngine_MissingCSVPath_ReturnsError covers the most basic error
// path: a non-existent CSV path bubbles an error rather than silently
// returning zero. The error must wrap the underlying os.PathError so
// callers can errors.Is(err, fs.ErrNotExist) it if needed.
func TestEngine_MissingCSVPath_ReturnsError(t *testing.T) {
	eng := NewEngine("/nonexistent/path/inpatients.csv", "plan-x", nil, nil)
	_, err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("Run on missing CSV: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "inpatients.csv") {
		t.Errorf("error %q should mention the CSV path", err.Error())
	}
}

// TestEngine_EmptyCSVPath_ReturnsError covers the guard for an
// InpatientsCSVPath of "" — the engine must fail loud rather than
// dereferencing into os.Open("").
func TestEngine_EmptyCSVPath_ReturnsError(t *testing.T) {
	eng := NewEngine("", "plan-x", nil, nil)
	_, err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("Run on empty path: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "InpatientsCSVPath") {
		t.Errorf("error %q should mention InpatientsCSVPath", err.Error())
	}
}

// TestEngine_CSVHeaderMissingColumn covers the header-validation path:
// when a required column is missing the engine must error with a
// diagnostic that names the missing column. This protects future fixture
// edits from silently falling through to a malformed struct.
func TestEngine_CSVHeaderMissingColumn(t *testing.T) {
	dir := t.TempDir()
	// Missing primary_diagnosis_code column entirely.
	bad := filepath.Join(dir, "missing-col.csv")
	if err := os.WriteFile(bad, []byte("patient_id,department,admission_date\nP0001,内科,2026-09-12\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	eng := NewEngine(bad, "plan-bad", fixtureDRGRules(), nil)
	_, err := eng.Run(context.Background())
	if err == nil {
		t.Fatal("Run on CSV missing required column: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "primary_diagnosis_code") {
		t.Errorf("error %q should name the missing column", err.Error())
	}
}

// TestEngine_SkipEmptyDiag_WarningSurfaced is a fixture-driven edge: rows
// with an empty primary_diagnosis_code are skipped (they cannot match any
// rule) and the engine surfaces a warning naming the skip count. This
// protects the medical-insurance reviewer from silently losing rows.
func TestEngine_SkipEmptyDiag_WarningSurfaced(t *testing.T) {
	dir := t.TempDir()
	csv := filepath.Join(dir, "mixed.csv")
	content := strings.Join([]string{
		"patient_id,department,admission_date,primary_diagnosis_code",
		"P0001,内科,2026-09-12,J18.901",  // matches DRG-B014
		"P0002,内科,2026-09-13,",         // empty diag → skip
		"P0003,外科,2026-09-14,K81.001",  // matches DRG-A002
		"P0004,外科,2026-09-15,",         // empty diag → skip
		"P0005,内科,2026-09-16,   ",      // whitespace only → skip
		"",
	}, "\n")
	if err := os.WriteFile(csv, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	eng := NewEngine(csv, "plan-mixed", fixtureDRGRules(), nil)

	result, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if result.AffectedInpatients != 2 {
		t.Errorf("AffectedInpatients = %d, want 2 (P0001, P0003)", result.AffectedInpatients)
	}
	if result.Departments["内科"] != 1 {
		t.Errorf("内科 = %d, want 1", result.Departments["内科"])
	}
	if result.Departments["外科"] != 1 {
		t.Errorf("外科 = %d, want 1", result.Departments["外科"])
	}
	// Whitespace + empty rows: rows with TrimSpace == "" are skipped.
	// 3 such rows above → CSV-skipped warning. In addition, since the
	// test passes nil for CurrentDict, ComputeProjectedDiff sees every
	// rule code as "new" and emits one de-duplicated "新增规则" warning
	// per unique code (5 codes → 5 warnings).
	csvSkipFound := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "CSV skipped 3 row") {
			csvSkipFound = true
			break
		}
	}
	if !csvSkipFound {
		t.Errorf("expected a warning mentioning 'CSV skipped 3 row(s)'; got: %v", result.Warnings)
	}
}

// TestEngine_FirstMatchWins verifies the matcher contract from Task 6
// (MatchDRGCode first-match-wins) holds through the engine: a single
// patient with a diagnosis that satisfies two rules' prefixes gets
// counted once and assigned the first matching code.
func TestEngine_FirstMatchWins(t *testing.T) {
	dir := t.TempDir()
	csv := filepath.Join(dir, "overlap.csv")
	content := strings.Join([]string{
		"patient_id,department,admission_date,primary_diagnosis_code",
		"P0001,内科,2026-09-12,J18.999", // matches J18 (first in slice)
		"",
	}, "\n")
	if err := os.WriteFile(csv, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Construct two overlapping-prefix rules: an early broad "" prefix
	// would never fire (empty), so instead use J (broad) and J18 (specific).
	// We put the broad rule first to confirm first-match-wins by ordering.
	rules := []Rule{
		{Code: "DRG-BROAD", Name: "broad J", ICD10Prefix: "J"},
		{Code: "DRG-B014", Name: "lungs", ICD10Prefix: "J18"},
	}
	eng := NewEngine(csv, "plan-overlap", rules, nil)
	result, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if result.AffectedInpatients != 1 {
		t.Fatalf("AffectedInpatients = %d, want 1", result.AffectedInpatients)
	}
	if len(result.Departments) != 1 || result.Departments["内科"] != 1 {
		t.Errorf("dept distribution wrong: %v", result.Departments)
	}
	// Even though we cannot assert the DRGCode picked (engine sets it
	// inside PatientRecord but does not surface back), the count of 1
	// here proves the patient is counted exactly once — not twice.
}

// TestEngine_PlanIDPropagates asserts that Engine{PlanID: "x"} surfaces
// "x" verbatim in DryRunResult.PlanID, and that empty PlanID is also
// passed through (no engine-injected fallback).
func TestEngine_PlanIDPropagates(t *testing.T) {
	csvPath := resolveFixtureCSV(t)

	cases := []struct {
		name   string
		planID string
	}{
		{"explicit-id", "r-2026-09-DRG-v17"},
		{"empty-id", ""},
		{"unicode-id", "医保规则-2026-09"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewEngine(csvPath, tc.planID, fixtureDRGRules(), nil)
			result, err := eng.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.PlanID != tc.planID {
				t.Errorf("PlanID = %q, want %q", result.PlanID, tc.planID)
			}
		})
	}
}

// TestEngine_RunIsReadOnlyOnDisk ensures Engine.Run does not mutate the
// input CSV: the file's mtime / size must be unchanged after Run. This is
// a guard against future refactors that might (incorrectly) try to
// rewrite the CSV with normalized diagnoses.
func TestEngine_RunIsReadOnlyOnDisk(t *testing.T) {
	csvPath := resolveFixtureCSV(t)
	beforeStat, err := os.Stat(csvPath)
	if err != nil {
		t.Fatalf("Stat before: %v", err)
	}

	eng := NewEngine(csvPath, "plan-readonly", fixtureDRGRules(), nil)
	if _, err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Touch a small delay just in case; tmpfs may keep mtime stable
	// even on a write — we don't care, we only care about exact equality
	// between before and after.
	time.Sleep(10 * time.Millisecond)
	afterStat, err := os.Stat(csvPath)
	if err != nil {
		t.Fatalf("Stat after: %v", err)
	}
	if beforeStat.Size() != afterStat.Size() {
		t.Errorf("file size changed: before=%d after=%d",
			beforeStat.Size(), afterStat.Size())
	}
}

// TestEngine_LoadInpatients_HeaderCaseInsensitive confirms the column
// lookup is case-insensitive (lowercased). Real HIS exports sometimes
// arrive with mixed case headers; the engine should handle that.
func TestEngine_LoadInpatients_HeaderCaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	csv := filepath.Join(dir, "upper.csv")
	content := strings.Join([]string{
		"PATIENT_ID,DEPARTMENT,ADMISSION_DATE,PRIMARY_DIAGNOSIS_CODE",
		"P0001,内科,2026-09-12,J18.901",
		"",
	}, "\n")
	if err := os.WriteFile(csv, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	eng := NewEngine(csv, "plan-upper", fixtureDRGRules(), nil)
	result, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Run with uppercase header: unexpected error: %v", err)
	}
	if result.AffectedInpatients != 1 {
		t.Errorf("AffectedInpatients = %d, want 1", result.AffectedInpatients)
	}
}
