# ADR-7: Multi-Agent Review & Iteration Workflow

| | |
|---|---|
| **Status** | Proposed |
| **Date** | 2026-08-27 |
| **Deciders** | Me |
| **Consulted** | Security/InfoSec, Developer Experience |
| **Informed** | Engineering leadership, platform team |
| **Supersedes / Related** | Builds on [ADR-0](week1-adr-0-and-capability-map.md) (risk tiers used as stage-selection key), [ADR-1](week1-adr-1-agent-execution-model.md) (every stage below is just another invocation under that model), [ADR-2](week1-adr-2-agent-identity-and-permission-model.md) (bot-account-per-role pattern extended to reviewer roles), [ADR-3](week1-adr-3-policy-enforcement-layer.md) (reused as the gate-criteria decision point), and [ADR-5](week1-adr-5-observability-and-audit-trail.md) (audit trail needs a task-level grouping key this ADR introduces); decided ahead of and informs [ADR-8](week1-adr-8-coding-agent-runtime-selection.md) (coding-agent runtime selection), since the runtime's required capabilities fall out of the roles defined here |

## Context

ADR-1 fixed the mechanics of a *single* invocation (ephemeral container, one credential, one round). It deliberately left open how many rounds a unit of work takes, what triggers a new one, and whether every round is the same kind of work. The initial proposal on the table was a linear pipeline: coding agent opens a PR → a review agent comments → a security-review agent comments → orchestrator (or another agent) decides whether to loop the coding agent back in or hand the PR to a human for final approval.

That skeleton is directionally sound and matches patterns already in production use elsewhere, but three things needed resolving before it could be relied on:

1. **Self-review bias.** If the review/security-review stages run on the same model (even in a fresh prompt) as the coding agent, research on this is concrete: models measurably favor their own output, and reviewing code adjacent to how they just reasoned about writing it tends to reproduce the same blind spot rather than catch it. A documented real-world instance of this exact failure mode already exists at scale — GitHub Copilot PRs are reviewed by Copilot's own reviewer bot roughly 91% of the time, a provider-aligned review loop that risks reinforcing the same provider's blind spots rather than catching them.
2. **Security review needs a different shape than code review.** LLMs are inconsistent at spotting vulnerability classes unassisted. Current practice pairs deterministic scanners (SAST, secret scanning, dependency scanning) with an LLM triage layer on top — the scanner detects, the agent judges reachability/severity/remediation — rather than asking an LLM to find vulnerabilities cold from a diff.
3. **Extensibility was an explicit requirement**, not a nice-to-have: the ability to add a new reviewer role (e.g. architecture review, license-compliance review) or retire one that isn't earning its keep (e.g. drop security-review for docs-only changes), without redeploying orchestrator control-flow code.

This ADR fixes the workflow shape those constraints imply. It is decided before ADR-8 (coding-agent runtime) deliberately: once roles are separated (coding vs. review vs. security-triage), the runtime-selection question changes — most of the runtime capability comparisons under discussion (native PR creation, git-provider integration) only matter for the coding role. Review roles need far less: read access to a diff plus a way to post structured findings.

## Decision Drivers

- **Extensibility at low cost** — the explicit ask: adding or removing a stage should not require touching core orchestrator logic.
- **Reviewer independence** — avoid the self-attribution bias failure mode documented above.
- **Security-finding reliability** — deterministic detection, agentic triage; not agentic detection.
- **Cost containment** (ADR-0) — the iteration loop must be bounded, not open-ended.
- **Reuse over parallel infrastructure** — every new stage should ride on ADR-1's invocation mechanics and ADR-2's identity model rather than needing its own bespoke plumbing.
- **Auditability** (ADR-5) — a task now spans multiple invocations across multiple roles; the audit trail needs to represent that as one traceable unit, not disconnected rows.
- **Avoiding a single point of failure** (ADR-0) — the "is this PR ready" decision should not rest on a single LLM's judgment call.

## Options Considered

**1. Hardcoded linear pipeline** (the original proposal, as literally specified). Rejected as the final design — every stage addition or removal means editing and redeploying orchestrator control flow, directly contrary to the extensibility requirement.

**2. Event-driven plugin/registry** — review stages subscribe to a "PR ready for review" event and self-register; the orchestrator holds no fixed list, just fans out to current subscribers. More dynamically extensible than option 3 (a new reviewer can appear without touching central config) and naturally parallel. Considered, deferred — it's solving a scaling/dynamism problem this pilot doesn't have yet, and adds real complexity (a pub/sub or registry layer, harder-to-reason-about ordering when a stage depends on another's output). Worth revisiting if stage churn in practice turns out to be frequent enough that even config edits become a bottleneck.

**3. Declarative stage list — "pipeline as config" — chosen.** The orchestrator's control flow is fixed and generic; the specific set of stages is data it loads, not code. Detailed below. This is the same shape every CI system already uses (GitHub Actions workflows, Forgejo Actions, Tekton pipelines) — a proven pattern, not a novel one.

## Decision

### Fixed core loop (orchestrator code — genuinely stable, not configurable)

For a given task: **generate → fan out to configured stages → aggregate findings → evaluate against policy gate → continue, stop-success, or stop-escalate.**

- **Generate**: one coding-agent invocation (ADR-1 mechanics unchanged) — clone/create, branch, edit, run tests, open or update the PR.
- **Fan out**: every currently-configured review-type stage applicable to this task runs as its own invocation, **in parallel** where stages don't depend on each other's output (review and security-triage both only need the PR's current state, so they run concurrently, not sequentially as originally proposed).
- **Aggregate**: findings from all stages are collected into one structured set, tagged by stage, severity, and blocking/advisory status.
- **Evaluate**: ADR-3's policy engine (PDP) — not a bespoke decision system — evaluates the aggregated findings against gate criteria (below) and returns continue / stop-success / stop-escalate.
- **Round cap**: a hard maximum of **5 rounds** per task (first-pass number — actor-critic experiments report 3–5 rounds eliminate 90%+ of issues that would otherwise reach human review, with sharply diminishing returns past that point; revisit once real usage data exists). This cap lives in the core loop, not in stage config, because it's a safety valve, not a tuning knob. Hitting it is a **stop-escalate**, not a silent failure or an infinite loop.

### Configurable stage list (data, not code)

Each stage entry specifies: a stage id, the agent role/bot-account it runs under (ADR-2), its `task_type`, whether it's **blocking** or **advisory**, and which conditions (e.g. ADR-0 risk tier, files touched) it applies to. Adding a stage is adding an entry; retiring one is deleting or disabling an entry — no core-loop change either way. Default stage set for v1:

- **Review stage** — code-quality/logic review. Read + comment-only bot account. Must run on a genuinely separate context from the coding agent, and where feasible a **different underlying model** — not just a different prompt persona of the same model in a fresh window. Cross-model is the target; a fully isolated context (no shared reasoning trace) on the same model family is the acceptable floor if cross-model isn't available yet, tracked as an open question below.
- **Security-triage stage** — not "review the diff for vulnerabilities." Deterministic scanners (SAST/secret/dependency scanning) run first as a plain pipeline step against the pushed branch, producing structured findings (e.g. SARIF). A separate triage agent — its own read-only bot account, no write access — consumes those findings to validate reachability, classify severity, and suggest remediation. The agent judges scanner output; it does not replace the scanner.

Both reviewer-type stages inherit ADR-2's principle directly: a dedicated, narrowly-scoped bot account per role, read+comment only, never write/push — if either is compromised or prompt-injected, its blast radius is "a bad comment," never a code change. Their inputs (diff, PR description, prior comments) are treated as untrusted content per ADR-0's existing tool-output principle — a hijacked coding agent, or content it pulled in from an issue/dependency, could craft text specifically to manipulate a reviewer; reviewer prompts are framed accordingly, not as trusted instructions.

### Gate criteria (policy, not ad-hoc LLM judgment)

"Ready for human approval" is a deterministic checklist evaluated by ADR-3's PDP, not a single LLM's holistic call:
- Tests re-verified independently against the pushed branch — not accepted on the coding agent's self-report, consistent with ADR-1's "agent's own output is not trusted as an audit record."
- Zero unresolved blocking-severity findings across all stages.
- Every review comment addressed or explicitly dismissed with a recorded rationale.

LLM output feeds this checklist; it doesn't substitute for it. This keeps the approval gate from becoming the single point of failure ADR-0 already warns against, and keeps it tunable (e.g. changing which severities block) via policy rules rather than code changes.

### Task-level grouping

All invocations belonging to one logical unit of work (coding rounds + every review-stage round they trigger) are tied together under one task id — the PR/branch identity is the natural key, since every round already operates against "the current PR branch at its current HEAD" per ADR-1. This feeds ADR-5's audit trail and cost/quota tracking, which need to answer "how much has this task cost, across how many rounds and roles" as a unit, not as disconnected per-invocation rows.

### Human touchpoints

Two, not one:
- **Success path** — gate criteria pass: notify the human that the PR is ready for approval (the original proposal's step 8, unchanged).
- **Escalation path** — the round cap is hit, or the task falls into an ADR-0 high-risk tier requiring a mid-process approval gate: notify the human with the full finding/round history, rather than looping silently or failing silently.

## Consequences

**Positive**
- Adding or retiring a stage is a config change, not an orchestrator redeploy — the extensibility requirement is met directly.
- Every new stage rides on ADR-1's invocation mechanics and ADR-2's identity model instead of needing new infrastructure — "add a reviewer role" means one bot account, one `task_type`, one config entry.
- Cross-model/isolated-context review and deterministic-first security triage directly target the two documented failure modes (self-attribution bias, unreliable agentic vulnerability discovery) instead of reproducing them.
- The round cap converts an open-ended loop into a bounded one with a concrete, evidence-based default and a defined escalation path.
- Gate criteria living in the policy engine avoids a second bespoke decision system and avoids pure LLM-as-judge as a single point of failure.

**Negative / trade-offs**
- Declarative config is less dynamically flexible than a full event/plugin registry (option 2) — adding a stage still needs a config edit/reload, just not a code change. Accepted as right-sized for now; option 2 is the documented upgrade path if stage churn ever makes even that a bottleneck.
- Enforcing genuine cross-model review adds real cost and operational complexity (a second model provider, or at minimum strict context isolation) — how strictly to enforce this for v1 is an open question below, not fully resolved here.
- Deterministic scanners are a new operational component this harness now depends on and must maintain, beyond what ADR-1 through ADR-6 already required.
- Task-level grouping was a new requirement on ADR-2's delegation record and ADR-5's audit schema that those ADRs didn't originally anticipate — folded back into both directly (`task_id` field) rather than left as a gap.

## Open Questions

- Exact stage-list schema and where it lives (flat config file vs. DB table vs. versioned alongside the orchestrator) — implementation detail, not decided here.
- How strictly to enforce cross-model independence for v1 given cost — leaning toward requiring at minimum a fully isolated context now, with genuine cross-model as a stretch goal, but not fixed.
- Whether 5 is the right round cap, and what blocking-vs-advisory defaults should be per ADR-0 risk tier — first-pass numbers, pending real usage data.
- Which specific deterministic scanners to adopt (e.g. Semgrep, CodeQL, gitleaks, Trivy, or a subset) — tooling choice, deferred.
- When the event-driven plugin pattern (option 2) becomes worth its added complexity — revisit once real stage churn is observed rather than guessed at now.

## References

- [ADR-0: AI Coding Agent Harness — Problem Statement & Scope](week1-adr-0-and-capability-map.md)
- [ADR-1: Agent Execution Model — Orchestrator + Ephemeral One-Shot Containers](week1-adr-1-agent-execution-model.md)
- [ADR-2: Agent Identity & Permission Model](week1-adr-2-agent-identity-and-permission-model.md)
- [ADR-3: Authorization / Policy Enforcement Layer](week1-adr-3-policy-enforcement-layer.md)
- [ADR-5: Observability & Audit Trail](week1-adr-5-observability-and-audit-trail.md)
- Augment Code, "Adversarial Code Review: Why the Maker Shouldn't Grade the Checker" — https://www.augmentcode.com/guides/adversarial-code-review
- "Self-Attribution Bias: When AI Monitors Go Easy on Themselves" (arXiv) — https://arxiv.org/pdf/2603.04582
- Codex Knowledge Base, "Cross-Model Adversarial Review: Using Multiple AI Models to Catch Agent Blind Spots" — https://codex.danielvaughan.com/2026/03/28/cross-model-adversarial-review/
- understandingdata.com, "Actor-Critic Adversarial Coding: Multi-Pass Quality Through AI" — https://understandingdata.com/posts/actor-critic-adversarial-coding/
- AgentPatterns.ai, "Agentic Code Review Patterns and Review Architectures" — https://agentpatterns.ai/code-review/
