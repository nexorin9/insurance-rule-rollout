// store.go — typed CRUD on top of the SQLite handle. Row types are imported
// from internal/workflow so the store directly satisfies the workflow.AuditWriter
// interface — no adapter needed in cmd/demo.
//
// Task 3 covers happy-path CRUD (InsertPlan / UpdatePlanStatus /
// InsertAuditEvent / InsertIssue / GetPlan / ListEvents). Task 4 layers
// the transactional Store interface (WithTx + LockPlan + tx-scoped
// UpdatePlanStatus / InsertIssue / InsertAuditEvent) on top of the same
// *sql.DB handle. The two surfaces coexist because Engine keeps
// AuditWriter for the per-call paths and Store for the WithTx paths.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/example/insurance-rule-rollout/internal/workflow"
)

// Store is the SQLite-backed persistence layer for Plan / Issue / AuditEvent.
type Store struct {
	db *sql.DB
}

// rowToPlan converts the internal scan row into workflow.PlanRow (the row
// type the workflow package owns and Engine.AuditWriter consumes).
func rowToPlan(planID, versionID, ruleSet, effectiveDate, specJSON, prevVersionID, rollbackSQL, status string, draft int, createdBy, lastAction, lastAt, createdAt, updatedAt string) workflow.PlanRow {
	row := workflow.PlanRow{
		PlanID:        planID,
		VersionID:     versionID,
		RuleSet:       ruleSet,
		EffectiveDate: effectiveDate,
		SpecJSON:      specJSON,
		PrevVersionID: prevVersionID,
		RollbackSQL:   rollbackSQL,
		Status:        workflow.PlanStatus(status),
		Draft:         draft != 0,
		CreatedBy:     createdBy,
		LastAction:    workflow.IssueAction(lastAction),
	}
	if lastAt != "" {
		if t, err := time.Parse(time.RFC3339, lastAt); err == nil {
			row.LastActionAt = t
		}
	}
	if createdAt != "" {
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			row.CreatedAt = t
		}
	}
	if updatedAt != "" {
		if t, err := time.Parse(time.RFC3339, updatedAt); err == nil {
			row.UpdatedAt = t
		}
	}
	return row
}

// InsertPlan persists a new Plan row. The (plan_id, version_id) UNIQUE
// constraint guards against duplicate inserts from demo re-imports.
func (s *Store) InsertPlan(ctx context.Context, row workflow.PlanRow) error {
	const stmt = `INSERT INTO plan (
		plan_id, version_id, rule_set, effective_date, spec_json,
		prev_version_id, rollback_sql, status, draft, created_by,
		last_action, last_action_at, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	draftInt := 0
	if row.Draft {
		draftInt = 1
	}
	lastActionAt := ""
	if !row.LastActionAt.IsZero() {
		lastActionAt = row.LastActionAt.UTC().Format(time.RFC3339)
	}
	_, err := s.db.ExecContext(ctx, stmt,
		row.PlanID, row.VersionID, row.RuleSet, row.EffectiveDate, row.SpecJSON,
		row.PrevVersionID, row.RollbackSQL, string(row.Status), draftInt, row.CreatedBy,
		string(row.LastAction), lastActionAt,
		row.CreatedAt.UTC().Format(time.RFC3339),
		row.UpdatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("store: insert plan %s: %w", row.PlanID, err)
	}
	return nil
}

// UpdatePlanStatus mutates plan.status, plan.draft, plan.last_action,
// plan.last_action_at and plan.updated_at for an existing Plan. Returns
// ErrPlanNotFound if the plan_id is absent.
func (s *Store) UpdatePlanStatus(ctx context.Context, planID string, status workflow.PlanStatus, draft bool, lastAction workflow.IssueAction, lastActionAt, updatedAt time.Time) error {
	const stmt = `UPDATE plan SET status = ?, draft = ?, last_action = ?, last_action_at = ?, updated_at = ? WHERE plan_id = ?`
	draftInt := 0
	if draft {
		draftInt = 1
	}
	lastAt := ""
	if !lastActionAt.IsZero() {
		lastAt = lastActionAt.UTC().Format(time.RFC3339)
	}
	res, err := s.db.ExecContext(ctx, stmt,
		string(status), draftInt, string(lastAction), lastAt,
		updatedAt.UTC().Format(time.RFC3339), planID,
	)
	if err != nil {
		return fmt.Errorf("store: update plan %s: %w", planID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rows affected for plan %s: %w", planID, err)
	}
	if n == 0 {
		return ErrPlanNotFound
	}
	return nil
}

// GetPlan loads a Plan by plan_id. Returns ErrPlanNotFound if absent.
func (s *Store) GetPlan(ctx context.Context, planID string) (workflow.PlanRow, error) {
	const stmt = `SELECT plan_id, version_id, rule_set, effective_date, spec_json,
		prev_version_id, rollback_sql, status, draft, created_by,
		last_action, last_action_at, created_at, updated_at
		FROM plan WHERE plan_id = ?`
	var planIDOut, versionID, ruleSet, effectiveDate, specJSON, prevVersionID, rollbackSQL, status, createdBy, lastAction, lastAt, createdAt, updatedAt string
	var draft int
	err := s.db.QueryRowContext(ctx, stmt, planID).Scan(
		&planIDOut, &versionID, &ruleSet, &effectiveDate, &specJSON,
		&prevVersionID, &rollbackSQL, &status, &draft, &createdBy,
		&lastAction, &lastAt, &createdAt, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return workflow.PlanRow{}, ErrPlanNotFound
		}
		return workflow.PlanRow{}, fmt.Errorf("store: select plan %s: %w", planID, err)
	}
	return rowToPlan(planIDOut, versionID, ruleSet, effectiveDate, specJSON, prevVersionID, rollbackSQL, status, draft, createdBy, lastAction, lastAt, createdAt, updatedAt), nil
}

// InsertIssue persists a reviewer's Issue record.
func (s *Store) InsertIssue(ctx context.Context, row workflow.IssueRow) error {
	const stmt = `INSERT INTO issue (issue_id, plan_id, action, actor, comment, created_at) VALUES (?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, stmt,
		row.IssueID, row.PlanID, string(row.Action), row.Actor, row.Comment,
		row.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("store: insert issue %s: %w", row.IssueID, err)
	}
	return nil
}

// InsertAuditEvent persists an audit chain event. The unique constraint
// (plan_id, actor, action, timestamp) guards against double-writes from a
// re-played demo run.
func (s *Store) InsertAuditEvent(ctx context.Context, row workflow.AuditEventRow) error {
	const stmt = `INSERT INTO audit_event (event_id, plan_id, actor, action, payload, timestamp, prev_hash, event_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	payload := "{}"
	if len(row.Payload) > 0 {
		b, err := json.Marshal(row.Payload)
		if err != nil {
			return fmt.Errorf("store: marshal audit payload: %w", err)
		}
		payload = string(b)
	}
	_, err := s.db.ExecContext(ctx, stmt,
		row.EventID, row.PlanID, row.Actor, string(row.Action), payload,
		row.Timestamp.UTC().Format(time.RFC3339), row.PrevHash, row.EventHash,
	)
	if err != nil {
		return fmt.Errorf("store: insert audit event %s: %w", row.EventID, err)
	}
	return nil
}

// ListEvents returns all audit events for a plan in ascending timestamp
// order — the order required by Task 3's demo assertions ("AuditEvent 顺序
// 按 timestamp 升序"). Returns an empty slice when no events match.
func (s *Store) ListEvents(ctx context.Context, planID string) ([]workflow.AuditEventRow, error) {
	const stmt = `SELECT event_id, plan_id, actor, action, payload, timestamp, prev_hash, event_hash
		FROM audit_event WHERE plan_id = ? ORDER BY timestamp ASC`
	rows, err := s.db.QueryContext(ctx, stmt, planID)
	if err != nil {
		return nil, fmt.Errorf("store: list events plan %s: %w", planID, err)
	}
	defer rows.Close()
	out := make([]workflow.AuditEventRow, 0)
	for rows.Next() {
		var eventID, planIDOut, actor, action, payload, ts, prevHash, eventHash string
		if err := rows.Scan(&eventID, &planIDOut, &actor, &action, &payload, &ts, &prevHash, &eventHash); err != nil {
			return nil, fmt.Errorf("store: scan audit event: %w", err)
		}
		ev := workflow.AuditEventRow{
			EventID:   eventID,
			PlanID:    planIDOut,
			Actor:     actor,
			Action:    workflow.IssueAction(action),
			PrevHash:  prevHash,
			EventHash: eventHash,
		}
		if payload != "" {
			pm := map[string]string{}
			if jerr := json.Unmarshal([]byte(payload), &pm); jerr == nil {
				ev.Payload = pm
			}
		}
		if ts != "" {
			if t, perr := time.Parse(time.RFC3339, ts); perr == nil {
				ev.Timestamp = t
			}
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate audit events: %w", err)
	}
	return out, nil
}

// ErrPlanNotFound is returned by UpdatePlanStatus / GetPlan when no row matches.
var ErrPlanNotFound = errors.New("store: plan not found")

// -----------------------------------------------------------------------------
// Task 4 — workflow.Store (transactional) implementation.
//
// SQLite does not speak `SELECT ... FOR UPDATE` natively; the engine uses
// `BEGIN IMMEDIATE` to acquire a database-wide write lock at the start of
// the transaction. That serialises any other writer at WithTx entry, which
// is the closest SQLite equivalent of a row-level lock for our purposes
// (single-process demos; production deployments swap in MySQL/PostgreSQL
// with literal FOR UPDATE — see Task 8/25).

// sqliteTx is the concrete workflow.Tx handle for SQLite. It wraps *sql.Tx
// and is passed back into the workflow.Store methods during a WithTx
// callback. The concrete type is private to the store package; only the
// store knows how to interpret it.
type sqliteTx struct {
	tx *sql.Tx
}

// WithTx implements workflow.Store.WithTx. Opens a transaction, runs fn,
// commits on nil return, rolls back on any non-nil return (including the
// rolled-back case where fn never returns an error but the deferred
// Rollback is a no-op once Commit has succeeded).
//
// SQLite uses BEGIN IMMEDIATE semantics: SetMaxOpenConns(1) on the store
// (configured in Open) combined with sql.Tx gives us single-writer
// behaviour. For the file::memory:?cache=shared DSN used by integration
// tests (Task 18), concurrent goroutines queue on the writer.
func (s *Store) WithTx(ctx context.Context, fn func(tx workflow.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	stx := &sqliteTx{tx: tx}
	if err := fn(stx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit tx: %w", err)
	}
	return nil
}

// LockPlan implements workflow.Store.LockPlan. Inside the supplied
// transaction it reads the current Plan row and returns it; the row is
// implicitly locked because we are inside an IMMEDIATE-style transaction
// (BeginTx on a single-writer pool). SQLite's database-level write lock
// is held for the rest of the transaction; other writers will block on
// the next BeginTx call until we commit / rollback.
//
// The caller (Engine.reviewIssueTx / Engine.applyTx) re-checks the
// locked.Status against the table because state may have changed between
// the Engine's in-memory snapshot read and the LockPlan call.
func (s *Store) LockPlan(ctx context.Context, tx workflow.Tx, planID string) (workflow.Plan, error) {
	stx, ok := tx.(*sqliteTx)
	if !ok {
		return workflow.Plan{}, fmt.Errorf("store: LockPlan requires *sqliteTx, got %T", tx)
	}
	const stmt = `SELECT plan_id, version_id, rule_set, effective_date, spec_json,
		prev_version_id, rollback_sql, status, draft, created_by,
		last_action, last_action_at, created_at, updated_at
		FROM plan WHERE plan_id = ?`
	var (
		planIDOut, versionID, ruleSet, effectiveDate, specJSON, prevVersionID, rollbackSQL, status string
		createdBy, lastAction, lastAt, createdAt, updatedAt                                    string
		draft                                                                                  int
	)
	err := stx.tx.QueryRowContext(ctx, stmt, planID).Scan(
		&planIDOut, &versionID, &ruleSet, &effectiveDate, &specJSON,
		&prevVersionID, &rollbackSQL, &status, &draft, &createdBy,
		&lastAction, &lastAt, &createdAt, &updatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return workflow.Plan{}, &workflow.NotFoundError{PlanID: planID}
		}
		return workflow.Plan{}, fmt.Errorf("store: lock plan %s: %w", planID, err)
	}
	p := workflow.Plan{
		PlanID:        planIDOut,
		VersionID:     versionID,
		RuleSet:       ruleSet,
		EffectiveDate: effectiveDate,
		SpecJSON:      specJSON,
		PrevVersionID: prevVersionID,
		RollbackSQL:   rollbackSQL,
		Status:        workflow.PlanStatus(status),
		Draft:         draft != 0,
		CreatedBy:     createdBy,
		LastAction:    workflow.IssueAction(lastAction),
	}
	if lastAt != "" {
		if t, perr := time.Parse(time.RFC3339, lastAt); perr == nil {
			p.LastActionAt = t
		}
	}
	if createdAt != "" {
		if t, perr := time.Parse(time.RFC3339, createdAt); perr == nil {
			p.CreatedAt = t
		}
	}
	if updatedAt != "" {
		if t, perr := time.Parse(time.RFC3339, updatedAt); perr == nil {
			p.UpdatedAt = t
		}
	}
	return p, nil
}

// UpdatePlanStatusTx is the tx-scoped variant of UpdatePlanStatus. Identical
// SQL, but executed on the supplied *sql.Tx instead of the underlying
// *sql.DB. Helper used by the workflow.Store.UpdatePlanStatus method
// below.
func updatePlanStatusTx(ctx context.Context, stx *sqliteTx, planID string, status workflow.PlanStatus, draft bool, lastAction workflow.IssueAction, lastActionAt, updatedAt time.Time) error {
	const stmt = `UPDATE plan SET status = ?, draft = ?, last_action = ?, last_action_at = ?, updated_at = ? WHERE plan_id = ?`
	draftInt := 0
	if draft {
		draftInt = 1
	}
	lastAt := ""
	if !lastActionAt.IsZero() {
		lastAt = lastActionAt.UTC().Format(time.RFC3339)
	}
	res, err := stx.tx.ExecContext(ctx, stmt,
		string(status), draftInt, string(lastAction), lastAt,
		updatedAt.UTC().Format(time.RFC3339), planID,
	)
	if err != nil {
		return fmt.Errorf("store: update plan %s: %w", planID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rows affected for plan %s: %w", planID, err)
	}
	if n == 0 {
		return ErrPlanNotFound
	}
	return nil
}

// UpdatePlanStatus implements workflow.Store.TxUpdatePlanStatus. Tx-scoped.
// The Tx-prefixed name avoids colliding with the per-call AuditWriter
// method of the same name on Store (Task 3).
func (s *Store) TxUpdatePlanStatus(ctx context.Context, tx workflow.Tx, planID string, status workflow.PlanStatus, draft bool, lastAction workflow.IssueAction, lastActionAt, updatedAt time.Time) error {
	stx, ok := tx.(*sqliteTx)
	if !ok {
		return fmt.Errorf("store: TxUpdatePlanStatus requires *sqliteTx, got %T", tx)
	}
	return updatePlanStatusTx(ctx, stx, planID, status, draft, lastAction, lastActionAt, updatedAt)
}

// TxInsertIssue implements workflow.Store.TxInsertIssue. Tx-scoped.
func (s *Store) TxInsertIssue(ctx context.Context, tx workflow.Tx, row workflow.IssueRow) error {
	stx, ok := tx.(*sqliteTx)
	if !ok {
		return fmt.Errorf("store: TxInsertIssue requires *sqliteTx, got %T", tx)
	}
	const stmt = `INSERT INTO issue (issue_id, plan_id, action, actor, comment, created_at) VALUES (?, ?, ?, ?, ?, ?)`
	_, err := stx.tx.ExecContext(ctx, stmt,
		row.IssueID, row.PlanID, string(row.Action), row.Actor, row.Comment,
		row.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("store: insert issue %s: %w", row.IssueID, err)
	}
	return nil
}

// TxInsertAuditEvent implements workflow.Store.TxInsertAuditEvent. Tx-scoped.
func (s *Store) TxInsertAuditEvent(ctx context.Context, tx workflow.Tx, row workflow.AuditEventRow) error {
	stx, ok := tx.(*sqliteTx)
	if !ok {
		return fmt.Errorf("store: TxInsertAuditEvent requires *sqliteTx, got %T", tx)
	}
	const stmt = `INSERT INTO audit_event (event_id, plan_id, actor, action, payload, timestamp, prev_hash, event_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	payload := "{}"
	if len(row.Payload) > 0 {
		b, err := json.Marshal(row.Payload)
		if err != nil {
			return fmt.Errorf("store: marshal audit payload: %w", err)
		}
		payload = string(b)
	}
	_, err := stx.tx.ExecContext(ctx, stmt,
		row.EventID, row.PlanID, row.Actor, string(row.Action), payload,
		row.Timestamp.UTC().Format(time.RFC3339), row.PrevHash, row.EventHash,
	)
	if err != nil {
		return fmt.Errorf("store: insert audit event %s: %w", row.EventID, err)
	}
	return nil
}
