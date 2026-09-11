# Orchestrator Control-Plane MVP — Development Plan

| | |
|---|---|
| **Status** | Draft |
| **Date** | 2026-09-08 |
| **Owner** | kniemiec |
| **Purpose** | Get from a blank repo to a running, testable orchestrator control-plane implementing ADR-11, backed by real Forgejo + Postgres and mocked PDP / Secrets / Launcher services, with every orchestrator↔external-service contract formally defined before it's implemented. |

## Goal

Stand up a running orchestrator control-plane (Go) that executes [ADR-7](../../architecture/week1-adr-7-multi-agent-review-workflow.md)'s fixed core loop (`generate → fan out → aggregate → evaluate → continue/stop-success/stop-escalate`) end-to-end for one real task, against a docker-compose stack of real Forgejo + real Postgres + mocked PDP + mocked SecretsClient + mocked ContainerLauncher, with both config repos seeded, audit records written and hash-chain-verifiable, and every contract between the orchestrator and an external service ([ADR-11](../../architecture/week3-adr-11-orchestrator-execution-engine.md)'s PDP, launcher/jailer+agent, and the other ADR-9-named dependencies) defined as a versioned artifact before implementation begins.

This is a **bare-minimum, mocked-dependencies** slice: it proves the orchestrator's own control flow, its `StepExecutor` registry extensibility point, and its contracts — not the real PDP (OPA), real secrets minting (OpenBao), or the real Firecracker/jailer launcher (ADR-10), all of which are validated separately or deferred.

## Prerequisites

- Go toolchain
- Docker / docker-compose
- `protoc` / `buf` for gRPC codegen
- An OpenAPI + JSON Schema validator for spec/fixture checks

## Context — Why This Shape

[ADR-11](../../architecture/week3-adr-11-orchestrator-execution-engine.md) (Accepted, 2026-09-08) specifies the orchestrator's process architecture (unprivileged control-plane + privileged root launcher, split over a local IPC boundary), a closed-set `StepExecutor` registry keyed by step `type` (`agent-stage` | `deterministic-step`), per-task step applicability resolved by the same PDP call the orchestrator already makes (response gains a `resolved_steps` field), and Go as the implementation language for both components. It builds on [ADR-9](../../architecture/week2-adr-9-orchestrator-foundations.md) (config storage, six named-but-unspecified test-seam interfaces: `GitHostClient`, `PolicyClient`, `SecretsClient`, `AuditWriter`, `ApprovalVerifier`, `ContainerLauncher`), [ADR-3](../../architecture/week1-adr-3-policy-enforcement-layer.md) (PDP = OPA via the AuthZEN Authorization API, fail-closed), [ADR-5](../../architecture/week1-adr-5-observability-and-audit-trail.md) (append-only hash-chained Postgres audit trail), and [ADR-10](../../architecture/week2-adr-10-microvm-launch-and-lifecycle-mechanics.md) (real jailer/Firecracker mechanics, validated in a separate PoC — [`firecracker-microvm-launch-poc.md`](../firecracker-microvm-launch-poc.md)).

No orchestrator code exists yet — this repo is otherwise docs-only. Two prior PoCs give directly reusable operational knowledge that this plan's mocks lean on:
- [`openhands-localruntime-validation.md`](../openhands-localruntime-validation.md) — Forgejo bot-account/PAT patterns, headless-agent-CLI gotchas.
- [`firecracker-microvm-launch-poc.md`](../firecracker-microvm-launch-poc.md) — jailer/MMDS/vsock/nftables mechanics (not needed here since the launcher is mocked, but the real launcher slot this plan leaves open must eventually match it), and a measured ~79-86s wall clock per real agent invocation (relevant to future load/timing expectations, not this plan's mocked timings).

## Decisions Locked In For This Plan

1. **Control-plane ↔ Launcher IPC: loopback gRPC + mTLS.** Chosen over a unix domain socket because in docker-compose these are separate containers on a bridge network, not same-host processes — mTLS, not network locality, is what enforces the privilege boundary ADR-11 calls out as needing "real authentication."
2. **Mock scope: only Forgejo + Postgres are real.** PDP, SecretsClient, and ContainerLauncher are all mocked as their own docker-compose services implementing the real contracts.
3. **Config repos: two.** (a) a sample **target repo** with `.harness/policy.yaml` + `.harness/stages.yaml` that the orchestrator actually operates against, and (b) a separate **platform-owned global-baseline policy repo**, matching ADR-9/ADR-6's real shape.
4. **Human-approval flow deferred.** `ApprovalVerifier` gets a fail-closed stub (`NoopApprovalVerifier`, always `Approved: false`); the real Forgejo issue+label+webhook-HMAC mechanism (ADR-9) is out of scope for this plan.
5. **Secrets-mock fidelity: static pre-provisioned bot-token pool**, not real per-invocation Forgejo token mint/revoke (an intentional simplification, upgradeable later).
6. **`launcher-mock` performs real Forgejo side-effects** (branch/commit/PR/comment) via a small closed set of fixture-driven behaviors, rather than returning purely canned data — this proves the credential round-trip and real git-visible outcomes without building any real sandbox mechanics.
7. **OPA bundle-build pipeline stays out of scope.** `pdp-mock` reads the two seeded repos' raw files directly via Forgejo's contents API to compute decisions/`resolved_steps`, instead of a static fixture or the real webhook-triggered bundle builder ADR-9 describes.

## Component Breakdown

Single Go module (monorepo), consistent with ADR-11's Go decision and its `runc`/`containerd` precedent.

| Deployable | Role | ADR-9 interfaces |
|---|---|---|
| `cmd/control-plane` | The real, unprivileged production binary — REST API, task/round state machine, `StepExecutor` registry/dispatch, PDP calls, audit writes | Consumes all six: `GitHostClient`, `PolicyClient`, `SecretsClient`, `AuditWriter`, `ApprovalVerifier`, `ContainerLauncher` |
| `cmd/pdp-mock` | Standalone AuthZEN-shaped HTTP service; decisions computed from the two seeded config repos, incl. `resolved_steps` | Implements `PolicyClient`'s server side |
| `cmd/secrets-mock` | Standalone HTTP service, static pre-provisioned bot-token pool | Implements `SecretsClient`'s server side |
| `cmd/launcher-mock` | Standalone gRPC/mTLS service — fixture-driven, but performs real Forgejo calls | Implements `ContainerLauncher`'s server side; future slot for the real ADR-10 jailer/Firecracker launcher, untouched by this plan |
| `cmd/seed` | One-shot idempotent CLI: bootstraps Forgejo bot accounts, the global-baseline repo, and the sample target repo (with `.harness/policy.yaml` + `.harness/stages.yaml`) | Uses `GitHostClient`'s production impl |

`internal/` layout:
- `internal/domain` — `Task`, `Round`, `StepRun`, `Invocation` models
- `internal/coreloop` — ADR-7's fixed state machine, round cap = 5 hardcoded
- `internal/executor` — `StepExecutor` interface, closed-set registry keyed by `type`, `AgentStageExecutor` + `DeterministicStepExecutor`
- `internal/clients/{gitclient,policyclient,secretsclient,launcherclient}` — one production implementation per ADR-9 interface
- `internal/audit` — Postgres `AuditWriter` impl + hash-chain logic
- `internal/approval` — `ApprovalVerifier` stub
- `internal/config` — Go structs + JSON Schema validation for `.harness/policy.yaml` / `.harness/stages.yaml`
- `internal/fakes` — one in-memory fake per interface, used only in `internal/coreloop`'s unit tests

## Contracts

Each of the following is a versioned artifact, written and reviewed **before** any implementation:

### `ContainerLauncher` — gRPC + mTLS
`api/proto/launcher/v1/launcher.proto`:
```
service ContainerLauncher {
  rpc Launch(LaunchRequest) returns (LaunchResponse);
  rpc Kill(KillRequest) returns (KillResponse);
  rpc ExtractLogs(ExtractLogsRequest) returns (ExtractLogsResponse); // independently callable, decoupled from Launch — mirrors the Firecracker PoC's vsock-independent-of-network finding
}
message LaunchRequest {
  string invocation_id = 1; string task_id = 2;
  string step_type = 3;        // "agent-stage" | "deterministic-step"
  string step_id = 4;
  string payload_ref = 5;      // mock: fixture key; real: image+command ref
  map<string,string> env = 6;  // injected credential, target_repo, branch, prompt/command
  int32 timeout_seconds = 7;
  repeated string granted_scopes = 8;
}
message LaunchResponse { string invocation_id=1; int32 exit_code=2; string status=3; /* completed|timed_out|killed|error */
  google.protobuf.Timestamp started_at=4; google.protobuf.Timestamp finished_at=5; }
message KillRequest { string invocation_id = 1; }
message KillResponse { bool killed = 1; }
message ExtractLogsRequest { string invocation_id = 1; }
message ExtractLogsResponse { bytes stdout=1; bytes stderr=2; bytes structured_result_json=3; }
```
Go interface (`internal/clients/launcherclient`):
```go
type ContainerLauncher interface {
    Launch(ctx context.Context, req LaunchRequest) (RunResult, error)
    Kill(ctx context.Context, invocationID string) error
    ExtractLogs(ctx context.Context, invocationID string) (LogBundle, error)
}
```
mTLS certs: a local dev CA generated at compose bring-up (`deploy/certs/`), mounted read-only into `control-plane` and `launcher-mock`.

### `PolicyClient` — AuthZEN HTTP
`api/openapi/pdp-authzen.yaml` — base AuthZEN Authorization API evaluation endpoint, extended with a documented local extension field `resolved_steps` (array of `{step_id, type, blocking}` — ADR-11's addition, not part of the base AuthZEN spec).
```go
type PolicyClient interface {
    Evaluate(ctx context.Context, req PolicyRequest) (PolicyDecision, error)
}
type PolicyRequest struct {
    InvocationID, TaskID, TriggeringPrincipal, OrchestratorInstanceID, BotAccount, TargetRepo string
    TaskType string   // new-task | pr-review-round | review | security-triage
    RiskTier string
    RequestedScopes []string
    Facts map[string]any // round number, files touched, etc.
}
type PolicyDecision struct {
    Decision string // allow | deny | require_human_approval
    GrantedScopes []string
    Reason string
    PolicyBundleVersion string
    ResolvedSteps []ResolvedStep
}
type ResolvedStep struct { StepID, Type string; Blocking bool }
```

### `SecretsClient` — HTTP (OpenBao-shaped subset)
`api/openapi/secrets.yaml` — `POST /credentials`, `DELETE /credentials/{id}`.
```go
type SecretsClient interface {
    IssueCredential(ctx context.Context, req CredentialRequest) (Credential, error)
    RevokeCredential(ctx context.Context, credentialID string) error
}
type CredentialRequest struct { InvocationID, BotAccount, TargetRepo string; Scopes []string; TTL time.Duration }
type Credential struct { ID, Token string; ExpiresAt time.Time; Scopes []string }
```

### `GitHostClient` — Forgejo REST
Reuse **`code.gitea.io/sdk/gitea`** (official Gitea SDK, Forgejo-compatible by lineage) under a narrow hand-rolled interface, rather than hand-rolling raw HTTP — keeps ADR-9's "one interface, one production implementation" principle without reinventing an HTTP client. Confirm SDK endpoint coverage as an early spike (Phase 0); hand-roll only calls that diverge.
```go
type GitHostClient interface {
    GetFileContents(ctx context.Context, repo RepoRef, path, ref string) ([]byte, error)
    CreateOrUpdatePR(ctx context.Context, repo RepoRef, branch, base, title, body string) (PullRequest, error)
    GetPR(ctx context.Context, repo RepoRef, number int) (PullRequest, error)
    PostPRComment(ctx context.Context, repo RepoRef, number int, body string) (Comment, error)
    ListPRComments(ctx context.Context, repo RepoRef, number int) ([]Comment, error)
    GetBranchProtection(ctx context.Context, repo RepoRef, branch string) (BranchProtection, error)
    VerifyCollaboratorRole(ctx context.Context, repo RepoRef, username string, minRole Role) (bool, error)
    CreateWebhook(ctx context.Context, repo RepoRef, cfg WebhookConfig) (Webhook, error)
}
```

### `AuditWriter` — Postgres DDL
Two schemas, two DB roles (makes ADR-5's "INSERT-only" a Postgres grant, not a convention):
- `orchestrator` schema, role `orchestrator_app` (full CRUD): `tasks`, `rounds`, `step_runs` — live state driving the state machine.
- `audit` schema, role `orchestrator_audit_writer` (`GRANT INSERT` only): `invocation_audit` — one row per `invocation_id` tagged `task_id`, merging ADR-2 (delegation record) + ADR-3 (PDP decision) + ADR-1 (execution outcome) + ADR-9 (approval fields), plus `resolved_steps JSONB` and `prev_hash`/`row_hash` (SHA-256).
```go
type AuditWriter interface {
    WriteInvocationRecord(ctx context.Context, rec InvocationAuditRecord) error
    GetTaskChain(ctx context.Context, taskID string) ([]InvocationAuditRecord, error)
}
```
This plan's answer to ADR-11's open question ("does `resolved_steps` need its own audit field?") — **yes**, `JSONB` on the same row as `decision`/`granted_scopes`.

### `ApprovalVerifier` — stub only
```go
type ApprovalVerifier interface {
    VerifyApproval(ctx context.Context, req ApprovalCheckRequest) (ApprovalResult, error)
}
type ApprovalCheckRequest struct { TargetRepo string; IssueNumber int; WebhookSignature, WebhookPayload []byte }
type ApprovalResult struct { Approved bool; ApprovedBy, Evidence string }
```
v1 impl: `NoopApprovalVerifier`, always `Approved: false` (fail-closed default, matching ADR-3/ADR-9 posture).

### Stage-list / policy JSON Schemas
`schemas/stages.schema.json` — array of entries: `id`, `type` (`agent-stage` | `deterministic-step`), `bot_account`, `task_type`, `blocking` (bool), `conditions` (`risk_tier: []`, `files_touched_patterns: []`), and for `deterministic-step` only: `command`, `working_dir`, `timeout_seconds`, `expected_exit_codes` (default `[0]`) — resolves ADR-11's named open question.
`schemas/policy.schema.json` — narrow-only override shape: `risk_tier_overrides`, `require_human_approval` flags, quota rules.

## Phased Build Sequence

**Phase 0 — Repo scaffolding + compose skeleton (Forgejo + Postgres only).**
Go module init, `Makefile`, `deploy/docker-compose.yml` with real Forgejo + real Postgres, admin bootstrap script. Spike: confirm `code.gitea.io/sdk/gitea` covers needed endpoints against Forgejo.
*Done*: `docker compose up` gives a reachable Forgejo UI/API and Postgres; empty module builds.

**Phase 1 — Contracts as versioned artifacts + generated stubs.**
Write `launcher.proto`, both OpenAPI specs, both JSON Schemas, and the six Go interfaces (doc-commented, no implementations). Generate gRPC stubs. Validate example fixture YAML/JSON against the schemas.
*Done*: everything compiles; schema validator has passing/failing fixture tests.

**Phase 2 — Control-plane skeleton + fakes + unit tests.**
Domain models, `internal/coreloop` state machine, `StepExecutor` registry (two stub executors), in-memory fakes for all six interfaces. Unit tests: PDP-deny short-circuits before launch; round cap fires stop-escalate at round 5 (fake PDP always signals a blocking finding, no real time cost); fan-out runs steps concurrently; aggregate/evaluate transitions.
*Done*: `go test ./...` green against fakes only, no live services needed.

**Phase 3 — Mock services as real compose services.**
Build `pdp-mock`, `secrets-mock`, `launcher-mock` (each with its own Dockerfile + compose entry; mTLS wired for the launcher channel).
*Done*: control-plane's real client implementations (not fakes) pass integration tests against these three mocks plus real Forgejo.

**Phase 4 — Core loop wiring end-to-end.**
Wire all six real client impls into `coreloop`. `AgentStageExecutor` and `DeterministicStepExecutor` both dispatch exclusively through `ContainerLauncher` (ADR-11's unified execution primitive). REST endpoint to create a task.
*Done*: a manually POSTed task produces a real PR + a real review comment in the seeded sample repo, verifiable via Forgejo's own UI/API.

**Phase 5 — Audit trail + Postgres schema + hash chain.**
Migrations for both schemas/roles, `AuditWriter` wired at every decision point (mint, PDP decision incl. `resolved_steps`, launch start/stop, task outcome). Hash-chain verifier + tamper test. Policy-gate reconciliation check (launch count vs. PDP-allow count) as an offline query/test.
*Done*: one correctly chained `invocation_audit` row per invocation under one `task_id`, queryable well within the ADR-5 5-minute target; a tamper test fails as expected.

**Phase 6 — Config repos seeded + bundle resolution + full e2e smoke test.**
`cmd/seed` creates the global-baseline repo and the sample target repo with `.harness/policy.yaml` + `.harness/stages.yaml` (≥1 `agent-stage` review entry, ≥1 `deterministic-step` entry) plus trivial source content. `pdp-mock` reads both repos' raw files live via Forgejo's contents API to compute `resolved_steps`. One scripted end-to-end test drives a full task through the real compose stack.
*Done*: overall Success Criteria (below) pass.

## Testing Strategy

- **Unit** (fast, no live services): `internal/coreloop` against `internal/fakes` — PDP allow/deny/require-approval branches, round-cap-at-5 without real time cost, parallel fan-out, aggregation blocking-vs-advisory logic, registry dispatch-by-`type`, hash-chain math, JSON Schema fixture validation.
- **Integration** (`//go:build integration`, run against compose): each real client implementation against its real mocked counterpart or real infra — `PolicyClient` vs `pdp-mock`, `SecretsClient` vs `secrets-mock`, `ContainerLauncher` (gRPC/mTLS) vs `launcher-mock`, `GitHostClient` vs real seeded Forgejo, `AuditWriter` vs real Postgres — including a literal Postgres-role privilege test: attempt an `UPDATE` as the audit-writer role and confirm Postgres itself rejects it.
- **One true e2e smoke test**: full stack up; seed script runs; POST a task against the sample repo; poll to terminal state; assert (1) a new PR exists with a commit trailer containing `invocation_id`, (2) the PR has ≥1 review comment from the `agent-stage` review executor, (3) the deterministic-step executor's structured result is present in its `step_run`, (4) `invocation_audit` has ≥3 rows under one `task_id` with a valid hash chain, (5) task status is `stop-success`. A second, narrower variant configures `pdp-mock` to always report a blocking finding, forcing all 5 rounds and asserting `stop-escalate` — cheap because `launcher-mock`'s fixtures return in milliseconds.

## Architect Perspective / Decision Tree

- Full e2e smoke test passes reproducibly → ADR-11 considered validated at the control-plane level; ready to start the real ADR-10 launcher implementation next (swap `launcher-mock` for the real jailer/Firecracker launcher behind the same gRPC contract).
- Contract mismatch surfaces during Phase 3/4 (e.g. AuthZEN extension field doesn't fit cleanly, gRPC message shape needs rework) → revise the relevant spec file, not ADR-11 itself — contracts are implementation detail ADR-11 deliberately left open.
- Systemic core-loop failure (e.g. round cap doesn't actually stop, fan-out isn't parallel, aggregation drops findings) → escalate back to ADR-7/ADR-11 for reopening — this would mean the ADR's decision doesn't hold up under a real implementation attempt.

## Success Criteria

- The Phase 6 e2e smoke test passes reproducibly.
- Unit + integration suites are green.
- The audit hash chain is verifiable and a tamper test fails as expected.
- Every contract (proto, 2x OpenAPI, 2x JSON Schema) exists as a file in `api/`/`schemas/`, reviewable independently of its implementation, before that implementation was written.

## Out of Scope

Real `ContainerLauncher`/jailer/Firecracker implementation (ADR-10 — untouched, mock stays in its slot pending a future plan); real OPA + webhook-triggered bundle-build pipeline; real per-invocation secrets minting; human-approval flow (Forgejo issue/label/HMAC verification); UI-serving; Prometheus/Alertmanager wiring; horizontal control-plane scaling; rootfs/copy-on-write layering (inherited ADR-9/ADR-10 debt, irrelevant while the launcher is mocked).

## Named Risks / Follow-Ups

- **Round-cap testing is only cheap while the launcher is mocked.** `launcher-mock` fixtures return in milliseconds; once the real launcher is swapped in (~79-86s wall clock per invocation, per the Firecracker PoC), an equivalent 5-round integration run will take several minutes minimum — not a blocker now, but a future test-runtime consideration.
- **`code.gitea.io/sdk/gitea` Forgejo-compatibility is assumed, not yet verified** — the Phase 0 spike closes this; hand-roll only the specific calls that diverge if a gap surfaces.
- **ADR-9 is still status Proposed**, not Accepted, despite ADR-11 (Accepted) building on and extending it. Not a blocker for this plan, but worth resolving separately from this work.

## Results

*(Appended here as dated run entries once phases are executed — see the OpenHands/Firecracker PoC plans for the convention this repo uses.)*
