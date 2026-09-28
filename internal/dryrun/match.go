package dryrun

import "strings"

// MatchDRGCode returns the DRG/DIP rule whose ICD10Prefix is a leading
// substring of the patient's primary diagnosis code, along with a boolean
// indicating whether any rule matched.
//
// Matching rules:
//   - Patient.PrimaryDiagnosisCode is trimmed of leading whitespace; an
//     empty string yields ("", false) without iterating rules.
//   - An empty rules slice yields ("", false) — there is no implicit
//     "match all" rule.
//   - A Rule with an empty ICD10Prefix is skipped (matches nothing);
//     this guards against malformed fixtures silently swallowing every
//     patient under a "DRG-XXX" rule with no ICD-10 anchor.
//   - When multiple rules match (overlapping prefixes), the FIRST rule
//     in the slice wins. Callers that care about precedence should sort
//     rules from most-specific prefix to least-specific before passing
//     them in.
//
// The function is allocation-free for the common single-match path.
func MatchDRGCode(patient PatientRecord, rules []Rule) (string, bool) {
	diag := strings.TrimSpace(patient.PrimaryDiagnosisCode)
	if diag == "" {
		return "", false
	}
	if len(rules) == 0 {
		return "", false
	}
	for _, r := range rules {
		prefix := strings.TrimSpace(r.ICD10Prefix)
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(diag, prefix) {
			return r.Code, true
		}
	}
	return "", false
}