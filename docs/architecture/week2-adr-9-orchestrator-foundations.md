# ADR-9: Orchestrator Foundations — Config Storage, Human-Approval Mechanism, and Testable Boundaries

| | |
|---|---|
| **Status** | Proposed |
| **Date** | 2026-09-02 |
| **Deciders** | Me |
| **Consulted** | Security/InfoSec, Developer Experience |
| **Informed** | Engineering leadership, platform team |
| **Supersedes / Related** | Builds on [ADR-0](week1-adr-0-and-capability-map.md) (risk tiers), [ADR-1](week1-adr-1-agent-execution-model.md) (bespoke orchestrator), [ADR-2](week1-adr-2-agent-identity-and-permission-model.md) (bot-account/repo-permission pattern, delegation record), [ADR-3](week1-adr-3-policy-enforcement-layer.md) (resolves its open questions on policy-data storage/ownership and the human-approval-event mechanism), [ADR-5](week1-adr-5-observability-and-audit-trail.md) (approval events extend the audit record), [ADR-6](week1-adr-6-git-hosting-platform-selection.md) (reuses Forgejo's repo permissions, labels, webhooks — no new platform introduced), and [ADR-7](week1-adr-7-multi-agent-review-workflow.md) (resolves its open question on where the stage-list config lives); microVM launch/lifecycle mechanics (Firecracker operational details) are explicitly deferred to [ADR-10](week2-adr-10-microvm-launch-and-lifecycle-mechanics.md), not covered here

## Context

ADR-1 decided the orchestrator is bespoke infrastructure but deliberately left its internal design open until ADR-2/ADR-3 made its responsibilities concrete. Those responsibilities are now concrete — mint a credential, call the PDP, launch a container, emit an audit record — but three foundational things real orchestrator code needs are still open, all flagged as explicit gaps in earlier ADRs rather than newly discovered here:

1. **Where policy/stage-config data actually lives.** ADR-3 decided policy is *data*, layered as a global baseline that per-repo overrides can only narrow — but left "where/how the override data is stored, and who can write to it" as an open question, noting that data source is itself a privileged artifact. ADR-7 separately decided the review/security-triage stage list is also declarative config, not orchestrator code, and left its schema/storage location open too. Both are the same underlying problem and should get one answer, not two.
2. **How a human-approval event is concretely recorded.** ADR-3's "requires human approval" gate means the orchestrator simply doesn't mint a credential or launch a container until an approval event exists — but what that event actually *is* (a CLI command, a chat message, a UI click) was left as "process detail, not decided here."
3. **How the orchestrator's own codebase should be structured at its external boundaries** (git host, PDP, secrets, audit sink, container launcher) to be genuinely unit-testable without every real dependency running simultaneously — this was the trigger for this ADR, raised as "should the orchestrator be built against interfaces before all its infrastructure exists." ADR-6 already answered a closely related-sounding question in the negative — no adapter/abstraction layer for a hypothetical future git-host swap — and this ADR needs to state clearly why the answer here is different in kind, not silently contradict that precedent.

None of this is about *what* the orchestrator's control flow does (ADR-1/ADR-3/ADR-7 already fixed that: mint → policy call → launch → audit, fan-out/aggregate/evaluate for multi-stage tasks). It's about what real, testable orchestrator code needs decided before it can be written.

## Decision Drivers

- **Avoiding a single point of organizational failure** (ADR-0) — config write-access must not concentrate in one team for changes that should be self-service, and must not silently allow a repo to grant itself more than the platform ceiling (ADR-3's core narrow-only requirement).
- **Auditability** — an approval event must become an unambiguous, attributable fact in the audit record (ADR-5), not an implicit side-channel.
- **Reuse over parallel infrastructure** (ADR-7) — every new mechanism here should ride on infrastructure already decided (Forgejo, Postgres, OPA) rather than introducing a new system.
- **Fail-closed consistency** (ADR-3) — an approval or config-load path that can't be verified should behave like the PDP does when unreachable: deny, don't guess.
- **Testability without speculative abstraction** — the orchestrator's control-flow logic should be exercisable by fast unit tests against fakes, without conflating that with building for backends that don't exist and aren't scheduled (ADR-6).
- **Cost/operational simplicity** (ADR-0, ADR-5) — proportionate to actual need; reject anything sized for problems this pilot doesn't have.

## Options Considered

### A. Policy / stage-config storage

**A1. A database table** (e.g., in the same Postgres instance ADR-5 already stood up for audit). Rejected — rows aren't naturally versioned or diffable; "what changed, by whom, when, reviewed by whom" would need to be hand-rolled on top, reinventing what version control already gives for free. Also reopens a "who can write which rows" access-control problem that git-based storage doesn't have, since Postgres has no native per-path permission model.

**A2. A dedicated config service** (etcd, Consul, a bespoke config API). Rejected — a new operational component with no capability this problem actually needs beyond what's already decided; the same "disproportionate to actual need" reasoning ADR-5 used to reject a full enterprise observability stack applies directly here.

**A3. OPAL** (`permitio/opal`) as the git-repo→OPA sync path. Considered, rejected. OPAL's actual differentiator — real-time push, multi-source diffing, fan-out to many OPA replicas — targets authorization data that changes *frequently* and needs to propagate fast; this project's policy/stage-config changes are infrequent (a human editing a YAML file, not a live entitlement stream), a shape OPA's own built-in bundle-polling already covers natively with latency (tens of seconds) that doesn't matter here. Adopting it would mean standing up an OPAL server, a client sidecar per OPA instance, and a pub/sub backbone between them — real operational surface — for a capability (fast propagation) this problem doesn't need, the same "disproportionate to actual need" reasoning ADR-5 used to reject a full observability stack. It's also a company-steered dependency (Permit.io; Apache 2.0, no BSL-style restriction found, but the same "one company's roadmap" governance shape already flagged for Gitea Ltd. and OpenHands/All Hands AI) — an additional cost with no matching benefit for this specific need. No spike was run; the mismatch between what OPAL optimizes for and what this problem actually requires was clear enough on its own to decide without one.

**A4. Version-controlled files in Forgejo, loaded into OPA as a bundle via a hand-rolled webhook-triggered bundle builder, with override data stored inside the target repo it governs, not a separate config repo — chosen.** Detailed below.

### B. Human-approval-event mechanism

**B1. Chatops** (a Slack/Discord bot command). Rejected for now — introduces a new external system and integration surface the stack doesn't otherwise need, for a pilot that's a single person (ADR-6's scope note: the pilot team is currently just the deciders themselves).

**B2. A dedicated approval UI/web app.** Rejected for now — real build effort disproportionate to a solo pilot; revisit if/when the harness has more than one human in the approval loop.

**B3. CLI-only** (an approver runs a local command against the orchestrator). Considered, not chosen as primary — trivial to build, but doesn't generalize past a single person with direct machine access to the orchestrator, and produces no independently-verifiable record of *who* approved beyond "whoever ran the command locally."

**B4. Git-native: a labeled/commented issue on the git host, verified against the platform's own permission model — chosen.** Detailed below.

### C. Orchestrator internal interface boundaries

**C1. No formal boundary — call Forgejo's SDK, OPA's HTTP client, OpenBao's client, etc. directly from control-flow code.** Rejected — makes the mint→policy→launch→audit sequence untestable without every real dependency live simultaneously, which was the concrete problem that opened this discussion.

**C2. A general-purpose, swappable-backend abstraction layer per dependency** (a generic `GitProvider` interface designed to support arbitrary future forges, a generic `ObjectStore` interface designed to support arbitrary future audit sinks, etc.). Rejected — this is exactly what ADR-6 already declined to build for the git host specifically, for a stated reason that generalizes: "an adapter layer would be speculative design against a swap that isn't scheduled, contrary to ADR-0's bias against designing for hypothetical future requirements." Building it here for storage/secrets/etc. would quietly reverse that precedent without a new argument for why this dependency is different.

**C3. A thin, single-production-implementation interface per external dependency, used purely as a test seam — chosen.** Detailed below.

## Decision

### A. Config storage: file-based, in-repo overrides, loaded into OPA as a bundle

- **Global baseline** — ADR-0's risk-tier table encoded as policy files in one platform-owned repo on the already-decided Forgejo instance (ADR-6). Only the platform team has write access, via Forgejo's ordinary repo permissions — no new access-control mechanism.
- **Per-repo override and stage-list config** — stored as files *inside the target repo itself* (e.g. `.harness/policy.yaml`, `.harness/stages.yaml`), not in a separate shared config repo. This directly resolves ADR-3's open question of "who can write to per-repo override data, and how is that access itself protected": the repo's own existing owners already have exactly the right permission scope over their own repo, enforced by a mechanism (Forgejo repo/branch permissions) that already exists and needs no new design. Changing your own repo's policy override is a normal PR against your own repo, reviewable the same way a code change is.
- **Bundle build**: a hand-rolled webhook-triggered job (Forgejo webhook → small build step) reads the global baseline repo plus every target repo's override file via Forgejo's contents API, merges them, and publishes the result as an OPA bundle (a gzip-compressed tarball OPA pulls via its standard bundle API — this is the mechanism ADR-3 already assumed without naming a concrete transport). OPAL was evaluated and rejected for this (see Options Considered, A3) — infrequent policy changes don't need OPAL's real-time-push differentiator, so a small in-house webhook handler is simpler and avoids an extra company-steered dependency for a capability this problem doesn't use.
- **Enforcing "narrow only," concretely** (ADR-3 flagged this as needing a spike, not just documentation): the bundle-build step validates each repo's override against the global baseline before including it. An override that would *widen* scope (e.g. downgrading a required-approval gate to auto-allow) is rejected — that repo's bundle entry falls back to the unmodified global baseline, and the rejection is logged as a signal in its own right, on the same footing as ADR-5's policy-gate reconciliation check: a rejected widening attempt is worth surfacing, not silently discarding.
- **Stage-list config** (ADR-7) uses the same file-in-repo, same-bundle-build mechanism, as a sibling file — deliberately not a second config-versioning system, since it's the same declarative-pipeline-as-data pattern ADR-7 already chose, and the CI-config-in-repo convention (`.github/workflows`-style) it explicitly likened itself to already assumes this shape.

### B. Human approval: a labeled issue on the git host, independently re-verified

- For a task that ADR-3's policy gate marks `require_human_approval`, the orchestrator opens (or reuses) an issue on the target repo containing the task spec, and waits for an `approved` label (an issue label is chosen over a comment-with-marker or a PR review for unambiguity — it's a single, queryable, non-free-text state rather than something that has to be parsed out of comment text).
- On receiving the label-added webhook, the orchestrator does **not** trust the webhook payload's claim about who applied the label. Consistent with ADR-1's "the agent's own output is not trusted as an audit record" principle applied to this new input source, it independently calls Forgejo's API to (a) verify the webhook's HMAC signature against the configured per-repo webhook secret (Forgejo signs deliveries via `X-Forgejo-Signature`, HMAC-SHA256), and (b) confirm the labeling user currently holds at-least-Write collaborator status (or membership in a designated approvers team) on that repo — the same permission-intersection instinct as ADR-2/ADR-3: possessing the ability to click "add label" is not itself self-authorizing.
- Verification failure (bad signature, insufficiently-privileged labeler, or Forgejo unreachable to check) is treated as **not approved** — fail-closed, mirroring ADR-3's PDP posture, rather than a benefit-of-the-doubt default.
- The resulting `approved_by` (the verified human principal) and `approval_evidence` (issue URL, label event, verification timestamp) become fields on the invocation's delegation record alongside `triggering_principal` (ADR-2) — extending that record rather than introducing a parallel one, and giving ADR-5's audit trail a real answer to "who approved this and how do we know."
- A thin CLI convenience wrapper that performs the same "add label via API" call is fine to build for local solo-pilot dev friction — it is not a second decision mechanism, just a shortcut to the one that exists.
- **Named limitation, not solved here**: for the pilot, the triggering principal and the available approver are frequently the same person. This is the same single-point-of-organizational-failure caveat ADR-0 already accepted for pilot-phase sponsorship — acceptable now, must be broadened (a real second approver) before this gate means anything past the pilot.

### C. Orchestrator interfaces: single-implementation test seams, not multi-backend abstraction

Define one narrow interface per external dependency the orchestrator's control-flow code calls — `GitHostClient`, `PolicyClient` (the ADR-3 PDP call), `SecretsClient` (ADR-4/OpenBao), `AuditWriter` (ADR-5's Postgres table), `ApprovalVerifier` (part B above), `ContainerLauncher` (ADR-1/ADR-4; internals deferred to ADR-10). Each interface gets **exactly one production implementation** — a Forgejo REST client, an AuthZEN HTTP client, an OpenBao client, a Postgres writer, and so on — plus a test double used only in unit tests of the control-flow logic itself (mint → policy call → launch → audit).

This is explicitly **not** the multi-backend abstraction ADR-6 declined to build: there is no intent, now or scheduled, to run against a second git host, a second policy engine, or a second audit store behind these interfaces. The seam exists so "does the orchestrator call the PDP before minting a credential, and deny when the PDP says deny" is a fast, deterministic unit test against a fake `PolicyClient`, not something that requires a live OPA instance to even exercise — which is the direct answer to the question that opened this discussion: orchestrator development does not need to wait for every piece of real infrastructure to be simultaneously running, provided its control flow is tested against these seams first. Integration tests — a separate, smaller, slower suite — exercise each real implementation against a local dev instance (Forgejo, OPA, OpenBao `-dev` mode, Postgres) as each becomes available, and nothing is considered done until that integration suite passes against the real thing, not just the fake.

## Consequences

**Positive**
- Every new mechanism here reuses infrastructure already decided (Forgejo permissions/labels/webhooks, OPA's bundle API, Postgres) — no new system introduced solely for this ADR.
- "Who can write policy for repo X" and "who can approve a high-risk task on repo X" both resolve to "whoever the git host already says can act on repo X" — one mental model, not two bespoke access-control schemes.
- The narrow-only enforcement point (bundle-build time) is independent of and in addition to the PR-review enforcement point ADR-3 already relies on — a genuine defense-in-depth pairing, not a single check.
- The approval mechanism produces an independently-verifiable, non-self-reported record, consistent with the series' "never trust the thing being audited" pattern.
- The interface/testability decision directly unblocks starting orchestrator development now, without waiting on every infrastructure piece to be live — while explicitly not repeating the reasoning ADR-6 used to decline speculative abstraction.

**Negative / trade-offs**
- In-repo override files mean the target repo's own maintainers can see and edit their policy override alongside their code — appropriate for a scoped-down "narrow only" gate, but would need re-examination if override data ever needed to carry anything more sensitive than narrowing rules.
- The hand-rolled bundle-build step is a new, small piece of infrastructure this design does introduce — modest, but real, and it's now something this project maintains itself rather than an off-the-shelf tool's problem; needs to be kept in sync with every repo's actual current override state, not just the global baseline.
- Label-based approval depends on Forgejo's webhook delivery and signature verification being correctly configured; a misconfigured webhook secret fails closed (no false approvals) but could also silently block a legitimate approval until noticed — worth its own alert, not just relying on someone to notice a stuck task.
- The solo-pilot approver/principal overlap remains a real, named gap, not a resolved one.
- Thin test-seam interfaces still add a small amount of indirection relative to calling SDKs directly — accepted because the alternative (C1) was the concrete blocker that prompted this ADR.

## Open Questions

- Exact approver-set definition mechanics — a specific Forgejo team vs. a minimum collaborator-role threshold — needs to be pinned down before implementation, not left ambiguous.
- Schema/validation for `.harness/policy.yaml` and `.harness/stages.yaml` (e.g. JSON Schema validated at bundle-build time before a malformed file can break the merge) — implementation detail, not decided here.
- Whether a rejected "widening" override attempt should also trigger a real-time alert (per ADR-5's pattern for egress-proxy denials) rather than only being logged — leaning yes, not fixed here.
- ~~MicroVM launch/lifecycle mechanics for `ContainerLauncher`'s production implementation~~ — **Resolved 2026-09-04.** [ADR-10: MicroVM Launch & Lifecycle Mechanics](week2-adr-10-microvm-launch-and-lifecycle-mechanics.md) covers jailer/cgroup-v2 launch, MMDS credential injection, vsock log extraction, the tap-device egress design, and kill-switch/teardown semantics, all validated against a real Firecracker host. One implementation gap it identifies as still needed, not yet built: `ContainerLauncher`'s production implementation must give each invocation a fresh copy-on-write rootfs layer, which the validating PoC did not itself do.

## References

- [ADR-0: AI Coding Agent Harness — Problem Statement & Scope](week1-adr-0-and-capability-map.md)
- [ADR-1: Agent Execution Model — Orchestrator + Ephemeral One-Shot Containers](week1-adr-1-agent-execution-model.md)
- [ADR-2: Agent Identity & Permission Model](week1-adr-2-agent-identity-and-permission-model.md)
- [ADR-3: Authorization / Policy Enforcement Layer](week1-adr-3-policy-enforcement-layer.md)
- [ADR-5: Observability & Audit Trail](week1-adr-5-observability-and-audit-trail.md)
- [ADR-6: Git Hosting Platform Selection](week1-adr-6-git-hosting-platform-selection.md)
- [ADR-7: Multi-Agent Review & Iteration Workflow](week1-adr-7-multi-agent-review-workflow.md)
- Open Policy Agent, "Bundles" (bundle API / gzipped-tarball transport) — https://www.openpolicyagent.org/docs/management-bundles
- OPAL Docs, "Track an API Bundle Server" (git-to-OPA policy/data sync) — https://docs.opal.ac/tutorials/track_an_api_bundle_server/
- Forgejo Docs, "Webhooks" (`X-Forgejo-Signature`, HMAC-SHA256 delivery signing) — https://forgejo.org/docs/latest/user/webhooks/
- Forgejo Docs, "Labels" — https://forgejo.org/docs/latest/user/collaboration/labels/
