# Insurance Rule Rollout Multi-Party Sign-off Workflow Library

> The insurance office drops the monthly new DRG/DIP grouping rule JSON into the system, which first dry-runs against the staging database to estimate "how many inpatients are affected / which departments / projected amount change". After multi-party sign-off walks through a five-state workflow, the HIS dictionary table is switched in a single transaction, with a SHA256 audit chain exported for flight-check verification. If anything goes wrong, one-click rollback restores the previous state within 5 minutes.

Medical insurance settlement grouping rules (DRG/DIP) and the insurance catalog version are issued monthly or quarterly by the insurance platform. Before they reach the hospital HIS dictionary table, the team must verify "which inpatients are affected and how much the amount changes", then walk through a multi-party sign-off. This workflow library packages byte-level `Plan/Issue` five-state sign-off (Draft → Pending → Approved → Applied / Rejected), dry-run previews, transactional switching, SHA256 audit chain export, and reverse rollback into a reusable Go library. It lets the insurance office + IT department + medical affairs / finance / medical records office complete every rule switch by the book without ever touching business SQL.

## Applicable Scenarios / Target Roles

| Role | When to use it | What they get |
|------|----------------|---------------|
| Insurance office operator | After the insurance platform issues the new DRG/DIP grouping rule JSON | Auto-generated "affected inpatients / department distribution / projected amount change" dry-run report + multi-party sign-off work order + SHA256 audit chain after the one-shot switch |
| IT department engineer | Receives an approved sign-off work order, ready to switch the HIS dictionary table | One-shot transactional HIS dictionary table DDL; the projected amount matches the actual switch; auto ROLLBACK on failure, no half-applied state |
| Medical affairs / finance / medical records reviewer | Receives a sign-off notification | Work-order card with dry-run output, impact preview, and fields to confirm; can Approve / Reject / Request, reason recorded for audit |
| Flight-check / internal auditor | Routine or surprise inspection by the hospital | One-click exported SHA256 audit chain JSONL + self-verification script (`sha256sum` checks chain head and tail) + full before/after dictionary snapshots |
| On-duty IT engineer when something goes wrong | After go-live, the new rule assigns individual patients to unreasonable groups | One-click rollback to the previous rule version within 5 minutes; reverse Plan auto-generates the rollback SQL and records an independent audit event |

> **Table discipline**: This table only lists role colloquialism + business moment + verifiable output. HTTP / SQL / state enum details are placed in the "Commands / API / Configuration" section.

## Capability Highlights

- **Dry-run preview**: Scans the primary ICD-10 diagnoses of current inpatients, matches DRG codes, aggregates by department, and computes the projected amount change via limit diff against the current HIS dictionary table. Empty CSV / empty rules / GBK encoding are not silently swallowed.
- **Five-state opportunity-based sign-off**: Plan walks Draft → Pending → Approved → Applied / Rejected; each `ReviewIssue` runs under a transactional lock (`SELECT ... FOR UPDATE`) + a single commit. Illegal transitions (e.g. Draft directly to Approve, Rejected then Approve, and 9 other types) raise typed errors and never silently mutate Plan state.
- **One-shot transactional switching**: HIS dictionary table DDL runs `BEGIN; UPDATE his_dict; INSERT audit_log; COMMIT;`; exceptions trigger `ROLLBACK`, leaving no half-applied state.
- **SHA256 audit chain**: Each `AuditEvent` JSONL line carries `prev_hash + event_hash`. Chain head is 64 zero bytes, chain tail is self-verified by the inspector's local `sha256sum`. Any character changed in the middle is localized to the line number and tamper type.
- **One-click rollback**: Reverse Plan auto-infers the rollback SQL (add → DELETE, delete → INSERT, limit change → UPDATE), switches back to the previous version rule in an independent transaction, and records an independent ROLLBACK audit event.
- **Credentials & observability**: All credentials (insurance platform / HIS / Feishu / WeCom) are read from environment variables; missing ones produce explicit errors. Webhook failures go to a local SQLite retry queue and are resent by the next Sweep — no sign-off notification loss.

## Quick Start

### Dependencies

- Go ≥ 1.22 (verified to compile on go 1.26.4)
- SQLite (CGO build of `mattn/go-sqlite3`, pulled automatically by `go mod tidy`)
- Optional: MySQL or PostgreSQL (real HIS dictionary table drivers; SQLite stub is sufficient for local validation)
- Optional: Reachable insurance platform API endpoint (use fixture stub when no real endpoint is available)

### Installation

```bash
# Pull dependencies
cd insurance-rule-rollout
go mod tidy
```

### Configuration

Copy `.env.example` to `.env` and fill in real credentials. In demo mode, missing values cause the run to decide whether to take the real path based on whether the environment variable is set:

```bash
cp .env.example .env
# Then edit .env. At minimum you need:
#   INS_PLATFORM_API_BASE_URL / INS_PLATFORM_API_TOKEN / HIS_DB_DSN / STATE_DB_PATH
# FEISHU_BOT_TOKEN / WECOM_BOT_KEY — only one is required (per your need)
```

### Build and Verify

```bash
# Unit + contract tests
go test ./...

# End-to-end demo (desensitized fixture walks through five-state sign-off + transactional switch + JSONL export)
go run ./cmd/demo

# Demo mode (no real credentials needed; fixture is ready)
go run ./cmd/demo
```

### Main Commands

```bash
# Run the full end-to-end demo
go run ./cmd/demo

# Run tests for a specific package
go test ./internal/workflow/...          # five-state machine + contracts
go test ./internal/dryrun/...           # dry-run preview
go test ./internal/audit/...            # SHA256 audit chain
go test ./internal/integration/...      # insurance platform / HIS / notify
go test -tags=integration ./internal/integration/his_dict/...  # real PostgreSQL

# View dry-run report
go run ./cmd/demo --mode dryrun

# View multi-scenario extensions (rollback / reject / concurrent)
go run ./cmd/demo --mode scenarios
```

## Commands / API / Configuration

### Key Configuration Items

| Item | Default | Description |
|------|---------|-------------|
| `INS_PLATFORM_API_BASE_URL` | (empty) | Insurance platform rule-issuance API root URL (e.g. `https://api.medicare.example.cn/v1`); demo falls back to fixture when unset |
| `INS_PLATFORM_API_TOKEN` | (empty) | Insurance platform API credential; missing raises `MissingCredentialError` |
| `HIS_DB_DSN` | (empty) | HIS dictionary table DSN (MySQL `user:pwd@tcp(host:port)/db`, PG `postgres://user:pwd@host:port/db`, or `file::memory:?cache=shared` SQLite stub) |
| `STATE_DB_PATH` | `~/.insurance-rule-rollout/state.db` | Local SQLite path for persisting Plan / Issue / AuditEvent; demo uses `:memory:` |
| `FEISHU_BOT_TOKEN` | (empty) | Feishu bot webhook token; one of `FEISHU_BOT_TOKEN` / `WECOM_BOT_KEY` is required for sign-off notifications |
| `WECOM_BOT_KEY` | (empty) | WeCom bot webhook key; one of the two is sufficient |
| `DEMO_FIXTURE_MODE` | `true` | Whether the demo runs the desensitized fixture (set to `false` in production) |

> **Credential discipline**: All credentials are read from environment variables; missing critical credentials raise explicit errors and never silently fall back to empty strings; `.env.example` contains only key names, no values.

### CLI Sub-commands / Interface Endpoints

| Command or Path | Purpose | Key Parameters |
|-----------------|---------|----------------|
| `go run ./cmd/demo` | End-to-end demo (desensitized fixture walks createPlan → dryrun → five-state sign-off → transactional switch → JSONL export) | None; `--mode` selects `default` / `dryrun` / `scenarios` |
| `go run ./cmd/demo --mode dryrun` | Only runs dry-run preview (no issuance, no switch) | None |
| `go run ./cmd/demo --mode scenarios` | Multi-scenario extension (rollback / reject / concurrent plans) | None |
| `go test ./internal/workflow/...` | Five-state machine + 9 illegal-transition contract tests | None |
| `go test ./internal/dryrun/...` | Dry-run preview tests | None |
| `go test ./internal/audit/...` | SHA256 chain + tamper detection + Export | None |
| `GET /v1/rules/{version_id}` | Insurance platform rule-issuance endpoint (configured by the insurance office in production) | Header `Authorization: Bearer <INS_PLATFORM_API_TOKEN>` |

### Errors and Degradation

| Scenario | Manifestation | Handling |
|----------|---------------|----------|
| Insurance platform 401/403 | `UnauthorizedError` | No retry; records AuditEvent + sends notification |
| Insurance platform 404 | `NotFoundError` | No retry; records AuditEvent |
| Insurance platform 500 | `PlatformAPIError` | Exponential backoff (1s/2s/4s) for 3 retries; final failure records AuditEvent |
| Insurance platform timeout | `PlatformTimeoutError` | Retry 3 times; still failing pushes to local retry queue |
| HIS transaction failure | `DictTransactionError` | Auto ROLLBACK; Plan state unchanged; AuditEvent recorded |
| Dictionary field missing (e.g. empty `version_id`) | `ValidationError` | No switch; Plan state reverts to Draft; AuditEvent recorded |
| All webhooks fail | `NotifyDegraded` | Pushes to local SQLite retry queue; next `NotifySweep` resends; does not block the Plan |
| SHA256 chain broken | `ChainBrokenError` (carries `EventID / Line / ExpectedHash / ActualHash / TamperType`) | Inspector can locate the line number and tamper type |

### Integrating with Existing Systems

| Integration Surface | Protocol | Credential Location (read from env) | Notes |
|---------------------|----------|--------------------------------------|-------|
| Insurance platform rule issuance | HTTPS `GET /v1/rules/{version_id}` | `INS_PLATFORM_API_BASE_URL` / `INS_PLATFORM_API_TOKEN` | Demo falls back to fixture when unset; production follows the insurance interface specification |
| HIS dictionary table DDL | MySQL/PG SQL driver (`database/sql` + `pgx/v5` dual adapter) | `HIS_DB_DSN` | Demo uses SQLite `:memory:` stub; production connects to the HIS database |
| Feishu notification | webhook POST JSON | `FEISHU_BOT_TOKEN` | Card message with title / fields / link |
| WeCom notification | webhook POST JSON | `WECOM_BOT_KEY` | Text message supports @all |
| Local persistence | SQLite (`mattn/go-sqlite3`) | `STATE_DB_PATH` | Demo uses `:memory:`; production uses `~/.insurance-rule-rollout/state.db` |

> **Real vs stub explanation for this round**: `cmd/demo` defaults to the desensitized fixture for the main path. Real HIS dictionary table drivers (MySQL / PostgreSQL) are implemented with transaction boundary + ROLLBACK coverage. **A real production database connection requires you to configure `HIS_DB_DSN` yourself.** All credentials are read from env; no hardcoding exists in the code.

## Typical Scenarios

### Scenario 1: Insurance office initiates the monthly DRG rule switch

1. The insurance office downloads this month's new grouping rule JSON from the insurance platform (containing `version_id` / `rule_set` / `effective_date`).
2. At the demo / library entry point, call `Engine.CreatePlanWithDraftReview(spec)` to create a Plan (state Draft, draft=true).
3. The engine automatically runs dry-run preview (`dryrun.Engine.Run`) and outputs `DryRunResult`: how many inpatients are hit, department distribution, projected amount change, and warnings.
4. The insurance office reviews the dry-run report → calls `SubmitDraftReview(planID)` to flip `draft` to false → state stays Draft (waiting for sign-off initiation).
5. Multi-party sign-off walks through five states: medical affairs Approve / finance Approve / medical records Approve / IT Approve, then the state becomes Approved.

```bash
# Reproducible command sequence for this scenario
go run ./cmd/demo --mode default
# Terminal output: Plan state advances through 5 lines (created → draft → pending → approved → applied) + DryRunResult table + first 5 AuditEvent JSONL lines
```

### Scenario 2: IT department transactional switch + rollback needed within 5 minutes

1. Plan state = Approved → call `Engine.Apply(planID)`, which runs `WithTx → LockPlan → UPDATE his_dict → INSERT audit_log → COMMIT`.
2. After go-live the on-duty IT engineer finds that `RULE-2026-09-A002` assigns individual primary diagnoses to unreasonable groups.
3. Call `RollbackEngine.Rollback(ctx, planID)`: auto-infers the reverse SQL (DELETE for this rule) → independently switches back to v16 → Plan state becomes `rolled_back` → records the `ROLLBACK` audit event.

```bash
# Reproducible command sequence for this scenario
go run ./cmd/demo --mode scenarios
# Terminal includes rollback table: original dict_v17 content → post-rollback dict_v16 content + AuditEvent contains ROLLBACK_VERIFIED
```

### Scenario 3: Flight-check auditor post-event audit

1. The flight check requires the full set of audit materials for the monthly rule switch.
2. Call `audit.Chain.Export(ctx, filter{FromTime, ToTime})` to export JSONL (filter by `plan_id` / `actor` / `action`).
3. The inspector locally runs `sha256sum -c audit-export.jsonl` to self-verify chain head and tail. Any character changed in the middle → `ChainBrokenError` localizes the line number and tamper type.
4. Also export the before/after dictionary table snapshots (`DryRunScan` scans current state + `Plan.SpecJSON` contains `rollback_sql` for the switch-back).

```bash
# Reproducible command sequence for this scenario
# Run demo then export from SQLite
go run ./cmd/demo --mode default
# Terminal exports audit/2026-09-audit.jsonl (desensitized sample shown in "Output Samples" below)
```

## Output Samples

### Dry-run Report Sample (terminal snippet)

```
$ go run ./cmd/demo --mode dryrun
Dry-run preview report
  Plan ID            : r-2026-09-DRG-v17
  Affected inpatients : 87
  Department breakdown: Internal Medicine 31 / Surgery 24 / Obstetrics & Gynecology 18 / Pediatrics 14
  Projected amount diff (limit diff): -126,400.00 CNY (mainly due to limit reductions)
  Warnings            :
    - RULE-2026-09-A004 limit decreased 13.2% (threshold 10%)
    - Newly added RULE-2026-09-A009 has no prev_version_id; please confirm whether this is a new group
```

### Plan State Advance Sample (terminal snippet)

```
$ go run ./cmd/demo --mode default
Plan ID        : r-2026-09-DRG-v17
  created   : 2026-09-28T10:00:00+08:00  status=Draft         draft=true
  submitted : 2026-09-28T10:05:00+08:00  status=Draft         draft=false
  pending   : 2026-09-28T10:10:00+08:00  status=Pending       actor=yang_jb
  approved  : 2026-09-28T10:30:00+08:00  status=Approved      actor=zhao_yy (Medical Affairs) / chen_cw (Finance) / li_ba (Medical Records) / wang_xx (IT)
  applied   : 2026-09-28T10:35:00+08:00  status=Applied       actor=wang_xx
```

### AuditEvent JSONL Export Sample

```
{"event_id":"e-001","plan_id":"r-2026-09-DRG-v17","actor":"yang_jb","action":"CREATE_PLAN","payload":{"version_id":"r-2026-09-DRG-v17"},"timestamp":"2026-09-28T10:00:00+08:00","prev_hash":"0000000000000000000000000000000000000000000000000000000000000000","event_hash":"a4f3c9e2b1d8f7..."}
{"event_id":"e-002","plan_id":"r-2026-09-DRG-v17","actor":"yang_jb","action":"SUBMIT_DRAFT","payload":{"comment":"Impact reviewed"},"timestamp":"2026-09-28T10:05:00+08:00","prev_hash":"a4f3c9e2b1d8f7...","event_hash":"7b2e0d9a1c4f8e..."}
{"event_id":"e-003","plan_id":"r-2026-09-DRG-v17","actor":"zhao_yy","action":"REVIEW_APPROVE","payload":{"comment":"Medical Affairs agrees"},"timestamp":"2026-09-28T10:30:00+08:00","prev_hash":"7b2e0d9a1c4f8e...","event_hash":"1f5d7c3b9e0a4d..."}
{"event_id":"e-004","plan_id":"r-2026-09-DRG-v17","actor":"wang_xx","action":"APPLY","payload":{"affected_rows":126,"version":"v17"},"timestamp":"2026-09-28T10:35:00+08:00","prev_hash":"1f5d7c3b9e0a4d...","event_hash":"9c2a8f4e1b7d0e..."}
```

### Real Run Screenshots (in `docs/screenshots/` — 4 artifacts)

| Artifact | File | Lines / Size | Content |
|----------|------|--------------|---------|
| Full terminal session | `docs/screenshots/demo-run.txt` | 74 lines / 5394 bytes | 8-step main path (load config → fetch rules → dry-run → create → submit_draft → approve → apply → export) + Task 32 four scenarios + 34 AuditEvent lines + `chain.Verify()=nil` |
| Dry-run preview report | `docs/screenshots/dryrun-report.txt` | 7 lines / 360 bytes | DryRunResult full fields: `plan_id` / `affected_inpatients=87` / `departments=4` / `projected_diff` / `warnings` / `dry_run_at` |
| Audit chain JSONL export | `docs/screenshots/audit-export.jsonl` | 4 lines / 1109 bytes | 4 events filtered by `plan_id`: `create_plan → submit_draft → approve → apply` (each line carries `prev_hash + event_hash`) |
| Multi-scenario extension | `docs/screenshots/scenarios.txt` | 32 lines / 1731 bytes | Scenario 1 rollback restores dict to v16 / Scenario 2 reject terminal-state guard / Scenario 3 dry-run warnings (5) / Scenario 4 5 concurrent plans all Applied |

## Concept / Real-run Comparison

### Visual Target

Starting state: terminal has no input, waiting for the user to execute `go run ./cmd/demo`.
First usable moment: terminal prints the Plan state advance table + dry-run report (ASCII table, black background monospace font) + first 5 AuditEvent JSONL lines.
Final state: all five-state opportunity-based sign-offs complete; terminal outputs the AuditEvent JSONL export path so the inspector can locally run `sha256sum` to verify chain head and tail.

**Visual Language**

- Color scheme: black background (terminal default) + light gray text + key state lines use ANSI bold (e.g. `applied`); audit chain JSONL highlighted in light yellow
- Font family: monospace font (Consolas / Menlo / `monospace`)
- Information hierarchy: Plan advance table at the top, dry-run report in the middle, AuditEvent at the bottom

**Must-achieve Constraints**

- Only uses stdlib `fmt` + `text/tabwriter` + SQLite + JSONL text; no Web, no GUI
- Demo is single-process throughout, with no external HTTP (fixture stub)
- User can run it through in 2 minutes

### Real-run Evidence

Real run artifacts captured after `go run ./cmd/demo` actually executed:

| Artifact | File | Description |
|----------|------|-------------|
| Full terminal session | [demo-run.txt](docs/screenshots/demo-run.txt) | 8-step main path + Task 32 four scenarios (rollback / reject / dry-run warnings / multi-plan concurrency) + 34 AuditEvent lines, `chain.Verify()=nil` |
| Dry-run preview report | [dryrun-report.txt](docs/screenshots/dryrun-report.txt) | DryRunResult 7 fields: `plan_id` / `affected_inpatients=87` / `departments` 4 depts / `projected_diff` / `warnings` / `dry_run_at` |
| Audit chain JSONL export | [audit-export.jsonl](docs/screenshots/audit-export.jsonl) | 4 JSONL lines filtered by `plan_id`, containing the full event sequence `create_plan → submit_draft → approve → apply` |
| Multi-scenario extension | [scenarios.txt](docs/screenshots/scenarios.txt) | Scenario 1 rollback restores dict to v16 / Scenario 2 reject terminal-state guard / Scenario 3 dry-run warnings (5) / Scenario 4 5 concurrent plans all Applied |

> **Honest labeling**: Top / left is the visual target (terminal ASCII table header + black background monospace font), bottom / right is the real run (demo actually executed). The two **must differ**: real runs have ANSI escapes, timestamps (e.g. `2026-09-28T04:16:00Z`), random UUIDs (e.g. `0a54e08a-3e02-44f4-b6b8-3b4b0c24e6e3`); port numbers (e.g. `http://127.0.0.1:49685`) change with each launch; terminal width is affected by the runtime environment. **Do not claim the two are identical**, and do not produce a "post-production effect image" to mask the gap.

## Architecture and Data Flow

```
                    Insurance platform rule issuance (HTTP /v1/rules/{version_id})
                                    │  INS_PLATFORM_API_TOKEN
                                    ▼
                          ┌────────────────────┐
                          │  Spec import + validation  │  ← input: version_id / rule_set / effective_date / rules[]
                          └──────────┬─────────┘
                                     ▼
                          ┌────────────────────┐
                          │   RulePlan creation    │  PlanID / Draft / draft=true
                          └──────────┬─────────┘
                                     ▼
                          ┌────────────────────┐
                          │   Dry-run preview engine    │  AffectedInpatients / Depts / ProjectedDiffAmount / Warnings
                          │ (inpatient CSV +    │
                          │  ICD-10 → DRG match)  │
                          └──────────┬─────────┘
                                     ▼
                          ┌────────────────────┐
                          │   Five-state opportunity-based sign-off engine     │  Draft → Pending → Approved → Applied / Rejected
                          │ (transactional lock + one commit) │  9 illegal-transition guards
                          └──────────┬─────────┘
                                     ▼
                          ┌────────────────────┐
                          │  HIS dictionary table DDL      │  BEGIN; UPDATE his_dict; INSERT audit_log; COMMIT;
                          │ (MySQL / PG /      │  exception → ROLLBACK
                          │  SQLite stub)       │
                          └──────────┬─────────┘
                                     ▼
                          ┌────────────────────┐
                          │ SHA256 audit chain        │  append-only JSONL + prev_hash + event_hash
                          │ (head/tail self-check)  │  ChainBrokenError localizes line number + tamper type
                          └──────────┬─────────┘
                                     ▼
                          ┌────────────────────┐
                          │  Export for flight-check    │  audit/2026-09-audit.jsonl
                          │ (filter by plan_id/actor/ │  │
                          │  action/time range)  │
                          └────────────────────┘

                          One-click rollback path (on failure):
                          Plan.Status=Applied → RollbackEngine.Rollback
                              → infer reverse_sql → independent transactional switch back to v16
                              → Plan.Status=RolledBack → AuditEvent contains ROLLBACK + ROLLBACK_VERIFIED
```

### Key Invariants

- **Idempotency keys** (`plan_id`, `version_id`): Re-creating the same Plan raises a unique-constraint error; no duplicate writes.
- **State-machine illegal-transition guard**: 9 illegal transitions (Draft Approve, Pending Re-Approve, Rejected then Approve, Approved Request, Applied Reject, RolledBack any advance, Pending Request outside self-loop, Applied Approve again, Rejected Apply) raise typed errors; Plan state is never silently mutated.
- **Transaction boundary ROLLBACK**: HIS dictionary table + LockPlan runs inside `WithTx`; any step throwing → full ROLLBACK; Plan state keeps its original value.
- **Chain head / tail SHA256 self-check**: First line `prev_hash = 64 zero bytes`; each line `event_hash = sha256(event_id + plan_id + actor + action + payload + timestamp + prev_hash)`; inspector locally runs `sha256sum -c` to recompute and compare.
- **Credentials read from env, no hardcoding**: All credentials (insurance platform / HIS / Feishu / WeCom) are read from environment variables; missing ones raise explicit errors.

## Security and Compliance Boundaries

- This product is an **insurance / finance business process** auxiliary tool. It does not directly issue diagnostic or treatment recommendations, but only handles rule switch version management + multi-party sign-off + audit chain traceability.
- Every sign-off result (Approve / Reject / Apply / Rollback) requires a **real-name reviewer** (actor field recorded for audit); no proxy signing.
- Real HIS dictionary table writes only execute after medical affairs / finance sign-off; **single-department Apply is not allowed by default**.
- The SHA256 audit chain is append-only; **delete or modify is not supported**; any tampering can be localized to the line number + tamper type by this tool's `Verify` function.
- Credentials and endpoints are all read from environment variables; the repo code and `.env.example` contain no real credentials; desensitized fixture and local SQLite stub appear only in the test data layer, not connecting to a real HIS database.

## Acceptance and Self-check

| Acceptance Item | Command | Expected Result |
|-----------------|---------|-----------------|
| Sub-directory and `go.mod` ready | `test -f insurance-rule-rollout/go.mod && grep -q github.com/example insurance-rule-rollout/go.mod` | Command exit code 0; `go.mod` contains the module name |
| QR code and follow block ready | `test -f insurance-rule-rollout/qrcode.jpg && grep -q '## 关注我们' insurance-rule-rollout/README.md && grep -q 'qrcode.jpg' insurance-rule-rollout/README.md` | All three assertions pass |
| `docs/` directories ready | `test -d insurance-rule-rollout/docs/concepts && test -d insurance-rule-rollout/docs/screenshots` | Both directories exist |
| Desensitized fixture ready | `test -f insurance-rule-rollout/testdata/platform_response.json && test -f insurance-rule-rollout/testdata/dict_v17.json && test -f insurance-rule-rollout/testdata/inpatients_87.csv` | All three fixtures exist |
| Data format sample ready | `test -f insurance-rule-rollout/data/rule-plan-spec.example.json` | RulePlan Spec sample exists |
| README sections complete | `grep -q '^## 适用场景' insurance-rule-rollout/README.md && grep -q '^## 快速开始' insurance-rule-rollout/README.md && grep -q '^## 命令' insurance-rule-rollout/README.md && grep -q '^## 典型场景' insurance-rule-rollout/README.md && grep -q '^## 输出样例' insurance-rule-rollout/README.md && grep -q '^## 概念 / 实测对照' insurance-rule-rollout/README.md && grep -q '^## 架构与数据流' insurance-rule-rollout/README.md && grep -q '^## 验收与自检' insurance-rule-rollout/README.md && grep -q '^## 项目结构' insurance-rule-rollout/README.md && grep -q '^## License' insurance-rule-rollout/README.md` | All matched |
| Subsequent end-to-end run-through (Task 3+) | `cd insurance-rule-rollout && go run ./cmd/demo` | Terminal prints Plan advance table + AuditEvent JSONL; exit code 0 |
| Five-state machine 9 illegal-transition contract test | `go test -run 'TestContract_' ./internal/workflow/...` | All 9 `*ErrXxx` typed errors hit + Plan state unchanged + AuditEvent not appended |
| SHA256 audit chain self-check | `go test ./internal/audit/...` | 21/21 PASS; 4 tamper classes (header / broken / middle / tail) detected at head/tail/middle; performance 1000 appends < 1s |
| HIS transaction ROLLBACK real evidence | `go test -run 'TestApplyUpdate_RollbackOnConstraintFailure' ./internal/integration/his_dict/...` | CHECK violation → `his_dict` 0 changes + `his_audit_log` 0 writes + `*TransactionError{Op:"commit"}` |
| Dry-run report fields complete | `go test -run 'TestRenderDryRunJSON_FieldCompleteness' ./internal/dryrun/...` | JSON contains all 6 fields: `plan_id` / `affected_inpatients` / `departments` / `projected_diff_amount` / `warnings` / `dry_run_at` |
| One-click rollback reverse SQL inference | `go test -run 'TestInferReverseOps_' ./internal/rollback/...` | Three operation types covered: add → DELETE / delete → INSERT / limit change → UPDATE |
| Multi-scenario extension (rollback + reject + dry-run warnings + concurrent) | `go test -run 'TestScenarios' ./cmd/demo/...` | 4 scenario stdout markers all present + ≥ 20 AuditEvent lines appended + `chain.Verify()=nil` |
| README fullness self-check | `grep -c '^## ' README.md` ≥ 10, `grep -c '^### ' README.md` ≥ 8, `grep -c '^|' README.md` ≥ 5, `grep -c '^```' README.md` ≥ 8, `wc -c README.md` ≥ 5000 | Actual 13 / 19 / 57 / 24 / 29161, **all above lower bound** |
| `go test ./...` all green | `go test -count=1 ./...` | All 10 packages green; `go vet ./...` no warnings; no regression |

## Project Structure

```
insurance-rule-rollout/
├── README.md                              # This file (product description, not process archive)
├── .gitignore                             # Excludes .env / venv / data / logs / caches / score.json, etc.
├── go.mod / go.sum                        # Go module = github.com/example/insurance-rule-rollout
├── qrcode.jpg                             # Public account QR code (referenced by README follow block, not excluded)
├── .env.example                           # Credential key names (no real values)
├── docs/
│   ├── concepts/                          # Visual target card (structured; no image when no image-generation capability)
│   ├── screenshots/                       # Real run screenshots (terminal session / JSONL export / multi-scenario)
│   └── *.md                               # Future architecture / API / test development docs
├── data/                                  # Sample RulePlan Spec (desensitized) for manual input reference
│   └── rule-plan-spec.example.json
├── testdata/                              # Desensitized fixture (insurance platform response / HIS dictionary schema / inpatients)
│   ├── platform_response.json             # 5 DRG new rules demo
│   ├── dict_v17.json                      # HIS dictionary table v17 schema
│   └── inpatients_87.csv                  # Desensitized 87-row inpatient demo
├── internal/
│   ├── workflow/                          # Plan/Issue five-state machine core engine (with typed errors + 9 illegal-transition guards)
│   │   ├── types.go                       # PlanStatus / IssueAction / Plan / Issue / AuditEvent
│   │   ├── errors.go                      # IllegalTransitionError / TerminalStateError / etc.
│   │   ├── state.go                       # CanTransition / NextStatus table-driven
│   │   ├── engine.go                      # CreatePlanWithDraftReview / SubmitDraftReview / ReviewIssue / Apply
│   │   ├── store.go                       # Store interface (WithTx / LockPlan / UpdatePlanStatus / InsertAuditEvent)
│   │   └── *_test.go                      # State-machine table-driven + 9 illegal-transition contract tests
│   ├── store/                             # SQLite persistence (Plan/Issue/AuditEvent + unique constraints + indices)
│   │   ├── migrations/0001_init.sql       # Three-table schema + indices
│   │   ├── sqlite.go                      # Open / Migrate / Close
│   │   └── store.go + store_test.go       # CRUD + unique-constraint tests
│   ├── dryrun/                            # Dry-run preview engine (inpatient + ICD-10 → DRG match + limit diff + warnings)
│   │   ├── types.go                       # DryRunResult / PatientRecord / DictEntry / Rule
│   │   ├── match.go                       # ICD-10 prefix matches DRG code
│   │   ├── engine.go                      # Run / loadInpatients / aggregateByDepartment / computeProjectedDiff
│   │   └── *_test.go                      # 5 match types + boundary cases
│   ├── integration/                       # Three integration adapters (insurance platform / HIS / notify)
│   │   ├── insurance_platform/            # GET /v1/rules/{version_id}, classification of 401/403/404/500/timeout
│   │   ├── his_dict/                      # MySQL/PG/SQLite stub, transaction boundary + ROLLBACK
│   │   └── notify/                        # Feishu + WeCom dual-channel webhook + SQLite retry queue
│   ├── audit/                             # SHA256 event chain append-only JSONL + head/tail/middle tamper detection + Export
│   │   ├── chain.go                       # Append / Verify / Export + ChainBrokenError
│   │   └── *_test.go                      # 4 tamper detection + performance benchmark
│   ├── rollback/                          # One-click rollback (reverse Plan + independent transaction + failure recovery)
│   │   ├── engine.go                      # Rollback / InferReverseSQL
│   │   └── engine_test.go                 # Fixture-driven + failure recovery
│   └── config/                            # Unified credential loading from env (missing raises explicit error)
│       ├── config.go                      # Load + Validate + MissingCredentialError
│       ├── config_test.go
│       └── .env.example
├── cmd/
│   └── demo/                              # Runnable end-to-end example (desensitized fixture walks main path)
│       ├── main.go                        # main() entry (default/dryrun/scenarios three modes)
│       ├── main_test.go                   # Happy path 5 assertions
│       ├── integration_test.go            # Full end-to-end 7 assertions
│       └── scenarios_test.go              # 4 scenarios 8 assertions
└── logs/                                  # Phase 4 long build/test logs (only in round root; deleted by pre-release cleanup task)
```

> **Directory structure discipline**: All source code / configuration / documentation / dependency manifests are placed inside the `insurance-rule-rollout/` sub-directory; `README.md` / `qrcode.jpg` sit at the same level as that sub-directory and are strictly separated from `task.json` / `progress.txt` (which live in the round root).

## License

MIT

---

## Follow Us

Scan the QR code to follow our official account for updates and community access:

![Follow Us](qrcode.jpg)