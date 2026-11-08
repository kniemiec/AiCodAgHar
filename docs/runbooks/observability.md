# Observability runbook

| | |
|---|---|
| **Status** | Living document. A skeleton since PR D of [`platform-observability-design.md`](../designs/active/platform-observability-design.md); PR F adds one section per alert, and PR E the trace procedure |
| **Scope** | Finding what happened to one task, reading the pipeline's signals, and what to do when the pipeline itself is down (OP-2) |

The pipeline: both binaries export metrics and non-sensitive logs over OTLP to a Collector on their host (`OTEL_EXPORTER_OTLP_ENDPOINT`). The Collector forwards logs to Loki, metrics to Prometheus (remote write) and traces to Tempo. Grafana reads all three. Locally the whole pipeline is part of `make compose-up` (`deploy/docker-compose.observability.yml`; ports in [`local-development.md` §7](../local-development.md#7-observability-stack)).

## 1. One task, from one ID

You need the `task_id` (`task-<n>`, from the `POST /tasks` response, `GET /admin/tasks?status=…`, or the review PR's branch name `task/<task_id>`).

| What | How | Source of truth |
|---|---|---|
| **Timeline** (state changes, rounds, steps, approvals, failure) | `curl -s localhost:8080/tasks/<task_id>/events` | `orchestrator.task_events` in Postgres; never lossy |
| **Current state** (status, `error_class`, `retryable`) | `curl -s localhost:8080/tasks/<task_id>` | `orchestrator.tasks` |
| **Logs**, both components, time order | `make task-logs TASK=<task_id>` (= `deploy/observability/task-logs.sh <task_id> [days]`) | Loki, 14-day retention; lossy |
| **Trace** | Not yet: spans arrive with PR E | Tempo, 7-day retention |
| **Audit trail** (policy decisions, step outcomes, hash chain) | `psql` query in [`local-development.md` §4](../local-development.md#4-using-it) | `audit.invocation_audit` |

`task-logs.sh` runs `{service_name=~"control-plane|container-launcher"} | task_id="<task_id>"` against Loki (`LOKI_URL`, default `http://localhost:3100`) over the last 30 days, pages through the whole window, and prints one line per record: time, service, level, message, then the record's fields (`round`, `step_id`, `invocation_id`, `error_class`, …). In Grafana → Explore → Loki, the same query works directly; `task_id` is a field (structured metadata), not a stream label, so it goes after the `|`, never inside `{…}`.

Two things are deliberately **not** in Loki: records marked sensitive (the launcher's prompt prefix, guest event-log tail and structured results, the executor's structured-result line) stay on each process's stderr only (`docker compose -f deploy/docker-compose.yml logs control-plane`, or the launcher host's process manager). Agent output and task descriptions never reach the central store.

A task that failed: its `task.failed` event and `GET /tasks/{id}` carry `error_class`/`retryable`, and `task-logs` shows the `task failed` error line with the `dependency` and error text. A retryable failure is resumed by an operator: `curl -X POST localhost:8090/admin/tasks/<task_id>/retry -d '{"operator":"you"}'` (admin listener, host loopback only).

## 2. Alerts

*To be completed in PR F*, one section per alert rule in `deploy/observability/prometheus/rules/`, each with its meaning, first diagnostic and the alert's `runbook_url` anchor. Alertmanager currently has a null receiver: alerts are visible at `http://localhost:9093`, and nothing is routed to people (ADR-5 leaves routing to process).

## 3. When the pipeline itself is down

Telemetry is lossy by design: the SDKs export asynchronously through bounded queues and drop on overflow, so a dead Collector, Loki or Prometheus never slows or fails a task (CC-4). What is lost is only what was exported while it was down.

| Symptom | First check | Still available |
|---|---|---|
| `make task-logs` says "Loki query failed" | `docker compose -f deploy/docker-compose.yml ps loki otel-collector`; `curl localhost:3100/ready` | `task_events` timeline (`/tasks/{id}/events`), stderr (`docker compose … logs control-plane`) |
| `task-logs` finds nothing for a task you know ran | The Collector's log: `docker compose … logs otel-collector`; is `OTEL_EXPORTER_OTLP_ENDPOINT` set on the process? Older than 14 days → past Loki retention | Same as above |
| Metrics missing in Prometheus/Grafana | `curl -s localhost:9090/api/v1/targets` (the pipeline's own components, `up`); `harness_telemetry_dropped_total` on the process once it is back | `task_events` and the gauges' source tables |
| Collector up but exporting fails | Collector log (exporter errors name the backend); the process's stderr shows rate-limited `telemetry export error` lines | — |

Restarting any pipeline service is safe: `docker compose -f deploy/docker-compose.yml restart <service>`. Their data is in named volumes (`prometheus-data`, `loki-data`, `tempo-data`, `grafana-data`, `alertmanager-data`); `make compose-down` keeps them, `docker compose … down -v` deletes them.

## 4. Across hosts (production shape)

In production each host runs its own Collector, with exporters pointed at the observability host; all traffic is outbound from the hosts (CC-8). Every log stream and metric series carries `service_name` (`control-plane` / `container-launcher`), `host_name` and `deployment_environment`, so one task's lines from both hosts merge by time in `task-logs`. Ordering across hosts assumes their clocks are synced (NTP/chrony).

The local stack runs `launcher-mock`, which exports no telemetry, so locally `task-logs` shows control-plane lines only. To check the two-component path by hand on a Linux/KVM host: run the real `container-launcher` with `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4317` (the Collector publishes 4317/4318 on host loopback) and `OTEL_RESOURCE_ATTRIBUTES=deployment.environment=local`, switch the control-plane to it (`deploy/docker-compose.real-launcher.yml`), run a task, and confirm `make task-logs` shows `container-launcher` lines interleaved with `control-plane` ones.
