# Plan: OpenHands LocalRuntime Validation (ADR-8 PoC)

| | |
|---|---|
| **Status** | Draft |
| **Date** | 2026-08-27 |
| **Owner** | Krzysztof Niemiec |
| **Purpose** | Validate, hands-on rather than from documentation alone, whether OpenHands' `LocalRuntime` actually holds up as the sandboxing boundary [ADR-8](../architecture/week1-adr-8-coding-agent-runtime-selection.md) relies on — before that ADR's status moves past Proposed. |

## Goal

Answer, with evidence: does `LocalRuntime` run OpenHands' full coding-role loop (clone → edit → test → push → open-or-update PR) against Forgejo without ever requiring elevated container privileges, and does it tear down cleanly? If not, does the documented fallback (OpenHands' default Docker Runtime, nested inside the same sandbox) work instead?

This plan is split into two perspectives on purpose: the **developer** section is what to actually run; the **architect** section is how to read the results and what each outcome means for ADR-8.

## Prerequisites

- Local Forgejo instance up and reachable (per [ADR-6](../architecture/week1-adr-6-git-hosting-platform-selection.md) / `local-gitea-cicd-base.md`).
- Two Forgejo bot accounts + scoped PATs, per [ADR-2](../architecture/week1-adr-2-agent-identity-and-permission-model.md)'s pattern: one repo-scoped with write access (simulating the coding role), one repo-scoped read+comment-only (simulating review/security-triage).
- A disposable test repo on the local Forgejo instance — not a real project — with a trivial test/build command wired in (so Step 3 below has something real to run, not just an edit).
- The sandbox tooling [ADR-4](../architecture/week1-adr-4-blast-radius-and-sandboxing.md) names as the macOS dev-loop stand-in for Firecracker: Apple's Containerization framework, or libkrun via Podman Desktop. Use this, not a plain Docker container — a plain container doesn't test the boundary that actually matters here.

## Developer Perspective — PoC Execution Steps

### 1. Stand up the sandbox boundary
- Launch a fresh instance via the chosen ADR-4 stand-in, with no elevated privileges and no mounted Docker socket.
- Confirm it can reach the local Forgejo instance and package registries, and nothing else (approximating ADR-4's egress-allowlist intent even though the real egress proxy isn't built yet).

### 2. Install and configure OpenHands
- Install OpenHands headless inside the sandbox.
- Configure it explicitly for `LocalRuntime` (not the default Docker Runtime).
- Inject the write-scoped Forgejo PAT via environment variable at sandbox start — mirroring how ADR-1's orchestrator would inject it, never baked into an image or config file.

### 3. Core loop
- Run: `openhands --headless -t "<task: make a small, verifiable change to the test repo and open a PR>"`.
- Observe and record: does clone succeed, does the edit happen, does the test/build command actually run, does the branch push, does a PR get opened against Forgejo. Note wall-clock time and any manual intervention needed.

### 4. Round-2 / update-PR behavior (per ADR-7)
- Run a second headless invocation with a follow-up prompt referencing the PR from Step 3 (e.g. "address this feedback: ...") against the same branch.
- Confirm OpenHands updates the existing PR rather than opening a duplicate — this is the specific behavior ADR-7's iteration loop depends on.

### 5. No-privilege-escalation check
- Confirm nothing in Steps 3–4 required `--privileged`, a mounted Docker socket, or any elevated capability inside the sandbox.
- If the sandbox tooling allows it, rerun under an intentionally restrictive profile (non-root, no extra capabilities) and confirm LocalRuntime still works.

### 6. Kill-switch / teardown check
- Forcibly destroy the sandbox mid-run (or immediately after a run).
- Confirm nothing survives outside it — no orphaned OpenHands process, temp file, or credential remnant left on the host requiring manual cleanup.

### 7. Token-scope negative test
- Repeat Step 3 using the read+comment-only PAT instead of the write-scoped one.
- Confirm the push/PR-create step fails cleanly (permission denied) rather than silently succeeding. This is a direct, hands-on check against the token-scope-bypass bug class the ADR-6 CVE research surfaced — verify it yourself rather than trusting the upstream fix secondhand.

### 8. Conditional fallback test — only if any of Steps 3–7 failed under LocalRuntime
- Reconfigure the same sandbox to use OpenHands' default Docker Runtime (requires a container runtime available inside the sandbox itself).
- Repeat Steps 3–7 against this configuration.
- Record any additional startup latency versus LocalRuntime.

### Deliverable
A short results log — pass/fail per step, timings, and any error output — appended to the **Results** section of this document (see below). This is what the architect-perspective evaluation runs against.

## Architect Perspective — Validation & Decision Criteria

### What each step is actually checking

| Step | Validates |
|---|---|
| 3 | OpenHands' native Forgejo integration works end-to-end under LocalRuntime — the core claim ADR-8's decision rests on. |
| 4 | ADR-7's round-based loop is actually supported (update, not duplicate). |
| 5 | The specific reason LocalRuntime was preferred — no privilege escalation needed inside ADR-4's sandbox. |
| 6 | ADR-1's "container destruction is the kill switch" still holds when OpenHands is the process running inside it. |
| 7 | ADR-2/ADR-6's assumption that token-scope enforcement is trustworthy — checked directly, not assumed from Forgejo's changelog. |
| 8 | Whether the documented fallback (Docker Runtime nested in the sandbox) is actually usable if LocalRuntime isn't, and what it costs. |

### Decision tree

- **Steps 3–7 all pass under LocalRuntime** → LocalRuntime is confirmed. Update ADR-8: replace the "needs validation in practice" open question with a note that it's validated, and move ADR-8's Status from Proposed to Accepted. (ADR-8 is still Proposed at time of writing, so this is a direct edit, consistent with the project's convention of editing freely pre-acceptance and only through new amending ADRs afterward.)
- **LocalRuntime fails on 3–7, but Step 8 (Docker Runtime nested) passes** → revise ADR-8's Decision section to adopt Docker Runtime nested inside the sandbox as primary instead, documenting the added latency/operational weight as an accepted trade-off. Still a direct edit — ADR-8 hasn't been accepted yet.
- **Both fail** → this is bigger than a runtime-mode swap. Reopen ADR-8's Options Considered and seriously reconsider OpenCode or Goose for the coding role too, accepting the previously-identified cost (no native Forgejo PR-mutation, would need `gitea-mcp` or bash+`tea` CLI). Treat as substantially revisiting the decision, not patching it.
- **Step 7 specifically fails (a token-scope bypass is reproduced)** → this is not an OpenHands or runtime problem — it's a Forgejo finding. Escalate back to ADR-6: check whether the locally-running Forgejo version has the relevant CVE patched, and patch/upgrade before proceeding, rather than treating it as an ADR-8 blocker. Since ADR-6 is already Accepted, any resulting change to it must go through a new amending ADR, not a direct edit.

### Non-functional checks worth judgment, not just pass/fail

- Latency: if Step 8 ran, does LocalRuntime's advantage over nested Docker Runtime actually matter, given ADR-1's cold-start cost (clone, dependency install) already dominates wall-clock time per invocation?
- Resource footprint: does LocalRuntime meaningfully preserve the sandbox's minimal-footprint intent, or is the difference negligible in practice?
- Anything observed that changes ADR-7's assumptions (e.g., PR update-vs-create behavior working differently than expected) should be flagged back to ADR-7, not silently absorbed here.

### Documentation follow-through

1. Append results to this plan's **Results** section below — don't rewrite the steps above, add to them.
2. Update ADR-8 per whichever branch of the decision tree applies.
3. If a genuine Forgejo/ADR-6 finding surfaces (Step 7), open a new ADR amending ADR-6 rather than editing it directly.

## Results

*(Empty until the PoC is run — append findings here, dated, one entry per run.)*

## Success Criteria

- Clear pass/fail recorded for Steps 3–7 (and 8, if triggered).
- A documented decision — LocalRuntime confirmed, Docker Runtime substituted, or framework reconsidered — that ADR-8 has been updated to reflect.
- No "needs validation in practice" language left unresolved in ADR-8 once this plan has been executed once.

## Out of Scope

- Production Firecracker/gVisor validation — this PoC runs against the macOS dev-loop stand-in ADR-4 itself carves out as a substitute; validating against real Firecracker on the Linux production target is a separate, later exercise.
- Building ADR-1's actual orchestrator — this plan hand-drives the steps a real orchestrator would automate.
- Load/scale or concurrent-invocation testing — single-invocation validation only.
- The deterministic-scanner pipeline (Semgrep/CodeQL/etc.) for the security-triage role — separate piece of ADR-7's design, not needed to answer this plan's question.
