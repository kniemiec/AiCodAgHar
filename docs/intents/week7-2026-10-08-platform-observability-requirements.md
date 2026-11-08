# Platform Observability — Requirements

| | |
|---|---|
| **Status** | Draft |
| **Date** | 2026-10-08 |
| **Owner** | Me |
| **Governing decision** | [ADR-12](../architecture/week7-adr-12-platform-observability-extension.md) (Proposed) — extends [ADR-5](../architecture/week1-adr-5-observability-and-audit-trail.md). Where this document and ADR-12 disagree, resolve the conflict before design starts. |
| **Next step** | A design document (`/wf-design`) derived from this one |

Priority key: **M** = must (first release), **S** = should (first release if feasible, otherwise next), **C** = could (later). Requirement IDs are stable; the design and tests should cite them.

## 1. Goals and non-goals

### 1.1 Goals

- G1. Anyone operating the platform can tell, at any moment, what each task is doing and whether the platform itself is healthy.
- G2. For any single task, the full ordered story — across the control-plane and the launcher — can be retrieved from one identifier.
- G3. Fleet-level behaviour (throughput, durations, implement-review cycles, failures) is measurable over time and drives alerts.
- G4. Failures are visible, classified, and distinguishable from active work and from expected governance outcomes.
- G5. The number of implement-review cycles is measured directly, as the platform's proxy for implementation quality and cost.

### 1.2 Non-goals

- N1. **Agent observability** — prompts, tool requests, model selection, token usage and agent reasoning inside the microVM. A later round; this document only reserves the attachment point (§10).
- N2. Replacing or weakening the ADR-5 audit trail. It remains the tamper-evident record of authority, policy decisions and outcomes.
- N3. Finding severity in loop-quality measurement. Version one distinguishes blocking from non-blocking only.
- N4. Authentication and authorisation for the orchestrator HTTP API. Recorded as a dependency (§9.4), not delivered here.
- N5. Cost in currency. Platform-side cost is measured in resource time (VM-seconds); currency and token cost belong to the agent round.

## 2. Users and the questions they must be able to answer

| User | Must be able to answer |
|---|---|
| **Operator** (on call) | Is the platform up? Is anything stuck? What is failing and why? Is a dependency down? |
| **Developer of the platform** | Where did this task spend its time? Which step failed and with what class of error? Did my change slow a phase down or increase failures? |
| **Platform owner** | How many tasks are we running? How long do they take? How many cycles does a typical task need and is that getting better or worse? How much execution capacity are we using? |
| **Auditor / reviewer** | What exactly happened on task X, in order, including how long it waited for a human? |

## 3. Current state (baseline)

Findings from the code review that this document is written against.

| Capability | Today |
|---|---|
| Structured logs | `log/slog`, JSON via `LOG_FORMAT=json`, `component` tag; `task_id`/`round`/`step_id`/`invocation_id` on step-level lines (`internal/logging`, `coreloop.go`, `executor/credentials.go`, launcher `server.go`/`launch.go`) |
| gRPC and HTTP access logs | Present in both binaries |
| Task status API | `GET /tasks/{id}` returns status, current round, PR number, approval issue, reason, step runs |
| Audit trail | Hash-chained `audit.invocation_audit` (one row per invocation plus `pdp-decision` and `task-outcome` rows) |
| Metrics, health endpoints, tracing, log shipping, central log store | **None** |
| Timing data | `rounds.created_at`, `step_runs.created_at/updated_at`; `tasks.updated_at` is overwritten every round; no terminal timestamp; no phase timing |
| Status history | None — `tasks.status` is one mutable value |
| Failure representation | `taskrunner.fail` logs, stores free text in `reason`, leaves `status=running` for reconciliation |
| Launcher outcome | `completed` / `error` (plus cancellation); no queue-wait, boot or run time |
| Cross-process correlation | IDs passed as fields only; no trace/request ID |

## 4. Definitions

- **Task**: one unit of orchestrator work from acceptance to a terminal outcome.
- **Round**: one pass of the core loop (`coreloop.RunRound`); the round cap is `coreloop.MaxRounds` (currently 5).
- **Phase**: generate, review, approve, publish — as grouped by `coreloop`.
- **Step**: one dispatched unit within a phase, executed through the launcher (agent-stage or deterministic-step).
- **Implement-review cycle**: a round in which the review phase produced at least one blocking finding, causing another round to be needed. **Rounds per task** is the number of rounds started for a task.
- **First-round approval**: a task whose first round ended without outstanding blocking findings.
- **Active time** vs **wait time**: active time is time the platform or an agent is executing; wait time is time a task spends in `waiting_approval` or queued behind the launcher's one-VM-at-a-time limit.
- **Stuck task**: a task in a non-terminal, non-waiting state with no recorded progress for longer than a configured interval.
- **Failure classes** (ADR-12 §4): `platform`, `infrastructure`, `dependency`, `agent`, `policy`. `policy` is a governance outcome, not an error.

## 5. Functional requirements

### 5.1 Task status and timeline

| ID | Pri | Requirement | Acceptance |
|---|---|---|---|
| TS-1 | M | `GET /tasks/{id}` keeps returning current status, round, PR number, approval issue and step runs. | Existing contract tests still pass; additions are backward-compatible except as stated in TS-4. |
| TS-2 | M | Every state transition of a task is recorded in an append-only `orchestrator.task_events` table with timestamp, `task_id`, `round`, optional `step_id`/`invocation_id`, event type and a small structured payload. | For any completed task, the sequence of events reconstructs: acceptance, each round start/finish, each phase start/finish, each step launch/resolve, approval requested/granted, resume, reconciliation actions, terminal outcome. |
| TS-3 | M | Events are written in the same database transaction as the state change they describe. | No test can produce a state change without its event, or an event without its state change, including under injected failure. |
| TS-4 | M | A task that errored is a distinct, first-class state. It is distinguishable from an active task in the database, in `GET /tasks/{id}`, in metrics, and in `task_events`, and carries a failure class and a retryable/terminal indicator. | A task whose round errors is reported as `failed` (not `running`). Startup reconciliation does not pick up failed tasks, retryable or not; retry is manual (TS-9). Tasks merely interrupted by a restart (still `running`) are reconciled as today. |
| TS-5 | M | The status API exposes the task's timeline (or a way to fetch it): at minimum accepted-at, per-round start/finish, and terminal-at. | A caller can compute total duration, per-round duration, and time in `waiting_approval` from the API alone. |
| TS-6 | S | The timeline is queryable by task, by time range, and by event type through the status API or an equivalent read path. | An operator can list the tasks currently `waiting_approval`, with age. |
| TS-7 | M | The migration of existing rows is defined: tasks currently persisted as `running` with an `error:` reason are handled explicitly rather than left ambiguous. | Documented migration with a test on a seeded database. |
| TS-9 | M | A `failed` task flagged retryable can be retried by an operator through an explicit action. There is no automatic retry. A retry is recorded in `task_events` with who/what triggered it. A terminal failure cannot be retried. | Retrying a retryable failed task resumes it and emits a retry event; retrying a terminal one is rejected; a restart never retries a failed task on its own. |
| TS-8 | C | Task-level summary fields are denormalised for cheap fleet queries (e.g. rounds run, terminal-at). | — |

### 5.2 Metrics

All metrics are low-cardinality; `task_id` and `invocation_id` are never metric labels (ADR-12 §1). Allowed labels are drawn from: `component`, `step_type`, `stage`/`step_id` (as configured, not generated), `phase`, `outcome`, `status`, `error_class`, `retryable`, `dependency`, `target_repo`, `method`, `code`. Histograms are used for all durations; averages alone are not acceptable.

| ID | Pri | Requirement | Acceptance |
|---|---|---|---|
| MT-1 | M | Task counters: tasks accepted; tasks reaching each terminal outcome (success, escalated, failed). | Per-outcome counts can be graphed over any window; the totals equal the corresponding `task_events` counts for the same window. |
| MT-2 | M | Task duration histogram (acceptance to terminal), reporting wall-clock time, plus a derived **active time** (wall-clock minus wait time). | p50/p95/p99 available for both. |
| MT-3 | M | Rounds-per-task: a histogram of rounds run per task at terminal outcome; counters for **first-round approval** and **round-cap reached**. | First-round approval rate and cap-hit rate are computable per window. |
| MT-4 | M | Per-round blocking-finding presence: a counter of rounds ending with / without outstanding blocking findings, by round number. | The convergence curve (share of tasks still with blocking findings after round N) is computable. |
| MT-5 | M | Phase duration histogram, by phase. | p95 per phase available. |
| MT-6 | M | Step duration histogram and outcome counter, by step type and configured step id. | Slow or failing step types are identifiable. |
| MT-7 | M | Wait time: time in `waiting_approval` (histogram) and the current number of tasks waiting (gauge, with the oldest age). | Approval turnaround is reportable independent of execution time. |
| MT-8 | M | Failure counters by `error_class`, `retryable`, and — for dependency failures — `dependency`. `policy` outcomes are counted separately and excluded from error rates. | A PDP deny or round cap never increments an error counter. |
| MT-9 | M | Active-tasks gauge by status, and a stuck-task gauge (count of tasks beyond the stuck interval). | Alert in §8 can be driven directly from the gauge. |
| MT-10 | M | Reconciliation and resume counters (tasks reconciled at startup, steps re-resolved, tasks resumed after approval). | A restart storm is visible. |
| MT-11 | M | Launcher: invocation outcome counter (completed, boot failure, timeout, guest error, killed — LA-1), queue depth gauge, queue wait histogram, boot time histogram, guest run time histogram. | Capacity saturation is visible as queue wait growth. |
| MT-12 | M | Launcher: redaction event counter. | A spike is visible and dashboarded. |
| MT-13 | M | Dependency client metrics for Git host, PDP, secrets issuer, launcher and Postgres: request count, error count and latency histogram. | Each dependency has its own error rate and latency view. |
| MT-14 | S | Webhook intake counters: received, rejected (bad signature or unknown event), failed processing. | Dropped webhook deliveries are visible. |
| MT-15 | S | Policy governance counters: PDP decisions by decision and policy outcome; approvals requested/granted/rejected. | Governance activity is visible without being treated as errors. |
| MT-16 | S | Resource-time cost: VM-seconds consumed (boot plus run), by step type. Per-task cost is derived from `task_events`, not a metric label. | Total and per-step-type VM-seconds can be graphed. |
| MT-17 | C | HTTP API request count and latency by route and status. | — |

### 5.3 Errors

| ID | Pri | Requirement | Acceptance |
|---|---|---|---|
| ER-1 | M | Every recorded failure carries an `error_class` from the closed set (`platform`, `infrastructure`, `dependency`, `agent`) and a retryable/terminal indicator. | No failure path writes an unclassified failure; a table-driven test covers each. |
| ER-2 | M | Every failure is visible in all three places consistently: the log line at `error`, a `task_events` row, and the error counter. | A single injected failure produces all three, with matching class. |
| ER-3 | M | `policy` outcomes (PDP deny, approval rejected, round cap reached) are recorded as outcomes, not errors, in events and metrics. | They appear in outcome counters but not error counters. |
| ER-4 | M | Failures from external calls identify the dependency by a bounded name. | The dependency label is always one of the known dependencies. |
| ER-5 | M | Silent failure paths are instrumented, in particular reconciliation errors, webhook processing errors, and failures to persist state or events. | Each has a log, a counter and (where a task is involved) an event. |
| ER-6 | S | Errors returned to the status API remain bounded and contain no secret values. | Existing redaction rules apply. |

### 5.4 Logs

| ID | Pri | Requirement | Acceptance |
|---|---|---|---|
| LG-1 | M | Both binaries emit JSON logs in production. | `LOG_FORMAT=json` is the deployed setting; configuration is documented. |
| LG-2 | M | Every log line produced during task work carries `task_id`; step-scoped lines also carry `round`, `step_id`, `invocation_id`. Lines within an active trace carry `trace_id` and `span_id`. | A review of every log call site in task paths finds the fields present, enforced by a context-aware logger rather than by hand. |
| LG-3 | M | Logs from the control-plane host and the launcher host are shipped to a central Loki. | Querying Loki by `task_id` returns lines from both components, in time order. |
| LG-4 | M | Loki labels are low-cardinality (component, host, level); identifiers such as `task_id`, `invocation_id` and `trace_id` are fields filtered at query time, not stream labels. | Loki stream count remains bounded as task volume grows. |
| LG-5 | M | Retrieving every log line for one task is a documented, single-step operator action. | The runbook procedure takes one identifier and returns the full set. |
| LG-6 | M | Log content obeys the security rules in §9.1. | Review and a negative test (no env values, no secrets, no task description or agent output) pass. |
| LG-7 | M | Launcher teardown, vsock and boot-phase log lines carry the invocation, task, round and step identifiers. | Currently vsock and teardown lines carry no identifiers; this gap is closed. |
| LG-8 | S | HTTP requests carry a request ID that appears on access logs and on the logs of work the request triggers. | A webhook can be followed to the task it triggered. |
| LG-9 | S | Log levels are used consistently (failures at `error`, retryable degradations at `warn`). | Documented level policy. |

### 5.5 Traces

| ID | Pri | Requirement | Acceptance |
|---|---|---|---|
| TR-1 | M | A task is represented as a trace with spans for the task, each round, each phase, each step, and — under each step — the launcher call and the external calls (PDP, secrets issuer, Git host). | A single task can be opened in Tempo and shows the full tree. |
| TR-2 | M | W3C Trace Context is propagated on HTTP and on gRPC from the control-plane to the launcher. | Launcher work appears as child spans of the requesting step, across the two hosts. |
| TR-3 | M | Inside the launcher, spans separate queue wait, jail and image staging, VM boot, guest run, and result extraction/redaction. | Where time went inside one launch is visible. |
| TR-4 | M | A task paused for approval ends its span at the pause; resumed work starts a new trace linked to the original by span link and `task_id`. | No span stays open across an approval wait. |
| TR-5 | M | Spans carry the identifiers in LG-2 and the outcome/error class on failure. | Failed steps show their class on the span. |
| TR-6 | M | Span attributes obey §9.1. | Same checks as LG-6. |
| TR-7 | S | Trace sampling is configurable and defaults to sampling everything (volume permitting). | Configuration documented. |
| TR-8 | M | Spans for each step are the attachment point reserved for agent observability (§10). | Documented extension point in the design. |

### 5.6 Launcher outcomes

| ID | Pri | Requirement | Acceptance |
|---|---|---|---|
| LA-1 | M | The launcher's invocation outcome is a closed set mapping onto the failure classes: at least completed, boot failure, timeout, guest-reported error, killed. | Each outcome is produced by a test; the control-plane maps each to an `error_class`. |
| LA-2 | M | Queue wait, boot time and guest run time are measured separately per invocation and surfaced as metrics and span attributes. | Visible on the dashboard of §7. |
| LA-3 | S | The step timeout that applies (the caller's, or the launcher's default) is recorded on the invocation so timeouts are interpretable. | Executors currently send `TimeoutSeconds: 0`; the effective timeout is visible in events or spans. |
| LA-4 | M | The gRPC change preserves compatibility with the mock launcher and existing clients, or the mock is updated in the same change. | `make test` and the compose e2e suite pass. |

### 5.7 Health and platform self-observability

| ID | Pri | Requirement | Acceptance |
|---|---|---|---|
| HL-1 | M | Both binaries expose liveness and readiness endpoints; readiness reflects database connectivity and, for the control-plane, the ability to reach its fail-closed dependencies as ADR-5 requires. | Endpoints exist and are exercised in tests. |
| HL-2 | M | Both binaries expose a metrics endpoint or export via OTLP, per the ADR-12 pipeline. | Prometheus sees both. |
| HL-3 | M | Build version and relevant configuration (non-secret) are logged at startup and exposed as a metric/resource attribute. | A deployment marker is visible on dashboards. |
| HL-4 | S | ADR-5's signals are implemented as specified: dependency availability, egress denials, kill-switch timing, quota/cost trend, policy-gate consistency check. | Each has a metric and, where ADR-5 says so, an alert. (The policy-gate consistency check compares launches against PDP allows per ADR-5.) |
| HL-5 | S | A periodic audit-chain verification job runs and reports success/failure as a metric. | A broken chain raises an alert. |

## 6. Cross-cutting requirements

### 6.1 Correlation model
CC-1 (M). `task_id` (and `round`, `step_id`, `invocation_id` where applicable) is present in logs, spans and `task_events`; `trace_id`/`span_id` in logs. The same identifiers allow moving between Loki, Tempo and Postgres.

### 6.2 Cardinality
CC-2 (M). No unbounded value is a metric label. Enforced by review and a test that inspects registered metric label sets.

### 6.3 Retention
CC-3 (M). Retention is configured, documented and different per store: audit trail (ADR-5, unchanged), `task_events` (long, since it is per-task history), logs, traces, metrics (shorter). Values are an open question (§12) but the *existence* of a defined retention for each store is a requirement.

### 6.4 Performance and availability of the telemetry path
CC-4 (M). Telemetry failure must not fail or block task execution. If the collector, Loki or Tempo is unavailable, the platform keeps running and drops or buffers telemetry within bounded memory.
CC-5 (M). The one exception is `task_events` writes, which are part of the task's own transaction (TS-3): failure to persist an event fails the transition like any other persistence failure.
CC-6 (S). Instrumentation overhead is small enough that the task throughput targets are unaffected; measured once in the design phase.

### 6.5 Delivery environments
CC-7 (M). The local compose stack includes the collector, Prometheus, Grafana, Loki and Tempo so instrumentation is exercised in development.
CC-8 (M). In production each host (control-plane host, launcher host) has its own export path; resource attributes identify component and host.
CC-9 (S). Dashboards, alert rules and collector configuration are stored in the repository and applied by deployment, not hand-edited in a UI.

## 7. Dashboards

Required views (layout is a design matter):

- **D1. Fleet overview** — tasks accepted/completed/failed/escalated, active tasks by status, stuck tasks, task duration percentiles, current waiting-for-approval count and oldest age, error rate by class.
- **D2. Loop quality** — rounds-per-task distribution, first-round approval rate, round-cap hit rate, convergence by round number, trend over time.
- **D3. Single task** — the drill-down from a `task_id`: ordered timeline from `task_events`, the trace, and the matching log query.
- **D4. Launcher** — queue depth and wait, boot and run time, outcomes, redaction events, VM-seconds.
- **D5. Dependencies** — latency, error rate and availability for Git host, PDP, secrets issuer, launcher, Postgres.
- **D6. Governance** — PDP decisions, approvals requested/granted/rejected, approval turnaround, policy-gate consistency, egress denials (ADR-5).

## 8. Service-level objectives and alerts

Alerts derive from a small set of objectives (ADR-12 §8). **The thresholds below are proposals for you to set**; they exist so the design has concrete numbers to build against.

| ID | Objective (proposed) | Alert condition (proposed) | Severity |
|---|---|---|---|
| SLO-1 | No task is stuck | A non-terminal, non-waiting task has made no recorded progress for 30 minutes | Page |
| SLO-2 | Tasks complete reliably | Failure rate (`platform` + `infrastructure` + `dependency` + `agent`, excluding `policy`) over 1 hour exceeds 10% of started tasks | Page |
| SLO-3 | Launcher is not saturated | Launcher queue wait p95 over 15 minutes exceeds 10 minutes | Warn |
| SLO-4 | Dependencies are healthy | Any fail-closed dependency (PDP, secrets issuer) unreachable beyond 30 seconds (ADR-5), or any dependency error rate above 5% for 10 minutes | Page for fail-closed, warn otherwise |
| SLO-5 | Approvals do not rot | A task has been in `waiting_approval` beyond a configured age (proposal: 24 hours) | Notify (not a page) |
| SLO-6 | Loop quality is stable | Round-cap-hit rate over 24 hours exceeds a configured level (proposal: 20% of tasks) | Warn |
| SLO-7 | Audit trail is intact | Audit-chain verification fails | Page |
| SLO-8 | Launch bypass detection | Launches exceed PDP allows (ADR-5) | Page |
| SLO-9 | Observability pipeline is healthy | Collector, Loki, Tempo or Prometheus ingestion stopped for 10 minutes | Warn |

Requirement AL-1 (M): alert rules for SLO-1, 2, 4, 5, 7, 8 and 9 exist before the first release is considered complete; SLO-3 and SLO-6 are (S).
Requirement AL-2 (M): every alert links to a runbook entry stating what it means and the first diagnostic step.
Requirement AL-3 (M): `policy` outcomes never page.

## 9. Non-functional requirements

### 9.1 Security and privacy
SEC-1 (M). No secret values in logs, spans, metric labels or `task_events`. Environment maps are logged by key only (`logging.Keys`).
SEC-2 (M). Task descriptions, agent output, event logs from the guest, and PR/issue bodies are not copied into logs, span attributes or metric labels. Identifiers and bounded metadata only.
SEC-3 (M). The launcher's literal-secret redaction (ADR-10) is unchanged and applies before any guest-derived value could enter a log or span.
SEC-4 (M). Error text recorded in events and the status API is bounded in length and passes the same secret checks.
SEC-5 (M). The audit trail's integrity does not depend on the telemetry pipeline; the pipeline having no write path to the audit schema is part of the design.

### 9.2 Access control
SEC-6 (M). Grafana, Loki, Tempo and Prometheus are not exposed on an unauthenticated public path in production.
SEC-7 (S). Access is role-based: operators and developers can read all; the dashboards expose repository names and task identifiers and are treated as internal.

### 9.3 Operability
OP-1 (M). Every new component and setting is documented in `docs/components/orchestrator.md` and `container-launcher.md` in the same change (those docs are living; CLAUDE.md requires it).
OP-2 (M). A runbook covers: finding a task's logs/trace/timeline, interpreting each alert, and what to do when the pipeline itself is down.

### 9.4 Dependencies on other work
- Authn/authz for the orchestrator HTTP API (N4) — required before production exposure of status and task data.
- A review-result schema change for finding severity (N3) — only for the later extension.
- The agent-observability ADR and design (N1).

### 9.5 Compatibility
CMP-1 (M). Changes to the status API and the launcher gRPC contract are backward-compatible or accompanied by a documented break, with mocks and generated code updated in the same change.
CMP-2 (M). Migrations follow the existing `deploy/migrations/` conventions (idempotent, role grants stated).

## 10. Boundary with agent observability

This document delivers, for the later round to build on:
- the per-step span as the attachment point (TR-8);
- `task_events` with an open event-type vocabulary and a payload column;
- the correlation model (CC-1).

It deliberately does not specify what is captured inside the guest.

## 11. Traceability to the original request

| Original ask | Requirements |
|---|---|
| 1. Tracking task status | TS-1 … TS-8, MT-9, D1, D3 |
| 2. Metrics — tasks over time, phase durations, average loops | MT-1 … MT-7, D1, D2 |
| 3. Tracking errors | TS-4, ER-1 … ER-6, MT-8, SLO-2 |
| 4. Central logs for one task | LG-1 … LG-9, TR-1 … TR-2, D3 |
| Additional aspects from the review | Launcher (LA-*, MT-11, D4), dependency health (MT-13, D5), wait vs active time (MT-2, MT-7), governance (MT-15, D6), cost (MT-16), stuck tasks (MT-9, SLO-1), security of telemetry (§9) |

## 12. Open questions

### 12.1 Decided before design

| # | Decision |
|---|---|
| Q3 | **Failure state.** `failed` is a real task status. Each failure carries an `error_class` (ER-1) and a `retryable` flag derived from the class by a fixed mapping (transient causes such as `dependency` and `infrastructure` are retryable; `platform` and deterministic `agent` failures are terminal). **Retry is manual only**, triggered by an operator (TS-9); there is no automatic retry and reconciliation does not touch failed tasks. Exact status and enum names are settled in the design. |
| Q5 | **No back-fill.** History is not reconstructed from `rounds`/`step_runs`. The migration inserts one synthetic `migrated` event per existing task (timestamp = migration time, payload = status and round at that moment, note that earlier history was not recorded), so every task has a non-empty timeline. Tasks persisted as `running` with an `error:` reason become `failed`; the design defines their class and retryable flag (ER-1 has no "unknown" class). |
| Q6 | **`target_repo` label** is accepted, restricted to a few task-level metrics (task counters, rounds-per-task), taken from a configured bounded set with `other` as overflow. Not used on histograms, step or dependency metrics. Revisit if the repository count outgrows the set. |
| Q11 | **Release scope.** Slice 1 (foundation) and slice 2 (pipeline) are designed together and implemented as separate PRs in order. The release boundary is confirmed after the design sizes them; if both do not fit one release, release one is slice 1 plus 2 anyway as the starting target and tracing/dashboards follow. |

### 12.2 To be decided in the design

- **Q4. `task_events` catalogue** — exact event types and payloads.
- **Q7. Timeline delivery** — inline in `GET /tasks/{id}` or a separate endpoint (TS-5, TS-6).
- **Q8. Collector topology** — one collector per host or a shared one, and the transport between hosts (check against ADR-12 first; drop if already decided).
- **Definition of "progress"** for SLO-1 (proposal: latest `task_events`/`step_runs` timestamp), which obliges long-running activity to emit events more often than the stuck interval.
- **Configurable in-process thresholds** — the stuck interval (MT-9) and approval age (SLO-5) are computed by the platform, so the design makes them configuration.
- **Failure-rate rule at low volume** (SLO-2) needs a minimum-volume guard.
- **Retry mechanism** (TS-9) — operator-facing surface (API endpoint or CLI), retry-count bound, and interaction with the missing API authn (N4).
- **`task_events` volume** — whether it needs partitioning.

### 12.3 Deferred to implementation or deployment

- **Q1. Threshold values** in §8 (alert-only thresholds are tuned in alert rules).
- **Q2. Retention values** for each store (CC-3 requires that each has one).
- **Q9. Sampling** (TR-7 defaults to 100%); revisit with real volume.
- **Q10. Authentication for observability UIs** — interim measure; must be closed before production exposure (SEC-6). Local compose is unaffected.

## 13. Suggested delivery slicing (for the design to confirm)

1. **Foundation** — correlation model (context logger), `task_events`, failure state and classification, launcher outcome enumeration, health endpoints, basic metrics.
2. **Pipeline** — collector, Prometheus, Loki, Tempo, Grafana in compose and in deployment; log shipping from both hosts.
3. **Tracing** — spans end to end, propagation across gRPC.
4. **Dashboards and alerts** — D1–D6, SLO alerts, runbook.
5. **Hardening** — retention, access control, audit-chain verification job, ADR-5's remaining signals.
