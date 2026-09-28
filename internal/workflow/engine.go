// Engine for the workflow package. In-memory map is the cache; the SQLite
// Store (Task 3) is the durable side. Task 4 introduces a transactional
// workflow.Store interface and replaces the per-call writes with WithTx.
//
// Public API surface (extracted from bytebase frontend/src/lib/plan/workflow.ts
// + backend/component/review/workflow.go, narrowed to the insurance domain):
//
//   - CreatePlanWithDraftReview  → matches frontend createPlanWithDraftReview
//   - SubmitDraftReview          → matches frontend submitDraftReview (flip draft flag only)
//   - ReviewIssue                → Approve / Reject / Request transition
//   - Apply                      → Approved → Applied transition
//   - Rollback                   → Applied → RolledBack transition
//   - GetPlan                    → lookup by planID
//
// Engine.SetStore injects a persistence sink; when nil the engine is purely
// in-memory (Task 2 behaviour). When non-nil every state transition also
// writes the row / audit event to the store. AuditEvent prev_hash / event_hash
// are populated by internal/audit in Task 10 — Task 3 leaves them empty so the
// demo shows the raw store output without coupling to the chain code.
package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// AuditWriter is the minimal persistence sink Engine needs. The concrete
// SQLite-backed implementation lives in internal/store.Store (Task 3) and is
// wired via SetStore. Task 4 swaps this for the transactional workflow.Store
// interface.
//
// Methods mirror the workflow.Plan / Issue / AuditEvent writes the engine
// must produce on each transition. Returning an error rolls the engine state
// back (the in-memory map is not mutated until after a successful write).
type AuditWriter interface {
	InsertPlan(ctx context.Context, row PlanRow) error
	UpdatePlanStatus(ctx context.Context, planID string, status PlanStatus, draft bool, lastAction IssueAction, lastActionAt, updatedAt time.Time) error
	InsertIssue(ctx context.Context, row IssueRow) error
	InsertAuditEvent(ctx context.Context, row AuditEventRow) error
}

// PlanRow, IssueRow, AuditEventRow mirror the persistence shape. Defined in
// the workflow package (not store) so the engine can write without importing
// internal/store and creating a cycle. internal/store.Store implements this
// surface verbatim.
type PlanRow struct {
	PlanID        string
	VersionID     string
	RuleSet       string
	EffectiveDate string
	SpecJSON      string
	PrevVersionID string
	RollbackSQL   string
	Status        PlanStatus
	Draft         bool
	CreatedBy     string
	LastAction    IssueAction
	LastActionAt  time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type IssueRow struct {
	IssueID   string
	PlanID    string
	Action    IssueAction
	Actor     string
	Comment   string
	CreatedAt time.Time
}

type AuditEventRow struct {
	EventID   string
	PlanID    string
	Actor     string
	Action    IssueAction
	Payload   map[string]string
	Timestamp time.Time
	PrevHash  string
	EventHash string
}

// eventIDGen is overridden by SetStore so the engine uses the same id-gen
// scheme as the persistence layer (uuidv4 by default).
var _ = uuid.NewString

// DefaultReviewer is the actor recorded on Plan creation when no createdBy
// is provided. Matches bytebase's "no-actor fallback" pattern.
const DefaultReviewer = "system:rollout"

// Engine owns the in-memory Plan store. The transactional gate (Task 4) is
// held in tStore; when non-nil, ReviewIssue and Apply route through
// WithTx → LockPlan → UpdatePlanStatus → InsertIssue → InsertAuditEvent so
// concurrent reviewers can't observe an intermediate state. Methods that
// don't need row-level locking (CreatePlanWithDraftReview,
// SubmitDraftReview, Rollback) keep using the per-call AuditWriter for
// simplicity — those paths don't share state with other transitions.
//
// The in-memory map is the engine's own cache and is always authoritative
// from the caller's perspective. The tStore / store sinks are projections of
// that cache to durable storage; a failure in either sink causes the engine
// to return the error to the caller WITHOUT updating its in-memory map
// (see ReviewIssue / Apply implementations).
//
// Methods are safe for concurrent use via the inner mutex.
type Engine struct {
	mu     sync.RWMutex
	plans  map[string]Plan
	now    func() time.Time
	idGen  func() string
	store  AuditWriter // per-call sink: InsertPlan / InsertIssue / UpdatePlanStatus / InsertAuditEvent
	tStore Store       // transactional sink: WithTx + LockPlan + ... (Task 4)
}

// NewEngine creates a fresh Engine with default uuid / time-clock dependencies.
// Tests can overwrite the idGen / now closures to inject deterministic IDs and
// timestamps.
func NewEngine() *Engine {
	return &Engine{
		plans: make(map[string]Plan),
		now:   time.Now,
		idGen: func() string { return uuid.NewString() },
	}
}

// SetClock allows tests to pin the engine's clock to a deterministic time.
func (e *Engine) SetClock(now func() time.Time) { e.now = now }

// SetIDGen allows tests to inject deterministic IDs (e.g. "plan-001").
func (e *Engine) SetIDGen(gen func() string) { e.idGen = gen }

// SetStore wires a per-call persistence sink (AuditWriter). When non-nil
// every state transition also writes the row / audit event to the store.
// A nil store leaves the engine in-memory (Task 2 behaviour).
func (e *Engine) SetStore(s AuditWriter) { e.store = s }

// SetTxStore wires a transactional persistence sink (workflow.Store). When
// non-nil ReviewIssue and Apply route through WithTx so concurrent
// reviewers serialise on the row-level lock. A nil tStore falls back to
// the per-call AuditWriter path — Task 2 / 3 behaviour. Engines typically
// have BOTH SetStore and SetTxStore wired (AuditWriter handles CreatePlan
// and SubmitDraftReview; Store handles ReviewIssue and Apply).
func (e *Engine) SetTxStore(s Store) { e.tStore = s }

// persist writes the supplied row to the engine's store when one is wired.
// It also appends an AuditEvent capturing the transition. Safe to call with
// a nil store (returns nil immediately).
func (e *Engine) persist(ctx context.Context, p Plan, action IssueAction, actor, comment string, payload map[string]string) error {
	if e.store == nil {
		return nil
	}
	if payload == nil {
		payload = map[string]string{}
	}
	ev := AuditEventRow{
		EventID:   e.idGen(),
		PlanID:    p.PlanID,
		Actor:     actor,
		Action:    action,
		Payload:   payload,
		Timestamp: p.LastActionAt,
	}
	if err := e.store.InsertAuditEvent(ctx, ev); err != nil {
		return fmt.Errorf("engine: persist audit event for %s: %w", p.PlanID, err)
	}
	return nil
}

// CreatePlanWithDraftReview creates a new Plan in Draft with the draft flag
// set true. Mirrors bytebase frontend createPlanWithDraftReview: the Plan is
// only useful after SubmitDraftReview flips Draft off.
//
// spec must be valid (VersionID, RuleSet, EffectiveDate non-empty, ≥1 Rule);
// validation is light here — full field validation lives in Task 24 +
// integration/insurance_platform.
func (e *Engine) CreatePlanWithDraftReview(spec RulePlanSpec, createdBy string) (Plan, error) {
	if createdBy == "" {
		createdBy = DefaultReviewer
	}
	if spec.VersionID == "" || spec.RuleSet == "" || spec.EffectiveDate == "" {
		return Plan{}, fmt.Errorf("rule plan spec missing required fields: version_id/rule_set/effective_date all required")
	}
	if len(spec.Rules) == 0 {
		return Plan{}, fmt.Errorf("rule plan spec has no rules; at least one Rule entry is required")
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return Plan{}, fmt.Errorf("marshal rule plan spec: %w", err)
	}

	now := e.now()
	plan := Plan{
		PlanID:        e.idGen(),
		VersionID:     spec.VersionID,
		RuleSet:       spec.RuleSet,
		EffectiveDate: spec.EffectiveDate,
		SpecJSON:      string(specJSON),
		PrevVersionID: spec.PrevVersionID,
		RollbackSQL:   spec.RollbackSQL,
		Status:        StatusDraft,
		Draft:         true,
		CreatedBy:     createdBy,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if e.store != nil {
		row := PlanRow{
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
			CreatedAt:     plan.CreatedAt,
			UpdatedAt:     plan.UpdatedAt,
		}
		if err := e.store.InsertPlan(context.Background(), row); err != nil {
			return Plan{}, fmt.Errorf("engine: persist plan: %w", err)
		}
		// AuditEvent for plan creation (action=CreatePlan — synthesized action
		// since the state machine doesn't define a "create" transition).
		ev := AuditEventRow{
			EventID:   e.idGen(),
			PlanID:    plan.PlanID,
			Actor:     createdBy,
			Action:    IssueAction("create_plan"),
			Payload:   map[string]string{"version_id": plan.VersionID, "rule_set": plan.RuleSet, "comment": "rule plan created"},
			Timestamp: now,
		}
		if err := e.store.InsertAuditEvent(context.Background(), ev); err != nil {
			return Plan{}, fmt.Errorf("engine: persist create audit: %w", err)
		}
	}
	e.mu.Lock()
	e.plans[plan.PlanID] = plan
	e.mu.Unlock()
	return plan, nil
}

// SubmitDraftReview advances Plan.Status from Draft → Pending and flips the
// Draft bool off (legacy mirror of bytebase's payload.draft flag, kept for
// spec compatibility). The transition table entry (Draft, SubmitDraft) →
// Pending is the source of truth for this state change; the legacy bool flag
// is only consulted by ReviewIssue's pre-check.
//
// Per spec and bytebase frontend/src/lib/plan/workflow.ts:92:
//
//	"Submitting only flips the draft flag." (legacy comment; the Go state
//	 machine additionally records the Draft → Pending transition.)
func (e *Engine) SubmitDraftReview(planID string) (Plan, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	plan, ok := e.plans[planID]
	if !ok {
		return Plan{}, &NotFoundError{PlanID: planID}
	}
	next, err := NextStatus(plan.Status, ActionSubmitDraft)
	if err != nil {
		return Plan{}, err
	}
	plan.Status = next
	plan.Draft = false
	plan.LastAction = ActionSubmitDraft
	plan.LastActionAt = e.now()
	plan.UpdatedAt = plan.LastActionAt
	if e.store != nil {
		now := plan.LastActionAt
		if err := e.store.UpdatePlanStatus(context.Background(), planID, plan.Status, plan.Draft, plan.LastAction, now, now); err != nil {
			return Plan{}, fmt.Errorf("engine: persist submit draft: %w", err)
		}
		ev := AuditEventRow{
			EventID:   e.idGen(),
			PlanID:    planID,
			Actor:     "yang_jb",
			Action:    ActionSubmitDraft,
			Payload:   map[string]string{"from_status": string(StatusDraft), "to_status": string(StatusPending), "comment": "submit draft for multi-department review"},
			Timestamp: now,
		}
		if err := e.store.InsertAuditEvent(context.Background(), ev); err != nil {
			return Plan{}, fmt.Errorf("engine: persist submit draft audit: %w", err)
		}
	}
	e.plans[planID] = plan
	return plan, nil
}

// ReviewIssue is the main interactive transition entry point. Approve /
// Reject / Request are the supported review actions. Self-loop guard for
// (Pending, Request x2) lives in NextStatusForPlan.
//
// Persistence path:
//   - When Engine.tStore is set (Task 4 transactional mode), the in-memory
//     cache is checked first for the Draft flag, then WithTx wraps a
//     LockPlan + NextStatusForPlan + UpdatePlanStatus + InsertIssue +
//     InsertAuditEvent. Any failure rolls back the transaction and the
//     in-memory cache is NOT mutated.
//   - When Engine.tStore is nil, falls back to the Task 3 AuditWriter path
//     (per-call UpdatePlanStatus / InsertIssue / InsertAuditEvent).
func (e *Engine) ReviewIssue(planID string, action IssueAction, actor, comment string) (Plan, error) {
	if action != ActionApprove && action != ActionReject && action != ActionRequest {
		return Plan{}, &InvalidActionError{From: zeroStatus, Action: action}
	}
	if actor == "" {
		return Plan{}, fmt.Errorf("reviewer actor is required")
	}

	if e.tStore != nil {
		return e.reviewIssueTx(planID, action, actor, comment)
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	plan, ok := e.plans[planID]
	if !ok {
		return Plan{}, &NotFoundError{PlanID: planID}
	}
	if plan.IsDraft() {
		return Plan{}, &ErrDraftApprove{
			IllegalTransitionError: IllegalTransitionError{
				From:     plan.Status,
				Action:   action,
				Expected: "Draft plans cannot be reviewed; call SubmitDraftReview first.",
			},
		}
	}
	fromStatus := plan.Status
	next, err := NextStatusForPlan(plan, action)
	if err != nil {
		return Plan{}, err
	}
	// Double insurance: terminal-state guard after NextStatusForPlan. The
	// state machine's typedErrorFor already rejects every terminal-state
	// pair (Task 19), so this branch should never fire in practice — but
	// if a future change ever lets a terminal plan transition, this is
	// the last line of defence before we mutate the in-memory cache.
	if plan.Status.IsTerminal() {
		return Plan{}, &TerminalStateError{From: plan.Status, Action: action}
	}
	now := e.now()
	plan.Status = next
	plan.LastAction = action
	plan.LastActionAt = now
	plan.UpdatedAt = now
	if e.store != nil {
		if err := e.store.UpdatePlanStatus(context.Background(), planID, plan.Status, plan.Draft, plan.LastAction, now, now); err != nil {
			return Plan{}, fmt.Errorf("engine: persist review status: %w", err)
		}
		if err := e.store.InsertIssue(context.Background(), IssueRow{
			IssueID:   e.idGen(),
			PlanID:    planID,
			Action:    action,
			Actor:     actor,
			Comment:   comment,
			CreatedAt: now,
		}); err != nil {
			return Plan{}, fmt.Errorf("engine: persist review issue: %w", err)
		}
		ev := AuditEventRow{
			EventID:   e.idGen(),
			PlanID:    planID,
			Actor:     actor,
			Action:    action,
			Payload:   map[string]string{"from_status": string(fromStatus), "to_status": string(next), "comment": comment},
			Timestamp: now,
		}
		if err := e.store.InsertAuditEvent(context.Background(), ev); err != nil {
			return Plan{}, fmt.Errorf("engine: persist review audit: %w", err)
		}
	}
	e.plans[planID] = plan
	return plan, nil
}

// reviewIssueTx is the transactional ReviewIssue path. Steps:
//  1. Acquire in-memory read lock; look up Plan + check Draft flag.
//  2. Outside the in-memory lock, run WithTx on the persistence store:
//     LockPlan → NextStatusForPlan → TxUpdatePlanStatus → TxInsertIssue →
//     TxInsertAuditEvent. Any error returns early (WithTx rolls back).
//  3. On commit success, re-acquire the in-memory write lock and update the
//     cache so subsequent reads see the new state.
//
// Step 2 runs WITHOUT the in-memory lock so the SQLite write doesn't block
// unrelated reads. The plan_id passed to LockPlan is the same one we
// validated in step 1; if a concurrent caller mutated the row out from
// under us, TxUpdatePlanStatus will hit ErrPlanNotFound (the row was
// re-created with a different plan_id by CreatePlanWithDraftReview) and
// the transaction will roll back. We re-check the cached state under the
// write lock at step 3 to avoid double-applying.
func (e *Engine) reviewIssueTx(planID string, action IssueAction, actor, comment string) (Plan, error) {
	e.mu.RLock()
	plan, ok := e.plans[planID]
	e.mu.RUnlock()
	if !ok {
		return Plan{}, &NotFoundError{PlanID: planID}
	}
	if plan.IsDraft() {
		return Plan{}, &ErrDraftApprove{
			IllegalTransitionError: IllegalTransitionError{
				From:     plan.Status,
				Action:   action,
				Expected: "Draft plans cannot be reviewed; call SubmitDraftReview first.",
			},
		}
	}
	fromStatus := plan.Status
	now := e.now()
	var (
		next      PlanStatus
		eventHash string // optional prev_hash for chain (filled by audit pkg in Task 10)
	)
	txErr := e.tStore.WithTx(context.Background(), func(tx Tx) error {
		locked, err := e.tStore.LockPlan(context.Background(), tx, planID)
		if err != nil {
			return err
		}
		// Re-check the locked row's state — another tx may have committed
		// between our snapshot read and the LockPlan call. The locked
		// row's Status is the source of truth.
		if locked.IsDraft() {
			return &ErrDraftApprove{
				IllegalTransitionError: IllegalTransitionError{
					From:     locked.Status,
					Action:   action,
					Expected: "Draft plans cannot be reviewed; call SubmitDraftReview first.",
				},
			}
		}
		// Compute the next status against the locked snapshot.
		nextStatus, nerr := NextStatusForPlan(locked, action)
		if nerr != nil {
			return nerr
		}
		// Double insurance: terminal-state guard after NextStatusForPlan
		// (Task 19). Mirrors the per-call-path check — the state machine
		// already rejects terminal-state pairs via typedErrorFor, but
		// the engine layer also blocks any accidental terminal transition
		// so the in-memory cache is never promoted for a terminal plan.
		if locked.Status.IsTerminal() {
			return &TerminalStateError{From: locked.Status, Action: action}
		}
		next = nextStatus
		if err := e.tStore.TxUpdatePlanStatus(context.Background(), tx, planID, next, locked.Draft, action, now, now); err != nil {
			return fmt.Errorf("engine: persist review status: %w", err)
		}
		if err := e.tStore.TxInsertIssue(context.Background(), tx, IssueRow{
			IssueID:   e.idGen(),
			PlanID:    planID,
			Action:    action,
			Actor:     actor,
			Comment:   comment,
			CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("engine: persist review issue: %w", err)
		}
		ev := AuditEventRow{
			EventID:   e.idGen(),
			PlanID:    planID,
			Actor:     actor,
			Action:    action,
			Payload:   map[string]string{"from_status": string(fromStatus), "to_status": string(next), "comment": comment},
			Timestamp: now,
			EventHash: eventHash,
		}
		if err := e.tStore.TxInsertAuditEvent(context.Background(), tx, ev); err != nil {
			return fmt.Errorf("engine: persist review audit: %w", err)
		}
		return nil
	})
	if txErr != nil {
		return Plan{}, txErr
	}

	// Commit succeeded — promote the in-memory cache. Re-read under the
	// write lock and overwrite atomically.
	e.mu.Lock()
	defer e.mu.Unlock()
	plan, ok = e.plans[planID]
	if !ok {
		// Lost the plan between snapshot and commit — extremely unlikely
		// (only happens if another caller deletes the in-memory entry,
		// which we don't currently expose). Return NotFoundError so the
		// caller knows the durable write succeeded but the cache is gone.
		return Plan{}, &NotFoundError{PlanID: planID}
	}
	plan.Status = next
	plan.LastAction = action
	plan.LastActionAt = now
	plan.UpdatedAt = now
	e.plans[planID] = plan
	return plan, nil
}

// Apply moves the Plan from Approved to Applied. Apply is intentionally a
// separate method (not a ReviewIssue action) because its side effect is the
// actual DDL transaction against the HIS dictionary table — see Task 8/25.
// In Task 2 the side effect is limited to flipping Status to Applied.
//
// Persistence path mirrors ReviewIssue: when Engine.tStore is set the
// Apply runs through WithTx → LockPlan → NextStatus → UpdatePlanStatus →
// InsertAuditEvent so the Apply event and the Status mutation commit or
// roll back together.
func (e *Engine) Apply(planID, actor, comment string) (Plan, error) {
	if actor == "" {
		return Plan{}, fmt.Errorf("apply actor is required")
	}
	if e.tStore != nil {
		return e.applyTx(planID, actor, comment)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	plan, ok := e.plans[planID]
	if !ok {
		return Plan{}, &NotFoundError{PlanID: planID}
	}
	fromStatus := plan.Status
	next, err := NextStatus(plan.Status, ActionApply)
	if err != nil {
		return Plan{}, err
	}
	// Double insurance: terminal-state guard after NextStatus (Task 19).
	// typedErrorFor already rejects (Rejected, Apply) → ErrRejectedApply
	// and (RolledBack, *) → ErrRolledBackAny; this is the last line of
	// defence before mutating the in-memory cache.
	if plan.Status.IsTerminal() {
		return Plan{}, &TerminalStateError{From: plan.Status, Action: ActionApply}
	}
	now := e.now()
	plan.Status = next
	plan.LastAction = ActionApply
	plan.LastActionAt = now
	plan.UpdatedAt = now
	if e.store != nil {
		if err := e.store.UpdatePlanStatus(context.Background(), planID, plan.Status, plan.Draft, plan.LastAction, now, now); err != nil {
			return Plan{}, fmt.Errorf("engine: persist apply status: %w", err)
		}
		ev := AuditEventRow{
			EventID:   e.idGen(),
			PlanID:    planID,
			Actor:     actor,
			Action:    ActionApply,
			Payload:   map[string]string{"from_status": string(fromStatus), "to_status": string(next), "comment": comment},
			Timestamp: now,
		}
		if err := e.store.InsertAuditEvent(context.Background(), ev); err != nil {
			return Plan{}, fmt.Errorf("engine: persist apply audit: %w", err)
		}
	}
	e.plans[planID] = plan
	return plan, nil
}

// applyTx is the transactional Apply path. Mirrors reviewIssueTx but
// without TxInsertIssue (Apply records an AuditEvent directly — the
// reviewing-side Issue is created by ReviewIssue(Approve)).
func (e *Engine) applyTx(planID, actor, comment string) (Plan, error) {
	e.mu.RLock()
	plan, ok := e.plans[planID]
	e.mu.RUnlock()
	if !ok {
		return Plan{}, &NotFoundError{PlanID: planID}
	}
	fromStatus := plan.Status
	now := e.now()
	var next PlanStatus
	txErr := e.tStore.WithTx(context.Background(), func(tx Tx) error {
		locked, err := e.tStore.LockPlan(context.Background(), tx, planID)
		if err != nil {
			return err
		}
		nextStatus, nerr := NextStatus(locked.Status, ActionApply)
		if nerr != nil {
			return nerr
		}
		// Double insurance: terminal-state guard after NextStatus (Task 19).
		// Mirrors the per-call-path check in Engine.Apply — the state
		// machine already rejects terminal-state pairs, but this prevents
		// the locked-row's terminal status from being promoted accidentally.
		if locked.Status.IsTerminal() {
			return &TerminalStateError{From: locked.Status, Action: ActionApply}
		}
		next = nextStatus
		if err := e.tStore.TxUpdatePlanStatus(context.Background(), tx, planID, next, locked.Draft, ActionApply, now, now); err != nil {
			return fmt.Errorf("engine: persist apply status: %w", err)
		}
		ev := AuditEventRow{
			EventID:   e.idGen(),
			PlanID:    planID,
			Actor:     actor,
			Action:    ActionApply,
			Payload:   map[string]string{"from_status": string(fromStatus), "to_status": string(next), "comment": comment},
			Timestamp: now,
		}
		if err := e.tStore.TxInsertAuditEvent(context.Background(), tx, ev); err != nil {
			return fmt.Errorf("engine: persist apply audit: %w", err)
		}
		return nil
	})
	if txErr != nil {
		return Plan{}, txErr
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	plan, ok = e.plans[planID]
	if !ok {
		return Plan{}, &NotFoundError{PlanID: planID}
	}
	plan.Status = next
	plan.LastAction = ActionApply
	plan.LastActionAt = now
	plan.UpdatedAt = now
	e.plans[planID] = plan
	return plan, nil
}

// Rollback moves the Plan from Applied to RolledBack. The rollback SQL
// execution itself lives in internal/rollback (Task 11).
func (e *Engine) Rollback(planID, actor, comment string) (Plan, error) {
	if actor == "" {
		return Plan{}, fmt.Errorf("rollback actor is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	plan, ok := e.plans[planID]
	if !ok {
		return Plan{}, &NotFoundError{PlanID: planID}
	}
	fromStatus := plan.Status
	next, err := NextStatus(plan.Status, ActionRollback)
	if err != nil {
		return Plan{}, err
	}
	now := e.now()
	plan.Status = next
	plan.LastAction = ActionRollback
	plan.LastActionAt = now
	plan.UpdatedAt = now
	if e.store != nil {
		if err := e.store.UpdatePlanStatus(context.Background(), planID, plan.Status, plan.Draft, plan.LastAction, now, now); err != nil {
			return Plan{}, fmt.Errorf("engine: persist rollback status: %w", err)
		}
		ev := AuditEventRow{
			EventID:   e.idGen(),
			PlanID:    planID,
			Actor:     actor,
			Action:    ActionRollback,
			Payload:   map[string]string{"from_status": string(fromStatus), "to_status": string(next), "comment": comment},
			Timestamp: now,
		}
		if err := e.store.InsertAuditEvent(context.Background(), ev); err != nil {
			return Plan{}, fmt.Errorf("engine: persist rollback audit: %w", err)
		}
	}
	e.plans[planID] = plan
	return plan, nil
}

// GetPlan returns a copy of the current Plan, or NotFoundError.
func (e *Engine) GetPlan(planID string) (Plan, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	plan, ok := e.plans[planID]
	if !ok {
		return Plan{}, &NotFoundError{PlanID: planID}
	}
	return plan, nil
}

// ListPlans returns all Plans in deterministic order (sorted by createdAt).
// Used by Task 12's cmd/demo for the "multi-plan concurrent" scenario.
func (e *Engine) ListPlans() []Plan {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Plan, 0, len(e.plans))
	for _, p := range e.plans {
		out = append(out, p)
	}
	return out
}

// zeroStatus is the placeholder PlanStatus used by ReviewIssue when rejecting
// the rare case of a zero-value action. Not exported.
var zeroStatus PlanStatus = ""
