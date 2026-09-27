# Pre-Generate Branch Creation — Development Plan

| | |
|---|---|
| **Status** | Draft |
| **Date** | 2026-09-27 |
| **Owner** | kniemiec |
| **Purpose** | Fix the orchestrator-owned-branch-creation call site so the task's branch exists *before* the generate phase dispatches, not after it completes — so the coding agent works on (and pushes to) the task branch instead of falling back to `main`. |

## Goal

Move `CreateOrReuseBranch` in `coreloop.RunRound` from after the generate phase to just before it, and bring the one test that drives a real agent directly (`TestLaunchKVM_AgentStageRealForgejo`) in line with that guarantee. `CreateOrUpdatePR` stays where it is.

## Context — What Went Wrong

`orchestrator-owned-branch-creation.md` set out to have the control-plane provision the task branch "before the task starts." When it was rebased onto Task Intent Propagation, the `CreateOrReuseBranch` call moved into `RunRound` alongside `CreateOrUpdatePR`. That block runs only after the generate phase's fan-out has returned without a blocking failure (`internal/control-plane/coreloop/coreloop.go:228`).

As a result, the branch still doesn't exist while the generate step runs:

- **Agent-stage generate (`implement`)**: the prompt says "You are working on … on branch X … push it to the X branch" (`internal/launcher/prompts/prompts.go:55-63`), but X doesn't exist in Forgejo. OpenHands clones the default branch and commits there. Nothing stops the push, so changes land on `main`, as seen in a real KVM run.
- **Deterministic generate step**: `guest_wrapper.py`'s `run_command_mode` does `git clone --branch $BRANCH`, which fails outright against a branch that doesn't exist.
- **A failed first round**: if round 1's generate fails with a blocking finding, the post-generate block never runs. Round 2's generate then starts against a branch that still doesn't exist.
- **The `CreateOrReuseBranch` call that runs after generate** either reuses a branch the agent happened to create itself or creates an empty branch off `base`. In the second case, the agent's commits sit on `main` and the PR opens with zero diff.

`TestRunRound_CreatesBranchBeforeOpeningPR` didn't catch this. It only checks that the branch exists by the time `RunRound` returns, not that it existed when the generate step was dispatched.

Separately, `TestLaunchKVM_AgentStageRealForgejo` calls `launcherServer.Launch` directly and never goes through `coreloop`. It gets no branch provisioning from either call site. Its doc comment requires `BRANCH` to already exist, but nothing enforces that, and if it doesn't, the run fails in the same way.

## Design

### 1. Move branch creation before the phase loop

`internal/control-plane/coreloop/coreloop.go` — in `RunRound`, after `groupStepsByPhase` and before the `for _, phase := range …` loop:

```go
// Pre-generate branch creation design: the generate step's agent (or
// deterministic command) clones and pushes to task.Branch, so it must
// exist before that step dispatches — not after, or the agent falls back
// to the default branch. Idempotent, so a retried round or an
// approve-phase resume just reuses it.
if len(groups[policyclient.PhaseGenerate]) > 0 && task.PRNumber == 0 {
    if _, err := deps.PRCreator.CreateOrReuseBranch(ctx, approvalRepoRef(task), task.Branch, task.BaseBranch); err != nil {
        return RoundResult{}, fmt.Errorf("round %d: create task branch: %w", round, err)
    }
}
```

The existing post-generate block drops its `CreateOrReuseBranch` call and keeps only `CreateOrUpdatePR` + `SetTaskPRNumber`. Its guard (`phase == PhaseGenerate && task.PRNumber == 0 && !aggregate(phaseRuns)`) is unchanged. Opening the PR only after a successful generate is still Task Intent Propagation's intent.

Guard choices:
- **`len(groups[PhaseGenerate]) > 0`**: a round with no generate step has no one to push, so there's nothing to provision. This also keeps the existing `Dependencies.PRCreator` contract: nil is valid for a config that never resolves a generate step.
- **`task.PRNumber == 0`**: an open PR implies its head branch already exists. Skipping the call keeps the other half of the nil-`PRCreator` contract ("or one whose PRNumber is already set") and avoids an extra Forgejo round-trip every round.
- **Every round, not just round 1**, while `PRNumber == 0`: this covers the "round 1 generate failed, round 2 retries" case. The call is idempotent, so repeating it is safe.

Update the `Dependencies.PRCreator` doc comment (`coreloop.go:80-92`) and the comment above the post-generate block (`coreloop.go:220-227`). Both currently say the branch is created after generate completes.

### 2. Regression test that checks ordering, not just existence

`internal/control-plane/coreloop/coreloop_test.go` — replace `TestRunRound_CreatesBranchBeforeOpeningPR` with `TestRunRound_CreatesBranchBeforeGenerateDispatch`:

- Use a test-local `PRCreator` wrapper around `fakes.FakeGitHostClient`, plus a `FakeContainerLauncher.LaunchFunc`, that append to one shared ordered event log: `"branch"`, `"launch:<stepID>"`, `"pr"`.
- Assert that the order is exactly `branch → launch:implement → pr`.

Add two sibling cases:
- `TestRunRound_BlockingGenerateStillHasBranch`: generate returns a blocking finding. Assert that `"branch"` was logged before the launch and `"pr"` was never logged, so round 2 will find the branch.
- `TestRunRound_SkipsBranchCreationWhenPROpen`: `task.PRNumber != 0` and `PRCreator` is nil. `RunRound` must not panic, and generate still dispatches.

### 3. Make the KVM manual test provision its own branch

`cmd/container-launcher/launcher_kvm_test.go` — before `srv.Launch`, build a `gitclient.NewClient(FORGEJO_URL, FORGEJO_TOKEN)` and call `CreateOrReuseBranch(ctx, repo, BRANCH, BASE_BRANCH)`. `BASE_BRANCH` is optional and defaults to `main`. This makes the test match what `RunRound` now guarantees, instead of depending on hand-prepared Forgejo state. `FORGEJO_URL`'s tap-gateway address (`172.16.0.1`) is the host's own interface, so the host can reach it as well as the guest.

Also:
- Fail fast with a clear message if `BRANCH == BASE_BRANCH`. Running against `main` is exactly the silent failure this plan fixes.
- Update the test's doc comment: `BRANCH` no longer has to exist in advance, and the new optional `BASE_BRANCH` is documented.

### 4. Make the prompt explicit about the branch

`internal/launcher/prompts/prompts.go` — in `implementTmpl` and `implementWithFindingsTmpl`, replace the generic "Clone the repository …" / "push it to the X branch" wording with an explicit instruction: clone, `git checkout {{.Branch}}` (it already exists), commit there, push only to `{{.Branch}}`, and never push to any other branch. If `{{.Branch}}` cannot be checked out, write `{"has_blocking_finding": true}` instead of working on another branch. Add a `prompts_test.go` assertion that the rendered prompt contains the checkout and "only to" wording.

This is defense in depth, not a boundary. It turns a missing branch into a visible blocking result instead of a silent push to `main`. The real boundary is in Follow-Ups below.

## Testing Strategy

- **Unit**: the three `coreloop` tests in §2, the `prompts_test.go` assertion in §4, and the full `go test ./...`.
- **Manual KVM** (`sudo -E … go test -tags launcherkvm ./cmd/container-launcher/... -run TestLaunchKVM_AgentStageRealForgejo -v`) with a fresh, never-pushed `BRANCH`. Confirm in Forgejo that the branch exists off `BASE_BRANCH`, that the agent's commits are on it, and that `main`'s head SHA is unchanged from before the run.

## Success Criteria

- In `RunRound`, `CreateOrReuseBranch` always completes before any generate-phase step is dispatched, whenever `PRNumber == 0`. The ordering test in §2 enforces this.
- A KVM run with a never-before-pushed `BRANCH` leaves `main` untouched and the agent's commits on `BRANCH`.
- A blocking round-1 generate still leaves a branch for round 2.
- PR creation timing is unchanged: it still happens only after a non-blocking generate.

## Out of Scope

- Moving `CreateOrUpdatePR` earlier. A zero-diff PR is possible (confirmed in the previous plan), but opening the PR only after a successful generate is a deliberate Task Intent Propagation choice.
- Branch-name collision policy. This is unchanged from the previous plan.

## Named Risks / Follow-Ups

- **`main` is not protected in Forgejo.** Nothing in `deploy/` seeds branch protection, so the agent's token can push to `main`. Only the prompt stops it. The real fix is a Forgejo branch-protection rule on `base_branch` that denies pushes from the agent's credential. `bootstrap-admin.sh` would need to seed it, and `gitclient` already reads it via `GetBranchProtection`. It needs its own design because the publish phase's merge has to stay allowed. Until then, §4 is the only safeguard.
- **Update the previous plan's record.** Add a Results entry to `orchestrator-owned-branch-creation.md` pointing here, noting that its 2026-09-26 relocation placed the call after generate and that this plan corrects it.

## Results

### 2026-09-27 — Implemented (§1–§4); manual KVM run pending

- **§1** `coreloop.RunRound` now calls `CreateOrReuseBranch` after `groupStepsByPhase` and before the phase loop, guarded by `len(groups[PhaseGenerate]) > 0 && task.PRNumber == 0`. The post-generate block keeps only `CreateOrUpdatePR` + `SetTaskPRNumber`, with its guard unchanged. The `Dependencies.PRCreator` doc comment and the post-generate block comment were updated to match.
- **§2** `TestRunRound_CreatesBranchBeforeOpeningPR` was replaced by `TestRunRound_CreatesBranchBeforeGenerateDispatch` (asserts `branch → launch:implement → pr`), `TestRunRound_BlockingGenerateStillHasBranch` (`branch → launch:implement`, no `pr`), and `TestRunRound_SkipsBranchCreationWhenPROpen` (nil `PRCreator`, `PRNumber: 7`, generate still dispatches). They use a shared `eventLog` fed by a wrapped `FakeGitHostClient` and a `FakeContainerLauncher.LaunchFunc`. Against the old `coreloop.go`, the first two fail with `got [launch:implement branch pr]` and `got [launch:implement]`, so they do catch the original bug.
- **§3** `TestLaunchKVM_AgentStageRealForgejo` now provisions `BRANCH` off `BASE_BRANCH` (default `main`) through `gitclient` before `srv.Launch`, fails fast if `BRANCH == BASE_BRANCH`, and its doc comment is updated. It vets cleanly under `-tags launcherkvm`.
- **§4** The implement-family prompts share `checkoutBlock` and `pushBlock` constants: `git checkout {{.Branch}}`, commit and push only there, and write `{"has_blocking_finding": true}` rather than work on another branch if checkout fails. `TestRender_Implement_PinsTaskBranch` covers both the fresh and prior-findings variants.
- `go test ./...` is green. `go vet` is clean under the `e2e`, `integration`, and `launcherkvm` tags.
- **Not yet run:** the manual KVM verification in §Testing Strategy (a fresh `BRANCH`, then confirm `main`'s head SHA is unchanged). The "Update the previous plan's record" follow-up is done (Results entry added to `orchestrator-owned-branch-creation.md`). Forgejo branch protection is still open.
