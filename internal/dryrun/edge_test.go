// edge_test.go covers the Task 22 boundary suite: empty CSV (zero data
// rows), empty rules JSON, partial CSV rows (some columns missing), and
// GBK-encoded CSV (legacy Windows HIS export).
//
// These tests share the project-root testdata/ directory with cmd/demo
// so we resolve fixtures via filepath.Abs + walking up from the package
// directory, matching the helper in engine_test.go.
package dryrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// resolveEdgeFixture walks up from the test working directory to find
// testdata/<name> relative to the project root. Same idea as
// resolveFixtureCSV in engine_test.go but generic.
func resolveEdgeFixture(t *testing.T, name string) string {
	t.Helper()
	candidates := []string{
		filepath.Join("testdata", name),
		filepath.Join("..", "..", "testdata", name),
		filepath.Join("..", "..", "..", "testdata", name),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	t.Fatalf("testdata/%s not found (cwd=%s)", name, mustGetwd())
	return ""
}

// loadRulesFromJSON is a small helper that the empty-rules fixture test
// uses: in production the dry-run gets its rules from
// integration/insurance_platform.RuleSet, but for the edge suite we
// want a JSON fixture we can drop on disk. The shape mirrors the
// platform-side RuleSet (code/name/weight/limit/icd10_prefix).
func loadRulesFromJSON(t *testing.T, path string) []Rule {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc struct {
		Rules []Rule `json:"rules"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc.Rules
}

// TestEdge_EmptyCSV covers the primary empty-CSV boundary: a file that
// has the required header but no data rows. Engine.Run must return
// AffectedInpatients == 0, no error, and no warnings. Pass an empty
// rule set too so ComputeProjectedDiff takes the early-return short
// circuit (rather than emitting "新增规则" warnings for every rule
// code — those warnings would make the "no warnings" assertion noisy
// when only the CSV is empty, not the rules).
func TestEdge_EmptyCSV(t *testing.T) {
	csvPath := resolveEdgeFixture(t, "inpatients_empty.csv")
	eng := NewEngine(csvPath, "plan-empty", nil, nil)

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
	if len(result.Warnings) != 0 {
		t.Errorf("Empty CSV should not produce warnings; got: %v", result.Warnings)
	}
	if !result.IsZero() {
		// Affected=0 + Departments empty + diff=0 + warnings empty is the
		// canonical "I ran, there was nothing to count" zero result.
		t.Errorf("Empty CSV should produce zero result; got: %+v", result)
	}
}

// TestEdge_EmptyRulesJSON covers the second boundary: the rule fixture
// JSON parses to a zero-length rules slice. Running Engine.Run with an
// empty rule list must NOT error — it returns AffectedInpatients == 0
// and zero department counts. The fixture file is loaded fresh so a
// future fixture edit that accidentally re-introduces a default rule will
// surface here as a test failure.
func TestEdge_EmptyRulesJSON(t *testing.T) {
	csvPath := resolveEdgeFixture(t, "inpatients_87.csv")
	rulesPath := resolveEdgeFixture(t, "rules_empty.json")

	rules := loadRulesFromJSON(t, rulesPath)
	if len(rules) != 0 {
		t.Fatalf("rules_empty.json: parsed %d rules, want 0 (fixture should hold an empty array)", len(rules))
	}

	eng := NewEngine(csvPath, "plan-empty-rules", rules, nil)
	result, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Run with empty rules: unexpected error: %v", err)
	}
	if result.AffectedInpatients != 0 {
		t.Errorf("AffectedInpatients = %d, want 0 (no rules → no matches)", result.AffectedInpatients)
	}
	if len(result.Departments) != 0 {
		t.Errorf("Departments should be empty: %v", result.Departments)
	}
	// Note: warnings from ComputeProjectedDiff are NOT expected here —
	// with both currentDict nil and rules empty, ComputeProjectedDiff
	// short-circuits to (0, nil).
	if len(result.Warnings) != 0 {
		t.Errorf("Empty rules should not produce warnings; got: %v", result.Warnings)
	}
}

// TestEdge_PartialCSV covers the partial-fields boundary: some rows
// have empty primary_diagnosis_code, some have empty department, some
// have fewer columns than the header. None should crash; every
// malformed row should be silently skipped and surfaced via the
// "CSV skipped N row(s)" warning.
//
// Layout of testdata/inpatients_partial.csv (8 lines incl. header):
//
//	patient_id,department,admission_date,primary_diagnosis_code
//	P0001,内科,2026-09-12,J18.901       ← ok, match DRG-B014 (J18)
//	P0002,内科,2026-09-13                ← missing primary_diagnosis_code → skip
//	P0003,,2026-09-14,K81.001           ← empty department, K81 match
//	P0004,外科,2026-09-15,               ← empty primary_diagnosis_code → skip
//	P0005,外科,2026-09-16,K35.001        ← ok, match DRG-A001 (K35)
//	P0006                                ← short row (only 1 col) → skip
//	P0007,内科,2026-09-18,I63.901        ← ok, match DRG-C007 (I63)
//
// Expected: 4 affected inpatients (P0001/P0003/P0005/P0007), 3 skipped
// rows (P0002 + P0004 + P0006). Department distribution: 内科=2,
// 外科=1, "" (empty) = 1.
func TestEdge_PartialCSV(t *testing.T) {
	csvPath := resolveEdgeFixture(t, "inpatients_partial.csv")
	rules := fixtureDRGRules()
	eng := NewEngine(csvPath, "plan-partial", rules, nil)

	result, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Run on partial CSV: unexpected error: %v", err)
	}
	if result.AffectedInpatients != 4 {
		t.Errorf("AffectedInpatients = %d, want 4 (P0001/P0003/P0005/P0007)", result.AffectedInpatients)
	}
	// P0001 内科, P0003 empty-dept, P0005 外科, P0007 内科.
	if result.Departments["内科"] != 2 {
		t.Errorf("内科 = %d, want 2", result.Departments["内科"])
	}
	if result.Departments["外科"] != 1 {
		t.Errorf("外科 = %d, want 1", result.Departments["外科"])
	}
	if result.Departments[""] != 1 {
		t.Errorf("(empty department) = %d, want 1", result.Departments[""])
	}
	// Warnings must include a CSV skipped line. P0002 (empty diag) +
	// P0004 (empty diag) + P0006 (short row) → 3 skips.
	csvSkipFound := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "CSV skipped 3 row") {
			csvSkipFound = true
			break
		}
	}
	if !csvSkipFound {
		t.Errorf("expected a warning naming 'CSV skipped 3 row(s)'; got: %v", result.Warnings)
	}
}

// TestEdge_GBKCSV covers the GBK-encoded boundary. Real HIS exports
// from older Windows terminals often ship GBK without a BOM. The
// engine must auto-detect the encoding, transcode the bytes to UTF-8,
// and surface the Chinese department names correctly. We assert on the
// raw bytes that loadInpatients returns plus the result.Departments map
// to confirm Chinese characters round-tripped.
//
// Layout of testdata/inpatients_gbk.csv:
//
//	patient_id,department,admission_date,primary_diagnosis_code
//	P0001,内科,2026-09-12,J18.901       (内=C4D5, 科=BF C6 in GBK)
//	P0002,内科,2026-09-15,I63.901
//	P0003,外科,2026-09-18,K35.001
//	P0004,外科,2026-09-20,K81.001
//	P0005,妇产科,2026-09-22,O80.001     (妇=BBF0, 产=B2FA)
//	P0006,儿科,2026-09-25,P07.001       (儿=B6F9)
func TestEdge_GBKCSV(t *testing.T) {
	csvPath := resolveEdgeFixture(t, "inpatients_gbk.csv")
	rules := fixtureDRGRules()
	eng := NewEngine(csvPath, "plan-gbk", rules, nil)

	result, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Run on GBK CSV: unexpected error: %v", err)
	}
	if result.AffectedInpatients != 6 {
		t.Errorf("AffectedInpatients = %d, want 6", result.AffectedInpatients)
	}
	// Department distribution: 内科=2, 外科=2, 妇产科=1, 儿科=1.
	want := map[string]int{"内科": 2, "外科": 2, "妇产科": 1, "儿科": 1}
	for dept, n := range want {
		if got := result.Departments[dept]; got != n {
			t.Errorf("department %q: got %d, want %d (got all: %v)",
				dept, got, n, result.Departments)
		}
	}
}

// TestEdge_GBKCSV_WithBOM is the BOM-path sibling: same shape as
// TestEdge_GBKCSV but the CSV starts with the full GB18030 BOM
// (84 31 95 33). The auto-detector must recognise the BOM, strip it,
// and decode the rest as GBK. If the BOM detector picks the wrong path
// (e.g. treats it as UTF-8 and fails validation) the test surfaces a
// distinct error.
func TestEdge_GBKCSV_WithBOM(t *testing.T) {
	csvPath := resolveEdgeFixture(t, "inpatients_gbk_bom.csv")
	rules := fixtureDRGRules()
	eng := NewEngine(csvPath, "plan-gbk-bom", rules, nil)

	result, err := eng.Run(context.Background())
	if err != nil {
		t.Fatalf("Run on GBK-with-BOM CSV: unexpected error: %v", err)
	}
	if result.AffectedInpatients != 3 {
		t.Errorf("AffectedInpatients = %d, want 3", result.AffectedInpatients)
	}
	if result.Departments["内科"] != 2 {
		t.Errorf("内科 = %d, want 2", result.Departments["内科"])
	}
	if result.Departments["外科"] != 1 {
		t.Errorf("外科 = %d, want 1", result.Departments["外科"])
	}
}

// TestEdge_DetectEncoding covers the detector directly so future
// changes to the BOM heuristics are testable in isolation. Each
// payload asserts on the label returned.
func TestEdge_DetectEncoding(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty bytes", []byte{}, "ascii"},
		{"plain ascii", []byte("patient_id,department\n"), "ascii"},
		{"utf8 with bom", append(append([]byte{}, utf8BOM...), []byte("内科\n")...), "utf-8"},
		{"utf8 without bom", []byte("内科\n"), "utf-8"},
		{"gbk full bom", append(append([]byte{}, gbkBOMFull...), []byte("GBK content")...), "gbk"},
		{"gbk half bom", append(append([]byte{}, gbkBOMHalf...), []byte("GBK content")...), "gbk"},
		{"gbk no bom (Chinese)", []byte{0xC4, 0xDA, 0xBF, 0xC6}, "gbk"}, // 内科 in GBK
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectEncoding(tc.in); got != tc.want {
				t.Errorf("detectEncoding = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEdge_DecodeCSVBytes covers the decoder end-to-end: BOM stripping
// + transcoding. After decodeCSVBytes returns, the result must be
// valid UTF-8 regardless of the input encoding, and the label must
// match the detector.
func TestEdge_DecodeCSVBytes(t *testing.T) {
	// 内科 in GBK = C4 DA BF C6; in UTF-8 = E5 86 85 E7 A7 91.
	gbkBytes := []byte{0xC4, 0xDA, 0xBF, 0xC6, '\n', 'O', 'K', '\n'}
	out, label, err := decodeCSVBytes(gbkBytes)
	if err != nil {
		t.Fatalf("decodeCSVBytes: unexpected error: %v", err)
	}
	if label != EncodingGBK {
		t.Errorf("label = %q, want %q", label, EncodingGBK)
	}
	// 内科 in UTF-8 is 6 bytes per char = 12 bytes.
	if !strings.Contains(string(out), "内科") {
		t.Errorf("decoded output missing 中文 '内科': %q", string(out))
	}
	if !utf8.Valid(out) {
		t.Errorf("decoded output is not valid UTF-8: % x", out)
	}

	// UTF-8 BOM stripping: leading BOM must be gone from the result.
	bomOnly := append(append([]byte{}, utf8BOM...), []byte("hello")...)
	out, label, err = decodeCSVBytes(bomOnly)
	if err != nil {
		t.Fatalf("decodeCSVBytes BOM: %v", err)
	}
	if label != EncodingUTF8 {
		t.Errorf("label = %q, want %q", label, EncodingUTF8)
	}
	if !strings.HasPrefix(string(out), "hello") {
		t.Errorf("BOM not stripped: %q", string(out))
	}

	// Pure ASCII: should pass through with label "ascii".
	ascii := []byte("patient_id,department\n")
	out, label, err = decodeCSVBytes(ascii)
	if err != nil {
		t.Fatalf("decodeCSVBytes ASCII: %v", err)
	}
	if label != EncodingASCII {
		t.Errorf("label = %q, want %q", label, EncodingASCII)
	}
	if string(out) != string(ascii) {
		t.Errorf("ASCII pass-through mutated bytes: %q vs %q", string(out), string(ascii))
	}
}