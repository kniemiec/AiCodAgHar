# Plan: OpenHands LocalRuntime Validation (ADR-8 PoC)

| | |
|---|---|
| **Status** | Completed |
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

### 2026-08-30 — Run 1 (in progress: Prerequisites + Steps 1–3 done, Steps 4–8 pending)

**Environment set up for this run:**
- Forgejo (`codeberg.org/forgejo/forgejo:16`) via Docker Compose at `~/forgejo/`, SQLite backend, data bind-mounted to `~/forgejo/data` for portability. Reachable at `http://localhost:3000`.
- Bot accounts: `bot-coder` (PAT scopes `write:repository,write:issue`, Write collaborator) and `bot-reviewer` (PAT scopes `read:repository,write:issue`, Read collaborator). Verified directly: `bot-coder` push succeeds, `bot-reviewer` push returns `403 Forbidden`.
- Disposable repo `forgejo-admin/adr8-poc-repo`: trivial stdlib-only Python (`calc.py` / `test_calc.py` / `run_tests.sh`), no dependency install needed.
- Sandbox tooling: Apple's Containerization framework (`container` CLI, Homebrew formula `container` v1.3.1 — not a cask, and not bundled with macOS despite running macOS 26 Tahoe). Apple Silicon (M4) required.
- OpenHands SDK CLI v1.21.0, installed via `uv tool install openhands --python 3.12` inside the sandbox. LLM: `anthropic/claude-sonnet-4-5-20250929`.

**Step 1 — Stand up the sandbox boundary: PASS**
- Launched via `container run`, no `--privileged`, no mounted Docker socket.
- Confirmed a genuinely separate kernel inside the sandbox (`uname -a` → Linux 6.18.35, distinct from the macOS host) — a real VM boundary, not shared-kernel namespacing.
- Reachability confirmed: host-side Forgejo reachable at the vmnet gateway IP (`192.168.64.1:3000`, **not** `localhost` — each sandbox container gets its own IP off a vmnet bridge and reaches the host via the gateway). PyPI and npm registries both reachable (200 OK).
- Known gap, expected and in-scope per this plan: no egress allowlist yet (full open internet reachable, not just Forgejo + registries) — the real egress proxy is ADR-4 future work, not required for this PoC.

**Step 2 — Install and configure OpenHands: PASS, with two real friction points on the Anthropic account side (not OpenHands/sandbox issues)**
- Installed cleanly via `uv tool install openhands --python 3.12` inside the persistent sandbox container.
- Explicitly configured `RUNTIME=process` (current name; docs still alias it as `local`) rather than relying on any implicit default.
- Confirmed no Docker binary or socket exists anywhere in the sandbox (`which docker` → not found, no `/var/run/docker.sock`) — so Docker Runtime was categorically unavailable as a silent fallback; whatever ran, ran via LocalRuntime/process execution.
- Smoke-tested LLM connectivity + basic tool use (`whoami && pwd && echo done` via the agent's terminal tool): executed directly in the sandbox process (`hostname: adr8-poc-sandbox`), no nested container spun up, exit code 0.
- Credential injected via `-e FORGEJO_TOKEN` / `-e LLM_API_KEY` at `container exec` time, sourced from a host-side file never written into the sandbox image or any config file — consistent with ADR-2's injection pattern.
- Two blockers hit and resolved, both on the Anthropic account side, not OpenHands or the sandbox:
  1. `anthropic-workspace-id is required when authenticating with an identity-linked API key` — the API key wasn't scoped to a single workspace. Fixed by regenerating the key scoped to one workspace in the Anthropic console.
  2. `Your credit balance is too low to access the Anthropic API` — fixed by adding billing credits.

**Step 3 — Core loop: PASS**
- Task given (no scripted git/curl commands, just the target repo + change description): clone `adr8-poc-repo`, add `subtract(a, b)` to `calc.py` + a unit test, run `./run_tests.sh`, commit on a new branch, push, open a PR against `main`.
- Wall clock: **93 seconds** (07:32:13Z–07:33:46Z), zero manual intervention during the run itself.
- Observed sequence (from the `--json` event log): `git clone` → read files via `file_editor` → `git checkout -b add-subtract-function` → edits via `file_editor` (`str_replace`) → `./run_tests.sh` (2/2 tests passed) → `git commit` → `git push` (authenticated via `http://bot-coder:${FORGEJO_TOKEN}@...`) → **raw `curl -X POST` against Forgejo's `/api/v1/repos/.../pulls` REST endpoint**, executed through the generic `terminal` tool.
- PR #1 created successfully — independently verified via a separate direct call to Forgejo's API (not just trusting the agent's self-report): `state: open`, `mergeable: true`, `user.login: bot-coder`, `head: add-subtract-function → base: main`.
- **Finding worth flagging back to ADR-8's framing**: PR creation was accomplished via the agent's own generic shell tool use (git + curl against the REST API), not a dedicated Forgejo-provider action type in the event log (only `TerminalAction` / `FileEditorAction` kinds appeared — no distinct "create pull request" tool call). The *outcome* matches what ADR-8 assumed ("native Forgejo integration works end-to-end"), but the underlying *mechanism* is closer to "capable general-purpose shell agent correctly figures out the right API calls" than a purpose-built provider integration. Both get the job done, but they have different failure modes worth distinguishing — e.g. a first-class integration should be more inspectable/consistent than a curl invocation the model constructs fresh each time (headers, JSON body correctness, etc. depend on the model getting it right, not on tested provider-integration code).

**Raw audit trail**: full `--json` event log for the Step 3 run saved at `docs/plans/results/step3-run1-raw.log`.

**Step 4 — Round-2 / update-PR behavior: PASS**
- Cleared the round-1 checkout and started a fresh `openhands` conversation (new invocation, not `--resume`) with a fresh clone — matching ADR-1's "fresh clone of the current PR branch at its current HEAD" per-round model, not a reused workspace.
- Task: address a follow-up ("add `multiply(a, b)` + a test") on the *existing* PR #1 / branch `add-subtract-function`, explicit instruction not to open a new PR.
- Wall clock: **80 seconds** (08:22:34Z–08:23:54Z), zero manual intervention.
- Observed sequence: `git clone` → `git checkout add-subtract-function` (tracked `origin/add-subtract-function`, i.e. it correctly found and checked out the existing branch rather than starting a new one) → edits via `file_editor` → `./run_tests.sh` (3/3 passed, including the two prior tests) → `git commit` → `git push` to the same branch. **No `curl` POST to `/pulls` this round** — unlike round 1, it recognized a PR already existed and simply pushed, letting Forgejo's own head-branch-tracking update the PR automatically, rather than attempting to create a second one.
- Independently verified via Forgejo's API (not just the agent's self-report): still exactly **1 open PR** (`#1`), `changed_files: 2`, and `pulls/1/commits` lists both the round-1 (`subtract`) and round-2 (`multiply`) commits. No duplicate PR created.
- This directly confirms ADR-7's iteration-loop assumption holds under LocalRuntime: a second invocation referencing an existing PR updates it rather than duplicating it — and it did so without needing to be told the update mechanism (push to the same head branch), it inferred that from context.

**Raw audit trail**: `docs/plans/results/step4-round2-raw.log`.

**Step 5 — No-privilege-escalation check: PASS, with one real finding on the way**
- Baseline confirmation via `container inspect adr8-poc-sandbox` (the container Steps 3–4 ran in): `capAdd: []`, `capDrop: []` (framework defaults only, nothing added), `mounts: []` (no Docker socket, no host bind mounts of any kind). Steps 3–4 ran as root but with zero elevation requested or granted beyond the sandbox's own default.
- Also noted: `container run --help` has **no `--privileged` flag at all** — this CLI doesn't expose an escalation knob the way `docker run --privileged` does.
- **Finding**: attempted a maximally restrictive rerun with `--cap-drop ALL` on a fresh container (`adr8-poc-restricted`). This broke `apt-get install` and `useradd` even as root — `apt`'s internal privilege-separation (drops to the `_apt` user for downloads) and `useradd`'s `/etc/shadow` write both depend on specific capabilities (`CAP_SETUID`/`CAP_SETGID`/etc.) that a literal "ALL" removes. This is expected Linux-capability behavior, not an OpenHands or Containerization-framework issue — real-world hardening never actually drops literally everything for a container that still needs a package manager.
- Recreated with a **curated drop** of the capabilities that are actually security-relevant here (`CAP_SYS_ADMIN`, `CAP_SYS_PTRACE`, `CAP_SYS_MODULE`, `CAP_SYS_RAWIO`, `CAP_SYS_BOOT`, `CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_MKNOD`, `CAP_DAC_READ_SEARCH`, `CAP_SYS_TIME`, `CAP_AUDIT_CONTROL`, `CAP_SYSLOG`) — setup (git/uv/OpenHands install, non-root user creation) succeeded cleanly under this profile.
- Ran a full round-3 task (add `divide(a, b)` + test, update PR #1) as **non-root uid 1000**, inside the capability-restricted container, with `LLM_API_KEY`/`FORGEJO_TOKEN` injected the same way as before. Wall clock: **74 seconds** (08:29:03Z–08:30:17Z).
- Sequence and result identical in shape to Steps 3–4: clone → checkout existing branch → edit → `./run_tests.sh` (4/4 passed, including all prior rounds' tests) → commit → push to the same branch. Independently verified via Forgejo's API: still exactly **1 open PR**, now with **3 commits** (`subtract`, `multiply`, `divide`), no duplicate.
- **Conclusion**: LocalRuntime does not require root or any elevated/extra capability for the actual agent execution path (clone/edit/test/push). The only privilege dependency found was in one-time image *setup* tooling (`apt`, `useradd`) needing a sane (not literally empty) capability set — which is a non-issue for a real deployment, since a production agent image would be pre-baked with these tools already installed rather than doing `apt-get install` inside the restricted per-invocation runtime. Worth carrying forward to ADR-1's still-open "image build/maintenance process" question: bake once under normal privileges, run per-invocation under the restricted profile.

**Raw audit trail**: `docs/plans/results/step5-restricted-raw.log`.

**Step 6 — Kill-switch / teardown check: PASS**
- Launched a new task (round 4: add `power(a, b)` + test, update PR #1) in the background, let it clone the repo and start exploring files (confirmed via the live JSON log: `git clone` done, `file_editor view` calls in progress), then forcibly killed it mid-run with `container kill adr8-poc-sandbox` (default signal `KILL`/SIGKILL — this CLI has no `--privileged`-style flag but does have a direct kill primitive).
- **Timing**: kill issued → command returned in **~1 second** (`08:36:10Z` → `08:36:11Z`); `container list` immediately showed zero running containers. Well inside ADR-4's sub-60-second p95 target for per-invocation revocation (first-pass estimate in that ADR — this is one data point, not a load-tested measurement).
- **Confirmed clean interruption**: the event log stops right after a `file_editor view` call — no `git commit` or `git push` was ever reached. Independently verified against Forgejo: PR #1 still has exactly 3 commits (the Step 3–5 ones), no partial 4th commit leaked through.
- **No host-side residue**: `ps aux` on the macOS host shows no orphaned `openhands`/`python`/`git` process — the only related process is `container-apiserver`, the framework's own persistent management daemon (not something spawned per-invocation). This is structurally different from a bind-mounted Docker setup: since the sandbox never had any host directory mounted into it (`mounts: []`, confirmed back in Step 5), there was never a path for it to leave temp files on the host filesystem even in principle — the entire workspace lived inside the now-destroyed VM's own disk.
- **Belt-and-suspenders token revocation** (the other half of ADR-4's kill-switch design — revoke the token independently of killing the container, in case it already leaked out): listed `bot-coder`'s tokens via Forgejo's API, deleted it (`DELETE /users/bot-coder/tokens/1` → `204`, **instant**, `08:36:47Z`), then confirmed the old token now gets **401 Unauthorized** on a call that previously succeeded.
- **Conclusion**: container destruction is a genuine, fast, unilateral kill switch requiring no cooperation from the OpenHands process — confirms ADR-1's "destruction is the kill switch" claim and ADR-4's two-signal (kill + revoke) design both hold in practice, not just on paper.
- Note for later steps: `bot-coder`'s original token is now revoked and the `adr8-poc-sandbox` container is gone (by design, this was the point of the test) — a fresh sandbox + a newly minted `bot-coder` token will be needed if any later step needs write access again, which is itself a faithful mirror of ADR-1's "fresh container + fresh credential per invocation" model rather than a limitation of this test.

**Raw audit trail**: `docs/plans/results/step6-killswitch-raw.log`.

**Step 7 — Token-scope negative test: PASS**
- Fresh sandbox (Step 6 destroyed the previous one — rebuilt from scratch: `container run` → `apt-get install git curl` → `uv tool install openhands`, mirroring the fresh-container-per-invocation model rather than reusing state).
- Repeated Step 3's task verbatim (clone `adr8-poc-repo`, add a `power(a, b)` function + test, commit on a new branch, push, open a PR) but authenticated as **`bot-reviewer`** (`read:repository,write:issue` token scope, **Read** collaborator on the repo) instead of `bot-coder`.
- **First attempt** ran past the 2-minute mark without finishing — the underlying `openhands` process kept running *inside* the sandbox even after my host-side capture was killed by a tool timeout (a real, useful side-finding: killing the local `container exec` client does **not** kill the remote process; the kill switch has to act on the container/VM itself, which is exactly what Step 6 already did — reinforces that Step 6's approach was the correct one, not an alternative). Reset with a fresh container and reran with an explicit instruction not to retry indefinitely on a permissions failure.
- **Second attempt — PASS, wall clock 76 seconds** (`08:42:10Z`–`08:43:26Z`):
  - `git clone` (authenticated as `bot-reviewer`) — **succeeded**, confirming read access works as intended.
  - File edits, `./run_tests.sh` (2/2 passed), `git checkout -b add-power-function`, `git commit` — all **succeeded** (these are local operations, unaffected by remote permissions).
  - `git push` — **failed cleanly**: `exit_code=128`, `remote: Forbidden`, `The requested URL returned error: 403`.
  - No PR-creation `curl` call was attempted afterward — the agent correctly recognized the push failure meant there was nothing to open a PR against.
  - Final message was an honest, accurate failure report: identified the exact error, correctly diagnosed the cause (**"the `bot-reviewer` user does not have write/push permissions"**), and stopped rather than retrying further or claiming success.
- Independently verified via Forgejo's API (not the agent's self-report): the `add-power-function` branch **does not exist** on the remote, and there is still exactly **1 open PR** (`#1`) — confirming nothing leaked through despite `bot-reviewer`'s token carrying `write:issue` scope.
- **This is the direct, hands-on check against the token-scope-bypass CVE class ADR-6's research flagged**: a token with broader *scope* (`write:issue`) did not let the agent bypass the account's actual *collaborator-level* Read restriction. ADR-2's permission-intersection model (token scope ∩ account's actual repo role) held under a real, adversarial-shaped test, not just as a documented assumption.
- Per the plan's decision tree: Step 7 did **not** reproduce a token-scope bypass, so there is no Forgejo/ADR-6 finding to escalate — Forgejo's enforcement behaved correctly here.

**Raw audit trail**: `docs/plans/results/step7-negative-raw.log`.

**Remaining for this plan**: Step 8 is conditional and **not triggered** — Steps 3–7 all passed under LocalRuntime, so per the plan's decision tree this points toward "LocalRuntime confirmed," pending the write-up in ADR-8 itself.

## Success Criteria

- Clear pass/fail recorded for Steps 3–7 (and 8, if triggered).
- A documented decision — LocalRuntime confirmed, Docker Runtime substituted, or framework reconsidered — that ADR-8 has been updated to reflect.
- No "needs validation in practice" language left unresolved in ADR-8 once this plan has been executed once.

## Out of Scope

- Production Firecracker/gVisor validation — this PoC runs against the macOS dev-loop stand-in ADR-4 itself carves out as a substitute; validating against real Firecracker on the Linux production target is a separate, later exercise.
- Building ADR-1's actual orchestrator — this plan hand-drives the steps a real orchestrator would automate.
- Load/scale or concurrent-invocation testing — single-invocation validation only.
- The deterministic-scanner pipeline (Semgrep/CodeQL/etc.) for the security-triage role — separate piece of ADR-7's design, not needed to answer this plan's question.
