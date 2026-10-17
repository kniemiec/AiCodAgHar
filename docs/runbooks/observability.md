# Observability runbook

| | |
|---|---|
| **Status** | Skeleton (platform-observability design PR D). Trace lookup arrives with PR E; per-alert entries and dashboards with PR F |
| **Scope** | Finding what happened to one task, and what to do when the telemetry pipeline itself is down |

The pipeline is described in [`docs/designs/active/platform-observability-design.md`](../designs/active/platform-observability-design.md) §11; ports and versions for the local stack are in [`docs/local-development.md`](../local-development.md) §1. Telemetry is **lossy by design**: Postgres `task_events` and each process's stderr are the records that survive a pipeline outage.

## 1. One task, from its ID

| What | How |
|---|---|
| State and step runs | `curl localhost:8080/tasks/<task_id>` |
| Timeline (every state change, phase, heartbeat, failure, retry) | `curl localhost:8080/tasks/<task_id>/events` — filters `type`, `since`, `until`, `after_id`, `limit` ([`orchestrator.md`](../components/orchestrator.md) §4) |
| Logs, both binaries, time order | `make task-logs TASK=<task_id>` (or `deploy/observability/task-logs.sh <task_id>`; `LOKI_URL` overrides `http://localhost:3100`). In Grafana → Explore → Loki: `{service_name=~"control-plane\|container-launcher"} \| task_id="<task_id>"`. Narrow with `\| step_id="…"`, `\| round="2"` or `\| invocation_id="…"` |
| Trace | *PR E* |

Log lines marked sensitive (prompt prefix, structured agent result, guest event-log tail) never reach Loki; they are on the producing host's stderr only (`docker compose -f deploy/docker-compose.yml logs control-plane`, or the launcher host's process-manager log).

Cross-host ordering in `task-logs` assumes the hosts' clocks are synchronised (NTP/chrony).

## 2. When the pipeline is down

A dead Collector, Loki, Tempo or Prometheus never slows or fails a task; the binaries drop what they can't export.

| Symptom | First check |
|---|---|
| `task-logs` prints nothing for a task that ran | `docker compose -f deploy/docker-compose.yml logs otel-collector loki`. Then the producer: `harness_telemetry_dropped_total` rising, or `telemetry export error` lines on its stderr, mean it can't reach the Collector (`OTEL_EXPORTER_OTLP_ENDPOINT`) |
| Metrics missing in Prometheus | `up{job="otel-collector"}` in Prometheus; the Collector's own `otelcol_exporter_send_failed_metric_points` |
| Loki rejects lines | Loki logs `structured_metadata_too_large`/`_too_many`; `loki_discarded_samples_total{reason=…}` |
| Need the history anyway | `GET /tasks/{id}/events` (Postgres) and the hosts' stderr still have it |

## 3. Alerts

*PR F*: one entry per alert rule — what it means and its first diagnostic. Alertmanager currently routes everything to a null receiver; firing alerts are visible at `http://localhost:9093`.

## 4. Cross-host check (manual)

Compose runs `launcher-mock`, which exports no telemetry, so local runs only prove the control-plane half. On the Linux hosts: point both binaries' `OTEL_EXPORTER_OTLP_ENDPOINT` at their host's Collector, run one task through the real launcher, and confirm `make task-logs TASK=<id>` (with `LOKI_URL` pointing at the observability host) shows `container-launcher` lines interleaved with `control-plane` lines in time order.
