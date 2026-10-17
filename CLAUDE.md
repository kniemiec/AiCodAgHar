# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

A Go module implementing an AI coding-agent harness: an **orchestrator** (`cmd/control-plane`) drives a review/automation loop against a Git repo, dispatching sandboxed agent steps through a **container launcher** (`cmd/container-launcher`, Firecracker microVMs). Toolchain is pinned in `.mise.toml` (Go, buf); `mise install` gets both. There is no linter/formatter config — only `buf lint`.

## Commands

```sh
make build                 # go build ./...
make test                  # go test ./... (unit tests only; no build tags)
go test ./internal/control-plane/coreloop -run TestName -count=1   # single test
make proto-gen             # buf lint + generate → api/gen/go (from api/proto/launcher/v1)
```

Tests are split by **build tags**; plain `go test` skips all of these (`make test-spike` also exists, for an exploratory gitclient suite). Unit tests run against `internal/control-plane/fakes`; the `mocks/` services only exist for the compose stack.

| Tag | Make target | Needs |
|---|---|---|
| `integration` | `make test-integration` | local stack up (below), `deploy/.env`, mTLS certs |
| `e2e` | `make test-e2e` | full local stack, seeded (`internal/control-plane/e2e`) |
| `launcherkvm` | `make test-launcher-kvm` | Linux + `/dev/kvm` + root + real jailer/firecracker; can't run on macOS |

Local stack (Forgejo + Postgres + 3 mocks + control-plane, plus the Collector/Prometheus/Loki/Tempo/Grafana pipeline via `include:` of `deploy/docker-compose.observability.yml`): `cp deploy/.env.example deploy/.env`, then `make compose-up bootstrap-admin migrate seed`. Details, ports, and the approval-webhook walkthrough are in `docs/local-development.md`. Gotchas:
- `docker compose up -d` does **not** rebuild images — after changing `cmd/control-plane` or `mocks/*`, run `docker compose -f deploy/docker-compose.yml build <svc>` then `up -d`, then `make seed`.
- Use `-count=1` for live-stack tests; a cached "ok" ignores rebuilt images.
- `make test-*` source `deploy/.env` with bash, so JSON values in it (e.g. `SECRETS_MOCK_WEBHOOK_POOL_JSON`) must be wrapped in single quotes.
- Forgejo is on `:3001`, not `:3000`; Grafana is on `127.0.0.1:3002`. `make task-logs TASK=<id>` prints one task's lines from Loki.
- Debugging: `LOG_LEVEL` (`debug|info|warn|error`) and `LOG_FORMAT=json`; log lines carry `task_id`/`round`/`step_id`, so one task can be followed across control-plane and launcher.

## Architecture

Read the matching doc before changing a component:
- `docs/components/orchestrator.md`, `docs/components/container-launcher.md` — **living** current-state guides; when they disagree with `docs/designs/` or ADRs, trust these. Update them in the same change that makes them stale.
- `docs/architecture/` (ADRs), `docs/designs/completed/`, `docs/intents/` — frozen historical records; don't edit to match new code.
- `.claude/skills/wf-design` writes new designs to `docs/designs/active/` (created on first use).

**Orchestrator** (`internal/control-plane/*`). The key boundary is `coreloop` vs `taskrunner`:
- `coreloop.RunRound` is a pure single-round function: PDP evaluate → create task branch → generate phase → open review PR → review phase → aggregate → approve gate → `publish`. Phases run sequentially, steps within a phase concurrently. It knows nothing about round caps or resumption.
- `taskrunner.Runner` owns the loop across rounds, persists `TaskState` to Postgres after each round, resumes tasks at startup (`Reconcile`) and after approval (`Resume`, which re-runs the *same* round, skipping completed steps).
- The orchestrator (not the sandboxed agent) creates the branch and PR; the agent only pushes commits.
- Dependencies (Git host, PDP, secrets issuer, launcher) are interfaces under `internal/clients/*` and `internal/control-plane/clients/*`, with hand-written fakes in `internal/control-plane/fakes/` (ADR-9 test seams). Every decision/step is written to a hash-chained Postgres audit trail (`audit/`).
- Step IDs (`<task_id>-r<round>-<step_id>`) must match `[A-Za-z0-9_-]` — the real launcher embeds them in jail paths.

**Launcher** (`cmd/container-launcher`, `internal/launcher/*`). gRPC+mTLS (`api/proto/launcher/v1`; generated code in `api/gen/go` is checked in). One jailer-wrapped Firecracker microVM per invocation, one VM at a time (concurrent `Launch` calls queue). Config is `LAUNCHER_*` env vars read in `cmd/container-launcher/config.go`. Secrets go in via MMDS only, results come back over vsock and are redacted before leaving. `guest_wrapper.py` is baked into the golden image manually.

**Mocks** (`mocks/pdp-mock`, `launcher-mock`, `secrets-mock`) are local stand-ins for the orchestrator's other dependencies, wire-compatible with the real contracts (`api/openapi/*.yaml`, the launcher proto). They are not part of any component's architecture.

**HTTP API** (documented in `docs/components/orchestrator.md` §4): `POST /tasks` (only `target_repo` required; returns 202), `GET /tasks/{id}` to poll, `POST /webhooks/forgejo`. No authn/authz exists yet.

**Config contracts**: `.harness/stages.yaml`, `.harness/policy.yaml`, `.harness/APPROVERS` live in *target* repos; `internal/config` parses them and validates against `schemas/*.schema.json` (fixtures in `schemas/fixtures/`). SQL migrations are in `deploy/migrations/` (applied by `make migrate`).
