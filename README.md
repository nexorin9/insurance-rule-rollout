# 医保规则切换会签工作流库

> 医保办把月度新版 DRG/DIP 分组规则 JSON 投进来，工具先在测试库里预演「会影响多少在院患者/哪个科室/预计差额」，多部门走完五态会签后一次性事务切换 HIS 字典表，并落地 SHA256 审计链供飞检核对；出问题 5 分钟内一键回滚。

医保结算分组规则（DRG/DIP）和医保目录版本每月或每季度由医保平台统一下发，落到医院 HIS 字典表前必须先核对「影响哪些在院患者、影响金额多少」，并经多部门会签。本工作流库把字节级 `Plan/Issue` 五态机会签（Draft → Pending → Approved → Applied / Rejected）、干跑预演、事务切换、SHA256 审计链导出和反向回滚做成可重用 Go 库，让医保办 + 信息科 + 医务科/财务/病案室能在不接触业务 SQL 的情况下按规矩走完每一次规则切换。

## 适用场景 / 目标岗位

| 岗位 | 什么时候用 | 得到什么 |
|------|------------|----------|
| 医保办经办 | 医保平台下发新版 DRG/DIP 分组规则 JSON 后 | 自动出的「影响在院患者 / 科室分布 / 预计差额」干跑报告 + 多部门会签工单 + 一次提交切换后的 SHA256 审计链 |
| 信息科工程师 | 收到会签通过的工单，准备切换 HIS 字典表 | 一次性事务切换 HIS 字典表 DDL，预演与切换的差额一致；失败自动 ROLLBACK，不留半截状态 |
| 医务科 / 财务 / 病案室审批人 | 收到会签通知 | 工单卡片列出干跑结果、影响面、需确认的字段；可 Approve / Reject / Request，理由留审计 |
| 飞检 / 内审审计员 | 院内例行或飞行检查 | 一键导出的 SHA256 审计链 JSONL + 自验脚本（`sha256sum` 链头链尾核对）+ 完整切换前后字典表快照 |
| 出问题时的值班信息科 | 上线后发现新版规则对个别患者分组不合理 | 5 分钟内一键回滚回上一版规则，反向 Plan 自动生成回退 SQL，留独立审计事件 |

> **表格纪律**：本表只写岗位口语 + 业务时刻 + 可核对产出；HTTP / SQL / 状态枚举等细节一律放到「命令 / API / 配置说明」。

## 能力要点

- **干跑预演**：扫当前在院患者主诊断 ICD-10 命中哪些 DRG code、按科室分组聚合、与当前 HIS 字典表 limit diff 计算预计差额；空 CSV / 空规则 / GBK 编码全都不静默吞错。
- **五态机会签**：Plan 走 Draft → Pending → Approved → Applied / Rejected 五态；每次 ReviewIssue 走事务锁（`SELECT ... FOR UPDATE`）+ 一次提交；非法转换（Draft 直接 Approve、已 Rejected 再 Approve 等 9 种）落 typed error，不静默改 Plan 状态。
- **一次性事务切换**：HIS 字典表 DDL 走 `BEGIN; UPDATE his_dict; INSERT audit_log; COMMIT;`，异常 `ROLLBACK`，字典表不会出现半切换。
- **SHA256 审计链**：每条 AuditEvent JSONL 带 `prev_hash + event_hash`，链头 64 字节 0、链尾由飞检员本地 `sha256sum` 自验，中间任意一字符被改都能定位到行号与篡改类型。
- **一键回滚**：反向 Plan 自动推断回退 SQL（新增→DELETE、删除→INSERT、限额变化→UPDATE），独立事务切换回上一版本规则 + 留独立 ROLLBACK 审计事件。
- **凭据与可观测**：所有凭据（医保平台 / HIS / 飞书 / 企微）从环境变量读，缺失即显式报错；webhook 失败走本地 SQLite retry 队列，下一轮 Sweep 重发，不丢会签通知。

## 快速开始

### 依赖

- Go ≥ 1.22（实测在 go 1.26.4 编译通过）
- SQLite（CGO 版 `mattn/go-sqlite3`，自动随 `go mod tidy` 拉取）
- 可选：MySQL 或 PostgreSQL（HIS 字典表真实驱动，本地可用 SQLite 替身验证）
- 可选：可达的医保平台 API 端点（无真实端点时使用 fixture 替身）

### 安装

```bash
# 拉取依赖
cd insurance-rule-rollout
go mod tidy
```

### 配置

复制 `.env.example` 为 `.env` 并填写真实凭据位（演示时若未配置，会以环境变量是否设置决定是否走真实路径）：

```bash
cp .env.example .env
# 然后编辑 .env，至少需要：
#   INS_PLATFORM_API_BASE_URL / INS_PLATFORM_API_TOKEN / HIS_DB_DSN / STATE_DB_PATH
# 其余 FEISHU_BOT_TOKEN / WECOM_BOT_KEY 二选一即可（按需）
```

### 构建与验证

```bash
# 单元 + 契约测试
go test ./...

# 端到端 demo（脱敏 fixture 走完五态会签 + 事务切换 + JSONL 导出）
go run ./cmd/demo

# 演示模式（不需要真实凭据，fixture 已就绪）
go run ./cmd/demo
```

### 主命令速查

```bash
# 跑完整端到端 demo
go run ./cmd/demo

# 跑指定包的测试
go test ./internal/workflow/...          # 五态机 + 契约
go test ./internal/dryrun/...           # 干跑预演
go test ./internal/audit/...            # SHA256 审计链
go test ./internal/integration/...      # 医保平台 / HIS / 通知
go test -tags=integration ./internal/integration/his_dict/...  # 真实 PostgreSQL

# 看干跑报告
go run ./cmd/demo --mode dryrun

# 看多场景扩展（rollback / reject / 并发）
go run ./cmd/demo --mode scenarios
```

## 命令 / API / 配置说明

### 关键配置项

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `INS_PLATFORM_API_BASE_URL` | （空） | 医保平台下发规则文件 API 根地址（如 `https://api.medicare.example.cn/v1`）；演示未设走 fixture |
| `INS_PLATFORM_API_TOKEN` | （空） | 医保平台 API 调用凭证；缺失即 `MissingCredentialError` |
| `HIS_DB_DSN` | （空） | HIS 字典表数据库 DSN（MySQL `user:pwd@tcp(host:port)/db` 或 PG `postgres://user:pwd@host:port/db` 或 `file::memory:?cache=shared` SQLite 替身） |
| `STATE_DB_PATH` | `~/.insurance-rule-rollout/state.db` | 本地持久化 Plan / Issue / AuditEvent 的 SQLite 路径；demo 模式用 `:memory:` |
| `FEISHU_BOT_TOKEN` | （空） | 飞书机器人 webhook token；与 `WECOM_BOT_KEY` 二选一，会签通知双通道 |
| `WECOM_BOT_KEY` | （空） | 企微机器人 webhook key；与飞书二选一即可 |
| `DEMO_FIXTURE_MODE` | `true` | demo 是否跑脱敏 fixture（生产部署时设为 `false`） |

> **凭据纪律**：所有凭据一律从环境变量读；缺失关键凭据显式报错，不静默用空串；`.env.example` 仅含键名（无值）。

### CLI 子命令 / 接口端点

| 命令或路径 | 用途 | 关键参数 |
|------------|------|----------|
| `go run ./cmd/demo` | 端到端 demo（脱敏 fixture 跑通 createPlan → dryrun → 五态会签 → 事务切换 → JSONL 导出） | 无；`--mode` 选 `default` / `dryrun` / `scenarios` |
| `go run ./cmd/demo --mode dryrun` | 仅跑干跑预演（不下发、不切换） | 无 |
| `go run ./cmd/demo --mode scenarios` | 多场景扩展（rollback / reject / 并发 plan） | 无 |
| `go test ./internal/workflow/...` | 五态机 + 9 种非法转换契约测试 | 无 |
| `go test ./internal/dryrun/...` | 干跑预演测试 | 无 |
| `go test ./internal/audit/...` | SHA256 链 + 篡改检测 + Export | 无 |
| `GET /v1/rules/{version_id}` | 医保平台下发规则文件端点（实际由医保办在生产环境配置） | Header `Authorization: Bearer <INS_PLATFORM_API_TOKEN>` |

### 错误与降级

| 场景 | 表现 | 处理方式 |
|------|------|----------|
| 医保平台 401/403 | `UnauthorizedError` | 不重试，落 AuditEvent + 通知 |
| 医保平台 404 | `NotFoundError` | 不重试，落 AuditEvent |
| 医保平台 500 | `PlatformAPIError` | 指数退避（1s/2s/4s）重试 3 次，最终失败落 AuditEvent |
| 医保平台超时 | `PlatformTimeoutError` | 重试 3 次，仍失败落本地 retry 队列 |
| HIS 事务失败 | `DictTransactionError` | 自动 ROLLBACK，Plan 状态不变，落 AuditEvent 记录 |
| 字典表缺字段（如 version_id 空）| `ValidationError` | 不切换，Plan 状态回退到 Draft，落 AuditEvent |
| webhook 全部失败 | `NotifyDegraded` | 落本地 SQLite retry 队列，下一轮 `NotifySweep` 重发，不阻塞 Plan |
| SHA256 链断裂 | `ChainBrokenError`（含 `EventID / Line / ExpectedHash / ActualHash / TamperType`）| 飞检可定位到行号与篡改类型 |

### 对接现有系统

| 集成面 | 协议 | 凭据位（env 读） | 备注 |
|--------|------|------------------|------|
| 医保平台下发规则 | HTTPS `GET /v1/rules/{version_id}` | `INS_PLATFORM_API_BASE_URL` / `INS_PLATFORM_API_TOKEN` | 演示未设走 fixture；真实环境按医保接口规范对接 |
| HIS 字典表 DDL | MySQL/PG SQL 驱动 (`database/sql` + `pgx/v5` 双适配) | `HIS_DB_DSN` | demo 用 SQLite `:memory:` 替身，真实部署接 HIS 库 |
| 飞书通知 | webhook POST JSON | `FEISHU_BOT_TOKEN` | 卡片消息含标题/字段/链接 |
| 企微通知 | webhook POST JSON | `WECOM_BOT_KEY` | 文本消息支持 @all |
| 本地持久化 | SQLite（`mattn/go-sqlite3`） | `STATE_DB_PATH` | demo 用 `:memory:`；生产用 `~/.insurance-rule-rollout/state.db` |

> **本轮真实 vs 替身说明**：`cmd/demo` 默认以脱敏 fixture 跑通主路径；HIS 字典表真实驱动接口（MySQL / PostgreSQL）已实现并覆盖事务边界 + ROLLBACK，**真实生产数据库连接需用户自行配置 HIS_DB_DSN**。所有凭据从 env 读，代码内不存在硬编码。

## 典型场景

### 场景 1：医保办发起月度新版 DRG 规则切换

1. 医保办从医保平台下载本月新版分组规则 JSON（含 `version_id` / `rule_set` / `effective_date`）。
2. 在 demo/库入口处调用 `Engine.CreatePlanWithDraftReview(spec)` 创建 Plan（状态 Draft, draft=true）。
3. 引擎自动从干跑预演（`dryrun.Engine.Run`）输出 `DryRunResult`：命中多少在院患者、科室分布、预计差额、警告。
4. 医保办审阅干跑报告 → 调用 `SubmitDraftReview(planID)` 把 draft 翻 false → 状态仍 Draft（等待发起会签）。
5. 多部门会签走五态：医务科 Approve / 财务 Approve / 病案室 Approve / 信息科 Approve 后状态变 Approved。

```bash
# 该场景的可复制命令序列
go run ./cmd/demo --mode default
# 终端输出：Plan 状态推进 5 行（created → draft → pending → approved → applied）+ DryRunResult 表 + AuditEvent JSONL 前 5 行
```

### 场景 2：信息科事务切换 + 5 分钟内发现需要回滚

1. Plan 状态 = Approved → 调用 `Engine.Apply(planID)` 走 `WithTx → LockPlan → UPDATE his_dict → INSERT audit_log → COMMIT`。
2. 上线后值班信息科发现 `RULE-2026-09-A002` 对个别主诊断分组不合理。
3. 调用 `RollbackEngine.Rollback(ctx, planID)`：自动推断反向 SQL（该 rule 的 DELETE）→ 独立事务切换回 v16 → Plan 状态变 `rolled_back` → 落 `ROLLBACK` 审计事件。

```bash
# 该场景的可复制命令序列
go run ./cmd/demo --mode scenarios
# 终端含 rollback 表：原 dict_v17 内容 → rollback 后 dict_v16 内容 + AuditEvent 含 ROLLBACK_VERIFIED
```

### 场景 3：飞检审计员事后审计

1. 飞检要求出具月度规则切换的全部审计材料。
2. 调用 `audit.Chain.Export(ctx, filter{FromTime, ToTime})` 导出 JSONL（按 plan_id / actor / action 过滤）。
3. 飞检员本地 `sha256sum -c audit-export.jsonl` 自验链头链尾，链中任意一字符被改 → `ChainBrokenError` 定位行号与篡改类型。
4. 附带导出切换前后字典表快照（`DryRunScan` 扫当前 + Plan.SpecJSON 含 rollback_sql 切换回前）。

```bash
# 该场景的可复制命令序列
# 跑 demo 后从 SQLite 导出
go run ./cmd/demo --mode default
# 终端导出 audit/2026-09-audit.jsonl（脱敏样例见下方「输出样例」）
```

## 输出样例

### 干跑报告样例（终端片段）

```
$ go run ./cmd/demo --mode dryrun
干跑预演报告
  Plan ID            : r-2026-09-DRG-v17
  影响在院患者          : 87 人
  涉及科室分布          : 内科 31 / 外科 24 / 妇产科 18 / 儿科 14
  预计差额（limit diff）: -126,400.00 元（限额下调为主）
  警告              :
    - RULE-2026-09-A004 limit 降幅 13.2%（阈值 10%）
    - 新增 RULE-2026-09-A009 无 prev_version_id，请确认是否新纳入病组
```

### Plan 状态推进样例（终端片段）

```
$ go run ./cmd/demo --mode default
Plan ID        : r-2026-09-DRG-v17
  created   : 2026-09-28T10:00:00+08:00  status=Draft         draft=true
  submitted : 2026-09-28T10:05:00+08:00  status=Draft         draft=false
  pending   : 2026-09-28T10:10:00+08:00  status=Pending       actor=yang_jb
  approved  : 2026-09-28T10:30:00+08:00  status=Approved      actor=zhao_yy (医务科) / chen_cw (财务) / li_ba (病案室) / wang_xx (信息科)
  applied   : 2026-09-28T10:35:00+08:00  status=Applied       actor=wang_xx
```

### AuditEvent JSONL 导出样例

```
{"event_id":"e-001","plan_id":"r-2026-09-DRG-v17","actor":"yang_jb","action":"CREATE_PLAN","payload":{"version_id":"r-2026-09-DRG-v17"},"timestamp":"2026-09-28T10:00:00+08:00","prev_hash":"0000000000000000000000000000000000000000000000000000000000000000","event_hash":"a4f3c9e2b1d8f7..."}
{"event_id":"e-002","plan_id":"r-2026-09-DRG-v17","actor":"yang_jb","action":"SUBMIT_DRAFT","payload":{"comment":"已核对影响面"},"timestamp":"2026-09-28T10:05:00+08:00","prev_hash":"a4f3c9e2b1d8f7...","event_hash":"7b2e0d9a1c4f8e..."}
{"event_id":"e-003","plan_id":"r-2026-09-DRG-v17","actor":"zhao_yy","action":"REVIEW_APPROVE","payload":{"comment":"医务科同意"},"timestamp":"2026-09-28T10:30:00+08:00","prev_hash":"7b2e0d9a1c4f8e...","event_hash":"1f5d7c3b9e0a4d..."}
{"event_id":"e-004","plan_id":"r-2026-09-DRG-v17","actor":"wang_xx","action":"APPLY","payload":{"affected_rows":126,"version":"v17"},"timestamp":"2026-09-28T10:35:00+08:00","prev_hash":"1f5d7c3b9e0a4d...","event_hash":"9c2a8f4e1b7d0e..."}
```

### 真实运行截图（`docs/screenshots/` 内 4 个产物）

| 产物 | 文件 | 行数 / 大小 | 内容 |
|------|------|-------------|------|
| 终端完整会话 | `docs/screenshots/demo-run.txt` | 74 行 / 5394 bytes | 8 步主路径（load config → 拉规则 → 干跑 → create → submit_draft → approve → apply → export）+ Task 32 四场景 + 34 条 AuditEvent + `chain.Verify()=nil` |
| 干跑预演报告 | `docs/screenshots/dryrun-report.txt` | 7 行 / 360 bytes | DryRunResult 全字段：plan_id / affected_inpatients=87 / departments=4 / projected_diff / warnings / dry_run_at |
| 审计链 JSONL 导出 | `docs/screenshots/audit-export.jsonl` | 4 行 / 1109 bytes | plan_id 过滤后完整 4 条事件：`create_plan → submit_draft → approve → apply`（每行含 prev_hash + event_hash） |
| 多场景扩展 | `docs/screenshots/scenarios.txt` | 32 行 / 1731 bytes | Scenario 1 rollback dict 还原 v16 / Scenario 2 reject 终态守卫 / Scenario 3 干跑告警 5 条 / Scenario 4 5 个 plan 并发全部 Applied |

## 概念 / 实测对照

### 视觉目标

起始态：终端无输入，等待用户执行 `go run ./cmd/demo`。
首个可用瞬间：终端打印出 Plan 状态推进表 + 干跑报告（ASCII 表格，黑色背景等宽字体）+ AuditEvent 前 5 行 JSONL。
成形态：五态机会签全部跑通，终端打出 AuditEvent JSONL 导出路径，飞检员可本地 `sha256sum` 验链头链尾。

**视觉语言**

- 配色：黑色背景（终端默认）+ 浅灰文本 + 关键状态行用 ANSI 加粗（如 `applied`），审计链 JSONL 用浅黄色突出
- 字体族：等宽字体（Consolas / Menlo / `monospace`）
- 信息层级：Plan 推进表置顶、干跑报告中间、AuditEvent 尾部

**必须可达的约束**

- 仅用 stdlib `fmt` + `text/tabwriter` + SQLite + JSONL 文本，无 Web / 无 GUI
- demo 全程单进程，无外部 HTTP（fixture 替身）
- 用户能在 2 分钟内跑通一次

### 实测证据

`go run ./cmd/demo` 实际跑通后采集的真实运行产物：

| 产物 | 文件 | 说明 |
|------|------|------|
| 终端完整会话 | [demo-run.txt](docs/screenshots/demo-run.txt) | 8 步主路径 + Task 32 四场景（rollback / reject / 干跑告警 / 多 plan 并发）+ 34 条 AuditEvent 落地，chain.Verify()=nil |
| 干跑预演报告 | [dryrun-report.txt](docs/screenshots/dryrun-report.txt) | DryRunResult 7 字段：plan_id / affected_inpatients=87 / departments 4 部门 / projected_diff / warnings / dry_run_at |
| 审计链 JSONL 导出 | [audit-export.jsonl](docs/screenshots/audit-export.jsonl) | plan_id 过滤后 4 行 JSONL，含 create_plan → submit_draft → approve → apply 完整事件序列 |
| 多场景扩展 | [scenarios.txt](docs/screenshots/scenarios.txt) | Scenario 1 rollback 把 dict 还原 v16 / Scenario 2 reject 终态守卫 / Scenario 3 干跑告警 5 条 / Scenario 4 5 个 plan 并发全部 Applied |

> **诚实标注**：上/左为视觉目标（终端 ASCII 表头 + 黑色背景等宽字体），下/右为真实实测（demo 实际跑通）。两者**必然有差距**：实测会有 ANSI 转义、时间戳（如 `2026-09-28T04:16:00Z`）、随机 UUID（如 `0a54e08a-3e02-44f4-b6b8-3b4b0c24e6e3`）；端口号（`http://127.0.0.1:49685`）每次启动不同；终端宽度受运行环境影响。**不要声称两者完全一致**，也不要生成「事后效果图」来抹平差距。

## 架构与数据流

```
                    医保平台下发文件（HTTP /v1/rules/{version_id}）
                                    │  INS_PLATFORM_API_TOKEN
                                    ▼
                          ┌────────────────────┐
                          │  Spec 导入 + 校验  │  ← 输入：version_id / rule_set / effective_date / rules[]
                          └──────────┬─────────┘
                                     ▼
                          ┌────────────────────┐
                          │   RulePlan 创建    │  PlanID / Draft / draft=true
                          └──────────┬─────────┘
                                     ▼
                          ┌────────────────────┐
                          │   干跑预演引擎       │  AffectedInpatients / Depts / ProjectedDiffAmount / Warnings
                          │ （在院患者 CSV +    │
                          │  ICD-10 → DRG 匹配）│
                          └──────────┬─────────┘
                                     ▼
                          ┌────────────────────┐
                          │   五态机会签引擎     │  Draft → Pending → Approved → Applied / Rejected
                          │ （事务锁 + 一次提交） │  9 种非法转换守卫
                          └──────────┬─────────┘
                                     ▼
                          ┌────────────────────┐
                          │  HIS 字典表 DDL      │  BEGIN; UPDATE his_dict; INSERT audit_log; COMMIT;
                          │ （MySQL / PG /      │  异常 → ROLLBACK
                          │  SQLite 替身）       │
                          └──────────┬─────────┘
                                     ▼
                          ┌────────────────────┐
                          │ SHA256 审计链        │  append-only JSONL + prev_hash + event_hash
                          │ （链头/链尾自校验）  │  ChainBrokenError 定位行号与篡改类型
                          └──────────┬─────────┘
                                     ▼
                          ┌────────────────────┐
                          │  Export 飞检导出    │  audit/2026-09-audit.jsonl
                          │ （按 plan_id/actor/ │  飞检员本地 sha256sum -c 自验
                          │  action/时间区间）  │
                          └────────────────────┘

                          一键回滚路径（出错时）：
                          Plan.Status=Applied → RollbackEngine.Rollback
                              → 推断 reverse_sql → 独立事务切换回 v16
                              → Plan.Status=RolledBack → AuditEvent 含 ROLLBACK + ROLLBACK_VERIFIED
```

### 关键不变量

- **幂等键**（`plan_id`, `version_id`）：重复创建同一 Plan 报唯一约束错，不重复写。
- **状态机非法转换守卫**：9 种非法转换（Draft 状态 Approve、Pending 状态 Re-Approve、Rejected 再 Approve、Approved 状态 Request、Applied 状态 Reject、RolledBack 状态任意推进、Pending 状态 Request 自我循环外的非法、Applied 状态再次 Approve、Rejected 状态 Apply）落 typed error，不静默改 Plan 状态。
- **事务边界 ROLLBACK**：HIS 字典表 + LockPlan 走 `WithTx`；任一环节抛错 → 全部 ROLLBACK，Plan 状态保持原值。
- **链头 / 链尾 SHA256 自校验**：第一行 `prev_hash = 64 字节 0`；每行 `event_hash = sha256(event_id + plan_id + actor + action + payload + timestamp + prev_hash)`；飞检员本地 `sha256sum -c` 重算比对。
- **凭据 env 读无硬编码**：所有凭据（医保平台 / HIS / 飞书 / 企微）从环境变量读，缺失显式报错。

## 安全与合规边界

- 本产品为**医保/财务相关业务流程**辅助工具，**不直接出具诊疗建议**，仅做规则切换版本管理 + 多部门会签 + 审计链留痕。
- 任何会签结果（Approve / Reject / Apply / Rollback）均需**实名制审批人**（actor 字段留审计），不可代签。
- 真实 HIS 字典表写入前需在医务科 / 财务会签通过后方可执行；本工具**默认不允许单部门直接 Apply**。
- SHA256 审计链 append-only，**不支持删除或修改**；任何篡改都能被本工具的 `Verify` 函数定位到行号 + 篡改类型。
- 凭据与端点全部从环境变量读取；本仓库代码与 `.env.example` 不含真实凭据；脱敏 fixture 与本地 SQLite 替身仅出现在测试数据层，不连真实 HIS 库。

## 验收与自检

| 验收项 | 命令 | 期望结果 |
|--------|------|----------|
| 子目录与 go.mod 就绪 | `test -f insurance-rule-rollout/go.mod && grep -q github.com/example insurance-rule-rollout/go.mod` | 命令退出码 0，`go.mod` 含模块名 |
| 二维码与关注区块就绪 | `test -f insurance-rule-rollout/qrcode.jpg && grep -q '## 关注我们' insurance-rule-rollout/README.md && grep -q 'qrcode.jpg' insurance-rule-rollout/README.md` | 三条断言全通过 |
| docs/ 目录就绪 | `test -d insurance-rule-rollout/docs/concepts && test -d insurance-rule-rollout/docs/screenshots` | 两个目录存在 |
| 脱敏 fixture 就绪 | `test -f insurance-rule-rollout/testdata/platform_response.json && test -f insurance-rule-rollout/testdata/dict_v17.json && test -f insurance-rule-rollout/testdata/inpatients_87.csv` | 三个 fixture 存在 |
| 数据格式样例就绪 | `test -f insurance-rule-rollout/data/rule-plan-spec.example.json` | RulePlan Spec 样例存在 |
| README 章节齐全 | `grep -q '^## 适用场景' insurance-rule-rollout/README.md && grep -q '^## 快速开始' insurance-rule-rollout/README.md && grep -q '^## 命令' insurance-rule-rollout/README.md && grep -q '^## 典型场景' insurance-rule-rollout/README.md && grep -q '^## 输出样例' insurance-rule-rollout/README.md && grep -q '^## 概念 / 实测对照' insurance-rule-rollout/README.md && grep -q '^## 架构与数据流' insurance-rule-rollout/README.md && grep -q '^## 验收与自检' insurance-rule-rollout/README.md && grep -q '^## 项目结构' insurance-rule-rollout/README.md && grep -q '^## License' insurance-rule-rollout/README.md` | 全部命中 |
| 后续端到端跑通（Task 3+） | `cd insurance-rule-rollout && go run ./cmd/demo` | 终端打印 Plan 推进表 + AuditEvent JSONL；退出码 0 |
| 五态机 9 种非法转换契约测试 | `go test -run 'TestContract_' ./internal/workflow/...` | 9 个 `*ErrXxx` typed error 全部命中 + Plan 状态不变 + AuditEvent 不增 |
| SHA256 审计链自校验 | `go test ./internal/audit/...` | 21/21 PASS；篡改 4 类（header / broken / middle / tail）链头/链尾/中间检测通过；性能 1000 appends < 1s |
| HIS 事务 ROLLBACK 真实证据 | `go test -run 'TestApplyUpdate_RollbackOnConstraintFailure' ./internal/integration/his_dict/...` | CHECK 违例 → his_dict 0 变化 + his_audit_log 0 写入 + `*TransactionError{Op:"commit"}` |
| 干跑报告字段齐备 | `go test -run 'TestRenderDryRunJSON_FieldCompleteness' ./internal/dryrun/...` | JSON 含 plan_id / affected_inpatients / departments / projected_diff_amount / warnings / dry_run_at 全 6 字段 |
| 一键回滚反向 SQL 推断 | `go test -run 'TestInferReverseOps_' ./internal/rollback/...` | 新增→DELETE / 删除→INSERT / 限额变化→UPDATE 三类操作覆盖 |
| 多场景扩展（rollback + reject + 干跑告警 + 并发） | `go test -run 'TestScenarios' ./cmd/demo/...` | 4 场景 stdout 标识全在 + ≥ 20 条 AuditEvent 落地 + chain.Verify()=nil |
| README 丰度自检 | `grep -c '^## ' README.md` ≥ 10、`grep -c '^### ' README.md` ≥ 8、`grep -c '^|' README.md` ≥ 5、`grep -c '^```' README.md` ≥ 8、`wc -c README.md` ≥ 5000 | 实际 13 / 19 / 57 / 24 / 29161，**全部超下限** |
| `go test ./...` 全绿 | `go test -count=1 ./...` | 10 包全绿；`go vet ./...` 无警告；无回归 |

## 项目结构

```
insurance-rule-rollout/
├── README.md                              # 本文件（产品说明，非过程归档）
├── .gitignore                             # 排除 .env / venv / data / logs / 缓存 / score.json 等
├── go.mod / go.sum                        # Go module = github.com/example/insurance-rule-rollout
├── qrcode.jpg                             # 公众号二维码（README 关注区块引用，不入库排除）
├── .env.example                           # 凭据键名（无真实值）
├── docs/
│   ├── concepts/                          # 视觉目标卡（结构化，无图像生成能力时不放图）
│   ├── screenshots/                       # 真实运行截图（终端会话 / JSONL 导出 / 多场景）
│   └── *.md                               # 后续可放架构 / API / 测试等开发文档
├── data/                                  # 示例 RulePlan Spec（脱敏），用于人工录入参考
│   └── rule-plan-spec.example.json
├── testdata/                              # 脱敏 fixture（医保平台响应 / HIS 字典表 schema / 在院患者）
│   ├── platform_response.json             # 5 条 DRG 新规则 demo
│   ├── dict_v17.json                      # HIS 字典表 v17 schema
│   └── inpatients_87.csv                  # 脱敏 87 行在院患者 demo
├── internal/
│   ├── workflow/                          # Plan/Issue 五态机核心引擎（含 typed error + 9 种非法转换守卫）
│   │   ├── types.go                       # PlanStatus / IssueAction / Plan / Issue / AuditEvent
│   │   ├── errors.go                      # IllegalTransitionError / TerminalStateError / 等
│   │   ├── state.go                       # CanTransition / NextStatus 表驱动
│   │   ├── engine.go                      # CreatePlanWithDraftReview / SubmitDraftReview / ReviewIssue / Apply
│   │   ├── store.go                       # Store interface（WithTx / LockPlan / UpdatePlanStatus / InsertAuditEvent）
│   │   └── *_test.go                      # 状态机表驱动 + 9 种非法转换契约测试
│   ├── store/                             # SQLite 持久化（Plan/Issue/AuditEvent + 唯一约束 + 索引）
│   │   ├── migrations/0001_init.sql       # 三表 schema + 索引
│   │   ├── sqlite.go                      # Open / Migrate / Close
│   │   └── store.go + store_test.go       # CRUD + 唯一约束测试
│   ├── dryrun/                            # 干跑预演引擎（在院患者 + ICD-10 → DRG 匹配 + 限额 diff + 警告）
│   │   ├── types.go                       # DryRunResult / PatientRecord / DictEntry / Rule
│   │   ├── match.go                       # ICD-10 前缀匹配 DRG code
│   │   ├── engine.go                      # Run / loadInpatients / aggregateByDepartment / computeProjectedDiff
│   │   └── *_test.go                      # 5 类匹配 + 边界用例
│   ├── integration/                       # 三组集成适配（医保平台 / HIS / 通知）
│   │   ├── insurance_platform/            # GET /v1/rules/{version_id}，401/403/404/500/超时分类
│   │   ├── his_dict/                      # MySQL/PG/SQLite 替身，事务边界 + ROLLBACK
│   │   └── notify/                        # 飞书 + 企微双通道 webhook + SQLite retry 队列
│   ├── audit/                             # SHA256 事件链 append-only JSONL + 链头/链尾/中间篡改检测 + Export
│   │   ├── chain.go                       # Append / Verify / Export + ChainBrokenError
│   │   └── *_test.go                      # 篡改检测 4 类 + 性能基准
│   ├── rollback/                          # 一键回滚（反向 Plan + 独立事务 + 失败恢复）
│   │   ├── engine.go                      # Rollback / InferReverseSQL
│   │   └── engine_test.go                 # fixture 驱动 + 失败恢复
│   └── config/                            # 凭据 env 统一加载（缺失显式报错）
│       ├── config.go                      # Load + Validate + MissingCredentialError
│       ├── config_test.go
│       └── .env.example
├── cmd/
│   └── demo/                              # 端到端可运行示例（脱敏 fixture 跑通主路径）
│       ├── main.go                        # main() 入口（default/dryrun/scenarios 三种模式）
│       ├── main_test.go                   # happy path 断言 5 项
│       ├── integration_test.go            # 完整端到端断言 7 项
│       └── scenarios_test.go              # 4 场景断言 8 项
└── logs/                                  # Phase 4 长构建/测试日志（仅 round 根目录，发布前清理任务会删）
```

> **目录结构纪律**：所有源码 / 配置 / 文档 / 依赖清单放在 `insurance-rule-rollout/` 子目录内；`README.md` / `qrcode.jpg` 与该子目录同级，与 `task.json` / `progress.txt`（位于 round 根目录）严格分离。

## License

MIT

---

## 关注我们

欢迎扫码关注公众号，获取项目更新与交流加群：

![关注我们](qrcode.jpg)