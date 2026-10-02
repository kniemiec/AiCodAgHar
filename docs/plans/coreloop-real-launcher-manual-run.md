# Runbook: Driving `coreloop` End-to-End Through the Real Container Launcher

| | |
|---|---|
| **Status** | Draft |
| **Date** | 2026-09-21 |
| **Owner** | Krzysztof Niemiec |
| **Purpose** | Manual steps to run a real task through `internal/control-plane/coreloop` — generate → review → PR-create — backed by the real jailer-wrapped Firecracker launcher (`cmd/container-launcher`) instead of `launcher-mock`. Complements [`cmd/container-launcher/launcher_kvm_test.go`](../../cmd/container-launcher/launcher_kvm_test.go)'s `TestLaunchKVM_AgentStageRealForgejo`, which only exercises the launcher in isolation (calls `*launcherServer` directly, never opens a PR — `coreloop` does that, once the generate phase completes: `internal/control-plane/coreloop/coreloop.go` around the `CreateOrUpdatePR` call). |

## Why this is a different check than the KVM launcher test

`TestLaunchKVM_AgentStageRealForgejo` proves the launcher itself (jailer, fcapi, mmds, vsock, egress) boots a real microVM and drives one real OpenHands agent-stage invocation. It does **not** prove:

- that a `TASK_DESCRIPTION` sourced from a real `domain.Task` (via `internal/control-plane/executor/credentials.go`) reaches the agent correctly, rather than a hand-authored `Env` map in the test;
- that `coreloop` actually opens (or updates) a pull request after the generate-phase step completes;
- that the review phase (`review-with-commit` + `unit-tests`) runs against that same PR.

This runbook covers that gap by swapping `launcher-mock` for the real launcher inside the full compose stack.

## Prerequisites

Same as `TestLaunchKVM_AgentStageRealForgejo` (see that file's doc comment and `docs/components/container-launcher.md` §5), plus:

- The compose stack already up (`make compose-up && make bootstrap-admin && make migrate && make seed`) — `docker compose -f deploy/docker-compose.yml ps` should show `postgres`, `forgejo`, `secrets-mock`, `pdp-mock`, `launcher-mock`, `control-plane` all `Up`.
- The seeded `forgejo-admin/sample-repo` (created by `make seed`) — confirmed to carry `.harness/stages.yaml` wiring `implement` (generate) → `review-with-commit` + `unit-tests` (review). `pdp-mock` denies any repo without this file, so don't repurpose a scratch repo like `kvm-manual-test-repo` here without adding it first.
- `deploy/certs/{ca,server,client}.{crt,key}` already generated (`make compose-up`'s `gen-certs` dependency) — reused as-is, no new certs needed.

## Steps

**1. Stop the mocked launcher and the compose-managed control-plane** (leave postgres/forgejo/secrets-mock/pdp-mock running):

```
docker compose -f deploy/docker-compose.yml stop launcher-mock control-plane
```

**2. Run the real launcher on the host**, reusing `launcher-mock`'s former port and the same mTLS certs:

```
sudo -E env \
  LOG_LEVEL=debug \
  ADDR=:9443 \
  TLS_CA_CERT=deploy/certs/ca.crt \
  TLS_SERVER_CERT=deploy/certs/server.crt \
  TLS_SERVER_KEY=deploy/certs/server.key \
  LAUNCHER_CHROOT_BASE_DIR=/srv/fc-chroot \
  LAUNCHER_IMAGE_ROOT=$HOME/firecracker-poc/launcher-image-root \
  LAUNCHER_JAILER_BINARY=$HOME/firecracker-poc/bin/jailer \
  LAUNCHER_FIRECRACKER_BINARY=$HOME/firecracker-poc/bin/firecracker \
  LAUNCHER_LLM_API_KEY=$(cat $HOME/firecracker-poc/secrets/llm-api-key.txt) \
  LAUNCHER_LLM_MODEL=anthropic/claude-sonnet-4-5-20250929 \
  ~/.local/share/mise/installs/go/1.25.10/bin/go run ./cmd/container-launcher
```

Leave running in its own terminal — this is a long-lived gRPC server, not a one-shot test process.

`LOG_LEVEL` (`debug|info|warn|error`, default `info`) and `LOG_FORMAT` (`text|json`, default `text`) apply to both binaries (`internal/logging`). At `debug` the launcher logs every boot stage (`boot stage done stage=...`), each vsock envelope as it arrives, and the rendered prompt's prefix. At `info` you get one line per RPC, `launch accepted`/`microVM started`/`launch finished`, and which completion branch fired.

**3. Run `control-plane` on the host**, pointed at that launcher, using the host-side URLs `deploy/.env` already documents (`FORGEJO_URL`, `PDP_MOCK_URL`, `SECRETS_MOCK_URL`, `AUDIT_DB_DSN`, `ORCHESTRATOR_DB_DSN`):

```
set -a; . deploy/.env; set +a
LOG_LEVEL=debug \
LAUNCHER_ADDR=localhost:9443 \
FORGEJO_TOKEN="$FORGEJO_ADMIN_TOKEN" \
TLS_CA_CERT="$(pwd)/deploy/certs/ca.crt" \
TLS_CLIENT_CERT="$(pwd)/deploy/certs/client.crt" \
TLS_CLIENT_KEY="$(pwd)/deploy/certs/client.key" \
~/.local/share/mise/installs/go/1.25.10/bin/go run ./cmd/control-plane
```

Skip `WEBHOOK_APPROVAL_ENABLED=true` unless Forgejo's webhook is re-pointed at this host address — irrelevant for a `risk_tier: "low"` task anyway, since that path never reaches the approval gate.

**4. POST a task**, targeting the seeded `sample-repo`:

```
curl -s -X POST http://localhost:8080/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "target_repo": "forgejo-admin/sample-repo",
    "base_branch": "main",
    "risk_tier": "low",
    "triggering_principal": "manual-kvm-e2e",
    "bot_account": "requesting-account",
    "description": "Add power(a, b) and root(a, n) (n-th root) operations to the calculator, following existing style and tests."
  }'
```

Returns `{"task_id": "...", "status": "running"}` (202).

**5. Poll status** until terminal:

```
curl -s http://localhost:8080/tasks/<task_id>
```

Watch the `pr_number` field — set by `coreloop.go`'s `CreateOrUpdatePR` call once the generate-phase step completes.

**6. Confirm the real PR** at `http://localhost:3001/forgejo-admin/sample-repo/pulls/<pr_number>` — commits pushed by the real agent, then reviewed by `review-with-commit` + `unit-tests`.

**7. Cleanup**: Ctrl+C both host processes, then

```
docker compose -f deploy/docker-compose.yml start launcher-mock control-plane
```

to restore the normal mocked stack.

## Diagnosing a stuck or failed task

Logs first: every line carries `task_id`/`round`/`step_id`/`invocation_id`, so `grep <task_id>` on the control-plane output gives the round → phase → step → launch sequence. A round that errors logs `task failed; left status=running for reconciliation` with the error, and also writes that error into the task's `reason`, which `GET /tasks/<task_id>` returns. The task stays `running` on purpose, so a control-plane restart retries it. On the launcher side, grep for the `invocation_id`. The jailer/firecracker console output goes to `/tmp/container-launcher-<invocation_id>.log`, logged as `jailer_log=`.

Then Postgres, which records which stages finished, though not why:

```
docker compose -f deploy/docker-compose.yml exec postgres psql -U postgres -d orchestrator
```

```sql
SELECT id, status, current_round, pr_number, branch, reason, updated_at
FROM orchestrator.tasks WHERE id = '<task_id>';

-- launched -> completed | failed. exit_code/has_finding are NULL for failed rows,
-- and GET /tasks shows them as 0/false
SELECT step_id, invocation_id, status, exit_code, has_finding, structured_result, updated_at
FROM orchestrator.step_runs WHERE task_id = '<task_id>' ORDER BY id;

-- 'pdp-decision' per round (policy_decision, resolved_steps), one row per launch (status), 'task-outcome' at the end
SELECT id, step_type, step_id, invocation_id, policy_decision, resolved_steps, status, exit_code, created_at
FROM audit.invocation_audit WHERE task_id = '<task_id>' ORDER BY id;
```

## Cost/time note

This runs two real OpenHands agent invocations (`implement` + `review-with-commit`) plus a deterministic `unit-tests` step, not one — expect roughly double the ~20-minute wall-clock and LLM spend of the single-invocation `TestLaunchKVM_AgentStageRealForgejo` run.
