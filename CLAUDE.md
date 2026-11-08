# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

For what this project is, its architecture, repo layout, docs map, and how to bring up the local stack, read [`README.md`](README.md) first. This file only adds what you need when *changing* the code.

## Commands

Toolchain is pinned in `.mise.toml` (Go, buf). There is no linter/formatter config — only `buf lint`.

```sh
make build                 # go build ./...
make test                  # go test ./... (unit tests only; no build tags)
go test ./internal/control-plane/coreloop -run TestName -count=1   # single test
make proto-gen             # buf lint + generate → api/gen/go (from api/proto/launcher/v1)
```

Tests are split by **build tags** (`integration`, `e2e`, `launcherkvm`; see the README's test table); plain `go test` skips all of them. `make test-spike` also exists, for an exploratory gitclient suite. Unit tests run against `internal/control-plane/fakes`; the `mocks/` services only exist for the compose stack.

## Local stack gotchas

Bring-up and details are in `docs/local-development.md`. Things that commonly bite:
- `docker compose up -d` does **not** rebuild images. After changing `cmd/control-plane` or `mocks/*`, run `docker compose -f deploy/docker-compose.yml build <svc>`, then `up -d`, then `make seed`.
- Use `-count=1` for live-stack tests; a cached "ok" ignores rebuilt images.
- `make test-*` source `deploy/.env` with bash, so JSON values in it (e.g. `SECRETS_MOCK_WEBHOOK_POOL_JSON`) must be wrapped in single quotes.
- Debugging: `LOG_LEVEL` (`debug|info|warn|error`) and `LOG_FORMAT=json`. Log lines carry `task_id`/`round`/`step_id`, so one task can be followed across control-plane and launcher.

## Docs rules

Read the matching component doc before changing a component:
- `docs/components/orchestrator.md`, `docs/components/container-launcher.md` are **living** current-state guides. When they disagree with `docs/designs/` or ADRs, trust these. Update them in the same change that makes them stale. Keep `README.md` in sync too if a change affects its overview, commands or setup steps.
- `docs/architecture/` (ADRs), `docs/designs/completed/` and `docs/intents/` are frozen historical records. Don't edit them to match new code.
- `.claude/skills/wf-design` writes new designs to `docs/designs/active/` (created on first use).

## Invariants to preserve

- **`coreloop` vs `taskrunner`:** `coreloop.RunRound` is a pure single-round function that knows nothing about round caps or resumption. Phases run sequentially, but steps within a phase run concurrently, so don't share mutable state across steps. `taskrunner.Runner` owns the loop across rounds, persists `TaskState` after each round, and resumes tasks (`Reconcile` at startup, `Resume` after approval, which re-runs the *same* round and skips completed steps).
- The orchestrator (not the sandboxed agent) creates the branch and PR; the agent only pushes commits.
- The HTTP API (`cmd/control-plane`) has **no authn/authz yet**. Don't assume a caller is authenticated when adding or changing endpoints.
- Dependencies are interfaces under `internal/clients/*` and `internal/control-plane/clients/*`, with hand-written fakes in `internal/control-plane/fakes/` (ADR-9 test seams). Add or extend the fake alongside any interface change.
- Every decision/step is written to the hash-chained Postgres audit trail (`audit/`).
- Step IDs (`<task_id>-r<round>-<step_id>`) must match `[A-Za-z0-9_-]`, because the real launcher embeds them in jail paths.
- Launcher: one VM at a time (concurrent `Launch` calls queue). Secrets go in via MMDS only, and results come back over vsock and are redacted before leaving. Config is `LAUNCHER_*` env vars in `cmd/container-launcher/config.go`. `guest_wrapper.py` is baked into the golden image manually.
- Mocks must stay wire-compatible with the real contracts (`api/openapi/*.yaml`, the launcher proto). Generated code in `api/gen/go` is checked in, so regenerate with `make proto-gen` after proto changes.
- Target-repo config (`.harness/stages.yaml`, `policy.yaml`, `APPROVERS`) is parsed in `internal/config`. The two YAML files are validated against `schemas/*.schema.json`; update the fixtures in `schemas/fixtures/` together with the schema. `APPROVERS` has no schema (hand-parsed in `internal/config/approvers.go`). SQL migrations live in `deploy/migrations/`.
