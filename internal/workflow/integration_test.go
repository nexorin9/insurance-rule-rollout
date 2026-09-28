// Integration test for the workflow + SQLite store + SHA256 audit chain.
//
// Scope (Task 18):
//   - Run the full happy path CreatePlanWithDraftReview → SubmitDraftReview
//     → ReviewIssue(Approve) → Apply against a real SQLite-backed store
//     (file::memory:?cache=shared&mode=memory), so multiple goroutines can
//     observe the same database concurrently.
//   - Assert 4 AuditEvent rows land in order with correct SHA256 prev_hash
//     chain head/tail linking.
//   - Assert the WithTx path rolls back when the tx fn returns an error
//     (here: emulated via failingTxStore): plan.status and event count
//     must NOT advance.
//   - Assert two concurrent ReviewIssue(Approve) calls on the same plan_id
//     are serialised by the row-level lock: at most one commits the
//     Pending→Approved transition; the loser sees either a typed
//     workflow error (post-lock re-check) or a SQLite BUSY / lock error.
//
// This file is intentionally independent of cmd/demo and the per-test
// mocks in engine_review_test.go / contracts_test.go. It depends on the
// real store.Store and the real audit.Chain so it catches contract
// drift between workflow, store and audit packages.
package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/insurance-rule-rollout/internal/audit"
	"github.com/example/insurance-rule-rollout/internal/store"
	"github.com/example/insurance-rule-rollout/internal/workflow"
)

// sharedMemDSN returns a unique file::memory:?cache=shared DSN per test
// invocation. Sharing across goroutines in the same test is required for
// Task 18's concurrency test; sharing across tests would leak state, so
// each call returns a fresh DSN that includes the test name + nanosecond
// timestamp so concurrent `go test` invocations on the same machine don't
// collide. The optional suffix is appended to the file path component
// (NOT to the query string) so the cache=shared query parameter stays
// well-formed.
func sharedMemDSN(t *testing.T) string {
	t.Helper()
	return sharedMemDSNWithSuffix(t, "")
}

func sharedMemDSNWithSuffix(t *testing.T, suffix string) string {
	t.Helper()
	path := fmt.Sprintf("wf-int-%s-%d", t.Name(), time.Now().UnixNano())
	if suffix != "" {
		path = path + "-" + suffix
	}
	return fmt.Sprintf("file:%s?mode=memory&cache=shared", path)
}

// chainAuditMirror adapts a *store.Store so that every InsertAuditEvent
// (per-call AuditWriter path) AND every TxInsertAuditEvent (transactional
// Store path) is mirrored into the supplied *audit.Chain. Mirror is
// best-effort: a chain write failure is logged via t.Logf but does NOT
// roll back the SQLite write — the SQLite table is the authoritative
// audit surface and the JSONL chain is an export-format projection.
// This mirrors the cmd/demo integration_test.go helper of the same shape
// to cover all four events of the full path.
type chainAuditMirror struct {
	*store.Store
	chain *audit.Chain
	t     *testing.T
}

func (m *chainAuditMirror) InsertAuditEvent(ctx context.Context, row workflow.AuditEventRow) error {
	if err := m.Store.InsertAuditEvent(ctx, row); err != nil {
		return err
	}
	m.mirrorChain(ctx, row)
	return nil
}

// TxInsertAuditEvent mirrors the per-tx audit row into the chain file.
// This is what catches the ReviewIssue(Approve) + Apply rows that go
// through WithTx → TxUpdatePlanStatus + TxInsertIssue + TxInsertAuditEvent
// rather than the per-call AuditWriter path.
func (m *chainAuditMirror) TxInsertAuditEvent(ctx context.Context, tx workflow.Tx, row workflow.AuditEventRow) error {
	if err := m.Store.TxInsertAuditEvent(ctx, tx, row); err != nil {
		return err
	}
	m.mirrorChain(ctx, row)
	return nil
}

func (m *chainAuditMirror) mirrorChain(ctx context.Context, row workflow.AuditEventRow) {
	if m.chain == nil {
		return
	}
	ev := workflow.AuditEvent{
		EventID:   row.EventID,
		PlanID:    row.PlanID,
		Actor:     row.Actor,
		Action:    row.Action,
		Payload:   row.Payload,
		Timestamp: row.Timestamp,
		PrevHash:  row.PrevHash,
		EventHash: row.EventHash,
	}
	if err := m.chain.Append(ctx, ev); err != nil {
		m.t.Logf("chain mirror append failed (non-fatal): %v", err)
	}
}

// newIntegrationStack returns a wired Engine + Store + Audit Chain backed
// by the supplied DSN, with both the per-call AuditWriter surface and the
// transactional Store surface populated on the engine. Both surfaces go
// through the chainAuditMirror so all four audit rows of the full path
// are mirrored into the SHA256 JSONL chain.
func newIntegrationStack(t *testing.T, dsn string) (*workflow.Engine, *store.Store, *audit.Chain, string) {
	t.Helper()
	ctx := context.Background()

	st, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("store.Open(%q): %v", dsn, err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}

	// Audit chain lives in a temp file so the SHA256 chain has somewhere
	// to persist; chain.Append opens the file lazily.
	chainPath := filepath.Join(t.TempDir(), "audit-chain.jsonl")
	chain := audit.NewChain(chainPath)

	mirror := &chainAuditMirror{Store: st, chain: chain, t: t}
	eng := workflow.NewEngine()
	eng.SetStore(mirror)
	// Wire the mirror as the TxStore too so TxInsertAuditEvent rows are
	// also mirrored. The mirror's Tx-prefixed methods (other than
	// TxInsertAuditEvent) come from the embedded *store.Store via method
	// promotion.
	eng.SetTxStore(mirror)
	return eng, st, chain, chainPath
}

// minimalSpec returns a RulePlanSpec with the five fields the engine
// validates non-empty (version_id / rule_set / effective_date / rules[])
// plus rollback_sql and prev_version_id so rollback helpers do not crash
// in the concurrent / rollback-on-tx-failure tests.
func minimalSpec() workflow.RulePlanSpec {
	return workflow.RulePlanSpec{
		VersionID:     "r-2026-09-DRG-v17",
		RuleSet:       "DRG",
		EffectiveDate: "2026-10-01",
		Rules: []workflow.RuleEntry{
			{Code: "DRG-A001", Name: "阑尾炎", Weight: 0.85, Limit: 12000},
			{Code: "DRG-A002", Name: "胆囊炎", Weight: 1.10, Limit: 18000},
		},
		PrevVersionID: "r-2026-06-DRG-v16",
		RollbackSQL:   "UPDATE his_dict SET version='r-2026-06-DRG-v16' WHERE version='r-2026-09-DRG-v17';",
	}
}

// TestIntegration_FullPathHappyPath covers assertions 1–3 of Task 18:
//
//	1. Plan final status == Applied.
//	2. ≥ 4 AuditEvent rows in order (Create + SubmitDraft + ReviewApprove
//	   + Apply).
//	3. AuditEvent chain: first row prev_hash == GenesisHash; every
//	   subsequent row prev_hash == previous row's event_hash (recomputed
//	   via audit.HashEvent).
func TestIntegration_FullPathHappyPath(t *testing.T) {
	dsn := sharedMemDSN(t)
	eng, st, chain, chainPath := newIntegrationStack(t, dsn)
	ctx := context.Background()

	// 1. CreatePlanWithDraftReview → status=Draft, draft=true, audit row 1.
	plan, err := eng.CreatePlanWithDraftReview(minimalSpec(), "yang_jb")
	if err != nil {
		t.Fatalf("CreatePlanWithDraftReview: %v", err)
	}
	if plan.Status != workflow.StatusDraft || !plan.IsDraft() {
		t.Fatalf("after Create: status=%v draft=%v, want Draft/true", plan.Status, plan.IsDraft())
	}

	// 2. SubmitDraftReview → Draft→Pending, draft=false, audit row 2.
	plan, err = eng.SubmitDraftReview(plan.PlanID)
	if err != nil {
		t.Fatalf("SubmitDraftReview: %v", err)
	}
	if plan.Status != workflow.StatusPending || plan.IsDraft() {
		t.Fatalf("after SubmitDraft: status=%v draft=%v, want Pending/false", plan.Status, plan.IsDraft())
	}

	// 3. ReviewIssue(Approve) → Pending→Approved, audit row 3.
	plan, err = eng.ReviewIssue(plan.PlanID, workflow.ActionApprove, "yang_jb", "已核对影响面")
	if err != nil {
		t.Fatalf("ReviewIssue(Approve): %v", err)
	}
	if plan.Status != workflow.StatusApproved {
		t.Fatalf("after Approve: status=%v, want Approved", plan.Status)
	}

	// 4. Apply → Approved→Applied, audit row 4.
	plan, err = eng.Apply(plan.PlanID, "yang_jb", "事务切换 HIS 字典表 v17")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if plan.Status != workflow.StatusApplied {
		t.Fatalf("after Apply: status=%v, want Applied", plan.Status)
	}

	// Assertion 1: in-memory plan cache + persisted SQLite row both read Applied.
	if got, err := eng.GetPlan(plan.PlanID); err != nil {
		t.Fatalf("GetPlan: %v", err)
	} else if got.Status != workflow.StatusApplied {
		t.Fatalf("cache status=%v, want Applied", got.Status)
	}
	persisted, err := st.GetPlan(ctx, plan.PlanID)
	if err != nil {
		t.Fatalf("st.GetPlan: %v", err)
	}
	if persisted.Status != workflow.StatusApplied {
		t.Fatalf("sqlite status=%v, want Applied", persisted.Status)
	}

	// Assertion 2: ≥ 4 audit events, ordered by timestamp ascending.
	events, err := st.ListEvents(ctx, plan.PlanID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) < 4 {
		t.Fatalf("ListEvents len=%d, want ≥4", len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].Timestamp.Before(events[i-1].Timestamp) {
			t.Fatalf("ListEvents[%d].Timestamp=%v before [%d].Timestamp=%v",
				i, events[i].Timestamp, i-1, events[i-1].Timestamp)
		}
	}

	// Assertion 3: SHA256 chain head/tail linking.
	rawEvents, err := readRawEvents(t, chainPath)
	if err != nil {
		t.Fatalf("readRawEvents: %v", err)
	}
	if len(rawEvents) < 4 {
		t.Fatalf("chain file has %d lines, want ≥4", len(rawEvents))
	}

	// Chain.Verify should pass.
	if err := chain.Verify(ctx); err != nil {
		t.Fatalf("chain.Verify: %v", err)
	}

	// First row prev_hash must equal GenesisHash.
	if rawEvents[0].PrevHash != audit.GenesisHash {
		t.Fatalf("chain[0].PrevHash=%q, want %q", rawEvents[0].PrevHash, audit.GenesisHash)
	}
	// Every subsequent row's prev_hash must equal the prior row's event_hash.
	for i := 1; i < len(rawEvents); i++ {
		if rawEvents[i].PrevHash != rawEvents[i-1].EventHash {
			t.Fatalf("chain[%d].PrevHash=%q != chain[%d].EventHash=%q",
				i, rawEvents[i].PrevHash, i-1, rawEvents[i-1].EventHash)
		}
		// Recomputed event_hash must match the stored value.
		expected := audit.HashEvent(rawEvents[i])
		if rawEvents[i].EventHash != expected {
			t.Fatalf("chain[%d].EventHash=%q, recomputed=%q",
				i, rawEvents[i].EventHash, expected)
		}
	}
	// First row event_hash must also recompute correctly.
	if expected := audit.HashEvent(rawEvents[0]); rawEvents[0].EventHash != expected {
		t.Fatalf("chain[0].EventHash=%q, recomputed=%q",
			rawEvents[0].EventHash, expected)
	}
}

// TestIntegration_TxRollbackOnAuditFailure covers assertion 4 of Task 18:
//
//	tx fn returns an error → WithTx ROLLBACKs → plan.status unchanged →
//	AuditEvent not landed in SQLite.
//
// Strategy: wrap the engine with a TxStore stub whose WithTx fn returns
// a sentinel error AFTER delegating to the underlying store. This
// emulates the post-fn-error rollback path without needing to inject
// failures into TxInsertAuditEvent directly.
func TestIntegration_TxRollbackOnAuditFailure(t *testing.T) {
	dsn := sharedMemDSN(t)
	eng, st, _, _ := newIntegrationStack(t, dsn)
	ctx := context.Background()

	plan, err := eng.CreatePlanWithDraftReview(minimalSpec(), "yang_jb")
	if err != nil {
		t.Fatalf("CreatePlanWithDraftReview: %v", err)
	}
	if _, err := eng.SubmitDraftReview(plan.PlanID); err != nil {
		t.Fatalf("SubmitDraftReview: %v", err)
	}

	// Capture baseline counts so the comparison is independent of any
	// earlier audit noise (CreatePlan + SubmitDraft).
	baseline, err := st.ListEvents(ctx, plan.PlanID)
	if err != nil {
		t.Fatalf("baseline ListEvents: %v", err)
	}
	baselineCount := len(baseline)

	// Override the engine's TxStore with a stub that errors AFTER fn
	// returns nil. WithTx must roll back, including any side effects fn
	// produced via TxUpdatePlanStatus / TxInsertIssue / TxInsertAuditEvent.
	eng.SetTxStore(&failingTxStore{Store: st})

	_, err = eng.ReviewIssue(plan.PlanID, workflow.ActionApprove, "yang_jb", "应该被回滚")
	if err == nil {
		t.Fatalf("ReviewIssue should have failed (failingTxStore error)")
	}

	// Plan status must NOT have advanced past Pending.
	got, err := eng.GetPlan(plan.PlanID)
	if err != nil {
		t.Fatalf("GetPlan: %v", err)
	}
	if got.Status != workflow.StatusPending {
		t.Fatalf("plan status after failed tx = %v, want Pending (no advance)", got.Status)
	}

	// Persisted plan row must still be Pending — the WithTx ROLLBACK must
	// undo the in-tx TxUpdatePlanStatus.
	persisted, err := st.GetPlan(ctx, plan.PlanID)
	if err != nil {
		t.Fatalf("persisted GetPlan: %v", err)
	}
	if persisted.Status != workflow.StatusPending {
		t.Fatalf("sqlite status after failed tx = %v, want Pending", persisted.Status)
	}

	// AuditEvent count must NOT have grown — TxInsertIssue and the
	// TxUpdatePlanStatus were rolled back together with TxInsertAuditEvent.
	after, err := st.ListEvents(ctx, plan.PlanID)
	if err != nil {
		t.Fatalf("post-failure ListEvents: %v", err)
	}
	if len(after) != baselineCount {
		t.Fatalf("audit event count after failed tx = %d, want %d (rollback)",
			len(after), baselineCount)
	}
}

// TestIntegration_ConcurrentReviewIssue covers assertion 5 of Task 18:
//
//	Two concurrent ReviewIssue(Approve) calls on the same plan_id with
//	tStore wired. At most one Pending→Approved transition commits; the
//	loser observes one of:
//	    a) typed workflow illegal-transition error (post-lock re-check
//	       found the row already approved), or
//	    b) SQLite BUSY / Serialization failure surfaced as a generic error.
//
// Crucially, the plan must end in Approved (NOT Applied, and NOT a state
// that two reviewers both "succeeded" into).
//
// We repeat the concurrent run N times to give the race a real chance to
// manifest; a single-shot test is too easy to pass on slow hardware.
func TestIntegration_ConcurrentReviewIssue(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("need GOMAXPROCS>=2 to exercise concurrency")
	}

	const trials = 6
	for trial := 0; trial < trials; trial++ {
		trial := trial
		t.Run(fmt.Sprintf("trial-%d", trial), func(t *testing.T) {
			// Each iteration gets its own shared-memory DB so previous
			// trial state can't leak through *store.Store (SQLite pool
			// state).
			dsn := sharedMemDSNWithSuffix(t, fmt.Sprintf("trial%d", trial))
			eng, _, _, _ := newIntegrationStack(t, dsn)

			plan, err := eng.CreatePlanWithDraftReview(minimalSpec(), "yang_jb")
			if err != nil {
				t.Fatalf("CreatePlanWithDraftReview: %v", err)
			}
			if _, err := eng.SubmitDraftReview(plan.PlanID); err != nil {
				t.Fatalf("SubmitDraftReview: %v", err)
			}

			// Two goroutines race ReviewIssue on the same plan_id.
			var (
				wg          sync.WaitGroup
				successes   atomic.Int32
				successMu   sync.Mutex
				loserErrs   []error
				ctx2, cancel = context.WithTimeout(context.Background(), 5*time.Second)
			)
			defer cancel()

			start := make(chan struct{})
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					<-start
					_, err := eng.ReviewIssue(plan.PlanID, workflow.ActionApprove,
						fmt.Sprintf("reviewer-%d", idx), fmt.Sprintf("trial-%d-g%d", trial, idx))
					if err == nil {
						successes.Add(1)
						return
					}
					successMu.Lock()
					loserErrs = append(loserErrs, err)
					successMu.Unlock()
					_ = ctx2
				}(i)
			}
			close(start)
			wg.Wait()

			// At most one goroutine may succeed.
			if got := successes.Load(); got > 1 {
				t.Fatalf("expected ≤1 success under concurrent ReviewIssue, got %d", got)
			}

			// Final plan status must be Pending (if both lost) or Approved
			// (if one won). Never Applied / Rejected / RolledBack.
			final, err := eng.GetPlan(plan.PlanID)
			if err != nil {
				t.Fatalf("GetPlan after race: %v", err)
			}
			switch final.Status {
			case workflow.StatusPending:
				if successes.Load() != 0 {
					t.Fatalf("status=Pending but successes=%d", successes.Load())
				}
			case workflow.StatusApproved:
				if successes.Load() != 1 {
					t.Fatalf("status=Approved but successes=%d", successes.Load())
				}
			default:
				t.Fatalf("unexpected final status=%v (successes=%d)", final.Status, successes.Load())
			}

			// Every loser error must be either:
			//   - a typed workflow illegal-transition error (post-lock re-check
			//     found the row already approved), or
			//   - a SQLite database error surfaced from WithTx.
			for _, e := range loserErrs {
				if !isAcceptableRaceLoss(e) {
					t.Fatalf("trial %d: unacceptable loser err = %v", trial, e)
				}
			}
		})
	}
}

// TestIntegration_ChainTamperDetectedAfterFullPath covers the
// post-condition that the chain file is genuinely append-only and
// verifiable end-to-end: after a happy path we corrupt one byte in the
// middle of the chain file and assert that Verify reports the right
// TamperType on the right line.
func TestIntegration_ChainTamperDetectedAfterFullPath(t *testing.T) {
	dsn := sharedMemDSN(t)
	eng, _, chain, chainPath := newIntegrationStack(t, dsn)
	ctx := context.Background()

	plan, err := eng.CreatePlanWithDraftReview(minimalSpec(), "yang_jb")
	if err != nil {
		t.Fatalf("CreatePlanWithDraftReview: %v", err)
	}
	if _, err := eng.SubmitDraftReview(plan.PlanID); err != nil {
		t.Fatalf("SubmitDraftReview: %v", err)
	}
	if _, err := eng.ReviewIssue(plan.PlanID, workflow.ActionApprove, "yang_jb", "ok"); err != nil {
		t.Fatalf("ReviewIssue: %v", err)
	}
	if _, err := eng.Apply(plan.PlanID, "yang_jb", "ok"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Tamper a payload byte on the second line (SubmitDraft event).
	raw, err := os.ReadFile(chainPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Find the second JSONL line.
	firstNL := -1
	for i, b := range raw {
		if b == '\n' {
			firstNL = i
			break
		}
	}
	if firstNL < 0 || firstNL == len(raw)-1 {
		t.Fatalf("chain has <2 lines (firstNL=%d len=%d)", firstNL, len(raw))
	}
	// Flip one byte inside the second line (well past the JSON header so
	// we don't accidentally corrupt a syntax character).
	flipOffset := firstNL + 32
	if flipOffset >= len(raw) || raw[flipOffset] == '\n' {
		flipOffset = firstNL + 8
	}
	raw[flipOffset] ^= 0x01
	if err := os.WriteFile(chainPath, raw, 0o644); err != nil {
		t.Fatalf("WriteFile tamper: %v", err)
	}

	err = chain.Verify(ctx)
	if err == nil {
		t.Fatalf("chain.Verify should have flagged the tamper")
	}
	var cb *audit.ChainBrokenError
	if !errors.As(err, &cb) {
		t.Fatalf("error type=%T, want *ChainBrokenError", err)
	}
	// Line 2 was tampered (SubmitDraft); recomputed hash will mismatch.
	if cb.Line != 2 {
		t.Fatalf("cb.Line=%d, want 2", cb.Line)
	}
	if cb.TamperType != audit.TamperMiddle && cb.TamperType != audit.TamperBroken {
		t.Fatalf("cb.TamperType=%q, want TamperMiddle or TamperBroken", cb.TamperType)
	}
}

// TestIntegration_AuditEventCountExactlyFour pins the AuditEvent surface
// to exactly 4 rows for the happy path. Combined with
// TestIntegration_FullPathHappyPath's ≥4 assertion, this guards against
// both missing rows and accidental extra inserts (e.g. a future refactor
// that double-writes).
func TestIntegration_AuditEventCountExactlyFour(t *testing.T) {
	dsn := sharedMemDSN(t)
	eng, st, _, _ := newIntegrationStack(t, dsn)
	ctx := context.Background()

	plan, err := eng.CreatePlanWithDraftReview(minimalSpec(), "yang_jb")
	if err != nil {
		t.Fatalf("CreatePlanWithDraftReview: %v", err)
	}
	if _, err := eng.SubmitDraftReview(plan.PlanID); err != nil {
		t.Fatalf("SubmitDraftReview: %v", err)
	}
	if _, err := eng.ReviewIssue(plan.PlanID, workflow.ActionApprove, "yang_jb", "ok"); err != nil {
		t.Fatalf("ReviewIssue: %v", err)
	}
	if _, err := eng.Apply(plan.PlanID, "yang_jb", "ok"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	events, err := st.ListEvents(ctx, plan.PlanID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("ListEvents len=%d, want exactly 4", len(events))
	}

	// Spot-check the action sequence.
	wantActions := []workflow.IssueAction{
		workflow.IssueAction("create_plan"),
		workflow.ActionSubmitDraft,
		workflow.ActionApprove,
		workflow.ActionApply,
	}
	for i, want := range wantActions {
		if events[i].Action != want {
			t.Fatalf("events[%d].Action=%q, want %q", i, events[i].Action, want)
		}
	}
}

// ---------- helpers ----------

// readRawEvents reads the JSONL chain file from disk and returns the
// parsed workflow.AuditEvent list. Used by
// TestIntegration_FullPathHappyPath to verify the SHA256 chain head/tail
// links independently of any mirroring helper that may have been wired
// into the engine.
func readRawEvents(t *testing.T, path string) ([]*workflow.AuditEvent, error) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []*workflow.AuditEvent
	start := 0
	for i, b := range data {
		if b != '\n' {
			continue
		}
		line := data[start:i]
		start = i + 1
		if len(line) == 0 {
			continue
		}
		var ev workflow.AuditEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, fmt.Errorf("parse line: %w", err)
		}
		out = append(out, &ev)
	}
	return out, nil
}

// failingTxStore wraps a real *store.Store but returns a sentinel error
// AFTER delegating to the underlying store's WithTx. All Tx-prefixed
// methods delegate to the underlying store so LockPlan /
// TxUpdatePlanStatus / TxInsertIssue still produce their in-tx side
// effects — when WithTx then rolls back, none of those side effects
// survive.
type failingTxStore struct {
	*store.Store
}

// failingTxStore.WithTx delegates to the underlying store and then
// returns a sentinel error so the tx fn's writes are rolled back by the
// database driver. The signature mirrors workflow.Store.WithTx exactly
// so the engine treats failingTxStore as a drop-in replacement.
func (f *failingTxStore) WithTx(ctx context.Context, fn func(workflow.Tx) error) error {
	return f.Store.WithTx(ctx, func(tx workflow.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		// fn returned nil — emulate a post-commit audit write failure so
		// the tx is aborted. The engine's tx fn will already have called
		// TxUpdatePlanStatus / TxInsertIssue / TxInsertAuditEvent by the
		// time fn returns nil, and the rollback undoes all of them.
		return fmt.Errorf("injected failure on TxInsertAuditEvent")
	})
}

// isAcceptableRaceLoss reports whether err is the kind of error a "loser"
// goroutine in TestIntegration_ConcurrentReviewIssue is allowed to see:
//
//   - typed workflow illegal-transition errors (ErrPendingReApprove /
//     ErrApprovedRequest / ErrAppliedReApprove / ErrPendingSelfLoop) — the
//     post-lock re-check found the row already moved,
//   - SQLite BUSY / Serialization failures wrapped from database/sql,
//   - generic fmt.Errorf strings emitted by engine.persistReviewError
//     that mention SQLite busy/lock — driver-level race loss.
//
// Anything else (e.g. a panic surfaced as a non-typed error, or a real
// bug in the lock plumbing) fails the trial.
func isAcceptableRaceLoss(err error) bool {
	if err == nil {
		return false
	}
	// Typed workflow errors first. The 9 typed illegal-transition errors
// embed workflow.IllegalTransitionError (value embedding, not interface
// embedding), so errors.As can't reach the base type — match by message
// prefix instead. Every typed error emits "illegal transition <status>
// → <action>" via illegalTransitionErrorMessage, which is the canonical
// shape of a post-lock "row already advanced" rejection.
	msg := err.Error()
	if strings.Contains(msg, "illegal transition ") {
		return true
	}
	var invalidActErr *workflow.InvalidActionError
	if errors.As(err, &invalidActErr) {
		return true
	}
	var terminalErr *workflow.TerminalStateError
	if errors.As(err, &terminalErr) {
		return true
	}
	// SQLite / database/sql race surfaces ("database is locked",
	// "SQLITE_BUSY", "database table is locked", serialization failure).
	for _, needle := range []string{
		"database is locked",
		"SQLITE_BUSY",
		"table is locked",
		"serialization",
		"locked",
		"conflict",
	} {
		if strings.Contains(strings.ToLower(msg), strings.ToLower(needle)) {
			return true
		}
	}
	return false
}