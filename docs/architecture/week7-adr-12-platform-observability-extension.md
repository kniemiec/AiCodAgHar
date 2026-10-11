# ADR-12: Platform Observability Extension — OpenTelemetry, Loki, Per-Task Event History, and a First-Class Failed State

| | |
|---|---|
| **Status** | Proposed |
| **Date** | 2026-10-08 |
| **Deciders** | Me |
| **Consulted** | Security/InfoSec |
| **Informed** | Engineering leadership, platform team |
| **Supersedes / Related** | Extends [ADR-5](week1-adr-5-observability-and-audit-trail.md) (observability and audit trail) — does not supersede it; every ADR-5 decision stays in force. Builds on [ADR-7](week1-adr-7-multi-agent-review-workflow.md) (fixed core loop and round cap), [ADR-9](week2-adr-9-orchestrator-foundations.md) (orchestrator foundations, Postgres state), [ADR-10](week2-adr-10-microvm-launch-and-lifecycle-mechanics.md) (launch lifecycle), [ADR-11](week3-adr-11-orchestrator-execution-engine.md) (control-plane/launcher split). Scope is **platform** observability only; agent observability (prompts, tool calls, tokens) is a separate, later ADR. |

## Context

ADR-5 split observability into two stores: an append-only, hash-chained Postgres audit trail (compliance, exact-match lookups) and a Prometheus + Grafana + Alertmanager stack for operational telemetry. It listed the operational signals it wanted (dependency availability, kill-switch timing, egress denials, quota/cost trends, the policy-gate consistency check) and deliberately rejected a heavy log-search cluster. Since then the orchestrator and launcher have been built (ADR-9 through ADR-11), and a review of the code against ADR-5 shows the following state:

- **Logging is in good shape.** Both binaries use `log/slog` (`internal/logging`), JSON output is available, and step-level lines carry `task_id`, `round`, `step_id` and `invocation_id`.
- **Nothing else from ADR-5's operational stack exists.** There is no metrics endpoint, no health or readiness endpoint, no tracing, no log shipping, and the compose stack has no Prometheus, Grafana or log store.
- **ADR-5 left log centralisation unspecified.** It rejected an enterprise search cluster but did not say where logs go, so retrieving everything for one task across the control-plane and launcher is not possible today.
- **The persisted data cannot answer timing or history questions.** `orchestrator.tasks.status` is one mutable value, `tasks.updated_at` is overwritten every round, and no terminal timestamp is stored. Total task duration, time spent in `waiting_approval`, round and phase durations, and launcher queue wait cannot be derived. `rounds` and `step_runs` have `created_at`, and `step_runs` also has `updated_at`, but nothing records when a phase started or ended.
- **Failure is not a state.** When a round errors, `taskrunner.fail` logs it, writes a free-text reason, and leaves the task `status=running` so startup reconciliation retries it. A failed task is indistinguishable from an active one except by reading the reason string, and nothing can count failures or detect a stuck task.
- **Launcher outcomes are coarse.** An invocation ends as `completed` or `error` (plus a cancellation variant); boot failure, timeout, guest-reported error and kill are not distinguished, and the time a launch spends queued behind the one-VM-at-a-time limit is invisible.
- **There is no correlation across the gRPC boundary beyond IDs passed as fields.** There is no trace ID and no request ID on HTTP requests.

The operator needs to answer: what is the platform doing now, how healthy is it, how are tasks trending, how many implement-review cycles tasks need (a proxy for implementation quality and cost), what failed and why, and — for one task — exactly what happened, in order, across every component. ADR-5's decisions cover part of this; this ADR covers the rest and settles the cross-cutting choices the requirements document and design will build on.

## Decision Drivers

- **Per-task explainability.** Any single task's full story, across control-plane and launcher, must be retrievable by one identifier.
- **Fleet-level trend visibility.** Throughput, durations (as distributions, not averages), loop counts and error rates must be dashboarded and alertable.
- **Loop count as a quality and cost signal.** More implement-review cycles means lower quality and higher cost; this must be measured directly.
- **Failure must be observable and actionable.** A failure needs a class, a retryability, and a state a query or alert can see.
- **Fit with ADR-5.** Prefer extending the chosen stack (Prometheus, Grafana, Alertmanager, Postgres) over introducing parallel systems.
- **Proportionality.** Task volume is low; infrastructure should be sized to it (ADR-5's reasoning still holds).
- **Security.** Platform logs, traces and metrics must not become a new path for secrets or sensitive content; the audit trail's integrity must not depend on the telemetry pipeline.
- **Production topology.** Production runs on Linux hosts, with the launcher on its own host (KVM required). Telemetry must work across hosts; macOS is a development environment only.

## Options Considered

### A. Instrumentation standard

1. **Prometheus client library only** (`client_golang`) for metrics, with logs as today and no tracing. Simplest, and exactly matches ADR-5. But a task would remain a set of disconnected log lines and counters; there is no way to see where time went inside one slow task, and phase durations would need separate hand-written histograms.
2. **OpenTelemetry for traces, metrics, and log correlation — chosen.** A task maps naturally to one trace (task → round → phase → step → launcher call → VM boot and guest run), which yields per-step and per-phase timing, cross-process correlation over gRPC, and metrics from the same instrumentation. OpenTelemetry metrics export to Prometheus, so ADR-5's stack is kept. Cost: a larger dependency and some pipeline to operate (a Collector).
3. **A hosted observability vendor.** Rejected: contradicts the self-hosted posture ADR-5 and the earlier ADRs have taken, and adds an external destination for task content.

### B. Log centralisation

1. **Stay on stderr plus host-local files.** Rejected: cannot reconstruct one task across two hosts.
2. **Loki — chosen.** Label-indexed, stores log lines cheaply, integrates natively with Grafana and Prometheus-style labels, and is proportionate to this volume. Does not do full-text indexing, which matches the actual query pattern (exact match on `task_id`, `invocation_id`, `trace_id`, time range).
3. **OpenSearch/Elasticsearch.** Rejected for the reason ADR-5 already gave: a distributed search cluster is disproportionate to the expected volume and query pattern.

### C. Where per-task history lives

1. **Derive it from the audit trail** by adding timestamps. Rejected: the audit trail is compliance-oriented, hash-chained and deliberately narrow; loading it with operational, high-churn data weakens the property it exists for, and its strict append-only grants make correcting a schema mistake costly.
2. **Metrics only.** Rejected: metrics are aggregates with bounded cardinality; they cannot answer "why did this task sit in `waiting_approval` for three days" or reconstruct one task's timeline.
3. **A dedicated append-only `orchestrator.task_events` table — chosen.** Operational history, queryable with SQL, written by the orchestrator in the same transaction as the state change it describes.

### D. Representing failure

1. **Keep `running` plus a free-text reason.** Rejected: failures cannot be counted, alerted on, or told apart from active tasks.
2. **A separate health flag alongside `status`.** Rejected as the primary mechanism: two fields that must be kept consistent, and the API consumer still has to combine them.
3. **A first-class failure state in the task status model — chosen**, with the retryable/terminal distinction expressed in it, so reconciliation's current "leave it `running` and retry" behaviour becomes explicit.

## Decision

### 1. Instrumentation: OpenTelemetry, exporting to the ADR-5 stack

Both the control-plane and the launcher are instrumented with the OpenTelemetry SDK for traces and metrics. Telemetry is sent over OTLP to an OpenTelemetry Collector on each host (or a shared one), which forwards metrics to Prometheus, traces to Tempo, and logs to Loki. Prometheus, Grafana and Alertmanager remain the metrics, dashboarding and alerting layer exactly as ADR-5 decided; this ADR adds Tempo (traces) and Loki (logs) beside them. Dependency health endpoints and ADR-5's listed signals are unchanged and still required.

**Correlation model.** Every log line, span and `task_events` row carries `task_id`; step-scoped ones also carry `round`, `step_id` and `invocation_id`; every log line within an active trace also carries `trace_id` and `span_id`. W3C Trace Context is propagated on HTTP and on gRPC (control-plane to launcher) so the launcher's work appears as child spans of the step that requested it. `task_id` is allowed in logs, traces and `task_events`, and **never** as a metric label. Metric labels are limited to low-cardinality dimensions (for example: step type, stage, status, error class, outcome, target repository, dependency name).

**Trace shape.** One trace per task *round-sequence*: a root span for the task, child spans per round, per phase (generate, review, approve, publish), per step, and under each step the launcher call (queue wait, jail and image staging, VM boot, guest run, result extraction and redaction) and the external calls (PDP, secrets issuer, Git host). A task paused at the approval gate is not held open as a single multi-day span: the span is ended at the pause, and the resumed work starts a new trace linked to the original by span link and the shared `task_id`. The exact span names and attributes belong to the design.

### 2. Two stores with distinct jobs

Metrics (Prometheus) serve fleet-level monitoring and alerting. Per-task history (`orchestrator.task_events`, below) serves per-task auditing of operational behaviour. Neither replaces the ADR-5 audit trail, which remains the tamper-evident record of delegated authority, policy decisions and outcomes. The metrics layer is expected to be lossy and short-retention; the audit trail and `task_events` are durable.

### 3. Per-task history: `orchestrator.task_events`

A new append-only table, written by the orchestrator, records every state transition with a timestamp: task accepted, round started and finished, each phase started and finished, each step launched and resolved, approval requested and granted, task resumed, retry or reconciliation taken, and the terminal outcome. Each event carries `task_id`, `round`, optional `step_id` and `invocation_id`, an event type, a timestamp, and a small structured payload (for example the error class, or the number of blocking findings). Events are written in the same database transaction as the corresponding `tasks`/`step_runs` update, so the history cannot diverge from the state. The orchestrator's application role gets `INSERT` and `SELECT` only on this table. The table is operational data: it is not hash-chained and is not part of the ADR-5 tamper-evidence guarantee; retention is a separate setting (see Open Questions).

This table is the source for task duration, time per status, time waiting for approval, and per-round and per-phase durations on a per-task basis. The same events are emitted as span events and as the data behind the corresponding metrics, so the two views stay consistent.

### 4. Failure is a first-class task state

The task status model gains an explicit failure representation. The design must settle the exact vocabulary, but the decision here is its semantics: a task that errored is distinguishable from an active one in the database, in `GET /tasks/{id}`, in metrics and in `task_events`; and a failure is recorded as either **retryable** (startup reconciliation will pick it up, preserving today's behaviour) or **terminal**. Every recorded failure carries a class from this taxonomy:

| Class | Meaning | Examples |
|---|---|---|
| `platform` | A defect or fault in the orchestrator or its persistence | Postgres error, unexpected round outcome |
| `infrastructure` | A fault in the execution environment | VM boot failure, jailer/Firecracker error, vsock or image-staging failure |
| `dependency` | An external service was unavailable or errored | Git host, PDP, secrets issuer, launcher unreachable |
| `agent` | The sandboxed run did not produce a valid outcome | Guest-reported error, timeout, no valid structured result |
| `policy` | An expected governance outcome, **not an error** | PDP deny, approval rejected, round cap reached |

`policy` outcomes are reported separately from errors and never counted in error-rate alerts. This changes the externally visible task-status contract, so the design must treat the `GET /tasks/{id}` change as a deliberate, documented API change, and must reconcile the new state with `Reconcile` and `Resume`.

### 5. Launcher outcomes are enumerated

The launcher's invocation outcome is refined from `completed`/`error` into a closed set sufficient to map to the taxonomy above (at least: completed, boot failure, timeout, guest-reported error, killed). The queue wait, boot time and guest run time of each invocation are measured separately. The gRPC contract change, if any, is a design matter.

### 6. Loop-quality measurement (version one)

Loop quality is measured with the signals the system already produces: the number of rounds per task, whether the first round was approved without further rounds, whether the round cap was hit, and per round whether blocking findings were present. Finding **severity** is out of scope for version one — the current review result carries a boolean `has_blocking_finding`, and richer severity is a recorded future extension tied to a change in the review result schema.

### 7. Signals added by this ADR

In addition to ADR-5's list, the platform exposes at least the following (precise names and buckets belong to the requirements document and design):

- **Task level:** tasks started, and tasks reaching each terminal outcome, over time; task duration distribution; time in each status; rounds per task distribution; first-round approval rate; round-cap hits.
- **Phase and step level:** duration distributions per phase and per step type; step outcomes.
- **Human wait versus machine time:** approval-pending age and approval turnaround reported separately from execution time, so execution metrics are not polluted by waiting on people.
- **Failures:** counts by class, by retryability, and by dependency; reconciliation and resume events; a **stuck-task** signal for tasks in an active state with no progress for a configurable interval.
- **Launcher:** queue depth and queue wait, boot time, guest run time, outcome counts, redaction event counts.
- **Dependencies:** latency and error rate per Git host, PDP, secrets issuer, launcher and Postgres.
- **Webhook intake:** received, rejected and failed.
- **Service health:** liveness and readiness endpoints on both binaries, build and version information.

### 8. Alerting is derived from SLOs

Alerts are defined from a small set of service-level objectives, not from every metric. The principle is decided here; the specific objectives and thresholds belong to the requirements document. Candidate SLOs include: tasks reach a terminal or waiting state within a bounded time, no task is stuck beyond a bounded interval, launcher queue wait stays below a bound, and dependency error rates stay below a bound. ADR-5's existing alerts (dependency outage, egress denials, policy-gate consistency, kill-switch timing) are retained.

### 9. Security of the telemetry pipeline

Logs, traces, metrics and `task_events` must never carry secret values; the existing practice of logging environment keys but not values (`logging.Keys`) is the rule. Log lines and span attributes carry identifiers and bounded metadata, not task descriptions or agent output. The launcher's literal-secret redaction of guest output (ADR-10) is unchanged and applies before any guest-derived content could enter a log or span. The Loki, Tempo, Prometheus and Grafana surfaces expose repository names and task identifiers and must be access-controlled; because the orchestrator HTTP API has no authentication yet, this is a dependency on future work, recorded below, and the observability stack must not be exposed on an unauthenticated network path in production.

### 10. Boundary with agent observability

This ADR covers platform behaviour only. To keep the next round plug-in rather than rework, two things are reserved now: the per-step span is the attachment point for any agent-level telemetry, and `task_events` payloads and the correlation model are open to additional event types. What is captured *inside* the guest (prompts, tool calls, model, tokens) is decided in the later ADR.

### 11. Delivery environments

The local compose stack gains the collector and the observability backends so instrumentation is exercised during development; otherwise it would rot. Production runs on Linux hosts with the launcher on a dedicated host: each host exports telemetry via its own collector or OTLP endpoint, and logs are shipped from each host to the central Loki. The OpenTelemetry resource attributes identify the component and host so a task trace spanning both hosts is coherent.

## Consequences

**Positive**
- One identifier reconstructs a task's full story across both hosts: logs in Loki, spans in Tempo, history in `task_events`.
- Phase, step and launcher durations come from the same instrumentation as the traces, instead of separately hand-built.
- The implement-review cycle count becomes a first-class, trendable metric with a defined first-version meaning.
- Failures become countable, classifiable and alertable; stuck tasks become detectable.
- ADR-5's stack is extended, not replaced; the audit trail's tamper-evidence is untouched.
- The agent-observability round has a defined attachment point.

**Negative / trade-offs**
- More components to run: an OpenTelemetry Collector, Tempo and Loki in addition to Prometheus, Grafana and Alertmanager.
- Instrumentation is a broad, cross-cutting change touching the task runner, core loop, executors, both gRPC layers, and the launcher.
- Making failure a first-class status is an API-visible change and interacts with reconciliation and resume; it carries regression risk in the most failure-sensitive part of the orchestrator.
- `task_events` adds a write per transition inside existing transactions; volume is low, but it is a new table that needs a retention policy.
- Two histories (`task_events` and the audit trail) overlap in content and must be kept conceptually distinct by convention rather than by mechanism.
- Without severity, version-one loop-quality distinguishes only blocking from non-blocking findings.

## Open Questions

- **Retention** for logs, traces, metrics and `task_events`; the audit trail's retention remains open under ADR-5.
- **Exact failure-state vocabulary** and the migration path for tasks currently persisted as `running` with an error reason.
- **Whether the launcher's gRPC outcome change is additive** (new field) or a contract revision; compatibility with the existing mock launcher.
- **Trace sampling policy.** Volume is low, so full sampling may be viable; to be confirmed against real volume.
- **Authentication and authorisation** for the HTTP API and the observability UIs, which this ADR depends on but does not decide.
- **Whether `task_events` should also be forwarded into the audit trail** for a subset of events (for example approvals) — currently no, as the audit trail already records them.
- **Finding severity** for loop-quality metrics, deferred to a review-result schema change.
- **Agent-level observability** — captured prompts, tool use and token cost — deferred to a later ADR.

## References

- [ADR-5: Observability & Audit Trail](week1-adr-5-observability-and-audit-trail.md)
- [ADR-7: Multi-Agent Review Workflow](week1-adr-7-multi-agent-review-workflow.md)
- [ADR-9: Orchestrator Foundations](week2-adr-9-orchestrator-foundations.md)
- [ADR-10: MicroVM Launch and Lifecycle Mechanics](week2-adr-10-microvm-launch-and-lifecycle-mechanics.md)
- [ADR-11: Orchestrator Execution Engine](week3-adr-11-orchestrator-execution-engine.md)
- [Orchestrator guide](../components/orchestrator.md) and [container-launcher guide](../components/container-launcher.md) — current-state descriptions the code review was grounded in
