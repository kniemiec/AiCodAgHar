# ADR-2: Agent Identity & Permission Model

| | |
|---|---|
| **Status** | Accepted |
| **Date** | 2026-08-24 |
| **Deciders** | Me |
| **Consulted** | Security/InfoSec |
| **Informed** | Engineering leadership, platform team |
| **Supersedes / Related** | Builds on [ADR-0](week1-adr-0-and-capability-map.md) and [ADR-1](week1-adr-1-agent-execution-model.md); the identity→policy claims interface defined here is a hard dependency for ADR-3 (policy enforcement); assumes git-hosting-platform capabilities bound later in ADR-6 |

## Context

ADR-0 established that agents are a category of non-human identity (NHI) and should follow NHI lifecycle patterns — short-lived/session-scoped credentials, explicit delegation chains ("acting on behalf of"), rotation and revocation — rather than human-SSO patterns.

ADR-1 fixed the execution model on top of that: an orchestrator mints a short-lived, repo-scoped credential and injects it into a single-use container per invocation (one invocation = one new task, or one round of work on an already-open PR); the orchestrator, not the agent, is the source of truth for the audit record.

This ADR defines what "identity" concretely means inside that model: who or what holds which credential, how delegation is recorded, and what claims are exposed to the policy engine (ADR-3) so it doesn't have to re-derive this context itself.

**Platform requirements this model assumes.** This ADR deliberately does not name a git hosting platform — that binding decision is deferred to ADR-6, once the requirements from every ADR that touches the git host are known. What follows assumes the eventual platform provides: (1) fine-grained, expiring access tokens scoped down to a single repository and a narrow permission set, not just "admin" vs "none"; (2) dedicated bot/service accounts distinct from human users; (3) a PR/MR API that records author and description; and (4) token-scope enforcement that is actually correct, not merely documented — scope-based isolation is only as strong as the platform's enforcement of it. This capability set is not hypothetical: it is already confirmed available in free, self-hostable candidates (see ADR-6), so the model below is implementable, not just aspirational.

## Decision Drivers

- **NHI lifecycle pattern** — short-lived, delegation chains, rotation, revocation (ADR-0).
- **Auditability** — "which agent, whose delegated authority, what action" must be answerable from identity claims alone, per invocation, without cross-referencing agent-reported logs.
- **Avoiding a single point of failure** — identity claims should be necessary but not sufficient for authorization; the policy engine (ADR-3) evaluates on top of them, so no single artifact or service is the sole gate (the "permission-intersection" model referenced in ADR-0).
- **Containment** — revocation needs to work at two granularities: kill one invocation, or kill everything the orchestrator can currently mint (if the orchestrator itself is suspect).
- **Tool / MCP supply-chain risk** — the agent's credential is the boundary that limits what a compromised or prompt-injected agent can actually do against the git hosting platform, independent of what it's tricked into attempting.

## Options Considered

**1. Shared static bot credential for all agents.** Rejected — this is close to today's ungoverned baseline described in ADR-0's Context: no per-task attribution, one high-value standing credential, no natural expiry, and revocation kills every agent at once (no fine-grained containment).

**2. Per-invocation ephemeral bot *account* on the git hosting platform (create + delete per invocation).** Considered, not adopted. Gives the strongest possible attribution — the platform's own commit/PR author field would be unique per invocation — but adds real operational cost: full account lifecycle management, orphaned-account risk if the orchestrator crashes mid-invocation, and namespace churn. Worth revisiting if invocation-id correlation (option 3) proves insufficient in practice.

**3. Small pool of purpose-scoped bot accounts + per-invocation ephemeral scoped token, correlated via an explicit invocation id — chosen.** Detailed below.

## Decision

Three identity layers:

**a) Orchestrator identity (standing NHI).** The orchestrator holds a credential on the git hosting platform capable of minting scoped tokens under the bot-account pool (see c). This credential is itself short-lived and auto-renewed, not a permanent secret, and stored in a secrets manager. It is the highest-value target in this model; the requirement to treat it as a rotatable NHI rather than a static secret is fixed here, while its concrete storage/rotation mechanics belong to ADR-4.

**b) Triggering principal (who or what asked for the work).** Every invocation must trace back to an explicit principal — a human developer (via whatever request surface: CLI, chatops, UI) or a named automated trigger (e.g. `system:scheduled-dependency-bump`, `system:issue-label-trigger`). No invocation is anonymous; "a cron job ran" is not an acceptable principal of last resort.

**c) Per-invocation agent credential.** A small, fixed pool of purpose-scoped bot accounts on the git hosting platform — mapped to task category / risk tier from ADR-0's action-category table (e.g. a `pr-writer` bot distinct from a `repo-creator` bot, since creating a new repository is a materially broader privilege than writing to an existing one). For each invocation, the orchestrator mints a short-lived, repository-scoped access token under the appropriate bot account, with:
- scope limited to exactly what the task needs (e.g. `write:repository` + issue/PR scopes for a review round; repo-creation scope granted only when the task explicitly requires creating a new repo),
- restriction to the single target repository, not org-wide,
- expiry set to the container's max lifetime (per ADR-1),

injected into the container at launch, never baked into the image.

**Delegation & correlation record.** Each invocation gets an explicit record, emitted by the orchestrator at mint time — this is both the audit artifact and the identity→policy claims interface ADR-3 evaluates against:

```
{
  invocation_id,
  task_id,                 // groups every invocation belonging to one logical unit of work
  triggering_principal,
  orchestrator_instance_id,
  bot_account,
  target_repo,            // or org + create_new: true
  task_type,               // new-task | pr-review-round | review | security-triage (per ADR-7)
  risk_tier,               // from ADR-0's action-category table
  granted_scopes,
  issued_at,
  expires_at
}
```

**`task_id`** exists because a unit of work is no longer always a single invocation: ADR-7's multi-agent review workflow means one PR can span a coding round plus however many review/security-triage rounds it triggers, each its own invocation with its own credential. `task_id` is what ties all of them back together as one traceable, auditable, cost-attributable unit — the PR/branch identity is the natural value for it, since every round already operates against "the current PR branch at its current HEAD" per ADR-1. `invocation_id` still uniquely identifies one round; `task_id` is the grouping key across rounds.

These claims inform ADR-3's authorization decision; they are not self-authorizing on their own — ADR-3 still decides whether to grant/allow based on claims plus its own rules. The agent container also writes `invocation_id` into a trailer on every commit and into the PR description it creates or updates, so a PR sitting in the platform's UI — where the visible author is just the shared bot account — can always be traced back to the exact orchestrator record, principal, and risk tier that produced it.

**Revocation, two granularities:** revoke a single invocation (let its token expire, or explicitly invalidate it — the credential-layer expression of ADR-1's container-destruction kill switch), or revoke the orchestrator's own minting credential (stops all future invocations at once — the "something is systemically wrong" kill switch).

## Consequences

**Positive**
- Full chain is answerable for any PR: triggering principal → orchestrator instance → bot account/token → invocation id → risk tier — satisfies ADR-0's audit success criterion directly.
- Blast radius per invocation is narrow by construction: one repo, one scope set, one short TTL.
- Two-tier revocation gives both a scalpel (one invocation) and a big red button (the orchestrator's minting credential) without conflating them.
- Avoids the platform account-lifecycle overhead of option 2 while keeping equivalent traceability via invocation-id correlation.
- Identity claims feed, but don't replace, policy evaluation — keeps this ADR from becoming the single point of failure ADR-0 warns against.

**Negative / trade-offs**
- The platform's own UI/audit log alone is insufficient for attribution, since many invocations share one bot account — correlation depends on the orchestrator's log and on the invocation-id trailer convention being followed consistently. The orchestrator log remains authoritative regardless, but a missing trailer weakens the platform-side half of that linkage.
- The orchestrator's own minting credential is a concentrated high-value target; compromising it undermines every downstream scoping guarantee. Protecting it is essential but is properly detailed in ADR-4, not here.
- Depends on the eventual platform's token-scope enforcement actually being correct and kept patched — scope-based isolation is only as strong as that enforcement, and this is a live category of bug, not a theoretical one (e.g. Gitea has had CVEs in exactly this area). Whichever platform ADR-6 selects needs to be checked against its own history here.
- Bot-account pool sizing (how many, mapped to which risk tiers/task categories) is a judgment call with no clean formula, and will likely need adjustment after real usage.

## Open Questions

- Exact bot-account pool and its mapping to ADR-0's risk tiers / task categories — in particular, "create a new repository" isn't currently its own row in ADR-0's action-category table, but this ADR treats it as needing a distinct bot account and narrower default grant; flag as a follow-up addition to that table.
- Where/how the orchestrator's own minting credential is stored, rotated, and protected — deferred to ADR-4, but is a hard dependency of this design.
- Formal schema/signing mechanism for the delegation record (plain log entry vs. a signed token consumable by other systems) — enough is fixed here for ADR-3 to proceed; finalize during ADR-3 drafting.
- Which concrete git hosting platform satisfies the requirements above — deferred to ADR-6.
- Once ADR-6 selects a platform: confirm the deployed version/patch level closes any known token-scope-escalation CVEs for that platform before relying on scope as a security boundary.

## References

- [ADR-0: AI Coding Agent Harness — Problem Statement & Scope](week1-adr-0-and-capability-map.md)
- [ADR-1: Agent Execution Model — Orchestrator + Ephemeral One-Shot Containers](week1-adr-1-agent-execution-model.md)
- NHI credential/delegation research already cited in ADR-1 (GitGuardian, Descope, Cloud Security Alliance)
- Platform-specific evidence (token-scoping model, CVE history, resource footprint) is evaluated in ADR-6, which binds this ADR's requirements to a concrete git hosting platform.
