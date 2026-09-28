// Package audit implements an append-only SHA256 hash chain on top of
// workflow.AuditEvent rows. The chain is persisted as JSONL (one event per
// line) so the 飞检 inspector can self-verify the file with sha256sum
// after export.
//
// Source mapping: bytebase backend/generated-go/v1/v1connect/audit_log_service
// .connect.go's ExportAuditLogs endpoint. The bytebase side is Connect-RPC;
// here we drop the wire protocol and use local functions + a JSONL file so
// the chain survives a process restart and can be archived verbatim.
//
// Chain layout:
//
//	line 1:  { event_id, plan_id, actor, action, payload, timestamp,
//	          prev_hash = "0000...0000" (64 hex zero bytes),
//	          event_hash = sha256(canonical_form) }
//	line 2+: prev_hash = previous line's event_hash
//
// Canonical form fed into SHA256 is deterministic: payload keys sorted
// alphabetically, timestamp forced to UTC RFC3339Nano, fields joined with
// '\n' so a single JSONL line is also the canonical input. Tampering with
// any byte of any line — payload, actor, timestamp, even the chain link
// itself — changes the recomputed hash and is flagged by Verify.
package audit

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/example/insurance-rule-rollout/internal/workflow"
)

// GenesisHash is the prev_hash value stamped on the very first event of a
// new chain. 64 hex zero bytes (= 32 raw bytes of 0x00).
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// ChainBrokenType categorises *where* in the chain a tamper was detected.
// Three flavours, mirroring the three observable attack surfaces:
//
//   - HeaderTamper: the genesis link was rewritten (first event's prev_hash
//     no longer equals GenesisHash).
//   - BrokenLink:   a line in the body lost its prev_hash link to the line
//     immediately before it (covers "中间删除 1 行").
//   - MiddleTamper: a recomputed event_hash doesn't match the stored value
//     (covers "中间 payload 改 1 字符").
//   - TailTamper:   the same as MiddleTamper, but on the very last line
//     (covers "链尾 event_hash 改 1 字符").
type ChainBrokenType string

const (
	TamperHeader ChainBrokenType = "header"
	TamperBroken ChainBrokenType = "broken"
	TamperMiddle ChainBrokenType = "middle"
	TamperTail   ChainBrokenType = "tail"
)

// ChainBrokenError is the typed error returned by Chain.Verify when the
// SHA256 chain fails its self-check. EventID / Line let the 飞检 inspector
// jump straight to the offending row; ExpectedHash / ActualHash are kept
// verbatim so the auditor can grep the JSONL file for the discrepancy.
type ChainBrokenError struct {
	EventID      string
	Line         int
	ExpectedHash string
	ActualHash   string
	TamperType   ChainBrokenType
}

func (e *ChainBrokenError) Error() string {
	return fmt.Sprintf(
		"audit chain broken: line=%d event_id=%s type=%s expected=%s actual=%s",
		e.Line, e.EventID, e.TamperType, e.ExpectedHash, e.ActualHash,
	)
}

// Is allows errors.Is(err, &ChainBrokenError{}) to short-circuit.
func (e *ChainBrokenError) Is(target error) bool {
	_, ok := target.(*ChainBrokenError)
	return ok
}

// Filter narrows the events returned by Chain.Export. Zero-valued fields
// are no-op (empty string = "any plan_id", zero time = "no bound"). Limit
// and Offset support paging; Limit == 0 means "no limit".
//
// PlanID and PlanIDs are OR'd: an event passes when its plan_id matches
// PlanID OR appears in PlanIDs. With both empty, no constraint is applied.
// PlanIDs is the bulk variant the 飞检 inspector uses to bundle several
// quarters into one export.
type Filter struct {
	PlanID   string
	PlanIDs  []string
	Actor    string
	Action   workflow.IssueAction
	FromTime time.Time
	ToTime   time.Time
	Limit    int
	Offset   int
}

// Chain is the JSONL-backed hash chain. The Path is the only required
// configuration; Append will lazily create the file and Verify treats a
// missing file as a valid empty chain.
//
// lastHashCache caches the EventHash of the most recently appended row so
// sequential Append calls don't have to re-read the whole JSONL file each
// time. It is populated on the first Append and stays valid until the
// caller wipes the file or appends to a different process's chain.
type Chain struct {
	Path        string
	lastHashCache string
	cacheLoaded   bool
}

// NewChain constructs a Chain rooted at path.
func NewChain(path string) *Chain { return &Chain{Path: path} }

// canonicalBytes returns the deterministic byte slice fed into SHA256 for
// an audit row. Payload keys are sorted alphabetically so two events with
// the same logical content always hash to the same value regardless of Go
// map iteration order. The Timestamp is forced to UTC RFC3339Nano so time
// zone / monotonic clock drift cannot desynchronise the chain.
func canonicalBytes(e *workflow.AuditEvent) []byte {
	keys := make([]string, 0, len(e.Payload))
	for k := range e.Payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(e.EventID)
	b.WriteByte('\n')
	b.WriteString(e.PlanID)
	b.WriteByte('\n')
	b.WriteString(e.Actor)
	b.WriteByte('\n')
	b.WriteString(string(e.Action))
	b.WriteByte('\n')
	b.WriteString(e.Timestamp.UTC().Format(time.RFC3339Nano))
	b.WriteByte('\n')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(e.Payload[k])
	}
	b.WriteByte('\n')
	b.WriteString(e.PrevHash)
	return []byte(b.String())
}

// HashEvent computes the SHA256 event_hash for an event whose PrevHash has
// already been filled in. Exposed so test code (and the cmd/demo happy
// path) can pre-compute hashes when they need to assert exact values.
func HashEvent(e *workflow.AuditEvent) string {
	sum := sha256.Sum256(canonicalBytes(e))
	return hex.EncodeToString(sum[:])
}

// lastHash returns the event_hash stored on the final non-empty line of
// the JSONL file. GenesisHash when the file is missing or empty so callers
// can treat a fresh chain uniformly. Uses an in-memory cache populated on
// the first call so 1000 sequential Appends don't re-scan the whole file
// 1000 times.
func (c *Chain) lastHash() (string, error) {
	if c.cacheLoaded {
		return c.lastHashCache, nil
	}
	f, err := os.Open(c.Path)
	if err != nil {
		if os.IsNotExist(err) {
			c.lastHashCache = GenesisHash
			c.cacheLoaded = true
			return GenesisHash, nil
		}
		return "", fmt.Errorf("audit: open chain: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	var last string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev workflow.AuditEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return "", fmt.Errorf("audit: parse line: %w", err)
		}
		last = ev.EventHash
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("audit: scan chain: %w", err)
	}
	if last == "" {
		c.lastHashCache = GenesisHash
	} else {
		c.lastHashCache = last
	}
	c.cacheLoaded = true
	return c.lastHashCache, nil
}

// Append writes one event to the chain. It auto-fills PrevHash from the
// last line's EventHash (or GenesisHash when the chain is empty), then
// computes EventHash from the canonical form and appends a single JSON
// line followed by '\n'.
//
// EventID and Timestamp are required (otherwise the chain would have
// collisions); Append returns an error instead of synthesising values, so
// the caller remains in control of identity assignment.
func (c *Chain) Append(ctx context.Context, event workflow.AuditEvent) error {
	if event.EventID == "" {
		return errors.New("audit: event_id required")
	}
	if event.Timestamp.IsZero() {
		return errors.New("audit: timestamp required")
	}
	if event.PrevHash == "" {
		prev, err := c.lastHash()
		if err != nil {
			return err
		}
		event.PrevHash = prev
	}
	event.EventHash = HashEvent(&event)

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("audit: marshal event: %w", err)
	}

	f, err := os.OpenFile(c.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("audit: open chain for append: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("audit: write event: %w", err)
	}
	if _, err := f.Write([]byte("\n")); err != nil {
		return fmt.Errorf("audit: write newline: %w", err)
	}

	// Update the in-memory cache so the next Append doesn't re-scan the
	// whole JSONL file just to find the prev_hash anchor.
	c.lastHashCache = event.EventHash
	c.cacheLoaded = true
	return nil
}

// readAllLines loads the JSONL file into memory, skipping blank lines.
// Returns (lines, total-non-blank-count). Lines are byte-copied so the
// caller can mutate without affecting the on-disk file.
func (c *Chain) readAllLines() ([][]byte, error) {
	f, err := os.Open(c.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit: open chain: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	var lines [][]byte
	for scanner.Scan() {
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		buf := make([]byte, len(raw))
		copy(buf, raw)
		lines = append(lines, buf)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("audit: scan chain: %w", err)
	}
	return lines, nil
}

// Verify walks the JSONL file in memory and validates the SHA256 chain.
// Returns nil when the chain is intact; *ChainBrokenError with the right
// TamperType otherwise. An empty / missing file is treated as a valid
// (empty) chain.
func (c *Chain) Verify(ctx context.Context) error {
	lines, err := c.readAllLines()
	if err != nil {
		return err
	}

	prevHash := GenesisHash
	n := len(lines)
	for i, raw := range lines {
		var ev workflow.AuditEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return fmt.Errorf("audit: line %d: parse: %w", i+1, err)
		}

		// Header tamper: first line's prev_hash must equal GenesisHash.
		if i == 0 && ev.PrevHash != GenesisHash {
			return &ChainBrokenError{
				EventID:      ev.EventID,
				Line:         i + 1,
				ExpectedHash: GenesisHash,
				ActualHash:   ev.PrevHash,
				TamperType:   TamperHeader,
			}
		}

		// Broken link: prev_hash must equal previous line's event_hash.
		if i > 0 && ev.PrevHash != prevHash {
			return &ChainBrokenError{
				EventID:      ev.EventID,
				Line:         i + 1,
				ExpectedHash: prevHash,
				ActualHash:   ev.PrevHash,
				TamperType:   TamperBroken,
			}
		}

		// Middle / tail tamper: recompute event_hash from canonical form.
		expected := HashEvent(&ev)
		if ev.EventHash != expected {
			ttype := TamperMiddle
			if i == n-1 {
				ttype = TamperTail
			}
			return &ChainBrokenError{
				EventID:      ev.EventID,
				Line:         i + 1,
				ExpectedHash: expected,
				ActualHash:   ev.EventHash,
				TamperType:   ttype,
			}
		}

		prevHash = expected
	}
	return nil
}

// Export streams the filtered subset of the JSONL chain through an
// io.Reader. The reader is backed by the file on disk (via io.Pipe), so
// memory usage stays flat regardless of how large the chain grows.
//
// Returns an empty reader (not nil) when the file doesn't exist yet —
// callers can `io.ReadAll` the result without nil-deref.
func (c *Chain) Export(ctx context.Context, filter Filter) (io.Reader, error) {
	if _, err := os.Stat(c.Path); err != nil {
		if os.IsNotExist(err) {
			return strings.NewReader(""), nil
		}
		return nil, fmt.Errorf("audit: stat chain: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		f, err := os.Open(c.Path)
		if err != nil {
			pw.CloseWithError(fmt.Errorf("audit: open chain: %w", err))
			return
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1<<20), 1<<24)
		var written, skipped int
		for scanner.Scan() {
			raw := strings.TrimSpace(scanner.Text())
			if raw == "" {
				continue
			}
			var ev workflow.AuditEvent
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				pw.CloseWithError(fmt.Errorf("audit: parse line: %w", err))
				return
			}
			if !matchesFilter(&ev, &filter) {
				continue
			}
			if skipped < filter.Offset {
				skipped++
				continue
			}
			if filter.Limit > 0 && written >= filter.Limit {
				break
			}
			if _, err := pw.Write([]byte(raw)); err != nil {
				return
			}
			if _, err := pw.Write([]byte("\n")); err != nil {
				return
			}
			written++
		}
		if err := scanner.Err(); err != nil {
			pw.CloseWithError(fmt.Errorf("audit: scan chain: %w", err))
			return
		}
	}()
	return pr, nil
}

// matchesFilter is the predicate Chain.Export applies to each line. Empty
// Filter fields mean "no constraint" — only populated fields narrow the
// result set. PlanID and PlanIDs are OR'd; the rest of the fields are AND'd.
func matchesFilter(e *workflow.AuditEvent, f *Filter) bool {
	// PlanID + PlanIDs OR'd. The single-value PlanID match short-circuits
	// the multi-value list; otherwise we fall through to PlanIDs.
	switch {
	case f.PlanID != "" && len(f.PlanIDs) == 0:
		// Only single value populated.
		if e.PlanID != f.PlanID {
			return false
		}
	case f.PlanID != "" && len(f.PlanIDs) > 0:
		// OR: pass if either matches.
		if e.PlanID == f.PlanID {
			// single-value hit, pass
		} else {
			matched := false
			for _, id := range f.PlanIDs {
				if e.PlanID == id {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	case f.PlanID == "" && len(f.PlanIDs) > 0:
		// Only multi-value populated.
		matched := false
		for _, id := range f.PlanIDs {
			if e.PlanID == id {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if f.Actor != "" && e.Actor != f.Actor {
		return false
	}
	if f.Action != "" && e.Action != f.Action {
		return false
	}
	if !f.FromTime.IsZero() && e.Timestamp.Before(f.FromTime) {
		return false
	}
	if !f.ToTime.IsZero() && !e.Timestamp.Before(f.ToTime) {
		return false
	}
	return true
}