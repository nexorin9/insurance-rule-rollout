// Tests for the SHA256 hash chain. The fixture is small (5 events) so
// every failure mode is readable: chain head, broken link, payload
// tamper in the middle, hash tamper at the tail, and the happy-path
// round-trip via Export. A benchmark covers the 1000-event append target
// from task.json step 4.
package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example/insurance-rule-rollout/internal/workflow"
)

// fixtureEvents builds the canonical 5-event happy-path fixture used by
// every positive test in this file. EventIDs are deterministic so the
// test can grep for them when asserting export results.
func fixtureEvents() []workflow.AuditEvent {
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	return []workflow.AuditEvent{
		{
			EventID:   "evt-001",
			PlanID:    "plan-aaa",
			Actor:     "yang_jb",
			Action:    workflow.ActionSubmitDraft,
			Payload:   map[string]string{"from": "draft", "to": "pending"},
			Timestamp: base,
		},
		{
			EventID:   "evt-002",
			PlanID:    "plan-aaa",
			Actor:     "wang_xx",
			Action:    workflow.ActionApprove,
			Payload:   map[string]string{"from": "pending", "to": "approved"},
			Timestamp: base.Add(1 * time.Minute),
		},
		{
			EventID:   "evt-003",
			PlanID:    "plan-aaa",
			Actor:     "liu_yy",
			Action:    workflow.ActionRequest,
			Payload:   map[string]string{"from": "approved", "to": "approved", "note": "补医保对照单"},
			Timestamp: base.Add(2 * time.Minute),
		},
		{
			EventID:   "evt-004",
			PlanID:    "plan-aaa",
			Actor:     "wang_xx",
			Action:    workflow.ActionApprove,
			Payload:   map[string]string{"from": "pending", "to": "approved"},
			Timestamp: base.Add(3 * time.Minute),
		},
		{
			EventID:   "evt-005",
			PlanID:    "plan-aaa",
			Actor:     "zhao_zz",
			Action:    workflow.ActionApply,
			Payload:   map[string]string{"from": "approved", "to": "applied"},
			Timestamp: base.Add(4 * time.Minute),
		},
	}
}

// tempChainPath returns a fresh JSONL file path inside t.TempDir(). The
// file is NOT pre-created; Append handles creation lazily.
func tempChainPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "audit-chain.jsonl")
}

// writeChain appends a series of events to a Chain at path and returns
// the populated path. Calls Verify at the end so a buggy chain reaches
// the test failure with a clear "Append broke Verify" message.
func writeChain(t *testing.T, path string, events []workflow.AuditEvent) {
	t.Helper()
	c := NewChain(path)
	for i := range events {
		if err := c.Append(context.Background(), events[i]); err != nil {
			t.Fatalf("append event %d (%s): %v", i+1, events[i].EventID, err)
		}
	}
	if err := c.Verify(context.Background()); err != nil {
		t.Fatalf("verify after appends: %v", err)
	}
}

// readRawLines loads the file verbatim so tamper tests can patch bytes
// without re-running the canonicaliser.
func readRawLines(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var out [][]byte
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		raw := scanner.Bytes()
		buf := make([]byte, len(raw))
		copy(buf, raw)
		out = append(out, buf)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}

// rewriteFile replaces the file at path with the patched bytes.
func rewriteFile(t *testing.T, path string, lines [][]byte) {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("truncate %s: %v", path, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("reopen %s: %v", path, err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := f.Write(line); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		if _, err := f.Write([]byte("\n")); err != nil {
			t.Fatalf("rewrite newline: %v", err)
		}
	}
}

// patchJSONField finds "key":"oldValue" inside raw and replaces it with
// "key":"newValue". Returns the new bytes. Used by tamper tests to flip
// exactly one character of payload without disturbing the surrounding
// JSON structure.
func patchJSONField(t *testing.T, raw []byte, key, oldVal, newVal string) []byte {
	t.Helper()
	old := fmt.Sprintf("%q:%q", key, oldVal)
	new := fmt.Sprintf("%q:%q", key, newVal)
	out := strings.Replace(string(raw), old, new, 1)
	if out == string(raw) {
		t.Fatalf("patchJSONField: could not find %s in line", old)
	}
	return []byte(out)
}

// patchHash finds the first 64-hex-char event_hash inside raw and flips
// the very last hex character to something else (X→Y deterministic). Used
// to simulate "链尾 event_hash 改 1 字符".
func patchHash(t *testing.T, raw []byte) []byte {
	t.Helper()
	s := string(raw)
	idx := strings.LastIndex(s, `"event_hash":"`)
	if idx < 0 {
		t.Fatalf("patchHash: no event_hash field in %s", s)
	}
	start := idx + len(`"event_hash":"`)
	end := start + 64
	if end > len(s) {
		t.Fatalf("patchHash: event_hash shorter than 64 chars")
	}
	oldChar := s[end-1]
	newChar := byte('0')
	if oldChar == '0' {
		newChar = '1'
	}
	return []byte(s[:end-1] + string(newChar) + s[end:])
}

// =========================================================================
//  Happy-path
// =========================================================================

func TestChain_HappyPath_AppendFiveEventsVerify(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())

	c := NewChain(path)
	if err := c.Verify(context.Background()); err != nil {
		t.Fatalf("verify happy path: %v", err)
	}
}

func TestChain_HappyPath_PrevHashChain(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())

	lines := readRawLines(t, path)
	if len(lines) != 5 {
		t.Fatalf("want 5 lines, got %d", len(lines))
	}

	// First line's prev_hash must be GenesisHash.
	var first workflow.AuditEvent
	if err := json.Unmarshal(lines[0], &first); err != nil {
		t.Fatalf("parse line 0: %v", err)
	}
	if first.PrevHash != GenesisHash {
		t.Fatalf("line 0 prev_hash = %q, want %q", first.PrevHash, GenesisHash)
	}

	// Each subsequent line's prev_hash equals the previous line's event_hash.
	for i := 1; i < len(lines); i++ {
		var prev, cur workflow.AuditEvent
		if err := json.Unmarshal(lines[i-1], &prev); err != nil {
			t.Fatalf("parse line %d: %v", i-1, err)
		}
		if err := json.Unmarshal(lines[i], &cur); err != nil {
			t.Fatalf("parse line %d: %v", i, err)
		}
		if cur.PrevHash != prev.EventHash {
			t.Fatalf("line %d prev_hash = %q, want %q", i, cur.PrevHash, prev.EventHash)
		}
	}
}

func TestChain_HappyPath_AppendAutoFillsPrevHash(t *testing.T) {
	path := tempChainPath(t)
	c := NewChain(path)
	events := fixtureEvents()

	// Caller does NOT set PrevHash. Append should auto-fill.
	if err := c.Append(context.Background(), events[0]); err != nil {
		t.Fatalf("append #1: %v", err)
	}
	if err := c.Append(context.Background(), events[1]); err != nil {
		t.Fatalf("append #2: %v", err)
	}

	lines := readRawLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d", len(lines))
	}
	var l0, l1 workflow.AuditEvent
	if err := json.Unmarshal(lines[0], &l0); err != nil {
		t.Fatalf("parse l0: %v", err)
	}
	if err := json.Unmarshal(lines[1], &l1); err != nil {
		t.Fatalf("parse l1: %v", err)
	}
	if l0.PrevHash != GenesisHash {
		t.Fatalf("l0 prev_hash = %q, want %q", l0.PrevHash, GenesisHash)
	}
	if l1.PrevHash != l0.EventHash {
		t.Fatalf("l1 prev_hash = %q, want %q (l0.event_hash)", l1.PrevHash, l0.EventHash)
	}
}

func TestChain_AppendRejectsEmptyEventID(t *testing.T) {
	path := tempChainPath(t)
	c := NewChain(path)
	err := c.Append(context.Background(), workflow.AuditEvent{
		PlanID:    "plan-x",
		Actor:     "yang_jb",
		Action:    workflow.ActionApprove,
		Timestamp: time.Now(),
	})
	if err == nil {
		t.Fatalf("Append should reject empty event_id")
	}
	if !strings.Contains(err.Error(), "event_id") {
		t.Fatalf("error message should mention event_id, got: %v", err)
	}
}

func TestChain_AppendRejectsZeroTimestamp(t *testing.T) {
	path := tempChainPath(t)
	c := NewChain(path)
	err := c.Append(context.Background(), workflow.AuditEvent{
		EventID: "evt-1",
		PlanID:  "plan-x",
		Actor:   "yang_jb",
		Action:  workflow.ActionApprove,
	})
	if err == nil {
		t.Fatalf("Append should reject zero timestamp")
	}
	if !strings.Contains(err.Error(), "timestamp") {
		t.Fatalf("error message should mention timestamp, got: %v", err)
	}
}

// =========================================================================
//  Verify — tamper detection (header / broken / middle / tail)
// =========================================================================

func TestChain_Verify_DetectsHeaderTamper(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())

	lines := readRawLines(t, path)
	// Flip the first line's prev_hash from genesis to some other value.
	lines[0] = patchJSONField(t, lines[0], "prev_hash", GenesisHash,
		"1111111111111111111111111111111111111111111111111111111111111111")
	rewriteFile(t, path, lines)

	c := NewChain(path)
	err := c.Verify(context.Background())
	var cbe *ChainBrokenError
	if !errors.As(err, &cbe) {
		t.Fatalf("expected *ChainBrokenError, got %T: %v", err, err)
	}
	if cbe.TamperType != TamperHeader {
		t.Fatalf("TamperType = %q, want %q", cbe.TamperType, TamperHeader)
	}
	if cbe.Line != 1 {
		t.Fatalf("Line = %d, want 1", cbe.Line)
	}
	if cbe.ExpectedHash != GenesisHash {
		t.Fatalf("ExpectedHash = %q, want %q", cbe.ExpectedHash, GenesisHash)
	}
}

func TestChain_Verify_DetectsBrokenLink_MiddleDeletion(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())

	lines := readRawLines(t, path)
	// Drop the middle line; line 4's prev_hash now points to line 2's hash,
	// not line 3's. This is the "中间删除 1 行" attack.
	lines = append(lines[:2], lines[3:]...)
	rewriteFile(t, path, lines)

	c := NewChain(path)
	err := c.Verify(context.Background())
	var cbe *ChainBrokenError
	if !errors.As(err, &cbe) {
		t.Fatalf("expected *ChainBrokenError, got %T: %v", err, err)
	}
	if cbe.TamperType != TamperBroken {
		t.Fatalf("TamperType = %q, want %q", cbe.TamperType, TamperBroken)
	}
	if cbe.Line != 3 {
		t.Fatalf("Line = %d, want 3 (the new line 3 after deletion)", cbe.Line)
	}
}

func TestChain_Verify_DetectsMiddleTamper_PayloadCharFlip(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())

	lines := readRawLines(t, path)
	// Flip one character in the middle event's payload "from" field.
	// "approved" → "approveD" (1 char swap; recomputed hash will differ).
	lines[2] = patchJSONField(t, lines[2], "from", "approved", "approveD")
	rewriteFile(t, path, lines)

	c := NewChain(path)
	err := c.Verify(context.Background())
	var cbe *ChainBrokenError
	if !errors.As(err, &cbe) {
		t.Fatalf("expected *ChainBrokenError, got %T: %v", err, err)
	}
	if cbe.TamperType != TamperMiddle {
		t.Fatalf("TamperType = %q, want %q", cbe.TamperType, TamperMiddle)
	}
	if cbe.Line != 3 {
		t.Fatalf("Line = %d, want 3 (the patched middle event)", cbe.Line)
	}
	if cbe.ExpectedHash == cbe.ActualHash {
		t.Fatalf("expected hashes to differ: %q", cbe.ExpectedHash)
	}
}

func TestChain_Verify_DetectsTailTamper_HashCharFlip(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())

	lines := readRawLines(t, path)
	// Flip the very last hex character of the last event's event_hash.
	lines[4] = patchHash(t, lines[4])
	rewriteFile(t, path, lines)

	c := NewChain(path)
	err := c.Verify(context.Background())
	var cbe *ChainBrokenError
	if !errors.As(err, &cbe) {
		t.Fatalf("expected *ChainBrokenError, got %T: %v", err, err)
	}
	if cbe.TamperType != TamperTail {
		t.Fatalf("TamperType = %q, want %q", cbe.TamperType, TamperTail)
	}
	if cbe.Line != 5 {
		t.Fatalf("Line = %d, want 5 (the tail)", cbe.Line)
	}
}

// =========================================================================
//  Verify — happy paths on empty / missing files
// =========================================================================

func TestChain_Verify_EmptyFileIsValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatalf("create empty file: %v", err)
	}
	c := NewChain(path)
	if err := c.Verify(context.Background()); err != nil {
		t.Fatalf("empty file should verify clean: %v", err)
	}
}

func TestChain_Verify_MissingFileIsValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.jsonl")
	c := NewChain(path)
	if err := c.Verify(context.Background()); err != nil {
		t.Fatalf("missing file should verify clean: %v", err)
	}
}

func TestChain_Verify_BlankLinesIgnored(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())

	// Re-read + rewrite with blank lines interleaved.
	raw := readRawLines(t, path)
	blanks := [][]byte{[]byte(""), []byte("   "), []byte("\t")}
	var mixed [][]byte
	for i, line := range raw {
		mixed = append(mixed, line)
		if i < len(raw)-1 {
			mixed = append(mixed, blanks[i%len(blanks)])
		}
	}
	rewriteFile(t, path, mixed)

	c := NewChain(path)
	if err := c.Verify(context.Background()); err != nil {
		t.Fatalf("verify with blank lines: %v", err)
	}
}

// =========================================================================
//  Determinism — same event → same hash (no map iteration randomness)
// =========================================================================

func TestHashEvent_DeterministicAcrossMapOrdering(t *testing.T) {
	ev1 := workflow.AuditEvent{
		EventID:   "evt-x",
		PlanID:    "plan-1",
		Actor:     "yang_jb",
		Action:    workflow.ActionApprove,
		Payload:   map[string]string{"a": "1", "b": "2", "c": "3"},
		Timestamp: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
		PrevHash:  GenesisHash,
	}
	ev2 := workflow.AuditEvent{
		EventID:   "evt-x",
		PlanID:    "plan-1",
		Actor:     "yang_jb",
		Action:    workflow.ActionApprove,
		Payload:   map[string]string{"c": "3", "a": "1", "b": "2"}, // different insertion order
		Timestamp: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC),
		PrevHash:  GenesisHash,
	}
	if HashEvent(&ev1) != HashEvent(&ev2) {
		t.Fatalf("HashEvent must be deterministic across map iteration order")
	}
}

// =========================================================================
//  Export — happy path + filter combinations + edge cases
// =========================================================================

func TestExport_FilterByPlanID(t *testing.T) {
	path := tempChainPath(t)
	events := fixtureEvents()
	// Inject one event from a different plan_id so the filter has work.
	events = append(events, workflow.AuditEvent{
		EventID:   "evt-006",
		PlanID:    "plan-bbb",
		Actor:     "yang_jb",
		Action:    workflow.ActionApprove,
		Payload:   map[string]string{"note": "另一计划"},
		Timestamp: fixtureEvents()[4].Timestamp.Add(1 * time.Minute),
	})
	writeChain(t, path, events)

	c := NewChain(path)
	r, err := c.Export(context.Background(), Filter{PlanID: "plan-aaa"})
	if err != nil {
		t.Fatalf("export plan-aaa: %v", err)
	}
	defer drainReader(r)

	var exported []workflow.AuditEvent
	for _, raw := range readJSONL(t, r) {
		var ev workflow.AuditEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("parse exported: %v", err)
		}
		exported = append(exported, ev)
	}
	if len(exported) != 5 {
		t.Fatalf("filter by plan-aaa: want 5 events, got %d", len(exported))
	}
	for _, ev := range exported {
		if ev.PlanID != "plan-aaa" {
			t.Fatalf("exported event has wrong plan_id: %s", ev.PlanID)
		}
	}
}

func TestExport_FilterByActor(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())

	c := NewChain(path)
	r, err := c.Export(context.Background(), Filter{Actor: "wang_xx"})
	if err != nil {
		t.Fatalf("export actor: %v", err)
	}
	defer drainReader(r)

	exported := readJSONL(t, r)
	if len(exported) != 2 {
		t.Fatalf("filter by wang_xx: want 2 events, got %d", len(exported))
	}
	for _, raw := range exported {
		var ev workflow.AuditEvent
		_ = json.Unmarshal(raw, &ev)
		if ev.Actor != "wang_xx" {
			t.Fatalf("wrong actor: %s", ev.Actor)
		}
	}
}

func TestExport_FilterByAction(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())

	c := NewChain(path)
	r, err := c.Export(context.Background(), Filter{Action: workflow.ActionApprove})
	if err != nil {
		t.Fatalf("export action: %v", err)
	}
	defer drainReader(r)

	exported := readJSONL(t, r)
	if len(exported) != 2 {
		t.Fatalf("filter by ActionApprove: want 2, got %d", len(exported))
	}
}

func TestExport_FilterByTimeRange(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())
	base := fixtureEvents()[0].Timestamp

	c := NewChain(path)
	r, err := c.Export(context.Background(), Filter{
		FromTime: base.Add(1 * time.Minute), // inclusive lower bound
		ToTime:   base.Add(3 * time.Minute), // exclusive upper bound
	})
	if err != nil {
		t.Fatalf("export time range: %v", err)
	}
	defer drainReader(r)

	exported := readJSONL(t, r)
	if len(exported) != 2 {
		t.Fatalf("time-range filter: want 2 events, got %d", len(exported))
	}
}

func TestExport_LimitAndOffset(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())

	c := NewChain(path)
	r, err := c.Export(context.Background(), Filter{Offset: 1, Limit: 2})
	if err != nil {
		t.Fatalf("export limit/offset: %v", err)
	}
	defer drainReader(r)

	exported := readJSONL(t, r)
	if len(exported) != 2 {
		t.Fatalf("limit/offset: want 2 events, got %d", len(exported))
	}
	// Offset 1 = skip evt-001 → first emitted should be evt-002.
	var first workflow.AuditEvent
	_ = json.Unmarshal(exported[0], &first)
	if first.EventID != "evt-002" {
		t.Fatalf("offset 1 first emitted = %s, want evt-002", first.EventID)
	}
}

func TestExport_MissingFileReturnsEmptyReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such.jsonl")
	c := NewChain(path)
	r, err := c.Export(context.Background(), Filter{})
	if err != nil {
		t.Fatalf("export missing file: %v", err)
	}
	defer drainReader(r)
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("missing file should yield empty bytes, got %d bytes", len(data))
	}
}

func TestExport_NoFilterReturnsAllEvents(t *testing.T) {
	path := tempChainPath(t)
	writeChain(t, path, fixtureEvents())

	c := NewChain(path)
	r, err := c.Export(context.Background(), Filter{})
	if err != nil {
		t.Fatalf("export no filter: %v", err)
	}
	defer drainReader(r)

	exported := readJSONL(t, r)
	if len(exported) != 5 {
		t.Fatalf("no-filter export: want 5 events, got %d", len(exported))
	}
}

// =========================================================================
//  Performance — 1000 appends in well under 1s, 1000-event verify fast
// =========================================================================

func TestChain_Performance_ThousandAppends(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping performance test in -short mode")
	}
	path := tempChainPath(t)
	c := NewChain(path)
	base := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

	start := time.Now()
	for i := 0; i < 1000; i++ {
		ev := workflow.AuditEvent{
			EventID:   fmt.Sprintf("perf-evt-%04d", i),
			PlanID:    "plan-perf",
			Actor:     "loadgen",
			Action:    workflow.ActionApprove,
			Payload:   map[string]string{"i": fmt.Sprintf("%d", i), "note": "performance smoke"},
			Timestamp: base.Add(time.Duration(i) * time.Second),
		}
		if err := c.Append(context.Background(), ev); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 1*time.Second {
		t.Fatalf("1000 appends took %v, want < 1s", elapsed)
	}
	t.Logf("1000 appends took %v (%.2f µs/op)", elapsed, float64(elapsed.Microseconds())/1000.0)

	// Verify the whole chain too — must finish under 1s.
	start = time.Now()
	if err := c.Verify(context.Background()); err != nil {
		t.Fatalf("verify after perf appends: %v", err)
	}
	verifyElapsed := time.Since(start)
	if verifyElapsed > 1*time.Second {
		t.Fatalf("verify of 1000 events took %v, want < 1s", verifyElapsed)
	}
	t.Logf("verify 1000 events took %v", verifyElapsed)

	// Export performance: 1000 events streamed < 500ms.
	start = time.Now()
	r, err := c.Export(context.Background(), Filter{})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	n, err := io.Copy(io.Discard, r)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	exportElapsed := time.Since(start)
	if exportElapsed > 500*time.Millisecond {
		t.Fatalf("export 1000 events took %v, want < 500ms", exportElapsed)
	}
	t.Logf("export 1000 events (%d bytes) took %v", n, exportElapsed)
}

// =========================================================================
//  helpers
// =========================================================================

// readJSONL pulls JSON-decoded byte slices out of an io.Reader. Empty
// lines are skipped silently.
func readJSONL(t *testing.T, r io.Reader) [][]byte {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}
	if len(data) == 0 {
		return nil
	}
	var out [][]byte
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, []byte(line))
	}
	return out
}

// drainReader reads to EOF so the goroutine inside Export can exit and
// the underlying file is closed promptly.
func drainReader(r io.Reader) {
	_, _ = io.Copy(io.Discard, r)
}