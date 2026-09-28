-- 0001_init.sql
-- Initial schema for the insurance-rule-rollout Plan / Issue / AuditEvent store.
-- Backed by SQLite for the demo / test path (file::memory:?cache=shared or on-disk);
-- the production DSN-driven MySQL/PostgreSQL path lives in internal/integration/his_dict.
--
-- Tables:
--   plan         — one row per Rule version review lifecycle
--   issue        — one row per reviewer's interaction with a Plan
--   audit_event  — append-only audit chain (SHA256 chain fields populated by internal/audit)
--
-- Uniqueness:
--   plan: (plan_id) PRIMARY KEY, (plan_id, version_id) UNIQUE — protects against
--         duplicate inserts when a demo re-imports the same spec.
--   issue: (issue_id) PRIMARY KEY
--   audit_event: (event_id) PRIMARY KEY, (plan_id, actor, action, timestamp) UNIQUE
--         so a re-run of the same review does not double-write.

CREATE TABLE IF NOT EXISTS plan (
    plan_id         TEXT PRIMARY KEY,
    version_id      TEXT NOT NULL,
    rule_set        TEXT NOT NULL,
    effective_date  TEXT NOT NULL,
    spec_json       TEXT NOT NULL,
    prev_version_id TEXT NOT NULL DEFAULT '',
    rollback_sql    TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL,
    draft           INTEGER NOT NULL DEFAULT 1,
    created_by      TEXT NOT NULL DEFAULT '',
    last_action     TEXT NOT NULL DEFAULT '',
    last_action_at  TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    UNIQUE (plan_id, version_id)
);

CREATE INDEX IF NOT EXISTS idx_plan_status   ON plan (status);
CREATE INDEX IF NOT EXISTS idx_plan_version  ON plan (version_id);

CREATE TABLE IF NOT EXISTS issue (
    issue_id   TEXT PRIMARY KEY,
    plan_id    TEXT NOT NULL,
    action     TEXT NOT NULL,
    actor      TEXT NOT NULL,
    comment    TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    FOREIGN KEY (plan_id) REFERENCES plan (plan_id)
);

CREATE INDEX IF NOT EXISTS idx_issue_plan ON issue (plan_id);
CREATE INDEX IF NOT EXISTS idx_issue_ts   ON issue (created_at);

CREATE TABLE IF NOT EXISTS audit_event (
    event_id    TEXT PRIMARY KEY,
    plan_id     TEXT NOT NULL,
    actor       TEXT NOT NULL,
    action      TEXT NOT NULL,
    payload     TEXT NOT NULL DEFAULT '{}',
    timestamp   TEXT NOT NULL,
    prev_hash   TEXT NOT NULL DEFAULT '',
    event_hash  TEXT NOT NULL DEFAULT '',
    UNIQUE (plan_id, actor, action, timestamp)
);

CREATE INDEX IF NOT EXISTS idx_event_plan_ts ON audit_event (plan_id, timestamp);
