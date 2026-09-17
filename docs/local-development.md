# Local development environment

| | |
|---|---|
| **Status** | Living document — describes the current local dev/test setup; update it in the same change that makes it stale |
| **Scope** | Bringing up a full local stack to develop and test against: the real orchestrator ([`cmd/control-plane`](components/orchestrator.md)) plus a real Git host and Postgres, with its other three dependencies — the Policy Decision Point, secrets/credential issuer, and step launcher — currently filled by local stand-in implementations under `mocks/` (each satisfies the same contract a real implementation would; see [`docs/components/orchestrator.md`§5](components/orchestrator.md#5-contracts)) |

This is a testing/dev-environment concern, not part of the orchestrator's own architecture — for what the orchestrator *is*, see [`docs/components/orchestrator.md`](components/orchestrator.md).

## 1. What's in the stack

| Service | Stands in for | Source | Port |
|---|---|---|---|
| `control-plane` | — (the real orchestrator) | `cmd/control-plane` | `8080` |
| `pdp-mock` | Policy Decision Point | `mocks/pdp-mock` | `8081` |
| `secrets-mock` | Secrets / credential issuer | `mocks/secrets-mock` | `8082` |
| `launcher-mock` | Step launcher | `mocks/launcher-mock` | `9443` (mTLS only) |
| `forgejo` | Git host (real) | `codeberg.org/forgejo/forgejo:16` | `3001` (UI/API), `2222` (SSH) |
| `postgres` | `orchestrator` + `audit` schemas (real) | `postgres:16-alpine` | `5432` |

`pdp-mock` computes real decisions by reading `.harness/*.yaml` live from Forgejo. `launcher-mock` is fixture-driven but performs real Forgejo side effects (commits, comments) using the credential injected for each step. `secrets-mock` hands out a static, pre-provisioned bot-token pool. None of the three persist meaningfully beyond process lifetime.

## 2. Prerequisites

Go toolchain, Docker + Docker Compose, `openssl` (cert generation).

## 3. Bring it up

```sh
cp deploy/.env.example deploy/.env
#    (edit passwords if you want; the defaults are fine for local dev)

make compose-up          # Forgejo + Postgres + the 3 mocks + control-plane; generates dev mTLS certs
make bootstrap-admin      # mints a Forgejo admin token, recreates pdp-mock/control-plane to pick it up
make migrate               # applies deploy/migrations/*.sql
make seed                  # provisions bot accounts + the global-baseline and sample-repo config repos
```

After that: Forgejo at `http://localhost:3001` (admin login `forgejo-admin` / `changeme-local-only` — **not** `:3000`, and **not** the reserved `admin` username), `control-plane` at `http://localhost:8080`.

**If you change code** in `cmd/control-plane`, `mocks/pdp-mock`, `mocks/launcher-mock`, `mocks/secrets-mock`, or anything they import, rebuild the relevant image(s) — `docker compose up -d` alone does not rebuild:

```sh
docker compose -f deploy/docker-compose.yml build control-plane pdp-mock launcher-mock secrets-mock
docker compose -f deploy/docker-compose.yml up -d control-plane pdp-mock launcher-mock secrets-mock
make seed   # re-seeds .harness/stages.yaml and restarts secrets-mock/pdp-mock/control-plane
```

`go test`'s result cache doesn't know about a rebuilt image — pass `-count=1` to force a real re-run of a live-stack test rather than trusting a stale "(cached) ok".

Teardown: `make compose-down`. `make seed` is idempotent and safe to re-run any time (resets the two config repos, mints fresh bot tokens).

## 4. Using it

```sh
curl -s -X POST http://localhost:8080/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "target_repo": "forgejo-admin/sample-repo",
    "branch": "feature-branch",
    "base_branch": "main",
    "task_type": "pr-review-round",
    "risk_tier": "low",
    "triggering_principal": "manual-test",
    "bot_account": "requesting-account",
    "pr_title": "Manual test PR",
    "pr_body": "Testing the orchestrator"
  }' | python3 -m json.tool
```

See [`docs/components/orchestrator.md`§4](components/orchestrator.md#4-api) for the response shape (a `202` — poll `GET /tasks/{id}` for progress). Other scenarios against the seeded sample repo:
- `"risk_tier": "high"` → the global-baseline policy repo requires human approval at that tier; the task reaches `waiting_approval` once its normal round steps finish clean (design §4.4 — an approve-phase gate doesn't imply zero launches).
- Request POSTed twice against the same branch → the existing PR is found and updated, not duplicated.

### Trying the approval webhook flow (design §4.4)

The seeded sample repo doesn't have `.harness/APPROVERS` or a webhook, so a `waiting_approval` task there will sit forever without one. To actually clear a gate locally:

1. Set `WEBHOOK_APPROVAL_ENABLED=true` on `control-plane` (already the default in `deploy/docker-compose.yml`).
2. Add a `.harness/APPROVERS` file to the target repo listing the Forgejo username that will label the issue.
3. Register a webhook on that repo pointing at `http://control-plane:8080/webhooks/forgejo` (in-network address — Forgejo, not your host, delivers it), `content_type: json`, with some secret.
4. Provision that same secret in `secrets-mock`: `SECRETS_MOCK_WEBHOOK_POOL_JSON='{"<owner>/<repo>":"<secret>"}'` in `deploy/.env` — **single-quote the whole value**. `make test-e2e`/`test-integration` source `deploy/.env` with real bash (`set -a; . deploy/.env`), which strips *unquoted* embedded double quotes; Docker Compose's own `.env` parser doesn't have this problem, but a value that has to work in both places needs the outer single quotes regardless.
5. `GET /tasks/{id}` for `approval_issue_number`, then label that issue `approved` or `rejected` as the user from step 2.

`internal/e2e/smoke_test.go`'s `TestSmoke_ApprovalWebhookFlow` does exactly this against its own fixed scratch repo — read it for a fully worked example, including the `webhook.ALLOWED_HOST_LIST` note below.

Verify side effects directly:

```sh
# Forgejo
set -a; . deploy/.env; set +a
curl -s -H "Authorization: token $FORGEJO_ADMIN_TOKEN" \
  "$FORGEJO_URL/api/v1/repos/forgejo-admin/sample-repo/issues/1/comments"

# Postgres audit trail
docker compose -f deploy/docker-compose.yml exec -T postgres \
  env PGPASSWORD=orchestrator_audit_writer \
  psql -U orchestrator_audit_writer -d orchestrator -c \
  "SELECT invocation_id, step_type, policy_decision, status FROM audit.invocation_audit WHERE task_id = 'task-...' ORDER BY id;"

# Postgres task state
docker compose -f deploy/docker-compose.yml exec -T postgres \
  env PGPASSWORD=orchestrator_app \
  psql -U orchestrator_app -d orchestrator -c \
  "SELECT id, status, current_round FROM orchestrator.tasks ORDER BY created_at DESC LIMIT 5;"
```

## 5. Testing against the stack

| Layer | Needs | Command | What it proves |
|---|---|---|---|
| Integration | `compose-up` + `bootstrap-admin` + `migrate` | `make test-integration` | Each production client against its real/stand-in counterpart — incl. mTLS enforcement, the Postgres-role privilege test, and the tamper-detection test |
| Spike | `compose-up` + `bootstrap-admin` | `make test-spike` | `code.gitea.io/sdk/gitea` actually covers `GitHostClient`'s surface |
| E2E smoke | Full stack + `make seed` | `make test-e2e` | The real system, driven through its real HTTP API — see `internal/e2e/smoke_test.go` |

## 6. Troubleshooting

- **Forgejo admin login doesn't work.** Check you're on **`:3001`**, not `:3000` — a leftover, unrelated Forgejo container from earlier work may be squatting on the default port. `docker ps --format '{{.Names}}: {{.Ports}}'` shows both if so.
- **`pdp-mock` / `control-plane` crash-looping with "required env var FORGEJO_TOKEN is not set."** Expected before `make bootstrap-admin` has minted a token on a first-ever `compose-up` — it recreates both containers once the token exists. If you see this after that, check `deploy/.env` actually has `FORGEJO_ADMIN_TOKEN` set.
- **A migration you just applied doesn't seem to take effect.** Postgres client connections (including `control-plane`'s pool) can cache prepared-statement type metadata from before a schema change. Restart the affected container after altering a column type.
- **`make seed` fails with a password/auth error for a bot account.** If a bot account already exists with a different password than `cmd/seed` expects, it deletes and recreates it — expected and safe for these throwaway local accounts.
- **A webhook to `control-plane` never arrives; Forgejo logs `webhook can only call allowed HTTP servers (check your webhook.ALLOWED_HOST_LIST setting)`.** Forgejo's own SSRF guard blocks webhook delivery to private/loopback addresses by default — but `control-plane` is only ever reachable at a private compose-network address from Forgejo's side, so the default silently breaks every approval webhook in this exact topology. `deploy/docker-compose.yml` sets `FORGEJO__webhook__ALLOWED_HOST_LIST: "private,loopback"` on the `forgejo` service for this reason; if you're seeing this, that service definition changed or the container hasn't picked it up (`docker compose up -d forgejo` to recreate it).
