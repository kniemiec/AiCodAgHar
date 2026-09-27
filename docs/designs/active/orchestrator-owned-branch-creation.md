# Orchestrator-Owned Branch Creation — Development Plan

| | |
|---|---|
| **Status** | Draft |
| **Date** | 2026-09-24 |
| **Owner** | kniemiec |
| **Purpose** | Close the gap where `GitHostClient.CreateOrUpdatePR` assumes the task's branch already exists in Forgejo, by having `handleCreateTask` create (or reuse) it itself, before opening the PR — keeping both git-structural side effects on the trusted control-plane side of the sandbox boundary. |

## Goal

Extend `cmd/control-plane/handler.go`'s `handleCreateTask` to create the task's branch (off `base_branch`) if it doesn't already exist, immediately before its existing `CreateOrUpdatePR` call — so a caller can POST a task whose `branch` has never been pushed, and the control-plane provisions it, rather than requiring some other process to have created it first.

## Context — Why This Shape

`gitclient.Client.CreateOrUpdatePR`'s own doc comment (`internal/clients/gitclient/client.go:47-50`) is explicit: "It does not push any commits itself — the branch is expected to already carry the changes." Today nothing in this codebase ever creates that branch — `handler.go:93` requires `branch` as a caller-supplied, already-existing string. `domain.Task`'s doc comment (`internal/control-plane/domain/domain.go:11-14`) already states the intended shape for the PR half of this: "the control-plane resolves (or creates) the review PR once, before the task starts" — this plan extends that same eager, once-per-task, control-plane-side provisioning to the branch.

The alternative considered was having the coding agent create the branch (and PR) itself on its first `generate` step, on the theory that the orchestrator shouldn't need to know anything about git. Rejected: this repo runs coding-agent work inside a jailer-wrapped Firecracker sandbox specifically because agent-produced execution isn't fully trusted (ADR-4, blast-radius-and-sandboxing). Handing that sandboxed agent a Forgejo credential capable of creating branches/PRs with agent-chosen names and base-branch targets meaningfully expands its capability beyond "push commits to a branch the control-plane already named," for no real gain in generality — a non-git task type already just doesn't call `GitHostClient` today. Keeping branch + PR creation together in `handleCreateTask` also avoids introducing a new dynamic-state-propagation problem: `Branch`/`PRNumber` stay static `domain.Task` fields known before `coreloop.RunRound` is ever called, so no mid-round output has to flow from `generate` into the same round's `review`/`approve` steps, and there's no crash/resume window where a retried round could create a duplicate PR.

## Design

### 1. New `GitHostClient` method

`internal/clients/gitclient/gitclient.go` — add, alongside `CreateOrUpdatePR` and following `CreateOrReuseIssue`'s existing idempotent-by-name convention:

```go
// Branch is the subset of a Forgejo branch the orchestrator needs.
type Branch struct {
    Name string
    SHA  string
}

// CreateOrReuseBranch creates a branch off base if it doesn't already exist,
// or returns the existing branch as-is (design mirrors CreateOrReuseIssue) —
// idempotent against a crash between creating the branch and starting the
// task, which would otherwise error on retry instead of proceeding.
CreateOrReuseBranch(ctx context.Context, repo RepoRef, branch, base string) (Branch, error)
```

### 2. Production implementation

`internal/clients/gitclient/client.go` — implement using the already-available SDK surface (confirmed via `go doc`: `code.gitea.io/sdk/gitea.Client.GetRepoBranch`, `.CreateBranch`, `CreateBranchOption{BranchName, OldBranchName}`):

```go
func (c *Client) CreateOrReuseBranch(ctx context.Context, repo RepoRef, branch, base string) (Branch, error) {
    if err := ctx.Err(); err != nil {
        return Branch{}, err
    }
    if existing, _, err := c.sdk.GetRepoBranch(repo.Owner, repo.Name, branch); err == nil {
        return Branch{Name: existing.Name, SHA: existing.Commit.ID}, nil
    }
    created, _, err := c.sdk.CreateBranch(repo.Owner, repo.Name, gitea.CreateBranchOption{
        BranchName:    branch,
        OldBranchName: base,
    })
    if err != nil {
        return Branch{}, fmt.Errorf("gitclient: create branch %s from %s: %w", branch, base, err)
    }
    return Branch{Name: created.Name, SHA: created.Commit.ID}, nil
}
```
(Exact `GetRepoBranch` not-found error shape to confirm against the real SDK/Forgejo in Phase 2's integration test — some Gitea SDK calls return a typed 404 vs. a generic error, which decides whether the existence check should inspect `*gitea.Response.StatusCode` instead of just `err == nil`.)

### 3. Fake for unit tests

`internal/control-plane/fakes/fake_gitclient.go` — add a `branches map[string]gitclient.Branch` (`"owner/name/branch-name"` keyed) plus `SetBranch` preload helper and a `CreateOrReuseBranch` that mirrors `CreateOrReuseIssue`'s reuse-if-present-else-create shape.

### 4. Wire into `handleCreateTask`

`cmd/control-plane/handler.go` — insert a `CreateOrReuseBranch` call immediately before the existing `CreateOrUpdatePR` call at line 118, using the same `repo`/`req.Branch`/`req.BaseBranch` already in scope, and the same error-handling shape (502 `http.Error` on failure) already used for the PR call:

```go
if _, err := s.git.CreateOrReuseBranch(ctx, repo, req.Branch, req.BaseBranch); err != nil {
    http.Error(w, "create task branch: "+err.Error(), http.StatusBadGateway)
    return
}
pr, err := s.git.CreateOrUpdatePR(ctx, repo, req.Branch, req.BaseBranch, req.PRTitle, req.PRBody)
...
```

### 5. No changes elsewhere

`coreloop`, `taskrunner`, `domain.Task`, and every prompt template are untouched — `Task.Branch`/`Task.PRNumber` remain static fields resolved once before `runner.Start`, exactly as today.

## Testing Strategy

- **Unit** (`internal/control-plane/fakes`, `cmd/control-plane/handler_test.go`): `CreateOrReuseBranch` called twice with the same name returns the same branch without erroring (idempotency); `handleCreateTask` with a not-yet-existing branch succeeds and results in a task started with that branch name.
- **Integration** (`internal/clients/gitclient/client_integration_test.go`, `//go:build integration`, against real seeded Forgejo): `CreateOrReuseBranch` against a fresh branch name creates a real branch off `base` with zero diverging commits; immediately following it with the existing `CreateOrUpdatePR` call successfully opens a real PR against that zero-diff branch. This is the one open assumption from the design discussion — that Forgejo (Gitea-derived) permits opening a PR with no commits yet — and this test is what confirms or refutes it before the plan is considered done. If it's refuted, fall back to pushing a single empty commit (`git commit --allow-empty`) as part of `CreateOrReuseBranch` before returning.
- Re-run `CreateOrReuseBranch` against an already-existing branch (e.g. a second `handleCreateTask` retry with the same `task_id`/`branch`) and confirm no duplicate branch / no error.

## Success Criteria

- `POST /tasks` with a `branch` that does not yet exist in the target repo succeeds end-to-end: branch created off `base_branch`, PR opened against it, task started — where today it would fail once the coding agent's first push targets a branch nothing ever created.
- `POST /tasks` with a `branch` that already exists behaves exactly as before (no duplicate branch, no behavior change).
- `coreloop`/`taskrunner`/prompt templates remain untouched — this is purely a `handleCreateTask` + `GitHostClient` change.

## Out of Scope

- Branch-naming/collision policy (what happens if the requested name collides with an unrelated pre-existing branch) — callers are assumed to already supply task-scoped, unique branch names, as today.
- Any path for task types that don't need git at all — every task currently requires `branch` (`handler.go:93`); this plan doesn't add a way to opt out of that.

## Named Risks / Follow-Ups

- **Zero-diff PR creation on real Forgejo is unconfirmed.** The SDK supports creating a branch with no new commits; whether Forgejo's PR-creation endpoint accepts a head branch with zero diff against base is a server behavior this plan's integration test must verify, with an empty-commit fallback ready if not.
- **`GetRepoBranch`'s not-found signaling needs confirming** against the real SDK (typed error vs. HTTP status) before the "does it already exist" check in step 2 can be trusted to distinguish "doesn't exist yet" from "transient API error" — a bug here would silently attempt (and fail) branch creation on every real network hiccup.

## Results

### 2026-09-25 — Implemented (unit-verified; integration test written but not run)

- `GitHostClient.CreateOrReuseBranch` added (`internal/clients/gitclient/gitclient.go`), implemented in `client.go`, backed by a fake in `internal/control-plane/fakes/fake_gitclient.go`, and wired into `handleCreateTask` immediately before `CreateOrUpdatePR` (`cmd/control-plane/handler.go`), exactly per the design.
- Deviated from the design's `err == nil` sketch for `GetRepoBranch`'s not-found check: used the codebase's existing convention instead (`resp.StatusCode != 404`, as already used for `GetBranchProtection`'s absent case in `spike_test.go:83`) — a bare `err == nil` check would misread a transient network failure as "branch doesn't exist" and silently attempt (and fail) creation, which is exactly the risk §Named Risks flagged.
- Unit tests added/passing: `cmd/control-plane/handler_test.go` (`TestHandleCreateTask_CreatesBranchWhenMissing`, `TestHandleCreateTask_ReusesExistingBranchOnRetry` — the latter uses two distinct `task_id`s against the same branch, since the task store independently rejects a literally-duplicate `task_id` regardless of branch state). Full existing suite still passes; two failures in `internal/launcher/{mmds,vsock}` are pre-existing on this branch (macOS AF_VSOCK/socket-path platform limitation, confirmed unrelated via `git stash`), not caused by this change.
- Integration test extended (`internal/clients/gitclient/client_integration_test.go`, `//go:build integration`) to cover: fresh-branch creation + immediately opening a zero-diff PR against it, and idempotent re-`CreateOrReuseBranch` against an already-existing branch. **Not run** — Docker daemon wasn't running locally, so the seeded Forgejo this test needs (`deploy/docker-compose.yml`) wasn't available. Both Named Risks (zero-diff PR acceptance, `GetRepoBranch` 404 shape) remain unconfirmed against real Forgejo until someone runs `docker compose up` in `deploy/` and then `go test -tags integration ./internal/clients/gitclient/...` with `FORGEJO_URL`/`FORGEJO_ADMIN_TOKEN`/`FORGEJO_ADMIN_USERNAME` set.

### 2026-09-26 — Both Named Risks confirmed against real Forgejo

- `make test-integration`-equivalent run (`go test -tags integration ./internal/clients/gitclient/... -run TestClient_GitHostClientSurface -v` with `deploy/.env` sourced) against the real docker-composed Forgejo: all 10 subtests pass, including both new ones.
- **Zero-diff PR creation works as-is** — Forgejo accepts `CreatePullRequest` with a head branch carrying zero commits ahead of base. The empty-commit fallback this plan reserved is not needed; no follow-up required.
- **`GetRepoBranch`'s 404 signaling behaves as assumed** — the `resp.StatusCode != 404` check correctly distinguishes "branch doesn't exist yet" (proceeds to `CreateBranch`) from other failures, and the idempotent-reuse path (`CreateOrReuseBranch` called twice against the same branch) returns the same SHA both times with no error.
- Both items in §Named Risks / Follow-Ups are now resolved; this plan is complete pending PR/merge.

### 2026-09-26 — Call site relocated: `cmd/control-plane/handler.go` → `internal/control-plane/coreloop/coreloop.go`

Between the previous two entries, `main` merged an unrelated, larger design ("Task Intent Propagation") that removed `handleCreateTask`'s `CreateOrUpdatePR` call entirely: a task no longer arrives with a branch or PR already open, and PR creation moved into `coreloop.RunRound`, firing once the generate phase completes without a blocking failure (`coreloop.go`, `PRCreator.CreateOrUpdatePR`). This left this plan's `handler.go` insertion pointing at code that no longer exists.

Rebased this plan's commit onto `main` (`git merge-tree` confirmed the `gitclient`/fakes/integration-test changes merge cleanly with zero conflicts; only `handler.go`/`handler_test.go` conflicted, and those hunks were dropped in favor of `main`'s versions verbatim) and re-homed the actual fix:

- `coreloop.PRCreator` interface (`coreloop.go`) gained `CreateOrReuseBranch` alongside `CreateOrUpdatePR` — any `GitHostClient` already satisfies it structurally, no wiring changes needed in `cmd/control-plane/main.go`.
- `RunRound` now calls `deps.PRCreator.CreateOrReuseBranch` immediately before `deps.PRCreator.CreateOrUpdatePR`, inside the same `phase == PhaseGenerate && task.PRNumber == 0 && !aggregate(phaseRuns)` guard — this is what actually closes this plan's original gap under the new design: without it, a generate phase that completes without pushing any commits would still make `CreateOrUpdatePR` fail against a branch that never came to exist, exactly the failure mode this plan set out to prevent, just moved to a new call site.
- Added `TestRunRound_CreatesBranchBeforeOpeningPR` (`coreloop_test.go`) verifying the branch exists (via a reuse-check against the fake) once `RunRound` returns a non-zero `PRNumber`.
- Full suite (`go test ./...`) and the real-Forgejo integration test both re-verified green after the relocation.
