// Dry-run engine: scans the inpatients CSV, matches each patient's primary
// diagnosis against the rule's ICD10Prefix, aggregates affected counts per
// department, computes the projected settlement diff against the current
// HIS dictionary snapshot, and raises warnings when the limit drop crosses
// the configured threshold.
//
// Source mapping: bytebase has no equivalent dry-run scanner — this is a
// new module built for the medical-insurance domain. The shape (aggregate
// by department, prefix-match ICD-10 → DRG, diff against the current dict)
// is the medical-insurance office's own concern; see Task 7/20/21.
//
// Engine.Run is safe to call concurrently (it does not retain state
// between calls). The CSV is read once per Run and held in memory — for
// hospital-scale CSVs (≤10k rows) this is fine; production deployments
// with larger cohorts would stream the rows.
package dryrun

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// LimitDropWarnThreshold is the minimum percentage drop in `limit` that
// triggers a warning. A rule whose new limit is 10% or more below the
// previous limit surfaces as a warning so the medical-insurance office
// can flag it for review at the sign-off meeting.
const LimitDropWarnThreshold = 0.10

// Engine holds the inputs the dry-run needs at Run time. It is
// intentionally small — no caches, no goroutines — so a demo / unit test
// can construct one inline.
type Engine struct {
	// InpatientsCSVPath is the path to the on-disk CSV. The CSV must have
	// a header row followed by patient_id,department,admission_date,
	// primary_diagnosis_code (in that order).
	InpatientsCSVPath string

	// Rules is the rule set the matcher iterates over (order matters:
	// the first matching prefix wins, matching dryrun.MatchDRGCode).
	Rules []Rule

	// CurrentDict is the existing HIS dictionary snapshot used to compute
	// the projected-diff. May be empty — when empty, projected_diff_amount
	// is 0 and no limit-drop warnings are raised.
	CurrentDict []DictEntry

	// PlanID is stamped onto the returned DryRunResult. PlanID == ""
	// produces a result with plan_id == "" (no error).
	PlanID string
}

// NewEngine is a small constructor used by cmd/demo and tests. It copies
// the input slices so the caller can mutate them after construction
// without affecting the engine.
func NewEngine(csvPath, planID string, rules []Rule, currentDict []DictEntry) *Engine {
	rCopy := make([]Rule, len(rules))
	copy(rCopy, rules)
	dCopy := make([]DictEntry, len(currentDict))
	copy(dCopy, currentDict)
	return &Engine{
		InpatientsCSVPath: csvPath,
		Rules:             rCopy,
		CurrentDict:       dCopy,
		PlanID:            planID,
	}
}

// Run scans the inpatient CSV and produces a DryRunResult. Returns an
// error only when the CSV cannot be opened or its header is malformed;
// data-level errors (a row with an empty primary_diagnosis_code) are
// silently skipped and surfaced as SkippedRows in the result.
//
// The ctx parameter is accepted for forward-compat with streaming CSVs;
// the current implementation reads the file synchronously and does not
// honor cancellation. Callers should not rely on ctx to abort a Run.
func (e *Engine) Run(ctx context.Context) (DryRunResult, error) {
	result := DryRunResult{
		PlanID:      e.PlanID,
		Departments: map[string]int{},
		DryRunAt:    time.Now().UTC(),
	}

	patients, skippedFromLoad, err := e.loadInpatients(ctx)
	if err != nil {
		return result, err
	}

	affected, deptCounts, _ := e.aggregateByDepartment(patients)
	result.AffectedInpatients = affected
	for dept, n := range deptCounts {
		result.Departments[dept] = n
	}
	// Surface the load-time skip count (rows with empty/whitespace
	// primary_diagnosis_code are silently dropped by loadInpatients). The
	// medical-insurance reviewer needs to know if any rows were discarded
	// so they can investigate the upstream HIS extract.
	if skippedFromLoad > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("CSV skipped %d row(s) with empty primary_diagnosis_code", skippedFromLoad))
	}

	diff, warnings := ComputeProjectedDiff(e.CurrentDict, e.Rules)
	result.ProjectedDiffAmount = diff
	result.Warnings = append(result.Warnings, warnings...)
	return result, nil
}

// loadInpatients reads the CSV file from InpatientsCSVPath. Empty file /
// missing header → error. Each data row is parsed into a PatientRecord;
// rows with empty primary_diagnosis_code are skipped (counted separately).
//
// Encoding: GBK / GB18030 (older HIS exports from Windows terminals) is
// auto-detected from the BOM bytes and converted to UTF-8 before the
// CSV reader sees the stream. UTF-8 (with or without BOM) and US-ASCII
// pass through untouched. Anything that fails UTF-8 validation but has
// no GBK BOM is treated as GBK and trans-coded; decoding errors surface
// as "dryrun: decode CSV (GBK→UTF-8): …" so the medical-insurance
// reviewer can locate the malformed export.
//
// The function uses encoding/csv so quoted commas / UTF-8 patient names
// / future columns don't break parsing.
func (e *Engine) loadInpatients(ctx context.Context) ([]PatientRecord, int, error) {
	if e.InpatientsCSVPath == "" {
		return nil, 0, fmt.Errorf("dryrun: InpatientsCSVPath is empty")
	}
	raw, err := os.ReadFile(e.InpatientsCSVPath)
	if err != nil {
		return nil, 0, fmt.Errorf("dryrun: open CSV %s: %w", e.InpatientsCSVPath, err)
	}
	utf8Bytes, _, err := decodeCSVBytes(raw)
	if err != nil {
		return nil, 0, fmt.Errorf("dryrun: decode CSV (auto-detect): %w", err)
	}

	r := csv.NewReader(bufio.NewReader(bytes.NewReader(utf8Bytes)))
	r.FieldsPerRecord = -1 // tolerate row-length variation

	header, err := r.Read()
	if err != nil {
		return nil, 0, fmt.Errorf("dryrun: read CSV header: %w", err)
	}
	colIdx := map[string]int{}
	for i, name := range header {
		colIdx[strings.TrimSpace(strings.ToLower(name))] = i
	}
	required := []string{"patient_id", "department", "admission_date", "primary_diagnosis_code"}
	for _, req := range required {
		if _, ok := colIdx[req]; !ok {
			return nil, 0, fmt.Errorf("dryrun: CSV header missing column %q (got %v)", req, header)
		}
	}

	var patients []PatientRecord
	var skipped int
	for {
		row, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("dryrun: read CSV row: %w", err)
		}
		// Defensive: if a row is short, len(colIdx) > len(row) will panic.
		// Encode that as a per-row skip (the medical-insurance reviewer
		// needs to see the count, not a 500).
		if len(row) < len(required) {
			skipped++
			continue
		}
		diag := strings.TrimSpace(row[colIdx["primary_diagnosis_code"]])
		if diag == "" {
			skipped++
			continue
		}
		admissionStr := strings.TrimSpace(row[colIdx["admission_date"]])
		var admission time.Time
		if admissionStr != "" {
			if t, perr := time.Parse("2006-01-02", admissionStr); perr == nil {
				admission = t
			}
		}
		patients = append(patients, PatientRecord{
			PatientID:            strings.TrimSpace(row[colIdx["patient_id"]]),
			Department:           strings.TrimSpace(row[colIdx["department"]]),
			AdmissionDate:        admission,
			PrimaryDiagnosisCode: diag,
		})
	}
	_ = ctx // reserved for future streaming path
	return patients, skipped, nil
}

// aggregateByDepartment runs MatchDRGCode for each patient and tallies
// (a) the number of patients whose primary diagnosis matches at least one
// rule, and (b) the per-department distribution of those matches.
//
// Patients that do not match any rule are silently skipped — they
// represent a healthy population unaffected by the rule change.
func (e *Engine) aggregateByDepartment(patients []PatientRecord) (int, map[string]int, int) {
	affected := 0
	depts := map[string]int{}
	skippedEmptyDiag := 0
	for _, p := range patients {
		if strings.TrimSpace(p.PrimaryDiagnosisCode) == "" {
			skippedEmptyDiag++
			continue
		}
		if code, ok := MatchDRGCode(p, e.Rules); ok && code != "" {
			affected++
			depts[p.Department]++
		}
	}
	return affected, depts, skippedEmptyDiag
}

// ComputeProjectedDiff compares the new rule set against the current HIS
// dictionary snapshot and returns (projected_diff_amount, warnings). It is
// the deterministic core that Engine.Run calls; exposed as a free function
// so the cmd/demo and the contract tests can call it with constructed
// fixtures without spinning up an Engine.
//
// Three warning categories are emitted (Task 21):
//
//  1. **Limit drop ≥10%** (`<code> limit 降幅 X.X% (prev → new)`) — the new
//     rule's payment limit falls below 90% of the previous version's limit;
//     the medical-insurance office needs to flag this for the review
//     meeting because the hospital's reimbursement shrinks materially.
//
//  2. **New rule has no prev version** (`<code> 新增规则（无 prev_version 对照）`)
//     — the rule exists in the new rule set but is absent from the
//     current HIS dictionary; no prior limit to diff against, so we cannot
//     estimate the settlement impact. The reviewer must confirm the rule
//     was just introduced (and not e.g. a typo for an existing rule).
//     Duplicates per code are de-duplicated — a DRG code with three
//     ICD10Prefix entries surfaces ONE warning, not three.
//
//  3. **Rule deleted, no matching prev** (`<code> 删除规则（无对应 prev）`) — the
//     current HIS dictionary has a rule the new rule set does NOT carry.
//     This is the inverse of (2); it could be intentional (deprecation)
//     or a regression. The reviewer must confirm either way.
//
// Math for the projected_diff_amount: for every Rule present in BOTH the
// current dictionary and the new rule set, the diff is (cur.Limit −
// rule.Limit). A positive drop (limit lowered) is added to total because
// the positive number represents how much less the new rule will pay
// versus the prior version — interpretable as the hospital's expected
// reduction in settlement. Rules missing from currentDict (new) or
// missing from newRules (deleted) contribute 0 to the diff number, since
// the warnings carry the signal for those.
//
// Output warnings are sorted lexicographically so the report is
// deterministic across runs.
//
// Parameters:
//   currentDict — the existing HIS dictionary snapshot (may be empty → 0 diff)
//   newRules    — the new rule set being evaluated (may be empty → 0 diff)
//
// Returns:
//   float64  — projected settlement diff in yuan, rounded to nearest yuan
//   []string — sorted warning texts (nil-or-empty when no warnings)
func ComputeProjectedDiff(currentDict []DictEntry, newRules []Rule) (float64, []string) {
	if len(currentDict) == 0 && len(newRules) == 0 {
		return 0, nil
	}

	currentByCode := make(map[string]DictEntry, len(currentDict))
	for _, d := range currentDict {
		currentByCode[d.Code] = d
	}
	newByCode := make(map[string]bool, len(newRules))
	for _, r := range newRules {
		newByCode[r.Code] = true
	}

	var total float64
	var warnings []string
	seenNewRule := make(map[string]bool, len(newRules))

	// Pass 1: walk new rules, emit "limit drop" or "new rule" warnings
	// and accumulate the projected diff.
	for _, rule := range newRules {
		cur, ok := currentByCode[rule.Code]
		if !ok {
			// Warning category 2: new rule with no prior dictionary entry.
			// De-dup by code so a single DRG with multiple ICD10Prefix
			// entries doesn't flood the report with redundant warnings.
			if !seenNewRule[rule.Code] {
				warnings = append(warnings, fmt.Sprintf("%s 新增规则（无 prev_version 对照）", rule.Code))
				seenNewRule[rule.Code] = true
			}
			continue
		}
		if cur.Limit <= 0 {
			// No prior limit recorded; treat as no diff (cannot estimate).
			continue
		}
		drop := cur.Limit - rule.Limit
		if drop > 0 {
			// Warning category 1: limit lowered by ≥10%.
			pct := drop / cur.Limit
			if pct >= LimitDropWarnThreshold {
				warnings = append(warnings, fmt.Sprintf("%s limit 降幅 %.1f%% (%.0f → %.0f)",
					rule.Code, pct*100, cur.Limit, rule.Limit))
			}
			total += drop // positive = hospital saves money on this rule
		}
	}

	// Pass 2: walk current dictionary and flag deleted rules.
	for _, d := range currentDict {
		if !newByCode[d.Code] {
			// Warning category 3: rule was in current dict but absent
			// from the new rule set.
			warnings = append(warnings, fmt.Sprintf("%s 删除规则（无对应 prev）", d.Code))
		}
	}

	sort.Strings(warnings)
	return roundYuan(total), warnings
}

// RenderDryRunJSON serializes a DryRunResult as JSON for printing in the
// terminal / writing to disk. The encoder is set with HTMLEscape=false
// so Chinese characters in warning text and department names pass through
// verbatim (the medical-insurance reviewer reads these in 中文); also
// SetIndent("", "  ") so the output is human-readable.
//
// If the result has a zero DryRunAt (i.e. was constructed by hand and
// never run through Engine.Run), we DO NOT auto-fill it — the caller
// passed in what they passed in, and silently rewriting that would hide
// the "I forgot to set DryRunAt" bug.
func RenderDryRunJSON(result DryRunResult) (string, error) {
	buf := &strings.Builder{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		return "", fmt.Errorf("dryrun: render JSON: %w", err)
	}
	return buf.String(), nil
}

// roundYuan rounds to the nearest yuan (no fractional cents). Avoids
// importing math just for one call site.
func roundYuan(v float64) float64 {
	return float64(int64(v + 0.5))
}

// Itoa is a small helper kept local to the package so callers don't need
// to import strconv for the diff log. Exported as Itoa for test code
// that wants to build predictable warning strings.
func Itoa(n int) string { return strconv.Itoa(n) }

// Encoding labels surfaced by detectEncoding / decodeCSVBytes so tests
// and audit-log readers know which path the bytes took.
const (
	EncodingUTF8 = "utf-8"
	EncodingGBK  = "gbk"
	EncodingASCII = "ascii"
)

// gbkBOM is the Windows GB18030 / GBK BOM (0x84 0x31 0x95 0x33, treated as
// the same logical marker — some Windows tools only emit the leading two
// bytes 0x84 0x31). We honour both.
var (
	gbkBOMFull = []byte{0x84, 0x31, 0x95, 0x33}
	gbkBOMHalf = []byte{0x84, 0x31}
	utf8BOM    = []byte{0xEF, 0xBB, 0xBF}
)

// detectEncoding inspects the leading bytes of raw and labels the
// encoding. The rule is intentionally conservative:
//
//  1. UTF-8 BOM (EF BB BF) → "utf-8" (BOM stripped downstream)
//  2. GBK full BOM (84 31 95 33) → "gbk"
//  3. GBK half BOM (84 31) → "gbk"
//  4. Otherwise: if bytes are valid UTF-8 (utf8.Valid) → "utf-8"
//  5. Otherwise: assume "gbk" (most HIS exports are GBK when they are
//     not UTF-8)
//
// The returned label is consumed by decodeCSVBytes; callers can also
// assert on it in tests to confirm the detector picked the right path.
func detectEncoding(raw []byte) string {
	if len(raw) >= 3 && bytes.HasPrefix(raw, utf8BOM) {
		return EncodingUTF8
	}
	if len(raw) >= 4 && bytes.HasPrefix(raw, gbkBOMFull) {
		return EncodingGBK
	}
	if len(raw) >= 2 && bytes.HasPrefix(raw, gbkBOMHalf) {
		return EncodingGBK
	}
	if utf8.Valid(raw) {
		// Pure ASCII is a subset of UTF-8 — when the buffer contains no
		// bytes > 0x7F, surface "ascii" so tests can distinguish a CSV
		// with no Chinese from one that was re-encoded.
		hasHigh := false
		for _, b := range raw {
			if b > 0x7F {
				hasHigh = true
				break
			}
		}
		if !hasHigh {
			return EncodingASCII
		}
		return EncodingUTF8
	}
	return EncodingGBK
}

// decodeCSVBytes runs the auto-detect + transcode. The returned label
// matches detectEncoding. A failure from the GBK decoder is wrapped with
// the original error so the caller can errors.Is on it.
func decodeCSVBytes(raw []byte) ([]byte, string, error) {
	enc := detectEncoding(raw)
	switch enc {
	case EncodingGBK:
		// simplifiedchinese.GB18030 covers GBK, GB2312 and GB18030 —
		// all the encodings a Chinese HIS export could ship in.
		decoder := simplifiedchinese.GB18030.NewDecoder()
		out, err := decoder.Bytes(stripGBKBOM(raw))
		if err != nil {
			return nil, enc, fmt.Errorf("decode GBK→UTF-8: %w", err)
		}
		return out, enc, nil
	case EncodingUTF8:
		return stripUTF8BOM(raw), enc, nil
	case EncodingASCII:
		return stripUTF8BOM(raw), enc, nil
	default:
		return stripUTF8BOM(raw), enc, nil
	}
}

func stripUTF8BOM(b []byte) []byte {
	if bytes.HasPrefix(b, utf8BOM) {
		return b[len(utf8BOM):]
	}
	return b
}

func stripGBKBOM(b []byte) []byte {
	if bytes.HasPrefix(b, gbkBOMFull) {
		return b[len(gbkBOMFull):]
	}
	if bytes.HasPrefix(b, gbkBOMHalf) {
		return b[len(gbkBOMHalf):]
	}
	return b
}