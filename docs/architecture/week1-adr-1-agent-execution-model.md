# ADR-1: Agent Execution Model — Orchestrator + Ephemeral One-Shot Containers

| | |
|---|---|
| **Status** | Proposed |
| **Date** | 2026-08-24 |
| **Deciders** | Me |
| **Consulted** | Security/InfoSec, Developer Experience |
| **Informed** | Engineering leadership, platform team |
| **Supersedes / Related** | Builds on [ADR-0](week1-adr-0-and-capability-map.md); constrains ADR-2 (identity), ADR-3 (policy enforcement), and ADR-4 (sandboxing); assumes git-hosting-platform capabilities bound later in ADR-6 |

## Context

ADR-0 fixed the problem statement and scope but deliberately left the harness's internal design to ADR-1 onward. Before writing the identity model (ADR-2) or the policy enforcement layer (ADR-3), we need to fix *how an agent actually runs* — because the shape of "identity" and "where a policy gate can intercept an action" both fall out of the execution model, not the other way around.

Two proposals were on the table:

- **(a) Ephemeral container:** an agent runs inside a Docker container that is started with a task prompt, acts as a client of the target git hosting platform (clone or create a repo, make changes, open a PR), and shuts down when done.
- **(b) Persistent direct client:** an agent runs as a longer-lived process on a dedicated area, directly authenticated against the git hosting platform, picking up and pushing work over time — not necessarily containerized.

Which concrete git hosting platform is used is a separate, later decision (deferred to ADR-6, once requirements from every ADR that touches it are known). This ADR only assumes the platform provides: repository hosting with clone/push access, a pull/merge-request workflow reachable via API, and an authentication/token mechanism the orchestrator can mint scoped credentials against (see ADR-2).

A review of current practice across comparable platforms (GitHub Copilot coding agent, Devin, OpenHands, and internal engineering write-ups from teams like Ramp) shows ephemeral, per-task sandboxing is the dominant pattern, not the exception — see References. Non-human-identity guidance from the same period converges on the same shape: agents should hold short-lived, task-scoped credentials minted by something external to the agent, not durable secrets baked into a long-running process.

## Decision Drivers

Carried forward from ADR-0, the drivers most load-bearing for this specific decision:

- **Containment & recoverability** — the harness needs a kill switch that is fast and doesn't depend on the agent's own process cooperating.
- **NHI credential lifecycle** — short-lived, session-scoped, revocable credentials rather than durable standing access.
- **Auditability** — "which agent, whose delegated authority, what action" must be answerable per unit of work, not blurred across an open-ended session.
- **Cost containment** — needs a natural unit of metering.
- **Tool / MCP supply-chain risk** — wherever the agent executes, that boundary is also where third-party tool/MCP output must be treated as untrusted input; the execution boundary and the trust boundary should be the same thing.

## Options Considered

**1. Persistent direct-client agent (proposal b).** Rejected. The agent's git-hosting-platform credential has no natural expiry tied to a unit of work, so it drifts toward durable standing access — the anti-pattern ADR-0 explicitly warns against (NHI should follow short-lived/delegated patterns, not human-SSO patterns). Kill switch means locating and terminating an arbitrary long-running process rather than simply not relaunching a container. Audit trail blurs across however many tasks the process handles before restart.

**2. Ephemeral, one-shot container per invocation (proposal a, refined) — chosen.** Every unit of agent work — a new task, or a subsequent round on an already-open PR — gets its own freshly started container with a freshly minted, narrowly scoped credential, and the container is destroyed when the work is done or times out.

**3. Resumable / pause-and-resume workspace.** A workspace persists across review rounds for the same task (faster iteration, no repeated clone/install cost) but can be paused and later resumed. Considered and deferred, not rejected outright — see Consequences. The credential-lifetime and kill-switch problems it reintroduces are close to option 1's, and it requires pause/resume/GC infrastructure the orchestrator doesn't need to build for a v1.

## Decision

Adopt **option 2**, structured as two distinct roles:

- **Orchestrator** (runs outside any agent sandbox, is not itself an agent): decides which repo/task/branch a unit of work targets, mints a short-lived credential scoped to that one repo and bounded to the container's max lifetime, starts the container with the task prompt and credential injected via environment/secret manager (never baked into the container image), enforces a timeout, tears the container down, and emits the audit record for the invocation (start, credential issuance, stop, outcome). The orchestrator, not the agent, is the source of truth for "this happened" — the agent's own output is not trusted as an audit record, consistent with treating agent behavior as potentially prompt-injected.
- **Agent container** (single-use): runs a headless coding-agent CLI, given the task prompt at start. Clones the target repo (or creates it, if it doesn't exist yet) using the injected credential, does the work, opens or updates a pull request, and exits. The container is destroyed on exit or on timeout — this destruction *is* the kill switch, and it requires no cooperation from the agent process.

**Invocation granularity is one-shot per round, not per task.** A brand-new task and every subsequent round of work on that same task (e.g., addressing a human's PR review comment) each get their own complete container lifecycle: a fresh credential, a fresh clone of the current PR branch at its current HEAD, and the round's instruction as the prompt. Nothing is preserved via in-process/session state between rounds; any continuity the agent needs from a prior round is passed forward explicitly as prompt context (e.g., a short summary, or the PR's existing comment thread), not relied on implicitly.

Build the orchestrator as bespoke infrastructure rather than reusing whatever CI runner might ship with the eventual git hosting platform (e.g., a Gitea/Forgejo-style Actions runner), given there is not yet a clear picture of what orchestration logic (policy gate calls, credential minting, audit emission) will need to do — bespoke keeps that flexible for now. This can be revisited once ADR-2/ADR-3 make the orchestrator's required responsibilities concrete.

## Consequences

**Positive**
- Every invocation is a clean, self-contained audit unit: one container = one credential = one round = one log bundle = one PR (or PR update).
- Kill switch is precise and requires no agent cooperation: don't relaunch, and any live container can simply be destroyed.
- Credentials narrow naturally to exactly what one round of work needs, with a TTL bounded to container lifetime — no standing access to reason about.
- No state-drift risk: every round starts from a clean checkout of current HEAD, never a workspace that might have silently diverged from what's actually on the git hosting platform.
- Cost has a natural metering unit (container runtime × invocation count), which maps directly onto ADR-0's "mean time to provision an agent-scoped ephemeral environment" success criterion.

**Negative / trade-offs**
- Cold-start cost (clone, dependency install, any build/index step) repeats on every round, not just once per task — this could matter for large repos or heavy toolchains and should be watched once real usage exists.
- No implicit memory across rounds; anything the agent should "remember" from a previous round must be engineered as explicit input, not assumed.
- More container provisioning events over a task's full lifetime (one per round rather than one per task), which raises the bar on provisioning speed/reliability since it's now on the critical path more often.
- Resumable workspaces (option 3) remain a plausible future optimization if cold-start latency becomes a measured problem rather than a hypothetical one — this decision doesn't foreclose it, it just defers it.

## Open Questions

- Sandbox isolation mechanism (plain Docker vs. gVisor vs. Firecracker microVM) — deferred to ADR-4; likely needs to vary by the risk tier of the task (per ADR-0's action-category table), not be a single fixed choice.
- Network egress allowlist mechanics for the container (the git hosting platform + required package registries only) — deferred to ADR-4.
- Maximum per-invocation timeout — TBD, likely needs real usage data.
- Image build/maintenance process for the headless agent container — implementation detail, out of scope for this ADR.
- Which concrete git hosting platform to run — deferred to ADR-6, once the requirements this ADR and ADR-2 assume are joined by whatever ADR-3/ADR-4/ADR-5 add.
- Which concrete coding-agent CLI/framework runs inside the container (opencode, OpenHands, or another) — deferred to ADR-7. That decision is entangled with this one (some frameworks bundle their own session/workspace model), so ADR-7 must be evaluated against the one-shot execution model fixed here, not the other way around.

## References

- [ADR-0: AI Coding Agent Harness — Problem Statement & Scope](week1-adr-0-and-capability-map.md)
- Modal, "Best Sandboxes for SWE-Bench-Style Coding Agents in 2026" — https://modal.com/resources/best-sandboxes-swe-bench-coding-agents
- OpenHands SDK docs, "Workspace" — https://docs.openhands.dev/sdk/arch/workspace
- OpenHands Software Agent SDK (arXiv) — https://arxiv.org/html/2511.03690v2
- Cloud Security Alliance, "The Non-Human Identity Governance Vacuum" — https://labs.cloudsecurityalliance.org/research/csa-whitepaper-nonhuman-identity-agentic-ai-governance-v1-cs/
- GitGuardian, "AI Agents Authentication: How Autonomous Systems Prove Identity" — https://blog.gitguardian.com/ai-agents-authentication-how-autonomous-systems-prove-identity/
- Descope, "AI Agent Credential Management Best Practices" — https://www.descope.com/blog/post/ai-agent-credential-management
