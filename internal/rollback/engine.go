// Package rollback implements one-click rollback for an Applied RulePlan.
// When the medical-insurance office or a reviewer detects an issue in the
// newly-applied DRG/DIP rule set, they trigger Rollback to restore the
// previous version's data on the HIS dictionary table. The mechanism is:
//
//	1. Load the Plan (must be in StatusApplied)
//	2. Read the current DictSchema snapshot from the HIS backend
//	3. InferReverseSQL: compute the deterministic diff between plan.Spec
//	   and the live dict. Three categories:
//
//	   - INSERT for newly-added rules       → DELETE on rollback
//	   - DELETE for rules removed in v_new  → INSERT on rollback
//	   - UPDATE for weight / limit changes  → UPDATE back to prev_limit
//
//	4. Apply the rollback to his_dict via DictClient.ApplyUpdate (a single
//	   transaction with dual-write to his_audit_log)
//	5. Flip Plan.Status from Applied → RolledBack (durable)
//	6. Append a ROLLBACK audit event to the SHA256 chain (or ROLLBACK_FAILED
//	   on error)
//
// Source: bytebase's reverse-Apply semantic — wrapping the Plan's
// `rollback_sql` with the same tx boundary as the original Apply. We drop
// the literal rollback_sql string from the spec (it's still carried in the
// Plan row) and synthesise the reverse SQL ourselves so the rollback
// doesn't depend on whoever authored the spec having written it correctly.
package rollback

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/example/insurance-rule-rollout/internal/integration/his_dict"
	"github.com/example/insurance-rule-rollout/internal/workflow"
)

// PlanStore is the persistence surface RollbackEngine reads / writes. It is
// satisfied by *store.Store via a thin adapter (see engine_test.go) so the
// rollback package stays decoupled from the concrete SQLite driver.
type PlanStore interface {
	// LoadPlan returns the full workflow.Plan (with SpecJSON parsed upstream
	// for the caller, but the rollback engine only reads the raw JSON).
	LoadPlan(ctx context.Context, planID string) (workflow.Plan, error)

	// SavePlanStatus persists the Status / LastAction / LastActionAt /
	// UpdatedAt fields for an existing Plan.
	SavePlanStatus(ctx context.Context, planID string, status workflow.PlanStatus, draft bool, lastAction workflow.IssueAction, lastActionAt, updatedAt time.Time) error
}

// Chain is the audit-event appender. *audit.Chain satisfies it; the
// interface narrows the surface so tests can stub it with a mock that
// records events in memory.
type Chain interface {
	Append(ctx context.Context, event workflow.AuditEvent) error
}

// RollbackEngine owns the four collaborators needed for one-click rollback:
// DictClient (HIS adapter), PlanStore (workflow persistence), Chain
// (SHA256 audit), and optional time / id sources for deterministic tests.
type RollbackEngine struct {
	Dict  his_dict.DictClient
	Store PlanStore
	Chain Chain

	// mu serialises concurrent Rollback invocations across goroutines so
	// the SHA256 chain (whose lastHashCache is itself not goroutine-safe)
	// sees exactly one Append at a time and Verify never reports a broken
	// link caused by an interleaved partial write.
	mu sync.Mutex

	// Now returns the wall-clock time used for AuditEvent.Timestamp.
	// Defaults to time.Now().UTC() when nil.
	Now func() time.Time

	// NewID returns a fresh event_id for AuditEvent.EventID. Defaults to
	// uuid.NewString() when nil.
	NewID func() string

	// RollbackComment is appended to the ROLLBACK audit event's Payload so
	// the 飞检 inspector sees a single, deterministic comment for the
	// rollback batch (per Plan). Useful when the same plan is rolled back
	// multiple times during a release window.
	RollbackComment string
}

// now returns the configured wall-clock (defaults to UTC time.Now).
func (re *RollbackEngine) now() time.Time {
	if re.Now != nil {
		return re.Now()
	}
	return time.Now().UTC()
}

// newID returns a fresh event_id (defaults to uuid v4).
func (re *RollbackEngine) newID() string {
	if re.NewID != nil {
		return re.NewID()
	}
	return uuid.NewString()
}

// ReverseOpType categorises one inferred reverse operation.
type ReverseOpType string

const (
	// ReverseOpDelete corresponds to a rule that was newly added in the
	// new version. Forward operation was INSERT; rollback is DELETE.
	ReverseOpDelete ReverseOpType = "delete"

	// ReverseOpUpdate corresponds to a rule whose weight / limit changed
	// in the new version. Forward operation was UPDATE; rollback is UPDATE
	// back to the prev_limit carried in the rule's spec entry.
	ReverseOpUpdate ReverseOpType = "update"

	// ReverseOpInsert corresponds to a rule that was removed in the new
	// version. Forward operation was DELETE; rollback is INSERT using the
	// best-effort name / weight / current limit captured at rollback time.
	ReverseOpInsert ReverseOpType = "insert"
)

// ReverseOp is one row in the inferred reverse operation list. The Engine
// uses these to build the DictEntry slice fed to DictClient.ApplyUpdate;
// InferReverseSQL also emits a human-readable SQL form for the audit log
// and for inspector displays.
type ReverseOp struct {
	Type     ReverseOpType
	Code     string
	Name     string  // INSERT only — best-effort from current dict row
	Weight   float64 // INSERT only
	Limit    float64 // INSERT / UPDATE — target limit after rollback
	OldLimit float64 // UPDATE only — limit in the applied version (for diff)
	Reason   string  // human-readable explanation ("modified_limit", "new_rule", "removed_rule")
}

// InferReverseSQL inspects the Plan's Rules (re-parsed from SpecJSON) and
// the currentDict snapshot and returns the deterministic reverse SQL that
// undoes the new version's changes.
//
// Categories:
//
//	1. modify: rule.PrevLimit > 0 AND rule is present in currentDict
//	   → UPDATE his_dict SET limit = rule.PrevLimit, version = plan.PrevVersionID
//	2. add:    rule is NOT in currentDict
//	   → DELETE FROM his_dict WHERE code = rule.Code
//	3. remove: currentDict entry NOT in plan.Spec.Rules
//	   → INSERT INTO his_dict (code, name, weight, limit, ...) VALUES (...)
//
// The returned SQL is dialect-neutral with `?` placeholders for values
// where it makes sense. Code literals are inlined (they come from a
// trusted spec, not user input).
//
// Returns ("-- no reverse ops needed", nil) when the inferred diff is empty
// — the caller can short-circuit and just flip the status without writing.
func InferReverseSQL(plan workflow.Plan, currentDict []his_dict.DictEntry) (string, error) {
	ops, err := InferReverseOps(plan, currentDict)
	if err != nil {
		return "", err
	}
	if len(ops) == 0 {
		return "-- no reverse ops needed", nil
	}
	return formatSQL(ops, plan), nil
}

// InferReverseOps is the structured counterpart of InferReverseSQL.
// Returns one ReverseOp per inferred diff row. Use this when the caller
// wants to feed DictClient.ApplyUpdate (which takes []DictEntry, not raw
// SQL) rather than emitting SQL.
func InferReverseOps(plan workflow.Plan, currentDict []his_dict.DictEntry) ([]ReverseOp, error) {
	if plan.SpecJSON == "" {
		return nil, fmt.Errorf("rollback: plan %s has empty spec_json; cannot infer reverse ops", plan.PlanID)
	}
	var spec workflow.RulePlanSpec
	if err := json.Unmarshal([]byte(plan.SpecJSON), &spec); err != nil {
		return nil, fmt.Errorf("rollback: parse plan %s spec_json: %w", plan.PlanID, err)
	}
	if len(spec.Rules) == 0 {
		return nil, fmt.Errorf("rollback: plan %s spec has no rules; cannot infer reverse ops", plan.PlanID)
	}

	currentByCode := make(map[string]his_dict.DictEntry, len(currentDict))
	for _, e := range currentDict {
		if e.Code == "" {
			continue
		}
		currentByCode[e.Code] = e
	}

	var ops []ReverseOp
	seenCodes := make(map[string]bool, len(spec.Rules))

	for _, r := range spec.Rules {
		if r.Code == "" {
			return nil, fmt.Errorf("rollback: plan %s has empty rule code", plan.PlanID)
		}
		seenCodes[r.Code] = true
		cur, inDict := currentByCode[r.Code]
		switch {
		case !inDict:
			// rule was newly added in the new version → reverse is DELETE
			ops = append(ops, ReverseOp{
				Type:   ReverseOpDelete,
				Code:   r.Code,
				Reason: "new_rule_in_v_new",
			})
		case r.PrevLimit > 0 && cur.Limit != r.PrevLimit:
			// rule was modified (limit changed) → reverse is UPDATE
			ops = append(ops, ReverseOp{
				Type:     ReverseOpUpdate,
				Code:     r.Code,
				Limit:    r.PrevLimit,
				OldLimit: cur.Limit,
				Reason:   "modified_limit",
			})
		default:
			// rule unchanged (or has no PrevLimit to restore). Skip — no
			// reverse op needed.
		}
	}

	// rules in currentDict but NOT in plan.Spec.Rules → they were deleted
	// in the new version → reverse is INSERT (best-effort with current row
	// values; we don't have the prev weight / limit cached so we restore
	// the limit that was live before the rollback, which is the only data
	// we have on hand)
	for code, cur := range currentByCode {
		if seenCodes[code] {
			continue
		}
		ops = append(ops, ReverseOp{
			Type:   ReverseOpInsert,
			Code:   code,
			Name:   cur.Name,
			Weight: cur.Weight,
			Limit:  cur.Limit,
			Reason: "removed_rule_in_v_new",
		})
	}

	return ops, nil
}

// formatSQL turns a []ReverseOp into a multi-line SQL string for the audit
// log. Uses `?` placeholders (SQLite / MySQL) — Postgres callers can swap
// to $1..$N themselves.
func formatSQL(ops []ReverseOp, plan workflow.Plan) string {
	var b strings.Builder
	for i, op := range ops {
		switch op.Type {
		case ReverseOpDelete:
			fmt.Fprintf(&b, "-- %d. delete (was new rule in %s)\n", i+1, plan.VersionID)
			fmt.Fprintf(&b, "DELETE FROM his_dict WHERE code = '%s';\n", op.Code)
		case ReverseOpUpdate:
			fmt.Fprintf(&b, "-- %d. update limit back (was %.2f, restoring to %.2f, version=%s)\n",
				i+1, op.OldLimit, op.Limit, plan.PrevVersionID)
			fmt.Fprintf(&b,
				"UPDATE his_dict SET limit = %.2f, version = '%s' WHERE code = '%s';\n",
				op.Limit, plan.PrevVersionID, op.Code)
		case ReverseOpInsert:
			fmt.Fprintf(&b, "-- %d. insert (was removed in %s, restoring current row)\n", i+1, plan.VersionID)
			fmt.Fprintf(&b,
				"INSERT INTO his_dict (code, name, weight, limit, effective_date, version) VALUES ('%s', '%s', %.3f, %.2f, '%s', '%s');\n",
				op.Code, op.Name, op.Weight, op.Limit, plan.EffectiveDate, plan.PrevVersionID)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// buildRollbackEntries turns the inferred ops into a []DictEntry suitable
// for DictClient.ApplyUpdate. Modify / Insert ops produce an entry row;
// Delete ops produce no entry (the row should disappear from the dict, not
// be upserted).
//
// Modify rows require a name / weight / effective_date to satisfy
// validateEntries (UPSERT columns are all NOT NULL). We copy these from
// the currentDict row so the dict stays byte-identical except for the
// limit field we are restoring.
func buildRollbackEntries(plan workflow.Plan, ops []ReverseOp, currentDict []his_dict.DictEntry) []his_dict.DictEntry {
	currentByCode := make(map[string]his_dict.DictEntry, len(currentDict))
	for _, e := range currentDict {
		if e.Code == "" {
			continue
		}
		currentByCode[e.Code] = e
	}
	var entries []his_dict.DictEntry
	for _, op := range ops {
		switch op.Type {
		case ReverseOpUpdate:
			cur := currentByCode[op.Code]
			entries = append(entries, his_dict.DictEntry{
				Code:          op.Code,
				Name:          cur.Name,
				Weight:        cur.Weight,
				Limit:         op.Limit,
				EffectiveDate: cur.EffectiveDate,
				Version:       plan.PrevVersionID,
			})
		case ReverseOpInsert:
			entries = append(entries, his_dict.DictEntry{
				Code:          op.Code,
				Name:          op.Name,
				Weight:        op.Weight,
				Limit:         op.Limit,
				EffectiveDate: plan.EffectiveDate,
				Version:       plan.PrevVersionID,
			})
		case ReverseOpDelete:
			// no entry — nothing to upsert; the row should disappear
		}
	}
	return entries
}

// Rollback performs the one-click rollback. The actor and comment are
// recorded in the audit event so the 飞检 inspector knows who triggered
// the rollback and why.
//
// Sequence:
//  1. LoadPlan (NotFound / TerminalStateError if not in Applied).
//  2. ReadSchema — if it errors, append ROLLBACK_FAILED and return.
//  3. InferReverseOps — if it errors, append ROLLBACK_FAILED and return.
//  4. buildRollbackEntries — feed to DictClient.ApplyUpdate.
//  5. ApplyUpdate — single transaction; any error → ROLLBACK_FAILED, Plan
//     status stays Applied, dict state unchanged.
//  6. SavePlanStatus(applied → rolled_back).
//  7. Append ROLLBACK audit event (with ops_count + comment).
//
// Returns nil on success; on failure the Plan status is NOT mutated and a
// ROLLBACK_FAILED event is appended before returning the underlying error.
func (re *RollbackEngine) Rollback(ctx context.Context, planID, actor, comment string) error {
	// Serialise concurrent Rollback calls so the SHA256 chain (whose
	// lastHashCache is not goroutine-safe) never sees an interleaved
	// partial write. Multi-plan rollback callers (e.g. cmd/demo's "全部回滚"
	// scenario) therefore observe deterministic per-plan ordering on the
	// chain.
	re.mu.Lock()
	defer re.mu.Unlock()

	if planID == "" {
		return errors.New("rollback: planID required")
	}
	if actor == "" {
		return errors.New("rollback: actor required")
	}
	if re.Dict == nil {
		return errors.New("rollback: DictClient is required")
	}
	if re.Store == nil {
		return errors.New("rollback: PlanStore is required")
	}
	if re.Chain == nil {
		return errors.New("rollback: Chain is required")
	}

	// 1. Load plan
	plan, err := re.Store.LoadPlan(ctx, planID)
	if err != nil {
		return fmt.Errorf("rollback: load plan %s: %w", planID, err)
	}

	// Status guard: rollback is only legal from Applied. Other statuses
	// (Draft / Pending / Approved / Rejected / RolledBack) all reject.
	if plan.Status != workflow.StatusApplied {
		return &workflow.TerminalStateError{
			From:   plan.Status,
			Action: workflow.ActionRollback,
		}
	}

	now := re.now()

	// 2. Read current schema
	schema, err := re.Dict.ReadSchema(ctx)
	if err != nil {
		return re.recordFailure(ctx, plan, actor, comment, now,
			"read_schema_failed", fmt.Sprintf("read schema: %v", err))
	}

	// 3. Infer reverse ops
	ops, err := InferReverseOps(plan, schema.Entries)
	if err != nil {
		return re.recordFailure(ctx, plan, actor, comment, now,
			"infer_reverse_ops_failed", fmt.Sprintf("infer reverse ops: %v", err))
	}

	// 4 + 5. Apply the rollback
	entries := buildRollbackEntries(plan, ops, schema.Entries)
	if len(entries) > 0 {
		if err := re.Dict.ApplyUpdate(ctx, entries); err != nil {
			return re.recordFailure(ctx, plan, actor, comment, now,
				"apply_update_failed", fmt.Sprintf("apply update: %v", err))
		}
	}

	// 6. Flip status Applied → RolledBack
	if err := re.Store.SavePlanStatus(ctx, planID,
		workflow.StatusRolledBack, plan.Draft,
		workflow.ActionRollback, now, now); err != nil {
		// dict was already rolled back; we still want a trace. Record
		// the partial-success event and return the error.
		_ = re.Chain.Append(ctx, workflow.AuditEvent{
			EventID:   re.newID(),
			PlanID:    planID,
			Actor:     actor,
			Action:    workflow.ActionRollback,
			Payload: map[string]string{
				"from_status": string(workflow.StatusApplied),
				"to_status":   string(workflow.StatusRolledBack),
				"comment":     comment,
				"result":      "status_update_failed_dict_already_reverted",
				"ops_count":   fmt.Sprintf("%d", len(ops)),
				"warning":     "dict was reverted but plan status update failed; manual reconciliation required",
			},
			Timestamp: now,
		})
		return fmt.Errorf("rollback: dict reverted but status flip failed: %w", err)
	}

	// 7. Append ROLLBACK audit event
	rollbackComment := re.RollbackComment
	if comment != "" {
		rollbackComment = comment
	}
	if err := re.Chain.Append(ctx, workflow.AuditEvent{
		EventID:   re.newID(),
		PlanID:    planID,
		Actor:     actor,
		Action:    workflow.ActionRollback,
		Payload: map[string]string{
			"from_status":   string(workflow.StatusApplied),
			"to_status":     string(workflow.StatusRolledBack),
			"comment":       rollbackComment,
			"result":        "rollback_succeeded",
			"ops_count":     fmt.Sprintf("%d", len(ops)),
			"prev_version":  plan.PrevVersionID,
			"new_version":   plan.VersionID,
			"update_count":  fmt.Sprintf("%d", countOpTypes(ops, ReverseOpUpdate)),
			"delete_count":  fmt.Sprintf("%d", countOpTypes(ops, ReverseOpDelete)),
			"insert_count":  fmt.Sprintf("%d", countOpTypes(ops, ReverseOpInsert)),
		},
		Timestamp: now,
	}); err != nil {
		return fmt.Errorf("rollback: append audit event: %w", err)
	}

	return nil
}

// recordFailure appends a ROLLBACK_FAILED audit event and returns a
// wrapped error. The Plan status is NOT mutated; the dict transaction
// (if any was started) is already rolled back by DictClient.ApplyUpdate
// itself.
func (re *RollbackEngine) recordFailure(ctx context.Context, plan workflow.Plan,
	actor, comment string, now time.Time, reason, detail string) error {
	_ = re.Chain.Append(ctx, workflow.AuditEvent{
		EventID:   re.newID(),
		PlanID:    plan.PlanID,
		Actor:     actor,
		Action:    workflow.ActionRollback,
		Payload: map[string]string{
			"from_status": string(plan.Status),
			"to_status":   string(plan.Status),
			"comment":     comment,
			"result":      "rollback_failed",
			"reason":      reason,
			"detail":      detail,
		},
		Timestamp: now,
	})
	return fmt.Errorf("rollback: plan %s failed (%s): %s", plan.PlanID, reason, detail)
}

// countOpTypes returns the number of ops with the given type.
func countOpTypes(ops []ReverseOp, t ReverseOpType) int {
	n := 0
	for _, op := range ops {
		if op.Type == t {
			n++
		}
	}
	return n
}