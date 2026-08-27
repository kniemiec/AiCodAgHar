# ADR-3: Authorization / Policy Enforcement Layer

| | |
|---|---|
| **Status** | Accepted |
| **Date** | 2026-08-24 |
| **Deciders** | Me |
| **Consulted** | Security/InfoSec, Compliance/Legal, FinOps |
| **Informed** | Engineering leadership, platform team |
| **Supersedes / Related** | Builds on [ADR-0](week1-adr-0-and-capability-map.md) (risk-tier table), [ADR-1](week1-adr-1-agent-execution-model.md) (fixes where enforcement runs), and [ADR-2](week1-adr-2-agent-identity-and-permission-model.md) (identity claims consumed as policy input); constrains ADR-4 (sandboxing enforces the network/tool allowlist decided here); merge-gate verification mechanics depend on ADR-6's git-hosting-platform choice |

## Context

ADR-0's action-category/risk-tier table is the reference this ADR turns into actual enforcement — every row needs a concrete mechanism, not just a label like "auto-allowed" or "requires explicit approval."

ADR-1 already fixed the enforcement point in time and space: the orchestrator is the only thing that can act — mint a credential, launch a container — before any agent code runs, and invocations are one-shot with no mid-run checkpoint. So gating happens pre-invocation, not mid-flight; there is no "pause the running agent and ask" mechanism to design around, by construction.

ADR-2 fixed what's available to gate on: the delegation record (`invocation_id`, `triggering_principal`, `orchestrator_instance_id`, `bot_account`, `target_repo`, `task_type`, `risk_tier`, `granted_scopes`, `issued_at`, `expires_at`) — explicitly non-self-authorizing, built to be evaluated by this layer.

ADR-0 also deliberately deferred one question to this ADR: can policy consider the *provenance* of an instruction — was this action driven by the triggering principal's original task, or induced by something the agent encountered mid-run (a poisoned file, a compromised MCP tool's output) — not just the action itself? The honest answer given the execution model: provenance-of-intent (detecting a hijacked goal mid-run) is out of reach, because one-shot execution provides no mid-run checkpoint to inspect it at. What this ADR *can* guarantee instead is that no action can exceed the scope decided at mint time, regardless of what the agent is tricked into attempting mid-run — containment substitutes for detection where detection isn't feasible.

## Decision Drivers

- **Security exposure** — ungoverned actions are the core problem this whole initiative exists to fix.
- **Developer adoption / friction** — the gate must not become a bottleneck teams route around (ADR-0).
- **Avoiding a single point of organizational failure** — the permission-intersection model (ADR-0/ADR-2): identity sets the ceiling, this layer narrows further, and no single artifact or team should be the sole authority.
- **Cost containment** — quotas/rate limits are a policy decision, not a bolted-on afterthought.
- **Tool / MCP supply-chain risk** — this layer decides the allowlist; ADR-4 enforces it at the network layer.
- **Auditability** — ADR-0's success metric: % of agent actions that pass through a policy gate, target as close to 100% as possible for medium/high risk tiers.

## Options Considered

**1. Hard-coded rules inside the orchestrator.** Rejected — policy becomes inseparable from orchestrator code: every tweak needs an orchestrator redeploy reviewed by whoever owns that code, recreating the exact single-team bottleneck ADR-0 warns against, with no independent audit trail of "what policy was in effect when."

**2. Central Policy Decision Point (PDP), called by the orchestrator (the Policy Enforcement Point, per ADR-1), with one global policy bundle owned by the platform team.** Rejected as the *complete* design — decouples policy from orchestrator code, which is good, but recreates the bottleneck one level up: every repo- or team-specific tweak still requires the platform team to edit the global bundle.

**3. Central PDP + per-repo/org declarative policy data, loaded by the PDP as data alongside a global baseline that can only narrow, never widen — chosen.** Detailed below.

## Decision

### Architecture: PDP/PEP with layered, narrowing policy data

- The orchestrator remains the sole Policy Enforcement Point (already fixed by ADR-1): before minting any ADR-2 credential or launching any container, it calls a PDP with the delegation-record claims as input.
- A **global baseline**, owned by the platform team, encodes ADR-0's risk-tier table directly (e.g. `risk_tier: medium` → default allow, human review required before merge; `risk_tier: high` → `require_human_approval` before start). **Per-repo/org override data**, versioned in a policy-data source, can only *narrow* the global baseline (e.g. a repo owner requiring approval even for a normally-auto-allowed tier) — never widen it. This gives repo/team owners genuine self-service within guardrails without needing the platform team in the loop for every change, while the global baseline stays the actual ceiling.
- PDP decision output: `{ decision: allow | deny | require_human_approval, granted_scopes ⊆ requested_scopes, reason, policy_bundle_version }`. This becomes part of the audit record, extending ADR-2's delegation record — closing the loop on "which agent, whose delegated authority, what action, under what policy."
- **Human-approval-required path**: because ADR-1 made invocations one-shot and pre-start, "requiring approval" just means the orchestrator doesn't mint a credential or launch a container until an approval event exists — no mid-run pause/resume machinery needed. This is a direct payoff of the one-shot decision made in ADR-1.
- **Merge-time gate**: for risk tiers requiring "human review before merge," this ADR does not reimplement review logic. It requires the orchestrator to verify, before proceeding, that the target repository's branch protection (on whichever platform ADR-6 selects) actually enforces the required reviewers/checks for that tier. A repo whose branch protection doesn't match what its computed risk tier requires is a policy failure — the orchestrator denies the invocation rather than proceeding and hoping a human notices later.
- **MCP / tool allowlisting**: any MCP/tool server an invocation may call is itself a PDP-evaluated claim (by risk tier and/or repo), recorded in the delegation record alongside the git-scope claims. The PDP decides the allowlist; ADR-4's network-egress sandboxing is what actually enforces it — this layer produces a decision, not a control.
- **Cost/quota**: the same PDP call evaluates simple quota rules (e.g. max invocations per triggering principal per day/repo) as part of the same decision, so cost containment has one enforcement point instead of a separate bolted-on system.
- **Fail-closed**: if the PDP is unreachable, the orchestrator denies rather than proceeds. PDP availability becomes a hard dependency for running any agent at all — an intentional trade given the security drivers (see Consequences).

### Technology: PDP engine and interface

| | OPA (Rego) | Cedar | Cerbos |
|---|---|---|---|
| License / governance | Apache 2.0; core stays CNCF-governed, but Apple hired the core maintainers in Aug 2025 and Styra's commercial (DAS) offering is winding down — real drift, not disqualifying for a vendor-neutral CNCF project | Apache 2.0; open-sourced by AWS in 2023, still primarily AWS-driven | Apache 2.0; smaller, independent project, explicitly positioned for agentic/NHI authorization |
| Maturity / adoption | Widest production track record for general self-hosted PDP use (Netflix, Goldman Sachs, Capital One, Google Cloud) | Strong within AWS-centric IAM use cases; less proven for general self-hosted, non-AWS PDP deployments | Newest of the three for this exact use case; smallest ecosystem |
| Policy language safety | Rego is expressive but has real footguns — runtime exceptions, non-determinism in complex policies | Designed to be deterministic and formally verifiable — fewer footguns than Rego | Deterministic; narrower, purpose-built policy model |

Chosen: **OPA**, called via the emerging **AuthZEN Authorization API** (an OpenID Foundation standard, published March 2026) rather than an OPA-proprietary integration. OPA has the deepest self-hosted production track record for exactly this "arbitrary resource, arbitrary policy" PDP role, is free and fully usable without any Styra commercial component, and its governance concern — real, but so far limited to the commercial layer, not the CNCF-governed core — is mitigated by *not* coupling the orchestrator to OPA specifically. AuthZEN standardizes the PDP↔PEP call, so if OPA's community health degrades further, the PDP can be swapped for Cedar or Cerbos behind the same interface without reworking ADR-1's orchestrator — the same swap-later posture ADR-1/ADR-2 took on the git hosting platform, applied here to the policy engine.

## Consequences

**Positive**
- Single technical enforcement chokepoint — auditable, and a real kill switch, since deny-all is one bundle update away — without being a single *organizational* bottleneck, since repo/team owners can narrow policy for themselves.
- Policy is versioned, testable code, separate from orchestrator code; "what policy was in effect for invocation X" is answerable from `policy_bundle_version`.
- Directly satisfies ADR-0's "% of agent actions that pass through a policy gate" success metric — by construction, no invocation happens without a PDP call.
- Reuses the git host's own review/branch-protection mechanism for the actual merge-time human gate, rather than reimplementing review logic in this system.
- A swappable PDP implementation (via AuthZEN) limits exposure to any one policy-engine project's governance risk.

**Negative / trade-offs**
- Fail-closed means PDP availability is now a hard dependency for running any agent — an outage in the policy layer halts the whole harness by design, and needs monitoring/alerting proportionate to that criticality (ADR-5).
- Per-repo override data is itself a privileged artifact: whoever can write to it can loosen (within bounds) or attempt to bypass policy for their repo, so that data source needs its own strong protection — a bootstrapping concern, flagged below.
- Adds a synchronous network hop (orchestrator → PDP) to every invocation's startup latency.
- Rego has a real learning curve and known correctness footguns; mitigated by keeping global-baseline rules simple and pushing complexity toward well-tested per-tier defaults rather than bespoke per-repo Rego.

## Open Questions

- Exact global-baseline vs. per-repo-override precedence mechanics in Rego (how "can only narrow" is actually enforced, not just documented) — spike needed.
- Who can write to the per-repo/org policy-override data, and how that write access is itself protected — likely resolved once ADR-6 fixes the git hosting platform and its branch-protection primitives.
- Concrete mechanism for recording a "human approval" event (CLI, chatops, UI) — implementation detail, not decided here.
- PDP high-availability/deployment topology, given fail-closed makes it a hard dependency — informs ADR-5's alerting requirements.
- Whether to formally pilot Cerbos given it's purpose-built for agentic/NHI authorization — worth a short bake-off before implementation, not before this decision.

## References

- [ADR-0: AI Coding Agent Harness — Problem Statement & Scope](week1-adr-0-and-capability-map.md)
- [ADR-1: Agent Execution Model — Orchestrator + Ephemeral One-Shot Containers](week1-adr-1-agent-execution-model.md)
- [ADR-2: Agent Identity & Permission Model](week1-adr-2-agent-identity-and-permission-model.md)
- oso, "OPA vs Cedar vs Zanzibar: Policy Engine Guide" — https://www.osohq.com/learn/opa-vs-cedar-vs-zanzibar
- Permit.io, "Policy Engines: Open Policy Agent vs AWS Cedar vs Google Zanzibar" — https://www.permit.io/blog/policy-engines
- Cerbos, "AI Agent Authorization & Access Control for Agentic Systems" — https://www.cerbos.dev/features-benefits-and-use-cases/agentic-authorization
- CodiLime, "Why Open Policy Agent is the Missing Guardrail for Your AI Agents" — https://codilime.com/blog/why-use-open-policy-agent-for-your-ai-agents/
- AuthZEN Authorization API 1.0 (OpenID Foundation) overview — https://dev.to/kanywst/authzen-authorization-api-10-deep-dive-the-standard-api-that-separates-authorization-decisions-1m2a
- sph.sh, "Cedar vs Rego vs OpenFGA: Policy Language Comparison" — https://sph.sh/en/posts/policy-language-comparison-cedar-rego-openfga/
