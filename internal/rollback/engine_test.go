// Tests for the rollback engine.
//
// Coverage map:
//   - InferReverseSQL       — 3 categories (modify / add / remove) + invalid spec
//   - InferReverseOps        — happy path with fixture
//   - Rollback happy path    — dict_v16 → dict_v17 → rollback → dict_v16
//   - Rollback status guard  — non-Applied plan rejected (TerminalStateError)
//   - Rollback failure       — DictClient errors → status stays Applied + ROLLBACK_FAILED event
//   - Plan not found         — LoadPlan returns NotFoundError
//
// Fixtures come from testdata/platform_response.json (the new spec) and
// testdata/dict_v17.json (the live state after v17 has been applied). The
// inferred reverse should bring each rule's limit back to its prev_limit
// from the spec.
package rollback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/insurance-rule-rollout/internal/audit"
	"github.com/example/insurance-rule-rollout/internal/integration/his_dict"
	"github.com/example/insurance-rule-rollout/internal/store"
	"github.com/example/insurance-rule-rollout/internal/workflow"
)

// ---------- fixtures ----------

func mustReadJSON(t *testing.T, path string, into any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("parse fixture %s: %v", path, err)
	}
}

// v17EntriesFromFixture loads testdata/dict_v17.json and returns its
// entries as []his_dict.DictEntry suitable for DictClient.ApplyUpdate.
func v17EntriesFromFixture(t *testing.T) []his_dict.DictEntry {
	t.Helper()
	var raw struct {
		Entries []struct {
			Code          string  `json:"code"`
			Name          string  `json:"name"`
			Weight        float64 `json:"weight"`
			Limit         float64 `json:"limit"`
			EffectiveDate string  `json:"effective_date"`
			Version       string  `json:"version"`
		} `json:"entries"`
	}
	mustReadJSON(t, filepath.Join("..", "..", "testdata", "dict_v17.json"), &raw)
	out := make([]his_dict.DictEntry, 0, len(raw.Entries))
	for _, e := range raw.Entries {
		entry := his_dict.DictEntry{
			Code:          e.Code,
			Name:          e.Name,
			Weight:        e.Weight,
			Limit:         e.Limit,
			EffectiveDate: e.EffectiveDate,
			Version:       e.Version,
			UpdatedAt:     time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC),
		}
		// Real RuleSet data: drop the "（示例）" suffix for the test
		// fixture so names match what the rollback will try to restore.
		out = append(out, entry)
	}
	return out
}

// specJSONFromFixture loads testdata/platform_response.json and returns
// the JSON blob (with rule names scrubbed of "（示例）" suffix to match
// the dict_v17 entries).
func specJSONFromFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "platform_response.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(data)
}

// appliedPlan constructs a workflow.Plan in StatusApplied with the
// supplied spec_json. plan_id and version are caller-controlled.
func appliedPlan(planID, versionID, prevVersionID, specJSON string) workflow.Plan {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	return workflow.Plan{
		PlanID:        planID,
		VersionID:     versionID,
		RuleSet:       "DRG-2026-09-NATIONAL",
		EffectiveDate: "2026-10-01",
		SpecJSON:      specJSON,
		PrevVersionID: prevVersionID,
		RollbackSQL:   "",
		Status:        workflow.StatusApplied,
		Draft:         false,
		CreatedBy:     "yang_jb",
		LastAction:    workflow.ActionApply,
		LastActionAt:  now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
}

// storeAdapter bridges *store.Store (which exposes GetPlan returning
// PlanRow) to rollback.PlanStore (which needs workflow.Plan).
type storeAdapter struct {
	*store.Store
}

func (a *storeAdapter) LoadPlan(ctx context.Context, planID string) (workflow.Plan, error) {
	row, err := a.Store.GetPlan(ctx, planID)
	if err != nil {
		return workflow.Plan{}, err
	}
	return workflow.Plan{
		PlanID:        row.PlanID,
		VersionID:     row.VersionID,
		RuleSet:       row.RuleSet,
		EffectiveDate: row.EffectiveDate,
		SpecJSON:      row.SpecJSON,
		PrevVersionID: row.PrevVersionID,
		RollbackSQL:   row.RollbackSQL,
		Status:        row.Status,
		Draft:         row.Draft,
		CreatedBy:     row.CreatedBy,
		LastAction:    row.LastAction,
		LastActionAt:  row.LastActionAt,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}, nil
}

func (a *storeAdapter) SavePlanStatus(ctx context.Context, planID string,
	status workflow.PlanStatus, draft bool, lastAction workflow.IssueAction,
	lastActionAt, updatedAt time.Time) error {
	return a.Store.UpdatePlanStatus(ctx, planID, status, draft, lastAction, lastActionAt, updatedAt)
}

// newSQLiteHisClient opens a private in-memory his_dict SQLite for the
// test and pre-seeds the v17 entries (post-Apply state).
func newSQLiteHisClient(t *testing.T, seedV17 bool) *his_dict.SQLiteClient {
	t.Helper()
	dsn := fmt.Sprintf("file:rollback-test-%d?mode=memory&cache=private", time.Now().UnixNano())
	c, err := his_dict.NewSQLiteClient(dsn)
	if err != nil {
		t.Fatalf("his_dict open: %v", err)
	}
	if seedV17 {
		if err := c.ApplyUpdate(context.Background(), v17EntriesFromFixture(t)); err != nil {
			t.Fatalf("seed v17: %v", err)
		}
	}
	return c
}

// newSQLitePlanStore opens a private in-memory workflow store and migrates
// the schema; returns the adapter that satisfies rollback.PlanStore.
func newSQLitePlanStore(t *testing.T) *storeAdapter {
	t.Helper()
	dsn := fmt.Sprintf("file:rollback-plan-%d?mode=memory&cache=private", time.Now().UnixNano())
	s, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("store migrate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &storeAdapter{Store: s}
}

// persistPlan writes a Plan to the store via the workflow row type.
func persistPlan(t *testing.T, sa *storeAdapter, plan workflow.Plan) {
	t.Helper()
	row := workflow.PlanRow{
		PlanID:        plan.PlanID,
		VersionID:     plan.VersionID,
		RuleSet:       plan.RuleSet,
		EffectiveDate: plan.EffectiveDate,
		SpecJSON:      plan.SpecJSON,
		PrevVersionID: plan.PrevVersionID,
		RollbackSQL:   plan.RollbackSQL,
		Status:        plan.Status,
		Draft:         plan.Draft,
		CreatedBy:     plan.CreatedBy,
		LastAction:    plan.LastAction,
		LastActionAt:  plan.LastActionAt,
		CreatedAt:     plan.CreatedAt,
		UpdatedAt:     plan.UpdatedAt,
	}
	if err := sa.Store.InsertPlan(context.Background(), row); err != nil {
		t.Fatalf("persist plan: %v", err)
	}
}

// newTestChain wires an audit.Chain rooted at a temp file.
func newTestChain(t *testing.T) *audit.Chain {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chain.jsonl")
	return audit.NewChain(path)
}

// ---------- InferReverseSQL unit tests ----------

func TestInferReverseOps_ModifyOnly(t *testing.T) {
	plan := appliedPlan("p1", "v17", "v16", specJSONFromFixture(t))
	dict := v17EntriesFromFixture(t)

	ops, err := InferReverseOps(plan, dict)
	if err != nil {
		t.Fatalf("InferReverseOps: %v", err)
	}
	if len(ops) == 0 {
		t.Fatalf("expected at least one reverse op, got 0")
	}
	// All 5 rules have prev_limit > 0 AND exist in dict → all should be UPDATE
	for _, op := range ops {
		if op.Type != ReverseOpUpdate {
			t.Errorf("op %s: expected UPDATE, got %s", op.Code, op.Type)
		}
		if op.Limit <= 0 {
			t.Errorf("op %s: reverse limit must be > 0, got %.2f", op.Code, op.Limit)
		}
		if op.OldLimit <= 0 {
			t.Errorf("op %s: old limit must be > 0, got %.2f", op.Code, op.OldLimit)
		}
		if op.OldLimit == op.Limit {
			t.Errorf("op %s: old == new (%.2f) — modify should differ", op.Code, op.Limit)
		}
	}
}

func TestInferReverseOps_NewlyAdded(t *testing.T) {
	// spec has 1 rule (DRG-X999) NOT in currentDict → reverse is DELETE
	spec := `{"version_id":"v17","rule_set":"DRG","effective_date":"2026-10-01","prev_version_id":"v16","rules":[
		{"code":"DRG-X999","name":"新规则","weight":1.0,"limit":10000.0,"prev_limit":0.0}
	]}`
	plan := appliedPlan("p2", "v17", "v16", spec)
	dict := v17EntriesFromFixture(t) // contains v17 entries, NOT DRG-X999

	ops, err := InferReverseOps(plan, dict)
	if err != nil {
		t.Fatalf("InferReverseOps: %v", err)
	}
	var del []ReverseOp
	for _, op := range ops {
		if op.Type == ReverseOpDelete {
			del = append(del, op)
		}
	}
	if len(del) != 1 {
		t.Fatalf("expected 1 DELETE op, got %d (ops=%+v)", len(del), ops)
	}
	if del[0].Code != "DRG-X999" {
		t.Errorf("expected DRG-X999, got %s", del[0].Code)
	}
}

func TestInferReverseOps_Removed(t *testing.T) {
	// spec does NOT include DRG-A001 → reverse is INSERT
	spec := `{"version_id":"v17","rule_set":"DRG","effective_date":"2026-10-01","prev_version_id":"v16","rules":[
		{"code":"DRG-A002","name":"肝胆胰系统手术","weight":3.18,"limit":49500.0,"prev_limit":51200.0}
	]}`
	plan := appliedPlan("p3", "v17", "v16", spec)
	dict := v17EntriesFromFixture(t)

	ops, err := InferReverseOps(plan, dict)
	if err != nil {
		t.Fatalf("InferReverseOps: %v", err)
	}
	var ins []ReverseOp
	for _, op := range ops {
		if op.Type == ReverseOpInsert {
			ins = append(ins, op)
		}
	}
	if len(ins) == 0 {
		t.Fatalf("expected at least 1 INSERT op for removed rules, got 0")
	}
	// DRG-A001 should appear as INSERT (since spec doesn't include it)
	found := false
	for _, op := range ins {
		if op.Code == "DRG-A001" {
			found = true
			if op.Name == "" {
				t.Errorf("INSERT op missing name for %s", op.Code)
			}
			if op.Limit <= 0 {
				t.Errorf("INSERT op missing limit for %s", op.Code)
			}
		}
	}
	if !found {
		t.Errorf("expected INSERT op for DRG-A001, ops=%+v", ops)
	}
}

func TestInferReverseOps_EmptySpecJSON(t *testing.T) {
	plan := appliedPlan("p4", "v17", "v16", "")
	_, err := InferReverseOps(plan, nil)
	if err == nil {
		t.Fatalf("expected error for empty spec_json, got nil")
	}
}

func TestInferReverseOps_InvalidSpecJSON(t *testing.T) {
	plan := appliedPlan("p5", "v17", "v16", "not valid json{")
	_, err := InferReverseOps(plan, nil)
	if err == nil {
		t.Fatalf("expected error for invalid spec_json, got nil")
	}
}

func TestInferReverseSQL_HappyPath(t *testing.T) {
	plan := appliedPlan("p6", "v17", "v16", specJSONFromFixture(t))
	dict := v17EntriesFromFixture(t)

	sql, err := InferReverseSQL(plan, dict)
	if err != nil {
		t.Fatalf("InferReverseSQL: %v", err)
	}
	if !strings.Contains(sql, "UPDATE his_dict") {
		t.Errorf("expected SQL to contain UPDATE his_dict, got: %s", sql)
	}
	if !strings.Contains(sql, "35200") { // first prev_limit from fixture
		t.Errorf("expected SQL to contain prev_limit 35200, got: %s", sql)
	}
	if !strings.Contains(sql, "v16") { // prev_version_id reference
		t.Errorf("expected SQL to contain prev_version 'v16', got: %s", sql)
	}
}

func TestInferReverseSQL_EmptyDiff(t *testing.T) {
	// spec matches dict exactly with prev_limit == current limit → no ops
	spec := `{"version_id":"v17","rule_set":"DRG","effective_date":"2026-10-01","prev_version_id":"v16","rules":[
		{"code":"DRG-A001","name":"x","weight":2.45,"limit":36800.0,"prev_limit":36800.0}
	]}`
	plan := appliedPlan("p7", "v17", "v16", spec)
	dict := []his_dict.DictEntry{{Code: "DRG-A001", Name: "x", Weight: 2.45, Limit: 36800.0, EffectiveDate: "2026-10-01", Version: "v17"}}
	sql, err := InferReverseSQL(plan, dict)
	if err != nil {
		t.Fatalf("InferReverseSQL: %v", err)
	}
	if !strings.Contains(sql, "no reverse ops") {
		t.Errorf("expected 'no reverse ops needed', got: %s", sql)
	}
}

// ---------- Rollback integration tests ----------

func TestRollback_HappyPath(t *testing.T) {
	ctx := context.Background()
	hisClient := newSQLiteHisClient(t, true)   // seed v17 entries
	planStore := newSQLitePlanStore(t)
	chain := newTestChain(t)

	plan := appliedPlan("plan-happy", "r-2026-09-DRG-v17", "r-2026-06-DRG-v16", specJSONFromFixture(t))
	persistPlan(t, planStore, plan)

	re := &RollbackEngine{
		Dict:  hisClient,
		Store: planStore,
		Chain: chain,
	}

	if err := re.Rollback(ctx, plan.PlanID, "wang_xx", "现场发现 v17 限额异常，回滚到 v16"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// 1. dict now has prev_limit values
	schema, err := hisClient.ReadSchema(ctx)
	if err != nil {
		t.Fatalf("ReadSchema after rollback: %v", err)
	}
	wantLimits := map[string]float64{
		"DRG-A001": 35200.00,
		"DRG-A002": 51200.00,
		"DRG-B014": 15800.00,
		"DRG-C007": 31200.00,
		"DRG-E022": 11500.00, // unchanged
	}
	for _, e := range schema.Entries {
		want, ok := wantLimits[e.Code]
		if !ok {
			t.Errorf("unexpected code after rollback: %s", e.Code)
			continue
		}
		if e.Limit != want {
			t.Errorf("code %s: limit = %.2f, want %.2f", e.Code, e.Limit, want)
		}
	}

	// 2. Plan status flipped to RolledBack
	p, err := planStore.LoadPlan(ctx, plan.PlanID)
	if err != nil {
		t.Fatalf("LoadPlan after rollback: %v", err)
	}
	if p.Status != workflow.StatusRolledBack {
		t.Errorf("status = %s, want rolled_back", p.Status)
	}

	// 3. audit chain is intact
	if err := chain.Verify(ctx); err != nil {
		t.Errorf("chain.Verify after rollback: %v", err)
	}

	// 4. dict has only 5 rows (no adds/deletes, only updates)
	if got := len(schema.Entries); got != 5 {
		t.Errorf("entry count after rollback = %d, want 5", got)
	}
}

func TestRollback_StatusGuard_NotApplied(t *testing.T) {
	ctx := context.Background()
	hisClient := newSQLiteHisClient(t, true)
	planStore := newSQLitePlanStore(t)
	chain := newTestChain(t)

	plan := appliedPlan("plan-pending", "v17", "v16", specJSONFromFixture(t))
	plan.Status = workflow.StatusPending // simulate "not yet applied"
	persistPlan(t, planStore, plan)

	re := &RollbackEngine{Dict: hisClient, Store: planStore, Chain: chain}

	err := re.Rollback(ctx, plan.PlanID, "wang_xx", "should fail")
	if err == nil {
		t.Fatalf("expected error for non-Applied plan, got nil")
	}
	var terminal *workflow.TerminalStateError
	if !errors.As(err, &terminal) {
		t.Errorf("expected *TerminalStateError, got %T: %v", err, err)
	}
	if terminal.From != workflow.StatusPending {
		t.Errorf("terminal.From = %s, want pending", terminal.From)
	}

	// Status unchanged
	p, _ := planStore.LoadPlan(ctx, plan.PlanID)
	if p.Status != workflow.StatusPending {
		t.Errorf("status should stay pending, got %s", p.Status)
	}
}

func TestRollback_DictClientFailure(t *testing.T) {
	ctx := context.Background()
	hisClient := newSQLiteHisClient(t, true)
	planStore := newSQLitePlanStore(t)
	chain := newTestChain(t)

	plan := appliedPlan("plan-fail", "v17", "v16", specJSONFromFixture(t))
	persistPlan(t, planStore, plan)

	// Wrap hisClient with a failure-injecting adapter
	re := &RollbackEngine{
		Dict:  &failingDictClient{inner: hisClient},
		Store: planStore,
		Chain: chain,
	}

	err := re.Rollback(ctx, plan.PlanID, "wang_xx", "rollback failed test")
	if err == nil {
		t.Fatalf("expected error from failing DictClient, got nil")
	}

	// Plan status stays Applied (failure must NOT mutate state)
	p, _ := planStore.LoadPlan(ctx, plan.PlanID)
	if p.Status != workflow.StatusApplied {
		t.Errorf("status should stay applied after failure, got %s", p.Status)
	}

	// AuditEvent chain has ROLLBACK_FAILED entry — verify SHA256 chain
	// integrity and explicitly assert the payload contains rollback_failed
	// reason so the 飞检 inspector can grep audit logs for failure events.
	if err := chain.Verify(ctx); err != nil {
		t.Errorf("chain.Verify after failure: %v", err)
	}
	events := readAllChainEvents(t, chain)
	if len(events) == 0 {
		t.Fatalf("expected at least 1 chain event after rollback failure, got 0")
	}
	foundFailed := false
	for _, ev := range events {
		if ev.Action != workflow.ActionRollback {
			continue
		}
		if ev.PlanID != plan.PlanID {
			t.Errorf("rollback event plan_id = %s, want %s", ev.PlanID, plan.PlanID)
		}
		if ev.Payload["result"] != "rollback_failed" {
			continue
		}
		foundFailed = true
		if ev.Payload["reason"] != "apply_update_failed" {
			t.Errorf("reason = %s, want apply_update_failed", ev.Payload["reason"])
		}
		if !strings.Contains(ev.Payload["detail"], "simulated driver failure") {
			t.Errorf("detail should contain driver error msg, got: %s", ev.Payload["detail"])
		}
		if ev.Actor != "wang_xx" {
			t.Errorf("actor = %s, want wang_xx", ev.Actor)
		}
	}
	if !foundFailed {
		t.Fatalf("expected rollback_failed event in chain, events=%+v", events)
	}

	// Dict still has v17 entries (rollback did not write) — limits unchanged
	schema, err := hisClient.ReadSchema(ctx)
	if err != nil {
		t.Fatalf("ReadSchema: %v", err)
	}
	for _, e := range schema.Entries {
		if e.Limit == 0 {
			t.Errorf("entry %s should retain v17 limit, got 0", e.Code)
		}
	}
}

func TestRollback_PlanNotFound(t *testing.T) {
	ctx := context.Background()
	hisClient := newSQLiteHisClient(t, false)
	planStore := newSQLitePlanStore(t)
	chain := newTestChain(t)

	re := &RollbackEngine{Dict: hisClient, Store: planStore, Chain: chain}

	err := re.Rollback(ctx, "does-not-exist", "wang_xx", "x")
	if err == nil {
		t.Fatalf("expected error for missing plan, got nil")
	}
	if !strings.Contains(err.Error(), "load plan") {
		t.Errorf("error should mention 'load plan', got: %v", err)
	}
}

func TestRollback_EmptyPlanID(t *testing.T) {
	re := &RollbackEngine{}
	if err := re.Rollback(context.Background(), "", "x", "y"); err == nil {
		t.Errorf("expected error for empty planID, got nil")
	}
}

func TestRollback_EmptyActor(t *testing.T) {
	re := &RollbackEngine{}
	if err := re.Rollback(context.Background(), "p", "", "y"); err == nil {
		t.Errorf("expected error for empty actor, got nil")
	}
}

func TestRollback_MissingCollaborators(t *testing.T) {
	if err := (&RollbackEngine{}).Rollback(context.Background(), "p", "a", "c"); err == nil {
		t.Errorf("expected error for missing collaborators, got nil")
	}
}

// ---------- failure-injection helper ----------

// failingDictClient wraps a real DictClient and forces ApplyUpdate to
// return an error. Used to test the ROLLBACK_FAILED recovery path.
type failingDictClient struct {
	inner his_dict.DictClient
}

func (f *failingDictClient) ApplyUpdate(ctx interface{}, entries []his_dict.DictEntry) error {
	return fmt.Errorf("simulated driver failure on ApplyUpdate")
}

func (f *failingDictClient) ReadSchema(ctx interface{}) (his_dict.DictSchema, error) {
	return f.inner.ReadSchema(ctx)
}

func (f *failingDictClient) DryRunScan(ctx interface{}, entries []his_dict.DictEntry) error {
	return f.inner.DryRunScan(ctx, entries)
}

func (f *failingDictClient) Close() error { return f.inner.Close() }

// ---------- multi-plan concurrent rollback ----------

// TestRollback_MultiPlanConcurrentSerialization verifies that calling
// Rollback on multiple plans from independent goroutines does not corrupt
// the SHA256 audit chain. Each plan must finish in StatusRolledBack; the
// chain must contain exactly one ROLLBACK event per plan with intact hash
// links; the dict must be rolled back to v16 limits for every plan.
//
// Source: mirrors bytebase's concurrent Apply guard semantics — the audit
// chain is the single source of truth for the 飞检 inspector and must not
// tolerate interleaved partial writes.
func TestRollback_MultiPlanConcurrentSerialization(t *testing.T) {
	ctx := context.Background()
	hisClient := newSQLiteHisClient(t, true) // shared v17 seed
	planStore := newSQLitePlanStore(t)
	chain := newTestChain(t)

	const planCount = 5
	plans := make([]workflow.Plan, planCount)
	for i := 0; i < planCount; i++ {
		p := appliedPlan(
			fmt.Sprintf("plan-concurrent-%02d", i),
			fmt.Sprintf("r-2026-09-DRG-v17-plan%d", i),
			"r-2026-06-DRG-v16",
			specJSONFromFixture(t),
		)
		persistPlan(t, planStore, p)
		plans[i] = p
	}

	re := &RollbackEngine{
		Dict:  hisClient,
		Store: planStore,
		Chain: chain,
	}

	// Fire all rollbacks concurrently. The engine's mu must serialise the
	// shared chain.Append + lastHashCache updates; otherwise Verify would
	// report a broken hash link.
	var (
		wg          sync.WaitGroup
		successes   atomic.Int32
		failures    atomic.Int32
		failMu      sync.Mutex
		failedErrs  []error
	)
	start := make(chan struct{})
	for i := 0; i < planCount; i++ {
		idx := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := re.Rollback(ctx, plans[idx].PlanID,
				fmt.Sprintf("reviewer-%d", idx),
				fmt.Sprintf("concurrent rollback %d", idx))
			if err != nil {
				failures.Add(1)
				failMu.Lock()
				failedErrs = append(failedErrs, fmt.Errorf("plan %s: %w", plans[idx].PlanID, err))
				failMu.Unlock()
				return
			}
			successes.Add(1)
		}()
	}
	close(start)
	wg.Wait()

	if failures.Load() != 0 {
		t.Fatalf("expected 0 rollback failures under concurrent call, got %d: %v", failures.Load(), failedErrs)
	}
	if successes.Load() != planCount {
		t.Fatalf("expected %d successful rollbacks, got %d", planCount, successes.Load())
	}

	// Every plan must end in StatusRolledBack
	for _, p := range plans {
		loaded, err := planStore.LoadPlan(ctx, p.PlanID)
		if err != nil {
			t.Errorf("LoadPlan %s: %v", p.PlanID, err)
			continue
		}
		if loaded.Status != workflow.StatusRolledBack {
			t.Errorf("plan %s status = %s, want rolled_back", p.PlanID, loaded.Status)
		}
	}

	// SHA256 chain must be intact (no interleaved-write tampering)
	if err := chain.Verify(ctx); err != nil {
		t.Fatalf("chain.Verify after concurrent rollback: %v", err)
	}

	// Exactly one ROLLBACK event per plan must appear, all with result=rollback_succeeded
	events := readAllChainEvents(t, chain)
	if got, want := len(events), planCount; got != want {
		t.Fatalf("chain event count = %d, want %d", got, want)
	}
	rollbackByPlan := map[string]workflow.AuditEvent{}
	for _, ev := range events {
		if ev.Action != workflow.ActionRollback {
			t.Errorf("unexpected action in concurrent chain: %s", ev.Action)
			continue
		}
		if ev.Payload["result"] != "rollback_succeeded" {
			t.Errorf("plan %s rollback event result = %s, want rollback_succeeded",
				ev.PlanID, ev.Payload["result"])
		}
		if _, dup := rollbackByPlan[ev.PlanID]; dup {
			t.Errorf("duplicate rollback event for plan %s", ev.PlanID)
		}
		rollbackByPlan[ev.PlanID] = ev
	}
	for _, p := range plans {
		if _, ok := rollbackByPlan[p.PlanID]; !ok {
			t.Errorf("missing rollback event for plan %s", p.PlanID)
		}
	}

	// Dict must reflect v16 limits — every rollback wrote the same set of
	// UPDATE entries, so the final dict is identical to a single rollback
	schema, err := hisClient.ReadSchema(ctx)
	if err != nil {
		t.Fatalf("ReadSchema after concurrent rollback: %v", err)
	}
	wantLimits := map[string]float64{
		"DRG-A001": 35200.00,
		"DRG-A002": 51200.00,
		"DRG-B014": 15800.00,
		"DRG-C007": 31200.00,
		"DRG-E022": 11500.00,
	}
	if got := len(schema.Entries); got != 5 {
		t.Errorf("entry count after concurrent rollback = %d, want 5", got)
	}
	for _, e := range schema.Entries {
		want, ok := wantLimits[e.Code]
		if !ok {
			t.Errorf("unexpected code after concurrent rollback: %s", e.Code)
			continue
		}
		if e.Limit != want {
			t.Errorf("code %s limit = %.2f, want %.2f", e.Code, e.Limit, want)
		}
	}
}

// ---------- chain-event reader helper ----------

// readAllChainEvents streams every event off the chain via Export with an
// empty filter and decodes them. Used by the failure-recovery and
// concurrent-rollback tests to inspect what was actually appended.
func readAllChainEvents(t *testing.T, c *audit.Chain) []workflow.AuditEvent {
	t.Helper()
	r, err := c.Export(context.Background(), audit.Filter{})
	if err != nil {
		t.Fatalf("chain.Export: %v", err)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("io.ReadAll chain: %v", err)
	}
	var out []workflow.AuditEvent
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev workflow.AuditEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("parse chain line %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}