// Dedicated table-driven tests for the SHA256 chain tamper-detection
// surface. The "happy path" tests live in chain_test.go; this file
// focuses on Chain.Verify's error contract so the 飞检 auditor can
// reliably locate and categorise any tampering attempt.
//
// The four tamper categories correspond to the four observable attack
// surfaces (header / broken / middle / tail). Every test asserts:
//
//   - errors.As reaches *ChainBrokenError (typed error contract);
//   - TamperType matches the category;
//   - Line points to the offending row;
//   - ExpectedHash / ActualHash are populated;
//   - Error() message contains line, event_id, type and both hashes.
//
// Source mapping: bytebase backend/component/review/workflow.go uses
// typed errors with rich fields for 飞检 debugging; the same shape is
// preserved here so any future chain-tamper recovery tooling can reuse
// the same field set.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/example/insurance-rule-rollout/internal/workflow"
)

// tamperCase describes one row in the table-driven tamper-detection
// suite. Patch is given the pristine chain contents; it must return the
// patched bytes (which are written to disk verbatim) and the
// caller-supplied metadata used by Verify to categorise the breakage.
type tamperCase struct {
	name             string
	patch            func(t *testing.T, lines [][]byte) (patched [][]byte, expectedLine int, expectedEventID string)
	expectedTamper   ChainBrokenType
	hashShouldDiffer bool
}

// tamperCases lists every tamper-detection scenario the 飞检 inspector
// must be able to categorise. Each entry is self-contained: the patch
// function rewrites exactly what an attacker would change, and the
// metadata rows give Verify() everything it needs to report.
func tamperCases() []tamperCase {
	headerNewPrevHash := strings.Repeat("1", 64)
	headerNewPrevHashBig := strings.Repeat("f", 64)

	return []tamperCase{
		{
			name: "header/prev_hash_replaced_with_all_ones",
			patch: func(t *testing.T, lines [][]byte) ([][]byte, int, string) {
				lines[0] = patchJSONField(t, lines[0], "prev_hash", GenesisHash, headerNewPrevHash)
				return lines, 1, "evt-001"
			},
			expectedTamper:   TamperHeader,
			hashShouldDiffer: true,
		},
		{
			name: "header/prev_hash_replaced_with_high_hex",
			patch: func(t *testing.T, lines [][]byte) ([][]byte, int, string) {
				lines[0] = patchJSONField(t, lines[0], "prev_hash", GenesisHash, headerNewPrevHashBig)
				return lines, 1, "evt-001"
			},
			expectedTamper:   TamperHeader,
			hashShouldDiffer: true,
		},
		{
			name: "broken/middle_line_deleted",
			patch: func(t *testing.T, lines [][]byte) ([][]byte, int, string) {
				// Drop line 3 (zero-indexed 2). The new line 3's prev_hash
				// no longer points back to the new line 2's event_hash.
				lines = append(lines[:2], lines[3:]...)
				return lines, 3, "evt-004"
			},
			expectedTamper:   TamperBroken,
			hashShouldDiffer: false,
		},
		{
			name: "header/first_line_deleted_surfaces_as_header_tamper",
			patch: func(t *testing.T, lines [][]byte) ([][]byte, int, string) {
				// Drop the genesis row. The new line 1's prev_hash was
				// the genesis-row's event_hash, which is non-zero, so
				// Verify surfaces this as a header tamper (first line's
				// prev_hash != GenesisHash) rather than a broken link.
				lines = lines[1:]
				return lines, 1, "evt-002"
			},
			expectedTamper:   TamperHeader,
			hashShouldDiffer: false,
		},
		{
			name: "middle/payload_from_char_flipped",
			patch: func(t *testing.T, lines [][]byte) ([][]byte, int, string) {
				lines[2] = patchJSONField(t, lines[2], "from", "approved", "approveD")
				return lines, 3, "evt-003"
			},
			expectedTamper:   TamperMiddle,
			hashShouldDiffer: true,
		},
		{
			name: "middle/payload_note_field_overwritten",
			patch: func(t *testing.T, lines [][]byte) ([][]byte, int, string) {
				lines[2] = patchJSONField(t, lines[2], "note", "补医保对照单", "已删除对照单")
				return lines, 3, "evt-003"
			},
			expectedTamper:   TamperMiddle,
			hashShouldDiffer: true,
		},
		{
			name: "middle/actor_field_swapped",
			patch: func(t *testing.T, lines [][]byte) ([][]byte, int, string) {
				lines[2] = patchJSONField(t, lines[2], "actor", "liu_yy", "evil_actor")
				return lines, 3, "evt-003"
			},
			expectedTamper:   TamperMiddle,
			hashShouldDiffer: true,
		},
		{
			name: "middle/event_id_swapped",
			patch: func(t *testing.T, lines [][]byte) ([][]byte, int, string) {
				lines[2] = patchJSONField(t, lines[2], "event_id", "evt-003", "evt-XXX")
				return lines, 3, "evt-XXX"
			},
			expectedTamper:   TamperMiddle,
			hashShouldDiffer: true,
		},
		{
			name: "tail/event_hash_last_char_flipped",
			patch: func(t *testing.T, lines [][]byte) ([][]byte, int, string) {
				lines[4] = patchHash(t, lines[4])
				return lines, 5, "evt-005"
			},
			expectedTamper:   TamperTail,
			hashShouldDiffer: true,
		},
		{
			name: "tail/payload_changed_on_last_line",
			patch: func(t *testing.T, lines [][]byte) ([][]byte, int, string) {
				lines[4] = patchJSONField(t, lines[4], "from", "approved", "approveD")
				return lines, 5, "evt-005"
			},
			expectedTamper:   TamperTail,
			hashShouldDiffer: true,
		},
	}
}

// runTamperCase is the common body shared by the table-driven test and
// the dedicated single-purpose tests. It writes the fixture chain,
// applies the patch, runs Verify, then walks every contract assertion
// (typed error, TamperType, Line, hash fields, Error() content).
func runTamperCase(t *testing.T, tc tamperCase) {
	t.Helper()
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())

	lines := readRawLines(t, path)
	patched, expectedLine, expectedEventID := tc.patch(t, lines)
	rewriteFile(t, path, patched)

	c := NewChain(path)
	err := c.Verify(context.Background())
	if err == nil {
		t.Fatalf("Verify should detect tamper for %q, got nil", tc.name)
	}

	var cbe *ChainBrokenError
	if !errors.As(err, &cbe) {
		t.Fatalf("expected *ChainBrokenError, got %T: %v", err, err)
	}

	if cbe.TamperType != tc.expectedTamper {
		t.Fatalf("TamperType = %q, want %q (case=%s)", cbe.TamperType, tc.expectedTamper, tc.name)
	}
	if cbe.Line != expectedLine {
		t.Fatalf("Line = %d, want %d (case=%s)", cbe.Line, expectedLine, tc.name)
	}
	if cbe.EventID != expectedEventID {
		t.Fatalf("EventID = %q, want %q (case=%s)", cbe.EventID, expectedEventID, tc.name)
	}
	if cbe.ExpectedHash == "" {
		t.Fatalf("ExpectedHash empty (case=%s)", tc.name)
	}
	if cbe.ActualHash == "" {
		t.Fatalf("ActualHash empty (case=%s)", tc.name)
	}
	if tc.hashShouldDiffer && cbe.ExpectedHash == cbe.ActualHash {
		t.Fatalf("ExpectedHash == ActualHash but tamper should differ (case=%s): %q", tc.name, cbe.ExpectedHash)
	}

	// Error() must contain line, event_id, tamper type, both hashes —
	// 飞检 grep depends on this contract.
	msg := cbe.Error()
	for _, want := range []string{
		fmt.Sprintf("line=%d", expectedLine),
		fmt.Sprintf("event_id=%s", expectedEventID),
		fmt.Sprintf("type=%s", tc.expectedTamper),
		cbe.ExpectedHash,
		cbe.ActualHash,
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Error() = %q, missing %q (case=%s)", msg, want, tc.name)
		}
	}
	if !strings.HasPrefix(msg, "audit chain broken") {
		t.Fatalf("Error() should start with 'audit chain broken', got: %q", msg)
	}

	// errors.Is short-circuit (mirrors ChainBrokenError.Is).
	var probe *ChainBrokenError
	if !errors.Is(err, probe) {
		t.Fatalf("errors.Is should accept *ChainBrokenError probe (case=%s)", tc.name)
	}
}

// TestChain_Tamper_TableDriven walks every tamper category in one shot.
// New attack surfaces must be appended to tamperCases() — the body
// stays untouched so review burden is "look at one slice".
func TestChain_Tamper_TableDriven(t *testing.T) {
	cases := tamperCases()
	if len(cases) < 4 {
		t.Fatalf("tamperCases should cover >= 4 categories, got %d", len(cases))
	}
	// Sanity check: each ChainBrokenType is represented at least once.
	seen := map[ChainBrokenType]bool{}
	for _, c := range cases {
		seen[c.expectedTamper] = true
	}
	for _, want := range []ChainBrokenType{TamperHeader, TamperBroken, TamperMiddle, TamperTail} {
		if !seen[want] {
			t.Fatalf("tamperCases missing category %q", want)
		}
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			runTamperCase(t, tc)
		})
	}
}

// Dedicated single-purpose tests below mirror the four canonical
// categories from the task spec so the 飞检 contract is locked
// even if a future refactor drops the table-driven runner.
// (Names carry the `_Dedicated` suffix to avoid colliding with the
// single-purpose tests in chain_test.go.)

// TestChain_Verify_DetectsHeaderTamper_Dedicated verifies that a
// non-genesis prev_hash on the very first line is flagged with
// TamperHeader and points at line 1.
func TestChain_Verify_DetectsHeaderTamper_Dedicated(t *testing.T) {
	runTamperCase(t, tamperCase{
		name: "header/canonical",
		patch: func(t *testing.T, lines [][]byte) ([][]byte, int, string) {
			lines[0] = patchJSONField(t, lines[0], "prev_hash", GenesisHash,
				strings.Repeat("1", 64))
			return lines, 1, "evt-001"
		},
		expectedTamper:   TamperHeader,
		hashShouldDiffer: true,
	})
}

// TestChain_Verify_DetectsMiddleLineDeletion_Dedicated verifies that
// deleting a middle event causes a TamperBroken at the first line
// whose prev_hash no longer points back to its predecessor.
func TestChain_Verify_DetectsMiddleLineDeletion_Dedicated(t *testing.T) {
	runTamperCase(t, tamperCase{
		name: "broken/canonical",
		patch: func(t *testing.T, lines [][]byte) ([][]byte, int, string) {
			lines = append(lines[:2], lines[3:]...)
			return lines, 3, "evt-004"
		},
		expectedTamper:   TamperBroken,
		hashShouldDiffer: false,
	})
}

// TestChain_Verify_DetectsMiddlePayloadTamper_Dedicated verifies that
// flipping a single byte of payload in the middle of the chain
// surfaces as TamperMiddle.
func TestChain_Verify_DetectsMiddlePayloadTamper_Dedicated(t *testing.T) {
	runTamperCase(t, tamperCase{
		name: "middle/canonical",
		patch: func(t *testing.T, lines [][]byte) ([][]byte, int, string) {
			lines[2] = patchJSONField(t, lines[2], "from", "approved", "approveD")
			return lines, 3, "evt-003"
		},
		expectedTamper:   TamperMiddle,
		hashShouldDiffer: true,
	})
}

// TestChain_Verify_DetectsTailEventHashTamper_Dedicated verifies that
// flipping the very last hex character of the last line's event_hash
// surfaces as TamperTail (not TamperMiddle) so the auditor can
// distinguish "tail of chain" from "any line in the middle".
func TestChain_Verify_DetectsTailEventHashTamper_Dedicated(t *testing.T) {
	runTamperCase(t, tamperCase{
		name: "tail/canonical",
		patch: func(t *testing.T, lines [][]byte) ([][]byte, int, string) {
			lines[4] = patchHash(t, lines[4])
			return lines, 5, "evt-005"
		},
		expectedTamper:   TamperTail,
		hashShouldDiffer: true,
	})
}

// TestChain_Verify_DetectsMultipleTamperLocations walks every row
// individually so each chain position (1st / 2nd / 3rd / 4th / 5th)
// is exercised at least once with a payload-only mutation. The first
// row is excluded because it would surface as a header rewrite (its
// prev_hash is GenesisHash); this test focuses on middle/tail rows.
func TestChain_Verify_DetectsMultipleTamperLocations(t *testing.T) {
	positions := []struct {
		line    int
		eventID string
		from    string
	}{
		{1, "evt-002", "pending"},
		{2, "evt-003", "approved"},
		{3, "evt-004", "pending"},
		{4, "evt-005", "approved"},
	}
	for _, p := range positions {
		p := p
		t.Run(fmt.Sprintf("line_%d", p.line+1), func(t *testing.T) {
			path := tempChainPath(t)
			writeChain(t, path, fixtureEvents())

			lines := readRawLines(t, path)
			// Flip the last char of "from" deterministically.
			old := p.from
			last := old[len(old)-1]
			flipped := string(flipChar(last))
			new := old[:len(old)-1] + flipped
			lines[p.line] = patchJSONField(t, lines[p.line], "from", old, new)
			rewriteFile(t, path, lines)

			c := NewChain(path)
			err := c.Verify(context.Background())
			var cbe *ChainBrokenError
			if !errors.As(err, &cbe) {
				t.Fatalf("line %d: expected *ChainBrokenError, got %T: %v", p.line+1, err, err)
			}
			want := TamperMiddle
			if p.line == 4 {
				want = TamperTail
			}
			if cbe.TamperType != want {
				t.Fatalf("line %d: TamperType = %q, want %q", p.line+1, cbe.TamperType, want)
			}
			if cbe.Line != p.line+1 {
				t.Fatalf("line %d: Line = %d, want %d", p.line+1, cbe.Line, p.line+1)
			}
			if cbe.EventID != p.eventID {
				t.Fatalf("line %d: EventID = %q, want %q", p.line+1, cbe.EventID, p.eventID)
			}
			if cbe.ExpectedHash == cbe.ActualHash {
				t.Fatalf("line %d: hashes should differ", p.line+1)
			}
		})
	}
}

// flipChar deterministically returns a different ASCII character so a
// payload patch test always mutates exactly one byte.
func flipChar(b byte) byte {
	if b == 'a' {
		return 'b'
	}
	if b == 'd' {
		return 'e'
	}
	if b == 'g' {
		return 'h'
	}
	return byte('Z')
}

// TestChain_Verify_LocatesFirstFailure walks a chain that has been
// tampered at two distinct locations. Verify must short-circuit at the
// first inconsistency and report that one — the second tamper stays
// hidden behind the first failure. This documents the on-disk scan
// order so 飞检 doesn't depend on detecting every tamper in a single
// pass.
func TestChain_Verify_LocatesFirstFailure(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())

	lines := readRawLines(t, path)
	// Tamper 1: middle line (line 3) payload flip.
	lines[2] = patchJSONField(t, lines[2], "from", "approved", "approveD")
	// Tamper 2: tail line (line 5) event_hash flip.
	lines[4] = patchHash(t, lines[4])
	rewriteFile(t, path, lines)

	c := NewChain(path)
	err := c.Verify(context.Background())
	var cbe *ChainBrokenError
	if !errors.As(err, &cbe) {
		t.Fatalf("expected *ChainBrokenError, got %T: %v", err, err)
	}
	if cbe.Line != 3 {
		t.Fatalf("Verify should short-circuit at first tamper; Line = %d, want 3", cbe.Line)
	}
	if cbe.TamperType != TamperMiddle {
		t.Fatalf("first tamper is middle, TamperType = %q", cbe.TamperType)
	}
}

// TestChainBrokenError_Is sanity-checks the typed-error short-circuit
// so callers can rely on `errors.Is(err, &ChainBrokenError{})`.
func TestChainBrokenError_Is(t *testing.T) {
	var base *ChainBrokenError
	other := fmt.Errorf("plain error")
	if errors.Is(other, base) {
		t.Fatalf("errors.Is should reject non-ChainBrokenError")
	}
	wrapped := fmt.Errorf("wrap: %w", &ChainBrokenError{TamperType: TamperHeader})
	if !errors.Is(wrapped, base) {
		t.Fatalf("errors.Is should accept wrapped ChainBrokenError")
	}
}

// TestChainBrokenError_ErrorFormat locks the user-facing message
// contract — 飞检 scripts grep for "line=N" / "event_id=X" / "type=Y"
// / 64-hex hashes. Any reformat that breaks those substrings will fail
// this test before the auditor's tooling does.
func TestChainBrokenError_ErrorFormat(t *testing.T) {
	e := &ChainBrokenError{
		EventID:      "evt-deadbeef",
		Line:         42,
		ExpectedHash: strings.Repeat("a", 64),
		ActualHash:   strings.Repeat("b", 64),
		TamperType:   TamperMiddle,
	}
	msg := e.Error()
	wantSubstrings := []string{
		"audit chain broken",
		"line=42",
		"event_id=evt-deadbeef",
		"type=middle",
		strings.Repeat("a", 64),
		strings.Repeat("b", 64),
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(msg, s) {
			t.Fatalf("Error() missing %q in %q", s, msg)
		}
	}
}

// TestChain_Verify_PreservesUnaffectedLines confirms that a tamper
// surfaces a failure on the offending row only, and the rows after it
// are not silently accepted (they would have stale prev_hash links).
// This is more of a regression check than a contract test: if a future
// refactor "fixes" Verify by skipping the broken line, this test will
// surface it.
func TestChain_Verify_PreservesUnaffectedLines(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())

	lines := readRawLines(t, path)
	// Insert a syntactically valid but unverifiable line between row 2
	// and row 4 — this guarantees the inserted row's prev_hash != row
	// 2's event_hash AND its event_hash != HashEvent(canonical).
	var inserted workflow.AuditEvent
	{
		base := fixtureEvents()[0].Timestamp
		inserted = workflow.AuditEvent{
			EventID:   "evt-INJ",
			PlanID:    "plan-aaa",
			Actor:     "yang_jb",
			Action:    workflow.ActionApprove,
			Payload:   map[string]string{"from": "pending", "to": "approved"},
			Timestamp: base.Add(2*time.Minute + 30*time.Second),
			PrevHash:  strings.Repeat("9", 64),
			EventHash: strings.Repeat("9", 64),
		}
		insertedBytes, err := json.Marshal(inserted)
		if err != nil {
			t.Fatalf("marshal insert: %v", err)
		}
		lines = append(lines[:3], append([][]byte{insertedBytes}, lines[3:]...)...)
	}
	rewriteFile(t, path, lines)

	c := NewChain(path)
	err := c.Verify(context.Background())
	var cbe *ChainBrokenError
	if !errors.As(err, &cbe) {
		t.Fatalf("expected *ChainBrokenError for injected row, got %T: %v", err, err)
	}
	// Insertion point: the new line 4's prev_hash is the injected hash
	// (TamperBroken). The injected hash is also non-canonical, but
	// Verify reports the first failure it encounters — a broken link
	// surfaces before a hash mismatch.
	if cbe.Line != 4 {
		t.Fatalf("Line = %d, want 4 (insertion point)", cbe.Line)
	}
	if cbe.TamperType != TamperBroken {
		t.Fatalf("TamperType = %q, want %q", cbe.TamperType, TamperBroken)
	}
}