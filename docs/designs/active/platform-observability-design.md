# Platform Observability — Design

| | |
|---|---|
| **Status** | Draft — owner decisions recorded in §14; ready for implementation planning |
| **Date** | 2026-10-09 |
| **Owner** | kniemiec |
| **Purpose** | Close the gap between what ADR-5 asked for and what exists: no timing history, no failure state, no metrics/health/traces, no central logs. The fix is an append-only `task_events` table written in the same transaction as state changes, a first-class `failed` status with a failure taxonomy, an OpenTelemetry pipeline (Collector → Prometheus / Loki / Tempo / Grafana), and an additive launcher outcome contract. |
| **Derived from** | [`docs/intents/week7-2026-10-08-platform-observability-requirements.md`](../../intents/week7-2026-10-08-platform-observability-requirements.md) (requirement IDs cited as `TS-2`, `MT-5`, …) under [ADR-12](../../architecture/week7-adr-12-platform-observability-extension.md) (Proposed). |

## Goal

When this is built: (1) every task state change in `taskrunner`/`coreloop` also inserts a row in `orchestrator.task_events` inside the same Postgres transaction, so a task's ordered story and its wait/active time are queryable via `GET /tasks/{id}/events`; (2) a round that errors moves the task to `status=failed` with an `error_class` and `retryable` flag — replacing the "log it and leave it `running`" behaviour of `Runner.fail` (`taskrunner.go:215-221`) — and only an operator action (`POST /admin/tasks/{id}/retry`) resumes it; (3) both binaries emit context-enriched JSON logs, OTLP metrics and traces to a per-host Collector, with logs landing in Loki and every line retrievable by `task_id`; (4) the launcher reports one of five enumerated outcomes plus queue-wait, boot and guest-run durations on `LaunchResponse` (`launcher.proto:33-39`), delivered even when `Launch` returns an error; (5) both binaries expose `/healthz` and `/readyz`; (6) dashboards D1–D6, SLO alerts and a runbook live in the repo and come up with `make compose-up`. "Done" is the checklist in *Success Criteria*.

## Context — Why This Shape

**What the code does today (each claim from a read, this session).**

- A failed task stays `running`. `Runner.fail` logs, stores `"error: "+err` in `reason`, sends on `done`, and leaves status alone "so startup reconciliation still retries" (`internal/control-plane/taskrunner/taskrunner.go:210-221`). `Reconcile` selects `status = 'running'` (`postgres_store.go:246-265`), so a failed task is retried on every restart with no limit, and nothing can count or page on it.
- Timing is unrecoverable: `tasks.updated_at` is overwritten by every `UpdateTaskState` (`postgres_store.go:85-105`); `rounds` and `step_runs` have only `created_at`/`updated_at` (`002_orchestrator_schema.sql`, `004_task_state.sql`); coreloop logs `phase started/finished` (`coreloop.go:234,240,293`) but persists nothing.
- `coreloop.RunRound` is deliberately pure and has no store: its only persistence seams are the narrow `StepRunRecorder` and `PRRecorder` interfaces (`coreloop.go:67-104,123-135`). Any phase-level event has to follow that idiom.
- Every `TaskStore` method is a single autocommit statement (`postgres_store.go:38-295`; `db.ExecContext`, no `BeginTx`), so "event in the same transaction" requires restructuring the store.
- The launcher drops its own outcome on failure: `Launch` returns `resp, status.Errorf(codes.Internal, …)` on any boot error (`cmd/container-launcher/server.go:203-209`), and gRPC does not transmit a response body alongside a non-OK status. The control-plane executor on error never sees `Status` (`executor/agent_stage.go:63-67`). `timed_out` and `killed` exist as strings (`launch.go:76-86`, `launch.go:378`) but the only thing that reaches the caller is an opaque `Internal`.
- All `bootOutcome{status:"error"}` returns before the guest wait (`launch.go:125-283`) are boot failures; errors from `waitForAgentCompletion`/`waitForCommandCompletion` (`launch.go:332-475`) are guest-side. The enumeration can therefore be derived without new probes.
- `startedAt` is taken before the VM-slot wait (`server.go:168-176`), so `StartedAt` includes queue time and queue time is not separately visible.
- Executors always send `TimeoutSeconds: 0` (`agent_stage.go:61`), so the effective timeout is the launcher default **1800 s** (`config.go:54`, `launch.go:69-74`). This equals the proposed 30-minute stuck interval (SLO-1) — see §4 heartbeat.
- Logs: `slog` with `component` attr (`internal/logging/logging.go:17-26`); correlation fields attached ad hoc via `slog.With` in `coreloop.go:180`, `coreloop.go:442`, `executor/credentials.go:56-58`, launcher `server.go:123`. Not covered: launcher teardown (`launch.go:575-581`) and vsock lines (`vsock.go:123,189,202`) carry no IDs; `webhook.go`'s failure paths use stdlib `log.Printf` (inside `handleForgejoWebhook`, `webhook.go:66-207`) without task context; `Reconcile`'s goroutine logs `slog.Error(...)` without ctx (`taskrunner.go:397`).
- **Sensitive content is already in logs** (conflicts with SEC-2/LG-6): launcher logs `prompt_prefix` (500 bytes of the rendered prompt, which embeds the task description) at debug (`server.go:143`), the structured result at debug (`server.go:211`, `agent_stage.go:76-77`), and a 4 KB tail of the guest event log at **warn** (`server.go:206`, with the comment at `server.go:215-219` explaining it is "often the only copy").
- HTTP clients for PDP/secrets use `http.DefaultClient` (`policyclient/client.go:22-25`); the launcher client has a hand-rolled unary interceptor (`launcherclient/client.go:42,49-68`). The PDP/secrets OpenAPI contracts (`api/openapi/*.yaml`) define no health endpoint.
- The control-plane has no signal handling: `http.ListenAndServe` in `main.go:121` and `log.Fatalf` paths mean deferred closers do not run on exit.

**Alternatives considered.**

1. *Metrics emitted at call sites vs. derived from committed events.* Call-site metrics are the obvious path but duplicate every fact that `task_events` also records, and nothing stops them drifting (MT-1 requires totals equal the event counts). **Chosen:** task-/phase-/step-level metrics are derived by one `EventSink` from events *after* their transaction commits (ADR-12 §3 says the same events feed metrics and span events). Cost: a crash between commit and observe loses one counter increment; counters reset on restart anyway, and the events table is the source of truth. Metrics with no event (dependency calls, launcher internals, gauges, webhook intake) are emitted directly.
2. *Per-call transactions vs. a unit-of-work.* A `store.Tx(func(tx) error)` callback would be more general, but every state transition here is already one logical write; the minimal change is **variadic events on the existing methods**, executed in one `BeginTx` by `PostgresTaskStore`. Rejected: exposing a transaction to `coreloop`, which would end its purity.
3. *Operator retry surface.* API endpoint on the public mux vs. CLI against Postgres vs. a **separate admin listener**. A CLI that flips `status` in the DB cannot start a runner goroutine in the live process. A public `POST …/retry` is an unauthenticated state-changing endpoint (N4: no authn exists). **Chosen:** admin mux on its own listener, default `127.0.0.1:8090`, retry and the TS-6 list live there. This keeps N4 un-delivered but contained.
4. *Log transport:* OTLP logs from the process (slog → OTel bridge) vs. scraping stderr/journald. **Chosen:** OTLP via bridge, with stderr kept as an always-on second sink. One path works identically in compose (macOS) and on the Linux hosts, and trace/span IDs ride along. Fallback if the Go OTel logs SDK proves too unstable: Collector `journald`/`filelog` receiver (Named Risks).
5. *Collector topology (Q8):* one Collector per host (agent mode), each pushing **outbound** — logs to Loki, traces to Tempo, metrics via Prometheus `remote_write`. The KVM launcher host then needs no inbound telemetry port and no cross-host scrape. Rejected: single shared Collector (launcher host would depend on cross-host availability for local buffering).
6. *Release scope:* the intent's Q11 and AL-1 disagree; the owner decided (§14 item 1) that release 1 = PRs A–D and release 2 = PRs E–G.

## Design

### 1. Package map

| New / changed | Role |
|---|---|
| `internal/control-plane/failure` (new, leaf) | `Class` (`platform|infrastructure|dependency|agent`), `ClassifiedError{Class, Dependency, Err}`, `Classify(err) (Class, retryable bool, dependency string)`. Leaf package so `clients/*`, `executor`, `coreloop`, `taskrunner` can import it without cycles. Unclassified errors resolve to `platform`/terminal (ER-1: nothing is ever "unknown"). |
| `internal/control-plane/domain/events.go` (new) | `TaskEvent` struct and event-type constants (§3). |
| `internal/telemetry` (new) | OTel SDK bootstrap (`Setup(ctx, component) (shutdown func)`), `Metrics` catalogue, `EventSink`, bounded-label helper, no-op fallback. |
| `internal/control-plane/instrumented` (new) | Decorators: `Policy`, `Secrets`, `Git`, `Launcher`, `Audit`, `Store`. Each times the call, records dependency metrics (MT-13), opens a span, and wraps returned errors with `failure.ClassifiedError`. The `Store` decorator also calls `EventSink.Observe` after a successful commit. |
| `internal/logging` (changed) | Context-aware handler (§5). |
| `internal/buildinfo` (new) | `Version`, `Commit` (ldflags, fallback `debug.ReadBuildInfo`). |
| `deploy/observability/` + `deploy/docker-compose.observability.yml` (new) | Collector, Prometheus, Alertmanager, Loki, Tempo, Grafana configs/dashboards/rules (§11). |

### 2. Failure as a first-class state (TS-4, TS-7, TS-9, ER-1, ER-3)

**Vocabulary (settles Q3).** `domain.TaskStatusFailed = "failed"` beside the four existing constants (`domain.go:96-101`). New `tasks` columns: `error_class text`, `retryable boolean`, `retry_count integer NOT NULL DEFAULT 0`, `finished_at timestamptz` (set on `stop_success`, `stop_escalate`, and a **terminal** `failed`; NULL otherwise — this is TS-8's cheap part and TS-5's terminal-at). `TaskState` gains `ErrorClass string`, `Retryable bool`, `RetryCount int`, `FinishedAt *time.Time`. A `CHECK (status IN (…))` constraint is added (the vocabulary was previously only a comment, `004_task_state.sql`).

**Fixed mapping** (class → retryable): `dependency`, `infrastructure` → retryable; `platform`, `agent` → terminal. Implemented once in `failure.Classify`.

**Policy outcomes are not failures** (ER-3). `coreloop.RoundResult` gains `ReasonKind string` set where `Reason` is set today: `policy_denied` (`coreloop.go:198`), `invalid_phase_config` (`coreloop.go:208`; a bad `.harness` config is a governance/config outcome, not a platform fault — flagged in §14). The runner sets `round_cap` (`taskrunner.go:196-203`) and `approval_rejected` (`taskrunner.go:339-344`). These stay `stop_escalate`, appear as `task.finished` payload `reason_kind`, and feed `harness_policy_outcomes_total`, never the failure counter.

**Failure paths and their class** (the ER-1 table test enumerates exactly these):

| Path | Site | Class (dependency) | Retryable |
|---|---|---|---|
| PDP evaluate error | `coreloop.go:182-185` | dependency (`pdp`) | yes |
| Unknown PDP decision value | `coreloop.go:201-202` | platform | no |
| Audit write (PDP / step / task-outcome) | `coreloop.go:187`, `agent_stage.go:86`, `taskrunner.go:247-250` | platform | no |
| Branch / PR / approval-issue creation | `coreloop.go:218,249,283` | dependency (`git_host`) | yes |
| Credential mint | `executor/credentials.go:32-37` | dependency (`secrets_issuer`) | yes |
| Launch → `BOOT_FAILURE`, `KILLED` | `agent_stage.go:63-67` | infrastructure | yes |
| Launch → `TIMEOUT`, `GUEST_ERROR` | same | agent | no |
| Launch → gRPC `Unavailable`/`DeadlineExceeded`, no outcome detail | same | dependency (`launcher`) | yes |
| Launch → `InvalidArgument`/`Internal` from render (bad `payload_ref`) | `server.go:125-148` | platform | no |
| `ExtractLogs` error | `agent_stage.go:72-75` | dependency (`launcher`) | yes |
| Structured result unparseable | `agent_stage.go:81-83` | agent | no |
| `RecordLaunched`/`RecordCompleted`/`EnsureRound`/`UpdateTaskState`/`SetTaskPRNumber` | `coreloop.go:444,462`, `taskrunner.go:128,157,174` | platform | no |
| No executor for step type; unexpected round outcome | `coreloop.go:436`, `taskrunner.go:191` | platform | no |
| Launched step unreconcilable at startup | `taskrunner.go:469-484` | dependency (`launcher`) | yes |

The last row is a **behaviour change**: today it ends the task as `stop_escalate` and writes a `task-outcome` audit row. It becomes `failed`/retryable and writes no audit row (the audit trail records outcomes of authority, not infrastructure faults — N2 is unaffected; the eventual real outcome after retry is still audited). `taskrunner_test.go` cases asserting the escalation must be updated.

**`Runner.fail` rewrite.** It builds a `domain.FailureRecord{TaskID, Round, Class, Retryable, Reason}` and calls a new `TaskStore.FailTask(ctx, rec, events…)` which, in one transaction, sets `status='failed'`, `error_class`, `retryable`, `reason`, `finished_at` (only if terminal), `updated_at`, and appends `task.failed`. It does **not** overwrite `approval_issue_number`, `approved_by`, `prior_findings`, or `current_round` (the same reason `SetTaskReason` exists today, `store.go:41-45`; `SetTaskReason` is removed once nothing uses it). `reason` is bounded to 512 bytes (SEC-4) via a new `logging.Truncate`-based `failure.Bound`. If `FailTask` itself cannot be written (Postgres down), `fail` logs at error, increments `harness_persistence_failures_total`, and the task stays `running` — recovered by restart reconciliation and visible to the stuck-task gauge (ER-5).

**Reconcile / Resume interaction.** `ListRunningTaskIDs` (`postgres_store.go:246`) already filters `status='running'`, and `ReconcileTask` no-ops for any other status (`taskrunner.go:422-424`), so failed tasks are untouched by restarts with no code change there; a test pins it. `RecordApproval`/`RecordRejection` already require `waiting_approval` (`taskrunner.go:312,335`). A `Resume` (post-approval goroutine, `webhook.go:186-198`) whose `runLoop` errors now ends `failed` instead of `running`.

**Retry (TS-9).** `Runner.Retry(ctx, taskID, operator, remoteAddr string) (Result, error)`: loads the task; rejects (`ErrNotRetryable`, → HTTP 409) unless `status=failed AND retryable AND retry_count < MaxRetries` (`TASK_MAX_RETRIES`, default 3 — the bound for Q-retry); in one transaction, as a compare-and-set (`UPDATE … WHERE id=$1 AND status='failed' AND retryable AND retry_count < $max`; zero rows affected → `ErrNotRetryable`, so concurrent retries cannot start two runners), sets `status='running'`, clears `error_class/retryable/reason`, `retry_count+1`, appends `task.retried {triggered_by, remote_addr, retry_count, previous_class}`; then continues exactly like `ReconcileTask` (`taskrunner.go:433-445`) — `loadAlreadyCompleted` for `current_round+1`, `runLoop`. Because `approved_by` is untouched, a task that failed after approval re-enters the same round with approval still verifiable. Surface: `POST /admin/tasks/{id}/retry` with body `{"operator":"<name>"}` (required, ≤64 bytes) on the **admin listener** (`ADMIN_ADDR`, default `127.0.0.1:8090`; compose publishes `127.0.0.1:8090:8090`). The operator string is self-declared and recorded alongside the remote address; it is not authentication (N4). A terminal failure returns 409 and emits nothing. The handler returns **202** immediately after the compare-and-set and runs the continuation in a goroutine (like `POST /tasks`); the outcome is observed through `GET /tasks/{id}`. Nothing calls `Retry` except that handler.

**Migration `006_task_events_and_failure.sql` (TS-7, Q5, CMP-2).** Idempotent in the style of 004 (`ADD COLUMN IF NOT EXISTS`, `CREATE TABLE IF NOT EXISTS`, `DROP CONSTRAINT IF EXISTS` + `ADD`). Order: (a) add columns and the status CHECK; (b) `UPDATE orchestrator.tasks SET status='failed', error_class='platform', retryable=true, retry_count=0 WHERE status='running' AND reason LIKE 'error:%'` — `platform` because ER-1 has no unknown class, `retryable` because that is the retry the old behaviour promised and terminal would strand them (flagged in §14); (c) backfill `finished_at = updated_at` for `stop_*` rows (best available; `updated_at` of a terminal row is its terminal write); (d) `INSERT … SELECT` one `task.migrated` event per task lacking any event (`WHERE NOT EXISTS`), payload `{"status":…,"round":…,"history_recorded":false}`. Ordering note in `docs/local-development.md`: stop the control-plane before `make migrate` — a live `runLoop` could otherwise be flipped to `failed` mid-flight by (b). Tested per *Testing Strategy*.

### 3. `task_events` (TS-2, TS-3, TS-5; settles Q4, Q7 and the volume question)

```sql
CREATE TABLE IF NOT EXISTS orchestrator.task_events (
    id            bigserial PRIMARY KEY,
    task_id       text        NOT NULL REFERENCES orchestrator.tasks (id),
    at            timestamptz NOT NULL DEFAULT clock_timestamp(),
    round         integer     NOT NULL DEFAULT 0,
    step_id       text,
    invocation_id text,
    type          text        NOT NULL,
    payload       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    trace_id      text,
    span_id       text
);
CREATE INDEX IF NOT EXISTS task_events_task_idx ON orchestrator.task_events (task_id, id);
CREATE INDEX IF NOT EXISTS task_events_type_at_idx ON orchestrator.task_events (type, at);
REVOKE UPDATE, DELETE ON orchestrator.task_events FROM orchestrator_app;
```

The `REVOKE` is required because `002_orchestrator_schema.sql` set `ALTER DEFAULT PRIVILEGES … GRANT SELECT, INSERT, UPDATE, DELETE` for `orchestrator_app`, which would otherwise apply to this table; the intent (ADR-12 §3) is INSERT/SELECT only. `at` uses the database clock (single time source; durations inside payloads come from in-process monotonic timers). `trace_id`/`span_id` give CC-1's move between Postgres and Tempo. **Volume:** ~10–40 rows per round, plus 12-per-hour heartbeats per in-flight step; at current task volume no partitioning (the two indexes suffice); revisit with a BRIN on `at` if retention deletes become heavy. Retention deletion needs a role other than `orchestrator_app` — defined in PR G (migration `008_retention_role.sql`).

**Catalogue** (`domain/events.go`; vocabulary stays open per §10 of the intent):

| Type | Emitted by | Payload |
|---|---|---|
| `task.accepted` | `Store.CreateTask` (same tx) | `target_repo`, `risk_tier`, `base_branch`, `request_trace_id?` |
| `task.migrated` | migration 006 | see §2 |
| `task.reconciled` | `ReconcileTask` before `runLoop` | `from_status`, `resume_round`, `completed_steps` |
| `round.started` | `EnsureRound` when the INSERT actually inserted (now returns `created bool`) | `{}` |
| `round.resumed` | `runLoop` when `EnsureRound` found the row | `reason: reconcile|approval|retry` |
| `policy.decided` | coreloop after `auditPDPDecision` | `decision`, `bundle_version`, `resolved_steps`, `reason_kind?` |
| `phase.started` / `phase.finished` | coreloop (`coreloop.go:234/240/293`) | `phase`, `steps` / `phase`, `blocking_findings`, `duration_ms` |
| `step.launched` | `Store.RecordStepLaunched` (same tx) | `step_type`, `phase`, `blocking`, `effective_timeout_s?` |
| `step.heartbeat` | `fanOut` ticker (§4) | `elapsed_s` |
| `step.resolved` | `Store.CompleteStepRun` (same tx) | `outcome: completed|failed`, `exit_code`, `has_finding`, `duration_ms`, `launcher_outcome`, `queue_wait_ms`, `boot_ms`, `run_ms`, `error_class?`, `dependency?` |
| `step.reconciled` | `resolveLaunchedStep` | `resolved: true|false` |
| `approval.requested` | `UpdateTaskState` for `waiting_approval` | `issue_number` |
| `approval.granted` / `approval.rejected` | `RecordApproval` / `RecordRejection` | `by` |
| `round.finished` | runner after `UpdateTaskState` | `outcome`, `blocking_findings` (bool), `blocking_steps` |
| `task.failed` | `Store.FailTask` | `error_class`, `retryable`, `dependency?`, `error` (≤512 B), `wall_ms?` (terminal only) |
| `task.retried` | `Runner.Retry` | §2 |
| `task.finished` | `Runner.finish` | `outcome: success|escalated`, `reason_kind`, `rounds`, `wall_ms`, `wait_ms` |

**Atomicity implementation (TS-3).** `TaskStore` methods that change tasks/rounds/step_runs gain a trailing `events ...domain.TaskEvent` (`UpdateTaskState`, `FailTask`, `EnsureRound`); `CreateTask`, `RecordStepLaunched`, `CompleteStepRun` derive their own event internally. `PostgresTaskStore` runs state SQL and `INSERT INTO task_events` in one `BeginTx`/`Commit` (new helper `withTx`). `FakeTaskStore` appends to an in-memory slice under the same mutex as the state write, and exposes `Events(taskID)`. Events that do **not** accompany a row change (`phase.*`, `policy.decided`, `step.heartbeat`, `step.reconciled`) go through a standalone `Store.AppendEvents`; for them TS-3's "same transaction" is vacuous (no state row exists to diverge from), and CC-5 still applies — a failed non-heartbeat append fails the round like any persistence error. `coreloop.Dependencies` gains `Events EventAppender` (narrow interface mirroring `Recorder`, `coreloop.go:123-135`, nil = no-op) and `taskrunner.NewRunner` wires it from the store exactly as it wires `Recorder` (`taskrunner.go:54-58`).

**API (Q7 resolved: separate endpoint).**

- `GET /tasks/{id}` (`handler.go:149-171`) stays; `taskStatusResponse` (`handler.go:68-76`) gains additive fields `accepted_at`, `finished_at`, `error_class`, `retryable`, `retry_count` (all `omitempty`). The only contract change is the new status value `failed` (TS-4, documented in `orchestrator.md` §4).
- `GET /tasks/{id}/events?type=&since=&until=&after_id=&limit=` → `{"events":[…],"next_after_id":N}`; `limit` default 200, max 1000; events in `id` order. This satisfies TS-5 (acceptance, per-round, terminal and approval-wait derivable) and TS-6's per-task and time-range queries.
- Admin listener: `GET /admin/tasks?status=waiting_approval` → `[{task_id,status,since,age_seconds}]` (TS-6; `since` = latest `approval.requested` for waiting, latest `task.failed` for failed), and the retry endpoint (§2). Admin routes are read-only except retry.

### 4. Progress, stuck tasks, heartbeat, wait time

*Progress* (open question) = the latest `task_events.at` for the task. Because an agent step legitimately runs up to the 1800 s launcher timeout (`config.go:54`) — equal to the 30-minute stuck default — `fanOut`'s per-step goroutine (`coreloop.go:432-472`) runs a ticker while `exec.Execute` is in flight and appends `step.heartbeat` every `HEARTBEAT_INTERVAL` (default 5 min = stuck/6). A failed heartbeat append logs at warn and is not fatal (it describes no state change).

*Stuck interval* is configuration (`STUCK_TASK_INTERVAL`, default `30m`), consumed by the DB-polling gauge callback (below). The approval-age threshold (SLO-5) lives in the alert rule, not in-process: the oldest-wait gauge already carries the age, so a second in-process threshold would duplicate it (departure from the intent's "computed by the platform", §14).

*Wait vs. active (MT-2, MT-7).* At `task.finished` the runner loads the task's events (`TaskStore.ListEvents`) and sums wait = Σ(`approval.granted` or `approval.rejected` − the preceding `approval.requested`) + Σ(`task.retried` − `task.failed`); `wall = finished − accepted`, `active = wall − wait`. Both go in the `task.finished` payload (`wall_ms`, `wait_ms`) so metrics and the API agree. A **terminal** `failed` task never emits `task.finished`; its `task.failed` payload carries `wall_ms` (terminal only) and the duration histogram observes `outcome=failed` from it. Active time is not recorded for failed tasks. A retryable failure observes no duration (the task is not finished). Launcher queue wait is deliberately **not** subtracted from active time: queue waits of concurrent steps overlap, so a sum would over-subtract; it is reported separately (MT-11) — departure from the intent's §4 definition, flagged in §14.

### 5. Context-aware logging (LG-1, LG-2, LG-6, LG-7, LG-8, LG-9)

`internal/logging` gains:

```go
type taskAttrs struct{ TaskID string; Round int; StepID, InvocationID string }
func WithTask(ctx context.Context, taskID string) context.Context
func WithRound(ctx context.Context, round int) context.Context
func WithStep(ctx context.Context, stepID, invocationID string) context.Context
// Setup installs: stderr handler (text|json, as today) wrapped by contextHandler,
// plus the OTel log bridge handler when OTEL_EXPORTER_OTLP_ENDPOINT is set.
```

`contextHandler.Handle` appends `task_id`/`round`/`step_id`/`invocation_id` from ctx, and `trace_id`/`span_id` from `trace.SpanContextFromContext(ctx)`; an `slog.Handler` fan-out sends each record to stderr and to the bridge. `Setup` additionally adds `host` and `version` (`buildinfo`). Call-site changes: `Runner.runLoop` does `ctx = logging.WithTask(ctx, task.ID)`, `RunRound` adds `WithRound`, `fanOut`'s goroutine adds `WithStep`; the launcher's `Launch` handler builds the same context from `req.TaskId/StepId/InvocationId` (the launcher receives IDs as fields, not ctx — `server.go:123`) and passes it down into `booter.Launch`, `waitFor*Completion`, `vsock`, and `teardown(ctx, jailDir)` (closes LG-7: `launch.go:575-581`, `vsock.go:123,189,202`). Explicit `"task_id", task.ID` attrs are then deleted from those call sites (slog does not dedupe). Replace the `log.Printf` calls in `webhook.go` with `slog.*Context` at the levels the policy below sets. Level policy (LG-9): failures that change task outcome → `error`; retryable degradations (credential revoke failure, heartbeat append failure, reconcile `ExtractLogs` failure) → `warn`; lifecycle → `info`; per-RPC chatter → `debug`.

**Enforcement of LG-2.** `internal/logging/callsites_test.go` parses (`go/parser`) `internal/control-plane/**`, `cmd/control-plane/**`, `cmd/container-launcher/**`, `internal/launcher/**` and fails on any call to `slog.{Debug,Info,Warn,Error}` (non-`Context` variants) or stdlib `log.Print*` outside `main.go` startup code. This is the "enforced by a context-aware logger rather than by hand" check.

**Sensitive-content decision (SEC-2, LG-6).** The three existing lines in the Context section log task/agent content. They are changed to carry `slog.Bool(logging.AttrSensitive, true)`; the fan-out handler sends sensitive records to **stderr only**, never to the OTLP bridge (so never to Loki). The guest-tail line therefore keeps its forensic value on the host (it is already literal-secret-redacted, `launch.go:396-398`) without entering the central store. Decided by the owner (§14 item 4); the rejected alternative was deleting the lines. If a future shipper reads stderr/journald, these records become reachable — recorded in Named Risks.

**Request ID (LG-8, S).** `otelhttp.NewHandler(mux)` replaces `logRequests`' bare handler chain (`main.go:121,129-147` keeps its log line, now with `trace_id`); the webhook/`POST /tasks` handlers pass `trace.SpanContextFromContext(r.Context())` to `Runner.Start`, which records it as `request_trace_id` in `task.accepted` and as a **span link** on the task's first segment span.

### 6. Metrics (PR C; MT-*, CC-2, Q6)

**Instruments** (OTel names; Prometheus names follow the SDK→Prometheus conversion, `.`→`_`, unit suffixes). Labels are the intent's allowed set plus one bounded addition, `round` (1–5, `coreloop.MaxRounds`, needed by MT-4).

| Metric | Type | Labels | Source |
|---|---|---|---|
| `harness_tasks_accepted_total` | counter | `target_repo` | event `task.accepted` (MT-1) |
| `harness_tasks_finished_total` | counter | `outcome` (`success|escalated|failed`), `target_repo` | `task.finished`; `task.failed` (counts each entry into `failed`, retried tasks counted again) |
| `harness_task_duration_seconds`, `harness_task_active_duration_seconds` | histogram | `outcome` | `task.finished`; duration also from terminal `task.failed` (`outcome=failed`, no active series) (MT-2) |
| `harness_task_rounds` | histogram (buckets 1,2,3,4,5) | `outcome`, `target_repo` | `task.finished` (MT-3) |
| `harness_rounds_total` | counter | `round`, `outcome` (`clean|blocking`) | `round.finished` with outcome continue/stop-success (MT-3, MT-4; waiting-approval rounds are not "finished", mirroring the `CurrentRound` invariant, `taskrunner.go:163-168`) |
| `harness_policy_outcomes_total` | counter | `outcome` (`policy_denied|round_cap|approval_rejected|invalid_phase_config`) | `task.finished.reason_kind` (MT-8, MT-15; the round-cap-hit rate is this series with `round_cap` over accepted tasks) |
| `harness_phase_duration_seconds` | histogram | `phase` | `phase.finished` (MT-5) |
| `harness_step_duration_seconds`, `harness_step_outcomes_total` | histogram / counter | `step_type`, `step_id`, `outcome` | `step.resolved` (MT-6) |
| `harness_approval_wait_seconds` | histogram | – | `approval.granted/rejected` (MT-7) |
| `harness_task_failures_total` | counter | `error_class`, `retryable`, `dependency` | `task.failed` (MT-8; never incremented for policy outcomes) |
| `harness_approvals_total` | counter | `outcome` (`requested|granted|rejected`) | events (MT-15) |
| `harness_pdp_decisions_total`, `harness_pdp_allowed_steps_total` | counter | `outcome` / – | `policy.decided` (MT-15, SLO-8) |
| `harness_reconcile_total` | counter | `outcome` (`resumed|escalated|noop|error`) | runner (MT-10); plus `harness_step_reconciled_total{outcome}`, `harness_resumes_total` |
| `harness_tasks` | gauge | `status` | DB poll (MT-9) |
| `harness_tasks_stuck` | gauge | – | DB poll: running tasks with `now() − max(task_events.at) > STUCK_TASK_INTERVAL` (MT-9) |
| `harness_approvals_waiting`, `harness_approvals_oldest_wait_seconds` | gauge | – | DB poll (MT-7) |
| `harness_dependency_requests_total`, `harness_dependency_request_duration_seconds` | counter / histogram | `dependency` (`git_host|pdp|secrets_issuer|launcher|postgres`), `method`, `outcome`/`code` | `instrumented.*` decorators (MT-13) |
| `harness_dependency_up` | gauge | `dependency` | readiness prober (§8; SLO-4) |
| `harness_webhook_events_total` | counter | `outcome` (`processed|ignored|rejected|failed`; received = sum) | `webhook.go` (MT-14) |
| `harness_persistence_failures_total`, `harness_telemetry_dropped_total` | counter | `component` | `fail`, OTel error handler (ER-5, CC-4) |
| `harness_build_info` | gauge=1 | `version`, `component` | startup (HL-3) |
| `launcher_invocations_total` | counter | `outcome`, `step_type` | launcher (MT-11) |
| `launcher_queue_depth` | gauge | – | launcher |
| `launcher_queue_wait_seconds`, `launcher_boot_seconds`, `launcher_guest_run_seconds` | histogram | `step_type` | launcher (MT-11, LA-2) |
| `launcher_vm_seconds_total` | counter | `step_type` | launcher, boot+run (MT-16) |
| `launcher_redactions_total` | counter | `step_type` | `vsock.Redact` (MT-12) |

**Gauge callbacks.** One OTel async-gauge callback in the control-plane calls `Store.FleetSnapshot(ctx)` (one grouped query plus the waiting/stuck aggregates, 2 s timeout); on error it skips that collection and increments `harness_persistence_failures_total`. Assumes one control-plane instance (`orchestrator.md` §10 already states no horizontal scaling).

**Cardinality guard (CC-2).** Metrics are created only through `telemetry.NewCounter/NewHistogram(name, allowedKeys…)`; the SDK `View` for every instrument sets an `AttributeFilter` allowing only `{component, step_type, step_id, phase, outcome, status, error_class, retryable, dependency, target_repo, method, code, round}` as a hard backstop; the unit test enumerates the catalogue and asserts no key is `task_id`/`invocation_id`/`trace_id`. `target_repo` and `step_id` pass through `telemetry.Bounded`: if `TELEMETRY_TARGET_REPOS` / `TELEMETRY_STEP_IDS` is set, values outside the list become `other` (Q6); if unset, first-seen up to 64 distinct values, then `other`. Histogram boundaries: task seconds `30,60,120,300,600,1200,1800,3600,7200,14400,28800`; step/phase `1,5,15,30,60,120,300,600,1200,1800`; queue/boot/run per launcher timescales (`0.1…1800`).

**Metric-from-event path.** `instrumented.Store` calls `EventSink.Observe(ctx, events)` only after the wrapped method returned nil; `EventSink` is a pure switch on `event.Type` that updates instruments and adds a span event on the active span (ADR-12 §3). Span-less contexts are fine.

### 7. Launcher contract and measurements (LA-1…LA-4, MT-11/12/16)

**Proto (additive, `launcher.proto`).**

```proto
enum LaunchOutcome {
  LAUNCH_OUTCOME_UNSPECIFIED = 0;
  LAUNCH_OUTCOME_COMPLETED = 1;
  LAUNCH_OUTCOME_BOOT_FAILURE = 2;
  LAUNCH_OUTCOME_TIMEOUT = 3;
  LAUNCH_OUTCOME_GUEST_ERROR = 4;
  LAUNCH_OUTCOME_KILLED = 5;
}
message LaunchResponse {
  // fields 1-5 unchanged; `status` string retained for existing clients
  LaunchOutcome outcome = 6;
  google.protobuf.Duration queue_wait = 7;
  google.protobuf.Duration boot = 8;
  google.protobuf.Duration guest_run = 9;
  int32 effective_timeout_seconds = 10;   // LA-3
}
```

Enum prefix/zero-value naming satisfies `buf` `STANDARD` lint (`buf.yaml`); regenerate with `make proto-gen` (checked-in `api/gen/go`).

**Delivery on failure.** In `server.go`'s error branch (`:203-209`), build `st := status.New(codes.Internal, msg)` and `st, _ = st.WithDetails(resp)`; return `st.Err()`. `launcherclient.Client.Launch` (`client.go:94-121`) extracts a `*LaunchResponse` from `status.FromError(err).Details()` and returns a `*launcherclient.LaunchError{Result RunResult, Outcome, Err}` that `Instrumented.Launcher` maps to a `failure.ClassifiedError` per the §2 table. Old clients ignore details; new clients against an old server see no details and fall back to gRPC-code classification — so compatibility holds both ways (CMP-1). `RunResult` gains `Outcome`, `QueueWait`, `Boot`, `GuestRun`, `EffectiveTimeout` (`launcherclient.go:30-36`).

**Mapping inside the launcher.** `bootOutcome` (`server.go:62-67`) gains `reachedGuest bool` (set by `waitForAgentCompletion`/`waitForCommandCompletion`), `boot, run time.Duration`, `effectiveTimeout time.Duration`. `classify(o)`: `completed→COMPLETED`, `timed_out→TIMEOUT`, `killed→KILLED`, `error` → `GUEST_ERROR` if `reachedGuest` else `BOOT_FAILURE`. The pre-boot "guest process exited during boot" (`launch.go:294`) stays a boot failure. A killed-while-queued request (`server.go:178-179`) is `KILLED` with zero boot/run.

**Timing.** In `Launch`: `queueStart := now` before the slot select; on acquire `queueWait = since(queueStart)`; a `waiters` atomic counter backs `launcher_queue_depth` (inc before select, dec on acquire or cancel). `boot` is `bootStart → InstanceStart` (`launch.go:106,283`; already logged as `boot_elapsed`), `run` is `InstanceStart → completion`. The existing `stage()` helper (`launch.go:108-111`) becomes the span boundary for TR-3's jail / image-staging / boot spans (PR E). `vsock.Redact` (`vsock.go:157-165`) returns the replacement count via a new `RedactCounted`; the existing signature stays for callers/tests.

**Mock (LA-4).** `mocks/launcher-mock/server.go:44-66` sets `Outcome` (`COMPLETED`; `GUEST_ERROR` on fixture failure), zero durations, `EffectiveTimeoutSeconds` echoed, and attaches the response as an error detail on its failure path, so compose behaves like the real server.

### 8. Health and readiness (HL-1, HL-3)

- **Control-plane:** `/healthz` (always 200 once serving) and `/readyz` on the main mux. Readiness = Postgres ping on **both** pools (orchestrator and audit) + the latest result of a background prober (every 10 s) for the two fail-closed dependencies. The PDP/secrets contracts define no health endpoint (`api/openapi/`), so the prober treats *any HTTP response* from the base URL as reachable and a connect/timeout error as unreachable; it also drives `harness_dependency_up`. `/readyz` returns 503 with a JSON list of failing checks. (Assumption flagged.)
- **Launcher:** the gRPC port is mTLS-only, so a small plain-HTTP listener `LAUNCHER_HEALTH_ADDR` (default `127.0.0.1:9444`) serves `/healthz` and `/readyz`. The launcher has no database (no `database/sql` use under `cmd/container-launcher` or `internal/launcher`); readiness = egress setup completed (`main.go:44`), `LAUNCHER_JAILER_BINARY`/`LAUNCHER_FIRECRACKER_BINARY` executable, `LAUNCHER_IMAGE_ROOT` readable, `/dev/kvm` openable. HL-1's "database connectivity" is not applicable to the launcher.
- Both log `build.version`, `commit` and non-secret config at startup (extending `main.go:47-50` and launcher `main.go:32-37`) and set `harness_build_info`; OTel resource attributes `service.name`, `service.version`, `host.name`, `deployment.environment` identify component and host (CC-8).
- Add graceful shutdown to the control-plane: `signal.NotifyContext` + `http.Server.Shutdown` + `telemetry` shutdown with a 5 s flush. In-flight task goroutines are not awaited; restart reconciliation covers them as today.

### 9. Telemetry bootstrap and failure isolation (CC-4, SEC-5)

`telemetry.Setup(ctx, component)` returns no-op providers unless `OTEL_EXPORTER_OTLP_ENDPOINT` is set (compose and prod set it; plain `go test` and bare `go run` stay quiet). All sampler/endpoint/header options use the standard `OTEL_*` variables (`OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` → TR-7, default `parentbased_always_on`). Exporters use the SDK batch processors/periodic reader (bounded queues, drop on overflow, async export), so a dead Collector cannot block a task. The OTel internal error handler is routed to a **stderr-only** logger (not the default logger) with rate limiting, and increments `harness_telemetry_dropped_total`; routing it through the bridge would feed export failures back into the exporter. No Collector/Prometheus/Loki/Tempo/Grafana container receives `AUDIT_DB_DSN`/`ORCHESTRATOR_DB_DSN` (SEC-5); the `EventSink` and gauge callback only *read* `task_events`/`tasks`.

### 10. Tracing (PR E; TR-1…TR-8)

Span tree: `task.segment` (attrs `task_id`, `segment_kind=start|reconcile|resume|retry`) → `round` (`round`) → `phase` (`phase`) → `step` (`step_id`, `step_type`, `invocation_id`, `blocking`, `effective_timeout_s`) → children `pdp.evaluate`, `secrets.issue`/`secrets.revoke`, `git.<op>`, `launcher.launch` (client span) → launcher-side server span → `launcher.queue_wait`, `launcher.jail_setup`, `launcher.image_stage`, `launcher.vm_boot`, `launcher.guest_run`, `launcher.result_extract` (TR-3). Failed spans carry `error.class` and status Error (TR-5). The `step` span is the documented agent-observability attachment point (TR-8; ADR-12 §10).

- **Propagation (TR-2):** `otelgrpc` client handler in `launcherclient.NewClient` (`client.go:42`, alongside the existing interceptor) and server handler in launcher `main.go:60`; W3C Trace Context in gRPC metadata.
- **Approval pause (TR-4):** a *segment* is a Start/Reconcile/Resume/Retry invocation of `runLoop`; its span ends when `runLoop` returns (terminal, `waiting_approval`, or failed), so no span spans an approval wait. A new segment records a **span link** to the previous segment's last span, found via `TaskStore.LatestTraceContext(taskID)` (latest `task_events` row with a non-null `trace_id`). A crash loses the unended span; the events table still has the timeline.
- Each appended event takes `trace_id`/`span_id` from the ctx's active span, so D3 can jump events → trace.

### 11. Pipeline, dashboards, alerts, runbook (PRs D, F, G)

**Layout:** `deploy/observability/{otel-collector,prometheus,alertmanager,loki,tempo,grafana}/`. `deploy/docker-compose.observability.yml` defines the services and is pulled into `docker-compose.yml` with a top-level `include:` so every existing command in CLAUDE.md (`docker compose -f deploy/docker-compose.yml …`) still works (requires Compose ≥ 2.20 — Named Risks). The control-plane and launcher-mock services get `OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4317`, `ADMIN_ADDR`, and `LOG_FORMAT=json` (LG-1).

- **Collector (per host):** receivers `otlp`; processors `memory_limiter`, `batch`; exporters: `otlphttp` → Loki (`/otlp`), `otlp` → Tempo, `prometheusremotewrite` → Prometheus (`--web.enable-remote-write-receiver`), with `resource_to_telemetry_conversion` so `service_name`/`host_name` label every series. Prod: the launcher-host Collector's exporters point at the observability host; all traffic is outbound from the hosts.
- **Loki (LG-3/4/5):** OTLP ingest with `allow_structured_metadata: true` (Loki ≥ 3); index labels limited to `service.name`, `host.name`, `deployment.environment`, plus Loki's derived level; `task_id`, `round`, `step_id`, `invocation_id`, `trace_id` stay structured metadata. Single-step retrieval (LG-5): `deploy/observability/task-logs.sh <task_id>` (and `make task-logs TASK=<id>`) runs `{service_name=~"control-plane|container-launcher"} | task_id="<id>"` over a 30-day window sorted by time and prints it.
- **Retention (CC-3, values provisional):** Prometheus 30 d, Loki 14 d (compactor), Tempo 7 d, `task_events` 365 d via a PR G job under a dedicated role (migration `008`), audit trail per ADR-5 unchanged. Each is a setting in a committed config file.
- **Dashboards (CC-9):** provisioned JSON in `grafana/dashboards/` — D1 fleet, D2 loop quality (rounds histogram, `harness_rounds_total{round,outcome}` → convergence, first-round-clean rate), D3 single task (variable `task_id`; table panel from Postgres datasource on `task_events`, Tempo trace-by-ID via `trace_id` column, Loki logs panel), D4 launcher, D5 dependencies, D6 governance. D3 needs a **read-only Postgres role** (`grafana_reader`: `SELECT` on `orchestrator.task_events`, `orchestrator.tasks` — no audit schema, SEC-5), created in migration `007_observability_roles.sql` (PR F; idempotent `DO` block like `001_roles.sql`).
- **Alerts** (`prometheus/rules/*.yml`, thresholds proposed in the intent, tunable here; each has `runbook_url` → `docs/runbooks/observability.md#…`, AL-2; policy outcomes never page, AL-3):

| SLO | Expression (sketch) | Severity |
|---|---|---|
| 1 | `harness_tasks_stuck > 0` | page |
| 2 | `sum(rate(harness_task_failures_total[1h])) / sum(rate(harness_tasks_accepted_total[1h])) > 0.1 and sum(increase(harness_tasks_accepted_total[1h])) >= 5` (minimum-volume guard) | page |
| 3 | `histogram_quantile(0.95, sum by (le)(rate(launcher_queue_wait_seconds_bucket[15m]))) > 600` | warn |
| 4 | `harness_dependency_up{dependency=~"pdp|secrets_issuer"} == 0` `for: 30s`; other deps: error ratio `> 0.05` `for: 10m` | page / warn |
| 5 | `harness_approvals_oldest_wait_seconds > 86400` | notify |
| 6 | `increase(harness_policy_outcomes_total{outcome="round_cap"}[24h]) / increase(harness_tasks_accepted_total[24h]) > 0.2` | warn |
| 7 | `harness_audit_chain_verification_ok == 0` (PR G, HL-5) | page |
| 8 | `increase(launcher_invocations_total[1h]) > increase(harness_pdp_allowed_steps_total[1h])` | page |
| 9 | `up{job=~"loki|tempo|otel-collector"} == 0` `for: 10m` or `absent_over_time(harness_build_info[10m])` | warn |
| D-1 (design-added) | `harness_tasks{status="failed"} > 0` `for: 1h` — a failed task nobody has retried or closed; SLO-2's volume guard would otherwise let a lone failure go unnoticed | notify |

SLO-8 compares launches to *steps resolved by allow decisions*, not to allow decisions, because one allow resolves several steps (`describeSteps`, `coreloop.go:191-193`); re-evaluation after resume only overstates allowed steps, so the check stays one-sided-safe. Alertmanager ships with a null receiver (routing is a process detail per ADR-5).
- **PR G:** audit-chain verification job in the control-plane (hourly, reuses `audit.VerifyChain`, `hashchain.go`, reading via the existing writer role's `SELECT`, `003_audit_schema.sql`), `harness_audit_chain_verification_ok` / `_last_run_timestamp_seconds`; ADR-5 signals (egress denials, kill-switch timing, quota trend) as metrics where the launcher already observes them; retention job; access control (reverse proxy auth in front of Grafana/Loki/Tempo/Prometheus for SEC-6, mechanism depends on Q10).
- **Runbook (OP-2):** `docs/runbooks/observability.md` — find a task's logs/trace/timeline from one ID; each alert's meaning and first diagnostic; what to do if the pipeline itself is down (telemetry is lossy by design; `task_events` and stderr remain).

### 12. Documentation (OP-1)

Updated in the same PRs as the change that makes them stale: `docs/components/orchestrator.md` §4 (API: `failed`, events, admin), §6 (config: `OTEL_*`, `ADMIN_ADDR`, `STUCK_TASK_INTERVAL`, `HEARTBEAT_INTERVAL`, `TASK_MAX_RETRIES`, `TELEMETRY_*`), §7 (data model: `task_events`, new columns, `006`), §10; `docs/components/container-launcher.md` (outcomes, health listener, metrics); `docs/local-development.md` (observability stack, ports, migrate ordering). `docs/architecture/` and `docs/designs/completed/` stay frozen. When ADR-12 is accepted its status moves in a separate edit by the owner.

### 13. What explicitly does not change

- **The audit trail** (`internal/control-plane/audit`, `003_audit_schema.sql`): no new columns, rows, or writers; hash chain untouched (N2, SEC-5). The only audit-adjacent change is that the unreconcilable-launch case no longer writes a `task-outcome` row (§2).
- **`coreloop.RunRound`'s purity and phase logic**: it gains two nil-safe narrow interfaces (`Events`) and typed `ReasonKind`; no store, round caps or resumption logic enter it.
- **Step-run semantics** (`launched/completed/failed`, `alreadyCompleted` skipping, `loadAlreadyCompleted`, `resolveLaunchedStep` via `ExtractLogs`): unchanged apart from emitting events.
- **Launcher redaction** (ADR-10, `vsock.Redact`) and its fail-closed structured result (`status.go:13`): unchanged; counting is added beside it.
- **HTTP contracts of `POST /tasks` and the webhook**; **PDP/secrets contracts** (`api/openapi`): untouched — dependency instrumentation is client-side decoration.
- **Agent-level observability** (prompts, tool calls, tokens) — N1; only the attachment point is reserved.

### 14. Owner decisions

All five items below were decided by the owner on 2026-10-11.

1. **Release scope.** The intent's Q11 (release 1 = slices 1+2, tracing/dashboards follow) conflicts with AL-1/TR/D1–D6 being "Must". **Decided:** release 1 = PRs A–D; release 2 = PRs E–G (tracing, dashboards, alerts, hardening). AL-1, TR-1…TR-6 and D1–D6 are therefore satisfied at release 2; the Success Criteria below are tagged accordingly.
2. **Departures from the intent's wording:** (a) *active time* excludes human/operator wait only, not launcher queue wait (§4); (b) the SLO-5 age threshold is an alert-rule constant rather than in-process config (§4); (c) SLO-8 compares launches to allowed *steps* (§11); (d) metrics add one label, `round` (§6); (e) the invalid-phase-config outcome is classed as `policy` (§2). **Decided: accepted.**
3. **Migrated `running`+`error:` tasks become `failed`/`platform`/retryable** (§2). **Decided: accepted.**
4. **Sensitive log lines are kept on stderr only** rather than deleted (§5). **Decided: accepted.**
5. **Unreconcilable launched step becomes `failed`** instead of audited `stop_escalate` (§2). **Decided: accepted.**


### 15. Interface changes (consolidated reference)

Authoritative list of signature changes; sections above give the rationale. Fakes in `internal/control-plane/fakes` change in the same PR as the interface.

```go
// domain
const TaskStatusFailed = "failed"
type TaskEvent struct { TaskID string; Round int; StepID, InvocationID, Type string; Payload map[string]any; TraceID, SpanID string }
type FailureRecord struct { TaskID string; Round int; Class string; Retryable bool; Reason string }
// TaskState += ErrorClass string; Retryable bool; RetryCount int; FinishedAt *time.Time
// RoundResult += ReasonKind string

// taskrunner.TaskStore (PostgresTaskStore runs state SQL + event INSERT in one BeginTx via withTx)
UpdateTaskState(ctx, id string, st TaskState, events ...TaskEvent) error
EnsureRound(ctx, taskID string, round int, events ...TaskEvent) (created bool, err error)
FailTask(ctx, rec FailureRecord, events ...TaskEvent) error        // replaces SetTaskReason (removed)
RetryTask(ctx, id string, maxRetries int, ev TaskEvent) (domain.TaskState, error) // CAS; ErrNotRetryable on 0 rows
AppendEvents(ctx, events ...TaskEvent) error                        // events with no row change
ListEvents(ctx, taskID string, f EventFilter) ([]TaskEvent, error) // GET /tasks/{id}/events + wait/active calc
ListTasksByStatus(ctx, status string) ([]TaskAge, error)           // admin list
FleetSnapshot(ctx) (Snapshot, error)                                // gauges
LatestTraceContext(ctx, taskID string) (traceID, spanID string, ok bool, err error)
// CreateTask / RecordStepLaunched / CompleteStepRun derive their own event internally.

// coreloop.Dependencies += Events EventAppender (nil = no-op)
type EventAppender interface { AppendEvents(ctx context.Context, events ...domain.TaskEvent) error }

// taskrunner
func (r *Runner) Retry(ctx context.Context, taskID, operator, remoteAddr string) (Result, error)
var ErrNotRetryable = errors.New("task not retryable")

// failure (leaf)
type Class string
type ClassifiedError struct { Class Class; Dependency string; Err error }
func Classify(err error) (Class, retryable bool, dependency string)
func Bound(s string) string // ≤512 B

// logging
func WithTask(ctx, taskID string) context.Context
func WithRound(ctx, round int) context.Context
func WithStep(ctx, stepID, invocationID string) context.Context
const AttrSensitive = "sensitive"

// launcherclient
type RunResult struct { /* existing */ Outcome string; QueueWait, Boot, GuestRun, EffectiveTimeout time.Duration }
type LaunchError struct { Result RunResult; Outcome string; Err error }

// vsock
func RedactCounted(in string, secrets []string) (out string, n int)  // Redact unchanged
```

**As built in PR A** (differences from the signatures above, all narrowing or additive):

- `EnsureRound(ctx, taskID, round) (created bool, err error)` takes no `events`: the store derives `round.started` itself when it inserted the row; the runner appends `round.resumed` through `AppendEvents` when `created` is false.
- `Runner.StartRetry(ctx, taskID, operator, remoteAddr) (<-chan Result, error)` does the compare-and-set synchronously and continues in a goroutine (so the admin handler can answer 202/409/404); `Runner.Retry` is `StartRetry` plus waiting. `ErrNotRetryable` aliases `domain.ErrNotRetryable` (the stores return it and can't import `taskrunner`); `ErrNoSuchTask` is new.
- `domain.TaskEvent` also carries `ID` and `At` (store-assigned, read side); `domain.EventFilter`, `domain.TaskAge`, `domain.WaitActive(events, end)` and the event builders (`TaskAcceptedEvent`, `StepLaunchedEvent`, `StepResolvedEvent`, …) live in `domain/events.go` so the Postgres store and the fake derive identical events. `TaskState` also carries `AcceptedAt` (= `tasks.created_at`).
- `coreloop.Dependencies` gains `HeartbeatInterval` beside `Events`; `taskrunner.Dependencies` gains `MaxRetries`.
- `FleetSnapshot` and `LatestTraceContext` are not in PR A (they belong to PRs C and E). `step.resolved` carries no launcher timing fields until PR C.
- Until the launcher outcome contract lands (PR C), launch errors are classed from the gRPC code alone (`Unavailable`/`DeadlineExceeded`/`Canceled` → dependency/launcher; `InvalidArgument` → platform; anything else → infrastructure). The `BOOT_FAILURE`/`KILLED`/`TIMEOUT`/`GUEST_ERROR` rows of the §2 table are therefore not yet distinguishable.

HTTP surface: `GET /tasks/{id}` (+ additive fields, status `failed`), `GET /tasks/{id}/events`, `GET /healthz`, `GET /readyz`; admin listener: `GET /admin/tasks`, `POST /admin/tasks/{id}/retry`. Launcher: plain-HTTP `/healthz`, `/readyz` on `LAUNCHER_HEALTH_ADDR`.

**As built in PR B** (differences from §5/§8, narrowing or additive):

- `internal/logging.Setup(component, extra ...slog.Handler)`: the fan-out is built but only stderr is wired; PR C passes the OTLP bridge as `extra`. `trace_id`/`span_id` enrichment is deferred to PR C/E with the OTel dependency, so LG-2's trace half is not yet met. `logging.NewHandler(w, …)` is exported so tests capture exactly what the process emits.
- `WithStep` ignores empty arguments (keeps what ctx has), so the launcher client interceptor and `Kill` can add only an invocation id. The launcher has no round in its request, so its lines carry `task_id`/`step_id`/`invocation_id` but not `round`.
- `callsites_test.go` exempts `main.go` and `config.go` (startup, `log.Fatalf`) for both `log.*` and non-context `slog.*`. It sees `slog.X(` / `log.X(` selector calls only, not methods on a `*slog.Logger` variable.
- Sensitive records: launcher `prompt prefix`, `guest event log tail`, `structured result`, vsock guest-wrapper error; control-plane executor `logs extracted` (structured result). The webhook sender/repo names are logged, never payload bodies.
- Health lives in a shared `internal/health` package (`Register`, `Prober`) used by both binaries. `/readyz` returns failing check *names* only (details at `warn`).
- Control-plane readiness has no `harness_dependency_up` yet (metrics are PR C). `harness_build_info` likewise moves to PR C; PR B logs `version`/`commit` at startup and on every record.
- The launcher's `/readyz` pings nothing external; its listener starts before `egress.Setup` so probes are answered (not refused) during setup. Launcher graceful shutdown is not part of PR B (design §8 scopes it to the control-plane).

**As built in PR C** (differences from §6/§7/§9, narrowing or additive):

- Pinned (§17): `go.opentelemetry.io/otel`, `otel/metric`, `otel/sdk`, `otel/sdk/metric`, `otlpmetricgrpc` at **v1.46.0**; `otel/log`, `otel/sdk/log`, `otlploggrpc` at **v0.22.0**; `contrib/bridges/otelslog` **v0.20.1** (the 2026-08-25 train; v1.47 was two weeks old). The exporters require `google.golang.org/grpc` ≥ 1.83, so it moved from v1.81.0 to **v1.83.1**. Only metrics and logs are exported; no tracer provider until PR E.
- Package split: `internal/telemetry` holds the bootstrap, the key guard (`NewCounter`/`NewHistogram`/`DeclareGauge` panic on a key outside `AllowedKeys`; `AllowedKeysView` is the SDK backstop), `Bounded` and `RegisterBuildInfo`. The control-plane catalogue and the event sink are `instrumented.Metrics` (`Observe`), and the launcher's instruments are `cmd/container-launcher/metrics.go`, so the launcher never imports control-plane packages. `AllowedKeys` adds `version` (for `harness_build_info`).
- Metric-from-event path: the store, not a decorator, hands committed events to the sink — `PostgresTaskStore.SetEventObserver(fn)` runs `fn` after each commit with exactly the events it inserted (`withTx` became `withEvents`, whose closure returns its events), and `FakeTaskStore.Observer` mirrors it. A decorator couldn't see the events `CreateTask`/`EnsureRound`/`RecordStepLaunched`/`CompleteStepRun` derive themselves. `instrumented.Store` therefore only times Postgres calls. No span events until PR E.
- Decorators (`Policy`, `Secrets`, `Git`, `Launcher`, `Audit`, `Store`) record `harness_dependency_requests_total{dependency,method,outcome,code}` and `_request_duration_seconds{dependency,method,outcome}` but don't classify errors: the call sites have done that since PR A. Launch-outcome classification lives in `executor.classifyLaunchError` (now also used by `DeterministicStepExecutor`, whose launch errors were unclassified → `platform`). `audit` calls count as `dependency=postgres`, `method=audit.*`.
- `domain.StepRun.Launch` (`LaunchInfo`) carries the launcher's outcome and timings; executors return it beside a launch error too. Additive payload fields: `step.resolved` gains `step_type` (from the row), `launcher_outcome`, `queue_wait_ms`, `boot_ms`, `run_ms`, `effective_timeout_s` (here rather than on `step.launched`, where it isn't known yet); `task.finished`/`task.failed` gain `target_repo`; `approval.granted`/`rejected` gain `wait_ms`. `StepResolvedEvent` takes `stepType`.
- `FleetSnapshot(ctx, stuckAfter)` takes the stuck interval; the gauge callback reports every status (zeros included). `harness_dependency_up` covers `pdp` and `secrets_issuer` (the probed ones). `harness_reconcile_total{outcome}` is `resumed|failed|noop|error` (PR A removed "escalated"); `harness_resumes_total{outcome}` is the `round.resumed` reason; `harness_step_reconciled_total{outcome}` is `resolved|unresolved`. `harness_rounds_total` skips `stop-escalate` rounds, per §6. The runner reports reconcile outcomes and unpersistable failures through a nil-safe `taskrunner.Dependencies.Metrics`.
- Webhook outcomes: `rejected` = bad payload, bad signature, or a sender who isn't a listed approver / lacks write access; `failed` = a lookup or write failed (including a role check that errored); `ignored` = nothing to do (unknown action, no waiting task, no approval label, redelivery); `processed` = approval/rejection recorded or task started. An unknown action counts as `ignored`, not `rejected` as MT-14's wording suggests, since Forgejo sends many issue actions the handler legitimately doesn't use.
- Instrument names are the Prometheus names themselves (`harness_tasks_accepted_total`, `…_seconds` with unit `s`; counters carry no unit), so the Collector's suffixing should leave them unchanged; PR D's compose smoke confirms.
- `vsock.RedactCounted(data []byte, secrets map[string]string) ([]byte, int)` matches `Redact`'s real signature. `logging.NewHandler` applies `LOG_LEVEL` to extra sinks and skips nil ones (telemetry disabled), so `logging.Setup(component, tel.LogHandler)` is unconditional. Export errors go to a stderr-only logger, at most one line per 30 s.
- The launcher gained `SIGINT`/`SIGTERM` handling (`grpcServer.Stop`, then a 5 s telemetry flush) so the final export isn't lost; stopping cancels in-flight invocations, which tears their VMs down.
- Not built: MT-17 (HTTP request metrics, "Could").

### 16. Configuration reference

| Variable | Binary | Default | Section |
|---|---|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | both | unset → no-op telemetry | §9 |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | both | `parentbased_always_on` | §9 |
| `ADMIN_ADDR` | control-plane | `127.0.0.1:8090` | §2 |
| `TASK_MAX_RETRIES` | control-plane | `3` | §2 |
| `STUCK_TASK_INTERVAL` | control-plane | `30m` | §4 |
| `HEARTBEAT_INTERVAL` | control-plane | `5m` (keep ≤ stuck/6) | §4 |
| `TELEMETRY_TARGET_REPOS`, `TELEMETRY_STEP_IDS` | control-plane | unset → first 64 values, then `other` | §6 |
| `LOG_LEVEL`, `LOG_FORMAT` | both | existing; compose sets `json` | §5 |
| `LAUNCHER_HEALTH_ADDR` | launcher | `127.0.0.1:9444` | §8 |
| `LAUNCHER_DEFAULT_TIMEOUT_SECONDS` | launcher | `1800` (existing) | §7 |

### 17. Dependency versions

No versions are pinned in this design (none were verified). Rules: PR C pins exact `go.opentelemetry.io/otel*` module versions in `go.mod` (all from one release train; the logs bridge may be a separate, pre-stable module); PR D pins exact image tags for the Collector (contrib distribution, needed for `prometheusremotewrite`), Prometheus, Alertmanager, Loki (**≥ 3**), Tempo and Grafana in `docker-compose.observability.yml` and records them in `docs/local-development.md`; Docker Compose ≥ 2.20 for `include:`. Never use `latest`.

## Delivery (confirms the intent's slicing; sizes Q11)

| PR | Content | Requirements |
|---|---|---|
| A | `failure` pkg, `006` migration, `task_events` + store transactions + fakes, `FailTask`/`Retry`, `ReasonKind`, API/admin endpoints, heartbeat, doc updates | TS-1…TS-9, ER-1…ER-3, ER-5(part), ER-6, SEC-4, CMP-2 |
| B | Context logger + call-site AST test, sensitive-record split, webhook logging, launcher ID propagation (LG-7), health endpoints, graceful shutdown, `buildinfo` | LG-1, LG-2, LG-6…LG-9, HL-1, HL-3 |
| C | Launcher proto/outcomes/timings/mock, `telemetry` + `instrumented` decorators + `EventSink`, all metrics, gauges | LA-1…LA-4, MT-*, CC-2, CC-4, HL-2, ER-4 |
| D | Collector/Prometheus/Loki/Tempo/Grafana/Alertmanager in compose, log shipping, retention config, `task-logs.sh`, runbook skeleton | LG-3…LG-5, CC-3, CC-7, CC-8 |
| E (release 2) | Tracing | TR-1…TR-8, LG-8 completion |
| F (release 2) | Dashboards D1–D6, alert rules + `promtool` tests, runbook completion | AL-1…AL-3, CC-9, §7, §8 |
| G (release 2) | Audit-chain job, ADR-5 signals, retention job, access control | HL-4, HL-5, SEC-6, SEC-7 |

### Per-PR checklists

Each PR is mergeable on its own, keeps `make test` and `buf lint` green, and updates the §12 docs it makes stale.

- **A — Task state and events.** *Files:* `internal/control-plane/failure/`, `domain/{domain,events}.go`, `deploy/migrations/006_task_events_and_failure.sql`, `taskrunner/{store,postgres_store,taskrunner}.go`, `coreloop/coreloop.go` (`Events`, `ReasonKind`, heartbeat in `fanOut`), `fakes/`, `cmd/control-plane/{handler,main}.go` (events endpoint, admin mux). *Tests:* §Testing items for TS-2/3/4/7/9, ER-1/3, failure, handler, Postgres integration. *Done when:* success criteria 2, 3, 5, 6 hold and `orchestrator.md` §4/§6/§7 are updated.
- **B — Logging, health, shutdown.** *Files:* `internal/logging/`, `internal/buildinfo/`, call-site changes in coreloop/executor/webhook/launcher (`server.go`, `launch.go`, `vsock.go`), `cmd/*/main.go`, launcher health listener. *Done when:* `callsites_test.go` passes, criterion 8 (no metrics yet; `harness_dependency_up` arrives in C) and sensitive records are stderr-only.
- **C — Metrics and launcher contract.** *Files:* `api/proto/launcher/v1/launcher.proto` + `make proto-gen`, `launcherclient`, `cmd/container-launcher/{server,launch}.go`, `mocks/launcher-mock`, `internal/telemetry/`, `internal/control-plane/instrumented/`, readiness prober, `FleetSnapshot`. *Done when:* criteria 7, 10, 11 and the CC-2/MT-1 tests hold.
- **D — Pipeline in compose.** *Files:* `deploy/observability/*`, `deploy/docker-compose.observability.yml`, Makefile (`task-logs`), `task-logs.sh`, runbook skeleton, pinned versions (§17). *Done when:* `make compose-up` brings the stack up, criterion 9's log half holds (`make task-logs`), and e2e Loki query passes. **Release 1 ends here.**
- **E — Tracing (release 2).** Spans per §10, `otelgrpc` on both sides, `LatestTraceContext`, request-trace link. *Done when:* TR-1…TR-6 tests and criterion 9's trace half hold.
- **F — Dashboards and alerts (release 2).** Dashboards D1–D6, rules incl. D-1, `promtool` tests (`make lint-observability`), migration `007`, full runbook. *Done when:* criteria 4, 9 (dashboard half), 12 hold.
- **G — Hardening (release 2).** Audit-chain job, ADR-5 signals, retention job + migration `008`, access control.

A is the largest and riskiest (state-model change in the most failure-sensitive code); C depends on A (events) and B (ctx) and is next largest.

## Testing Strategy

Unit tests run on `internal/control-plane/fakes` (no build tag), matching the repo split in CLAUDE.md.

- `internal/control-plane/taskrunner/taskrunner_test.go` (extend): **TS-2** — run a multi-round, approval-gated task on `FakeTaskStore` and assert the ordered event types equal the §3 sequence; **TS-4/ER-1** — a table-driven test over every row of the §2 failure table (inject the error via the existing fakes' `*Func` hooks) asserting `status=failed`, class, retryable, `task.failed` event, and the failure counter; `ReconcileTask` and `Reconcile` skip failed tasks; **TS-9** — retry resumes and emits `task.retried`, terminal and exhausted-retry cases return `ErrNotRetryable`, restart never retries; **ER-3** — PDP deny / rejection / round cap produce `stop_escalate` with `reason_kind` and no failure-counter change; update the unreconcilable-step test for the new `failed` outcome; wait/active computation from a scripted event sequence.
- `internal/control-plane/coreloop/coreloop_test.go` (extend): phase, `policy.decided` events and `ReasonKind` values; events appender error fails the round.
- `internal/control-plane/failure/failure_test.go` (new): `Classify` mapping, `%w` chains, default-to-platform.
- `internal/control-plane/instrumented/*_test.go` (new): each decorator wraps errors with the right `dependency`/class and records metrics (OTel `ManualReader`).
- `internal/telemetry/telemetry_test.go` (new): **CC-2** catalogue label test and `View` filter; **MT-1** event→metric consistency (drive a scripted event stream, compare counters with event counts); `Bounded` overflow to `other`; no-op setup without an endpoint.
- `internal/logging/logging_test.go` + `callsites_test.go` (new): context fields, trace/span IDs, sensitive records excluded from the bridge sink, call-site rule. **LG-6/SEC-1/SEC-2/TR-6** negative test: run a task with a known secret token and description through runner + executors with an in-memory `slog` sink, `tracetest.SpanRecorder`, and the fake store; assert neither string appears in any log record sent to the central sink, span attribute, event payload, or metric attribute.
- `cmd/control-plane/handler_test.go` (extend): `failed` in `GET /tasks/{id}`, `/tasks/{id}/events` filtering/pagination, `/readyz` 200/503 with fake probes, admin retry 202/409; `webhook_test.go`: counters per outcome.
- `cmd/container-launcher/server_test.go` (extend, fake `vmLauncher`): **LA-1** one test per outcome including `BOOT_FAILURE` vs `GUEST_ERROR`; queue-wait measured with two concurrent launches; `LaunchResponse` delivered as error detail; `TimeoutSeconds` precedence recorded (LA-3); new `health_test.go`. `vsock` test for `RedactCounted`.
- `internal/control-plane/clients/launcherclient/client_integration_test.go` (`integration` tag, extend): details round-trip against launcher-mock; old-server fallback.
- `internal/control-plane/taskrunner/postgres_integration_test.go` (new, `integration` tag, modelled on `audit/postgres_integration_test.go`): **TS-3** — force the event INSERT to fail (violating row) and assert the `tasks`/`step_runs` change rolled back; `orchestrator_app` cannot `UPDATE`/`DELETE` `task_events`; **TS-7** — seed `running`+`error:` and `stop_*` rows, run `006` twice, assert statuses, one `task.migrated` per task, idempotence.
- `internal/control-plane/e2e/smoke_test.go` (`e2e` tag, extend): after a full task, `GET /tasks/{id}/events` shows accepted → terminal in order; collector → Loki query by `task_id` returns control-plane lines (launcher-mock only; cross-host proof is manual, Named Risks).
- `deploy/observability/prometheus/tests/*.yml` run by `promtool test rules` (`make lint-observability`): each alert fires/doesn't on synthetic series, including the SLO-2 minimum-volume guard and "policy outcomes never page".
- Existing contract tests (`handler_test.go`, `taskrunner_test.go`, compose e2e) keep passing except where the new `failed` status is the deliberate change (LA-4, TS-1).

## Success Criteria

Each criterion is tagged with the PR that must satisfy it (release 1 = A–D; release 2 = E–G).

1. `make test`, `make test-integration`, `make test-e2e` pass; `buf lint` passes. *[all PRs]*
2. For any completed task in the e2e stack, `GET /tasks/{id}/events` returns the §3 sequence in order, and total duration, per-round duration and approval wait are computable from it alone. *[A]*
3. Injecting an error at any row of the §2 table leaves the task `failed` with the listed class/retryable, one `task.failed` row, one `error`-level log line, and one `harness_task_failures_total` increment; a restart does not resume it; `Retry` resumes only retryable ones. *[A]*
4. PDP deny, rejected approval and round cap never change `harness_task_failures_total` and never trigger a page rule (`promtool` test). *[F (rule tests); counters from A/C]*
5. The `orchestrator_app` role cannot update or delete `task_events`; a failed event insert leaves state unchanged. *[A]*
6. Migration `006` on a seeded database converts `running`+`error:` tasks to `failed`, gives every task a non-empty timeline, and is re-runnable. *[A]*
7. The launcher returns one of the five outcomes plus queue/boot/run durations and effective timeout for success **and** failure; mock and real server agree. *[C]*
8. Both binaries answer `/healthz` and `/readyz`; killing Postgres or the PDP flips control-plane readiness to 503 within one probe interval. *[B (readiness probes: C)]*
9. With `make compose-up`, Grafana shows D-series data from a task, and `make task-logs TASK=<id>` prints that task's lines in time order from Loki. *[D (logs), E (traces), F (dashboards)]*
10. Stopping the Collector or Loki does not slow or fail a task (CC-4 test: run a task with an unreachable OTLP endpoint; completion time within noise of the no-op run). *[C]*
11. The negative test finds no secret value, task description, or agent output in central logs, spans, events, or metric labels. *[C (extend in E)]*
12. Every new setting and endpoint is documented in `orchestrator.md` / `container-launcher.md`, and a runbook entry exists for every alert. *[each PR for its own items; alerts' runbook entries: F]*

## Out of Scope

- Agent observability — prompts, tool calls, model, tokens, reasoning (N1); only the `step` span attachment point and open event vocabulary are provided.
- Authentication/authorisation for the HTTP and admin APIs (N4); the admin listener's loopback binding is a containment measure, not a substitute.
- Finding severity in loop-quality metrics (N3).
- Currency/token cost (N5); VM-seconds are provided.
- Horizontal scaling of the control-plane (already a documented gap, `orchestrator.md` §10); gauges assume one instance.
- Automatic retry of any kind (decided, Q3).
- Real alert routing/on-call configuration and production Collector/backend sizing.
- Numeric retention values and final alert thresholds (deferred, Q1/Q2); defaults here are placeholders.
- Observability-UI authentication (Q10) beyond keeping the stack off public paths; blocks production exposure, not local use.

## Named Risks / Follow-Ups

- **Go OTel logs SDK maturity** (not verified here; my understanding is that the log signal is still pre-stable in Go): if it proves unsuitable, fall back to Collector `filelog`/`journald` receivers on the hosts. The sensitive-record split then no longer holds (stderr would be scraped) and those three lines must be deleted instead.
- **Journald/file capture on the Linux hosts is not in this repo.** The launcher is run as root under a process manager whose unit files are not versioned here; only the OTLP path is designed end to end. Confirm how the hosts run both binaries before PR D.
- **Docker Compose `include:`** needs Compose ≥ 2.20; verify on the owner's version, else use `-f` pairs and update the Makefile and CLAUDE.md commands.
- **Loki OTLP structured-metadata behaviour** (Loki ≥ 3, label promotion defaults) is from documentation, not verified in this repo; confirm with the PR D compose smoke test. Fallback: promote nothing extra and use a `| json` filter on the line body.
- **Cross-host ordering** in LG-3/LG-5 assumes clock sync (NTP/chrony) between hosts; not verifiable here.
- **Local acceptance cannot prove cross-host behaviour**: compose runs `launcher-mock` (`docker-compose.yml:101-115`), not the KVM launcher (`launcherkvm` tests cannot run on macOS, CLAUDE.md). TR-2/LG-3 across two real hosts needs a manual run on Linux, documented in the runbook.
- **Dependency liveness without health endpoints**: "any HTTP response = reachable" can report a wedged PDP as up. If the real PDP/secrets services expose health endpoints, point the prober at them.
- **Secret leakage via error strings**: bounded length (SEC-4) does not scrub content. This design assumes our client wrappers never put credentials into error text (not audited exhaustively); the negative test covers the known token only. Add `vsock.Redact`-style scrubbing of known secret values to `failure.Bound` if the audit finds otherwise.
- **Crash between commit and `EventSink.Observe`** loses a counter increment; accepted (counters reset on restart; the events table is authoritative).
- **Mid-flight spans are lost on crash**; timeline survives in `task_events`.
- **Admin `operator` field is self-declared** until N4 lands; retry is the only mutating admin route and is bounded by `TASK_MAX_RETRIES`.
- **Heartbeat rows** add ~12 rows/hour per in-flight step; if that proves noisy, make progress a `tasks.last_progress_at` column updated in place instead.

## Results

*(Appended here as a dated run entry once implemented.)*
